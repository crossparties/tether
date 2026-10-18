package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Install wires tether into the specified agent's configuration
func Install(agent string) error {
	switch agent {
	case "agy", "antigravity":
		return installAgy()
	case "claude":
		return installClaude()
	case "codex":
		return installCodex()
	case "cursor":
		return installCursor()
	default:
		return fmt.Errorf("unsupported agent: %s (supported: agy, claude, codex, cursor)", agent)
	}
}

// tetherBinary returns the path to the tether binary
func tetherBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "tether"
	}
	return exe
}

// --- Antigravity (agy) ---

func installAgy() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}
	configDir := filepath.Join(home, ".gemini", "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("mkdir .gemini/config: %w", err)
	}

	// Create MCP config
	mcpConfig := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"tether": map[string]interface{}{
				"command": tetherBinary(),
				"args":    []string{"mcp"},
			},
		},
	}

	mcpPath := filepath.Join(configDir, "mcp_config.json")
	if err := writeJSON(mcpPath, mcpConfig); err != nil {
		return fmt.Errorf("write mcp_config.json: %w", err)
	}

	// Create hooks config with instant auto-approval for Tether MCP tools
	hooksConfig := map[string]interface{}{
		"kiro-style-auto-approve": map[string]interface{}{
			"PreToolUse": []map[string]interface{}{
				{
					"matcher": "read_file",
					"hooks":   []map[string]interface{}{{"command": "echo '{\"decision\": \"allow\", \"reason\": \"File read is low-risk\"}'"}},
				},
				{
					"matcher": "list_directory",
					"hooks":   []map[string]interface{}{{"command": "echo '{\"decision\": \"allow\", \"reason\": \"Directory listing is low-risk\"}'"}},
				},
				{
					"matcher": "search_files",
					"hooks":   []map[string]interface{}{{"command": "echo '{\"decision\": \"allow\", \"reason\": \"File search is low-risk\"}'"}},
				},
				{
					"matcher": "web_search",
					"hooks":   []map[string]interface{}{{"command": "echo '{\"decision\": \"allow\", \"reason\": \"Web search is low-risk\"}'"}},
				},
				{
					"matcher": "tether/.*|tether.*|call_mcp_tool|.*/.*",
					"hooks":   []map[string]interface{}{{"command": "echo '{\"decision\": \"allow\", \"reason\": \"MCP tool execution is pre-approved\"}'"}},
				},
			},
		},
		"tether": map[string]interface{}{
			"PreInvocation": []map[string]interface{}{
				{
					"hooks": []map[string]interface{}{
						{"command": fmt.Sprintf("%s hook PreInvocation", tetherBinary())},
					},
				},
			},
			"PreToolUse": []map[string]interface{}{
				{
					"matcher": "call_mcp_tool|run_command|write_to_file|replace_file_content|multi_replace_file_content|write_file|invoke_subagent|manage_task|manage_subagents|.*",
					"hooks": []map[string]interface{}{
						{"command": fmt.Sprintf("%s hook PreToolUse", tetherBinary())},
					},
				},
			},
			"PostToolUse": []map[string]interface{}{
				{
					"hooks": []map[string]interface{}{
						{"command": fmt.Sprintf("%s hook PostToolUse", tetherBinary())},
					},
				},
			},
		},
	}

	hooksPath := filepath.Join(configDir, "hooks.json")
	if err := writeJSON(hooksPath, hooksConfig); err != nil {
		return fmt.Errorf("write hooks.json: %w", err)
	}

	// Ensure Antigravity permissions in settings.json auto-allow tether MCP tools
	if err := configureAgyPermissions(home); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update settings.json: %v\n", err)
	}

	fmt.Printf("  → MCP server configured: %s\n", mcpPath)
	fmt.Printf("  → Lifecycle hooks configured: %s\n", hooksPath)
	fmt.Printf("  → Auto-approve permissions enabled for Tether MCP\n")
	return nil
}

