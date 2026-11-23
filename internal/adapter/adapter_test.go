package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// setTestHome redirects the home directory used by the adapter installers to a
// throw-away temp dir and returns it. Both HOME (unix) and USERPROFILE (windows)
// are set so the tests never touch the real user profile.
func setTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// readJSON parses path and fails the test when the file is missing or invalid.
func readJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(content, &out); err != nil {
		t.Fatalf("unmarshal %s: %v (content: %s)", path, err, content)
	}
	return out
}

// writeTestFile writes content to path, creating parent directories.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// nestedValue walks a decoded JSON document along keys and returns the value.
func nestedValue(t *testing.T, doc map[string]interface{}, keys ...string) interface{} {
	t.Helper()
	var current interface{} = doc
	for _, key := range keys {
		m, ok := current.(map[string]interface{})
		if !ok {
			t.Fatalf("path %v: not a JSON object at %q", keys, key)
		}
		current, ok = m[key]
		if !ok {
			t.Fatalf("path %v: missing key %q", keys, key)
		}
	}
	return current
}

// nestedString is nestedValue restricted to a string leaf.
func nestedString(t *testing.T, doc map[string]interface{}, keys ...string) string {
	t.Helper()
	value := nestedValue(t, doc, keys...)
	str, ok := value.(string)
	if !ok {
		t.Fatalf("path %v: expected string, got %T", keys, value)
	}
	return str
}

// nestedMap is nestedValue restricted to a JSON object leaf.
func nestedMap(t *testing.T, doc map[string]interface{}, keys ...string) map[string]interface{} {
	t.Helper()
	value := nestedValue(t, doc, keys...)
	m, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf("path %v: expected object, got %T", keys, value)
	}
	return m
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// permissionStrings converts perms[field] into []string, failing on non-strings.
func permissionStrings(t *testing.T, perms map[string]interface{}, field string) []string {
	t.Helper()
	raw, ok := perms[field].([]interface{})
	if !ok {
		t.Fatalf("permissions.%s = %T, want array", field, perms[field])
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		str, ok := item.(string)
		if !ok {
			t.Fatalf("permissions.%s contains non-string item %T", field, item)
		}
		out = append(out, str)
	}
	return out
}

// countTetherAllowEntries counts allow-list entries that look like tether MCP
// permissions.
func countTetherAllowEntries(perms map[string]interface{}) int {
	allow, ok := perms["allow"].([]interface{})
	if !ok {
		return -1
	}
	count := 0
	for _, item := range allow {
		if str, ok := item.(string); ok && strings.Contains(str, "mcp(tether/") {
			count++
		}
	}
	return count
}

