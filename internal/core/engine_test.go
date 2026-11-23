package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	e, err := NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestNewEngine(t *testing.T) {
	dir := t.TempDir()
	e, err := NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine returned error: %v", err)
	}
	defer e.Close()

	if e.SessionID == "" {
		t.Error("SessionID should be non-empty after NewEngine")
	}
	if _, err := os.Stat(filepath.Join(dir, "session.db")); os.IsNotExist(err) {
		t.Error("session.db not created in workspace")
	}
}

func TestProjectSettings(t *testing.T) {
	e := newTestEngine(t)

	if err := e.SetProjectSetting("lang", "go"); err != nil {
		t.Fatalf("SetProjectSetting: %v", err)
	}
	got, err := e.GetProjectSetting("lang")
	if err != nil {
		t.Fatalf("GetProjectSetting: %v", err)
	}
	if got != "go" {
		t.Errorf("got %q, want %q", got, "go")
	}
	// missing key → empty string, no error
	missing, err := e.GetProjectSetting("no_such_key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if missing != "" {
		t.Errorf("missing key got %q, want empty", missing)
	}
}

func TestSession(t *testing.T) {
	e := newTestEngine(t)

	const want = "completed initial scaffolding"
	if err := e.SetSessionSummary(want); err != nil {
		t.Fatalf("SetSessionSummary: %v", err)
	}
	got, err := e.GetSessionSummary()
	if err != nil || got != want {
		t.Errorf("GetSessionSummary = %q, %v; want %q, nil", got, err, want)
	}

	if err := e.CompleteSession(); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}
	var status string
	e.DB.QueryRow(`SELECT status FROM sessions WHERE id = ?`, e.SessionID).Scan(&status)
	if status != "completed" {
		t.Errorf("status = %q, want completed", status)
	}
}

func TestDecisions(t *testing.T) {
	e := newTestEngine(t)

	if err := e.AddDecision("auth", "use JWT", "stateless"); err != nil {
		t.Fatalf("AddDecision: %v", err)
	}
	decisions, err := e.GetDecisions()
	if err != nil || len(decisions) != 1 {
		t.Fatalf("GetDecisions: %v, len=%d", err, len(decisions))
	}
	d := decisions[0]
	if d.Topic != "auth" || d.Decision != "use JWT" || d.Reason != "stateless" {
		t.Errorf("decision mismatch: %+v", d)
	}
}

func TestPatterns(t *testing.T) {
	e := newTestEngine(t)

	if err := e.AddPattern("do", "snake_case for DB columns"); err != nil {
		t.Fatalf("AddPattern: %v", err)
	}
	patterns, err := e.GetPatterns()
	if err != nil || len(patterns) != 1 {
		t.Fatalf("GetPatterns: %v, len=%d", err, len(patterns))
	}
	if patterns[0].Type != "do" || patterns[0].Description != "snake_case for DB columns" {
		t.Errorf("pattern mismatch: %+v", patterns[0])
	}
}

