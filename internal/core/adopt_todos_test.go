package core

import (
	"strings"
	"testing"
)

// TestAdoptPreviousTODOsCarriesPendingWork is the regression guard for the
// backlog that vanished between sessions: todo_list filters on session_id, so a
// new session used to start with an empty list even though the previous session
// left unfinished tasks behind.
func TestAdoptPreviousTODOsCarriesPendingWork(t *testing.T) {
	e := newTestEngine(t)

	// Two todos in the current session: one stays open, one is finished.
	openID, err := e.AddTodo("finish undo wiring", "wire snapshot.LastTurn")
	if err != nil {
		t.Fatalf("AddTodo: %v", err)
	}
	doneID, err := e.AddTodo("write docs", "")
	if err != nil {
		t.Fatalf("AddTodo: %v", err)
	}
	if err := e.CompleteTodo(int(doneID)); err != nil {
		t.Fatalf("CompleteTodo: %v", err)
	}

	// Simulate a brand new session on the same branch.
	oldSession := e.SessionID
	newSession := "fresh-session-id"
	if _, err := e.DB.Exec(`INSERT INTO sessions (id, status, mode, git_branch) VALUES (?, 'active', 'code', ?)`, newSession, e.Branch); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	e.adoptPreviousTODOs(newSession)

	// The new session must see the open todo, with a pointer to its origin.
	var desc, details string
	err = e.DB.QueryRow(`SELECT description, details FROM todos WHERE session_id = ? AND description = ?`,
		newSession, "finish undo wiring").Scan(&desc, &details)
	if err != nil {
		t.Fatalf("open todo was not carried over: %v", err)
	}
	if !strings.Contains(details, "carried over") {
		t.Errorf("carried todo should record its origin, details = %q", details)
	}

	// Completed work must not come back.
	var completedCopies int
	if err := e.DB.QueryRow(`SELECT COUNT(*) FROM todos WHERE session_id = ? AND description = ?`,
		newSession, "write docs").Scan(&completedCopies); err != nil {
		t.Fatalf("count completed copies: %v", err)
	}
	if completedCopies != 0 {
		t.Errorf("completed todo was carried over %d time(s)", completedCopies)
	}

	// The original is retired so it cannot be adopted again by a later session.
	var oldStatus string
	if err := e.DB.QueryRow(`SELECT status FROM todos WHERE id = ?`, openID).Scan(&oldStatus); err != nil {
		t.Fatalf("read original status: %v", err)
	}
	if oldStatus != "carried" {
		t.Errorf("original todo status = %q, want carried", oldStatus)
	}

	// Retired rows must stay out of the open list.
	var openCount int
	if err := e.DB.QueryRow(`SELECT COUNT(*) FROM todos WHERE status = 'pending' AND session_id = ?`, oldSession).Scan(&openCount); err != nil {
		t.Fatalf("count pending in old session: %v", err)
	}
	if openCount != 0 {
		t.Errorf("old session still reports %d pending todo(s)", openCount)
	}
}

// TestAdoptPreviousTODOsIsBranchScoped keeps unrelated work from leaking
// across branches, where it would look like noise.
func TestAdoptPreviousTODOsIsBranchScoped(t *testing.T) {
	e := newTestEngine(t)

	if _, err := e.DB.Exec(`INSERT INTO sessions (id, status, mode, git_branch) VALUES ('other-branch-session', 'active', 'code', 'feat/unrelated')`); err != nil {
		t.Fatalf("insert other session: %v", err)
	}
	if _, err := e.DB.Exec(`INSERT INTO todos (session_id, git_branch, description, status) VALUES ('other-branch-session', 'feat/unrelated', 'unrelated work', 'pending')`); err != nil {
		t.Fatalf("insert other todo: %v", err)
	}

	if _, err := e.DB.Exec(`INSERT INTO sessions (id, status, mode, git_branch) VALUES ('new-branch-session', 'active', 'code', ?)`, e.Branch); err != nil {
		t.Fatalf("insert new session: %v", err)
	}
	e.adoptPreviousTODOs("new-branch-session")

	var leaked int
	if err := e.DB.QueryRow(`SELECT COUNT(*) FROM todos WHERE session_id = 'new-branch-session' AND description = 'unrelated work'`).Scan(&leaked); err != nil {
		t.Fatalf("count leaked: %v", err)
	}
	if leaked != 0 {
		t.Errorf("todo from another branch leaked into the new session")
	}
}
