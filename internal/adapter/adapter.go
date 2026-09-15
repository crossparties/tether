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

	// Create hooks config
	hooksConfig := map[string]interface{}{
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
					"matcher": "write_to_file|replace_file_content|multi_replace_file_content|write_file",
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

	fmt.Printf("  → MCP server configured: %s\n", mcpPath)
	fmt.Printf("  → Lifecycle hooks configured: %s\n", hooksPath)
	return nil
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
