package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossparties/tether/internal/core"
)

func TestDoctorReadOnlyOnEmptyWorkspaceCreatesNothing(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	var out bytes.Buffer
	passed, warnings, failed := runDoctor(ws, false, &out)
	_ = passed
	_ = warnings
	_ = failed

	if _, err := os.Stat(filepath.Join(ws, ".tether")); !os.IsNotExist(err) {
		t.Fatalf("tether doctor without --fix created .tether/ in fresh workspace!")
	}
	if _, err := os.Stat(filepath.Join(ws, "session.db")); !os.IsNotExist(err) {
		t.Fatalf("tether doctor without --fix created session.db!")
	}
	if !strings.Contains(out.String(), "not found") {
		t.Errorf("expected doctor output to mention missing .tether/session.db, got:\n%s", out.String())
	}
}

func TestDoctorChecksInvalidRegexLooseCredsAndLegacyDBAndFixRepairs(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Seed loose credentials.json (0644), invalid re: rule, and legacy session.db
	credPath := filepath.Join(home, ".config", "tether", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(credPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, []byte(`{"deepseek":"sk-test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".tether"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("re:[unclosed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "session.db"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	_, warnings, failed := runDoctor(ws, false, &out)
	txt := out.String()
	if failed == 0 || !strings.Contains(txt, "Ignore Syntax") {
		t.Errorf("expected invalid regex in .tether/ignore to fail doctor check, got failed=%d\n%s", failed, txt)
	}
	if warnings == 0 || !strings.Contains(txt, "Legacy Root DB") || !strings.Contains(txt, "looser than 0600") {
		t.Errorf("expected warnings for legacy session.db and loose credentials.json perms, got warnings=%d\n%s", warnings, txt)
	}

	// Fix invalid regex first, then run --fix to repair legacy session.db, .gitignore, cache.db, and 0600 perms.
	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("dist/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# Hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var fixOut bytes.Buffer
	passed, _, fixFailed := runDoctor(ws, true, &fixOut)
	if fixFailed != 0 || passed == 0 {
		t.Fatalf("expected --fix to leave 0 failures, got passed=%d failed=%d:\n%s", passed, fixFailed, fixOut.String())
	}
	if _, err := os.Stat(filepath.Join(ws, ".tether", "session.db")); err != nil {
		t.Errorf("expected .tether/session.db after --fix: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "session.db")); !os.IsNotExist(err) {
		t.Errorf("expected legacy root session.db migrated away after --fix")
	}
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected credentials.json tightened to 0600, got %04o", info.Mode().Perm())
	}
}

func TestDoctorReadOnlyWALIdleDoesNotCreateSHM(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte(".tether/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.Close()

	// Ensure no sidecar files remain before running read-only doctor.
	_ = os.Remove(filepath.Join(ws, ".tether", "session.db-shm"))
	_ = os.Remove(filepath.Join(ws, ".tether", "session.db-wal"))

	var out bytes.Buffer
	passed, _, failed := runDoctor(ws, false, &out)
	if failed != 0 || passed == 0 {
		t.Fatalf("runDoctor on clean closed workspace failed: passed=%d failed=%d\n%s", passed, failed, out.String())
	}
	if _, err := os.Stat(filepath.Join(ws, ".tether", "session.db-shm")); !os.IsNotExist(err) {
		t.Errorf("read-only tether doctor created .tether/session.db-shm!")
	}
}

func TestCLIInitAndHookExitCode2EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("cli binary build test skipped in -short")
	}

	bin := filepath.Join(t.TempDir(), "tether-cli")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tether: %v\n%s", err, out)
	}

	ws := t.TempDir()
	home := t.TempDir()
	// Multi-stack project: Go + nested web/ Svelte + Python
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.com/app\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "web"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "web", "package.json"), []byte(`{"dependencies":{"svelte":"^5.0.0"},"scripts":{"build":"vite build","check":"svelte-check"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "pyproject.toml"), []byte("[project]\nname='x'\n"), 0644); err != nil {
		t.Fatal(err)
	}

	initCmd := exec.Command(bin, "init")
	initCmd.Dir = ws
	initCmd.Env = append(os.Environ(), "HOME="+home)
	initOut, err := initCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tether init failed: %v\n%s", err, initOut)
	}
	if _, err := os.Stat(filepath.Join(ws, ".tether", "ignore")); err != nil {
		t.Fatalf("tether init did not seed .tether/ignore: %v", err)
	}

	e, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	stack, _ := e.GetProjectSetting("tech_stack")
	_ = e.Close()
	for _, want := range []string{"go", "svelte", "python"} {
		if !strings.Contains(stack, want) {
			t.Errorf("expected multi-stack tech_stack to contain %q, got %q", want, stack)
		}
	}

	// Verify `tether hook PreToolUse` exits with code 2 on dangerous shell commands.
	for _, cmdLine := range []string{"cat .env", "cat .e*", "cat $(echo .env)", "echo secret > .env"} {
		hookCmd := exec.Command(bin, "hook", "PreToolUse")
		hookCmd.Dir = ws
		hookCmd.Env = append(os.Environ(), "HOME="+home)
		hookCmd.Stdin = strings.NewReader(`{"tool_name":"run_command","command":` + mustQuoteCLI(cmdLine) + `}`)
		var stderr bytes.Buffer
		hookCmd.Stderr = &stderr
		out, herr := hookCmd.Output()
		exitErr, ok := herr.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 2 {
			t.Errorf("tether hook PreToolUse(%q) expected exit code 2, got err=%v out=%s stderr=%s", cmdLine, herr, out, stderr.String())
		}
	}
}

func mustQuoteCLI(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
