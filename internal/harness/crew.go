package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	tetherctx "github.com/crossparties/tether/internal/context"
	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/hooks"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/mount"
	"github.com/crossparties/tether/internal/safety"
	"github.com/google/uuid"
)

// CrewCreateRequest configures a new Crew Session spawned by Main.
type CrewCreateRequest struct {
	Name             string        `json:"name"`
	Task             string        `json:"task"`
	SkillName        string        `json:"skill_name,omitempty"`
	FocusFiles       []string      `json:"focus_files,omitempty"`
	LeaseFiles       []string      `json:"lease_files,omitempty"`
	LeaseTTL         time.Duration `json:"lease_ttl,omitempty"`
	PurposeCategory  string        `json:"purpose_category,omitempty"`
	Mode             string        `json:"mode,omitempty"` // "same-provider" (default) | "different-provider" | "readonly"
	Provider         string        `json:"provider,omitempty"`
	Model            string        `json:"model,omitempty"`
	BaseURL          string        `json:"base_url,omitempty"`
	APIKey           string        `json:"api_key,omitempty"`
	DelegateCritical bool          `json:"delegate_critical,omitempty"`
}

// CrewCreateResult summarizes a newly created Crew Session and its initial turn output.
type CrewCreateResult struct {
	CrewID       string   `json:"crew_id"`
	Name         string   `json:"name"`
	SameProvider bool     `json:"same_provider"`
	LeaseFiles   []string `json:"lease_files,omitempty"`
	Output       string   `json:"output"`
}

type crewHandle struct {
	id         string
	name       string
	runner     *Runner
	leaseFiles []string
	closed     bool
	mu         sync.Mutex
}

func crewToolsSchema() []openAITool {
	return []openAITool{
		{
			Type: "function",
			Function: openAIFunction{
				Name:        "crew_create",
				Description: "Spawn an isolated Crew Session with optional SKILL.md, focus_files, and exclusive file ownership lease_files.",
				Parameters: mcp.InputSchema{
					Type: "object",
					Properties: map[string]mcp.Property{
						"name":              {Type: "string", Description: "Short descriptive name for this crew worker"},
						"task":              {Type: "string", Description: "Specific task assignment for the crew"},
						"skill_name":        {Type: "string", Description: "Optional skill name under .agents/skills/<name>/SKILL.md"},
						"focus_files":       {Type: "string", Description: "Optional comma-separated relative workspace paths to focus"},
						"lease_files":       {Type: "string", Description: "Optional comma-separated relative paths for exclusive write lease lock"},
						"purpose_category":  {Type: "string", Description: "Optional category: implement | review | test | plan-critic"},
						"mode":              {Type: "string", Description: "same-provider (default) | different-provider | readonly"},
						"provider":          {Type: "string", Description: "Optional override provider for different-provider mode"},
						"model":             {Type: "string", Description: "Optional override model for different-provider mode"},
						"delegate_critical": {Type: "string", Description: "Optional 'true' to delegate routine non-guarded workspace writes to crew"},
					},
					Required: []string{"name", "task"},
				},
			},
		},
		{
			Type: "function",
			Function: openAIFunction{
				Name:        "crew_message",
				Description: "Send a follow-up message to an active Crew Session by crew_id.",
				Parameters: mcp.InputSchema{
					Type: "object",
					Properties: map[string]mcp.Property{
						"crew_id": {Type: "string", Description: "Active crew session ID"},
						"message": {Type: "string", Description: "Follow-up prompt or instruction"},
					},
					Required: []string{"crew_id", "message"},
				},
			},
		},
		{
			Type: "function",
			Function: openAIFunction{
				Name:        "crew_close",
				Description: "Close an active Crew Session, persist summary, and release any held file lease locks.",
				Parameters: mcp.InputSchema{
					Type: "object",
					Properties: map[string]mcp.Property{
						"crew_id": {Type: "string", Description: "Crew session ID to close"},
						"summary": {Type: "string", Description: "Optional concise completion summary"},
					},
					Required: []string{"crew_id"},
				},
			},
		},
	}
}

