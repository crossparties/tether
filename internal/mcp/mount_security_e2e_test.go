package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossparties/tether/internal/mount"
)

func TestMountSecurity5PointEnforcement(t *testing.T) {
	s, ws := newApprovalTestServer(t)
	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsideSecret, []byte("HOST_SECRET_999"), 0600); err != nil {
		t.Fatal(err)
	}

	seed := map[string]string{
		"cmd/app.go":            "package main\nfunc App() {}\n",
		"docs/guide.md":         "# Guide\npublic knowledge token\n",
		".env":                  "API_KEY=ENV_SECRET_123\n",
		"certs/prod.pem":        "PEM_SECRET_456\n",
		"credentials-prod.json": `{"token":"CRED_SECRET_789"}`,
		"secret_notes.md":       "# Secret Notes\nTOP_SECRET_ALPHA\n",
	}
	for rel, body := range seed {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Symlinks: escape outside workspace AND alias to .env
	if err := os.Symlink(outsideSecret, filepath.Join(ws, "escape_link.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(ws, ".env"), filepath.Join(ws, "env_link.txt")); err != nil {
		t.Fatal(err)
	}

	// Even if the user grants blanket approval ("approveAll"), hard security boundaries MUST hold.
	s.askMu.Lock()
	s.approveAll = true
	s.askMu.Unlock()

	// 1. Point 1: read_file / write_file / edit_file / glob / grep
	readBlocked := []string{
		"../outside.txt",
		outsideSecret,
		"escape_link.txt",
		"env_link.txt",
		".env",
		".tether/session.db",
		".tether/cache.db",
		"certs/prod.pem",
		"credentials-prod.json",
	}
	for _, p := range readBlocked {
		args, _ := json.Marshal(map[string]string{"path": p})
		res, isErr := s.CallTool("read_file", args)
		if !isErr || !strings.Contains(strings.ToLower(res), "access denied") {
			t.Errorf("read_file(%q) must be denied, got isErr=%v res=%q", p, isErr, res)
		}
	}
	// Missing normal file must NOT report access denied; it reports file not found.
	if res, isErr := s.CallTool("read_file", json.RawMessage(`{"path":"cmd/missing.go"}`)); !isErr || !strings.Contains(res, "file not found") {
		t.Errorf("read_file missing file expected 'file not found', got (%q, %v)", res, isErr)
	}

	for _, p := range []string{".env", "../escape.go", "escape_link.txt", ".tether/session.db"} {
		if res, isErr := s.CallTool("write_file", mustJSON(t, map[string]string{"path": p, "content": "pwn"})); !isErr || !strings.Contains(strings.ToLower(res), "access denied") {
			t.Errorf("write_file(%q) must be denied even under approveAll, got (%q, %v)", p, res, isErr)
		}
	}

	// glob and grep must never leak outside files, symlinks, or secrets.
	globOut, isErr := s.CallTool("glob", json.RawMessage(`{"pattern":"**/*"}`))
	if isErr {
		t.Fatalf("glob failed: %s", globOut)
	}
	for _, forbidden := range []string{".env", "escape_link.txt", "env_link.txt", "prod.pem", "credentials-prod.json", "session.db"} {
		if strings.Contains(globOut, forbidden) {
			t.Errorf("glob leaked %q in %q", forbidden, globOut)
		}
	}

	grepOut, isErr := s.CallTool("grep", json.RawMessage(`{"pattern":"SECRET"}`))
	if isErr {
		t.Fatalf("grep failed: %s", grepOut)
	}
	for _, leak := range []string{"ENV_SECRET_123", "PEM_SECRET_456", "CRED_SECRET_789", "HOST_SECRET_999"} {
		if strings.Contains(grepOut, leak) {
			t.Errorf("grep leaked secret %q in %q", leak, grepOut)
		}
	}

	// 2. Point 2: run_command shell guard
	for _, cmd := range []string{
		"cat .env",
		"cat .e*",
		"head *.pem",
		"cat credentials*.json",
		"cat $(echo .env)",
		"echo secret > .env",
		"echo pwn > .tether/session.db",
	} {
		res, isErr := s.CallTool("run_command", mustJSON(t, map[string]string{"command": cmd}))
		if !isErr {
			t.Errorf("run_command(%q) must fail closed, got %q", cmd, res)
		}
	}

	// 3. Point 3: KB FTS5 indexing + ReconcileKB purge after tightening .tether/ignore
	if _, err := s.engine.IndexDirectory(ws, nil); err != nil {
		t.Fatalf("IndexDirectory: %v", err)
	}
	if out, isErr := s.CallTool("kb_search", json.RawMessage(`{"query":"TOP_SECRET_ALPHA"}`)); isErr || !strings.Contains(out, "secret_notes.md") {
		t.Fatalf("expected secret_notes.md indexed before tightening ignore, got (%q, %v)", out, isErr)
	}
	// Tighten .tether/ignore to exclude secret_notes.md and reconcile KB.
	rule, _ := mount.ParseRule("secret_notes.md")
	if err := s.Mount().SaveCustomRules([]mount.Rule{rule}); err != nil {
		t.Fatalf("SaveCustomRules: %v", err)
	}
	purged, err := s.engine.ReconcileKB(nil)
	if err != nil {
		t.Fatalf("ReconcileKB: %v", err)
	}
	if purged != 1 {
		t.Errorf("ReconcileKB purged %d files, want 1", purged)
	}
	if out, isErr := s.CallTool("kb_search", json.RawMessage(`{"query":"TOP_SECRET_ALPHA"}`)); isErr || out != "no results found" {
		t.Errorf("expected 0 results from kb_search after ReconcileKB, got (%q, %v)", out, isErr)
	}
	// Verify kb_documents & kb_fts rows are physically deleted.
	var docCount int
	if err := s.engine.CacheDB().QueryRow(`SELECT COUNT(*) FROM kb_documents WHERE file_path LIKE '%secret_notes.md'`).Scan(&docCount); err != nil || docCount != 0 {
		t.Errorf("expected stale kb_documents rows deleted, got count=%d err=%v", docCount, err)
	}
}

func TestMountedFilesROAndGuardedModes(t *testing.T) {
	s, ws := newApprovalTestServer(t)
	if err := os.WriteFile(filepath.Join(ws, "frozen.txt"), []byte("v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "guarded.txt"), []byte("g1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.Mount().SetMountedMode("frozen.txt", mount.AccessRO, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := s.Mount().SetMountedMode("guarded.txt", mount.AccessGuarded, "manual"); err != nil {
		t.Fatal(err)
	}

	// Enable blanket auto-approve policy; guarded.txt MUST still require interactive approval,
	// and frozen.txt (ro) MUST be denied outright.
	s.SetAutoApprovePolicy(func(Decision) bool { return true })

	if res, isErr := s.CallTool("write_file", json.RawMessage(`{"path":"frozen.txt","content":"v2"}`)); !isErr || !strings.Contains(res, "read-only") {
		t.Errorf("expected ro mount write denied, got (%q, %v)", res, isErr)
	}
	if res, isErr := s.CallTool("read_file", json.RawMessage(`{"path":"frozen.txt"}`)); isErr || !strings.Contains(res, "1: v1") {
		t.Errorf("expected ro mount read allowed, got (%q, %v)", res, isErr)
	}
	if res, isErr := s.CallTool("write_file", json.RawMessage(`{"path":"guarded.txt","content":"g2"}`)); !isErr || !strings.Contains(res, "APPROVAL REQUIRED") {
		t.Errorf("expected guarded mount write to bypass AutoApprovePolicy and require approval, got (%q, %v)", res, isErr)
	}
	// Custom mounted_files ignored rule + ro enforcement also apply inside run_command via CheckShellCommandWithMount.
	if err := s.Mount().SetMountedMode("db_secret.txt", mount.AccessIgnored, "manual"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(ws, "db_secret.txt"), []byte("db-hidden\n"), 0600)
	if res, isErr := s.CallTool("run_command", json.RawMessage(`{"command":"cat db_secret.txt"}`)); !isErr {
		t.Errorf("expected run_command on mounted_files ignored path to fail, got %q", res)
	}
	if res, isErr := s.CallTool("run_command", json.RawMessage(`{"command":"echo x > frozen.txt"}`)); !isErr {
		t.Errorf("expected run_command redirect onto ro file to fail, got %q", res)
	}

	// Hardened read_file: binary, oversized (>1MB), and >32KB single-line safe truncation.
	_ = os.WriteFile(filepath.Join(ws, "blob.bin"), []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01}, 0644)
	if res, isErr := s.CallTool("read_file", json.RawMessage(`{"path":"blob.bin"}`)); !isErr || !strings.Contains(res, "binary") {
		t.Errorf("expected binary file rejected by read_file, got (%q, %v)", res, isErr)
	}
	_ = os.WriteFile(filepath.Join(ws, "huge.txt"), make([]byte, 1024*1024+16), 0644)
	if res, isErr := s.CallTool("read_file", json.RawMessage(`{"path":"huge.txt"}`)); !isErr || !strings.Contains(res, "too large") {
		t.Errorf("expected >1MB file rejected by read_file, got (%q, %v)", res, isErr)
	}
	_ = os.WriteFile(filepath.Join(ws, "longline.txt"), []byte(strings.Repeat("a", 40000)), 0644)
	if res, isErr := s.CallTool("read_file", json.RawMessage(`{"path":"longline.txt"}`)); isErr || !strings.Contains(res, "1: aaa") || !strings.Contains(res, "truncated") || len(res) > 32768 {
		t.Errorf("expected >32KB single line safely truncated inside 32KB, err=%v len=%d", isErr, len(res))
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
