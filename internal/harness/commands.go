package harness

// This file holds the single source of truth for slash commands.
//
// Before this existed, every command was a `case "/name":` inside the REPL
// switch. Three things went wrong with that shape, all of them observed:
//
//  1. Nothing could enumerate the commands. A "/" menu needs a list to render;
//     a switch has no list, it only has branches.
//  2. The startup banner was hand-maintained and drifted. It advertised
//     /plan, /code, /model, /provider, /key, /status, /exit and omitted /quit,
//     /clear and /undo, which the switch did handle.
//  3. Adding a command changed control flow, not data, so the fact that
//     "/undo" existed was only discoverable by reading the switch body.
//
// The registry is a table. The REPL looks names up in it, /help renders it,
// and the "/" menu numbers it. Adding a command is now one entry.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/crossparties/tether/internal/ui"
)

// command is one slash command. Handler receives the arguments after the
// command name. It returns true when the REPL should exit.
//
// Handlers print their own result. They do not return errors: a command that
// fails mid-session should tell the user and leave the session alive, and the
// REPL has no second place to render that.
type command struct {
	Name    string   // without the leading slash
	Args    string   // "<name>", "[turn]", or "" when it takes none
	Help    string   // one line, lowercase, no trailing period
	Aliases []string // extra names that resolve to this command
	Handler func(r *Runner, args []string) bool
}

// commands is the registry. Order here is the order shown by /help and the
// "/" menu, so it is grouped by what a user is trying to do rather than
// alphabetised: shape the session, then change provider/model, then inspect,
// then undo/exit.
//
// The help handler is wired in init rather than named in the literal: a
// handler that renders `commands` from inside the literal defining `commands`
// is an initialisation cycle, which the compiler rejects.
var commands []command

