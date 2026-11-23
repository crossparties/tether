package safety

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApprovalMenuLabels(t *testing.T) {
	labels := ApprovalMenuLabels()
	want := []string{
		"[Setujui & Jalankan]",
		"[Lihat Diff Lengkap]",
		"[Tolak Aksi Ini]",
		"[Batalkan Seluruh Task]",
	}
	if len(labels) != len(want) {
		t.Fatalf("len(ApprovalMenuLabels()) = %d, want %d", len(labels), len(want))
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("ApprovalMenuLabels()[%d] = %q, want %q", i, labels[i], want[i])
		}
	}

	if ChoiceApproveAndRun != 0 || ChoiceViewFullDiff != 1 || ChoiceRejectAction != 2 || ChoiceCancelAllTasks != 3 {
		t.Errorf("unexpected ApprovalChoice enum indices: %d, %d, %d, %d",
			ChoiceApproveAndRun, ChoiceViewFullDiff, ChoiceRejectAction, ChoiceCancelAllTasks)
	}
}

func TestClassifyRiskLowVsHigh(t *testing.T) {
	lowCases := []struct {
		tool   string
		target string
	}{
		{"read_file", "cmd/tether/main.go"},
		{"view_file", "internal/ui/picker.go"},
		{"grep", "MakeRaw"},
		{"glob", "**/*.go"},
		{"write_file", ".cache/ast.tmp"},
		{"edit_file", "tmp/build.tmp"},
	}

	for _, tc := range lowCases {
		risk, reason := ClassifyRisk(tc.tool, tc.target)
		if risk != RiskLow {
			t.Errorf("ClassifyRisk(%q, %q) = (%s, %q), want RiskLow", tc.tool, tc.target, risk, reason)
		}
	}

	highCases := []struct {
		tool   string
		target string
	}{
		{"write_file", "cmd/tether/main.go"},
		{"edit_file", "internal/ui/renderer.go"},
		{"write_file", "go.mod"},
		{"edit_file", "package.json"},
		{"write_file", "../outside.tmp"},
		{"delete_file", "old.go"},
		{"read_file", ".env"},
		{"read_file", "session.db"},
		{"read_file", ".tether/session.db"},
		{"read_file", ".tether/cache.db"},
		{"proposal_status", "anything"},
		{"run_command", "go test ./..."},
		{"/exec", "rm -rf build/"},
		{"bash", "git push origin main"},
	}

	for _, tc := range highCases {
		risk, reason := ClassifyRisk(tc.tool, tc.target)
		if risk != RiskHigh {
			t.Errorf("ClassifyRisk(%q, %q) = (%s, %q), want RiskHigh", tc.tool, tc.target, risk, reason)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("ClassifyRisk(%q, %q) returned empty reason", tc.tool, tc.target)
		}
	}
}

func TestEvaluateProposal(t *testing.T) {
	fullDiff := strings.Join([]string{
		"--- a/cmd/tether/main.go",
		"+++ b/cmd/tether/main.go",
		"@@ -1,5 +1,6 @@",
		" package main",
		"+// New comment",
	}, "\n")

	prop := EvaluateProposal("Coder", "edit_file", "cmd/tether/main.go", "", fullDiff)
	if prop.Risk != RiskHigh {
		t.Errorf("prop.Risk = %s, want HIGH", prop.Risk)
	}
	if !prop.RequiresApproval {
		t.Error("expected prop.RequiresApproval = true for edit_file")
	}
	if prop.AgentName != "Coder" {
		t.Errorf("prop.AgentName = %q, want 'Coder'", prop.AgentName)
	}
	if !strings.Contains(prop.DiffPreview, "+// New comment") {
		t.Errorf("expected DiffPreview to be populated from fullDiff, got %q", prop.DiffPreview)
	}

	lowProp := EvaluateProposal("", "read_file", "README.md", "", "")
	if lowProp.Risk != RiskLow || lowProp.RequiresApproval {
		t.Errorf("expected read_file on README.md to be RiskLow without approval, got %+v", lowProp)
	}
	if lowProp.AgentName != "Coder" {
		t.Errorf("expected default AgentName 'Coder', got %q", lowProp.AgentName)
	}
}

func TestIsValidationCommand(t *testing.T) {
	yes := []string{"go test ./...", "go test -race ./internal/tui", "go build ./...", "go vet ./...", "gofmt -l .", "go test ./... 2>&1"}
	no := []string{"go build -o /tmp/x ./cmd", "go test ./... && rm -rf .", "gofmt -w .", "go mod tidy", "go install x", "rm -rf /", "go test ./... > out.txt"}
	for _, c := range yes {
		if ok, _ := IsValidationCommand(c); !ok {
			t.Errorf("expected %q to be auto-approved", c)
		}
	}
	for _, c := range no {
		if ok, _ := IsValidationCommand(c); ok {
			t.Errorf("expected %q to require approval", c)
		}
	}
}