func isCrewToolName(name string) bool {
	switch name {
	case "crew_create", "crew_message", "crew_close":
		return true
	}
	return false
}

// CreateCrew spawns an isolated Crew Session (`depth == 1`) sharing the Main
// session's workspace engine, KV-cache prefix (for Same-Provider mode), and
// isolated UsageTracker (`usage_id: "crew:<id>"`), then executes its initial task.
func (r *Runner) CreateCrew(ctx context.Context, req CrewCreateRequest) (CrewCreateResult, error) {
	if r == nil {
		return CrewCreateResult{}, fmt.Errorf("runner is nil")
	}
	if r.depth >= 1 {
		return CrewCreateResult{}, fmt.Errorf("DENIED BY TETHER GUARD: nested crew spawning blocked at depth >= 1")
	}
	task := strings.TrimSpace(req.Task)
	if task == "" {
		return CrewCreateResult{}, fmt.Errorf("crew task is required")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "crew-worker"
	}

	crewID := "crew-" + uuid.New().String()[:8]
	modeNorm := strings.ToLower(strings.TrimSpace(req.Mode))
	if modeNorm == "" {
		modeNorm = "same-provider"
	}
	readOnly := modeNorm == "readonly"
	sameProvider := modeNorm == "same-provider" &&
		(strings.TrimSpace(req.Provider) == "" || strings.EqualFold(req.Provider, r.ProviderName())) &&
		(strings.TrimSpace(req.Model) == "" || strings.EqualFold(req.Model, r.ModelName())) &&
		strings.TrimSpace(req.BaseURL) == ""

	// Ensure Main's Tier 0-3 head & tools are frozen before inheriting into Same-Provider Crew.
	mainHead, mainTools := r.ensureFrozenHead()
	r.mu.RLock()
	mainSnap := r.cfg
	mainPool := r.keyPool
	mainPinned := r.pinnedKeyID
	mainClient := r.client
	mainBaselineCov := r.baselineRepoMapFiles
	threadID := r.effectiveThreadIDLocked()
	tracker := r.usageTracker
	r.mu.RUnlock()

	ws := "."
	if r.engine != nil && r.engine.Workspace != "" {
		ws = r.engine.Workspace
	}

	var mountEng *mount.Engine
	if r.engine != nil {
		mountEng = r.engine.Mount()
	} else {
		mountEng = mount.NewEngine(ws)
	}

	skillBody, skillPath, err := loadSkillBodySafe(mountEng, ws, req.SkillName)
	if err != nil {
		return CrewCreateResult{}, err
	}

	leaseTTL := req.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = mount.DefaultLeaseTTL
	}

	if r.engine != nil {
		if err := r.engine.CreateCrewSession(core.CrewSession{
			ID:              crewID,
			ParentThreadID:  threadID,
			Name:            name,
			SkillName:       strings.TrimSpace(req.SkillName),
			PurposeCategory: strings.TrimSpace(req.PurposeCategory),
			Status:          "active",
			LeaseFiles:      req.LeaseFiles,
		}); err != nil {
			return CrewCreateResult{}, err
		}
	}

	if len(req.LeaseFiles) > 0 {
		if err := mountEng.AcquireLeaseLock(crewID, req.LeaseFiles, leaseTTL); err != nil {
			if r.engine != nil {
				_ = r.engine.CloseCrewSession(crewID, "aborted", err.Error())
			}
			return CrewCreateResult{}, err
		}
	}

	// Build isolated Crew Runner.
	crewCfg := mainSnap
	if !sameProvider {
		if strings.TrimSpace(req.Provider) != "" {
			crewCfg.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
			if p, ok := presets[crewCfg.Provider]; ok && strings.TrimSpace(req.BaseURL) == "" {
				crewCfg.BaseURL = p.BaseURL
				if strings.TrimSpace(req.Model) == "" {
					crewCfg.Model = p.DefaultModel
				}
			}
		}
		if strings.TrimSpace(req.Model) != "" {
			crewCfg.Model = strings.TrimSpace(req.Model)
		}
		if strings.TrimSpace(req.BaseURL) != "" {
			crewCfg.BaseURL = strings.TrimSpace(req.BaseURL)
		}
		if strings.TrimSpace(req.APIKey) != "" {
			crewCfg.APIKey = strings.TrimSpace(req.APIKey)
		}
	}

	crewMCP := mcp.NewServer(r.engine)
	crewMCP.SetActorID(crewID)
	crewHooks := hooks.NewHandler(r.engine)
	crewHooks.SetActorID(crewID)
	if readOnly {
		crewMCP.SetReadOnly(true)
		crewHooks.SetReadOnly(true)
	}
	if readOnly || !req.DelegateCritical {
		crewMCP.SetForceAskWrites(true)
	}

	// Inherit AskPolicy from Main and configure Crew-specific safety gate.
	r.inheritAskPolicy(crewMCP, readOnly, req.DelegateCritical)
	crewMCP.SetAutoApprovePolicy(r.crewAutoApprovePolicy(readOnly, req.DelegateCritical))

	crewKeyPool := mainPool
	crewPinned := mainPinned
	if !sameProvider {
		crewPinned = ""
		if strings.TrimSpace(req.APIKey) != "" || strings.TrimSpace(req.BaseURL) != "" {
			crewKeyPool = NewKeyPoolWithCandidates(r.engine, mainClient, []KeyCandidate{{
				Provider: crewCfg.Provider,
				Entry: ProviderKeyEntry{
					ID:       crewID + "-key",
					Label:    crewCfg.Provider,
					APIKey:   crewCfg.APIKey,
					BaseURL:  crewCfg.BaseURL,
					Model:    crewCfg.Model,
					Priority: 1,
					Enabled:  true,
				},
			}})
		}
	}

	crewRunner := &Runner{
		engine:               r.engine,
		hooks:                crewHooks,
		mcp:                  crewMCP,
		cfg:                  crewCfg,
		client:               mainClient,
		agentType:            "crew:" + name,
		depth:                r.depth + 1,
		crewID:               crewID,
		usageID:              "crew:" + crewID,
		threadID:             threadID,
		sameProviderCrew:     sameProvider,
		readOnlyCrew:         readOnly,
		keyPool:              crewKeyPool,
		pinnedKeyID:          crewPinned,
		usageTracker:         tracker,
		lastUserActivity:     time.Now(),
		baselineRepoMapFiles: mainBaselineCov,
	}

	if sameProvider {
		// Same-Provider Mode 1: Exact identical tools + messages[0] inherited from Main!
		crewRunner.frozenSystemPrompt = mainHead
		toolsCopy := make([]openAITool, len(mainTools))
		copy(toolsCopy, mainTools)
		crewRunner.frozenTools = toolsCopy
	} else {
		// Different-Provider / ReadOnly Mode 2: compact focused system prompt & subset tools.
		crewRunner.frozenSystemPrompt = buildFocusedCrewSystemPrompt(ws, name, req.PurposeCategory, readOnly)
		crewRunner.mu.Lock()
		crewRunner.frozenTools = crewRunner.buildSessionToolsLocked()
		crewRunner.mu.Unlock()
	}

	handle := &crewHandle{
		id:         crewID,
		name:       name,
		runner:     crewRunner,
		leaseFiles: append([]string(nil), req.LeaseFiles...),
	}
	r.mu.Lock()
	if r.crews == nil {
		r.crews = make(map[string]*crewHandle)
	}
	r.crews[crewID] = handle
	r.mu.Unlock()

	assignment := buildCrewAssignmentMessage(mountEng, ws, req, skillBody, skillPath)
	initialPrompt := assignment + "\n\n" + task

	out, runErr := r.runCrewTurnSafely(ctx, handle, initialPrompt)
	if runErr != nil {
		// Guaranteed cleanup on panic/failure/abort.
		_ = r.abortCrew(handle, runErr.Error())
		return CrewCreateResult{}, runErr
	}

	return CrewCreateResult{
		CrewID:       crewID,
		Name:         name,
		SameProvider: sameProvider,
		LeaseFiles:   req.LeaseFiles,
		Output:       out,
	}, nil
}

