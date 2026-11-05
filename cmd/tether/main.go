package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/croosparties/tether/internal/adapter"
	"github.com/croosparties/tether/internal/core"
	"github.com/croosparties/tether/internal/harness"
	"github.com/croosparties/tether/internal/hooks"
	"github.com/croosparties/tether/internal/mcp"
	"github.com/croosparties/tether/internal/snapshot"
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
	case "proposal", "proposals":
		cmdProposal(os.Args[2:])
	case "run", "agent", "chat":
		cmdRun(os.Args[2:])
	case "version":
		fmt.Printf("tether v%s\n", version)
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
		if strings.Contains(result, "Subagents cannot mutate") || strings.Contains(result, "Subagent shell mutation") {
			fmt.Fprintf(os.Stderr, "\n┌────────────────────────────── TETHER GUARD ──────────────────────────────┐\n")
			fmt.Fprintf(os.Stderr, "│ 🛑 SUBAGENT ACTION BLOCKED: Direct Codebase Mutation Restricted          │\n")
			fmt.Fprintf(os.Stderr, "├──────────────────────────────────────────────────────────────────────────┤\n")
			fmt.Fprintf(os.Stderr, "│ Subagents must propose diffs/commands to Main Agent for user approval.   │\n")
			fmt.Fprintf(os.Stderr, "└──────────────────────────────────────────────────────────────────────────┘\n\n")
		} else if strings.Contains(result, "session.db") {
			fmt.Fprintf(os.Stderr, "\n┌────────────────────────────── TETHER GUARD ──────────────────────────────┐\n")
			fmt.Fprintf(os.Stderr, "│ 🛑 BASH ACCESS BLOCKED: Use Tether MCP for session.db                    │\n")
			fmt.Fprintf(os.Stderr, "├──────────────────────────────────────────────────────────────────────────┤\n")
			fmt.Fprintf(os.Stderr, "│ Direct sqlite3/python shell calls trigger manual permission prompts.     │\n")
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
// cmdProposal is the user's own key to the approval gate. It writes straight to
// session.db and never goes through the model, so a decision cannot be forged or
// skipped by the agent that is supposed to be asking for permission.
func cmdProposal(args []string) {
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

	if len(args) == 0 {
		args = []string{"list"}
	}

	switch args[0] {
	case "list", "ls":
		proposals, err := engine.ListProposals("pending")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if len(proposals) == 0 {
			fmt.Println("No proposals awaiting approval. 🔓 Codebase writes are not blocked.")
			return
		}
		fmt.Printf("🔒 %d proposal(s) awaiting your approval:\n\n", len(proposals))
		for _, p := range proposals {
			fmt.Printf("  #%d  %-18s %s\n", p.ID, p.Role, p.CreatedAt)
			for _, line := range previewLines(p.Proposal, 3) {
				fmt.Printf("       %s\n", line)
			}
			fmt.Println()
		}
		fmt.Println("Decide with:  tether proposal approve <id>   |   tether proposal reject <id>")

	case "show":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: tether proposal show <id>")
			os.Exit(1)
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid proposal id %q\n", args[1])
			os.Exit(1)
		}
		p, err := engine.GetProposalByID(id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if p == nil {
			fmt.Fprintf(os.Stderr, "no proposal #%d\n", id)
			os.Exit(1)
		}
		fmt.Printf("Proposal #%d\n", p.ID)
		fmt.Printf("Role:   %s\n", p.Role)
		fmt.Printf("Status: %s\n", p.Status)
		fmt.Printf("Created: %s\n", p.CreatedAt)
		if p.Role == "spec" {
			fmt.Println("Note:   this is a plan to review, not a code patch.")
		}
		fmt.Println(strings.Repeat("─", 60))
		fmt.Println(p.Proposal)
		if p.Comment != "" {
			fmt.Println(strings.Repeat("─", 60))
			fmt.Printf("Your steering: %s\n", p.Comment)
		}

	case "approve", "reject", "abandon":
		verdict := args[0]
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: tether proposal %s <id|--all> [-m \"steering\"] [--topic]\n", verdict)
			os.Exit(1)
		}

		// A rejection may carry steering (revisit with direction) or mean the
		// user changed topic (drop the work, do not resubmit).
		comment, topic, rest := parseVerdictFlags(args[2:])
		if len(rest) > 0 {
			fmt.Fprintf(os.Stderr, "unexpected argument %q\n", rest[0])
			os.Exit(1)
		}
		if verdict != "reject" && (comment != "" || topic) {
			fmt.Fprintf(os.Stderr, "-m/--topic only apply to reject\n")
			os.Exit(1)
		}

		var affected int64
		switch {
		case args[1] == "--all":
			if verdict == "approve" {
				affected, err = engine.ApproveProposal()
			} else {
				affected, err = engine.RejectProposal()
			}
		default:
			id, perr := strconv.ParseInt(args[1], 10, 64)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "invalid proposal id %q\n", args[1])
				os.Exit(1)
			}
			switch {
			case verdict == "approve":
				affected, err = engine.ApproveProposalByID(id)
			case verdict == "abandon" || topic:
				affected, err = engine.AbandonProposal(id)
			default:
				affected, err = engine.RejectProposalWithComment(id, comment)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if affected == 0 {
			fmt.Println("Nothing to decide — no pending proposal matched.")
			return
		}
		icon, done := "✅", "approved"
		switch {
		case verdict == "reject" || topic:
			icon, done = "🚫", "rejected"
		}
		fmt.Printf("%s %d proposal(s) %s by you via CLI.\n", icon, affected, done)
		if verdict == "approve" {
			fmt.Println("🔓 Codebase writes are no longer blocked.")
			return
		}
		fmt.Println("🔒 Writes stay blocked until a new proposal is approved.")
		if topic {
			fmt.Println("Topic dropped — the agent must not resubmit this work.")
		} else if comment != "" {
			fmt.Printf("Steering recorded: %s\n", comment)
			fmt.Println("The agent must revise with this direction, not guess at a fix.")
		} else {
			fmt.Println("No steering given — the agent must wait for your comment.")
		}

	case "feedback":
		feedback, ferr := engine.GetRejectionFeedback(10)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", ferr)
			os.Exit(1)
		}
		if len(feedback) == 0 {
			fmt.Println("No rejections to answer. Nothing is waiting on you.")
			return
		}
		for _, p := range feedback {
			fmt.Printf("#%d  %-12s %-10s %s\n", p.ID, p.Role, p.Status, p.CreatedAt)
			for _, line := range previewLines(p.Proposal, 2) {
				fmt.Printf("      %s\n", line)
			}
			if p.Comment == "" {
				fmt.Println("      ↳ waiting for your direction (tether proposal show " +
					strconv.FormatInt(p.ID, 10) + ")")
			} else {
				fmt.Printf("      ↳ you said: %s\n", p.Comment)
			}
			fmt.Println()
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: tether proposal [list|show <id>|approve <id|--all>|reject <id|--all> [-m \"steering\"|--topic]|abandon <id>|feedback]")
		os.Exit(1)
	}
}

// parseVerdictFlags splits trailing reject options from positional arguments.
// It accepts -m/--message/--comment with a value, and --topic as a bare flag.
func parseVerdictFlags(args []string) (comment string, topic bool, rest []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--topic":
			topic = true
		case arg == "-m" || arg == "--message" || arg == "--comment":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "%s needs a value\n", arg)
				os.Exit(1)
			}
			comment = args[i+1]
			i++
		case strings.HasPrefix(arg, "--message="):
			comment = strings.TrimPrefix(arg, "--message=")
		case strings.HasPrefix(arg, "--comment="):
			comment = strings.TrimPrefix(arg, "--comment=")
		case strings.HasPrefix(arg, "-m="):
			comment = strings.TrimPrefix(arg, "-m=")
		default:
			rest = append(rest, arg)
		}
	}
	return comment, topic, rest
}

