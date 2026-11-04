package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupWorkspace returns a workspace dir with one existing file.
func setupWorkspace(t *testing.T) (string, *Store) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "target.txt"), []byte("original"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return ws, New(ws)
}

func TestCaptureAndRestoreModifiedFile(t *testing.T) {
	ws, store := setupWorkspace(t)
	target := filepath.Join(ws, "target.txt")

	if err := store.Capture("turn-1", target); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The agent overwrites the file.
	if err := os.WriteFile(target, []byte("clobbered"), 0644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	restored, err := store.Restore("turn-1", ws)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(restored) != 1 || restored[0] != "target.txt" {
		t.Fatalf("restored = %v, want [target.txt]", restored)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("content = %q, want %q", got, "original")
	}
}

func TestRestoreRemovesFileThatDidNotExist(t *testing.T) {
	ws, store := setupWorkspace(t)
	created := filepath.Join(ws, "brand-new.txt")

	if err := store.Capture("turn-1", created); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := os.WriteFile(created, []byte("new"), 0644); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := store.Restore("turn-1", ws); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("expected the created file to be removed, stat err = %v", err)
	}
}

// Capturing twice must keep the state from before the first write, otherwise
// undo would restore an intermediate edit instead of the original.
func TestCaptureIsIdempotentWithinTurn(t *testing.T) {
	ws, store := setupWorkspace(t)
	target := filepath.Join(ws, "target.txt")

	if err := store.Capture("turn-1", target); err != nil {
		t.Fatalf("first Capture: %v", err)
	}
	if err := os.WriteFile(target, []byte("first edit"), 0644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := store.Capture("turn-1", target); err != nil {
		t.Fatalf("second Capture: %v", err)
	}
	if err := os.WriteFile(target, []byte("second edit"), 0644); err != nil {
		t.Fatalf("edit: %v", err)
	}

	if _, err := store.Restore("turn-1", ws); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Fatalf("content = %q, want the pre-turn original %q", got, "original")
	}
}

func TestTurnsAreIsolated(t *testing.T) {
	ws, store := setupWorkspace(t)
	target := filepath.Join(ws, "target.txt")

	if err := store.Capture("turn-1", target); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := os.WriteFile(target, []byte("after turn 1"), 0644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := store.Capture("turn-2", target); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := os.WriteFile(target, []byte("after turn 2"), 0644); err != nil {
		t.Fatalf("edit: %v", err)
	}

	// Restoring turn 2 goes back one step, not all the way.
	if _, err := store.Restore("turn-2", ws); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "after turn 1" {
		t.Fatalf("content = %q, want %q", got, "after turn 1")
	}
}

func TestLastTurnReturnsNewest(t *testing.T) {
	ws, store := setupWorkspace(t)

	if turn, err := store.LastTurn(); err != nil || turn != "" {
		t.Fatalf("LastTurn on empty store = %q, %v; want empty", turn, err)
	}

	// Control the clock so ordering is deterministic.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	target := filepath.Join(ws, "target.txt")

	store.now = func() time.Time { return base }
	if err := store.Capture("older", target); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	store.now = func() time.Time { return base.Add(time.Minute) }
	if err := store.Capture("newer", target); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	turn, err := store.LastTurn()
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn != "newer" {
		t.Fatalf("LastTurn = %q, want %q", turn, "newer")
	}
}

func TestRestoreUnknownTurnIsNoop(t *testing.T) {
	ws, store := setupWorkspace(t)
	restored, err := store.Restore("never-existed", ws)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(restored) != 0 {
		t.Fatalf("restored = %v, want nothing", restored)
	}
}

func TestCaptureUnreadableFileReportsError(t *testing.T) {
	ws, store := setupWorkspace(t)
	dir := filepath.Join(ws, "a-directory")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Reading a directory fails for a reason other than NotExist.
	if err := store.Capture("turn-1", dir); err == nil {
		t.Fatal("expected an error capturing a directory")
	}
}

func TestSanitizeKeepsNamesFlat(t *testing.T) {
	for _, in := range []string{"../etc/passwd", "a/b", "..", ""} {
		got := sanitize(in)
		if strings.Contains(got, "/") || strings.Contains(got, "..") {
			t.Errorf("sanitize(%q) = %q, must not contain separators or ..", in, got)
		}
	}
}