func init() {
	commands = []command{
		{
			Name: "plan",
			Help: "switch to plan mode (codebase writes locked)",
			Handler: func(r *Runner, _ []string) bool {
				_ = r.engine.SetMode("plan")
				fmt.Fprintln(r.cfg.Out, "🔵 Switched to PLAN mode (codebase writes locked).")
				return false
			},
		},
		{
			Name: "code",
			Help: "switch to code mode (codebase writes unlocked)",
			Handler: func(r *Runner, _ []string) bool {
				_ = r.engine.SetMode("code")
				fmt.Fprintln(r.cfg.Out, "🟢 Switched to CODE mode (codebase writes unlocked).")
				return false
			},
		},
		{
			Name: "model",
			Args: "[name]",
			Help: "pick or set the active model",
			Handler: func(r *Runner, args []string) bool {
				choices := modelsFor(r.cfg.Provider)
				if len(args) == 0 || args[0] == "" {
					if len(choices) > 0 && ui.IsTerminal(os.Stdin) && ui.IsTerminalFile(r.cfg.Out) {
						start := currentChoiceIndex(choices, r.cfg.Model)
						if idx, ok := ui.Pick(os.Stdin, r.cfg.Out, choices, start); ok {
							r.cfg.Model = choices[idx].Label
							_ = r.engine.SetProjectSetting("model", r.cfg.Model)
							fmt.Fprintf(r.cfg.Out, "✓ Model set to %s\n", r.cfg.Model)
							return false
						}
					}
					if len(choices) > 0 {
						fmt.Fprintf(r.cfg.Out, "Current model: %s. Options: %s\n", r.cfg.Model, choiceLabels(choices))
					} else {
						fmt.Fprintf(r.cfg.Out, "Current model: %s (e.g. /model deepseek-reasoner or /model deepseek-chat)\n", r.cfg.Model)
					}
					return false
				}
				canonical, valid := validateModel(r.cfg.Provider, args[0])
				if !valid {
					fmt.Fprintf(r.cfg.Out, "✖ unknown model %q for %s. Options: %s\n",
						args[0], r.cfg.Provider, choiceLabels(choices))
					return false
				}
				r.cfg.Model = canonical
				_ = r.engine.SetProjectSetting("model", r.cfg.Model)
				fmt.Fprintf(r.cfg.Out, "✓ Model set to %s\n", r.cfg.Model)
				return false
			},
		},
		{
			Name: "provider",
			Args: "[name]",
			Help: "pick or switch provider and load its API key",
			Handler: func(r *Runner, args []string) bool {
				if len(args) == 0 || args[0] == "" {
					choices := providerChoices()
					if len(choices) > 0 && ui.IsTerminal(os.Stdin) && ui.IsTerminalFile(r.cfg.Out) {
						start := currentChoiceIndex(choices, r.cfg.Provider)
						if idx, ok := ui.Pick(os.Stdin, r.cfg.Out, choices, start); ok {
							applyProvider(r, choices[idx].Label)
							return false
						}
					}
					fmt.Fprintf(r.cfg.Out, "Current provider: %s (%s). Options: %s\n",
						r.cfg.Provider, r.cfg.Model, strings.Join(providerNames(), ", "))
					return false
				}
				p := strings.ToLower(args[0])
				if _, known := presets[p]; !known {
					// Refuse instead of silently accepting an unusable provider:
					// the old behaviour set the name, then failed on the next call
					// with a URL that was never valid.
					fmt.Fprintf(r.cfg.Out, "✖ unknown provider %q. Options: %s\n", p, strings.Join(providerNames(), ", "))
					return false
				}
				applyProvider(r, p)
				return false
			},
		},
		{
			Name: "key",
			Args: "<api-key>",
			Help: "save an API key for the active provider",
			Handler: func(r *Runner, args []string) bool {
				if len(args) == 0 || args[0] == "" {
					fmt.Fprintf(r.cfg.Out, "Usage: /key <api-key>  (saves to ~/.config/tether/credentials.json for %s)\n", r.cfg.Provider)
					return false
				}
				r.cfg.APIKey = args[0]
				_ = SaveAPIKey(r.cfg.Provider, r.cfg.APIKey)
				fmt.Fprintf(r.cfg.Out, "✓ API key saved for %s\n", r.cfg.Provider)
				return false
			},
		},
		{
			Name: "status",
			Help: "show workspace, session, mode, KB and provider",
			Handler: func(r *Runner, _ []string) bool {
				kbStats, _ := r.engine.KBStatus()
				fmt.Fprintf(r.cfg.Out, "Workspace: %s | Session: %s | Mode: %s | KB: %d files | Provider: %s (%s)\n",
					r.engine.Workspace, r.engine.SessionID, r.engine.GetMode(), kbStats.TotalFiles, r.cfg.Provider, r.cfg.Model)
				return false
			},
		},
		{
			Name: "clear",
			Help: "clear conversation history (substrate memory kept)",
			Handler: func(r *Runner, _ []string) bool {
				r.history = nil
				fmt.Fprintln(r.cfg.Out, "✓ Conversation history cleared (substrate memory in session.db retained).")
				return false
			},
		},
		{
			Name: "undo",
			Args: "[turn]",
			Help: "restore a snapshot (latest when no turn given)",
			Handler: func(r *Runner, args []string) bool {
				turn := ""
				if len(args) > 0 {
					turn = args[0]
				}
				r.undo(turn)
				return false
			},
		},
		{
			Name: "btw",
			Args: "<question>",
			Help: "ask a side question in an isolated context without polluting the active task",
			Handler: func(r *Runner, args []string) bool {
				q := strings.TrimSpace(strings.Join(args, " "))
				if q == "" {
					fmt.Fprintln(r.cfg.Out, "Usage: /btw <question>  (side chat isolated from main history & session.db)")
					return false
				}
				ans, err := r.RunSideChat(r.sideChatCtx(), q)
				if err != nil {
					fmt.Fprintf(r.cfg.Out, "✖ [btw] %v\n", err)
					return false
				}
				fmt.Fprintf(r.cfg.Out, "💬 [btw] %s\n", strings.TrimSpace(ans))
				return false
			},
		},
		{
			Name: "read",
			Args: "<path>",
			Help: "inspect a workspace file safely inside the repository boundary",
			Handler: func(r *Runner, args []string) bool {
				if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
					fmt.Fprintln(r.cfg.Out, "Usage: /read <path>")
					return false
				}
				content, err := r.ReadWorkspaceFile(args[0])
				if err != nil {
					fmt.Fprintf(r.cfg.Out, "✖ [read] %v\n", err)
					return false
				}
				fmt.Fprintf(r.cfg.Out, "📄 [%s]\n%s\n", strings.TrimPrefix(args[0], "@"), strings.TrimRight(content, "\n"))
				return false
			},
		},
		{
			Name: "exec",
			Args: "<cmd>",
			Help: "propose and execute a shell command through the two-tiered safety gate",
			Handler: func(r *Runner, args []string) bool {
				cmdStr := strings.TrimSpace(strings.Join(args, " "))
				if cmdStr == "" {
					fmt.Fprintln(r.cfg.Out, "Usage: /exec <command>")
					return false
				}
				out, err := r.ExecuteGatedCommand(r.sideChatCtx(), cmdStr)
				if err != nil {
					fmt.Fprintf(r.cfg.Out, "🛑 [exec] %v\n", err)
					return false
				}
				fmt.Fprintf(r.cfg.Out, "%s\n", strings.TrimRight(out, "\n"))
				return false
			},
		},
		{
			Name:    "help",
			Aliases: []string{"?"},
			Help:    "list every command",
			Handler: runHelp,
		},
		{
			Name:    "exit",
			Aliases: []string{"quit"},
			Help:    "leave the session",
			Handler: func(r *Runner, _ []string) bool {
				return true
			},
		},
	}
}

