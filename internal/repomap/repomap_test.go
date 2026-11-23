package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crossparties/tether/internal/core"
)

func TestRepoMapExtractionCachingSecurityAndDelta(t *testing.T) {
	ws := t.TempDir()

	files := map[string]string{
		"main.go": strings.Join([]string{
			"package main",
			"",
			"type AppConfig struct {",
			"\tPort int",
			"}",
			"",
			"func RunServer(cfg AppConfig) error {",
			"\treturn nil",
			"}",
		}, "\n") + "\n",
		"web/App.svelte": `<script lang="ts">
export interface UserItem {
	id: string;
}
export function loadUser(id: string): UserItem {
	return { id };
}
const formatName = (u: UserItem) => u.id;
</script>
<h1>{loadUser("1").id}</h1>
`,
		"web/api.ts": `export interface ApiResult { ok: boolean; }
export async function fetchStatus(url: string) { return { ok: true }; }
export const buildEndpoint = (base: string) => base + "/status";
`,
		"scripts/check.py": `class HealthChecker:
    def run_probe(self, host: str) -> bool:
        # def fake_commented_out(): pass
        return True

async def fetch_metrics(endpoint: str):
    return {}

DEFAULT_PORT = 8080
`,
		"rust/lib.rs": `pub struct EngineState { pub ready: bool }
pub fn boot_engine(port: u16) -> bool { true }
`,
		"README.md":       "# Docs\nfunc FakeMarkdownSymbol() {}\n",
		".env":            "SECRET_KEY=leaked_value_999\n",
		"secret_code.go":  "package main\nfunc HiddenIgnoredFunc() {}\n",
		"extra/helper.go": "package extra\nfunc ExtraFocusOnly() {}\n",
	}

	for rel, body := range files {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("secret_code.go\n"), 0o644); err != nil {
		// .tether dir might not exist before NewEngine; write after NewEngine below if needed.
	}

	// Symlink escape outside workspace must never appear in RepoMap.
	outsideFile := filepath.Join(t.TempDir(), "outside.go")
	_ = os.WriteFile(outsideFile, []byte("package evil\nfunc OutsideEscapeFunc() {}\n"), 0o644)
	_ = os.Symlink(outsideFile, filepath.Join(ws, "escape_link.go"))

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("secret_code.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	builder := NewBuilder(ws, eng.CacheDB(), eng.Mount())

	// 1. Initial Refresh parses all 6 allowed source files (excluding README.md, .env, secret_code.go, escape_link.go).
	stats1, err := builder.Refresh()
	if err != nil {
		t.Fatalf("Refresh #1: %v", err)
	}
	if stats1.TotalFiles != 6 || stats1.ParsedFiles != 6 || stats1.CacheHits != 0 {
		t.Fatalf("unexpected initial refresh stats: %+v", stats1)
	}

	// 2. Second Refresh with unchanged mtimes MUST hit cache 100% (0 re-parses).
	stats2, err := builder.Refresh()
	if err != nil {
		t.Fatalf("Refresh #2: %v", err)
	}
	if stats2.CacheHits != 6 || stats2.ParsedFiles != 0 {
		t.Fatalf("expected 6 cache hits and 0 re-parses on unchanged files, got %+v", stats2)
	}

	// 3. Touch one file with a newer mtime -> only that 1 file re-parses.
	helperPath := filepath.Join(ws, "extra", "helper.go")
	if err := os.WriteFile(helperPath, []byte("package extra\nfunc ExtraFocusOnly() {}\nfunc ExtraSecondSymbol() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(helperPath, future, future)

	stats3, err := builder.Refresh()
	if err != nil {
		t.Fatalf("Refresh #3: %v", err)
	}
	if stats3.CacheHits != 5 || stats3.ParsedFiles != 1 {
		t.Fatalf("expected 5 cache hits + 1 re-parse after modifying helper.go, got %+v", stats3)
	}

	// 4. Verify baseline extraction across Go, Svelte, TS, Python, Rust and exclusion of docs/secrets/ignore.
	baseline, covered := builder.BuildBaselineDetailed(1024)
	if baseline == "" {
		t.Fatal("expected non-empty baseline RepoMap")
	}
	if tok := EstimateTokens(baseline); tok > 1024 {
		t.Fatalf("baseline exceeded 1024 token budget: got %d", tok)
	}
	for _, wantSym := range []string{"AppConfig", "RunServer", "loadUser", "fetchStatus", "HealthChecker", "run_probe", "boot_engine"} {
		if !strings.Contains(baseline, wantSym) {
			t.Errorf("baseline RepoMap missing expected symbol %q:\n%s", wantSym, baseline)
		}
	}
	for _, forbidden := range []string{"FakeMarkdownSymbol", "SECRET_KEY", "HiddenIgnoredFunc", "OutsideEscapeFunc", "README.md", ".env"} {
		if strings.Contains(baseline, forbidden) {
			t.Errorf("baseline RepoMap leaked excluded symbol/file %q:\n%s", forbidden, baseline)
		}
	}

	// Determinism check: identical baseline across repeated invocations.
	baselineAgain := builder.BuildBaseline(1024)
	if baseline != baselineAgain {
		t.Fatalf("BuildBaseline is not deterministic across runs")
	}

	// 5. BuildDelta skips files already covered in baseline, and includes uncovered focus files.
	if deltaCovered := builder.BuildDelta([]string{"main.go"}, covered, 380); deltaCovered != "" {
		t.Fatalf("expected empty delta for file already covered in baseline, got:\n%s", deltaCovered)
	}
	// Simulate main.go covered in baseline while extra/helper.go was excluded from baseline.
	smallCovered := map[string]bool{"main.go": true}
	delta := builder.BuildDelta([]string{"extra/helper.go"}, smallCovered, 380)
	if !strings.Contains(delta, "ExtraFocusOnly") || !strings.Contains(delta, "ExtraSecondSymbol") {
		t.Fatalf("expected BuildDelta to include uncovered focus file symbols, got:\n%s", delta)
	}
	if strings.Contains(delta, "RunServer") {
		t.Fatalf("BuildDelta should exclude baseline-covered files:\n%s", delta)
	}

	// 6. Tightening .tether/ignore purges repomap_tags from cache.db automatically.
	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("secret_code.go\nextra/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stats4, err := builder.Refresh()
	if err != nil {
		t.Fatalf("Refresh #4: %v", err)
	}
	if stats4.PurgedFiles != 1 {
		t.Fatalf("expected 1 purged row after ignoring extra/, got %+v", stats4)
	}
	var count int
	if err := eng.CacheDB().QueryRow(`SELECT COUNT(*) FROM repomap_tags WHERE file_path = 'extra/helper.go'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expected extra/helper.go deleted from repomap_tags, count=%d err=%v", count, err)
	}
}
