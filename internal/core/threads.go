package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ConversationThread represents a persistent multi-turn discussion thread in .tether/session.db.
type ConversationThread struct {
	ID          string `json:"id"`
	Workspace   string `json:"workspace"`
	GitBranch   string `json:"git_branch"`
	Title       string `json:"title"`
	Mode        string `json:"mode"`
	PinnedKeyID string `json:"pinned_key_id"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// ConversationEvent represents one completed step/turn/condensation event in a thread.
// Per-token streaming deltas are never written here; only completed turns/steps are persisted.
type ConversationEvent struct {
	ID          int64  `json:"id"`
	ThreadID    string `json:"thread_id"`
	CrewID      string `json:"crew_id,omitempty"`
	Seq         int    `json:"seq"`
	Role        string `json:"role"`
	UsageID     string `json:"usage_id"`
	PayloadJSON string `json:"payload_json"`
	TokenEst    int    `json:"token_est"`
	CreatedAt   string `json:"created_at"`
}

const maxStoredEventPayloadBytes = 16 * 1024

// CreateThread creates and persists a new conversation thread.
func (e *Engine) CreateThread(id, title, mode string) (ConversationThread, error) {
	if e == nil || e.writeDB == nil {
		return ConversationThread{}, fmt.Errorf("engine is closed")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = uuid.New().String()
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = e.GetMode()
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Thread " + id[:min(8, len(id))]
	}
	_, err := e.writeDB.Exec(`
		INSERT INTO conversation_threads (id, workspace, git_branch, title, mode, pinned_key_id)
		VALUES (?, ?, ?, ?, ?, '')
	`, id, e.Workspace, e.Branch, title, mode)
	if err != nil {
		return ConversationThread{}, err
	}
	return e.GetThread(id)
}

// EnsureThread returns an existing thread by id or creates it safely if absent.
func (e *Engine) EnsureThread(id, title string) (ConversationThread, error) {
	if e == nil || e.writeDB == nil {
		return ConversationThread{}, fmt.Errorf("engine is closed")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = e.SessionID
	}
	if id == "" {
		id = uuid.New().String()
	}
	if strings.TrimSpace(title) == "" {
		title = "Main Thread"
	}
	_, err := e.writeDB.Exec(`
		INSERT OR IGNORE INTO conversation_threads (id, workspace, git_branch, title, mode, pinned_key_id)
		VALUES (?, ?, ?, ?, ?, '')
	`, id, e.Workspace, e.Branch, title, e.GetMode())
	if err != nil {
		return ConversationThread{}, err
	}
	return e.GetThread(id)
}

// GetThread loads a single thread by ID from readDB.
func (e *Engine) GetThread(id string) (ConversationThread, error) {
	if e == nil || e.readDB == nil {
		return ConversationThread{}, fmt.Errorf("engine is closed")
	}
	var t ConversationThread
	err := e.readDB.QueryRow(`
		SELECT id, workspace, git_branch, title, mode, pinned_key_id,
		       COALESCE(created_at, ''), COALESCE(updated_at, '')
		FROM conversation_threads
		WHERE id = ?
	`, strings.TrimSpace(id)).Scan(
		&t.ID, &t.Workspace, &t.GitBranch, &t.Title, &t.Mode, &t.PinnedKeyID,
		&t.CreatedAt, &t.UpdatedAt,
	)
	return t, err
}

// ListThreads returns persisted threads ordered newest-updated first.
func (e *Engine) ListThreads(limit int) ([]ConversationThread, error) {
	if e == nil || e.readDB == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := e.readDB.Query(`
		SELECT id, workspace, git_branch, title, mode, pinned_key_id,
		       COALESCE(created_at, ''), COALESCE(updated_at, '')
		FROM conversation_threads
		ORDER BY updated_at DESC, created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConversationThread
	for rows.Next() {
		var t ConversationThread
		if err := rows.Scan(
			&t.ID, &t.Workspace, &t.GitBranch, &t.Title, &t.Mode, &t.PinnedKeyID,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateThreadTitle updates a thread's title.
func (e *Engine) UpdateThreadTitle(id, title string) error {
	if e == nil || e.writeDB == nil {
		return fmt.Errorf("engine is closed")
	}
	res, err := e.writeDB.Exec(`
		UPDATE conversation_threads
		SET title = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, strings.TrimSpace(title), strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteThread deletes a thread and all of its conversation events atomically.
func (e *Engine) DeleteThread(id string) error {
	if e == nil || e.writeDB == nil {
		return fmt.Errorf("engine is closed")
	}
	id = strings.TrimSpace(id)
	tx, err := e.writeDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM conversation_events WHERE thread_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM conversation_threads WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// AppendThreadEvent appends a completed turn/step/condensation event with a
// strictly monotonic per-thread seq inside a single serialized transaction.
func (e *Engine) AppendThreadEvent(ev ConversationEvent) (ConversationEvent, error) {
	if e == nil || e.writeDB == nil {
		return ConversationEvent{}, fmt.Errorf("engine is closed")
	}
	ev.ThreadID = strings.TrimSpace(ev.ThreadID)
	if ev.ThreadID == "" {
		ev.ThreadID = e.SessionID
	}
	if ev.ThreadID == "" {
		return ConversationEvent{}, fmt.Errorf("thread_id required")
	}
	ev.Role = strings.TrimSpace(ev.Role)
	if ev.Role == "" {
		return ConversationEvent{}, fmt.Errorf("role required")
	}
	if strings.TrimSpace(ev.UsageID) == "" {
		if ev.CrewID != "" {
			ev.UsageID = "crew:" + ev.CrewID
		} else {
			ev.UsageID = "main"
		}
	}
	ev.PayloadJSON = boundEventPayloadJSON(ev.PayloadJSON)
	if ev.TokenEst <= 0 {
		ev.TokenEst = (len(ev.PayloadJSON) + 3) / 4
	}

	tx, err := e.writeDB.Begin()
	if err != nil {
		return ConversationEvent{}, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO conversation_threads (id, workspace, git_branch, title, mode, pinned_key_id)
		VALUES (?, ?, ?, ?, ?, '')
	`, ev.ThreadID, e.Workspace, e.Branch, "Thread "+ev.ThreadID[:min(8, len(ev.ThreadID))], e.GetMode()); err != nil {
		return ConversationEvent{}, err
	}

	if ev.Seq <= 0 {
		var maxSeq int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM conversation_events WHERE thread_id = ?`, ev.ThreadID).Scan(&maxSeq); err != nil {
			return ConversationEvent{}, err
		}
		ev.Seq = maxSeq + 1
	}

	res, err := tx.Exec(`
		INSERT INTO conversation_events (thread_id, crew_id, seq, role, usage_id, payload_json, token_est)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, ev.ThreadID, ev.CrewID, ev.Seq, ev.Role, ev.UsageID, ev.PayloadJSON, ev.TokenEst)
	if err != nil {
		return ConversationEvent{}, err
	}
	id, err := res.LastInsertId()
	if err == nil {
		ev.ID = id
	}
	if _, err := tx.Exec(`UPDATE conversation_threads SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, ev.ThreadID); err != nil {
		return ConversationEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return ConversationEvent{}, err
	}
	if ev.CreatedAt == "" {
		ev.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return ev, nil
}

// ListThreadEvents reads events for threadID ordered by monotonic seq ASC.
func (e *Engine) ListThreadEvents(threadID string, limit int) ([]ConversationEvent, error) {
	return e.queryThreadEvents(strings.TrimSpace(threadID), "", false, limit)
}

// ListThreadEventsForActor returns events belonging to main (crew_id == "") or a specific crewID.
func (e *Engine) ListThreadEventsForActor(threadID, crewID string, limit int) ([]ConversationEvent, error) {
	return e.queryThreadEvents(strings.TrimSpace(threadID), strings.TrimSpace(crewID), true, limit)
}

func (e *Engine) queryThreadEvents(threadID, crewID string, filterCrew bool, limit int) ([]ConversationEvent, error) {
	if e == nil || e.readDB == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	if threadID == "" {
		threadID = e.SessionID
	}
	if limit <= 0 {
		limit = 500
	}

	var rows *sql.Rows
	var err error
	if filterCrew {
		rows, err = e.readDB.Query(`
			SELECT id, thread_id, crew_id, seq, role, usage_id, payload_json, token_est, COALESCE(created_at, '')
			FROM conversation_events
			WHERE thread_id = ? AND crew_id = ?
			ORDER BY seq ASC, id ASC
			LIMIT ?
		`, threadID, crewID, limit)
	} else {
		rows, err = e.readDB.Query(`
			SELECT id, thread_id, crew_id, seq, role, usage_id, payload_json, token_est, COALESCE(created_at, '')
			FROM conversation_events
			WHERE thread_id = ?
			ORDER BY seq ASC, id ASC
			LIMIT ?
		`, threadID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConversationEvent
	for rows.Next() {
		var ev ConversationEvent
		if err := rows.Scan(
			&ev.ID, &ev.ThreadID, &ev.CrewID, &ev.Seq, &ev.Role,
			&ev.UsageID, &ev.PayloadJSON, &ev.TokenEst, &ev.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListThreadEventsForLoad returns events needed to reconstruct an active thread:
// if a condensation checkpoint exists, it loads from the latest condensation seq forward;
// otherwise it loads the newest fallbackLimit events in chronological seq ASC order.
func (e *Engine) ListThreadEventsForLoad(threadID, crewID string, fallbackLimit int) ([]ConversationEvent, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	db := e.readDB
	if db == nil {
		db = e.writeDB
	}
	if db == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		threadID = e.SessionID
	}
	crewID = strings.TrimSpace(crewID)
	if fallbackLimit <= 0 {
		fallbackLimit = 500
	}

	var condSeq sql.NullInt64
	err := db.QueryRow(`
		SELECT seq FROM conversation_events
		WHERE thread_id = ? AND crew_id = ? AND role = 'condensation'
		ORDER BY seq DESC, id DESC
		LIMIT 1
	`, threadID, crewID).Scan(&condSeq)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	var rows *sql.Rows
	reverseResult := false
	if condSeq.Valid {
		rows, err = db.Query(`
			SELECT id, thread_id, crew_id, seq, role, usage_id, payload_json, token_est, COALESCE(created_at, '')
			FROM conversation_events
			WHERE thread_id = ? AND crew_id = ? AND seq >= ?
			ORDER BY seq ASC, id ASC
		`, threadID, crewID, condSeq.Int64)
	} else {
		reverseResult = true
		rows, err = db.Query(`
			SELECT id, thread_id, crew_id, seq, role, usage_id, payload_json, token_est, COALESCE(created_at, '')
			FROM conversation_events
			WHERE thread_id = ? AND crew_id = ?
			ORDER BY seq DESC, id DESC
			LIMIT ?
		`, threadID, crewID, fallbackLimit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConversationEvent
	for rows.Next() {
		var ev ConversationEvent
		if err := rows.Scan(
			&ev.ID, &ev.ThreadID, &ev.CrewID, &ev.Seq, &ev.Role,
			&ev.UsageID, &ev.PayloadJSON, &ev.TokenEst, &ev.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if reverseResult {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

func boundEventPayloadJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	if len(trimmed) <= maxStoredEventPayloadBytes {
		return trimmed
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err == nil && m != nil {
		origLen := len(trimmed)
		if content, ok := m["content"].(string); ok && len(content) > 4096 {
			m["content"] = truncateMiddleBytes(content, 2048, "content")
			m["compressed"] = true
			m["orig_bytes"] = origLen
		}
		if reasoning, ok := m["reasoning_content"].(string); ok && len(reasoning) > 1024 {
			m["reasoning_content"] = truncateMiddleBytes(reasoning, 512, "reasoning")
			m["compressed"] = true
			m["orig_bytes"] = origLen
		}
		if out, merr := json.Marshal(m); merr == nil && len(out) <= maxStoredEventPayloadBytes {
			return string(out)
		}
		if calls, ok := m["tool_calls"].([]any); ok && len(calls) > 0 {
			argBudget := max(256, 4096/len(calls))
			for _, item := range calls {
				tc, ok := item.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := tc["function"].(map[string]any)
				if !ok {
					continue
				}
				if args, ok := fn["arguments"].(string); ok && len(args) > argBudget {
					fn["arguments"] = truncateToolArgsJSON(args, argBudget)
					m["compressed"] = true
					m["orig_bytes"] = origLen
				}
			}
			if out, merr := json.Marshal(m); merr == nil && len(out) <= maxStoredEventPayloadBytes {
				return string(out)
			}
		}
		compact := map[string]any{
			"role":       m["role"],
			"compressed": true,
			"orig_bytes": origLen,
		}
		if id, ok := m["tool_call_id"]; ok {
			compact["tool_call_id"] = id
		}
		if name, ok := m["name"]; ok {
			compact["name"] = name
		}
		if content, ok := m["content"].(string); ok && content != "" {
			compact["content"] = truncateMiddleBytes(content, 512, "content")
		}
		if out, merr := json.Marshal(compact); merr == nil && len(out) <= maxStoredEventPayloadBytes {
			return string(out)
		}
	}
	preview := trimmed
	if len(preview) > 8192 {
		preview = preview[:8192]
	}
	fallbackObj := map[string]any{
		"compressed": true,
		"orig_bytes": len(trimmed),
		"preview":    preview,
	}
	if role, _ := m["role"].(string); role != "" {
		fallbackObj["role"] = role
	}
	fallback, _ := json.Marshal(fallbackObj)
	return string(fallback)
}

func truncateMiddleBytes(s string, side int, label string) string {
	if len(s) <= side*2 {
		return s
	}
	return s[:side] + fmt.Sprintf("\n... [compressed %d %s bytes in session.db] ...\n", len(s)-side*2, label) + s[len(s)-side:]
}

func truncateToolArgsJSON(args string, budget int) string {
	keep := max(64, budget-64)
	if keep > len(args) {
		keep = len(args)
	}
	b, _ := json.Marshal(map[string]any{
		"_truncated": true,
		"orig_bytes": len(args),
		"preview":    args[:keep],
	})
	return string(b)
}
