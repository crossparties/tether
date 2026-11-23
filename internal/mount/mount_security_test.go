package mount

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestIgnoreParserAndFallback(t *testing.T) {
	ws := t.TempDir()
	writeTestFiles(t, ws, map[string]string{
		".tetherignore":    "*.log\n!keep.log\nbuild/\n/rootonly.txt\nre:^secrets/.*\\.txt$\n",
		"app.log":          "log",
		"sub/app.log":      "log",
		"keep.log":         "keep",
		"build/out.js":     "js",
		"src/build/in.js":  "js",
		"rootonly.txt":     "root",
		"sub/rootonly.txt": "sub",
		"secrets/api.txt":  "sec",
		"src/main.go":      "package main\n",
	})

	// Fallback reads .tetherignore when .tether/ignore does not exist.
	m := NewEngine(ws)
	cases := []struct {
		path string
		want bool
	}{
		{"app.log", false},
		{"sub/app.log", false},
		{"keep.log", true},
		{"build/out.js", false},
		{"src/build/in.js", false},
		{"rootonly.txt", false},
		{"sub/rootonly.txt", true},
		{"secrets/api.txt", false},
		{"src/main.go", true},
	}
	for _, tc := range cases {
		d := m.AuthorizePath(tc.path, OpRead, "code")
		if d.Allowed != tc.want {
			t.Errorf("fallback AuthorizePath(%q) = %v (%s), want %v", tc.path, d.Allowed, d.Reason, tc.want)
		}
	}

	// Primary .tether/ignore overrides .tetherignore once created.
	if err := os.MkdirAll(filepath.Join(ws, ".tether"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("*.tmp\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if d := m.AuthorizePath("app.log", OpRead, "code"); !d.Allowed {
		t.Errorf("expected primary .tether/ignore to override .tetherignore; app.log denied: %s", d.Reason)
	}
	if d := m.AuthorizePath("temp.tmp", OpRead, "code"); d.Allowed {
		t.Errorf("expected *.tmp in .tether/ignore to block temp.tmp")
	}
}

func TestBuiltinSecretsNonRemovableAndPlanException(t *testing.T) {
	ws := t.TempDir()
	// Attempt to negate built-in secret rules via user .tether/ignore — must NEVER succeed.
	writeTestFiles(t, ws, map[string]string{
		".tether/ignore":         "!.env\n!*.pem\n!.git/config\n!.tether/session.db\n",
		".env":                   "SECRET=1\n",
		"config/.env.production": "SECRET=2\n",
		"keys/id_rsa":            "priv\n",
		"keys/id_ed25519":        "priv\n",
		"keys/id_ecdsa":          "priv\n",
		"tls/server.pem":         "pem\n",
		"tls/server.key":         "key\n",
		"tls/cert.p12":           "p12\n",
		"tls/cert.pfx":           "pfx\n",
		"vault.kdbx":             "kdbx\n",
		".npmrc":                 "npm\n",
		".pypirc":                "pypi\n",
		".netrc":                 "netrc\n",
		"credentials-prod.json":  "creds\n",
		"kubeconfig":             "kube\n",
		".git/config":            "[core]\n",
		".tether/session.db":     "db\n",
		".tether/ignore.bak":     "x\n",
		".tether/PLAN.md":        "# Plan\n",
		"docs/design.md":         "# Design\n",
		"src/app.go":             "package src\n",
	})

	m := NewEngine(ws)
	secrets := []string{
		".env",
		"config/.env.production",
		"keys/id_rsa",
		"keys/id_ed25519",
		"keys/id_ecdsa",
		"tls/server.pem",
		"tls/server.key",
		"tls/cert.p12",
		"tls/cert.pfx",
		"vault.kdbx",
		".npmrc",
		".pypirc",
		".netrc",
		"credentials-prod.json",
		"kubeconfig",
		".git/config",
		".tether/session.db",
		".tether/ignore",
		".tether/ignore.bak",
	}
	for _, s := range secrets {
		for _, op := range []Operation{OpRead, OpWrite} {
			d := m.AuthorizePath(s, op, "code")
			if d.Allowed || !d.IsBuiltin {
				t.Errorf("AuthorizePath(%q, %s) = (allowed=%v, builtin=%v), want (false, true)", s, op, d.Allowed, d.IsBuiltin)
			}
		}
	}

	// Explicit Planner exception ONLY for .tether/PLAN.md (real, non-symlinked).
	if d := m.AuthorizePath(".tether/PLAN.md", OpRead, "plan"); !d.Allowed {
		t.Errorf("expected .tether/PLAN.md OpRead allowed, got: %s", d.Reason)
	}
	if d := m.AuthorizePath(".tether/PLAN.md", OpWrite, "plan"); !d.Allowed {
		t.Errorf("expected .tether/PLAN.md OpWrite in plan mode allowed, got: %s", d.Reason)
	}
	if d := m.AuthorizePath(".tether/PLAN.md", OpWrite, "review"); d.Allowed {
		t.Errorf("expected .tether/PLAN.md OpWrite in review mode blocked")
	}
	if d := m.AuthorizePath("src/app.go", OpWrite, "plan"); d.Allowed {
		t.Errorf("expected code write in plan mode blocked")
	}
	if d := m.AuthorizePath("docs/design.md", OpWrite, "plan"); !d.Allowed {
		t.Errorf("expected docs/ write in plan mode allowed, got: %s", d.Reason)
	}
}

func TestSymlinkAndDanglingAncestorContainment(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	outsideSecret := filepath.Join(outside, "outside-secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("top-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	writeTestFiles(t, ws, map[string]string{
		".env":           "KEY=123\n",
		"sub/allowed.go": "package sub\n",
	})

	// 1. Symlink to existing outside file
	if err := os.Symlink(outsideSecret, filepath.Join(ws, "escape.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// 2. Dangling symlink directory pointing outside workspace (foo/evil-link/secret)
	_ = os.MkdirAll(filepath.Join(ws, "foo"), 0755)
	_ = os.Symlink(filepath.Join(outside, "nonexistent-dir"), filepath.Join(ws, "foo", "evil-link"))
	// 3. Symlink pointing inside workspace to .env
	_ = os.Symlink(filepath.Join(ws, ".env"), filepath.Join(ws, "sub", "env-link.txt"))
	// 4. Symlinked .tether/PLAN.md pointing to .env must STILL be blocked!
	_ = os.MkdirAll(filepath.Join(ws, ".tether"), 0755)
	_ = os.Symlink(filepath.Join(ws, ".env"), filepath.Join(ws, ".tether", "PLAN.md"))
	// 5. Symlink loop
	_ = os.Symlink(filepath.Join(ws, "loop_b"), filepath.Join(ws, "loop_a"))
	_ = os.Symlink(filepath.Join(ws, "loop_a"), filepath.Join(ws, "loop_b"))

	m := NewEngine(ws)
	blocked := []string{
		"../outside-secret.txt",
		outsideSecret,
		"escape.txt",
		"foo/evil-link/secret",
		"sub/env-link.txt",
		".tether/PLAN.md",
		"loop_a",
	}
	for _, p := range blocked {
		for _, op := range []Operation{OpRead, OpWrite} {
			d := m.AuthorizePath(p, op, "code")
			if d.Allowed {
				t.Errorf("AuthorizePath(%q, %s) allowed unexpectedly (canonical=%q)", p, op, d.CanonicalPath)
			}
		}
	}

	// Distinguish secret/ignore blocks from missing normal files.
	missingDec := m.AuthorizePath("sub/does-not-exist.go", OpRead, "code")
	if !missingDec.Allowed || !missingDec.IsMissing {
		t.Errorf("expected missing normal file to have Allowed=true, IsMissing=true, got %+v", missingDec)
	}
	envDec := m.AuthorizePath(".env", OpRead, "code")
	if envDec.Allowed || !envDec.IsBuiltin || envDec.MatchedPattern != ".env" {
		t.Errorf("expected .env to report builtin block, got %+v", envDec)
	}
}

func TestMountedFilesAndIgnoreRulesDBSync(t *testing.T) {
	ws := t.TempDir()
	writeTestFiles(t, ws, map[string]string{
		"docs/frozen.md":  "frozen\n",
		"src/critical.go": "package src\n",
		"src/hidden.go":   "package src\n",
		"src/normal.go":   "package src\n",
	})

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE mounted_files (
			rel_path TEXT PRIMARY KEY,
			access_mode TEXT NOT NULL DEFAULT 'guarded',
			source TEXT NOT NULL DEFAULT 'rule',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE mount_ignore_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pattern TEXT NOT NULL,
			is_regex INTEGER NOT NULL DEFAULT 0,
			is_builtin INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		t.Fatal(err)
	}

	m := NewEngine(ws)
	m.BindDB(db, db)

	if err := m.SetMountedMode("docs/frozen.md", AccessRO, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetMountedMode("src/critical.go", AccessGuarded, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetMountedMode("src/hidden.go", AccessIgnored, "manual"); err != nil {
		t.Fatal(err)
	}

	if d := m.AuthorizePath("docs/frozen.md", OpRead, "code"); !d.Allowed || d.AccessMode != AccessRO {
		t.Errorf("ro read mismatch: %+v", d)
	}
	if d := m.AuthorizePath("docs/frozen.md", OpWrite, "code"); d.Allowed || d.AccessMode != AccessRO {
		t.Errorf("ro write should be blocked: %+v", d)
	}
	if d := m.AuthorizePath("src/critical.go", OpWrite, "code"); !d.Allowed || d.AccessMode != AccessGuarded {
		t.Errorf("guarded write mismatch: %+v", d)
	}
	if d := m.AuthorizePath("src/hidden.go", OpRead, "code"); d.Allowed || d.AccessMode != AccessIgnored {
		t.Errorf("ignored read should be blocked: %+v", d)
	}

	rule, _ := ParseRule("*.bak")
	if err := m.SaveCustomRules([]Rule{rule}); err != nil {
		t.Fatal(err)
	}
	dbRules, err := LoadRulesFromDB(db)
	if err != nil {
		t.Fatal(err)
	}
	foundCustom := false
	for _, r := range dbRules {
		if !r.IsBuiltin && r.Pattern == "*.bak" {
			foundCustom = true
		}
	}
	if !foundCustom {
		t.Errorf("expected *.bak synced into mount_ignore_rules, got %+v", dbRules)
	}
}

func TestImpactSimulatorAndRepoMapHookAndGitignoreConversion(t *testing.T) {
	ws := t.TempDir()
	writeTestFiles(t, ws, map[string]string{
		"main.go":         "package main\n",
		"web/App.svelte":  "<script></script>\n",
		"web/utils.ts":    "export const x = 1;\n",
		"scripts/run.py":  "print(1)\n",
		"rust/src/lib.rs": "pub fn f() {}\n",
		"README.md":       "# Docs\n",
		"docs/arch.md":    "# Arch\n",
		".env":            "SECRET=1\n",
		".gitignore":      ".tether/session.db*\n.env\ndist/\nnode_modules/\n*.go\n",
	})

	sim, err := SimulateRule(ws, "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if sim.MatchedCount != 1 || sim.ByExtension[".go"] != 1 || len(sim.Warnings) == 0 {
		t.Fatalf("expected warning for *.go source impact, got %+v", sim)
	}

	// Simulate multi-extension rule hitting .ts, .svelte, .py, .rs
	rules, errs := ParseIgnoreContent("web/**\nscripts/*.py\nrust/**\nre:[invalid\n")
	if len(errs) != 1 {
		t.Fatalf("expected 1 invalid regex error, got %v", errs)
	}
	simAll, err := SimulateIgnoreRules(ws, rules)
	if err != nil {
		t.Fatal(err)
	}
	if len(simAll.InvalidPatterns) != 1 {
		t.Errorf("expected 1 invalid pattern flagged, got %v", simAll.InvalidPatterns)
	}
	for _, ext := range []string{".svelte", ".ts", ".py", ".rs"} {
		if simAll.ByExtension[ext] != 1 {
			t.Errorf("expected extension %s count 1, got %+v", ext, simAll.ByExtension)
		}
	}

	// Gitignore conversion keeps dist/ and node_modules/, drops .tether*, .env, and *.go source pattern
	converted, skipped, err := ConvertGitignoreToTetherIgnore(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(converted, "dist/") || !strings.Contains(converted, "node_modules/") {
		t.Errorf("expected dist/ and node_modules/ in converted output:\n%s", converted)
	}
	if strings.Contains(converted, "*.go") || strings.Contains(converted, ".tether") {
		t.Errorf("converted output should exclude *.go and .tether*:\n%s", converted)
	}
	if len(skipped) == 0 {
		t.Errorf("expected *.go to be reported in skipped")
	}

	// ShouldIndexRepoMap filters out *.md docs and secrets while keeping code files.
	m := NewEngine(ws)
	if !m.ShouldIndexRepoMap("main.go") || !m.ShouldIndexRepoMap("web/App.svelte") {
		t.Errorf("expected Go and Svelte source files eligible for RepoMap")
	}
	if m.ShouldIndexRepoMap("README.md") || m.ShouldIndexRepoMap("docs/arch.md") || m.ShouldIndexRepoMap(".env") {
		t.Errorf("expected markdown docs and .env excluded from RepoMap")
	}
}

func TestHardenedSecurityInvariantsAndParseErrors(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	outsideDir := filepath.Join(outside, "dir")
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFiles(t, ws, map[string]string{
		".env":            "SECRET=1\n",
		".tetherignore":   "!.env\n",
		".gitignore":      "dist/\n",
		".tether/PLAN.md": "# Plan\n",
		"build/out.txt":   "x\n",
		"build/keep.txt":  "y\n",
		"dist/a.txt":      "a\n",
	})
	// Non-directory .git file (git worktree style)
	if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: ../repo/.git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Dangling relative symlink inside symlinked directory: /ws/sub -> /outside/dir, dangling -> ../escape.txt
	if err := os.Symlink(outsideDir, filepath.Join(ws, "sub")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink("../escape.txt", filepath.Join(outsideDir, "dangling")); err != nil {
		t.Fatal(err)
	}

	m := NewEngine(ws)

	// 1. Dangling relative symlink inside symlinked dir MUST be blocked.
	if d := m.AuthorizePath("sub/dangling", OpRead, "code"); d.Allowed {
		t.Errorf("expected dangling relative symlink in symlinked parent blocked, got %+v", d)
	}
	if d := m.AuthorizePath("sub/dangling/leaf", OpWrite, "code"); d.Allowed {
		t.Errorf("expected child of dangling relative symlink blocked, got %+v", d)
	}

	// 2. !.env in .tetherignore must NEVER bypass built-in secret rules.
	for _, op := range []Operation{OpRead, OpWrite} {
		if d := m.AuthorizePath(".env", op, "code"); d.Allowed {
			t.Errorf("expected !.env negation NOT to bypass built-in rule (%s), got %+v", op, d)
		}
	}

	// 3. .git (regular file), .ssh (nonexistent), and .tether/* internals must stay blocked.
	for _, target := range []string{".git", ".ssh", ".ssh/id_rsa", ".tether/ignore", ".tether/mcp.json", ".tether/session.db"} {
		for _, op := range []Operation{OpRead, OpWrite} {
			if d := m.AuthorizePath(target, op, "code"); d.Allowed {
				t.Errorf("expected %q (%s) blocked, got %+v", target, op, d)
			}
		}
	}

	// 4. .tether/PLAN.md writable ONLY in plan mode; readable in code/plan/review.
	if d := m.AuthorizePath(".tether/PLAN.md", OpRead, "code"); !d.Allowed {
		t.Errorf("expected .tether/PLAN.md readable in code mode: %s", d.Reason)
	}
	if d := m.AuthorizePath(".tether/PLAN.md", OpWrite, "code"); d.Allowed {
		t.Errorf("expected .tether/PLAN.md OpWrite blocked in code mode")
	}
	if d := m.AuthorizePath(".tether/PLAN.md", OpWrite, "plan"); !d.Allowed {
		t.Errorf("expected .tether/PLAN.md OpWrite allowed in plan mode: %s", d.Reason)
	}

	// 5. .tetherignore and .gitignore are readable but AccessGuarded on write.
	for _, cfg := range []string{".tetherignore", ".gitignore"} {
		if d := m.AuthorizePath(cfg, OpRead, "code"); !d.Allowed {
			t.Errorf("expected %s OpRead allowed: %s", cfg, d.Reason)
		}
		if d := m.AuthorizePath(cfg, OpWrite, "code"); !d.Allowed || d.AccessMode != AccessGuarded {
			t.Errorf("expected %s OpWrite to be AccessGuarded, got %+v", cfg, d)
		}
		if d := m.AuthorizePath(cfg, OpWrite, "plan"); d.Allowed {
			t.Errorf("expected %s OpWrite denied in plan mode", cfg)
		}
	}

	// 6. Ordered ShouldSkipDir handles unrelated negations without disabling skips.
	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("build/\n!dist/keep.txt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if !m.ShouldSkipDir("build") {
		t.Errorf("expected ShouldSkipDir(build) = true when negation targets unrelated dist/")
	}
	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("build/\n!build/keep.txt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if m.ShouldSkipDir("build") {
		t.Errorf("expected ShouldSkipDir(build) = false when !build/keep.txt negates child")
	}
	if d := m.AuthorizePath("build/out.txt", OpRead, "code"); d.Allowed {
		t.Errorf("expected build/out.txt denied")
	}
	if d := m.AuthorizePath("build/keep.txt", OpRead, "code"); !d.Allowed {
		t.Errorf("expected build/keep.txt allowed via negation: %s", d.Reason)
	}

	// 7. Invalid re: rule surfaces error in Reload/ParseErrors without disabling valid rules,
	// and SaveIgnoreRules rejects invalid syntax before mutating disk.
	ignorePath := filepath.Join(ws, ".tether", "ignore")
	if err := os.WriteFile(ignorePath, []byte("secret_local.txt\nre:[broken\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err == nil {
		t.Errorf("expected Reload() error for invalid re: rule")
	}
	if errs := m.ParseErrors(); len(errs) != 1 {
		t.Errorf("expected 1 ParseError, got %v", errs)
	}
	if d := m.AuthorizePath("secret_local.txt", OpRead, "code"); d.Allowed {
		t.Errorf("expected valid rule secret_local.txt to remain active despite invalid sibling line")
	}
	if err := SaveIgnoreRules(ws, nil, []Rule{{Pattern: "re:[invalid", Enabled: true}}); err == nil {
		t.Errorf("expected SaveIgnoreRules to reject invalid regex rule")
	}
	if raw, _ := os.ReadFile(ignorePath); !strings.Contains(string(raw), "secret_local.txt") {
		t.Errorf("expected failed SaveIgnoreRules not to clobber existing ignore file")
	}

	// 8. LoadRulesFromDB honors is_regex flag even when stored pattern omits re: prefix.
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE mount_ignore_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pattern TEXT NOT NULL,
			is_regex INTEGER NOT NULL DEFAULT 0,
			is_builtin INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO mount_ignore_rules (pattern, is_regex, is_builtin, enabled) VALUES ('^token_.*\.txt$', 1, 0, 1);
	`); err != nil {
		t.Fatal(err)
	}
	dbRules, err := LoadRulesFromDB(db)
	if err != nil || len(dbRules) != 1 || !dbRules[0].IsRegex || !dbRules[0].Matches("token_prod.txt", false) {
		t.Fatalf("LoadRulesFromDB regex compilation failed: rules=%+v err=%v", dbRules, err)
	}
}

func writeTestFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}
