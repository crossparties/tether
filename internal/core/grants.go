package core

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Approval grants let the user say "stop asking about this path" once, instead
// of confirming every single write. A grant is scoped to a path prefix and to
// the session that created it, so a decision made in one session cannot silently
// authorize writes in another.
//
// The default lifetime is deliberately short. A grant is a convenience, not a
// policy: it should expire while the user is still paying attention.

// DefaultGrantTTLMinutes is how long an "approve all changes here" grant lasts.
const DefaultGrantTTLMinutes = 60

// ApprovalGrant is one stored permission.
type ApprovalGrant struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	PathScope string `json:"path_scope"`
	Kind      string `json:"kind"` // "path" or "all"
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

// AddApprovalGrant records permission for pathScope. An empty scope with kind
// "all" authorizes every path for the session.
func (e *Engine) AddApprovalGrant(pathScope, kind string) error {
	if kind == "" {
		kind = "path"
	}
	scope := filepath.ToSlash(strings.TrimSpace(pathScope))
	if kind == "path" && scope == "" {
		return fmt.Errorf("path scope is required for a path grant")
	}
	ttl := DefaultGrantTTLMinutes
	// datetime('now', ?) evaluates the modifier in SQLite. Binding "+60 minutes"
	// as a plain value would store the literal string, and the grant would look
	// expired immediately.
	_, err := e.writeDB.Exec(`
		INSERT INTO approval_grants (session_id, path_scope, kind, expires_at)
		VALUES (?, ?, ?, datetime('now', ?))
	`, e.SessionID, scope, kind, fmt.Sprintf("+%d minutes", ttl))
	return err
}

