package core

import "testing"

// TestScopeCoversPathNormalisesBothSides guards the normalisation asymmetry
// between AddApprovalGrant (which stores the scope verbatim) and
// HasApprovalGrant (which cleans the path it tests).
func TestScopeCoversPathNormalisesBothSides(t *testing.T) {
	// A scope with a trailing slash and a redundant segment must still match
	// the cleaned rendering of the same path.
	cases := []struct{ scope, p string }{
		{"internal//core/", "internal/core/a.go"},
		{"./internal/core", "internal/core/a.go"},
		{"internal/core/./", "internal/core/a.go"},
	}
	for _, c := range cases {
		if !scopeCoversPath(c.scope, c.p) {
			t.Errorf("scopeCoversPath(%q, %q) = false, want true", c.scope, c.p)
		}
	}
}
