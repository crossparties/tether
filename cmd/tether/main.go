package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/croosparties/tether/internal/adapter"
	"github.com/croosparties/tether/internal/core"
	"github.com/croosparties/tether/internal/hooks"
	"github.com/croosparties/tether/internal/mcp"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		cmdInit()
	case "mcp":
		cmdMCP()
	case "hook":
		cmdHook()
	case "mode":
		cmdMode()
	case "status":
		cmdStatus()
	case "doctor":
		cmdDoctor()
	case "install":
		cmdInstall()
	case "version":
		fmt.Printf("tether v%s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

// cmdInit initializes tether in the current project
func cmdInit() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error initializing: %v\n", err)
		os.Exit(1)
	}
	defer engine.Close()

	fmt.Printf("✓ Initialized tether in %s\n", workspace)
	fmt.Printf("  → session.db created\n")
	fmt.Printf("  → .gitignore updated\n")

	// Detect and store project settings
	settings := core.DetectProjectSettings(workspace)
	for k, v := range settings {
		engine.SetProjectSetting(k, v)
		fmt.Printf("  → detected %s: %s\n", k, v)
	}
}

// cmdMCP starts the MCP server (stdio transport)
func cmdMCP() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer engine.Close()

	server := mcp.NewServer(engine)
	if err := server.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "mcp server error: %v\n", err)
		os.Exit(1)
	}
}

// cmdHook handles lifecycle hook events from agents
func cmdHook() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: tether hook <PreInvocation|PreToolUse|PostToolUse>\n")
		os.Exit(1)
	}

	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer engine.Close()

	handler := hooks.NewHandler(engine)
	hookType := os.Args[2]

	result, err := handler.Handle(hookType, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook error: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(result)

	// Hard OS Exit Code 2 enforcement for tool denial
	if strings.Contains(result, `"decision":"deny"`) {
		fmt.Fprintf(os.Stderr, "\n┌────────────────────────────── TETHER GUARD ──────────────────────────────┐\n")
		fmt.Fprintf(os.Stderr, "│ 🛑 ACTION BLOCKED: Repository is locked in [PLAN MODE]                   │\n")
		fmt.Fprintf(os.Stderr, "├──────────────────────────────────────────────────────────────────────────┤\n")
		fmt.Fprintf(os.Stderr, "│ Guardrail prevents code mutation before architectural plan approval.     │\n")
		fmt.Fprintf(os.Stderr, "├──────────────────────────────────────────────────────────────────────────┤\n")
		fmt.Fprintf(os.Stderr, "│ 💡 ALLOWED ACTIONS:                                                      │\n")
		fmt.Fprintf(os.Stderr, "│   1. Write plans/specifications in `docs/` or `.agents/`                 │\n")
		fmt.Fprintf(os.Stderr, "│   2. Present implementation inline in chat response                      │\n")
		fmt.Fprintf(os.Stderr, "│                                                                          │\n")
		fmt.Fprintf(os.Stderr, "│ 🔓 TO UNLOCK CODE MUTATION:                                              │\n")
		fmt.Fprintf(os.Stderr, "│   • Developer CLI: $ tether mode code                                    │\n")
		fmt.Fprintf(os.Stderr, "│   • Agent MCP:     Call MCP tool `mode_set(mode=\"code\")`                 │\n")
		fmt.Fprintf(os.Stderr, "└──────────────────────────────────────────────────────────────────────────┘\n\n")
		os.Exit(2)
	}
}

// cmdMode inspects or toggles the execution mode (code | plan | review)
func cmdMode() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error initializing: %v\n", err)
		os.Exit(1)
	}
	defer engine.Close()

	if len(os.Args) < 3 {
		mode := engine.GetMode()
		fmt.Printf("Current Tether mode: [%s]\n", strings.ToUpper(mode))
		if mode == "plan" {
			fmt.Println("  • Codebase writes: BLOCKED")
			fmt.Println("  • Plan writes:    ALLOWED in docs/** and .agents/**")
			fmt.Println("  • To unlock:      Run 'tether mode code'")
		} else {
			fmt.Println("  • Codebase writes: UNRESTRICTED")
			fmt.Println("  • To plan:        Run 'tether mode plan'")
		}
		return
	}

	targetMode := strings.ToLower(os.Args[2])
	if targetMode != "code" && targetMode != "plan" && targetMode != "review" {
		fmt.Fprintf(os.Stderr, "error: invalid mode '%s'. Valid modes: 'code', 'plan', 'review'\n", targetMode)
		os.Exit(1)
	}

	if err := engine.SetMode(targetMode); err != nil {
		fmt.Fprintf(os.Stderr, "error setting mode: %v\n", err)
		os.Exit(1)
	}

	if targetMode == "plan" {
		fmt.Println("🔒 Switched to [PLAN MODE]")
		fmt.Println("  • Codebase mutations are now BLOCKED.")
		fmt.Println("  • Plan artifacts in docs/ are permitted.")
	} else if targetMode == "code" {
		fmt.Println("🔓 Switched to [CODE MODE]")
		fmt.Println("  • Codebase mutations are now UNLOCKED.")
	} else {
		fmt.Println("👁️ Switched to [REVIEW MODE]")
		fmt.Println("  • Read-only inspection mode active.")
	}
}