// settingsPathFor returns the agy settings.json path under home.
func settingsPathFor(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// expectedAgentFiles maps an installer name to the files it must create.
func expectedAgentFiles() map[string][]string {
	return map[string][]string{
		"agy":         {".gemini/config/mcp_config.json", ".gemini/config/hooks.json"},
		"antigravity": {".gemini/config/mcp_config.json", ".gemini/config/hooks.json"},
		"claude":      {".claude/mcp_config.json", ".claude/hooks.json"},
		"codex":       {".codex/mcp_config.json"},
		"cursor":      {".cursor/mcp.json"},
		"windsurf":    {".codeium/windsurf/mcp_config.json"},
		"cline":       {".cline/mcp_settings.json"},
		"roo":         {".roo/mcp_settings.json"},
		"opencode":    {".config/opencode/mcp.json"},
	}
}

// ---------------------------------------------------------------------------
// Install
// ---------------------------------------------------------------------------

func TestInstall(t *testing.T) {
	expected := expectedAgentFiles()

	for agent, files := range expected {
		agent, files := agent, files
		t.Run(agent, func(t *testing.T) {
			home := setTestHome(t)
			if err := Install(agent); err != nil {
				t.Fatalf("Install(%q) returned error: %v", agent, err)
			}
			for _, rel := range files {
				if !fileExists(filepath.Join(home, filepath.FromSlash(rel))) {
					t.Errorf("Install(%q): expected file %s to exist", agent, rel)
				}
			}
		})
	}

	t.Run("unsupported", func(t *testing.T) {
		cases := []string{"", "unknown", "ChatGPT", "CLAUDE", "claud", "gemini", "all-agents", " cursor"}
		for _, agent := range cases {
			agent := agent
			t.Run(agent, func(t *testing.T) {
				home := setTestHome(t)
				err := Install(agent)
				if err == nil {
					t.Fatalf("Install(%q) returned nil, want unsupported-agent error", agent)
				}
				if !strings.Contains(err.Error(), "unsupported agent") {
					t.Fatalf("Install(%q) error = %q, want it to contain %q", agent, err, "unsupported agent")
				}
				entries, derr := os.ReadDir(home)
				if derr != nil {
					t.Fatalf("read home dir: %v", derr)
				}
				if len(entries) != 0 {
					t.Fatalf("Install(%q) created %d entries in home, want 0", agent, len(entries))
				}
			})
		}
	})

	t.Run("all", func(t *testing.T) {
		home := setTestHome(t)
		if err := Install("all"); err != nil {
			t.Fatalf("Install(\"all\") returned error: %v", err)
		}
		for _, agent := range []string{"agy", "claude", "codex", "cursor", "windsurf", "cline", "roo", "opencode"} {
			for _, rel := range expected[agent] {
				if !fileExists(filepath.Join(home, filepath.FromSlash(rel))) {
					t.Errorf("Install(\"all\"): missing %s file %s", agent, rel)
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// tetherBinary
// ---------------------------------------------------------------------------

func TestTetherBinary(t *testing.T) {
	got := tetherBinary()
	if got == "" {
		t.Fatal("tetherBinary() returned empty string")
	}
	if strings.TrimSpace(got) != got {
		t.Fatalf("tetherBinary() = %q, want no surrounding whitespace", got)
	}
	// Under `go test`, os.Executable() succeeds, so we must get a real path,
	// not the bare "tether" fallback string.
	if got == "tether" {
		t.Fatalf("tetherBinary() = %q, want the test binary path while os.Executable() works", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("tetherBinary() = %q, which is not reachable: %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// writeJSON
// ---------------------------------------------------------------------------

func TestWriteJSON(t *testing.T) {
	tetherPayload := func() map[string]interface{} {
		return map[string]interface{}{
			"mcpServers": map[string]interface{}{
				"tether": map[string]interface{}{"command": "tether", "args": []string{"mcp"}},
			},
		}
	}

	t.Run("creates new file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp_config.json")
		if err := writeJSON(path, tetherPayload()); err != nil {
			t.Fatalf("writeJSON: %v", err)
		}
		doc := readJSON(t, path)
		if got := nestedString(t, doc, "mcpServers", "tether", "command"); got != "tether" {
			t.Fatalf("mcpServers.tether.command = %q, want %q", got, "tether")
		}
	})

	t.Run("merges preserving unrelated top-level keys", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp_config.json")
		writeTestFile(t, path, `{"theme":"dark","mcpServers":{"other":{"command":"other"}}}`)

		if err := writeJSON(path, tetherPayload()); err != nil {
			t.Fatalf("writeJSON: %v", err)
		}
		doc := readJSON(t, path)
		if got := doc["theme"]; got != "dark" {
			t.Fatalf("theme = %v, want %q (unrelated key must survive)", got, "dark")
		}
		// mcpServers is replaced wholesale by the top-level merge, so only pin
		// the tether entry here.
		if got := nestedString(t, doc, "mcpServers", "tether", "command"); got != "tether" {
			t.Fatalf("mcpServers.tether.command = %q, want %q", got, "tether")
		}
	})

	t.Run("removes legacy top-level harness", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp_config.json")
		writeTestFile(t, path, `{"harness":"legacy","keep":"me"}`)

		if err := writeJSON(path, map[string]interface{}{"mcpServers": map[string]interface{}{}}); err != nil {
			t.Fatalf("writeJSON: %v", err)
		}
		doc := readJSON(t, path)
		if _, ok := doc["harness"]; ok {
			t.Fatal("legacy top-level \"harness\" key was not removed")
		}
		if doc["keep"] != "me" {
			t.Fatalf("keep = %v, want %q", doc["keep"], "me")
		}
	})

	t.Run("removes legacy nested mcpServers.harness", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp_config.json")
		writeTestFile(t, path, `{"mcpServers":{"harness":{"command":"old"},"other":{"command":"other"}}}`)

		// Non-map payload so the merge does not overwrite mcpServers and the
		// nested cleanup can be observed in isolation.
		if err := writeJSON(path, []string{"ignored"}); err != nil {
			t.Fatalf("writeJSON: %v", err)
		}
		servers := nestedMap(t, readJSON(t, path), "mcpServers")
		if _, ok := servers["harness"]; ok {
			t.Fatal("legacy mcpServers.harness entry was not removed")
		}
		if _, ok := servers["other"]; !ok {
			t.Fatal("unrelated mcpServers.other entry was removed")
		}
	})

	t.Run("non-map data is silently dropped", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp_config.json")
		writeTestFile(t, path, `{"existing":"value"}`)

		if err := writeJSON(path, []string{"a", "b"}); err != nil {
			t.Fatalf("writeJSON with non-map data returned error: %v", err)
		}
		doc := readJSON(t, path)
		if doc["existing"] != "value" {
			t.Fatalf("existing = %v, want %q", doc["existing"], "value")
		}
		if len(doc) != 1 {
			t.Fatalf("document = %v, want only the pre-existing key (non-map payload must be dropped)", doc)
		}
	})

	t.Run("non-map data on missing file writes empty object", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fresh.json")
		if err := writeJSON(path, "not-a-map"); err != nil {
			t.Fatalf("writeJSON with non-map data returned error: %v", err)
		}
		if doc := readJSON(t, path); len(doc) != 0 {
			t.Fatalf("document = %v, want empty object", doc)
		}
	})

	t.Run("corrupt existing file is overwritten without error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp_config.json")
		writeTestFile(t, path, `{ this is not json `)

		if err := writeJSON(path, tetherPayload()); err != nil {
			t.Fatalf("writeJSON over corrupt file returned error: %v", err)
		}
		doc := readJSON(t, path)
		if got := nestedString(t, doc, "mcpServers", "tether", "command"); got != "tether" {
			t.Fatalf("mcpServers.tether.command = %q, want %q", got, "tether")
		}
	})

	t.Run("empty existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp_config.json")
		writeTestFile(t, path, "")

		if err := writeJSON(path, tetherPayload()); err != nil {
			t.Fatalf("writeJSON over empty file returned error: %v", err)
		}
		doc := readJSON(t, path)
		if got := nestedString(t, doc, "mcpServers", "tether", "command"); got != "tether" {
			t.Fatalf("mcpServers.tether.command = %q, want %q", got, "tether")
		}
	})

	t.Run("path in nonexistent directory errors", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no", "such", "dir", "mcp_config.json")
		if err := writeJSON(path, map[string]interface{}{"a": "b"}); err == nil {
			t.Fatal("writeJSON into nonexistent directory returned nil, want error")
		}
	})
}

