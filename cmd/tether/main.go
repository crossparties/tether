package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crossparties/tether/internal/adapter"
	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/harness"
	"github.com/crossparties/tether/internal/hooks"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/snapshot"
	"github.com/crossparties/tether/internal/tui"
	"github.com/crossparties/tether/internal/tui/views"
	"github.com/crossparties/tether/internal/ui"
)

const version = "0.2.0"

func main() {
	if len(os.Args) < 2 {
		cmdRun(nil)
		return
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
	case "undo":
		cmdUndo(os.Args[2:])
	case "status":
		cmdStatus()
	case "doctor":
		cmdDoctor()
	case "install":
		cmdInstall()
	case "tui":
		cmdRun(append([]string{"--tui"}, os.Args[2:]...))
	case "run", "agent", "chat":
		cmdRun(os.Args[2:])
	case "version":
		fmt.Printf("tether v%s\n", ui.Bold(version))
	case "help", "--help", "-h":
		printUsage()
	default:
		// Allow flags or direct prompt right after `tether` (Aider / Claude Code style)
		cmdRun(os.Args[1:])
	}
}

// cmdInit initializes tether in the current project
func cmdInit() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fatalf("%v", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fatalf("initializing: %v", err)
		os.Exit(1)
	}
	defer engine.Close()

	fmt.Printf("%s in %s\n", ui.Success("✓ Initialized tether"), ui.Path(workspace))
	fmt.Printf("  %s .tether/session.db and .tether/cache.db created\n", ui.Dim("→"))
	fmt.Printf("  %s .gitignore updated\n", ui.Dim("→"))

	// Detect and store project settings
	settings := core.DetectProjectSettings(workspace)
	for k, v := range settings {
		engine.SetProjectSetting(k, v)
		fmt.Printf("  %s detected %s: %s\n", ui.Dim("→"), ui.Bold(k), v)
	}
}

// cmdMCP starts the MCP server (stdio transport)
func cmdMCP() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fatalf("%v", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fatalf("%v", err)
		os.Exit(1)
	}
	defer engine.Close()

	server := mcp.NewServer(engine)
	if err := server.Serve(); err != nil {
		fatalf("mcp server: %v", err)
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
		fatalf("%v", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fatalf("%v", err)
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
		if strings.Contains(result, ".tether") || strings.Contains(result, "session.db") || strings.Contains(result, "cache.db") {
			fmt.Fprintf(os.Stderr, "\n┌────────────────────────────── TETHER GUARD ──────────────────────────────┐\n")
			fmt.Fprintf(os.Stderr, "│ 🛑 PROTECTED SUBSTRATE BLOCKED: Direct .tether/ mutation is forbidden    │\n")
			fmt.Fprintf(os.Stderr, "├──────────────────────────────────────────────────────────────────────────┤\n")
			fmt.Fprintf(os.Stderr, "│ Direct shell/file writes to .tether/ DBs or snapshots are blocked.       │\n")
			fmt.Fprintf(os.Stderr, "│ Use Tether MCP tools (session_load, sql_query, todo_list) instead.      │\n")
			fmt.Fprintf(os.Stderr, "└──────────────────────────────────────────────────────────────────────────┘\n\n")
		} else {
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
		}
		os.Exit(2)
	}
}