// lookupCommand resolves a name or alias. The lookup is rebuilt on each call
// rather than cached: the registry is ten entries and read once per submitted
// line, so a map is not worth the risk of two sources of truth drifting.
func lookupCommand(name string) (command, bool) {
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "/")
	for _, c := range commands {
		if c.Name == name {
			return c, true
		}
		for _, a := range c.Aliases {
			if a == name {
				return c, true
			}
		}
	}
	return command{}, false
}

// dispatchCommand runs the command named in line (which already starts with
// "/"). It returns done=true when the REPL should exit, and ok=false when the
// name is unknown so the caller can say so without running a model turn.
func dispatchCommand(r *Runner, line string) (done, ok bool) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false, false
	}
	c, found := lookupCommand(parts[0])
	if !found {
		return false, false
	}
	return c.Handler(r, parts[1:]), true
}

// usage renders "/model <name>" for the help table.
func (c command) usage() string {
	if c.Args == "" {
		return "/" + c.Name
	}
	return "/" + c.Name + " " + c.Args
}

// commandNameList renders "/plan, /code, ..." for the startup banner. It is
// derived from the registry so the banner cannot drift from the commands that
// actually exist.
func commandNameList() string {
	names := make([]string, len(commands))
	for i, c := range commands {
		names[i] = "/" + c.Name
	}
	return strings.Join(names, ", ")
}

// chooseCommand renders the "/" menu and reads a selection from the shared
// scanner. It returns the chosen command name, or ok=false when the user
// picked nothing usable.
//
// It reads through the caller's scanner on purpose. A second reader on stdin
// would race the chat loop for the same bytes, and the ordering guarantee
// ("a menu answer arrives in the same order the user typed it") would be lost.
func chooseCommand(out io.Writer, scanner *bufio.Scanner) (string, bool) {
	if ui.IsTerminal(os.Stdin) && ui.IsTerminalFile(out) {
		choices := make([]ui.Choice, len(commands))
		for i, c := range commands {
			choices[i] = ui.Choice{Label: c.usage(), Detail: c.Help}
		}
		fmt.Fprintln(out)
		idx, ok := ui.Pick(os.Stdin, out, choices, 0)
		if !ok {
			return "", false
		}
		return commands[idx].Name, true
	}

	fmt.Fprintln(out)
	for i, c := range commands {
		fmt.Fprintf(out, "  %2d) %-16s %s\n", i+1, c.usage(), c.Help)
	}
	fmt.Fprintln(out, ui.Dim("  Enter with nothing selected keeps you in the session."))

	for {
		fmt.Fprintf(out, "\n  %s  ", ui.Dim(fmt.Sprintf("Select [1-%d]:", len(commands))))
		if !scanner.Scan() {
			return "", false
		}
		choice := strings.TrimSpace(scanner.Text())
		if choice == "" {
			// An empty line selects nothing. The prompt stays open rather than
			// closing the menu with a guess.
			continue
		}
		if n, err := strconv.Atoi(choice); err == nil {
			if n >= 1 && n <= len(commands) {
				return commands[n-1].Name, true
			}
			fmt.Fprintf(out, "  %s number %d is out of range\n", ui.Warn("✖"), n)
			continue
		}
		// Typing the name is accepted too: the menu is a shortcut for names,
		// not a separate vocabulary.
		if c, ok := lookupCommand(choice); ok {
			return c.Name, true
		}
		fmt.Fprintf(out, "  %s no command matches %q\n", ui.Warn("✖"), choice)
	}
}

// applyProvider updates the active provider, model, base URL, and stored key.
func applyProvider(r *Runner, p string) {
	r.cfg.Provider = p
	preset := presets[p]
	r.cfg.BaseURL = preset.BaseURL
	r.cfg.Model = preset.DefaultModel
	r.cfg.APIKey = loadSavedKey(p)
	_ = r.engine.SetProjectSetting("provider", p)
	_ = r.engine.SetProjectSetting("model", r.cfg.Model)
	fmt.Fprintf(r.cfg.Out, "✓ Provider set to %s (%s)\n", r.cfg.Provider, r.cfg.Model)
}

