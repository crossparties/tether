package mount

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// DefaultLeaseTTL is the default ownership lease duration for a Crew session.
const DefaultLeaseTTL = 5 * time.Minute

// ErrLeaseConflict is returned when a file write or lease request conflicts with
// another active Crew session's non-expired File Ownership Lease Lock.
var ErrLeaseConflict = errors.New("file ownership lease conflict")

// LeaseInfo describes an active, non-expired file ownership lease.
type LeaseInfo struct {
	CrewID    string    `json:"crew_id"`
	Files     []string  `json:"files"`
	ExpiresAt time.Time `json:"expires_at"`
}

type leaseEntry struct {
	files     []string
	expiresAt time.Time
}

// SetActorID identifies the caller ("main" or crew ID) for lease enforcement on OpWrite.
func (e *Engine) SetActorID(actorID string) {
	e.mu.Lock()
	e.actorID = strings.TrimSpace(actorID)
	e.mu.Unlock()
}

// ActorID returns the configured actor identifier (default "main").
func (e *Engine) ActorID() string {
	e.mu.RLock()
	id := e.actorID
	e.mu.RUnlock()
	if id == "" {
		return "main"
	}
	return id
}

func (e *Engine) nowUTC() time.Time {
	e.mu.RLock()
	fn := e.now
	e.mu.RUnlock()
	if fn != nil {
		return fn().UTC()
	}
	return time.Now().UTC()
}

// SetClockForTest overrides the clock used for lease TTL evaluation in unit tests.
func (e *Engine) SetClockForTest(fn func() time.Time) {
	e.mu.Lock()
	e.now = fn
	e.mu.Unlock()
}