// HasApprovalGrant reports whether a live grant covers path.
//
// Coverage is prefix-based on path segments, never on raw strings: a grant for
// "docs" must not authorize "docs-secret/key.pem". The comparison therefore
// appends a separator before testing the prefix.
func (e *Engine) HasApprovalGrant(path string) (bool, error) {
	e.expireApprovalGrants()

	cleanPath := filepath.ToSlash(filepath.Clean(path))
	// Command grants share the table but must never authorize a write. Filter
	// them out in SQL so a grant whose scope happens to look like a path (for
	// example a command starting with a directory name) cannot leak into file
	// permissions.
	rows, err := e.readDB.Query(`
		SELECT path_scope, kind FROM approval_grants
		WHERE session_id = ?
		  AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)
		  AND kind NOT IN (?, ?)
	`, e.SessionID, GrantKindCommand, GrantKindCommandPrefix)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var scope, kind string
		if err := rows.Scan(&scope, &kind); err != nil {
			return false, err
		}
		if kind == "all" {
			return true, nil
		}
		if scopeCoversPath(scope, cleanPath) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Command grant kinds. They live in the same table as path grants but must
// never be consulted by HasApprovalGrant: a command approval says nothing about
// which files may be written, and a path approval says nothing about which
// commands may run.
const (
	// GrantKindCommand approves one exact command line, once.
	GrantKindCommand = "command"
	// GrantKindCommandPrefix approves any command starting with a prefix.
	GrantKindCommandPrefix = "command-prefix"
)

// AddCommandGrant records permission to run cmd. With prefix=true the grant
// covers every later command that begins with cmd followed by a space or the
// end of the string, so approving "go test" also allows "go test ./...".
func (e *Engine) AddCommandGrant(cmd string, prefix bool) error {
	scope := strings.TrimSpace(cmd)
	if scope == "" {
		return fmt.Errorf("command is required for a command grant")
	}
	kind := GrantKindCommand
	if prefix {
		kind = GrantKindCommandPrefix
	}
	_, err := e.writeDB.Exec(`
		INSERT INTO approval_grants (session_id, path_scope, kind, expires_at)
		VALUES (?, ?, ?, datetime('now', ?))
	`, e.SessionID, scope, kind, fmt.Sprintf("+%d minutes", DefaultGrantTTLMinutes))
	return err
}

// deleteGrant consumes a one-shot command grant so a single "approve once"
// cannot be replayed for every later invocation.
func (e *Engine) deleteGrant(scope, kind string) {
	_, _ = e.writeDB.Exec(`
		DELETE FROM approval_grants
		WHERE session_id = ? AND path_scope = ? AND kind = ?
	`, e.SessionID, scope, kind)
}

// HasCommandGrant reports whether a live grant allows running cmd.
//
// An exact grant matches the whole line and is consumed on use, so "approve
// once" really means once. A prefix grant matches a leading token run and
// survives until it expires, which is what "stop asking about go test" needs.
func (e *Engine) HasCommandGrant(cmd string) (bool, error) {
	e.expireApprovalGrants()

	want := strings.TrimSpace(cmd)
	if want == "" {
		return false, nil
	}
	rows, err := e.readDB.Query(`
		SELECT path_scope, kind FROM approval_grants
		WHERE session_id = ?
		  AND kind IN (?, ?)
		  AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)
	`, e.SessionID, GrantKindCommand, GrantKindCommandPrefix)
	if err != nil {
		return false, err
	}

	var (
		exactHit  bool
		prefixHit bool
	)
	for rows.Next() {
		var scope, kind string
		if err := rows.Scan(&scope, &kind); err != nil {
			rows.Close()
			return false, err
		}
		switch kind {
		case GrantKindCommand:
			if scope == want {
				exactHit = true
			}
		case GrantKindCommandPrefix:
			if commandHasPrefix(want, scope) {
				prefixHit = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()

	if exactHit {
		e.deleteGrant(want, GrantKindCommand)
		return true, nil
	}
	if prefixHit {
		return true, nil
	}
	return false, nil
}

// commandHasPrefix reports whether cmd starts with prefix as a whole token run.
// It refuses bare substring matches so a prefix grant for "git" cannot authorize
// "github-transfer" and, more importantly, "go test" cannot authorize "go testall".
func commandHasPrefix(cmd, prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return false
	}
	if cmd == prefix {
		return true
	}
	return strings.HasPrefix(cmd, prefix+" ")
}

// ListApprovalGrants returns live grants for the current session, so the CLI can
// show what the user has already authorized.
func (e *Engine) ListApprovalGrants() ([]ApprovalGrant, error) {
	e.expireApprovalGrants()
	rows, err := e.readDB.Query(`
		SELECT id, session_id, path_scope, kind, created_at, COALESCE(expires_at, '')
		FROM approval_grants
		WHERE session_id = ?
		ORDER BY id DESC
	`, e.SessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var grants []ApprovalGrant
	for rows.Next() {
		var g ApprovalGrant
		if err := rows.Scan(&g.ID, &g.SessionID, &g.PathScope, &g.Kind, &g.CreatedAt, &g.ExpiresAt); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// RevokeApprovalGrants drops every grant for the current session and reports how
// many were removed, giving the user a way back to the safe default.
func (e *Engine) RevokeApprovalGrants() (int64, error) {
	res, err := e.writeDB.Exec(`DELETE FROM approval_grants WHERE session_id = ?`, e.SessionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// expireApprovalGrants deletes grants past their lifetime. It runs before every
// check so a stale grant can never authorize a write.
func (e *Engine) expireApprovalGrants() {
	_, _ = e.writeDB.Exec(`
		DELETE FROM approval_grants
		WHERE expires_at IS NOT NULL AND expires_at <= CURRENT_TIMESTAMP
	`)
}

// scopeCoversPath reports whether scope is an ancestor of, or equal to, p.
//
// An empty scope or "." means the workspace root itself, which is what the CLI
// stores when the user says "stop asking about everything here". Treating it as
// "no match" made that option silently ineffective: the grant was written, the
// user believed the prompts would stop, and every write asked again.
func scopeCoversPath(scope, p string) bool {
	scope = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(scope)), "/")
	if scope == "" || scope == "." {
		return true
	}
	// Both sides must be cleaned. Cleaning only the scope let a caller walk
	// out of a grant with a relative segment: scope "internal/hooks" matched
	// "internal/hooks/../core/x.go" by prefix, even though that path resolves
	// to internal/core, a package the grant never covered.
	p = filepath.ToSlash(filepath.Clean(p))
	if scope == p {
		return true
	}
	return strings.HasPrefix(p, scope+"/")
}

// GrantExpiryHint renders when a new grant would expire, for user-facing output.
func GrantExpiryHint() string {
	return time.Now().Add(DefaultGrantTTLMinutes * time.Minute).Format("15:04")
}
