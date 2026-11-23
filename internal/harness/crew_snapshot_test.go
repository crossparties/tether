package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/mount"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/snapshot"
)

func TestCrewSafetyEscalationRejectFeedbackLeaseLockAndPostHashRestore(t *testing.T) {
	ws := t.TempDir()
	leasedPath := filepath.Join(ws, "leased.go")
	guardedPath := filepath.Join(ws, "guarded.go")
	if err := os.WriteFile(leasedPath, []byte("package main\nfunc Original() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guardedPath, []byte("package main\nfunc Guarded() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.Mount().SetMountedMode("guarded.go", mount.AccessGuarded, "manual"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	crewStep := 0
	var sawRejectReason bool
	var mainLeaseBlocked bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		rawMsgs, _ := json.Marshal(body.Messages)
		s := string(rawMsgs)

		mu.Lock()
		crewStep++
		step := crewStep
		mu.Unlock()

		var msg chatMessage
		switch step {
		case 1:
			// Crew attempts a delegated write on leased.go AND an AccessGuarded edit on guarded.go!
			msg = chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{
					{
						ID:   "c_write_leased",
						Type: "function",
						Function: functionCall{
							Name:      "write_file",
							Arguments: `{"path":"leased.go","content":"package main\nfunc CrewUpdated() {}\n"}`,
						},
					},
					{
						ID:   "c_write_guarded",
						Type: "function",
						Function: functionCall{
							Name:      "write_file",
							Arguments: `{"path":"guarded.go","content":"package main\nfunc Hacked() {}\n"}`,
						},
					},
				},
			}
		default:
			if strings.Contains(s, "keep original guarded logic") {
				mu.Lock()
				sawRejectReason = true
				mu.Unlock()
			}
			msg = chatMessage{Role: "assistant", Content: "Updated leased.go and respected guarded.go rejection."}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		})
	}))
	defer srv.Close()

	r := NewRunner(eng, Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  srv.URL,
		APIKey:   "sk-crew-safety",
		Out:      &bytes.Buffer{},
	})

	// Install interactive asker with explicit reject reason on Main (inherited by Crew).
	askedGuarded := 0
	r.SetAskPolicyWithReason(func(d mcp.Decision) (mcp.Verdict, string, bool) {
		if strings.Contains(d.Path, "guarded.go") {
			askedGuarded++
			return mcp.VerdictReject, "keep original guarded logic", true
		}
		return mcp.VerdictOnce, "", true
	})

	// Spawn Crew with delegate_critical=true and exclusive lease on leased.go.
	res, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:             "editor-crew",
		LeaseFiles:       []string{"leased.go"},
		DelegateCritical: true,
		Mode:             "same-provider",
		Task:             "Update leased.go and try guarded.go",
	})
	if err != nil {
		t.Fatalf("CreateCrew: %v", err)
	}

	if askedGuarded == 0 || !sawRejectReason {
		t.Fatalf("expected AccessGuarded write to escalate and preserve reject reason (asked=%d, sawReason=%v)", askedGuarded, sawRejectReason)
	}

	// While Crew is active and holding lease on leased.go, Main MUST be blocked from writing leased.go!
	_, mainErr := r.mcp.CallTool("write_file", json.RawMessage(`{"path":"leased.go","content":"main-clobber"}`))
	if mainErr {
		mainLeaseBlocked = true
	}
	if !mainLeaseBlocked {
		t.Fatalf("expected Main write to leased.go to be blocked while Crew holds active lease")
	}

	// Closing Crew releases the lease lock cleanly.
	if err := r.CloseCrew(res.CrewID, "done"); err != nil {
		t.Fatalf("CloseCrew: %v", err)
	}
	leases, err := eng.Mount().ListActiveLeases()
	if err != nil || len(leases) != 0 {
		t.Fatalf("expected 0 active leases after CloseCrew, got %+v err=%v", leases, err)
	}

	// Verify PostHash snapshot was recorded with CrewID and protects external edits on RestoreCrew!
	if err := os.WriteFile(leasedPath, []byte("package main\nfunc ManualUserEdit() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	crewRes, err := snapshot.New(ws).RestoreCrew(res.CrewID, ws, false)
	if !errors.Is(err, snapshot.ErrExternalModification) {
		t.Fatalf("expected ErrExternalModification on RestoreCrew after external edit, got err=%v res=%+v", err, crewRes)
	}
	got, _ := os.ReadFile(leasedPath)
	if !strings.Contains(string(got), "ManualUserEdit") {
		t.Fatalf("external user edit was overwritten despite force=false: %s", string(got))
	}

	// Force=true restores back to pre-crew Original().
	if _, err := snapshot.New(ws).RestoreCrew(res.CrewID, ws, true); err != nil {
		t.Fatalf("RestoreCrew(force=true): %v", err)
	}
	got, _ = os.ReadFile(leasedPath)
	if !strings.Contains(string(got), "func Original()") {
		t.Fatalf("expected pre-crew Original restored with force=true, got: %s", string(got))
	}
}

func TestCrewPanicAndForceReleaseCleanupLeaseLock(t *testing.T) {
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "panic_target.go"), []byte("package main\n"), 0o644)

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	r := NewRunner(eng, Config{Provider: "ollama", Out: &bytes.Buffer{}})
	if err := eng.CreateCrewSession(core.CrewSession{
		ID:             "crew-panic",
		ParentThreadID: eng.SessionID,
		Name:           "panic-worker",
		Status:         "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Mount().AcquireLeaseLock("crew-panic", []string{"panic_target.go"}, 5*time.Minute); err != nil {
		t.Fatal(err)
	}

	// Simulate panic inside runCrewTurnSafely -> defer MUST release lease lock & abort session!
	h := &crewHandle{
		id:         "crew-panic",
		name:       "panic-worker",
		runner:     nil, // dereferencing nil runner inside runWithOptions triggers panic cleanly
		leaseFiles: []string{"panic_target.go"},
	}
	_, runErr := r.runCrewTurnSafely(context.Background(), h, "boom")
	if runErr == nil || !strings.Contains(runErr.Error(), "panicked") {
		t.Fatalf("expected recovered panic error, got: %v", runErr)
	}

	leases, err := eng.Mount().ListActiveLeases()
	if err != nil || len(leases) != 0 {
		t.Fatalf("expected panic defer cleanup to release all leases, got %+v err=%v", leases, err)
	}
	cs, err := eng.GetCrewSession("crew-panic")
	if err != nil || cs.Status != "aborted" {
		t.Fatalf("expected crew status aborted after panic, got %+v err=%v", cs, err)
	}

	// Verify ForceReleaseCrewLease also unlocks active leases explicitly.
	if err := eng.CreateCrewSession(core.CrewSession{ID: "crew-stuck", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Mount().AcquireLeaseLock("crew-stuck", []string{"panic_target.go"}, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := r.ForceReleaseCrewLease("crew-stuck"); err != nil {
		t.Fatalf("ForceReleaseCrewLease: %v", err)
	}
	if dec := eng.Mount().AuthorizePathForActor("panic_target.go", mount.OpWrite, "code", "main"); !dec.Allowed {
		t.Fatalf("expected write unlocked after ForceReleaseCrewLease, got: %s", dec.Reason)
	}
}

func TestCrewMessageDelegateFalseEscalatesAndRestoreFile(t *testing.T) {
	ws := t.TempDir()
	notePath := filepath.Join(ws, "note.txt")
	if err := os.WriteFile(notePath, []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var mu sync.Mutex
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		call++
		n := call
		mu.Unlock()
		var msg chatMessage
		switch n {
		case 1:
			msg = chatMessage{Role: "assistant", Content: "Ready for instructions."}
		case 2:
			msg = chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{{
					ID:       "c_edit_1",
					Type:     "function",
					Function: functionCall{Name: "edit_file", Arguments: `{"path":"note.txt","target":"v0","replacement":"v1"}`},
				}},
			}
		case 3:
			// Repeat same edit after AskPolicy granted path approval!
			msg = chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{{
					ID:       "c_edit_2",
					Type:     "function",
					Function: functionCall{Name: "edit_file", Arguments: `{"path":"note.txt","target":"v0","replacement":"v1"}`},
				}},
			}
		default:
			msg = chatMessage{Role: "assistant", Content: "note.txt updated to v1."}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		})
	}))
	defer srv.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "sk-msg", Out: &bytes.Buffer{}})
	escalated := 0
	r.SetAskPolicy(func(d mcp.Decision) (mcp.Verdict, bool) {
		if d.Path == "note.txt" {
			escalated++
		}
		return mcp.VerdictOnce, true
	})

	// DelegateCritical=false -> even routine workspace edit escalates via AskPolicy!
	res, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:             "strict-crew",
		LeaseFiles:       []string{"note.txt"},
		DelegateCritical: false,
		Task:             "Acknowledge readiness",
	})
	if err != nil {
		t.Fatalf("CreateCrew: %v", err)
	}

	reply, err := r.MessageCrew(context.Background(), res.CrewID, "Update note.txt to v1")
	if err != nil || !strings.Contains(reply, "v1") {
		t.Fatalf("MessageCrew: reply=%q err=%v", reply, err)
	}
	if escalated != 1 {
		t.Fatalf("expected delegate_critical=false to escalate edit_file once via AskPolicy, got %d", escalated)
	}

	// Verify PostHash & RestoreFile guard against external edit.
	if err := os.WriteFile(notePath, []byte("manual-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := snapshot.New(ws)
	turnID, err := store.LastTurn()
	if err != nil || turnID == "" {
		t.Fatalf("LastTurn: %q err=%v", turnID, err)
	}
	if rfRes, rerr := store.RestoreFile(turnID, "note.txt", ws, false); !errors.Is(rerr, snapshot.ErrExternalModification) || !rfRes.Conflict {
		t.Fatalf("expected RestoreFile ErrExternalModification conflict, got res=%+v err=%v", rfRes, rerr)
	}
	if rfRes, rerr := store.RestoreFile(turnID, "note.txt", ws, true); rerr != nil || !rfRes.Restored {
		t.Fatalf("expected RestoreFile(force=true) to restore, got res=%+v err=%v", rfRes, rerr)
	}
	got, _ := os.ReadFile(notePath)
	if string(got) != "v0\n" {
		t.Fatalf("expected note.txt restored to v0, got %q", string(got))
	}
	_ = r.CloseCrew(res.CrewID, "done")
}

func TestReadOnlyCrewBlockedDespiteFolderGrantAndDelegateFalseUnderTUI(t *testing.T) {
	ws := t.TempDir()
	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Grant broad folder approval & ensure mode=code so only explicit read-only / ForceAsk stops writes.
	_ = eng.SetMode("code")
	if err := eng.AddApprovalGrant(".", "path"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "ro_target.txt"), []byte("keep-me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	phase := "ro"
	roStep := 0
	dfStep := 0
	var roToolOutcome string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		curPhase := phase
		mu.Unlock()

		var msg chatMessage
		if curPhase == "ro" {
			mu.Lock()
			roStep++
			n := roStep
			mu.Unlock()
			switch n {
			case 1:
				msg = chatMessage{
					Role: "assistant",
					ToolCalls: []toolCall{{
						ID:       "ro_w1",
						Type:     "function",
						Function: functionCall{Name: "write_file", Arguments: `{"path":"ro_target.txt","content":"hacked"}`},
					}},
				}
			case 2:
				roToolOutcome = body.Messages[len(body.Messages)-1].Content
				msg = chatMessage{
					Role: "assistant",
					ToolCalls: []toolCall{{
						ID:       "ro_m1",
						Type:     "function",
						Function: functionCall{Name: "mode_set", Arguments: `{"mode":"code"}`},
					}},
				}
			default:
				msg = chatMessage{Role: "assistant", Content: "ro blocked as expected."}
			}
		} else {
			mu.Lock()
			dfStep++
			n := dfStep
			mu.Unlock()
			switch n {
			case 1:
				msg = chatMessage{
					Role: "assistant",
					ToolCalls: []toolCall{{
						ID:       "df_w1",
						Type:     "function",
						Function: functionCall{Name: "write_file", Arguments: `{"path":"routine.txt","content":"v1"}`},
					}},
				}
			case 2:
				msg = chatMessage{
					Role: "assistant",
					ToolCalls: []toolCall{{
						ID:       "df_w2",
						Type:     "function",
						Function: functionCall{Name: "write_file", Arguments: `{"path":"routine.txt","content":"v1"}`},
					}},
				}
			default:
				msg = chatMessage{Role: "assistant", Content: "routine updated after prompt."}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		})
	}))
	defer srv.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "sk-sec", Out: &bytes.Buffer{}})
	r.SetAutoApprovePolicy(r.DefaultAutoApprovePolicy())

	tuiPromptCount := 0
	// Simulate cmd/tether/main.go BindApproval policy using safety.IsNonCriticalTool when !d.ForceAsk.
	r.SetAskPolicy(func(d mcp.Decision) (mcp.Verdict, bool) {
		target := d.Path
		if target == "" {
			target = d.Command
		}
		if !d.ForceAsk {
			if ok, _ := safety.IsNonCriticalTool(eng.GetMode(), d.Tool, target, d.Command, ws); ok {
				return mcp.VerdictOnce, true
			}
		}
		tuiPromptCount++
		return mcp.VerdictOnce, true
	})

	roRes, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name: "auditor",
		Mode: "readonly",
		Task: "Try writing ro_target.txt",
	})
	if err != nil {
		t.Fatalf("CreateCrew readonly: %v", err)
	}
	if !strings.Contains(strings.ToLower(roToolOutcome), "read-only") && !strings.Contains(strings.ToLower(roToolOutcome), "denied") {
		t.Fatalf("expected readonly write denial, got: %q", roToolOutcome)
	}
	gotRO, _ := os.ReadFile(filepath.Join(ws, "ro_target.txt"))
	if string(gotRO) != "keep-me\n" {
		t.Fatalf("readonly crew modified ro_target.txt despite block: %q", string(gotRO))
	}
	if tuiPromptCount != 0 {
		t.Fatalf("readonly crew should fail closed before asking user, got %d prompts", tuiPromptCount)
	}
	_ = r.CloseCrew(roRes.CrewID, "done")

	mu.Lock()
	phase = "df"
	mu.Unlock()

	dfRes, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:             "unprivileged-writer",
		DelegateCritical: false,
		LeaseFiles:       []string{"routine.txt"},
		Task:             "Write routine.txt",
	})
	if err != nil {
		t.Fatalf("CreateCrew delegate_critical=false: %v", err)
	}
	if tuiPromptCount != 1 {
		t.Fatalf("expected delegate_critical=false to escalate via ForceAsk under TUI policy, got tuiPromptCount=%d", tuiPromptCount)
	}
	gotRoutine, _ := os.ReadFile(filepath.Join(ws, "routine.txt"))
	if string(gotRoutine) != "v1" {
		t.Fatalf("expected approved routine.txt to be written after escalation, got %q", string(gotRoutine))
	}
	_ = r.CloseCrew(dfRes.CrewID, "done")
}