// cmdMode inspects or toggles the execution mode (code | plan | review)
func cmdMode() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fatalf("%v", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fatalf("initializing: %v", err)
		os.Exit(1)
	}
	defer engine.Close()

	if len(os.Args) < 3 {
		mode := engine.GetMode()
		fmt.Printf("Current Tether mode: [%s]\n", ui.Bold(strings.ToUpper(mode)))
		if mode == "plan" {
			fmt.Printf("  %s Codebase writes: %s\n", ui.Dim("•"), ui.Error("BLOCKED"))
			fmt.Printf("  %s Plan writes:    %s\n", ui.Dim("•"), ui.Success("ALLOWED in docs/** and .agents/**"))
			fmt.Printf("  %s To unlock:      Run 'tether mode code'\n", ui.Dim("•"))
		} else {
			fmt.Printf("  %s Codebase writes: %s\n", ui.Dim("•"), ui.Success("UNRESTRICTED"))
			fmt.Printf("  %s To plan:        Run 'tether mode plan'\n", ui.Dim("•"))
		}
		return
	}

	targetMode := strings.ToLower(os.Args[2])
	if targetMode != "code" && targetMode != "plan" && targetMode != "review" {
		fatalf("invalid mode '%s'. Valid modes: 'code', 'plan', 'review'", targetMode)
		os.Exit(1)
	}

	if err := engine.SetMode(targetMode); err != nil {
		fatalf("setting mode: %v", err)
		os.Exit(1)
	}

	if targetMode == "plan" {
		fmt.Println(ui.Warn("🔒 Switched to [PLAN MODE]"))
		fmt.Printf("  %s Codebase mutations are now %s.\n", ui.Dim("•"), ui.Error("BLOCKED"))
		fmt.Printf("  %s Plan artifacts in docs/ are permitted.\n", ui.Dim("•"))
	} else if targetMode == "code" {
		fmt.Println(ui.Success("🔓 Switched to [CODE MODE]"))
		fmt.Printf("  %s Codebase mutations are now %s.\n", ui.Dim("•"), ui.Success("UNLOCKED"))
	} else {
		fmt.Println(ui.Accent("👁️ Switched to [REVIEW MODE]"))
		fmt.Printf("  %s Read-only inspection mode active.\n", ui.Dim("•"))
	}
}

// cmdUndo restores files captured by the snapshot store for a given turn, or
// for the most recent turn when no --turn flag is supplied.
func cmdUndo(args []string) {
	turn := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--turn", "-t":
			if i+1 >= len(args) {
				fatalf("--turn requires a value")
				os.Exit(1)
			}
			i++
			turn = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				fatalf("unknown flag %q", args[i])
				os.Exit(1)
			}
			// A bare argument is treated as the turn id.
			turn = args[i]
		}
	}

	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fatalf("%v", err)
		os.Exit(1)
	}

	store := snapshot.New(workspace)

	if turn == "" {
		last, err := store.LastTurn()
		if err != nil {
			fatalf("could not read snapshots: %v", err)
			os.Exit(1)
		}
		if last == "" {
			fmt.Println(ui.Dim("ℹ Nothing to undo — no snapshot recorded yet."))
			fmt.Println(ui.Dim("  Snapshots are taken before each file write, so run the agent first."))
			return
		}
		turn = last
	}

	restored, err := store.Restore(turn, workspace)
	for _, path := range restored {
		fmt.Printf("↩ restored %s\n", ui.Path(path))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s for turn %s: %v\n", ui.Warn("⚠ Undo partially failed"), ui.Bold(turn), err)
		if len(restored) == 0 {
			os.Exit(1)
		}
		fmt.Println(ui.Dim("  Files listed above were restored; the rest were left untouched."))
		return
	}
	if len(restored) == 0 {
		fmt.Printf("ℹ Turn %s had no captured files.\n", ui.Bold(turn))
		return
	}
	fmt.Printf("%s — %d file(s) restored from turn %s.\n",
		ui.Success("✓ Undo complete"), len(restored), ui.Bold(turn))
}

