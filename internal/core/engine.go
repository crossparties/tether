package core

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crossparties/tether/internal/mount"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Engine is the core Tether engine managing sessions, guardrails, and project memory.
type Engine struct {
	DB        *sql.DB // Write handle for .tether/session.db (kept for compatibility; equals writeDB)
	writeDB   *sql.DB
	readDB    *sql.DB
	cacheDB   *sql.DB
	Workspace string
	SessionID string
	Branch    string
	mu        sync.RWMutex
	mode      string // "code" | "plan" — backed by SQLite
}

// WriteDB returns the serialized write connection pool for .tether/session.db.
func (e *Engine) WriteDB() *sql.DB { return e.writeDB }

// ReadDB returns the concurrent read-only (query_only=ON) pool for .tether/session.db.
func (e *Engine) ReadDB() *sql.DB { return e.readDB }

// CacheDB returns the connection handle for .tether/cache.db.
func (e *Engine) CacheDB() *sql.DB { return e.cacheDB }

// OpenReadOnlyDB opens a SQLite database in strict read-only (mode=ro, query_only=ON) mode.
// When the WAL sidecar is absent or empty (e.g. closed DB scanned by `tether doctor`),
// immutable=1 prevents SQLite from creating a `-shm` file in a read-only probe.
func OpenReadOnlyDB(dbPath string) (*sql.DB, error) {
	dsn := sqliteDSN(dbPath, true)
	if walInfo, err := os.Stat(dbPath + "-wal"); err != nil || walInfo.Size() == 0 {
		dsn += "&immutable=1"
	}
	return sql.Open("sqlite", dsn)
}

// Mount returns a Mount Engine bound to this Engine's workspace and SQLite pools.
func (e *Engine) Mount() *mount.Engine {
	m := mount.NewEngine(e.Workspace)
	if e.readDB != nil || e.writeDB != nil {
		m.BindDB(e.readDB, e.writeDB)
	}
	return m
}

// EnsureGitignore ensures .tether runtime artifacts are ignored in .gitignore.
func (e *Engine) EnsureGitignore() error {
	return e.ensureGitignore()
}

// DetectGitBranch reads .git/HEAD directly with <0.1ms latency.
func DetectGitBranch(workspace string) string {
	headPath := filepath.Join(workspace, ".git", "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		return "default"
	}
	content := strings.TrimSpace(string(data))
	if strings.HasPrefix(content, "ref: refs/heads/") {
		return strings.TrimPrefix(content, "ref: refs/heads/")
	}
	if len(content) > 7 {
		return content[:7]
	}
	return "default"
}

// SchemaVersion tracks the database schema version for fast-path startup.
// Bump this whenever the migrate() schema gains a table: migrate() exits early
// when PRAGMA user_version >= SchemaVersion, so a new CREATE TABLE without a
// bump is silently never executed on existing databases.
const SchemaVersion = 5

func sqliteDSN(path string, readOnly bool) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{}
	q.Set("_busy_timeout", "5000")
	q.Add("_pragma", "cache_size=-10000")
	q.Add("_pragma", "temp_store=MEMORY")
	q.Add("_pragma", "mmap_size=67108864")
	if readOnly {
		q.Set("mode", "ro")
		q.Set("_query_only", "on")
	} else {
		q.Set("_journal_mode", "WAL")
		q.Set("_synchronous", "NORMAL")
		q.Set("_foreign_keys", "on")
		q.Add("_pragma", "wal_autocheckpoint=1000")
	}
	return (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(abs),
		RawQuery: q.Encode(),
	}).String()
}

// legacyCheckpointBusyTimeoutMs bounds how long legacy WAL checkpointing waits
// for concurrent readers/writers; overridden only in unit tests.
var legacyCheckpointBusyTimeoutMs = 5000