// MessageCrew sends a follow-up message to an active Crew Session.
func (r *Runner) MessageCrew(ctx context.Context, crewID, message string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("runner is nil")
	}
	if r.depth >= 1 {
		return "", fmt.Errorf("DENIED BY TETHER GUARD: nested crew spawning blocked at depth >= 1")
	}
	crewID = strings.TrimSpace(crewID)
	message = strings.TrimSpace(message)
	if crewID == "" || message == "" {
		return "", fmt.Errorf("crew_id and message are required")
	}

	r.mu.RLock()
	h := r.crews[crewID]
	r.mu.RUnlock()
	if h == nil {
		return "", fmt.Errorf("unknown crew_id %q", crewID)
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return "", fmt.Errorf("crew %q is already closed", crewID)
	}

	if len(h.leaseFiles) > 0 && r.engine != nil {
		_ = r.engine.Mount().HeartbeatLease(crewID, mount.DefaultLeaseTTL)
	}

	out, err := r.runCrewTurnSafely(ctx, h, message)
	if err != nil {
		_ = r.abortCrew(h, err.Error())
		return "", err
	}
	return out, nil
}

// CloseCrew closes an active Crew Session, records its summary, and releases its lease lock.
func (r *Runner) CloseCrew(crewID, summary string) error {
	if r == nil {
		return nil
	}
	crewID = strings.TrimSpace(crewID)
	if crewID == "" {
		return fmt.Errorf("crew_id required")
	}

	r.mu.RLock()
	h := r.crews[crewID]
	r.mu.RUnlock()

	if h != nil {
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
	}

	var relErr error
	if r.engine != nil {
		relErr = r.engine.Mount().ReleaseLeaseLock(crewID)
		if cerr := r.engine.CloseCrewSession(crewID, "closed", summary); cerr != nil && relErr == nil {
			relErr = cerr
		}
	}
	return relErr
}