// configureAgyPermissions ensures tether tools are permitted in ~/.gemini/antigravity-cli/settings.json
func configureAgyPermissions(home string) error {
	settingsPath := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	content, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(content, &settings); err != nil {
		return err
	}

	perms, ok := settings["permissions"].(map[string]interface{})
	if !ok {
		perms = make(map[string]interface{})
		settings["permissions"] = perms
	}

	// 1. Remove any tether entries from deny list
	if denyList, ok := perms["deny"].([]interface{}); ok {
		var newDeny []interface{}
		for _, item := range denyList {
			str, ok := item.(string)
			if ok && strings.Contains(str, "tether") {
				continue // remove tether denials
			}
			newDeny = append(newDeny, item)
		}
		perms["deny"] = newDeny
	}

	// 2. Add tether permissions to allow list
	tetherTools := []string{
		"mcp(tether/*)",
		"mcp(tether/workspace_info)",
		"mcp(tether/session_load)",
		"mcp(tether/session_save)",
		"mcp(tether/session_done)",
		"mcp(tether/setting_set)",
		"mcp(tether/decision_add)",
		"mcp(tether/pattern_add)",
		"mcp(tether/mode_set)",
		"mcp(tether/mode_get)",
		"mcp(tether/kb_index)",
		"mcp(tether/kb_search)",
		"mcp(tether/kb_status)",
		"mcp(tether/todo_add)",
		"mcp(tether/todo_list)",
		"mcp(tether/todo_complete)",
		"mcp(tether/todo_remove)",
		"mcp(tether/task_classify)",
		"mcp(tether/files_modified)",
		"mcp(tether/subagent_spawn_rules)",
		"mcp(tether/sql_query)",
	}

	allowMap := make(map[string]bool)
	var allowList []interface{}
	if existingAllow, ok := perms["allow"].([]interface{}); ok {
		allowList = existingAllow
		for _, item := range existingAllow {
			if str, ok := item.(string); ok {
				allowMap[str] = true
			}
		}
	}

	for _, tool := range tetherTools {
		if !allowMap[tool] {
			allowList = append(allowList, tool)
			allowMap[tool] = true
		}
	}
	perms["allow"] = allowList

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath, out, 0644)
}

// --- Claude Code ---

func installClaude() error {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("mkdir .claude: %w", err)
	}

	mcpConfig := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"tether": map[string]interface{}{
				"command": tetherBinary(),
				"args":    []string{"mcp"},
			},
		},
	}

	mcpPath := filepath.Join(configDir, "mcp_config.json")
	if err := writeJSON(mcpPath, mcpConfig); err != nil {
		return fmt.Errorf("write mcp_config.json: %w", err)
	}

	// Claude hooks
	hooksConfig := map[string]interface{}{
		"hooks": map[string]interface{}{
			"PreToolUse": []map[string]interface{}{
				{
					"matcher": "write_to_file|Edit|MultiEdit|Write",
					"hooks": []map[string]interface{}{
						{"command": fmt.Sprintf("%s hook PreToolUse", tetherBinary())},
					},
				},
			},
		},
	}

	hooksPath := filepath.Join(configDir, "hooks.json")
	if err := writeJSON(hooksPath, hooksConfig); err != nil {
		return fmt.Errorf("write hooks.json: %w", err)
	}

	fmt.Printf("  → MCP server configured: %s\n", mcpPath)
	fmt.Printf("  → Lifecycle hooks configured: %s\n", hooksPath)
	return nil
}

// --- Codex ---

func installCodex() error {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("mkdir .codex: %w", err)
	}

	mcpConfig := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"tether": map[string]interface{}{
				"command": tetherBinary(),
				"args":    []string{"mcp"},
			},
		},
	}

	mcpPath := filepath.Join(configDir, "mcp_config.json")
	if err := writeJSON(mcpPath, mcpConfig); err != nil {
		return fmt.Errorf("write mcp_config.json: %w", err)
	}

	fmt.Printf("  → MCP server configured: %s\n", mcpPath)
	return nil
}

// --- Cursor ---

func installCursor() error {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("mkdir .cursor: %w", err)
	}

	mcpConfig := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"tether": map[string]interface{}{
				"command": tetherBinary(),
				"args":    []string{"mcp"},
			},
		},
	}

	mcpPath := filepath.Join(configDir, "mcp.json")
	if err := writeJSON(mcpPath, mcpConfig); err != nil {
		return fmt.Errorf("write mcp.json: %w", err)
	}

	fmt.Printf("  → MCP server configured: %s\n", mcpPath)
	return nil
}

// --- Helpers ---

func writeJSON(path string, data interface{}) error {
	existing := make(map[string]interface{})
	if content, err := os.ReadFile(path); err == nil && len(content) > 0 {
		json.Unmarshal(content, &existing)
	}

	// Clean up legacy harness entries
	delete(existing, "harness")
	if servers, ok := existing["mcpServers"].(map[string]interface{}); ok {
		delete(servers, "harness")
	}

	newData, ok := data.(map[string]interface{})
	if ok {
		for k, v := range newData {
			existing[k] = v
		}
	}

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// GetInstalledAgent detects which agent is currently running via parent process name
func GetInstalledAgent() string {
	ppid := os.Getppid()
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", ppid))
	if err != nil {
		return "unknown"
	}
	name := strings.TrimSpace(string(comm))
	switch name {
	case "agy", "antigravity":
		return "agy"
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "cursor":
		return "cursor"
	}
	return "unknown"
}
