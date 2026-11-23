package core

// Regression test for a traversal leak in scopeCoversPath.
//
// The scope was cleaned but the candidate path was not, so a relative segment
// matched by prefix: scope "internal/hooks" appeared to cover
// "internal/hooks/../core/x.go", a path that resolves into a different package
// the grant never covered. Path grants and handoff boundaries both use this
// function, so the leak widened both at once.

import "testing"

func TestScopeCoversPathRejectsTraversalOut(t *testing.T) {
	cases := []struct {
		scope string
		path  string
		want  bool
	}{
		// The leak, in its original form.
		{"internal/hooks", "internal/hooks/../core/x.go", false},
		{"internal/hooks", "internal/hooks/../../etc/passwd", false},
		{"internal/hooks", "internal/hooks/./handler.go", true},
		{"internal/hooks/", "internal/hooks/handler.go", true},
		{"internal/hooks", "internal/hooks/nested/deep.go", true},
		// A scope that itself carries a traversal must still be interpreted
		// consistently on both sides.
		{"internal/hooks/../core", "internal/core/engine.go", true},
		{"internal/hooks/../core", "internal/hooks/handler.go", false},
	}
	for _, tc := range cases {
		if got := scopeCoversPath(tc.scope, tc.path); got != tc.want {
			t.Errorf("scopeCoversPath(%q, %q) = %v, want %v", tc.scope, tc.path, got, tc.want)
		}
	}
}