// ForceReleaseCrewLease forcefully releases any lease lock held by crewID.
func (r *Runner) ForceReleaseCrewLease(crewID string) error {
	if r == nil || r.engine == nil {
		return nil
	}
	return r.engine.Mount().ReleaseLeaseLock(crewID)
}

// CloseAllCrews releases all active Crew lease locks held under this Runner.
func (r *Runner) CloseAllCrews() {
	if r == nil {
		return
	}
	r.mu.RLock()
	handles := make([]*crewHandle, 0, len(r.crews))
	for _, h := range r.crews {
		handles = append(handles, h)
	}
	r.mu.RUnlock()
	for _, h := range handles {
		_ = r.CloseCrew(h.id, "session closed")
	}
}

func (r *Runner) runCrewTurnSafely(ctx context.Context, h *crewHandle, prompt string) (out string, err error) {
	// Guaranteed lease release if context is cancelled or a panic occurs inside a Crew turn.
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("crew %s panicked: %v", h.id, rec)
			_ = r.abortCrew(h, err.Error())
		} else if ctx.Err() != nil && err != nil {
			_ = r.abortCrew(h, ctx.Err().Error())
		}
	}()
	return h.runner.runWithOptions(ctx, prompt, r.outWriter(), nil)
}

func (r *Runner) abortCrew(h *crewHandle, reason string) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	if r.engine != nil {
		_ = r.engine.Mount().ReleaseLeaseLock(h.id)
		return r.engine.CloseCrewSession(h.id, "aborted", reason)
	}
	return nil
}

func (r *Runner) inheritAskPolicy(dst *mcp.Server, readOnly, delegateCritical bool) {
	if r.mcp == nil || dst == nil {
		return
	}
	forceAsk := readOnly || !delegateCritical
	if fn := r.mcp.AskPolicyWithReason(); fn != nil {
		dst.SetAskPolicyWithReason(func(d mcp.Decision) (mcp.Verdict, string, bool) {
			if forceAsk {
				d.ForceAsk = true
			}
			return fn(d)
		})
		return
	}
	if fn := r.mcp.AskPolicy(); fn != nil {
		dst.SetAskPolicy(func(d mcp.Decision) (mcp.Verdict, bool) {
			if forceAsk {
				d.ForceAsk = true
			}
			return fn(d)
		})
	}
}