func TestCreateCrewRejectsSkillSymlinkEscapeAndFocusIgnored(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("API_KEY=supersecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".agents", "skills", "evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "..", ".env"), filepath.Join(ws, ".agents", "skills", "evil", "SKILL.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".agents", "skills", "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".agents", "skills", "safe", "SKILL.md"), []byte("Always check errors."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "ignored.go"), []byte("package main\nconst s = \"SECRET_IGNORED_CONTENT\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "allowed.go"), []byte("package main\nconst p = \"PUBLIC_OK\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.Mount().SetMountedMode("ignored.go", mount.AccessIgnored, "manual"); err != nil {
		t.Fatal(err)
	}

	var capturedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		capturedPrompt = body.Messages[len(body.Messages)-1].Content
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": chatMessage{Role: "assistant", Content: "ok"}, "finish_reason": "stop"}},
		})
	}))
	defer srv.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "sk-skill", Out: &bytes.Buffer{}})

	if _, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:      "evil-symlink",
		SkillName: "evil",
		Task:      "Leak env",
	}); err == nil {
		t.Fatalf("expected CreateCrew to return error on symlinked SKILL.md -> .env")
	}
	if _, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:      "evil-dotdot",
		SkillName: "../evil",
		Task:      "Traverse",
	}); err == nil {
		t.Fatalf("expected CreateCrew to return error on traversal skill_name")
	}

	safeRes, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:       "safe-crew",
		Mode:       "readonly",
		SkillName:  "safe",
		FocusFiles: []string{"ignored.go", "allowed.go"},
		Task:       "Inspect allowed files",
	})
	if err != nil {
		t.Fatalf("CreateCrew safe skill: %v", err)
	}
	if !strings.Contains(capturedPrompt, "Always check errors.") || !strings.Contains(capturedPrompt, "PUBLIC_OK") {
		t.Fatalf("expected safe skill and allowed.go in prompt, got: %s", capturedPrompt)
	}
	if strings.Contains(capturedPrompt, "SECRET_IGNORED_CONTENT") {
		t.Fatalf("mounted_files ignored file leaked into Crew focus_files: %s", capturedPrompt)
	}
	_ = r.CloseCrew(safeRes.CrewID, "done")
}