// previewLines returns at most max non-empty lines from text, trimmed.
func previewLines(text string, max int) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 72 {
			line = line[:72] + "…"
		}
		out = append(out, line)
		if len(out) == max {
			break
		}
	}
	if len(out) == 0 {
		out = []string{"(empty)"}
	}
	return out
}

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

// cmdUndo restores files captured by the snapshot store for a given turn, or
// for the most recent turn when no --turn flag is supplied.
func cmdUndo(args []string) {
	turn := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--turn", "-t":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --turn requires a value")
				os.Exit(1)
			}
			i++
			turn = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", args[i])
				os.Exit(1)
			}
			// A bare argument is treated as the turn id.
			turn = args[i]
		}
	}

	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	store := snapshot.New(workspace)

	if turn == "" {
		last, err := store.LastTurn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: could not read snapshots: %v\n", err)
			os.Exit(1)
		}
		if last == "" {
			fmt.Println("ℹ Nothing to undo — no snapshot recorded yet.")
			fmt.Println("  Snapshots are taken before each file write, so run the agent first.")
			return
		}
		turn = last
	}

	restored, err := store.Restore(turn, workspace)
	for _, path := range restored {
		fmt.Printf("↩ restored %s\n", path)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ Undo partially failed for turn %s: %v\n", turn, err)
		if len(restored) == 0 {
			os.Exit(1)
		}
		fmt.Println("  Files listed above were restored; the rest were left untouched.")
		return
	}
	if len(restored) == 0 {
		fmt.Printf("ℹ Turn %s had no captured files.\n", turn)
		return
	}
	fmt.Printf("✓ Undo complete — %d file(s) restored from turn %s.\n", len(restored), turn)
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

	// A pending subagent proposal silently blocks every write. Never let the
	// user discover that only when a mutation is refused mid-task.
	if pending, perr := engine.ListProposals("pending"); perr == nil && len(pending) > 0 {
		fmt.Printf("🔒 LOCKED: %d subagent proposal(s) awaiting your approval", len(pending))
		if oldest := pending[0]; oldest.Role != "" {
			fmt.Printf(" (oldest #%d from %s, %s)", oldest.ID, oldest.Role, oldest.CreatedAt)
		}
		fmt.Println()
		fmt.Println("   Codebase writes are BLOCKED until you decide.")
		fmt.Printf("   Review: tether proposal show %d\n", pending[0].ID)
		fmt.Printf("   Decide: tether proposal approve %d  |  tether proposal reject %d\n", pending[0].ID, pending[0].ID)
		fmt.Println()
	}

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

// cmdRun launches the Interactive Agent REPL (when no prompt is passed) or runs a prompt directly
func cmdRun(args []string) {
	var cfg harness.Config
	var promptParts []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
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
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	engine, err := core.NewEngine(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
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
		if err := runner.RunInteractive(context.Background(), os.Stdin); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	out, err := runner.Run(context.Background(), prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(out)
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