func (r *Runner) crewAutoApprovePolicy(readOnly, delegateCritical bool) func(mcp.Decision) bool {
	return func(d mcp.Decision) bool {
		ws := "."
		mode := "code"
		if r.engine != nil {
			mode = r.engine.GetMode()
			if r.engine.Workspace != "" {
				ws = r.engine.Workspace
			}
		}
		if readOnly {
			mode = "review"
		}
		target := d.Path
		if target == "" {
			target = d.Command
		}
		if target == "" {
			target = d.Label
		}
		// Never auto-approve write/edit when crew is readOnly.
		if readOnly && (d.Tool == "write_file" || d.Tool == "edit_file" || d.Tool == "run_command") {
			if d.Tool == "run_command" {
				ok, _ := safety.IsReadOnlyCommand(d.Command)
				return ok
			}
			return false
		}

		// Non-critical read/validation tools are safe to auto-run.
		if d.Tool == "run_command" {
			if ok, _ := safety.IsReadOnlyCommand(d.Command); ok {
				return true
			}
			if ok, _ := safety.IsValidationCommand(d.Command); ok {
				return true
			}
			return false
		}

		if d.Tool == "write_file" || d.Tool == "edit_file" {
			if !delegateCritical {
				// Un-delegated critical code edits must escalate via AskPolicy.
				return false
			}
			risk, _ := safety.ClassifyRisk(d.Tool, target)
			if risk == safety.RiskLow {
				return true
			}
			if mode == "plan" {
				return safety.IsPlanArtifactTarget(target, ws)
			}
			ok, _ := safety.IsRoutineWorkspaceEdit(d.Tool, target, ws)
			return ok
		}

		ok, _ := safety.IsNonCriticalTool(mode, d.Tool, target, d.Command, ws)
		return ok
	}
}

func (r *Runner) executeCrewTool(ctx context.Context, name string, rawArgs string) (string, bool) {
	if r.depth >= 1 {
		return "DENIED BY TETHER GUARD: nested crew spawning blocked at depth >= 1", true
	}

	switch name {
	case "crew_create":
		var raw struct {
			Name             string          `json:"name"`
			Task             string          `json:"task"`
			SkillName        string          `json:"skill_name"`
			FocusFiles       json.RawMessage `json:"focus_files"`
			LeaseFiles       json.RawMessage `json:"lease_files"`
			PurposeCategory  string          `json:"purpose_category"`
			Mode             string          `json:"mode"`
			Provider         string          `json:"provider"`
			Model            string          `json:"model"`
			DelegateCritical json.RawMessage `json:"delegate_critical"`
		}
		if err := json.Unmarshal([]byte(rawArgs), &raw); err != nil {
			return fmt.Sprintf("invalid crew_create params: %v", err), true
		}
		req := CrewCreateRequest{
			Name:             raw.Name,
			Task:             raw.Task,
			SkillName:        raw.SkillName,
			FocusFiles:       parseFlexibleStringList(raw.FocusFiles),
			LeaseFiles:       parseFlexibleStringList(raw.LeaseFiles),
			PurposeCategory:  raw.PurposeCategory,
			Mode:             raw.Mode,
			Provider:         raw.Provider,
			Model:            raw.Model,
			DelegateCritical: parseFlexibleBool(raw.DelegateCritical),
		}
		res, err := r.CreateCrew(ctx, req)
		if err != nil {
			return err.Error(), true
		}
		out, _ := json.MarshalIndent(res, "", "  ")
		return string(out), false

	case "crew_message":
		var raw struct {
			CrewID  string `json:"crew_id"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(rawArgs), &raw); err != nil {
			return fmt.Sprintf("invalid crew_message params: %v", err), true
		}
		reply, err := r.MessageCrew(ctx, raw.CrewID, raw.Message)
		if err != nil {
			return err.Error(), true
		}
		return reply, false

	case "crew_close":
		var raw struct {
			CrewID  string `json:"crew_id"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(rawArgs), &raw); err != nil {
			return fmt.Sprintf("invalid crew_close params: %v", err), true
		}
		if err := r.CloseCrew(raw.CrewID, raw.Summary); err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("crew %s closed and lease locks released", raw.CrewID), false

	default:
		return fmt.Sprintf("unknown crew tool: %s", name), true
	}
}