// AcquireLeaseLock acquires an exclusive write ownership lease for crewID over paths.
// If any path is already leased by a different active non-expired crew, it returns ErrLeaseConflict.
func (e *Engine) AcquireLeaseLock(crewID string, paths []string, ttl time.Duration) error {
	crewID = strings.TrimSpace(crewID)
	if crewID == "" {
		return fmt.Errorf("crew_id required for lease lock")
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}

	normPaths, err := e.normalizeLeasePaths(paths)
	if err != nil {
		return err
	}
	if len(normPaths) == 0 {
		return nil
	}

	now := e.nowUTC()
	expiresAt := now.Add(ttl)

	e.mu.RLock()
	writeDB := e.writeDB
	e.mu.RUnlock()

	if writeDB == nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.leases == nil {
			e.leases = make(map[string]leaseEntry)
		}
		for otherID, entry := range e.leases {
			if otherID == crewID || !entry.expiresAt.After(now) {
				continue
			}
			for _, p := range normPaths {
				if leaseMatchesAny(p, entry.files) {
					return fmt.Errorf("%w: %q is leased by crew %q until %s", ErrLeaseConflict, p, otherID, entry.expiresAt.Format(time.RFC3339))
				}
			}
		}
		e.leases[crewID] = leaseEntry{files: normPaths, expiresAt: expiresAt}
		return nil
	}

	tx, err := writeDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id, lease_files_json, lease_expires_at
		FROM crew_sessions
		WHERE status = 'active' AND id <> ? AND lease_expires_at IS NOT NULL AND lease_files_json <> '[]'
	`, crewID)
	if err != nil {
		return err
	}

	type activeRow struct {
		id  string
		raw string
		exp string
	}
	var others []activeRow
	for rows.Next() {
		var r activeRow
		var exp sql.NullString
		if err := rows.Scan(&r.id, &r.raw, &exp); err != nil {
			_ = rows.Close()
			return err
		}
		if exp.Valid {
			r.exp = exp.String
			others = append(others, r)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, o := range others {
		expTime, perr := parseLeaseTime(o.exp)
		if perr != nil || !expTime.After(now) {
			continue
		}
		var otherFiles []string
		if err := json.Unmarshal([]byte(o.raw), &otherFiles); err != nil {
			continue
		}
		for _, p := range normPaths {
			if leaseMatchesAny(p, otherFiles) {
				return fmt.Errorf("%w: %q is leased by crew %q until %s", ErrLeaseConflict, p, o.id, expTime.Format(time.RFC3339))
			}
		}
	}

	rawJSON, _ := json.Marshal(normPaths)
	expStr := expiresAt.Format(time.RFC3339Nano)
	res, err := tx.Exec(`
		UPDATE crew_sessions
		SET lease_files_json = ?, lease_expires_at = ?, status = 'active'
		WHERE id = ?
	`, string(rawJSON), expStr, crewID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := tx.Exec(`
			INSERT INTO crew_sessions (id, parent_thread_id, name, status, lease_files_json, lease_expires_at)
			VALUES (?, '', ?, 'active', ?, ?)
		`, crewID, crewID, string(rawJSON), expStr); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HeartbeatLease extends the active lease expiry for crewID by ttl.
func (e *Engine) HeartbeatLease(crewID string, ttl time.Duration) error {
	crewID = strings.TrimSpace(crewID)
	if crewID == "" {
		return fmt.Errorf("crew_id required")
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	newExp := e.nowUTC().Add(ttl)

	e.mu.RLock()
	writeDB := e.writeDB
	e.mu.RUnlock()

	if writeDB == nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		if entry, ok := e.leases[crewID]; ok {
			entry.expiresAt = newExp
			e.leases[crewID] = entry
			return nil
		}
		return fmt.Errorf("no active lease for crew %q", crewID)
	}

	res, err := writeDB.Exec(`
		UPDATE crew_sessions
		SET lease_expires_at = ?
		WHERE id = ? AND status = 'active'
	`, newExp.Format(time.RFC3339Nano), crewID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no active crew session %q", crewID)
	}
	return nil
}

// ReleaseLeaseLock clears any held file ownership lease for crewID. Safe in defer.
func (e *Engine) ReleaseLeaseLock(crewID string) error {
	crewID = strings.TrimSpace(crewID)
	if crewID == "" {
		return nil
	}

	e.mu.RLock()
	writeDB := e.writeDB
	e.mu.RUnlock()

	if writeDB == nil {
		e.mu.Lock()
		delete(e.leases, crewID)
		e.mu.Unlock()
		return nil
	}

	_, err := writeDB.Exec(`
		UPDATE crew_sessions
		SET lease_files_json = '[]', lease_expires_at = NULL
		WHERE id = ?
	`, crewID)
	return err
}

// CheckLease verifies whether rawPath is locked by a different active, non-expired crew.
func (e *Engine) CheckLease(rawPath, actorID string) (holderCrewID string, expiresAt time.Time, err error) {
	relSlash, _, _, rerr := ResolveWithinWorkspace(rawPath, e.absRoot)
	if rerr != nil {
		return "", time.Time{}, nil
	}
	actor := strings.TrimSpace(actorID)
	if actor == "" {
		actor = e.ActorID()
	}
	now := e.nowUTC()

	leases, lerr := e.listLeasesLocked(now)
	if lerr != nil {
		return "", time.Time{}, lerr
	}
	for _, l := range leases {
		if l.CrewID == actor {
			continue
		}
		if leaseMatchesAny(relSlash, l.Files) {
			return l.CrewID, l.ExpiresAt, nil
		}
	}
	return "", time.Time{}, nil
}

// ListActiveLeases returns all currently non-expired crew file ownership leases.
func (e *Engine) ListActiveLeases() ([]LeaseInfo, error) {
	return e.listLeasesLocked(e.nowUTC())
}

func (e *Engine) listLeasesLocked(now time.Time) ([]LeaseInfo, error) {
	e.mu.RLock()
	db := e.readDB
	if db == nil {
		db = e.writeDB
	}
	e.mu.RUnlock()

	if db == nil {
		e.mu.RLock()
		defer e.mu.RUnlock()
		var out []LeaseInfo
		for cid, entry := range e.leases {
			if entry.expiresAt.After(now) && len(entry.files) > 0 {
				cp := make([]string, len(entry.files))
				copy(cp, entry.files)
				out = append(out, LeaseInfo{
					CrewID:    cid,
					Files:     cp,
					ExpiresAt: entry.expiresAt,
				})
			}
		}
		return out, nil
	}

	rows, err := db.Query(`
		SELECT id, lease_files_json, lease_expires_at
		FROM crew_sessions
		WHERE status = 'active' AND lease_expires_at IS NOT NULL AND lease_files_json <> '[]'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LeaseInfo
	for rows.Next() {
		var cid, raw string
		var exp sql.NullString
		if err := rows.Scan(&cid, &raw, &exp); err != nil {
			return nil, err
		}
		if !exp.Valid {
			continue
		}
		expTime, perr := parseLeaseTime(exp.String)
		if perr != nil || !expTime.After(now) {
			continue
		}
		var files []string
		if err := json.Unmarshal([]byte(raw), &files); err != nil || len(files) == 0 {
			continue
		}
		out = append(out, LeaseInfo{
			CrewID:    cid,
			Files:     files,
			ExpiresAt: expTime,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Engine) normalizeLeasePaths(paths []string) ([]string, error) {
	seen := make(map[string]bool, len(paths))
	var out []string
	for _, raw := range paths {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		relSlash, _, _, err := ResolveWithinWorkspace(trimmed, e.absRoot)
		if err != nil {
			return nil, err
		}
		if relSlash == "." {
			return nil, fmt.Errorf("cannot lease workspace root '.'")
		}
		if _, secret := MatchesBuiltinSecret(relSlash); secret {
			return nil, fmt.Errorf("access denied: cannot lease built-in protected path %q", relSlash)
		}
		if !seen[relSlash] {
			seen[relSlash] = true
			out = append(out, relSlash)
		}
	}
	return out, nil
}

func leaseMatchesAny(targetRel string, leased []string) bool {
	t := strings.Trim(filepath.ToSlash(filepath.Clean(targetRel)), "/")
	for _, l := range leased {
		n := strings.Trim(filepath.ToSlash(filepath.Clean(l)), "/")
		if n == "" || n == "." {
			continue
		}
		if t == n || strings.HasPrefix(t, n+"/") || strings.HasPrefix(n, t+"/") {
			return true
		}
	}
	return false
}

func parseLeaseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid lease timestamp %q", s)
}