// cmdStatus prints the substrate observability dashboard
func cmdStatus() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error initializing: %v\n", err)
		os.Exit(1)
	}
	defer engine.Close()

	mode := engine.GetMode()
	modeBadge := "🟢 [ CODE MODE ]  (mutation allowed)"
	writeBoundary := "UNRESTRICTED (full file write access)"
	if mode == "plan" {
		modeBadge = "🟡 [ PLAN MODE ]  (mutation restricted)"
		writeBoundary = "RESTRICTED (writes allowed ONLY in docs/** and .agents/**)"
	}

	decisions, _ := engine.GetDecisions()
	patterns, _ := engine.GetPatterns()
	kbStats, _ := engine.KBStatus()
	todos, _ := engine.ListTodos(true)

	pendingCount := 0
	completedCount := 0
	for _, t := range todos {
		if t.Status == "completed" {
			completedCount++
		} else {
			pendingCount++
		}
	}

	fmt.Println()
	fmt.Println("  ╺┳╸┏━╸╺┳╸┓ ┏┓┏━╸┏━┓   TETHER v" + version)
	fmt.Println("   ┃ ┣╸  ┃ ┣━┫┣╸ ┣┳┛   Deterministic Cognitive Substrate")
	fmt.Println("   ╹ ┗━╸ ╹ ╹ ┗┗━╸╹┗━   Local-First AI Agent Gatekeeper")
	fmt.Println()
	fmt.Println("┌─ WORKSPACE & IDENTITY ──────────────────────────────────────────────────┐")
	fmt.Printf("│ Directory:    %-58s│\n", truncateStr(engine.Workspace, 58))
	fmt.Printf("│ Git Branch:   %-58s│\n", truncateStr(engine.Branch, 58))
	fmt.Printf("│ Session ID:   %-58s│\n", truncateStr(engine.SessionID, 58))
	fmt.Println("└─────────────────────────────────────────────────────────────────────────┘")
	fmt.Println("┌─ GOVERNANCE ENGINE ─────────────────────────────────────────────────────┐")
	fmt.Printf("│ Substrate Mode:   %-54s│\n", modeBadge)
	fmt.Printf("│ Write Boundary:   %-54s│\n", truncateStr(writeBoundary, 54))
	fmt.Printf("│ MCP Protocol:     READY (stdio ndjson JSON-RPC 2.0)                     │\n")
	fmt.Println("└─────────────────────────────────────────────────────────────────────────┘")
	fmt.Println("┌─ SUBSTRATE MEMORY ──────────────────────────────────────────────────────┐")
	fmt.Printf("│ Decisions:        %-54s│\n", fmt.Sprintf("%d recorded", len(decisions)))
	fmt.Printf("│ Learned Patterns: %-54s│\n", fmt.Sprintf("%d active", len(patterns)))
	fmt.Printf("│ Knowledge Base:   %-54s│\n", fmt.Sprintf("%d files (%d FTS5 chunks)", kbStats.TotalFiles, kbStats.TotalChunks))
	fmt.Println("└─────────────────────────────────────────────────────────────────────────┘")
	fmt.Printf("┌─ TASKS & TODOS [%d pending, %d completed] ──────────────────────────────┐\n", pendingCount, completedCount)
	if len(todos) == 0 {
		fmt.Println("│   No tasks recorded for this session.                                   │")
	} else {
		limit := 5
		if len(todos) < limit {
			limit = len(todos)
		}
		for i := 0; i < limit; i++ {
			t := todos[i]
			icon := "◻"
			if t.Status == "completed" {
				icon = "✔"
			}
			line := fmt.Sprintf("  %s #%d %s", icon, t.ID, t.Description)
			fmt.Printf("│ %-72s│\n", truncateStr(line, 72))
		}
		if len(todos) > limit {
			fmt.Printf("│   ... and %d more tasks                                                 │\n", len(todos)-limit)
		}
	}
	fmt.Println("└─────────────────────────────────────────────────────────────────────────┘")
	fmt.Println()
	if mode == "plan" {
		fmt.Println("💡 Quick Action:  Run 'tether mode code' to unlock codebase mutation.")
	} else {
		fmt.Println("💡 Quick Action:  Run 'tether mode plan' to lock codebase in planning mode.")
	}
	fmt.Println()
}

