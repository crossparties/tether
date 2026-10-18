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