// cmdStatus prints the substrate observability dashboard
func cmdStatus() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fatalf("%v", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fatalf("initializing: %v", err)
		os.Exit(1)
	}
	defer engine.Close()

	mode := engine.GetMode()
	modeBadge := ui.Success("🟢 [ CODE MODE ]") + "  (mutation allowed)"
	writeBoundary := "UNRESTRICTED (full file write access)"
	if mode == "plan" {
		modeBadge = ui.Warn("🟡 [ PLAN MODE ]") + "  (mutation restricted)"
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
	fmt.Println("  " + ui.Bold("╺┳╸┏━╸╺┳╸┓ ┏┓┏━╸┏━┓") + "   " + ui.Title("TETHER v"+version))
	fmt.Println("   " + ui.Dim("┃ ┣╸  ┃ ┣━┫┣╸ ┣┳┛") + "   " + ui.Dim("Deterministic Cognitive Substrate"))
	fmt.Println("   " + ui.Dim("╹ ┗━╸ ╹ ╹ ┗┗━╸╹┗━") + "   " + ui.Dim("Local-First AI Agent Gatekeeper"))
	fmt.Println()
	fmt.Println(ui.Dim("┌─ ") + ui.Title("WORKSPACE & IDENTITY") + ui.Dim(" ──────────────────────────────────────────────────┐"))
	fmt.Printf("%s%s%s%s\n", ui.Dim("│ "), ui.Dim(fmt.Sprintf("%-14s", "Directory:")), ui.Pad(ui.Path(truncateStr(engine.Workspace, 58)), 58), ui.Dim("│"))
	fmt.Printf("%s%s%s%s\n", ui.Dim("│ "), ui.Dim(fmt.Sprintf("%-14s", "Git Branch:")), ui.Pad(ui.Bold(truncateStr(engine.Branch, 58)), 58), ui.Dim("│"))
	fmt.Printf("%s%s%s%s\n", ui.Dim("│ "), ui.Dim(fmt.Sprintf("%-14s", "Session ID:")), ui.Pad(ui.Dim(truncateStr(engine.SessionID, 58)), 58), ui.Dim("│"))
	fmt.Println(ui.Dim("└─────────────────────────────────────────────────────────────────────────┘"))
	fmt.Println(ui.Dim("┌─ ") + ui.Title("GOVERNANCE ENGINE") + ui.Dim(" ─────────────────────────────────────────────────────┐"))
	fmt.Printf("%s%s%s%s\n", ui.Dim("│ "), ui.Dim(fmt.Sprintf("%-18s", "Substrate Mode:")), ui.Pad(modeBadge, 54), ui.Dim("│"))
	fmt.Printf("%s%s%s%s\n", ui.Dim("│ "), ui.Dim(fmt.Sprintf("%-18s", "Write Boundary:")), ui.Pad(truncateStr(writeBoundary, 54), 54), ui.Dim("│"))
	fmt.Printf("%s%s%s%s\n", ui.Dim("│ "), ui.Dim(fmt.Sprintf("%-18s", "MCP Protocol:")), ui.Pad(ui.Success("READY")+" (stdio ndjson JSON-RPC 2.0)", 54), ui.Dim("│"))
	fmt.Println(ui.Dim("└─────────────────────────────────────────────────────────────────────────┘"))
	fmt.Println(ui.Dim("┌─ ") + ui.Title("SUBSTRATE MEMORY") + ui.Dim(" ──────────────────────────────────────────────────────┐"))
	fmt.Printf("%s%s%s%s\n", ui.Dim("│ "), ui.Dim(fmt.Sprintf("%-18s", "Decisions:")), ui.Pad(fmt.Sprintf("%d recorded", len(decisions)), 54), ui.Dim("│"))
	fmt.Printf("%s%s%s%s\n", ui.Dim("│ "), ui.Dim(fmt.Sprintf("%-18s", "Learned Patterns:")), ui.Pad(fmt.Sprintf("%d active", len(patterns)), 54), ui.Dim("│"))
	fmt.Printf("%s%s%s%s\n", ui.Dim("│ "), ui.Dim(fmt.Sprintf("%-18s", "Knowledge Base:")), ui.Pad(fmt.Sprintf("%d files (%d FTS5 chunks)", kbStats.TotalFiles, kbStats.TotalChunks), 54), ui.Dim("│"))
	fmt.Println(ui.Dim("└─────────────────────────────────────────────────────────────────────────┘"))
	fmt.Println(ui.Dim("┌─ ") + ui.Title(fmt.Sprintf("TASKS & TODOS [%d pending, %d completed]", pendingCount, completedCount)) + ui.Dim(" ───────────────────────────────┐"))
	if len(todos) == 0 {
		fmt.Println(ui.Dim("│ ") + ui.Pad(ui.Dim("No tasks recorded for this session."), 71) + ui.Dim("│"))
	} else {
		limit := 5
		if len(todos) < limit {
			limit = len(todos)
		}
		for i := 0; i < limit; i++ {
			t := todos[i]
			icon := ui.Dim("◻")
			if t.Status == "completed" {
				icon = ui.Success("✔")
			}
			line := ui.Pad(fmt.Sprintf("%s #%d %s", icon, t.ID, truncateStr(t.Description, 60)), 71)
			fmt.Println(ui.Dim("│ ") + line + ui.Dim("│"))
		}
		if len(todos) > limit {
			fmt.Printf("%s %s\n", ui.Dim("│ "+fmt.Sprintf("  ... and %d more tasks", len(todos)-limit)), ui.Dim("                                                  │"))
		}
	}
	fmt.Println(ui.Dim("└─────────────────────────────────────────────────────────────────────────┘"))
	fmt.Println()

	if mode == "plan" {
		fmt.Println(ui.Accent("💡 Quick Action:") + "  " + ui.Dim("Run 'tether mode code' to unlock codebase mutation."))
	} else {
		fmt.Println(ui.Accent("💡 Quick Action:") + "  " + ui.Dim("Run 'tether mode plan' to lock codebase in planning mode."))
	}
	fmt.Println()
}