// migrateLegacySessionDB atomically moves <root>/session.db to <root>/.tether/session.db
// after checkpointing any pending WAL frames so no v0.2 state is lost.
func migrateLegacySessionDB(workspace, sessionDBPath string) (migrated bool, err error) {
	if _, statErr := os.Stat(sessionDBPath); statErr == nil {
		return false, nil
	} else if !os.IsNotExist(statErr) {
		return false, statErr
	}

	legacyDBPath := filepath.Join(workspace, "session.db")
	legacyInfo, err := os.Stat(legacyDBPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	_, walErr := os.Stat(legacyDBPath + "-wal")
	hasWAL := walErr == nil

	if legacyInfo.Size() > 0 || hasWAL {
		u, parseErr := url.Parse(sqliteDSN(legacyDBPath, false))
		if parseErr != nil {
			return false, parseErr
		}
		q := u.Query()
		q.Set("_busy_timeout", fmt.Sprintf("%d", legacyCheckpointBusyTimeoutMs))
		u.RawQuery = q.Encode()

		legacyDB, openErr := sql.Open("sqlite", u.String())
		if openErr != nil {
			return false, fmt.Errorf("open legacy session.db: %w", openErr)
		}
		var busy, logFrames, ckptFrames int
		ckptErr := legacyDB.QueryRow("PRAGMA wal_checkpoint(TRUNCATE);").Scan(&busy, &logFrames, &ckptFrames)
		closeErr := legacyDB.Close()
		if ckptErr != nil {
			return false, fmt.Errorf("checkpoint legacy session.db before migration: %w", ckptErr)
		}
		if busy != 0 {
			return false, fmt.Errorf("checkpoint legacy session.db: database is busy (busy=%d, log=%d, checkpointed=%d); close active tether processes and retry", busy, logFrames, ckptFrames)
		}
		if closeErr != nil {
			return false, fmt.Errorf("close legacy session.db: %w", closeErr)
		}
	}

	if err := os.Rename(legacyDBPath, sessionDBPath); err != nil {
		return false, fmt.Errorf("move legacy session.db to .tether/: %w", err)
	}
	_ = os.Remove(legacyDBPath + "-wal")
	_ = os.Remove(legacyDBPath + "-shm")
	return true, nil
}

// NewEngine creates a new engine for the given workspace with Dual-Pool WAL connections
// for .tether/session.db and a dedicated .tether/cache.db handle.
func NewEngine(workspace string) (*Engine, error) {
	tetherDir := filepath.Join(workspace, ".tether")
	if err := os.MkdirAll(tetherDir, 0755); err != nil {
		return nil, fmt.Errorf("create .tether directory: %w", err)
	}

	sessionDBPath := filepath.Join(tetherDir, "session.db")
	cacheDBPath := filepath.Join(tetherDir, "cache.db")

	migratedLegacy, err := migrateLegacySessionDB(workspace, sessionDBPath)
	if err != nil {
		return nil, err
	}

	dbExisted := false
	if fi, statErr := os.Stat(sessionDBPath); statErr == nil && fi.Size() > 0 {
		dbExisted = true
	}

	writeDB, err := sql.Open("sqlite", sqliteDSN(sessionDBPath, false))
	if err != nil {
		return nil, fmt.Errorf("open .tether/session.db write pool: %w", err)
	}
	// Serialize writes through a single connection to eliminate SQLITE_BUSY in-process.
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)

	e := &Engine{
		DB:        writeDB,
		writeDB:   writeDB,
		Workspace: workspace,
		Branch:    DetectGitBranch(workspace),
		mode:      "code",
	}

	// Run schema migration on writeDB before opening the read-only pool so the
	// database file and WAL sidecars always exist before readDB attaches.
	if err := e.migrate(); err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("migrate session.db: %w", err)
	}

	readDB, err := sql.Open("sqlite", sqliteDSN(sessionDBPath, true))
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("open .tether/session.db read pool: %w", err)
	}
	readDB.SetMaxOpenConns(8)
	readDB.SetMaxIdleConns(4)
	if err := readDB.Ping(); err != nil {
		readDB.Close()
		writeDB.Close()
		return nil, fmt.Errorf("ping .tether/session.db read pool: %w", err)
	}
	e.readDB = readDB

	cacheDB, err := sql.Open("sqlite", sqliteDSN(cacheDBPath, false))
	if err != nil {
		readDB.Close()
		writeDB.Close()
		return nil, fmt.Errorf("open .tether/cache.db: %w", err)
	}
	cacheDB.SetMaxOpenConns(1)
	cacheDB.SetMaxIdleConns(1)
	e.cacheDB = cacheDB

	if err := e.migrateCache(); err != nil {
		_ = e.Close()
		return nil, fmt.Errorf("migrate cache.db: %w", err)
	}

	if !dbExisted || migratedLegacy {
		if err := e.ensureGitignore(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not update .gitignore: %v\n", err)
		}
	}

	if err := e.loadOrCreateSession(); err != nil {
		_ = e.Close()
		return nil, fmt.Errorf("session: %w", err)
	}

	return e, nil
}

func (e *Engine) Close() error {
	var errs []error
	if e.readDB != nil {
		if err := e.readDB.Close(); err != nil {
			errs = append(errs, err)
		}
		e.readDB = nil
	}
	if e.writeDB != nil {
		_, _ = e.writeDB.Exec("PRAGMA optimize; PRAGMA wal_checkpoint(PASSIVE);")
		if err := e.writeDB.Close(); err != nil {
			errs = append(errs, err)
		}
		e.writeDB = nil
		e.DB = nil
	}
	if e.cacheDB != nil {
		_, _ = e.cacheDB.Exec("PRAGMA optimize; PRAGMA wal_checkpoint(PASSIVE);")
		if err := e.cacheDB.Close(); err != nil {
			errs = append(errs, err)
		}
		e.cacheDB = nil
	}
	return errors.Join(errs...)
}

// requiredTables lists every table migrate() is responsible for in session.db.
// The fast path only trusts PRAGMA user_version when these all actually exist,
// so a database whose version was bumped without its DDL running gets repaired
// instead of silently staying broken.
var requiredTables = []string{
	"sessions",
	"session_context",
	"decisions",
	"learned_patterns",
	"project_settings",
	"todos",
	"approval_grants",
	"conversation_threads",
	"conversation_events",
	"crew_sessions",
	"mounted_files",
	"mount_ignore_rules",
	"api_keys_meta",
}

