package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPostHashRestoreFileAndCrewConflictProtection(t *testing.T) {
	ws, store := setupWorkspace(t)
	target := filepath.Join(ws, "target.txt")
	second := filepath.Join(ws, "second.txt")

	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }

	// Turn 1 by crew-1: modifies existing target.txt ("original" -> "crew-v1")
	if err := store.CaptureForCrew("turn-c1", "crew-1", target); err != nil {
		t.Fatalf("CaptureForCrew turn-c1: %v", err)
	}
	if err := os.WriteFile(target, []byte("crew-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPostHash("turn-c1", target); err != nil {
		t.Fatalf("RecordPostHash turn-c1: %v", err)
	}

	// Turn 2 by crew-1: modifies target.txt again ("crew-v1" -> "crew-v2") and creates second.txt.
	store.now = func() time.Time { return base.Add(time.Minute) }
	if err := store.CaptureForCrew("turn-c2", "crew-1", target); err != nil {
		t.Fatalf("CaptureForCrew turn-c2 target: %v", err)
	}
	if err := store.CaptureForCrew("turn-c2", "crew-1", second); err != nil {
		t.Fatalf("CaptureForCrew turn-c2 second: %v", err)
	}
	if err := os.WriteFile(target, []byte("crew-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("created-by-crew"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPostHash("turn-c2", target); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPostHash("turn-c2", second); err != nil {
		t.Fatal(err)
	}

	// Manual user edit to target.txt after crew finished!
	if err := os.WriteFile(target, []byte("manual-user-edit"), 0o644); err != nil {
		t.Fatal(err)
	}

	// RestoreFile without force MUST detect external modification and refuse overwrite.
	res, err := store.RestoreFile("turn-c2", "target.txt", ws, false)
	if !errors.Is(err, ErrExternalModification) || !res.Conflict || res.Restored {
		t.Fatalf("expected ErrExternalModification conflict, got res=%+v err=%v", res, err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "manual-user-edit" {
		t.Fatalf("manual edit was overwritten despite force=false: %q", string(got))
	}

	// RestoreCrew without force MUST refuse target.txt while restoring clean second.txt.
	crewRes, err := store.RestoreCrew("crew-1", ws, false)
	if !errors.Is(err, ErrExternalModification) {
		t.Fatalf("expected ErrExternalModification on RestoreCrew, got err=%v res=%+v", err, crewRes)
	}
	got, _ = os.ReadFile(target)
	if string(got) != "manual-user-edit" {
		t.Fatalf("target.txt manual edit overwritten during RestoreCrew(force=false): %q", string(got))
	}
	if _, statErr := os.Stat(second); !os.IsNotExist(statErr) {
		t.Fatalf("expected unmodified created file second.txt to be removed, statErr=%v", statErr)
	}

	// RestoreCrew with force=true restores target.txt all the way back to pre-crew "original".
	crewResForce, err := store.RestoreCrew("crew-1", ws, true)
	if err != nil {
		t.Fatalf("RestoreCrew(force=true): %v res=%+v", err, crewResForce)
	}
	got, _ = os.ReadFile(target)
	if string(got) != "original" {
		t.Fatalf("expected pre-crew original restored on force=true, got %q", string(got))
	}
}

func TestSnapshotRestoreBlocksSymlinkAndTraversalEscapes(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("untouched-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := New(ws)

	// Craft a turn index attempting:
	// 1) relative .. target escape
	// 2) symlinked parent directory inside workspace -> outside
	// 3) symlinked file target inside workspace -> outside
	// 4) .. backup path escape
	symDir := filepath.Join(ws, "linkeddir")
	if err := os.Symlink(outside, symDir); err != nil {
		t.Fatalf("Symlink dir: %v", err)
	}
	symFile := filepath.Join(ws, "linkedfile.txt")
	if err := os.Symlink(secretFile, symFile); err != nil {
		t.Fatalf("Symlink file: %v", err)
	}

	bakPath := filepath.Join(store.dir("evil-turn"), "ok.bak")
	if err := os.MkdirAll(filepath.Dir(bakPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bakPath, []byte("overwritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.save("evil-turn", &Index{
		Turn:    "evil-turn",
		Created: time.Now(),
		Entries: []Entry{
			{Path: "../secret.txt", State: Modified, Backup: "evil-turn/ok.bak"},
			{Path: "linkeddir/secret.txt", State: Modified, Backup: "evil-turn/ok.bak"},
			{Path: "linkedfile.txt", State: Modified, Backup: "evil-turn/ok.bak"},
			{Path: "safe.txt", State: Modified, Backup: "../../" + filepath.Base(outside) + "/secret.txt"},
			{Path: "linkedfile.txt", State: Created},
		},
	}); err != nil {
		t.Fatal(err)
	}

	restored, err := store.Restore("evil-turn", ws)
	if err == nil {
		t.Fatalf("expected symlink/traversal restore to fail, got restored=%v", restored)
	}
	if len(restored) != 0 {
		t.Fatalf("expected zero restored files on symlink/traversal failures, got %v", restored)
	}
	got, _ := os.ReadFile(secretFile)
	if string(got) != "untouched-secret" {
		t.Fatalf("outside secret was modified: %q", string(got))
	}
	if _, lerr := os.Lstat(symFile); lerr != nil {
		t.Fatalf("symlink file should not have been removed by Created restore: %v", lerr)
	}
}
