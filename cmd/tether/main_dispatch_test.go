package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSubcommandsDispatchedBeforeDefaultAndProposalRemoved(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	if regexp.MustCompile(`case "proposal"|func cmdProposal\(`).MatchString(text) {
		t.Fatal("legacy cmdProposal / proposal dispatcher must be removed")
	}

	defaultIdx := regexp.MustCompile(`(?m)^\tdefault:`).FindStringIndex(text)
	if defaultIdx == nil {
		t.Fatal("could not locate default arm in main()")
	}

	for _, sub := range []string{"setup", "init", "web", "mcp", "hook", "mode", "undo", "status", "doctor", "install", "tui", "run", "version", "help"} {
		idx := regexp.MustCompile(`"` + sub + `"`).FindStringIndex(text)
		if idx == nil || idx[0] > defaultIdx[0] {
			t.Fatalf("subcommand %q must be dispatched before default arm", sub)
		}
	}
}

func TestSetupCommandHeadlessNonBlockingWithProbeAndMCPImport(t *testing.T) {
	cfgDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("TETHER_CONFIG_DIR", cfgDir)
	t.Setenv("HOME", homeDir)
	t.Setenv("SSH_TTY", "/dev/pts/99")

	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"}]}`))
	}))
	defer probeSrv.Close()

	// Seed an IDE MCP config in HOME for optional global import.
	agyPath := filepath.Join(homeDir, ".gemini", "config", "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(agyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agyPath, []byte(`{"mcpServers":{"demo-mcp":{"url":"https://example.com/mcp","headers":{"Authorization":"Bearer tok-123"}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := runSetupCommand([]string{
		"--cli",
		"--provider", "deepseek",
		"--api-key", "sk-stage1-test",
		"--base-url", probeSrv.URL + "/chat/completions",
		"--import-mcp", "agy",
	}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("runSetupCommand failed (code=%d): %s\n%s", code, errOut.String(), out.String())
	}
	text := out.String()
	if strings.Contains(text, "sk-stage1-test") || strings.Contains(text, "tok-123") {
		t.Fatalf("setup output leaked secret!\n%s", text)
	}
	if !strings.Contains(text, "0-Token Key Probe Results") || !strings.Contains(text, "demo-mcp") {
		t.Fatalf("expected probe results and imported MCP summary, got:\n%s", text)
	}

	credBytes, err := os.ReadFile(filepath.Join(cfgDir, "credentials.json"))
	if err != nil {
		t.Fatalf("read credentials.json: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(credBytes, &parsed); err != nil {
		t.Fatalf("parse credentials.json: %v", err)
	}
	if _, ok := parsed["deepseek"]; !ok {
		t.Fatalf("expected deepseek saved in credentials.json, got %+v", parsed)
	}
}