func TestKBIndexAndSearch(t *testing.T) {
	e := newTestEngine(t)

	fpath := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(fpath, []byte("tether engine uses sqlite for persistent session storage"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := e.IndexFile(fpath); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	results, err := e.SearchKB("sqlite", 5)
	if err != nil {
		t.Fatalf("SearchKB: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("SearchKB returned no results")
	}
	if results[0].FilePath != fpath {
		t.Errorf("FilePath = %q, want %q", results[0].FilePath, fpath)
	}
}

func TestTodos(t *testing.T) {
	e := newTestEngine(t)

	id, err := e.AddTodo("write tests", "cover all critical paths")
	if err != nil || id == 0 {
		t.Fatalf("AddTodo: id=%d, err=%v", id, err)
	}

	pending, _ := e.ListTodos(false)
	if len(pending) != 1 || pending[0].Status != "pending" {
		t.Fatalf("expected 1 pending, got %+v", pending)
	}

	if err := e.CompleteTodo(int(id)); err != nil {
		t.Fatalf("CompleteTodo: %v", err)
	}

	pending, _ = e.ListTodos(false)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after complete, got %d", len(pending))
	}

	all, _ := e.ListTodos(true)
	if len(all) != 1 || all[0].Status != "completed" {
		t.Errorf("expected 1 completed, got %+v", all)
	}
}

func TestModeGating(t *testing.T) {
	e := newTestEngine(t)

	e.SetMode("plan")
	if e.IsWriteAllowed() {
		t.Error("IsWriteAllowed() true in plan mode")
	}
	if e.GetMode() != "plan" {
		t.Errorf("GetMode() = %q, want plan", e.GetMode())
	}

	e.SetMode("code")
	if !e.IsWriteAllowed() {
		t.Error("IsWriteAllowed() false in code mode")
	}
}

func TestEngine_ExecuteSQL_And_Optimizations(t *testing.T) {
	e := newTestEngine(t)

	// 1. Verify PRAGMA user_version fast-path
	var userVer int
	if err := e.DB.QueryRow("PRAGMA user_version;").Scan(&userVer); err != nil {
		t.Fatalf("QueryRow user_version: %v", err)
	}
	if userVer != SchemaVersion {
		t.Errorf("expected user_version = %d, got %d", SchemaVersion, userVer)
	}

	// 2. Verify WAL mode and busy timeout
	var journalMode string
	if err := e.DB.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		t.Fatalf("QueryRow journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("expected journal_mode wal, got %s", journalMode)
	}

	// 3. Test ExecuteSQL SELECT
	res, err := e.ExecuteSQL("SELECT key, value FROM project_settings")
	if err != nil {
		t.Fatalf("ExecuteSQL select: %v", err)
	}
	rows, ok := res.([]map[string]interface{})
	if !ok {
		t.Fatalf("expected []map[string]interface{}, got %T", res)
	}
	_ = rows

	// 4. Test ExecuteSQL INSERT
	insertRes, err := e.ExecuteSQL("INSERT INTO project_settings (key, value) VALUES ('test_opt', '123')")
	if err != nil {
		t.Fatalf("ExecuteSQL insert: %v", err)
	}
	insertMap, ok := insertRes.(map[string]interface{})
	if !ok || insertMap["rows_affected"].(int64) != 1 {
		t.Fatalf("expected rows_affected = 1, got %+v", insertRes)
	}

	// 5. Test ExecuteSQL Safety Guard against DROP TABLE
	_, err = e.ExecuteSQL("DROP TABLE sessions")
	if err == nil {
		t.Errorf("expected error when dropping core table, got nil")
	}
	if !strings.Contains(err.Error(), "safety guard: dropping core substrate table") {
		t.Errorf("expected safety guard error message, got %v", err)
	}
}

func TestRecordAndApproveProposal(t *testing.T) {
	e := newTestEngine(t)

	// No proposal initially.
	p, err := e.GetPendingProposal()
	if err != nil {
		t.Fatalf("GetPendingProposal: %v", err)
	}
	if p != nil {
		t.Fatalf("expected no pending proposal, got %+v", p)
	}

	blocked, _, err := e.ProposalGateBlocked()
	if err != nil {
		t.Fatalf("ProposalGateBlocked: %v", err)
	}
	if blocked {
		t.Fatal("gate should be open with no pending proposal")
	}

	id, err := e.RecordProposal("tether_qa", "add adapter tests")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if id == 0 {
		t.Fatal("RecordProposal returned id 0")
	}

	p, err = e.GetPendingProposal()
	if err != nil {
		t.Fatalf("GetPendingProposal: %v", err)
	}
	if p == nil {
		t.Fatal("expected a pending proposal")
	}
	if p.Role != "tether_qa" || p.Proposal != "add adapter tests" || p.Status != "pending" {
		t.Fatalf("unexpected proposal: %+v", p)
	}

	blocked, rec, err := e.ProposalGateBlocked()
	if err != nil {
		t.Fatalf("ProposalGateBlocked: %v", err)
	}
	if !blocked || rec == nil || rec.ID != id {
		t.Fatalf("gate should be closed by proposal #%d", id)
	}

	affected, err := e.ApproveProposal()
	if err != nil {
		t.Fatalf("ApproveProposal: %v", err)
	}
	if affected != 1 {
		t.Fatalf("ApproveProposal affected %d rows, want 1", affected)
	}

	blocked, _, err = e.ProposalGateBlocked()
	if err != nil {
		t.Fatalf("ProposalGateBlocked after approve: %v", err)
	}
	if blocked {
		t.Fatal("gate should reopen after approval")
	}
}

func TestRecordAndRejectProposal(t *testing.T) {
	e := newTestEngine(t)

	if _, err := e.RecordProposal("tether_architect", "rewrite engine"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	affected, err := e.RejectProposal()
	if err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	if affected != 1 {
		t.Fatalf("RejectProposal affected %d rows, want 1", affected)
	}

	p, err := e.GetPendingProposal()
	if err != nil {
		t.Fatalf("GetPendingProposal: %v", err)
	}
	if p != nil {
		t.Fatalf("expected no pending proposal after reject, got %+v", p)
	}
}

func TestProposalGate_OldestFirst(t *testing.T) {
	e := newTestEngine(t)

	if _, err := e.RecordProposal("tether_qa", "first"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if _, err := e.RecordProposal("tether_frontend", "second"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	p, err := e.GetPendingProposal()
	if err != nil {
		t.Fatalf("GetPendingProposal: %v", err)
	}
	if p == nil || p.Proposal != "first" {
		t.Fatalf("expected oldest proposal first, got %+v", p)
	}
}

// Every required table must exist after a fresh initialisation.
func TestMigrateCreatesAllRequiredTables(t *testing.T) {
	dir := t.TempDir()
	e, err := NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer e.Close()

	missing, err := e.missingTables()
	if err != nil {
		t.Fatalf("missingTables: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("after init, missing tables: %v", missing)
	}
}

// A database stamped with the current SchemaVersion but lacking a table must be
// repaired on the next open. This is the regression that let pending_proposals
// stay absent while PRAGMA user_version claimed the schema was current.
func TestMigrateRepairsSchemaDrift(t *testing.T) {
	dir := t.TempDir()

	// Build a healthy database first.
	e, err := NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// Simulate the bug: drop a table while user_version still says "current".
	if _, err := e.DB.Exec("DROP TABLE pending_proposals"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	var version int
	if err := e.DB.QueryRow("PRAGMA user_version;").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version < SchemaVersion {
		t.Fatalf("user_version = %d, want >= %d so the fast path is exercised", version, SchemaVersion)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopening must notice the missing table and recreate it.
	reopened, err := NewEngine(dir)
	if err != nil {
		t.Fatalf("reopen NewEngine: %v", err)
	}
	defer reopened.Close()

	missing, err := reopened.missingTables()
	if err != nil {
		t.Fatalf("missingTables: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("schema drift was not repaired, still missing: %v", missing)
	}

	// And the repaired table must actually be usable.
	if _, err := reopened.RecordProposal("tether_qa", "post-repair proposal"); err != nil {
		t.Fatalf("RecordProposal after repair: %v", err)
	}
}

func TestProposalPerIDLifecycle(t *testing.T) {
	e := newTestEngine(t)

	id1, err := e.RecordProposal("tether_qa", "first proposal")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	id2, err := e.RecordProposal("tether_architect", "second proposal")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	all, err := e.ListProposals("pending")
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListProposals returned %d, want 2", len(all))
	}
	if all[0].ID != id1 || all[1].ID != id2 {
		t.Fatalf("ListProposals order = [%d %d], want oldest first [%d %d]", all[0].ID, all[1].ID, id1, id2)
	}

	// Approving one must leave the other pending.
	affected, err := e.ApproveProposalByID(id1)
	if err != nil {
		t.Fatalf("ApproveProposalByID: %v", err)
	}
	if affected != 1 {
		t.Fatalf("ApproveProposalByID affected %d, want 1", affected)
	}

	pending, err := e.ListProposals("pending")
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id2 {
		t.Fatalf("after approving #%d, pending = %+v, want only #%d", id1, pending, id2)
	}

	// The write gate must stay closed because #2 is still undecided.
	if blocked, rec, err := e.ProposalGateBlocked(); err != nil || !blocked || rec.ID != id2 {
		t.Fatalf("gate should still be blocked by #%d (blocked=%v err=%v)", id2, blocked, err)
	}

	// Approving an already-decided proposal is a no-op.
	if affected, err := e.ApproveProposalByID(id1); err != nil || affected != 0 {
		t.Fatalf("re-approving #%d affected %d (err=%v), want 0", id1, affected, err)
	}

	p, err := e.GetProposalByID(id1)
	if err != nil {
		t.Fatalf("GetProposalByID: %v", err)
	}
	if p == nil || p.Status != "approved" {
		t.Fatalf("proposal #%d = %+v, want status approved", id1, p)
	}

	if _, err := e.RejectProposalByID(id2); err != nil {
		t.Fatalf("RejectProposalByID: %v", err)
	}
	if blocked, _, _ := e.ProposalGateBlocked(); blocked {
		t.Fatal("gate should reopen once every proposal is decided")
	}

	missing, err := e.GetProposalByID(99999)
	if err != nil {
		t.Fatalf("GetProposalByID missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("GetProposalByID(99999) = %+v, want nil", missing)
	}
}

// A pending proposal must not lock the workspace forever if it is abandoned.
func TestProposalTTLExpiry(t *testing.T) {
	e := newTestEngine(t)

	id, err := e.RecordProposal("tether_qa", "abandoned proposal")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	blocked, rec, err := e.ProposalGateBlocked()
	if err != nil || !blocked || rec.ID != id {
		t.Fatalf("fresh proposal should block writes (blocked=%v err=%v)", blocked, err)
	}

	// Age the proposal past its TTL, exactly as a forgotten decision would.
	if _, err := e.DB.Exec(`
		UPDATE pending_proposals SET expires_at = datetime('now', '-1 minute') WHERE id = ?`, id); err != nil {
		t.Fatalf("age proposal: %v", err)
	}

	blocked, _, err = e.ProposalGateBlocked()
	if err != nil {
		t.Fatalf("ProposalGateBlocked: %v", err)
	}
	if blocked {
		t.Fatal("an expired proposal must not keep the workspace locked")
	}

	p, err := e.GetProposalByID(id)
	if err != nil {
		t.Fatalf("GetProposalByID: %v", err)
	}
	if p == nil || p.Status != "expired" {
		t.Fatalf("proposal #%d status = %v, want expired", id, p)
	}
}

// A proposal from another session must never block this one.
func TestProposalSessionScoping(t *testing.T) {
	e := newTestEngine(t)

	// Simulate a proposal left behind by an unrelated session.
	if _, err := e.DB.Exec(`
		INSERT INTO pending_proposals (session_id, role, proposal, status, expires_at)
		VALUES ('some-other-session', 'tether_qa', 'stale from elsewhere', 'pending', datetime('now', '+2 hours'))`); err != nil {
		t.Fatalf("insert foreign proposal: %v", err)
	}

	blocked, _, err := e.ProposalGateBlocked()
	if err != nil {
		t.Fatalf("ProposalGateBlocked: %v", err)
	}
	if blocked {
		t.Fatal("a proposal owned by another session must not lock this session")
	}

	// This session's own proposal does block.
	if _, err := e.RecordProposal("tether_qa", "mine"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if blocked, _, _ := e.ProposalGateBlocked(); !blocked {
		t.Fatal("own proposal should block the session")
	}
}

// The TTL is operator-tunable.
func TestProposalTTLSetting(t *testing.T) {
	e := newTestEngine(t)

	if got := e.proposalTTLMinutes(); got != DefaultProposalTTLMinutes {
		t.Fatalf("default TTL = %d, want %d", got, DefaultProposalTTLMinutes)
	}
	if err := e.SetProjectSetting("proposal_ttl_minutes", "15"); err != nil {
		t.Fatalf("SetProjectSetting: %v", err)
	}
	if got := e.proposalTTLMinutes(); got != 15 {
		t.Fatalf("TTL = %d, want 15", got)
	}
	if err := e.SetProjectSetting("proposal_ttl_minutes", "garbage"); err != nil {
		t.Fatalf("SetProjectSetting: %v", err)
	}
	if got := e.proposalTTLMinutes(); got != DefaultProposalTTLMinutes {
		t.Fatalf("invalid TTL should fall back to default, got %d", got)
	}
}

// An expired proposal must not be listed as actionable. Listing raw
// status='pending' rows hid the TTL and showed stale items as live.
func TestListProposalsHidesExpired(t *testing.T) {
	e := newTestEngine(t)

	live, err := e.RecordProposal("tether_qa", "live one")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	stale, err := e.RecordProposal("tether_qa", "stale one")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if _, err := e.DB.Exec(`
		UPDATE pending_proposals SET expires_at = datetime('now', '-1 minute') WHERE id = ?`, stale); err != nil {
		t.Fatalf("age proposal: %v", err)
	}

	pending, err := e.ListProposals("pending")
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != live {
		t.Fatalf("ListProposals = %+v, want only the live proposal #%d", pending, live)
	}

	// The expired row is kept for the audit trail, just not as pending.
	p, err := e.GetProposalByID(stale)
	if err != nil {
		t.Fatalf("GetProposalByID: %v", err)
	}
	if p == nil || p.Status != "expired" {
		t.Fatalf("stale proposal status = %v, want expired", p.Status)
	}
}
