package core

import "testing"

// TestScopeCoversPath pins the prefix semantics of path grants. The "." case is
// a regression guard: the CLI writes "." when the user approves the whole
// workspace ("approve this folder"), and a bug once made that grant match
// nothing, so every later write asked again despite the user's consent.
func TestScopeCoversPath(t *testing.T) {
	cases := []struct {
		scope string
		path  string
		want  bool
	}{
		// Workspace root grants cover everything.
		{".", "a.go", true},
		{".", "internal/core/a.go", true},
		{"", "a.go", true},
		{"./", "a.go", true},
		// Exact and prefix matches.
		{"internal/core", "internal/core/a.go", true},
		{"internal/core", "internal/core", true},
		{"internal/core/", "internal/core/a.go", true},
		// Prefix must respect segment boundaries.
		{"docs", "docs-secret/a.go", false},
		// Unrelated branches do not match.
		{"internal/core", "internal/mcp/a.go", false},
		{"internal/core", "a.go", false},
	}
	for _, c := range cases {
		if got := scopeCoversPath(c.scope, c.path); got != c.want {
			t.Errorf("scopeCoversPath(%q, %q) = %v, want %v", c.scope, c.path, got, c.want)
		}
	}
}