// modelsFor returns the curated model choices for a provider.
func modelsFor(provider string) []ui.Choice {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "deepseek":
		return []ui.Choice{
			{Label: "deepseek-chat", Detail: "fast, general-purpose coding & chat (default)"},
			{Label: "deepseek-reasoner", Detail: "reasoning model, thinks step by step (R1)"},
		}
	case "openai":
		return []ui.Choice{
			{Label: "gpt-4o", Detail: "fast multimodal flagship (default)"},
			{Label: "gpt-4o-mini", Detail: "lightweight, fast and low-cost"},
			{Label: "o3-mini", Detail: "reasoning model for coding & logic"},
		}
	case "gemini":
		return []ui.Choice{
			{Label: "gemini-2.5-pro", Detail: "deep reasoning & coding flagship (default)"},
			{Label: "gemini-2.5-flash", Detail: "fast, low-latency model"},
		}
	case "groq":
		return []ui.Choice{
			{Label: "llama-3.3-70b-versatile", Detail: "fast 70B open-weights model (default)"},
			{Label: "deepseek-r1-distill-llama-70b", Detail: "distilled reasoning model on Groq"},
			{Label: "qwen-qwq-32b", Detail: "32B reasoning & coding model"},
		}
	case "xai":
		return []ui.Choice{
			{Label: "grok-2-latest", Detail: "Grok 2 flagship (default)"},
			{Label: "grok-beta", Detail: "Grok beta"},
		}
	case "openrouter":
		return []ui.Choice{
			{Label: "deepseek/deepseek-chat", Detail: "DeepSeek V3 via OpenRouter (default)"},
			{Label: "deepseek/deepseek-r1", Detail: "DeepSeek R1 reasoning via OpenRouter"},
			{Label: "anthropic/claude-3.7-sonnet", Detail: "Claude 3.7 Sonnet via OpenRouter"},
		}
	case "ollama":
		return []ui.Choice{
			{Label: "qwen2.5-coder:7b", Detail: "local Qwen 2.5 Coder 7B (default)"},
			{Label: "deepseek-coder-v2:16b", Detail: "local DeepSeek Coder V2"},
			{Label: "llama3.2:latest", Detail: "local Llama 3.2"},
		}
	}
	return nil
}

// validateModel checks whether name is a known model for provider.
func validateModel(provider, name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", false
	}
	choices := modelsFor(provider)
	if len(choices) == 0 {
		return trimmed, true
	}
	for _, c := range choices {
		if strings.EqualFold(c.Label, trimmed) {
			return c.Label, true
		}
	}
	return "", false
}

// providerChoices builds the picker list for /provider in stable order.
func providerChoices() []ui.Choice {
	names := providerNames()
	out := make([]ui.Choice, len(names))
	for i, name := range names {
		out[i] = ui.Choice{
			Label:  name,
			Detail: fmt.Sprintf("default: %s", presets[name].DefaultModel),
		}
	}
	return out
}

// currentChoiceIndex returns the index of active in choices, or 0 if not found.
func currentChoiceIndex(choices []ui.Choice, active string) int {
	for i, c := range choices {
		if strings.EqualFold(c.Label, active) {
			return i
		}
	}
	return 0
}

// choiceLabels joins the labels of choices with ", ".
func choiceLabels(choices []ui.Choice) string {
	labels := make([]string, len(choices))
	for i, c := range choices {
		labels[i] = c.Label
	}
	return strings.Join(labels, ", ")
}

// runHelp is a named handler, not a closure. A closure that mentions commands
// from inside the literal defining commands is an initialisation cycle; a
// function reference is not.
func runHelp(r *Runner, _ []string) bool {
	renderCommandList(r.cfg.Out, commands)
	return false
}

// renderCommandList prints the command table. Widths are computed, not
// hardcoded, so a new command cannot misalign the column.
//
// The list is a parameter rather than a read of the package-level commands,
// so that this function stays usable from tests with a synthetic table.
func renderCommandList(out io.Writer, list []command) {
	width := 0
	for _, c := range list {
		if n := len(c.usage()); n > width {
			width = n
		}
	}
	for _, c := range list {
		line := fmt.Sprintf("  %-*s  %s", width, c.usage(), c.Help)
		if len(c.Aliases) > 0 {
			aliases := make([]string, len(c.Aliases))
			for i, a := range c.Aliases {
				aliases[i] = "/" + a
			}
			line += " (" + strings.Join(aliases, ", ") + ")"
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, "  Anything without a leading slash is sent to the model.")
}

// providerNames returns the preset names in a stable order. Iterating the map
// directly would reorder the list between runs, which reads like a bug in a
// help message.
func providerNames() []string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