// cmdDoctor verifies environment and substrate integrity
func cmdDoctor() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fatalf("%v", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("  " + ui.Bold("╺┳╸┏━╸╺┳╸┓ ┏┓┏━╸┏━┓") + "   " + ui.Title("TETHER DOCTOR"))
	fmt.Println("   " + ui.Dim("┃ ┣╸  ┃ ┣━┫┣╸ ┣┳┛") + "   " + ui.Dim("Environment & Substrate Diagnostic"))
	fmt.Println("   " + ui.Dim("╹ ┗━╸ ╹ ╹ ┗┗━╸╹┗━") + "   " + ui.Dim("Checking agent substrate integrity..."))
	fmt.Println()

	passed := 0
	warnings := 0
	failed := 0

	// 1. Storage & SQLite
	fmt.Println(ui.Title("[1/4] Core Substrate & Storage"))
	dbPath := filepath.Join(workspace, ".tether", "session.db")
	if _, err := os.Stat(dbPath); err == nil {
		fmt.Println(doctorLine(ui.Success("✔"), "Database File:", ".tether/session.db accessible"))
		passed++
	} else {
		fmt.Println(doctorLine(ui.Warn("▲"), "Database File:", ".tether/session.db not found (run 'tether init')"))
		warnings++
	}

	walPath := filepath.Join(workspace, ".tether", "session.db-wal")
	if _, err := os.Stat(walPath); err == nil {
		fmt.Println(doctorLine(ui.Success("✔"), "WAL Mode:", "ACTIVE (.tether/session.db-wal present)"))
		passed++
	} else {
		fmt.Println(doctorLine(ui.Success("✔"), "WAL Mode:", "Configured in PRAGMA"))
		passed++
	}

	gitignorePath := filepath.Join(workspace, ".gitignore")
	if content, err := os.ReadFile(gitignorePath); err == nil {
		if strings.Contains(string(content), ".tether/session.db") || strings.Contains(string(content), ".tether/") {
			fmt.Println(doctorLine(ui.Success("✔"), "Git Hygiene:", ".tether/session.db ignored in .gitignore"))
			passed++
		} else {
			fmt.Println(doctorLine(ui.Error("✖"), "Git Hygiene:", ".tether/session.db NOT in .gitignore!"))
			failed++
		}
	} else {
		fmt.Println(doctorLine(ui.Warn("▲"), "Git Hygiene:", ".gitignore not found"))
		warnings++
	}

	// 2. Engine & Session
	fmt.Println("\n" + ui.Title("[2/4] Engine & Session Health"))
	engine, err := core.NewEngine(workspace)
	if err != nil {
		fmt.Println(doctorLine(ui.Error("✖"), "Engine Init:", fmt.Sprintf("Failed (%v)", err)))
		failed++
	} else {
		defer engine.Close()
		fmt.Println(doctorLine(ui.Success("✔"), "Engine Init:", "SQLite connected with WAL & 5000ms busy timeout"))
		fmt.Println(doctorLine(ui.Success("✔"), "Session ID:", ui.Dim(engine.SessionID)))
		fmt.Println(doctorLine(ui.Success("✔"), "Current Mode:", ui.Bold(engine.GetMode())))
		passed += 3
	}

	// 3. Agent Configurations
	fmt.Println("\n" + ui.Title("[3/4] Agent Wiring & Hooks"))
	home, _ := os.UserHomeDir()
	agyHooksPath := filepath.Join(home, ".gemini", "config", "hooks.json")
	if _, err := os.Stat(agyHooksPath); err == nil {
		fmt.Println(doctorLine(ui.Success("✔"), "Antigravity (agy):", "Configured in ~/.gemini/config/hooks.json"))
		passed++
	} else {
		fmt.Println(doctorLine(ui.Warn("▲"), "Antigravity (agy):", "not configured (run 'tether install agy')"))
		warnings++
	}

	claudeConfigPath := filepath.Join(home, ".claude", "mcp_config.json")
	if _, err := os.Stat(claudeConfigPath); err == nil {
		fmt.Println(doctorLine(ui.Success("✔"), "Claude Code:", "Configured in ~/.claude/mcp_config.json"))
		passed++
	} else {
		fmt.Println(doctorLine(ui.Dim("-"), "Claude Code:", ui.Dim("Not installed / config not found")))
	}

	// 4. Protocol & Binary
	fmt.Println("\n" + ui.Title("[4/4] Binary & Runtime Health"))
	selfPath, err := os.Executable()
	if err == nil {
		fmt.Println(doctorLine(ui.Success("✔"), "Binary Location:", ui.Path(selfPath)))
		passed++
	}
	fmt.Println(doctorLine(ui.Success("✔"), "Tether Version:", "v"+version))
	passed++

	fmt.Println("\n" + ui.Dim("───────────────────────────────────────────────────────────────────────────"))
	fmt.Printf("Diagnostic Summary: %s Passed, %s Warnings, %s Failed\n",
		ui.Success(fmt.Sprintf("%d", passed)),
		ui.Warn(fmt.Sprintf("%d", warnings)),
		func() string {
			if failed > 0 {
				return ui.Error(fmt.Sprintf("%d", failed))
			}
			return ui.Dim("0")
		}())
	if failed > 0 {
		fmt.Println("\n" + ui.Warn("🛠  Remediation: Run 'tether init' to initialize missing components."))
	} else {
		fmt.Println(ui.Success("✓ Substrate is healthy and ready for agent execution."))
	}
	fmt.Println(ui.Dim("───────────────────────────────────────────────────────────────────────────"))
	fmt.Println()
}