func buildFocusedCrewSystemPrompt(workspace, name, purpose string, readOnly bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are Tether Crew worker %q (workspace: %s).", name, workspace)
	if purpose != "" {
		fmt.Fprintf(&b, " Purpose: %s.", purpose)
	}
	if readOnly {
		b.WriteString(" You are strictly READ-ONLY; never attempt mutations.")
	}
	b.WriteString(" Stay focused on your assigned task and return a concise technical result.")
	return b.String()
}

func buildCrewAssignmentMessage(mountEng *mount.Engine, workspace string, req CrewCreateRequest, skillBody, skillPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Crew Assignment: %s]\n", req.Name)
	if req.PurposeCategory != "" {
		fmt.Fprintf(&b, "- Purpose: %s\n", req.PurposeCategory)
	}
	if len(req.LeaseFiles) > 0 {
		fmt.Fprintf(&b, "- Exclusive Lease Files: %s\n", strings.Join(req.LeaseFiles, ", "))
	}
	if skillBody != "" {
		fmt.Fprintf(&b, "\n<skill name=%q path=%q>\n%s\n</skill>\n", strings.TrimSpace(req.SkillName), skillPath, skillBody)
	}
	if len(req.FocusFiles) > 0 {
		b.WriteString("\n<focus_files>\n")
		for _, f := range req.FocusFiles {
			if content, cleanRel, err := tetherctx.SafeReadWorkspaceFileWithMount(mountEng, workspace, f, 24*1024); err == nil {
				fmt.Fprintf(&b, "<file path=%q>\n%s\n</file>\n", cleanRel, content)
			} else {
				fmt.Fprintf(&b, "- %s\n", f)
			}
		}
		b.WriteString("</focus_files>")
	}
	return strings.TrimRight(b.String(), "\n")
}

// loadSkillBodySafe resolves <workspace>/.agents/skills/<skillName>/SKILL.md through
// the DB-bound Mount Engine, rejecting path traversal, symlinks, and escapes outside .agents/skills/.
func loadSkillBodySafe(mountEng *mount.Engine, workspace, skillName string) (string, string, error) {
	skillName = strings.TrimSpace(skillName)
	if skillName == "" {
		return "", "", nil
	}
	if skillName == "." || skillName == ".." || strings.Contains(skillName, "..") || strings.ContainsAny(skillName, `/\`) {
		return "", "", fmt.Errorf("invalid skill name %q", skillName)
	}
	if mountEng == nil {
		mountEng = mount.NewEngine(workspace)
	}
	rel := ".agents/skills/" + skillName + "/SKILL.md"
	dec := mountEng.AuthorizePath(rel, mount.OpRead, "code")
	if !dec.Allowed || dec.IsDir || !strings.HasPrefix(dec.RelPath, ".agents/skills/") {
		reason := dec.Reason
		if reason == "" {
			reason = fmt.Sprintf("skill path %q resolves outside .agents/skills/", rel)
		}
		return "", "", fmt.Errorf("invalid skill %q: %s", skillName, reason)
	}
	info, err := os.Lstat(dec.CanonicalPath)
	if err != nil {
		return "", "", fmt.Errorf("read skill %q: %w", skillName, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("refusing symlinked skill file for %q", skillName)
	}
	data, err := os.ReadFile(dec.CanonicalPath)
	if err != nil {
		return "", "", fmt.Errorf("read skill %q: %w", skillName, err)
	}
	return clipTextRunes(strings.TrimSpace(string(data)), 8192), dec.RelPath, nil
}

func parseFlexibleStringList(raw json.RawMessage) []string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []string
		if err := json.Unmarshal(raw, &arr); err == nil {
			var out []string
			for _, s := range arr {
				if t := strings.TrimSpace(s); t != "" {
					out = append(out, t)
				}
			}
			return out
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var out []string
		for _, part := range strings.Split(s, ",") {
			if t := strings.TrimSpace(part); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

func parseFlexibleBool(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.EqualFold(strings.TrimSpace(s), "true")
	}
	return false
}
