package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CrewSession represents a persisted Crew worker session inside crew_sessions.
type CrewSession struct {
	ID              string    `json:"id"`
	ParentThreadID  string    `json:"parent_thread_id"`
	Name            string    `json:"name"`
	SkillName       string    `json:"skill_name,omitempty"`
	PurposeCategory string    `json:"purpose_category,omitempty"`
	Status          string    `json:"status"` // "active" | "closed" | "aborted"
	LeaseFiles      []string  `json:"lease_files,omitempty"`
	LeaseExpiresAt  time.Time `json:"lease_expires_at,omitempty"`
	Summary         string    `json:"summary,omitempty"`
	CreatedAt       string    `json:"created_at,omitempty"`
	ClosedAt        string    `json:"closed_at,omitempty"`
}

// CreateCrewSession inserts or updates a crew session row in .tether/session.db.
func (e *Engine) CreateCrewSession(cs CrewSession) error {
	if e == nil || e.writeDB == nil {
		return fmt.Errorf("engine is closed")
	}
	cs.ID = strings.TrimSpace(cs.ID)
	if cs.ID == "" {
		return fmt.Errorf("crew session id required")
	}
	if strings.TrimSpace(cs.ParentThreadID) == "" {
		cs.ParentThreadID = e.SessionID
	}
	if strings.TrimSpace(cs.Status) == "" {
		cs.Status = "active"
	}
	leaseJSON := encodeLeaseFilesJSON(cs.LeaseFiles)
	var expVal any
	if !cs.LeaseExpiresAt.IsZero() {
		expVal = cs.LeaseExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := e.writeDB.Exec(`
		INSERT INTO crew_sessions (
			id, parent_thread_id, name, skill_name, purpose_category,
			status, lease_files_json, lease_expires_at, summary
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			parent_thread_id = excluded.parent_thread_id,
			name = excluded.name,
			skill_name = excluded.skill_name,
			purpose_category = excluded.purpose_category,
			status = excluded.status,
			lease_files_json = excluded.lease_files_json,
			lease_expires_at = excluded.lease_expires_at,
			summary = excluded.summary
	`, cs.ID, cs.ParentThreadID, cs.Name, cs.SkillName, cs.PurposeCategory, cs.Status, leaseJSON, expVal, cs.Summary)
	return err
}

// GetCrewSession loads a single crew session by id.
func (e *Engine) GetCrewSession(id string) (CrewSession, error) {
	if e == nil || e.readDB == nil {
		return CrewSession{}, fmt.Errorf("engine is closed")
	}
	var cs CrewSession
	var leaseJSON string
	var expStr, closedStr sql.NullString
	err := e.readDB.QueryRow(`
		SELECT id, parent_thread_id, name, skill_name, purpose_category,
		       status, lease_files_json, lease_expires_at, summary,
		       COALESCE(created_at, ''), closed_at
		FROM crew_sessions
		WHERE id = ?
	`, strings.TrimSpace(id)).Scan(
		&cs.ID, &cs.ParentThreadID, &cs.Name, &cs.SkillName, &cs.PurposeCategory,
		&cs.Status, &leaseJSON, &expStr, &cs.Summary,
		&cs.CreatedAt, &closedStr,
	)
	if err != nil {
		return CrewSession{}, err
	}
	cs.LeaseFiles = decodeLeaseFilesJSON(leaseJSON)
	if expStr.Valid && expStr.String != "" {
		if t, perr := parseSQLiteTime(expStr.String); perr == nil {
			cs.LeaseExpiresAt = t
		}
	}
	if closedStr.Valid {
		cs.ClosedAt = closedStr.String
	}
	return cs, nil
}

// ListCrewSessions returns crew sessions, optionally filtered by parentThreadID.
func (e *Engine) ListCrewSessions(parentThreadID string) ([]CrewSession, error) {
	if e == nil || e.readDB == nil {
		return nil, fmt.Errorf("engine is closed")
	}
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(parentThreadID) != "" {
		rows, err = e.readDB.Query(`
			SELECT id, parent_thread_id, name, skill_name, purpose_category,
			       status, lease_files_json, lease_expires_at, summary,
			       COALESCE(created_at, ''), closed_at
			FROM crew_sessions
			WHERE parent_thread_id = ?
			ORDER BY created_at DESC, rowid DESC
		`, strings.TrimSpace(parentThreadID))
	} else {
		rows, err = e.readDB.Query(`
			SELECT id, parent_thread_id, name, skill_name, purpose_category,
			       status, lease_files_json, lease_expires_at, summary,
			       COALESCE(created_at, ''), closed_at
			FROM crew_sessions
			ORDER BY created_at DESC, rowid DESC
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CrewSession
	for rows.Next() {
		var cs CrewSession
		var leaseJSON string
		var expStr, closedStr sql.NullString
		if err := rows.Scan(
			&cs.ID, &cs.ParentThreadID, &cs.Name, &cs.SkillName, &cs.PurposeCategory,
			&cs.Status, &leaseJSON, &expStr, &cs.Summary,
			&cs.CreatedAt, &closedStr,
		); err != nil {
			return nil, err
		}
		cs.LeaseFiles = decodeLeaseFilesJSON(leaseJSON)
		if expStr.Valid && expStr.String != "" {
			if t, perr := parseSQLiteTime(expStr.String); perr == nil {
				cs.LeaseExpiresAt = t
			}
		}
		if closedStr.Valid {
			cs.ClosedAt = closedStr.String
		}
		out = append(out, cs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// CloseCrewSession marks a crew session closed/aborted, stores summary, and clears any active leases.
func (e *Engine) CloseCrewSession(crewID, status, summary string) error {
	if e == nil || e.writeDB == nil {
		return fmt.Errorf("engine is closed")
	}
	if strings.TrimSpace(status) == "" {
		status = "closed"
	}
	_, err := e.writeDB.Exec(`
		UPDATE crew_sessions
		SET status = ?,
		    summary = CASE WHEN ? <> '' THEN ? ELSE summary END,
		    lease_files_json = '[]',
		    lease_expires_at = NULL,
		    closed_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, status, summary, summary, strings.TrimSpace(crewID))
	return err
}

func encodeLeaseFilesJSON(files []string) string {
	if len(files) == 0 {
		return "[]"
	}
	b, err := json.Marshal(files)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func decodeLeaseFilesJSON(raw string) []string {
	var files []string
	_ = json.Unmarshal([]byte(raw), &files)
	return files
}

func parseSQLiteTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format %q", s)
}