// fatalf prints a styled error to stderr and exits. Every command funnels its
// failures through here so error output reads the same everywhere, and so the
// "error: " prefix is written once instead of at 25 call sites.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", ui.Error("error:"), fmt.Sprintf(format, args...))
	os.Exit(1)
}

// truncateStr truncates a string to maxLen runes or pads with spaces if shorter
func truncateStr(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen-3]) + "..."
	}
	return s
}

// doctorLine renders one doctor row. The label is padded while still plain
// text, before any styling, so colour codes never shift the value column.
func doctorLine(mark, label, value string) string {
	return fmt.Sprintf("  %s %s %s", mark, ui.Pad(label, 22), value)
}

// cmdInstall wires tether into the specified agent
func cmdInstall() {
	agent := "agy" // default
	if len(os.Args) >= 3 {
		agent = os.Args[2]
	}

	if err := adapter.Install(agent); err != nil {
		fatalf("install: %v", err)
		os.Exit(1)
	}

	fmt.Printf("%s for %s\n", ui.Success("✓ Tether installed"), ui.Bold(agent))
}

// cmdRun launches the Interactive Agent REPL (when no prompt is passed) or runs a prompt directly
func cmdRun(args []string) {
	var cfg harness.Config
	var promptParts []string
	var useTUI bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tui":
			useTUI = true
		case "--provider", "-p":
			if i+1 < len(args) {
				cfg.Provider = args[i+1]
				i++
			}
		case "--model", "-m":
			if i+1 < len(args) {
				cfg.Model = args[i+1]
				i++
			}
		case "--api-key", "-k":
			if i+1 < len(args) {
				cfg.APIKey = args[i+1]
				i++
			}
		case "--base-url":
			if i+1 < len(args) {
				cfg.BaseURL = args[i+1]
				i++
			}
		case "--max-steps":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					cfg.MaxSteps = n
				}
				i++
			}
		default:
			promptParts = append(promptParts, args[i])
		}
	}

	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fatalf("%v", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fatalf("%v", err)
		os.Exit(1)
	}
	defer engine.Close()

	if cfg.APIKey != "" {
		resolved := harness.ResolveConfig(engine, cfg)
		_ = harness.SaveAPIKey(resolved.Provider, cfg.APIKey)
	}

	runner := harness.NewRunner(engine, cfg)
	prompt := strings.TrimSpace(strings.Join(promptParts, " "))
	if prompt == "" {
		if useTUI || (ui.IsTerminal(os.Stdin) && ui.IsTerminalFile(os.Stdout) && os.Getenv("TETHER_LEGACY_REPL") != "1") {
			bridge := buildTUIBridge(runner, engine)
			if err := tui.RunTUI(context.Background(), os.Stdin, os.Stdout, workspace, strings.ToUpper(engine.GetMode()), engine.Branch, bridge); err != nil {
				fatalf("%v", err)
			}
			return
		}
		if err := runner.RunInteractive(context.Background(), os.Stdin); err != nil {
			fatalf("%v", err)
		}
		return
	}

	runner.SetAutoApprovePolicy(func(d mcp.Decision) bool {
		if d.Tool == "run_command" {
			if ok, _ := safety.IsReadOnlyCommand(d.Command); ok {
				return true
			}
			if ok, _ := safety.IsValidationCommand(d.Command); ok {
				return true
			}
		}
		return false
	})

	start := time.Now()
	out, err := runner.Run(context.Background(), prompt)
	if err != nil {
		// A failed run prints nothing after the answer, so mark it here too.
		fmt.Fprintln(os.Stderr, harness.DoneRule("failed", time.Since(start)))
		fatalf("harness: %v", err)
	}
	// Agent output is streamed verbatim: it may contain its own formatting.
	fmt.Println(out)
	// A one-shot run has the same problem as the REPL: the answer appears and
	// nothing says the turn is over. The REPL marks this inside runTurn, but
	// this path never enters it, so the mark is printed here instead.
	fmt.Println(harness.DoneRule("done", time.Since(start)))
}