// missingTables returns the required tables absent from session.db.
func (e *Engine) missingTables() ([]string, error) {
	var missing []string
	for _, table := range requiredTables {
		var name string
		err := e.writeDB.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name = ?", table).Scan(&name)
		if err == sql.ErrNoRows {
			missing = append(missing, table)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return missing, nil
}

// migrate creates all required session.db tables and indexes (fast-paths if schema is up-to-date).
func (e *Engine) migrate() error {
	var currentVersion int
	_ = e.writeDB.QueryRow("PRAGMA user_version;").Scan(&currentVersion)
	if currentVersion >= SchemaVersion {
		missing, err := e.missingTables()
		if err == nil && len(missing) == 0 {
			return nil
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		status TEXT DEFAULT 'active',
		mode TEXT DEFAULT 'code',
		git_branch TEXT DEFAULT 'default',
		summary TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS session_context (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT REFERENCES sessions(id),
		agent TEXT DEFAULT '',
		role TEXT,
		content TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS project_settings (
		key TEXT PRIMARY KEY,
		value TEXT,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS decisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT REFERENCES sessions(id),
		git_branch TEXT DEFAULT 'default',
		topic TEXT,
		decision TEXT,
		reason TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS learned_patterns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pattern_type TEXT,
		description TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS todos (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT REFERENCES sessions(id),
		git_branch TEXT DEFAULT 'default',
		description TEXT,
		details TEXT DEFAULT '',
		status TEXT DEFAULT 'pending',
		version INTEGER DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS approval_grants (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		path_scope TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'path',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		expires_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS conversation_threads (
		id TEXT PRIMARY KEY,
		workspace TEXT NOT NULL DEFAULT '',
		git_branch TEXT NOT NULL DEFAULT 'default',
		title TEXT NOT NULL DEFAULT '',
		mode TEXT NOT NULL DEFAULT 'code',
		pinned_key_id TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS conversation_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		thread_id TEXT NOT NULL REFERENCES conversation_threads(id),
		crew_id TEXT NOT NULL DEFAULT '',
		seq INTEGER NOT NULL,
		role TEXT NOT NULL,
		usage_id TEXT NOT NULL DEFAULT 'main',
		payload_json TEXT NOT NULL DEFAULT '{}',
		token_est INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS crew_sessions (
		id TEXT PRIMARY KEY,
		parent_thread_id TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL DEFAULT '',
		skill_name TEXT NOT NULL DEFAULT '',
		purpose_category TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
		lease_files_json TEXT NOT NULL DEFAULT '[]',
		lease_expires_at DATETIME,
		summary TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		closed_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS mounted_files (
		rel_path TEXT PRIMARY KEY,
		access_mode TEXT NOT NULL DEFAULT 'guarded',
		source TEXT NOT NULL DEFAULT 'rule',
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS mount_ignore_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pattern TEXT NOT NULL,
		is_regex INTEGER NOT NULL DEFAULT 0,
		is_builtin INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS api_keys_meta (
		key_id TEXT PRIMARY KEY,
		provider TEXT NOT NULL DEFAULT '',
		label TEXT NOT NULL DEFAULT '',
		avg_ttft_ms INTEGER NOT NULL DEFAULT 0,
		tool_error_rate REAL NOT NULL DEFAULT 0.0,
		cache_hit_ratio REAL NOT NULL DEFAULT 0.0,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_conversation_events_thread_seq ON conversation_events(thread_id, seq);
	CREATE INDEX IF NOT EXISTS idx_session_context_session_id ON session_context(session_id);
	CREATE INDEX IF NOT EXISTS idx_todos_session_status ON todos(session_id, status);
	CREATE INDEX IF NOT EXISTS idx_decisions_git_branch ON decisions(git_branch);
	CREATE INDEX IF NOT EXISTS idx_approval_grants_session_expires ON approval_grants(session_id, expires_at);
	`
	if _, err := e.writeDB.Exec(schema); err != nil {
		return err
	}

	// Non-breaking migrations for existing v1-v4 databases
	_, _ = e.writeDB.Exec("ALTER TABLE sessions ADD COLUMN mode TEXT DEFAULT 'code';")
	_, _ = e.writeDB.Exec("ALTER TABLE sessions ADD COLUMN git_branch TEXT DEFAULT 'default';")
	_, _ = e.writeDB.Exec("ALTER TABLE decisions ADD COLUMN git_branch TEXT DEFAULT 'default';")
	_, _ = e.writeDB.Exec("ALTER TABLE todos ADD COLUMN git_branch TEXT DEFAULT 'default';")
	_, _ = e.writeDB.Exec("ALTER TABLE todos ADD COLUMN version INTEGER DEFAULT 1;")

	// Drop legacy v4 tables/triggers removed in v5 (FTS cache moved to .tether/cache.db)
	cleanup := `
	DROP TABLE IF EXISTS pending_proposals;
	DROP TRIGGER IF EXISTS kb_ai;
	DROP TRIGGER IF EXISTS kb_ad;
	DROP TRIGGER IF EXISTS kb_au;
	DROP TABLE IF EXISTS kb_fts;
	DROP TABLE IF EXISTS kb_documents;
	`
	if _, err := e.writeDB.Exec(cleanup); err != nil {
		return err
	}

	_, _ = e.writeDB.Exec(fmt.Sprintf("PRAGMA user_version = %d;", SchemaVersion))
	return nil
}

// migrateCache initializes .tether/cache.db tables, indexes, and FTS5 triggers.
func (e *Engine) migrateCache() error {
	schema := `
	CREATE TABLE IF NOT EXISTS kb_documents (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		file_path TEXT,
		chunk_index INTEGER DEFAULT 0,
		content TEXT,
		metadata TEXT DEFAULT '',
		indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(file_path, chunk_index)
	);

	CREATE INDEX IF NOT EXISTS idx_kb_documents_path ON kb_documents(file_path);

	CREATE VIRTUAL TABLE IF NOT EXISTS kb_fts USING fts5(
		content,
		file_path,
		content='kb_documents',
		content_rowid='id',
		tokenize='porter unicode61'
	);

	CREATE TRIGGER IF NOT EXISTS kb_ai AFTER INSERT ON kb_documents BEGIN
		INSERT INTO kb_fts(rowid, content, file_path) VALUES (new.id, new.content, new.file_path);
	END;

	CREATE TRIGGER IF NOT EXISTS kb_ad AFTER DELETE ON kb_documents BEGIN
		INSERT INTO kb_fts(kb_fts, rowid, content, file_path) VALUES ('delete', old.id, old.content, old.file_path);
	END;

	CREATE TRIGGER IF NOT EXISTS kb_au AFTER UPDATE ON kb_documents BEGIN
		INSERT INTO kb_fts(kb_fts, rowid, content, file_path) VALUES ('delete', old.id, old.content, old.file_path);
		INSERT INTO kb_fts(rowid, content, file_path) VALUES (new.id, new.content, new.file_path);
	END;

	CREATE TABLE IF NOT EXISTS repomap_tags (
		file_path TEXT PRIMARY KEY,
		mtime_ns INTEGER NOT NULL DEFAULT 0,
		lang TEXT NOT NULL DEFAULT '',
		tags_json TEXT NOT NULL DEFAULT '[]'
	);
	`
	_, err := e.cacheDB.Exec(schema)
	return err
}

// ensureGitignore adds .tether runtime artifacts to .gitignore.
func (e *Engine) ensureGitignore() error {
	gitignorePath := filepath.Join(e.Workspace, ".gitignore")

	content, err := os.ReadFile(gitignorePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	existing := make(map[string]bool)
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == ".tether/" || trimmed == ".tether" {
			return nil
		}
		if trimmed != "" {
			existing[trimmed] = true
		}
	}

	required := []string{
		".tether/session.db*",
		".tether/cache.db*",
		".tether/snapshots/",
		".tether/PLAN.md",
	}
	var missing []string
	for _, entry := range required {
		if !existing[entry] {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(strings.Join(missing, "\n") + "\n")
	return err
}

// loadOrCreateSession loads the last active session for this branch or creates a new one.
func (e *Engine) loadOrCreateSession() error {
	var id, mode string
	err := e.writeDB.QueryRow(`
		SELECT id, COALESCE(mode, 'code') FROM sessions
		WHERE status = 'active' AND git_branch = ?
		ORDER BY updated_at DESC LIMIT 1
	`, e.Branch).Scan(&id, &mode)

	if err == sql.ErrNoRows {
		err = e.writeDB.QueryRow(`
			SELECT id, COALESCE(mode, 'code') FROM sessions
			WHERE status = 'active'
			ORDER BY updated_at DESC LIMIT 1
		`).Scan(&id, &mode)
	}

	if err == sql.ErrNoRows {
		_, _ = e.writeDB.Exec(`
			UPDATE sessions SET status = 'archived'
			WHERE status = 'active'
			AND updated_at < datetime('now', '-7 days')
		`)

		id = uuid.New().String()
		mode = "code"
		_, err = e.writeDB.Exec(`INSERT INTO sessions (id, status, mode, git_branch) VALUES (?, 'active', 'code', ?)`, id, e.Branch)
		if err != nil {
			return err
		}
		e.SessionID = id
		e.mode = mode
		e.adoptPreviousTODOs(id)

		_, _ = e.writeDB.Exec(`UPDATE sessions SET updated_at = CURRENT_TIMESTAMP, git_branch = ? WHERE id = ?`, e.Branch, id)
		return nil
	} else if err != nil {
		return err
	}

	e.SessionID = id
	e.mode = mode

	_, _ = e.writeDB.Exec(`UPDATE sessions SET updated_at = CURRENT_TIMESTAMP, git_branch = ? WHERE id = ?`, e.Branch, id)
	return nil
}

// adoptPreviousTODOs carries open work forward into a freshly created session.
func (e *Engine) adoptPreviousTODOs(newSessionID string) {
	rows, err := e.readDB.Query(`
		SELECT id, description, details FROM todos
		WHERE status = 'pending'
		  AND session_id <> ?
		  AND session_id IN (SELECT id FROM sessions WHERE git_branch = ?)
		ORDER BY id
	`, newSessionID, e.Branch)
	if err != nil {
		return
	}
	defer rows.Close()

	type pendingTodo struct {
		id          int
		description string
		details     string
	}
	var carry []pendingTodo
	for rows.Next() {
		var t pendingTodo
		if err := rows.Scan(&t.id, &t.description, &t.details); err != nil {
			return
		}
		carry = append(carry, t)
	}
	if err := rows.Err(); err != nil || len(carry) == 0 {
		return
	}

	for _, t := range carry {
		details := t.details
		if details != "" {
			details += "\n\n"
		}
		details += fmt.Sprintf("(carried over from todo #%d)", t.id)
		if _, err := e.writeDB.Exec(`
			INSERT INTO todos (session_id, git_branch, description, details, status)
			VALUES (?, ?, ?, ?, 'pending')
		`, newSessionID, e.Branch, t.description, details); err != nil {
			continue
		}
		_, _ = e.writeDB.Exec(`UPDATE todos SET status = 'carried' WHERE id = ?`, t.id)
	}
}

// --- Session Operations ---

func (e *Engine) CompleteSession() error {
	_, err := e.writeDB.Exec(`UPDATE sessions SET status = 'completed', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, e.SessionID)
	return err
}

func (e *Engine) GetSessionSummary() (string, error) {
	var summary string
	err := e.readDB.QueryRow(`SELECT COALESCE(summary, '') FROM sessions WHERE id = ?`, e.SessionID).Scan(&summary)
	return summary, err
}

func (e *Engine) SetSessionSummary(summary string) error {
	_, err := e.writeDB.Exec(`UPDATE sessions SET summary = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, summary, e.SessionID)
	return err
}

// --- Context Operations ---

func (e *Engine) AddContext(agent, role, content string) error {
	_, err := e.writeDB.Exec(`INSERT INTO session_context (session_id, agent, role, content) VALUES (?, ?, ?, ?)`,
		e.SessionID, agent, role, content)
	return err
}

func (e *Engine) GetRecentContext(limit int) ([]ContextEntry, error) {
	rows, err := e.readDB.Query(`
		SELECT agent, role, content, created_at FROM session_context
		WHERE session_id = ?
		ORDER BY created_at DESC LIMIT ?
	`, e.SessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []ContextEntry
	for rows.Next() {
		var entry ContextEntry
		if err := rows.Scan(&entry.Agent, &entry.Role, &entry.Content, &entry.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// --- Project Settings ---

func (e *Engine) SetProjectSetting(key, value string) error {
	_, err := e.writeDB.Exec(`
		INSERT INTO project_settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP
	`, key, value)
	return err
}

func (e *Engine) GetProjectSetting(key string) (string, error) {
	var value string
	err := e.readDB.QueryRow(`SELECT value FROM project_settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (e *Engine) GetAllProjectSettings() (map[string]string, error) {
	rows, err := e.readDB.Query(`SELECT key, value FROM project_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		settings[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return settings, nil
}

// --- Decisions ---

func (e *Engine) AddDecision(topic, decision, reason string) error {
	_, err := e.writeDB.Exec(`INSERT INTO decisions (session_id, git_branch, topic, decision, reason) VALUES (?, ?, ?, ?, ?)`,
		e.SessionID, e.Branch, topic, decision, reason)
	return err
}

func (e *Engine) GetDecisions() ([]Decision, error) {
	rows, err := e.readDB.Query(`SELECT topic, decision, reason, created_at FROM decisions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var decisions []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.Topic, &d.Decision, &d.Reason, &d.CreatedAt); err != nil {
			return nil, err
		}
		decisions = append(decisions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return decisions, nil
}

// InjectDecisions returns at most limit of the most recent decisions, newest
// first, for prompt injection.
func (e *Engine) InjectDecisions(limit int) ([]Decision, error) {
	rows, err := e.readDB.Query(`
		SELECT topic, decision, reason, created_at FROM decisions
		ORDER BY created_at DESC, rowid DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var decisions []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.Topic, &d.Decision, &d.Reason, &d.CreatedAt); err != nil {
			return nil, err
		}
		decisions = append(decisions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return decisions, nil
}

// --- Learned Patterns ---

func (e *Engine) AddPattern(patternType, description string) error {
	_, err := e.writeDB.Exec(`INSERT INTO learned_patterns (pattern_type, description) VALUES (?, ?)`,
		patternType, description)
	return err
}

func (e *Engine) GetPatterns() ([]Pattern, error) {
	rows, err := e.readDB.Query(`SELECT pattern_type, description, created_at FROM learned_patterns ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var patterns []Pattern
	for rows.Next() {
		var p Pattern
		if err := rows.Scan(&p.Type, &p.Description, &p.CreatedAt); err != nil {
			return nil, err
		}
		patterns = append(patterns, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return patterns, nil
}

// InjectPatterns mirrors InjectDecisions: the prompt only carries the newest
// few patterns, while the archive stays available in full through GetPatterns.
func (e *Engine) InjectPatterns(limit int) ([]Pattern, error) {
	rows, err := e.readDB.Query(`
		SELECT pattern_type, description, created_at FROM learned_patterns
		ORDER BY created_at DESC, rowid DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var patterns []Pattern
	for rows.Next() {
		var p Pattern
		if err := rows.Scan(&p.Type, &p.Description, &p.CreatedAt); err != nil {
			return nil, err
		}
		patterns = append(patterns, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return patterns, nil
}

// --- Mode Management ---

func (e *Engine) SetMode(mode string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = mode
	if e.SessionID != "" {
		_, err := e.writeDB.Exec(`UPDATE sessions SET mode = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, mode, e.SessionID)
		return err
	}
	return nil
}

func (e *Engine) GetMode() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.SessionID != "" {
		var mode string
		err := e.readDB.QueryRow(`SELECT COALESCE(mode, 'code') FROM sessions WHERE id = ?`, e.SessionID).Scan(&mode)
		if err == nil && mode != "" {
			return mode
		}
	}
	if e.mode != "" {
		return e.mode
	}
	return "code"
}

func (e *Engine) IsWriteAllowed() bool {
	return e.GetMode() != "plan"
}

// ClassifyTask returns the recommended execution mode based on prompt prefix.
// Returns one of: "tether_plan", "tether_code", "tether_review", "tether_direct".
func ClassifyTask(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "/plan") || strings.HasPrefix(lower, "/spec") {
		return "tether_plan"
	}
	if strings.HasPrefix(lower, "/code") || strings.HasPrefix(lower, "/apply") {
		return "tether_code"
	}
	if strings.HasPrefix(lower, "/audit") || strings.HasPrefix(lower, "/review") {
		return "tether_review"
	}
	return "tether_direct"
}

// --- Types ---

type ContextEntry struct {
	Agent     string
	Role      string
	Content   string
	CreatedAt time.Time
}

type Decision struct {
	Topic     string
	Decision  string
	Reason    string
	CreatedAt time.Time
}

type Pattern struct {
	Type        string
	Description string
	CreatedAt   time.Time
}

// --- Knowledge Base (.tether/cache.db) ---

func indexFileChunks(delStmt, insStmt *sql.Stmt, filePath, content string) error {
	if _, err := delStmt.Exec(filePath); err != nil {
		return err
	}

	lines := strings.Split(content, "\n")
	const chunkSize = 100
	if len(lines) <= chunkSize {
		_, err := insStmt.Exec(filePath, 0, content)
		return err
	}

	for i := 0; i < len(lines); i += chunkSize {
		end := i + chunkSize
		if end > len(lines) {
			end = len(lines)
		}
		chunk := strings.Join(lines[i:end], "\n")
		if _, err := insStmt.Exec(filePath, i/chunkSize, chunk); err != nil {
			return err
		}
	}
	return nil
}

// IndexFile indexes an authorized workspace file into .tether/cache.db inside
// a single transaction with prepared statements, blocking secrets, ignored paths,
// and symlink escapes.
func (e *Engine) IndexFile(filePath string) error {
	dec := e.Mount().AuthorizePath(filePath, mount.OpRead, "code")
	if !dec.Allowed {
		return fmt.Errorf("%s", dec.Reason)
	}
	if dec.IsDir {
		return fmt.Errorf("cannot index directory %q as file", dec.RelPath)
	}

	content, err := os.ReadFile(dec.CanonicalPath)
	if err != nil {
		return err
	}

	tx, err := e.cacheDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	delStmt, err := tx.Prepare(`DELETE FROM kb_documents WHERE file_path = ?`)
	if err != nil {
		return err
	}
	defer delStmt.Close()

	insStmt, err := tx.Prepare(`INSERT INTO kb_documents (file_path, chunk_index, content) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insStmt.Close()

	if err := indexFileChunks(delStmt, insStmt, filePath, string(content)); err != nil {
		return err
	}
	return tx.Commit()
}

// IndexDirectory indexes authorized matching files in dirPath inside a single
// cache.db transaction. When extensions is empty, it focuses on repository
// Markdown/documentation files (md, mdx, rst, txt), always prioritizing *.md first.
func (e *Engine) IndexDirectory(dirPath string, extensions []string) (int, error) {
	mountEngine := e.Mount()
	relDir, canonDir, _, rerr := mount.ResolveWithinWorkspace(dirPath, e.Workspace)
	if rerr != nil {
		return 0, rerr
	}
	if relDir != "." {
		if d := mountEngine.AuthorizePath(dirPath, mount.OpRead, "code"); !d.Allowed {
			return 0, fmt.Errorf("%s", d.Reason)
		}
		canonDir = dCanonOr(canonDir, dirPath)
	}

	if len(extensions) == 0 {
		extensions = []string{"md", "mdx", "rst", "txt"}
	}
	extSet := make(map[string]bool, len(extensions))
	for _, ext := range extensions {
		clean := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
		if clean != "" {
			extSet[clean] = true
		}
	}

	var files []string
	walkErr := filepath.WalkDir(canonDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path == canonDir {
				return nil
			}
			base := d.Name()
			if strings.HasPrefix(base, ".") || mount.IsDefaultSkipDirName(base) {
				return filepath.SkipDir
			}
			if relSub, _, _, err := mount.ResolveWithinWorkspace(path, e.Workspace); err == nil && relSub != "." {
				if mountEngine.ShouldSkipDir(relSub) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(extSet) > 0 {
			ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
			if !extSet[ext] {
				return nil
			}
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > 1024*1024 {
			return nil
		}
		if dec := mountEngine.AuthorizePath(path, mount.OpRead, "code"); !dec.Allowed || dec.IsDir {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if walkErr != nil {
		return 0, walkErr
	}

	// Prioritize Markdown documentation (*.md / *.mdx) before other text/code files.
	sort.Slice(files, func(i, j int) bool {
		mi := isMarkdownPath(files[i])
		mj := isMarkdownPath(files[j])
		if mi != mj {
			return mi
		}
		return files[i] < files[j]
	})

	tx, err := e.cacheDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	delStmt, err := tx.Prepare(`DELETE FROM kb_documents WHERE file_path = ?`)
	if err != nil {
		return 0, err
	}
	defer delStmt.Close()

	insStmt, err := tx.Prepare(`INSERT INTO kb_documents (file_path, chunk_index, content) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insStmt.Close()

	count := 0
	for _, path := range files {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		if err := indexFileChunks(delStmt, insStmt, path, string(content)); err != nil {
			return 0, err
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func dCanonOr(canon, fallback string) string {
	if canon != "" {
		return canon
	}
	return fallback
}

func isMarkdownPath(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".md" || ext == ".mdx" || ext == ".markdown"
}

// ReconcileKB purges stale rows from kb_documents (and kb_fts via delete trigger)
// that match isIgnored or violate current mount ignore/secret/containment rules.
func (e *Engine) ReconcileKB(isIgnored func(relPath string) bool) (int, error) {
	if e.cacheDB == nil {
		return 0, nil
	}
	if isIgnored == nil {
		m := e.Mount()
		isIgnored = func(relPath string) bool {
			return !m.AuthorizePath(relPath, mount.OpRead, "code").Allowed
		}
	}

	rows, err := e.cacheDB.Query(`SELECT DISTINCT file_path FROM kb_documents`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	absRoot, _ := filepath.Abs(e.Workspace)
	realRoot, rerr := mount.EvalSymlinksWithAncestor(absRoot)
	if rerr != nil {
		realRoot = absRoot
	}

	var toDelete []string
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return 0, err
		}
		rel := stored
		outside := false
		if filepath.IsAbs(stored) {
			if r, err := filepath.Rel(absRoot, stored); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				rel = filepath.ToSlash(r)
			} else if r, err := filepath.Rel(realRoot, stored); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				rel = filepath.ToSlash(r)
			} else {
				outside = true
			}
		} else {
			rel = filepath.ToSlash(filepath.Clean(stored))
			if rel == ".." || strings.HasPrefix(rel, "../") {
				outside = true
			}
		}
		if outside || isIgnored(rel) {
			toDelete = append(toDelete, stored)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(toDelete) == 0 {
		return 0, nil
	}

	tx, err := e.cacheDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	delStmt, err := tx.Prepare(`DELETE FROM kb_documents WHERE file_path = ?`)
	if err != nil {
		return 0, err
	}
	defer delStmt.Close()

	purged := 0
	for _, p := range toDelete {
		if _, err := delStmt.Exec(p); err != nil {
			return purged, err
		}
		purged++
	}
	if err := tx.Commit(); err != nil {
		return purged, err
	}
	return purged, nil
}

// SearchKB performs full-text search across authorized indexed documents in .tether/cache.db.
func (e *Engine) SearchKB(query string, limit int) ([]KBResult, error) {
	if limit <= 0 {
		limit = 10
	}
	mountEngine := e.Mount()
	_, _ = e.ReconcileKB(func(relPath string) bool {
		return !mountEngine.AuthorizePath(relPath, mount.OpRead, "code").Allowed
	})

	rows, err := e.cacheDB.Query(`
		SELECT d.file_path, d.chunk_index, snippet(kb_fts, 0, '>>>', '<<<', '...', 64) as snippet,
			   rank
		FROM kb_fts f
		JOIN kb_documents d ON d.id = f.rowid
		WHERE kb_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []KBResult
	for rows.Next() {
		var r KBResult
		if err := rows.Scan(&r.FilePath, &r.ChunkIndex, &r.Snippet, &r.Score); err != nil {
			return nil, err
		}
		if !mountEngine.AuthorizePath(r.FilePath, mount.OpRead, "code").Allowed {
			continue
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// KBStatus returns stats about indexed documents in .tether/cache.db.
func (e *Engine) KBStatus() (KBStats, error) {
	var stats KBStats
	if err := e.cacheDB.QueryRow(`SELECT COUNT(*) FROM kb_documents`).Scan(&stats.TotalChunks); err != nil {
		return stats, err
	}
	if err := e.cacheDB.QueryRow(`SELECT COUNT(DISTINCT file_path) FROM kb_documents`).Scan(&stats.TotalFiles); err != nil {
		return stats, err
	}
	return stats, nil
}

type KBResult struct {
	FilePath   string
	ChunkIndex int
	Snippet    string
	Score      float64
}

type KBStats struct {
	TotalFiles  int
	TotalChunks int
}

// --- Todo Tracking ---

func (e *Engine) AddTodo(description, details string) (int64, error) {
	result, err := e.writeDB.Exec(`INSERT INTO todos (session_id, git_branch, description, details) VALUES (?, ?, ?, ?)`,
		e.SessionID, e.Branch, description, details)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (e *Engine) ListTodos(includeCompleted bool) ([]Todo, error) {
	query := `SELECT id, description, details, status, created_at, completed_at FROM todos WHERE session_id = ?`
	if !includeCompleted {
		query += ` AND status = 'pending'`
	}
	query += ` ORDER BY created_at ASC`

	rows, err := e.readDB.Query(query, e.SessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var todos []Todo
	for rows.Next() {
		var t Todo
		var completedAt sql.NullString
		if err := rows.Scan(&t.ID, &t.Description, &t.Details, &t.Status, &t.CreatedAt, &completedAt); err != nil {
			return nil, err
		}
		if completedAt.Valid {
			t.CompletedAt = completedAt.String
		}
		todos = append(todos, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return todos, nil
}

func (e *Engine) CompleteTodo(id int) error {
	res, err := e.writeDB.Exec(`UPDATE todos SET status = 'completed', completed_at = CURRENT_TIMESTAMP WHERE id = ? AND session_id = ?`,
		id, e.SessionID)
	if err != nil {
		return err
	}
	if n, rerr := res.RowsAffected(); rerr == nil && n == 0 {
		return fmt.Errorf("todo #%d not found in session %s", id, e.SessionID)
	}
	return nil
}

func (e *Engine) RemoveTodo(id int) error {
	res, err := e.writeDB.Exec(`DELETE FROM todos WHERE id = ? AND session_id = ?`, id, e.SessionID)
	if err != nil {
		return err
	}
	if n, rerr := res.RowsAffected(); rerr == nil && n == 0 {
		return fmt.Errorf("todo #%d not found in session %s", id, e.SessionID)
	}
	return nil
}

type Todo struct {
	ID          int
	Description string
	Details     string
	Status      string
	CreatedAt   time.Time
	CompletedAt string
}

// ExecuteSQL executes a SQL query directly against .tether/session.db and returns JSON-friendly results.
// SELECT, PRAGMA, EXPLAIN route to readDB and return []map[string]interface{}.
// DML/DDL route to writeDB and return map[string]interface{} with rows_affected and last_insert_id.
func (e *Engine) ExecuteSQL(query string) (interface{}, error) {
	trimmed := strings.TrimSpace(query)
	upper := strings.ToUpper(trimmed)

	// Safety guard: prevent accidental deletion of core substrate tables
	if strings.Contains(upper, "DROP TABLE") {
		for _, tbl := range []string{"SESSIONS", "PROJECT_SETTINGS", "DECISIONS", "LEARNED_PATTERNS"} {
			if strings.Contains(upper, tbl) {
				return nil, fmt.Errorf("safety guard: dropping core substrate table '%s' is prohibited", tbl)
			}
		}
	}

	if strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "PRAGMA") || strings.HasPrefix(upper, "EXPLAIN") {
		rows, err := e.readDB.Query(query)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}

		var results []map[string]interface{}
		for rows.Next() {
			vals := make([]interface{}, len(cols))
			ptrs := make([]interface{}, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			rowMap := make(map[string]interface{}, len(cols))
			for i, col := range cols {
				val := vals[i]
				if b, ok := val.([]byte); ok {
					rowMap[col] = string(b)
				} else {
					rowMap[col] = val
				}
			}
			results = append(results, rowMap)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if results == nil {
			results = []map[string]interface{}{}
		}
		return results, nil
	}

	res, err := e.writeDB.Exec(query)
	if err != nil {
		return nil, err
	}
	rowsAffected, _ := res.RowsAffected()
	lastID, _ := res.LastInsertId()
	return map[string]interface{}{
		"rows_affected":  rowsAffected,
		"last_insert_id": lastID,
	}, nil
}
