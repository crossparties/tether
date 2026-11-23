package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	files := map[string]string{
		"cmd/tether/main.go":        "package main\nfunc main() {}\n",
		"internal/ui/renderer.go":   "package ui\n",
		"internal/ui/picker.go":     "package ui\n",
		"internal/safety/policy.go": "package safety\n",
		"README.md":                 "# Tether\n",
		"Makefile":                  "build:\n\tgo build ./...\n",
		".git/config":               "[core]\n",
		"node_modules/pkg/index.js": "module.exports = {}\n",
		"vendor/mod/mod.go":         "package mod\n",
		".bin/tether":               "binary-stub",
		".tether/snapshots/turn1":   "snapshot",
		".tether/session.db":        "sqlite-tether-main",
		".tether/cache.db":          "sqlite-tether-cache",
		"session.db":                "sqlite-main",
		"session.db-wal":            "sqlite-wal",
		"session.db-shm":            "sqlite-shm",
	}

	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

func TestIndexerScanIgnoresExcludedPaths(t *testing.T) {
	root := setupTestWorkspace(t)
	idx := NewIndexer(root)

	entries, err := idx.Scan()
	if err != nil {
		t.Fatalf("Scan() unexpected error: %v", err)
	}

	seen := make(map[string]FileEntry, len(entries))
	for _, e := range entries {
		seen[e.RelPath] = e
	}

	wantPresent := []string{
		"Makefile",
		"README.md",
		"cmd/tether/main.go",
		"internal/safety/policy.go",
		"internal/ui/picker.go",
		"internal/ui/renderer.go",
	}
	for _, want := range wantPresent {
		e, ok := seen[want]
		if !ok {
			t.Errorf("expected %q in scanned entries, but it was missing", want)
			continue
		}
		if e.SizeHuman == "" {
			t.Errorf("expected non-empty SizeHuman for %q", want)
		}
		if e.Ext == "" {
			t.Errorf("expected non-empty Ext for %q", want)
		}
	}

	wantIgnored := []string{
		".git/config",
		"node_modules/pkg/index.js",
		"vendor/mod/mod.go",
		".bin/tether",
		".tether/snapshots/turn1",
		".tether/session.db",
		".tether/cache.db",
		"session.db",
		"session.db-wal",
		"session.db-shm",
	}
	for _, ignored := range wantIgnored {
		if _, ok := seen[ignored]; ok {
			t.Errorf("expected %q to be ignored by Scan(), but it was indexed", ignored)
		}
	}
}

func TestFuzzyFilterRankingAndLimit(t *testing.T) {
	root := setupTestWorkspace(t)
	idx := NewIndexer(root)

	allTop3 := idx.FuzzyFilter("", 3)
	if len(allTop3) != 3 {
		t.Fatalf("expected 3 entries for empty query with limit 3, got %d", len(allTop3))
	}

	mainMatches := idx.FuzzyFilter("main.go", 5)
	if len(mainMatches) == 0 {
		t.Fatal("expected match for 'main.go', got 0")
	}
	if mainMatches[0].RelPath != "cmd/tether/main.go" {
		t.Errorf("top match for 'main.go' = %q, want 'cmd/tether/main.go'", mainMatches[0].RelPath)
	}

	fuzzyMatches := idx.FuzzyFilter("iup", 5)
	foundPicker := false
	for _, m := range fuzzyMatches {
		if m.RelPath == "internal/ui/picker.go" {
			foundPicker = true
			break
		}
	}
	if !foundPicker {
		t.Errorf("expected 'internal/ui/picker.go' in fuzzy results for 'iup', got %+v", fuzzyMatches)
	}
}