func TestIsReadOnlyCommandPipedChainedAndGuarded(t *testing.T) {
	yes := []string{
		"git status --short | head -n 20",
		"find . -name '*.go' | sort",
		"ls -la && git diff --stat",
		"rg -n \"foo\" internal/ | head -n 5",
		"sed -n '1,20p' cmd/tether/main.go",
		"sed -n '1,20p' engine.go",
		`grep -E "a|b" internal/safety/policy.go`,
		`grep "->" internal/tui/view.go`,
		"ls internal/ 2>/dev/null",
		"CGO_ENABLED=0 go list ./...",
		"cd internal && ls -la",
		"git tag -l 'v*'",
	}
	no := []string{
		"find . -delete",
		"find . -exec rm {} +",
		"sed -i 's/a/b/g' file.go",
		"sed -n '1e' file.go",
		"sed -n '/pat/e' file.go",
		"sed -n '1,5p; w out.txt' file.go",
		"sed -n 's/a/b/e' file.go",
		"cat .env",
		"cat .env.production",
		"cat ~/.ssh/id_ed25519",
		"cat .git/config",
		"LD_PRELOAD=/tmp/evil.so ls",
		"echo $(whoami)",
		"cat `id`",
		"ls > out.txt",
		"cd /tmp",
		"git tag v1.0.0",
		"tree -o out.txt",
		"sort -o out.txt in.txt",
	}
	for _, c := range yes {
		if ok, reason := IsReadOnlyCommand(c); !ok {
			t.Errorf("expected read-only command %q to be auto-approved (got reason=%q)", c, reason)
		}
	}
	for _, c := range no {
		if ok, _ := IsReadOnlyCommand(c); ok {
			t.Errorf("expected %q NOT to be classified read-only", c)
		}
	}
}

func TestIsValidationCommandExtended(t *testing.T) {
	yes := []string{
		"cd tether && go test ./... | tail -n 20",
		"CGO_ENABLED=0 go test -race ./... 2>&1",
		"go mod verify",
		"npm test",
		"npm run lint",
		"pnpm run typecheck",
		"npx --no-install tsc --noEmit",
		"npx eslint ./src",
		"npx prettier --check .",
		"cargo clippy",
		"cargo fmt --check",
		"pytest -q",
		"python3 -m pytest",
		"make test",
	}
	no := []string{
		"npm install lodash",
		"npx -y cowsay hi",
		"npx eslint --fix .",
		"npx prettier --write .",
		"python3 -c 'print(1)'",
		"make deploy",
		"cargo fmt",
		"npm test > out.txt",
	}
	for _, c := range yes {
		if ok, reason := IsValidationCommand(c); !ok {
			t.Errorf("expected validation command %q to be auto-approved (reason=%q)", c, reason)
		}
	}
	for _, c := range no {
		if ok, _ := IsValidationCommand(c); ok {
			t.Errorf("expected %q NOT to be classified validation", c)
		}
	}
}

func TestIsNonCriticalToolAndWorkspaceBoundaries(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.go")
	linkPath := filepath.Join(ws, "escape_link.go")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	// Routine edits in CODE mode auto-run cleanly; PLAN/REVIEW modes still guard code files.
	if ok, _ := IsNonCriticalTool("code", "edit_file", "internal/tui/view.go", "", ws); !ok {
		t.Errorf("expected routine edit in CODE mode to be non-critical")
	}
	if ok, _ := IsNonCriticalTool("plan", "edit_file", "internal/tui/view.go", "", ws); ok {
		t.Errorf("expected code edit in PLAN mode to require gate")
	}
	if ok, _ := IsNonCriticalTool("review", "edit_file", "internal/tui/view.go", "", ws); ok {
		t.Errorf("expected code edit in REVIEW mode to require gate")
	}
	if ok, _ := IsNonCriticalTool("plan", "write_file", "docs/design.plan.md", "", ws); !ok {
		t.Errorf("expected docs/ plan artifact write in PLAN mode to be non-critical")
	}

	// Critical paths/tools must STILL require approval even in CODE mode.
	criticalEdits := []string{
		".env",
		"config/.env.local",
		"session.db",
		".tether/session.db",
		".tether/cache.db",
		".git/config",
		".git/hooks/pre-commit",
		"id_ed25519",
		"tls/server.key",
		"go.mod",
		"package.json",
		"Cargo.toml",
		"../outside.go",
		outside,
		"escape_link.go",
	}
	for _, target := range criticalEdits {
		if ok, _ := IsNonCriticalTool("code", "write_file", target, "", ws); ok {
			t.Errorf("expected critical/escaping write to %q to require approval", target)
		}
	}

	if ok, _ := IsNonCriticalTool("code", "delete_file", "internal/old.go", "", ws); ok {
		t.Errorf("expected delete_file to require approval")
	}
	if ok, _ := IsNonCriticalTool("code", "read_file", ".env", "", ws); ok {
		t.Errorf("expected read_file on .env to require approval")
	}
	if ok, _ := IsNonCriticalTool("code", "read_file", "internal/tui/view.go", "", ws); !ok {
		t.Errorf("expected read_file on normal file to be non-critical")
	}

	// Policy config files (.tetherignore/.gitignore) stay readable but NEVER auto-approved on writes;
	// .tether/PLAN.md is writable ONLY in plan mode.
	for _, cfg := range []string{".tetherignore", ".gitignore"} {
		if risk, _ := ClassifyRisk("write_file", cfg); risk != RiskHigh {
			t.Errorf("expected write_file(%s) RiskHigh, got %s", cfg, risk)
		}
		if ok, _ := IsNonCriticalTool("code", "write_file", cfg, "", ws); ok {
			t.Errorf("expected %s write NOT auto-approved in code mode", cfg)
		}
		if ok, _ := IsNonCriticalTool("plan", "write_file", cfg, "", ws); ok {
			t.Errorf("expected %s write NOT allowed in plan mode", cfg)
		}
		if ok, _ := IsNonCriticalTool("code", "read_file", cfg, "", ws); !ok {
			t.Errorf("expected %s read allowed", cfg)
		}
	}
	if ok, _ := IsNonCriticalTool("code", "write_file", ".tether/PLAN.md", "", ws); ok {
		t.Errorf("expected .tether/PLAN.md write blocked in code mode")
	}
	if ok, _ := IsNonCriticalTool("plan", "write_file", ".tether/PLAN.md", "", ws); !ok {
		t.Errorf("expected .tether/PLAN.md write auto-approved in plan mode")
	}
}
