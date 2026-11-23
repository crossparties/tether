package harness

// Tripwire for the command registry.
//
// The registry exists because commands used to live as `case "/x":` branches in
// the REPL switch. That shape had no enumeration, so the "/" menu could not be
// built and the startup banner drifted (it omitted /quit, /clear and /undo).
//
// A `case` branch reintroduced into the loop would silently bypass the registry:
// the command would still work, but it would vanish from /help, from the menu,
// and from the banner. That is a shape regression, not a behaviour regression,
// so no behavioural test would catch it. This one does, by reading the source.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestREPLLoopHasNoHardcodedCommandSwitch fails if runner.go grows a switch on
// slash commands again. The check is deliberately narrow: it looks for a
// switch inside RunInteractive whose case labels are slash-prefixed strings.
func TestREPLLoopHasNoHardcodedCommandSwitch(t *testing.T) {
	const path = "runner.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var violations []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RunInteractive" {
			return true
		}
		ast.Inspect(fn, func(inner ast.Node) bool {
			sw, ok := inner.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			for _, stmt := range sw.Body.List {
				clause, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range clause.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					if strings.HasPrefix(val, "/") {
						violations = append(violations, val)
					}
				}
			}
			return true
		})
		return true
	})

	if len(violations) > 0 {
		t.Fatalf("RunInteractive switches on hardcoded slash commands %v; add them to the registry in commands.go instead, or they disappear from /help, the / menu and the banner", violations)
	}
}