func TestExtractAtTokenAndReplaceAtToken(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		cursor    int
		wantQuery string
		wantStart int
		wantEnd   int
		wantAct   bool
	}{
		{
			name:      "bare at sign at start",
			input:     "@",
			cursor:    1,
			wantQuery: "",
			wantStart: 0,
			wantEnd:   1,
			wantAct:   true,
		},
		{
			name:      "mid prompt at token",
			input:     "tolong cek @internal/ui sekarang",
			cursor:    len("tolong cek @internal/ui"),
			wantQuery: "internal/ui",
			wantStart: 11,
			wantEnd:   23,
			wantAct:   true,
		},
		{
			name:      "ordinary word without at",
			input:     "tolong cek internal/ui",
			cursor:    len("tolong cek internal/ui"),
			wantQuery: "",
			wantStart: 0,
			wantEnd:   0,
			wantAct:   false,
		},
		{
			name:      "email address is not an at token",
			input:     "contact dev@example.com",
			cursor:    len("contact dev@example.com"),
			wantQuery: "",
			wantStart: 0,
			wantEnd:   0,
			wantAct:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, s, e, act := ExtractAtToken(tc.input, tc.cursor)
			if act != tc.wantAct || q != tc.wantQuery || s != tc.wantStart || e != tc.wantEnd {
				t.Fatalf("ExtractAtToken(%q, %d) = (%q, %d, %d, %v), want (%q, %d, %d, %v)",
					tc.input, tc.cursor, q, s, e, act, tc.wantQuery, tc.wantStart, tc.wantEnd, tc.wantAct)
			}
		})
	}

	orig := "jelaskan fungsi di @main"
	_, s, e, act := ExtractAtToken(orig, len(orig))
	if !act {
		t.Fatal("expected active @ token")
	}
	replaced, newCursor := ReplaceAtToken(orig, s, e, "cmd/tether/main.go")
	wantReplaced := "jelaskan fungsi di @cmd/tether/main.go "
	if replaced != wantReplaced {
		t.Errorf("ReplaceAtToken = %q, want %q", replaced, wantReplaced)
	}
	if newCursor != len(wantReplaced) {
		t.Errorf("newCursor = %d, want %d", newCursor, len(wantReplaced))
	}
}

func TestSafeReadWorkspaceFileSecurityBoundaries(t *testing.T) {
	root := setupTestWorkspace(t)

	content, cleanRel, err := SafeReadWorkspaceFile(root, "cmd/tether/main.go", 4096)
	if err != nil {
		t.Fatalf("SafeReadWorkspaceFile valid file failed: %v", err)
	}
	if cleanRel != "cmd/tether/main.go" {
		t.Errorf("cleanRel = %q, want 'cmd/tether/main.go'", cleanRel)
	}
	if !strings.Contains(content, "package main") {
		t.Errorf("unexpected content: %q", content)
	}

	if _, _, err := SafeReadWorkspaceFile(root, "../../../etc/passwd", 4096); err == nil {
		t.Error("expected error for ../ path traversal outside workspace, got nil")
	}

	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	symlinkPath := filepath.Join(root, "leak.txt")
	if err := os.Symlink(outsideSecret, symlinkPath); err == nil {
		if _, _, err := SafeReadWorkspaceFile(root, "leak.txt", 4096); err == nil {
			t.Error("expected error when reading symlink pointing outside workspace, got nil")
		}
	}

	if _, _, err := SafeReadWorkspaceFile(root, "cmd/tether/main.go", 5); err == nil {
		t.Error("expected error when file exceeds maxBytes=5, got nil")
	}

	if _, _, err := SafeReadWorkspaceFile(root, "session.db", 4096); err == nil {
		t.Error("expected error when attempting to read legacy session.db, got nil")
	}
	if _, _, err := SafeReadWorkspaceFile(root, ".tether/session.db", 4096); err == nil {
		t.Error("expected error when attempting to read .tether/session.db, got nil")
	}
	if _, _, err := SafeReadWorkspaceFile(root, ".tether/cache.db", 4096); err == nil {
		t.Error("expected error when attempting to read .tether/cache.db, got nil")
	}
}
