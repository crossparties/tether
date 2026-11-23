package core

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if _, err := os.Stat(filepath.Join(dir, ".tether", "session.db")); os.IsNotExist(err) {
		t.Error(".tether/session.db not created in workspace")
	}
	if _, err := os.Stat(filepath.Join(dir, ".tether", "cache.db")); os.IsNotExist(err) {
		t.Error(".tether/cache.db not created in workspace")
	}
	if _, err := os.Stat(filepath.Join(dir, "session.db")); !os.IsNotExist(err) {
		t.Error("legacy root session.db must not be created on fresh init")
	}

	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	for _, want := range []string{".tether/session.db*", ".tether/cache.db*", ".tether/snapshots/", ".tether/PLAN.md"} {
		if !strings.Contains(string(gi), want) {
			t.Errorf(".gitignore missing %q:\n%s", want, string(gi))
		}
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
	if err := e.ReadDB().QueryRow(`SELECT status FROM sessions WHERE id = ?`, e.SessionID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
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

	_ = e.SetMode("plan")
	if e.IsWriteAllowed() {
		t.Error("IsWriteAllowed() true in plan mode")
	}
	if e.GetMode() != "plan" {
		t.Errorf("GetMode() = %q, want plan", e.GetMode())
	}

	_ = e.SetMode("code")
	if !e.IsWriteAllowed() {
		t.Error("IsWriteAllowed() false in code mode")
	}
}

func TestEngine_ExecuteSQL_And_Optimizations(t *testing.T) {
	e := newTestEngine(t)

	// 1. Verify PRAGMA user_version fast-path
	var userVer int
	if err := e.ReadDB().QueryRow("PRAGMA user_version;").Scan(&userVer); err != nil {
		t.Fatalf("QueryRow user_version: %v", err)
	}
	if userVer != SchemaVersion {
		t.Errorf("expected user_version = %d, got %d", SchemaVersion, userVer)
	}

	// 2. Verify WAL mode
	var journalMode string
	if err := e.WriteDB().QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		t.Fatalf("QueryRow journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("expected journal_mode wal, got %s", journalMode)
	}

	// 3. Test ExecuteSQL SELECT (routes via readDB)
	res, err := e.ExecuteSQL("SELECT key, value FROM project_settings")
	if err != nil {
		t.Fatalf("ExecuteSQL select: %v", err)
	}
	rows, ok := res.([]map[string]interface{})
	if !ok {
		t.Fatalf("expected []map[string]interface{}, got %T", res)
	}
	_ = rows

	// 4. Test ExecuteSQL INSERT (routes via writeDB)
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

// Every required table must exist after a fresh initialisation.
func TestMigrateCreatesAllRequiredTables(t *testing.T) {
	e := newTestEngine(t)

	missing, err := e.missingTables()
	if err != nil {
		t.Fatalf("missingTables: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("after init, missing tables: %v", missing)
	}

	wantIndexes := []string{
		"idx_conversation_events_thread_seq",
		"idx_session_context_session_id",
		"idx_todos_session_status",
		"idx_decisions_git_branch",
		"idx_approval_grants_session_expires",
	}
	for _, idx := range wantIndexes {
		var name string
		err := e.ReadDB().QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name = ?`, idx).Scan(&name)
		if err != nil || name != idx {
			t.Errorf("missing expected index %q in session.db (err=%v)", idx, err)
		}
	}
}

// A database stamped with the current SchemaVersion but lacking a table must be
// repaired on the next open.
func TestMigrateRepairsSchemaDrift(t *testing.T) {
	dir := t.TempDir()

	e, err := NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if _, err := e.WriteDB().Exec("DROP TABLE approval_grants"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	var version int
	if err := e.ReadDB().QueryRow("PRAGMA user_version;").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version < SchemaVersion {
		t.Fatalf("user_version = %d, want >= %d so the fast path is exercised", version, SchemaVersion)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

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

	if err := reopened.AddApprovalGrant("internal/core", "path"); err != nil {
		t.Fatalf("AddApprovalGrant after repair: %v", err)
	}
}

func TestLegacyV4AutoMigrationToDotTetherAndV5(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "session.db")

	legacyDB, err := sql.Open("sqlite", sqliteDSN(legacyPath, false))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	v4Schema := `
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		status TEXT DEFAULT 'active',
		mode TEXT DEFAULT 'code',
		git_branch TEXT DEFAULT 'default',
		summary TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE session_context (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT,
		agent TEXT DEFAULT '',
		role TEXT,
		content TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE project_settings (
		key TEXT PRIMARY KEY,
		value TEXT,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE decisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT,
		git_branch TEXT DEFAULT 'default',
		topic TEXT,
		decision TEXT,
		reason TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE learned_patterns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pattern_type TEXT,
		description TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE todos (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT,
		git_branch TEXT DEFAULT 'default',
		description TEXT,
		details TEXT DEFAULT '',
		status TEXT DEFAULT 'pending',
		version INTEGER DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME
	);
	CREATE TABLE kb_documents (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		file_path TEXT,
		chunk_index INTEGER DEFAULT 0,
		content TEXT,
		metadata TEXT DEFAULT '',
		indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(file_path, chunk_index)
	);
	CREATE VIRTUAL TABLE kb_fts USING fts5(content, file_path, content='kb_documents', content_rowid='id');
	CREATE TABLE pending_proposals (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT,
		role TEXT DEFAULT '',
		proposal TEXT DEFAULT '',
		status TEXT DEFAULT 'pending',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	INSERT INTO sessions (id, status, mode, git_branch, summary) VALUES ('v4-sess', 'active', 'code', 'default', 'kept summary');
	INSERT INTO decisions (session_id, git_branch, topic, decision, reason) VALUES ('v4-sess', 'default', 'db', 'dual-pool', 'perf');
	INSERT INTO pending_proposals (session_id, role, proposal) VALUES ('v4-sess', 'tether_qa', 'obsolete');
	PRAGMA user_version = 4;
	`
	if _, err := legacyDB.Exec(v4Schema); err != nil {
		legacyDB.Close()
		t.Fatalf("seed v4 schema: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	e, err := NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine migration failed: %v", err)
	}
	defer e.Close()

	// Root legacy files must be gone; .tether/session.db must exist.
	for _, old := range []string{"session.db", "session.db-wal", "session.db-shm"} {
		if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
			t.Errorf("legacy root file %q still exists after migration", old)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".tether", "session.db")); err != nil {
		t.Fatalf("migrated .tether/session.db missing: %v", err)
	}

	// Legacy session & decision data survived.
	if e.SessionID != "v4-sess" {
		t.Errorf("SessionID = %q, want v4-sess", e.SessionID)
	}
	decisions, err := e.GetDecisions()
	if err != nil || len(decisions) != 1 || decisions[0].Topic != "db" {
		t.Fatalf("legacy decisions lost during migration: %+v, err=%v", decisions, err)
	}

	// user_version bumped to 5, legacy tables removed from session.db.
	var ver int
	if err := e.ReadDB().QueryRow("PRAGMA user_version;").Scan(&ver); err != nil || ver != SchemaVersion {
		t.Fatalf("user_version = %d (err=%v), want %d", ver, err, SchemaVersion)
	}
	for _, removed := range []string{"pending_proposals", "kb_documents", "kb_fts"} {
		var cnt int
		if err := e.ReadDB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, removed).Scan(&cnt); err != nil {
			t.Fatalf("check removed table %s: %v", removed, err)
		}
		if cnt != 0 {
			t.Errorf("legacy table %q still exists in session.db after v4->v5 migration", removed)
		}
	}

	// All required v5 tables exist.
	missing, err := e.missingTables()
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing tables after v4->v5 migration: %v (err=%v)", missing, err)
	}
}

func TestLegacyV4AutoMigration_LiveWALAndBusyFailure(t *testing.T) {
	prevTimeout := legacyCheckpointBusyTimeoutMs
	legacyCheckpointBusyTimeoutMs = 50
	t.Cleanup(func() { legacyCheckpointBusyTimeoutMs = prevTimeout })

	seedDir := t.TempDir()
	legacyPath := filepath.Join(seedDir, "session.db")

	writerDB, err := sql.Open("sqlite", sqliteDSN(legacyPath, false))
	if err != nil {
		t.Fatalf("open writer db: %v", err)
	}
	if _, err := writerDB.Exec(`
		PRAGMA wal_autocheckpoint = 0;
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			status TEXT DEFAULT 'active',
			mode TEXT DEFAULT 'code',
			git_branch TEXT DEFAULT 'default',
			summary TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT,
			git_branch TEXT DEFAULT 'default',
			topic TEXT,
			decision TEXT,
			reason TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO sessions (id, status, mode, git_branch) VALUES ('wal-sess', 'active', 'code', 'default');
		PRAGMA user_version = 4;
		PRAGMA wal_checkpoint(TRUNCATE);
	`); err != nil {
		writerDB.Close()
		t.Fatalf("seed v4 schema: %v", err)
	}

	// Open a reader snapshot BEFORE writing the uncheckpointed row so SQLite
	// retains the new frame inside session.db-wal and blocks TRUNCATE.
	readerDB, err := sql.Open("sqlite", sqliteDSN(legacyPath, true))
	if err != nil {
		writerDB.Close()
		t.Fatalf("open reader db: %v", err)
	}
	readTx, err := readerDB.Begin()
	if err != nil {
		readerDB.Close()
		writerDB.Close()
		t.Fatalf("begin reader tx: %v", err)
	}
	var count int
	if err := readTx.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		_ = readTx.Rollback()
		readerDB.Close()
		writerDB.Close()
		t.Fatalf("pin wal read snapshot: %v", err)
	}

	// Commit a new row while the reader pins the older snapshot -> stays in -wal.
	if _, err := writerDB.Exec(`INSERT INTO decisions (session_id, git_branch, topic, decision, reason) VALUES ('wal-sess', 'default', 'wal-live', 'uncheckpointed-row', 'must-survive')`); err != nil {
		_ = readTx.Rollback()
		readerDB.Close()
		writerDB.Close()
		t.Fatalf("insert live wal row: %v", err)
	}

	walInfo, err := os.Stat(legacyPath + "-wal")
	if err != nil || walInfo.Size() == 0 {
		_ = readTx.Rollback()
		readerDB.Close()
		writerDB.Close()
		t.Fatalf("expected non-empty session.db-wal before migration (err=%v)", err)
	}

	// Snapshot the live uncheckpointed db + wal files to liveDir while readTx holds the WAL open.
	liveDir := t.TempDir()
	for _, suffix := range []string{"", "-wal"} {
		data, rerr := os.ReadFile(legacyPath + suffix)
		if rerr != nil {
			_ = readTx.Rollback()
			readerDB.Close()
			writerDB.Close()
			t.Fatalf("read %s: %v", legacyPath+suffix, rerr)
		}
		if werr := os.WriteFile(filepath.Join(liveDir, "session.db"+suffix), data, 0644); werr != nil {
			_ = readTx.Rollback()
			readerDB.Close()
			writerDB.Close()
			t.Fatalf("copy %s: %v", suffix, rerr)
		}
	}

	// Negative test: while readTx blocks TRUNCATE on seedDir, migration must fail
	// cleanly and leave root session.db + session.db-wal untouched.
	if _, err := NewEngine(seedDir); err == nil {
		_ = readTx.Rollback()
		readerDB.Close()
		writerDB.Close()
		t.Fatal("expected NewEngine to fail when legacy WAL cannot be truncated due to active lock")
	}
	if _, err := os.Stat(legacyPath + "-wal"); err != nil {
		t.Errorf("busy migration deleted session.db-wal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(seedDir, ".tether", "session.db")); !os.IsNotExist(err) {
		t.Errorf("busy migration created .tether/session.db unexpectedly")
	}

	_ = readTx.Rollback()
	readerDB.Close()
	writerDB.Close()

	// Positive test: migrate liveDir containing uncheckpointed -wal frames.
	e, err := NewEngine(liveDir)
	if err != nil {
		t.Fatalf("NewEngine live WAL migration failed: %v", err)
	}
	defer e.Close()

	decisions, err := e.GetDecisions()
	if err != nil {
		t.Fatalf("GetDecisions: %v", err)
	}
	foundLiveWAL := false
	for _, d := range decisions {
		if d.Topic == "wal-live" && d.Decision == "uncheckpointed-row" {
			foundLiveWAL = true
			break
		}
	}
	if !foundLiveWAL {
		t.Fatalf("uncheckpointed WAL row lost during migration; decisions=%+v", decisions)
	}
	if _, err := os.Stat(filepath.Join(liveDir, "session.db-wal")); !os.IsNotExist(err) {
		t.Errorf("expected root session.db-wal to be removed after successful migration")
	}
}

func TestReadDBRejectsWritesAndSupportsConcurrentReads(t *testing.T) {
	e := newTestEngine(t)

	if err := e.SetProjectSetting("read_test", "ok"); err != nil {
		t.Fatalf("SetProjectSetting: %v", err)
	}

	if _, err := e.ReadDB().Exec(`INSERT INTO project_settings (key, value) VALUES ('illegal', 'write')`); err == nil {
		t.Fatal("expected ReadDB().Exec(INSERT) to fail under query_only=ON / mode=ro, got nil")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err := e.GetProjectSetting("read_test")
			if err != nil {
				errs <- err
				return
			}
			if val != "ok" {
				errs <- sql.ErrNoRows
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ReadDB query failed: %v", err)
	}
}

func TestCacheDBSchemaAndKBStorage(t *testing.T) {
	e := newTestEngine(t)

	for _, tbl := range []string{"kb_documents", "kb_fts", "repomap_tags"} {
		var name string
		if err := e.CacheDB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, tbl).Scan(&name); err != nil {
			t.Errorf("expected table %q in .tether/cache.db: %v", tbl, err)
		}
	}
	var idxName string
	if err := e.CacheDB().QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name = 'idx_kb_documents_path'`).Scan(&idxName); err != nil {
		t.Errorf("expected idx_kb_documents_path in .tether/cache.db: %v", err)
	}

	// Ensure session.db does not host cache tables or pending_proposals.
	for _, absent := range []string{"kb_documents", "kb_fts", "pending_proposals"} {
		var cnt int
		if err := e.ReadDB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, absent).Scan(&cnt); err != nil {
			t.Fatalf("query session.db master: %v", err)
		}
		if cnt != 0 {
			t.Errorf("expected %q to be absent from .tether/session.db", absent)
		}
	}
}

func TestIndexDirectoryTransactional(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"main.go":           "package main\nfunc alphaSignal() {}\n",
		"sub/helper.go":     "package sub\nfunc betaSignal() {}\n",
		"README.md":         "# Documentation\n",
		"ignored.txt":       "ignored extension\n",
		".git/HEAD":         "ref: refs/heads/main\n",
		".tether/secret.go": "package secret\n",
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	e, err := NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer e.Close()

	count, err := e.IndexDirectory(dir, []string{"go", "md"})
	if err != nil {
		t.Fatalf("IndexDirectory: %v", err)
	}
	if count != 3 {
		t.Fatalf("IndexDirectory indexed %d files, want 3 (main.go, sub/helper.go, README.md)", count)
	}

	stats, err := e.KBStatus()
	if err != nil {
		t.Fatalf("KBStatus: %v", err)
	}
	if stats.TotalFiles != 3 || stats.TotalChunks != 3 {
		t.Fatalf("KBStatus = %+v, want {TotalFiles:3 TotalChunks:3}", stats)
	}

	// Re-indexing must overwrite without duplicating chunks.
	count2, err := e.IndexDirectory(dir, []string{"go", "md"})
	if err != nil || count2 != 3 {
		t.Fatalf("re-index = (%d, %v), want (3, nil)", count2, err)
	}
	stats2, err := e.KBStatus()
	if err != nil || stats2.TotalChunks != 3 {
		t.Fatalf("after re-index KBStatus = %+v (err=%v), want 3 chunks", stats2, err)
	}

	hits, err := e.SearchKB("betaSignal", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("SearchKB(betaSignal) = %+v, err=%v", hits, err)
	}
}