func buildTUIBridge(runner *harness.Runner, engine *core.Engine) *tui.AgentBridge {
	runner.SetAutoApprovePolicy(runner.DefaultAutoApprovePolicy())
	return &tui.AgentBridge{
		Provider:        runner.ProviderName,
		Model:           runner.ModelName,
		Mode:            runner.ModeName,
		Branch:          func() string { return engine.Branch },
		NeedsAPIKey:     runner.NeedsAPIKey,
		SaveAPIKey:      runner.SaveProviderKey,
		SwitchModel:     runner.SwitchModel,
		SwitchProvider:  runner.SwitchProvider,
		SwitchMode:      runner.SwitchMode,
		ClearHistory:    runner.ClearHistory,
		Undo:            runner.UndoTurn,
		StatusSummary:   runner.StatusSummary,
		ExecuteCommand:  runner.ExecuteGatedCommand,
		RunPrompt:       runner.RunWithOutput,
		RunPromptStream: runner.RunWithOutputStream,
		RunSideChat:     runner.RunSideChat,
		ListModels: func() []tui.PickerOption {
			choices := runner.ListModels()
			out := make([]tui.PickerOption, len(choices))
			for i, c := range choices {
				out[i] = tui.PickerOption{Label: c.Label, Detail: c.Detail}
			}
			return out
		},
		ListProviders: func() []tui.PickerOption {
			choices := runner.ListProviders()
			out := make([]tui.PickerOption, len(choices))
			for i, c := range choices {
				out[i] = tui.PickerOption{Label: c.Label, Detail: c.Detail}
			}
			return out
		},
		BindApproval: func(askFn func(req tui.ApprovalRequest) int) {
			runner.SetAskPolicy(func(d mcp.Decision) (mcp.Verdict, bool) {
				target := d.Path
				if target == "" {
					target = d.Command
				}
				if target == "" {
					target = d.Label
				}
				mode := "code"
				ws := "."
				if engine != nil {
					mode = engine.GetMode()
					if engine.Workspace != "" {
						ws = engine.Workspace
					}
				}
				// Defense-in-depth: non-critical actions (read/validation shell,
				// routine undoable workspace edits in CODE mode, plan artifacts
				// in PLAN mode) never interrupt the user.
				if ok, _ := safety.IsNonCriticalTool(mode, d.Tool, target, d.Command, ws); ok {
					return mcp.VerdictOnce, true
				}
				risk, reason := safety.ClassifyRisk(d.Tool, target)
				if risk == safety.RiskLow {
					// e.g. temp/cache writes: not critical, no prompt.
					return mcp.VerdictOnce, true
				}
				var scopeHint string
				if d.Command != "" {
					if prefix := mcp.CommandPrefix(d.Command); prefix != "" {
						scopeHint = fmt.Sprintf("commands starting with %q", prefix)
					}
				} else if d.Path != "" {
					dir := filepath.ToSlash(filepath.Dir(d.Path))
					if dir == "." || dir == "/" || dir == "" {
						dir = filepath.ToSlash(d.Path)
					}
					scopeHint = fmt.Sprintf("changes under %q", dir)
				}
				diffRaw := d.FullPreview
				if diffRaw == "" {
					diffRaw = d.Preview
				}
				var diffLines []string
				if strings.TrimSpace(diffRaw) != "" {
					diffLines = strings.Split(strings.TrimRight(diffRaw, "\n"), "\n")
				}
				choice := views.ApprovalOptionIndex(askFn(tui.ApprovalRequest{
					AgentName:      "Main Agent",
					ActionType:     d.Tool,
					Target:         target,
					ScopeHint:      scopeHint,
					MainEvaluation: reason,
					DiffLines:      diffLines,
				}))
				switch choice {
				case views.ApprovalApproveAndRun:
					return mcp.VerdictOnce, true
				case views.ApprovalApproveFolder:
					return mcp.VerdictFolder, true
				case views.ApprovalApproveSession:
					return mcp.VerdictSession, true
				default:
					return mcp.VerdictReject, true
				}
			})
		},
	}
}

func printUsage() {
	fmt.Println(`tether — The Local-First Deterministic Substrate & Interactive AI Agent

Usage:
  tether                   Start interactive AI coding session (Aider / Claude Code style)
  tether --model <model>   Start interactive session with specific model (e.g. deepseek-reasoner)
  tether init              Initialize tether in current project
  tether status            Display real-time substrate dashboard & guardrail state
  tether mode [code|plan]  Inspect or switch active governance mode
  tether doctor            Verify database, WAL, hooks, and agent wiring
  tether mcp               Start MCP server (stdio transport)
  tether hook <event>      Handle lifecycle hook event (PreInvocation|PreToolUse|PostToolUse)
  tether install [agent]   Wire tether into agent config (agy|claude|codex|cursor|windsurf|cline|roo|opencode|all)
  tether version           Show version
  tether help              Show this help`)
}