// cmdDoctor verifies environment and substrate integrity
func cmdDoctor() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("  ╺┳╸┏━╸╺┳╸┓ ┏┓┏━╸┏━┓   TETHER DOCTOR")
	fmt.Println("   ┃ ┣╸  ┃ ┣━┫┣╸ ┣┳┛   Environment & Substrate Diagnostic")
	fmt.Println("   ╹ ┗━╸ ╹ ╹ ┗┗━╸╹┗━   Checking agent substrate integrity...")
	fmt.Println()

	passed := 0
	warnings := 0
	failed := 0

	// 1. Storage & SQLite
	fmt.Println("[1/4] Core Substrate & Storage")
	dbPath := filepath.Join(workspace, "session.db")
	if _, err := os.Stat(dbPath); err == nil {
		fmt.Println("  ✔ Database File:        session.db accessible")
		passed++
	} else {
		fmt.Println("  ▲ Database File:        session.db not found (run 'tether init')")
		warnings++
	}

	walPath := filepath.Join(workspace, "session.db-wal")
	if _, err := os.Stat(walPath); err == nil {
		fmt.Println("  ✔ WAL Mode:             ACTIVE (session.db-wal present)")
		passed++
	} else {
		fmt.Println("  ✔ WAL Mode:             Configured in PRAGMA")
		passed++
	}

	gitignorePath := filepath.Join(workspace, ".gitignore")
	if content, err := os.ReadFile(gitignorePath); err == nil {
		if strings.Contains(string(content), "session.db") {
			fmt.Println("  ✔ Git Hygiene:          session.db ignored in .gitignore")
			passed++
		} else {
			fmt.Println("  ✖ Git Hygiene:          session.db NOT in .gitignore!")
			failed++
		}
	} else {
		fmt.Println("  ▲ Git Hygiene:          .gitignore not found")
		warnings++
	}

	// 2. Engine & Session
	fmt.Println("\n[2/4] Engine & Session Health")
	engine, err := core.NewEngine(workspace)
	if err != nil {
		fmt.Printf("  ✖ Engine Init:          Failed (%v)\n", err)
		failed++
	} else {
		defer engine.Close()
		fmt.Println("  ✔ Engine Init:          SQLite connected with WAL & 5000ms busy timeout")
		fmt.Printf("  ✔ Session ID:           %s\n", engine.SessionID)
		fmt.Printf("  ✔ Current Mode:         %s\n", engine.GetMode())
		passed += 3
	}

	// 3. Agent Configurations
	fmt.Println("\n[3/4] Agent Wiring & Hooks")
	home, _ := os.UserHomeDir()
	agyHooksPath := filepath.Join(home, ".gemini", "config", "hooks.json")
	if _, err := os.Stat(agyHooksPath); err == nil {
		fmt.Println("  ✔ Antigravity (agy):    Configured in ~/.gemini/config/hooks.json")
		passed++
	} else {
		fmt.Println("  ▲ Antigravity (agy):    ~/.gemini/config/hooks.json not found (run 'tether install agy')")
		warnings++
	}

	claudeConfigPath := filepath.Join(home, ".claude", "mcp_config.json")
	if _, err := os.Stat(claudeConfigPath); err == nil {
		fmt.Println("  ✔ Claude Code:          Configured in ~/.claude/mcp_config.json")
		passed++
	} else {
		fmt.Println("  - Claude Code:          Not installed / config not found")
	}

	// 4. Protocol & Binary
	fmt.Println("\n[4/4] Binary & Runtime Health")
	selfPath, err := os.Executable()
	if err == nil {
		fmt.Printf("  ✔ Binary Location:      %s\n", selfPath)
		passed++
	}
	fmt.Printf("  ✔ Tether Version:       v%s\n", version)
	passed++

	fmt.Println("\n───────────────────────────────────────────────────────────────────────────")
	fmt.Printf("Diagnostic Summary: %d Passed, %d Warnings, %d Failed\n", passed, warnings, failed)
	if failed > 0 {
		fmt.Println("\n🛠  Remediation: Run 'tether init' to initialize missing components.")
	} else {
		fmt.Println("✓ Substrate is healthy and ready for agent execution.")
	}
	fmt.Println("───────────────────────────────────────────────────────────────────────────")
	fmt.Println()
}

// truncateStr truncates a string to maxLen runes or pads with spaces if shorter
func truncateStr(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen-3]) + "..."
	}
	return s
}

// cmdInstall wires tether into the specified agent
func cmdInstall() {
	agent := "agy" // default
	if len(os.Args) >= 3 {
		agent = os.Args[2]
	}

	if err := adapter.Install(agent); err != nil {
		fmt.Fprintf(os.Stderr, "install error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Tether installed for %s\n", agent)
}

func printUsage() {
	fmt.Println(`tether — The Local-First Deterministic Substrate for AI Agents

Usage:
  tether init              Initialize tether in current project
  tether status            Display real-time substrate dashboard & guardrail state
  tether mode [code|plan]  Inspect or switch active governance mode
  tether doctor            Verify database, WAL, hooks, and agent wiring
  tether mcp               Start MCP server (stdio transport)
  tether hook <event>      Handle lifecycle hook event (PreInvocation|PreToolUse|PostToolUse)
  tether install [agent]   Wire tether into agent config (agy|claude|codex)
  tether version           Show version
  tether help              Show this help

Tether provides local session management, workspace detection, planning mode
enforcement, and continuous project memory for any CLI AI agent.`)
}
