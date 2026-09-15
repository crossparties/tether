package core

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Engine is the core Tether engine managing sessions, guardrails, and project memory
type Engine struct {
	DB        *sql.DB
	Workspace string
	SessionID string
	Branch    string
	mu        sync.RWMutex
	mode      string // "code" | "plan" — backed by SQLite
}

// DetectGitBranch reads .git/HEAD directly with <0.1ms latency
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

// NewEngine creates a new engine for the given workspace
func NewEngine(workspace string) (*Engine, error) {
	dbPath := filepath.Join(workspace, "session.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open session.db: %w", err)
	}

	// Hardening: WAL mode & busy timeout for concurrent multi-agent safety
	_, _ = db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;")

	branch := DetectGitBranch(workspace)
	e := &Engine{
		DB:        db,
		Workspace: workspace,
		Branch:    branch,
		mode:      "code",
	}

	if err := e.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if err := e.ensureGitignore(); err != nil {
		// non-fatal, just log
		fmt.Fprintf(os.Stderr, "warning: could not update .gitignore: %v\n", err)
	}

	// Load or create active session
	if err := e.loadOrCreateSession(); err != nil {
		db.Close()
		return nil, fmt.Errorf("session: %w", err)
	}

	return e, nil
}

func (e *Engine) Close() error {
	if e.DB != nil {
		return e.DB.Close()
	}
	return nil
}

// migrate creates all required tables
func (e *Engine) migrate() error {
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

	CREATE TABLE IF NOT EXISTS kb_documents (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		file_path TEXT,
		chunk_index INTEGER DEFAULT 0,
		content TEXT,
		metadata TEXT DEFAULT '',
		indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(file_path, chunk_index)
	);

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
	`
	if _, err := e.DB.Exec(schema); err != nil {
		return err
	}

	// Non-breaking migrations for existing databases
	_, _ = e.DB.Exec("ALTER TABLE sessions ADD COLUMN mode TEXT DEFAULT 'code';")
	_, _ = e.DB.Exec("ALTER TABLE sessions ADD COLUMN git_branch TEXT DEFAULT 'default';")
	_, _ = e.DB.Exec("ALTER TABLE decisions ADD COLUMN git_branch TEXT DEFAULT 'default';")
	_, _ = e.DB.Exec("ALTER TABLE todos ADD COLUMN git_branch TEXT DEFAULT 'default';")
	_, _ = e.DB.Exec("ALTER TABLE todos ADD COLUMN version INTEGER DEFAULT 1;")

	return nil
}

// ensureGitignore adds session.db to .gitignore
func (e *Engine) ensureGitignore() error {
	gitignorePath := filepath.Join(e.Workspace, ".gitignore")

	content, err := os.ReadFile(gitignorePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "session.db" {
			return nil // already present
		}
	}

	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		f.WriteString("\n")
	}
	_, err = f.WriteString("session.db\n")
	return err
}

// loadOrCreateSession loads the last active session for this branch or creates a new one
func (e *Engine) loadOrCreateSession() error {
	// Archive old sessions (>7 days without activity)
	_, _ = e.DB.Exec(`
		UPDATE sessions SET status = 'archived'
		WHERE status = 'active'
		AND updated_at < datetime('now', '-7 days')
	`)

	// Try to load active session for current branch
	var id, mode string
	err := e.DB.QueryRow(`
		SELECT id, COALESCE(mode, 'code') FROM sessions
		WHERE status = 'active' AND git_branch = ?
		ORDER BY updated_at DESC LIMIT 1
	`, e.Branch).Scan(&id, &mode)

	if err == sql.ErrNoRows {
		// Fallback: check if there's any active session without branch or default
		err = e.DB.QueryRow(`
			SELECT id, COALESCE(mode, 'code') FROM sessions
			WHERE status = 'active'
			ORDER BY updated_at DESC LIMIT 1
		`).Scan(&id, &mode)
	}

	if err == sql.ErrNoRows {
		// Create new session
		id = uuid.New().String()
		mode = "code"
		_, err = e.DB.Exec(`INSERT INTO sessions (id, status, mode, git_branch) VALUES (?, 'active', 'code', ?)`, id, e.Branch)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	e.SessionID = id
	e.mode = mode

	// Touch updated_at and update branch
	_, _ = e.DB.Exec(`UPDATE sessions SET updated_at = CURRENT_TIMESTAMP, git_branch = ? WHERE id = ?`, e.Branch, id)

	return nil
}

// --- Session Operations ---

func (e *Engine) CompleteSession() error {
	_, err := e.DB.Exec(`UPDATE sessions SET status = 'completed', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, e.SessionID)
	return err
}

func (e *Engine) GetSessionSummary() (string, error) {
	var summary string
	err := e.DB.QueryRow(`SELECT COALESCE(summary, '') FROM sessions WHERE id = ?`, e.SessionID).Scan(&summary)
	return summary, err
}