func TestCrewTransientErrorPreservesSessionAndLease(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "work.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		calls++
		c := calls
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if c == 2 {
			// Return a non-retryable 400 error on the follow-up MessageCrew turn.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"transient bad turn"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": chatMessage{Role: "assistant", Content: "crew ok"}, "finish_reason": "stop"}},
		})
	}))
	defer srv.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "sk-crew-transient", Out: &bytes.Buffer{}})
	res, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:       "transient-worker",
		LeaseFiles: []string{"work.go"},
		Task:       "Initialize work",
	})
	if err != nil {
		t.Fatalf("CreateCrew: %v", err)
	}

	// Follow-up MessageCrew hits non-context API error -> MUST return err while preserving active session & lease!
	if _, merr := r.MessageCrew(context.Background(), res.CrewID, "trigger transient error"); merr == nil {
		t.Fatal("expected MessageCrew to return transient error")
	}

	cs, err := eng.GetCrewSession(res.CrewID)
	if err != nil || cs.Status != "active" {
		t.Fatalf("expected crew session to remain active after transient error, got %+v err=%v", cs, err)
	}
	leases, err := eng.Mount().ListActiveLeases()
	if err != nil || len(leases) != 1 || leases[0].CrewID != res.CrewID {
		t.Fatalf("expected crew lease to remain held after transient error, got %+v err=%v", leases, err)
	}

	// Subsequent MessageCrew succeeds cleanly and CloseCrew releases lease.
	reply, err := r.MessageCrew(context.Background(), res.CrewID, "retry task")
	if err != nil || reply != "crew ok" {
		t.Fatalf("expected retry MessageCrew to succeed, got reply=%q err=%v", reply, err)
	}
	if err := r.CloseCrew(res.CrewID, "completed"); err != nil {
		t.Fatalf("CloseCrew: %v", err)
	}
}