// ---------------------------------------------------------------------------
// configureAgyPermissions
// ---------------------------------------------------------------------------

func TestConfigureAgyPermissions(t *testing.T) {
	t.Run("missing settings file is a no-op", func(t *testing.T) {
		home := setTestHome(t)
		if err := configureAgyPermissions(home); err != nil {
			t.Fatalf("configureAgyPermissions with missing settings.json = %v, want nil", err)
		}
		if fileExists(settingsPathFor(home)) {
			t.Fatal("configureAgyPermissions created settings.json, want no-op")
		}
	})

	t.Run("invalid json errors", func(t *testing.T) {
		home := setTestHome(t)
		writeTestFile(t, settingsPathFor(home), `{"permissions": `)
		if err := configureAgyPermissions(home); err == nil {
			t.Fatal("configureAgyPermissions on invalid JSON = nil, want error")
		}
	})

	t.Run("creates permissions when absent", func(t *testing.T) {
		home := setTestHome(t)
		writeTestFile(t, settingsPathFor(home), `{"theme":"dark"}`)
		if err := configureAgyPermissions(home); err != nil {
			t.Fatalf("configureAgyPermissions: %v", err)
		}
		doc := readJSON(t, settingsPathFor(home))
		if doc["theme"] != "dark" {
			t.Fatalf("theme = %v, want %q", doc["theme"], "dark")
		}
		allow := permissionStrings(t, nestedMap(t, doc, "permissions"), "allow")
		if len(allow) != 20 {
			t.Fatalf("permissions.allow has %d entries, want 20: %v", len(allow), allow)
		}
	})

	t.Run("wrong permissions type is replaced", func(t *testing.T) {
		home := setTestHome(t)
		writeTestFile(t, settingsPathFor(home), `{"permissions":"nope"}`)
		if err := configureAgyPermissions(home); err != nil {
			t.Fatalf("configureAgyPermissions: %v", err)
		}
		perms := nestedMap(t, readJSON(t, settingsPathFor(home)), "permissions")
		if got := len(permissionStrings(t, perms, "allow")); got != 20 {
			t.Fatalf("permissions.allow has %d entries, want 20", got)
		}
	})

	t.Run("preserves unrelated allow and deny entries", func(t *testing.T) {
		home := setTestHome(t)
		writeTestFile(t, settingsPathFor(home), `{
			"permissions": {
				"allow": ["run_command(ls)", "read_file"],
				"deny":  ["run_command(rm)"]
			}
		}`)
		if err := configureAgyPermissions(home); err != nil {
			t.Fatalf("configureAgyPermissions: %v", err)
		}
		perms := nestedMap(t, readJSON(t, settingsPathFor(home)), "permissions")
		allow := permissionStrings(t, perms, "allow")
		for _, want := range []string{"run_command(ls)", "read_file"} {
			found := false
			for _, got := range allow {
				if got == want {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("allow list lost pre-existing entry %q: %v", want, allow)
			}
		}
		deny := permissionStrings(t, perms, "deny")
		if len(deny) != 1 || deny[0] != "run_command(rm)" {
			t.Fatalf("deny = %v, want [run_command(rm)]", deny)
		}
	})

	t.Run("removes tether entries from deny", func(t *testing.T) {
		home := setTestHome(t)
		writeTestFile(t, settingsPathFor(home), `{
			"permissions": {
				"deny": ["mcp(tether/sql_query)", "run_command(rm)", "tether-hook", "read_file"]
			}
		}`)
		if err := configureAgyPermissions(home); err != nil {
			t.Fatalf("configureAgyPermissions: %v", err)
		}
		deny := permissionStrings(t, nestedMap(t, readJSON(t, settingsPathFor(home)), "permissions"), "deny")
		want := []string{"run_command(rm)", "read_file"}
		if len(deny) != len(want) {
			t.Fatalf("deny = %v, want %v", deny, want)
		}
		for i := range want {
			if deny[i] != want[i] {
				t.Fatalf("deny = %v, want %v", deny, want)
			}
		}
	})

	t.Run("keeps non-string deny entries", func(t *testing.T) {
		home := setTestHome(t)
		writeTestFile(t, settingsPathFor(home), `{
			"permissions": {
				"deny": ["mcp(tether/kb_search)", 42, {"rule":"x"}, null]
			}
		}`)
		if err := configureAgyPermissions(home); err != nil {
			t.Fatalf("configureAgyPermissions: %v", err)
		}
		perms := nestedMap(t, readJSON(t, settingsPathFor(home)), "permissions")
		raw, ok := perms["deny"].([]interface{})
		if !ok {
			t.Fatalf("permissions.deny = %T, want array", perms["deny"])
		}
		if len(raw) != 3 {
			t.Fatalf("deny has %d entries, want 3 (tether string removed, non-strings kept): %v", len(raw), raw)
		}
		for _, item := range raw {
			if str, ok := item.(string); ok && strings.Contains(str, "tether") {
				t.Fatalf("tether deny entry survived: %q", str)
			}
		}
	})

	t.Run("idempotent across runs", func(t *testing.T) {
		home := setTestHome(t)
		writeTestFile(t, settingsPathFor(home), `{"permissions":{"allow":["mcp(tether/*)"]}}`)

		if err := configureAgyPermissions(home); err != nil {
			t.Fatalf("first configureAgyPermissions: %v", err)
		}
		if got := countTetherAllowEntries(nestedMap(t, readJSON(t, settingsPathFor(home)), "permissions")); got != 20 {
			t.Fatalf("after first run: %d tether allow entries, want 20", got)
		}

		if err := configureAgyPermissions(home); err != nil {
			t.Fatalf("second configureAgyPermissions: %v", err)
		}
		perms := nestedMap(t, readJSON(t, settingsPathFor(home)), "permissions")
		if got := countTetherAllowEntries(perms); got != 20 {
			t.Fatalf("after second run: %d tether allow entries, want 20 (must be deduped)", got)
		}
		if got := len(permissionStrings(t, perms, "allow")); got != 20 {
			t.Fatalf("after second run: allow list has %d entries, want 20", got)
		}

		content, err := os.ReadFile(settingsPathFor(home))
		if err != nil {
			t.Fatalf("read settings.json: %v", err)
		}
		var roundTrip map[string]interface{}
		if err := json.Unmarshal(content, &roundTrip); err != nil {
			t.Fatalf("settings.json is not valid JSON: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// installGenericMCP / installCodex / installCursor
// ---------------------------------------------------------------------------

func TestInstallGenericMCP(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		home := setTestHome(t)
		if err := installGenericMCP(".testagent", "mcp.json"); err != nil {
			t.Fatalf("installGenericMCP: %v", err)
		}
		doc := readJSON(t, filepath.Join(home, ".testagent", "mcp.json"))
		if got := nestedString(t, doc, "mcpServers", "tether", "command"); got != tetherBinary() {
			t.Fatalf("command = %q, want %q", got, tetherBinary())
		}
		args, ok := nestedMap(t, doc, "mcpServers", "tether")["args"].([]interface{})
		if !ok {
			t.Fatalf("args is %T, want array", nestedMap(t, doc, "mcpServers", "tether")["args"])
		}
		if len(args) != 1 || args[0] != "mcp" {
			t.Fatalf("args = %v, want [mcp]", args)
		}
	})

	t.Run("mkdir failure", func(t *testing.T) {
		home := setTestHome(t)
		// Occupy the target directory path with a regular file so MkdirAll fails.
		writeTestFile(t, filepath.Join(home, ".blocked"), "not a directory")
		err := installGenericMCP(".blocked", "mcp.json")
		if err == nil {
			t.Fatal("installGenericMCP over a file path returned nil, want error")
		}
		if !strings.Contains(err.Error(), ".blocked") {
			t.Fatalf("error = %q, want it to mention the offending dir %q", err, ".blocked")
		}
	})
}

func TestInstallCodex(t *testing.T) {
	home := setTestHome(t)
	if err := installCodex(); err != nil {
		t.Fatalf("installCodex: %v", err)
	}
	path := filepath.Join(home, ".codex", "mcp_config.json")
	if !fileExists(path) {
		t.Fatalf("expected %s to exist", path)
	}
	if got := nestedString(t, readJSON(t, path), "mcpServers", "tether", "command"); got != tetherBinary() {
		t.Fatalf("command = %q, want %q", got, tetherBinary())
	}
}

func TestInstallCursor(t *testing.T) {
	home := setTestHome(t)
	if err := installCursor(); err != nil {
		t.Fatalf("installCursor: %v", err)
	}
	path := filepath.Join(home, ".cursor", "mcp.json")
	if !fileExists(path) {
		t.Fatalf("expected %s to exist", path)
	}
	if got := nestedString(t, readJSON(t, path), "mcpServers", "tether", "command"); got != tetherBinary() {
		t.Fatalf("command = %q, want %q", got, tetherBinary())
	}
}

// ---------------------------------------------------------------------------
// installAgy / installClaude
// ---------------------------------------------------------------------------

func TestInstallAgy(t *testing.T) {
	t.Run("writes mcp and hooks config", func(t *testing.T) {
		home := setTestHome(t)
		if err := installAgy(); err != nil {
			t.Fatalf("installAgy: %v", err)
		}

		mcpPath := filepath.Join(home, ".gemini", "config", "mcp_config.json")
		hooksPath := filepath.Join(home, ".gemini", "config", "hooks.json")
		if !fileExists(mcpPath) {
			t.Fatalf("expected %s to exist", mcpPath)
		}
		if !fileExists(hooksPath) {
			t.Fatalf("expected %s to exist", hooksPath)
		}

		mcpDoc := readJSON(t, mcpPath)
		if got := nestedString(t, mcpDoc, "mcpServers", "tether", "command"); got != tetherBinary() {
			t.Fatalf("mcp command = %q, want %q", got, tetherBinary())
		}

		hooksDoc := readJSON(t, hooksPath)
		for _, event := range []string{"PreInvocation", "PreToolUse", "PostToolUse"} {
			entries, ok := nestedValue(t, hooksDoc, "tether", event).([]interface{})
			if !ok || len(entries) == 0 {
				t.Fatalf("hooks.tether.%s = %v, want a non-empty array", event, nestedValue(t, hooksDoc, "tether", event))
			}
		}
		preToolUse, ok := nestedValue(t, hooksDoc, "tether", "PreToolUse").([]interface{})
		if !ok || len(preToolUse) == 0 {
			t.Fatalf("hooks.tether.PreToolUse is not a non-empty array")
		}
		preToolUseEntry, ok := preToolUse[0].(map[string]interface{})
		if !ok {
			t.Fatalf("hooks.tether.PreToolUse[0] = %T, want object", preToolUse[0])
		}
		if matcher, _ := preToolUseEntry["matcher"].(string); !strings.Contains(matcher, "run_command") {
			t.Fatalf("hooks.tether.PreToolUse[0].matcher = %q, want it to contain %q", matcher, "run_command")
		}
		autoApprove, ok := nestedValue(t, hooksDoc, "kiro-style-auto-approve", "PreToolUse").([]interface{})
		if !ok || len(autoApprove) == 0 {
			t.Fatalf("kiro-style-auto-approve.PreToolUse is not a non-empty array")
		}

		content, err := os.ReadFile(hooksPath)
		if err != nil {
			t.Fatalf("read hooks.json: %v", err)
		}
		if !strings.Contains(string(content), "hook PreToolUse") {
			t.Fatalf("hooks.json does not reference the PreToolUse hook command: %s", content)
		}
	})

	t.Run("broken settings.json is only a warning", func(t *testing.T) {
		home := setTestHome(t)
		writeTestFile(t, settingsPathFor(home), `{"permissions": `)

		if err := installAgy(); err != nil {
			t.Fatalf("installAgy returned %v, want nil (settings failure must be a warning)", err)
		}
		// The MCP/hooks configs must still have been written.
		if !fileExists(filepath.Join(home, ".gemini", "config", "mcp_config.json")) {
			t.Fatal("mcp_config.json missing after settings.json warning path")
		}
		// And the broken file must NOT have been rewritten.
		content, err := os.ReadFile(settingsPathFor(home))
		if err != nil {
			t.Fatalf("read settings.json: %v", err)
		}
		if string(content) != `{"permissions": ` {
			t.Fatalf("settings.json was modified on the error path: %s", content)
		}
	})
}

func TestInstallClaude(t *testing.T) {
	home := setTestHome(t)
	if err := installClaude(); err != nil {
		t.Fatalf("installClaude: %v", err)
	}

	mcpPath := filepath.Join(home, ".claude", "mcp_config.json")
	hooksPath := filepath.Join(home, ".claude", "hooks.json")
	if !fileExists(mcpPath) {
		t.Fatalf("expected %s to exist", mcpPath)
	}
	if !fileExists(hooksPath) {
		t.Fatalf("expected %s to exist", hooksPath)
	}

	if got := nestedString(t, readJSON(t, mcpPath), "mcpServers", "tether", "command"); got != tetherBinary() {
		t.Fatalf("mcp command = %q, want %q", got, tetherBinary())
	}

	hooksDoc := readJSON(t, hooksPath)
	entries, ok := nestedValue(t, hooksDoc, "hooks", "PreToolUse").([]interface{})
	if !ok {
		t.Fatalf("hooks.PreToolUse = %T, want array", nestedValue(t, hooksDoc, "hooks", "PreToolUse"))
	}
	if len(entries) == 0 {
		t.Fatal("hooks.PreToolUse is empty")
	}
	first, ok := entries[0].(map[string]interface{})
	if !ok {
		t.Fatalf("hooks.PreToolUse[0] = %T, want object", entries[0])
	}
	if got, _ := first["matcher"].(string); got != "write_to_file|Edit|MultiEdit|Write" {
		t.Fatalf("hooks.PreToolUse[0].matcher = %q", got)
	}
}

// ---------------------------------------------------------------------------
// GetInstalledAgent
// ---------------------------------------------------------------------------

func TestGetInstalledAgent(t *testing.T) {
	allowed := map[string]bool{"agy": true, "claude": true, "codex": true, "cursor": true, "unknown": true}
	for i := 0; i < 5; i++ {
		got := GetInstalledAgent()
		if !allowed[got] {
			t.Fatalf("GetInstalledAgent() = %q, want one of agy|claude|codex|cursor|unknown", got)
		}
	}
}