func (e *Engine) SetSessionSummary(summary string) error {
	_, err := e.DB.Exec(`UPDATE sessions SET summary = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, summary, e.SessionID)
	return err
}

// --- Context Operations ---

func (e *Engine) AddContext(agent, role, content string) error {
	_, err := e.DB.Exec(`INSERT INTO session_context (session_id, agent, role, content) VALUES (?, ?, ?, ?)`,
		e.SessionID, agent, role, content)
	return err
}

func (e *Engine) GetRecentContext(limit int) ([]ContextEntry, error) {
	rows, err := e.DB.Query(`
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
	return entries, nil
}

// --- Project Settings ---

func (e *Engine) SetProjectSetting(key, value string) error {
	_, err := e.DB.Exec(`
		INSERT INTO project_settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP
	`, key, value)
	return err
}

func (e *Engine) GetProjectSetting(key string) (string, error) {
	var value string
	err := e.DB.QueryRow(`SELECT value FROM project_settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (e *Engine) GetAllProjectSettings() (map[string]string, error) {
	rows, err := e.DB.Query(`SELECT key, value FROM project_settings`)
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
	return settings, nil
}

// --- Decisions ---

func (e *Engine) AddDecision(topic, decision, reason string) error {
	_, err := e.DB.Exec(`INSERT INTO decisions (session_id, topic, decision, reason) VALUES (?, ?, ?, ?)`,
		e.SessionID, topic, decision, reason)
	return err
}

func (e *Engine) GetDecisions() ([]Decision, error) {
	rows, err := e.DB.Query(`SELECT topic, decision, reason, created_at FROM decisions ORDER BY created_at DESC`)
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
	return decisions, nil
}

// --- Learned Patterns ---

func (e *Engine) AddPattern(patternType, description string) error {
	_, err := e.DB.Exec(`INSERT INTO learned_patterns (pattern_type, description) VALUES (?, ?)`,
		patternType, description)
	return err
}

func (e *Engine) GetPatterns() ([]Pattern, error) {
	rows, err := e.DB.Query(`SELECT pattern_type, description, created_at FROM learned_patterns ORDER BY created_at DESC`)
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
	return patterns, nil
}

// --- Mode Management ---

func (e *Engine) SetMode(mode string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = mode
	if e.SessionID != "" {
		_, err := e.DB.Exec(`UPDATE sessions SET mode = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, mode, e.SessionID)
		return err
	}
	return nil
}

func (e *Engine) GetMode() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.SessionID != "" {
		var mode string
		err := e.DB.QueryRow(`SELECT COALESCE(mode, 'code') FROM sessions WHERE id = ?`, e.SessionID).Scan(&mode)
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
	Topic    string
	Decision string
	Reason   string
	CreatedAt time.Time
}

type Pattern struct {
	Type        string
	Description string
	CreatedAt   time.Time
}

// --- Knowledge Base ---

// IndexFile indexes a file into the knowledge base, splitting into chunks
func (e *Engine) IndexFile(filePath string) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	// Remove old entries for this file
	e.DB.Exec(`DELETE FROM kb_documents WHERE file_path = ?`, filePath)

	// Split into chunks (~500 lines each for large files, or whole file if small)
	lines := strings.Split(string(content), "\n")
	chunkSize := 100 // lines per chunk
	if len(lines) <= chunkSize {
		// Small file - index as single chunk
		_, err = e.DB.Exec(`INSERT INTO kb_documents (file_path, chunk_index, content) VALUES (?, 0, ?)`,
			filePath, string(content))
		return err
	}

	// Large file - split into chunks
	for i := 0; i < len(lines); i += chunkSize {
		end := i + chunkSize
		if end > len(lines) {
			end = len(lines)
		}
		chunk := strings.Join(lines[i:end], "\n")
		chunkIdx := i / chunkSize
		_, err = e.DB.Exec(`INSERT INTO kb_documents (file_path, chunk_index, content) VALUES (?, ?, ?)`,
			filePath, chunkIdx, chunk)
		if err != nil {
			return err
		}
	}
	return nil
}

// IndexDirectory indexes all files in a directory recursively
func (e *Engine) IndexDirectory(dirPath string, extensions []string) (int, error) {
	count := 0
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() {
			// Skip hidden dirs and common non-source dirs
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "node_modules" || base == "vendor" || base == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}

		// Check extension filter
		if len(extensions) > 0 {
			ext := strings.TrimPrefix(filepath.Ext(path), ".")
			matched := false
			for _, e := range extensions {
				if ext == e {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}

		// Skip binary/large files
		if info.Size() > 1024*1024 { // skip >1MB
			return nil
		}

		if err := e.IndexFile(path); err != nil {
			return nil // skip individual file errors
		}
		count++
		return nil
	})
	return count, err
}

// SearchKB performs full-text search across indexed documents
func (e *Engine) SearchKB(query string, limit int) ([]KBResult, error) {
	if limit == 0 {
		limit = 10
	}

	rows, err := e.DB.Query(`
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
		results = append(results, r)
	}
	return results, nil
}

// KBStatus returns stats about indexed documents
func (e *Engine) KBStatus() (KBStats, error) {
	var stats KBStats
	e.DB.QueryRow(`SELECT COUNT(*) FROM kb_documents`).Scan(&stats.TotalChunks)
	e.DB.QueryRow(`SELECT COUNT(DISTINCT file_path) FROM kb_documents`).Scan(&stats.TotalFiles)
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
	result, err := e.DB.Exec(`INSERT INTO todos (session_id, description, details) VALUES (?, ?, ?)`,
		e.SessionID, description, details)
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

	rows, err := e.DB.Query(query, e.SessionID)
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
	return todos, nil
}

func (e *Engine) CompleteTodo(id int) error {
	_, err := e.DB.Exec(`UPDATE todos SET status = 'completed', completed_at = CURRENT_TIMESTAMP WHERE id = ? AND session_id = ?`,
		id, e.SessionID)
	return err
}

func (e *Engine) RemoveTodo(id int) error {
	_, err := e.DB.Exec(`DELETE FROM todos WHERE id = ? AND session_id = ?`, id, e.SessionID)
	return err
}

type Todo struct {
	ID          int
	Description string
	Details     string
	Status      string
	CreatedAt   time.Time
	CompletedAt string
}
