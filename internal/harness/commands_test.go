package harness

// These tests exist because the refactor that introduced the registry
// replaced a switch whose branches were the only record of what Tether
// supported. A branch dropped in the rewrite would not fail any existing test:
// it would simply stop existing. So the tests below pin the command set twice,
// once by name and once through the REPL itself.

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/core"
	"github.com/croosparties/tether/internal/mcp"
)

// legacyCommands is the exact set of branch heads the old switch handled,
// transcribed from the pre-registry code. If a future refactor removes one of
// these, this test fails and says which.
var legacyCommands = []string{
	"/exit", "/quit",
	"/plan",
	"/code",
	"/model",
	"/provider",
	"/key",
	"/status",
	"/clear",
	"/undo",
}

func TestRegistryCoversEveryLegacyCommand(t *testing.T) {
	for _, want := range legacyCommands {
		if _, ok := lookupCommand(want); !ok {
			t.Errorf("legacy command %q has no registry entry: it used to work and now does not", want)
		}
	}
}

func TestRegistryLookupIsCaseAndSlashInsensitive(t *testing.T) {
	for _, name := range []string{"plan", "/plan", "PLAN", " /Plan "} {
		if c, ok := lookupCommand(name); !ok || c.Name != "plan" {
			t.Errorf("lookupCommand(%q) = (%q, %v), want (plan, true)", name, c.Name, ok)
		}
	}
}

func TestRegistryHasNoDuplicateNamesOrAliases(t *testing.T) {
	seen := map[string]string{}
	for _, c := range commands {
		for _, name := range append([]string{c.Name}, c.Aliases...) {
			if prev, dup := seen[name]; dup {
				t.Errorf("name %q is claimed by both %q and %q: lookup order would decide silently", name, prev, c.Name)
			}
			seen[name] = c.Name
		}
	}
}

func TestEveryCommandHasHelpText(t *testing.T) {
	for _, c := range commands {
		if strings.TrimSpace(c.Help) == "" {
			t.Errorf("command %q has no help text: it would render as a blank row in /help", c.Name)
		}
		if c.Handler == nil {
			t.Errorf("command %q has no handler: dispatch would report success and do nothing", c.Name)
		}
	}
}

// TestHelpTableMentionsEveryCommand guards the specific bug that motivated
// this work: the startup banner advertised seven commands and silently omitted
// /quit, /clear and /undo.
func TestHelpTableMentionsEveryCommand(t *testing.T) {
	var out bytes.Buffer
	renderCommandList(&out, commands)
	rendered := out.String()

	for _, c := range commands {
		if !strings.Contains(rendered, "/"+c.Name) {
			t.Errorf("/help output does not mention /%s:\n%s", c.Name, rendered)
		}
	}
}

func TestStartupBannerMentionsEveryCommand(t *testing.T) {
	list := commandNameList()
	for _, c := range commands {
		if !strings.Contains(list, "/"+c.Name) {
			t.Errorf("startup banner omits /%s: %s", c.Name, list)
		}
	}
}

// TestKnownCommandsProduceOutputFromPipedInput drives the real REPL with piped
// input. It exists because in-process tests that call handlers directly cannot
// catch a plumbing gap in the dispatcher, which is how the previous menu bug
// survived.
func TestKnownCommandsProduceOutputFromPipedInput(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		out := runREPL(t, "/plan\n/quit\n")
		if !strings.Contains(out, "Switched to PLAN mode") {
			t.Fatalf("piped /plan did not switch mode:\n%s", out)
		}
	})

	t.Run("help", func(t *testing.T) {
		out := runREPL(t, "/help\n/quit\n")
		for _, c := range commands {
			if !strings.Contains(out, "/"+c.Name) {
				t.Errorf("/help via REPL omits /%s:\n%s", c.Name, out)
			}
		}
	})

	t.Run("clear", func(t *testing.T) {
		out := runREPL(t, "/clear\n/quit\n")
		if !strings.Contains(out, "Conversation history cleared") {
			t.Fatalf("piped /clear did not clear history:\n%s", out)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		out := runREPL(t, "/nonsense\n/quit\n")
		if !strings.Contains(out, "unknown command") {
			t.Fatalf("unknown command was not reported:\n%s", out)
		}
		if !strings.Contains(out, "/help") {
			t.Errorf("unknown-command message does not point at /help:\n%s", out)
		}
	})
}

// TestSlashMenuRunsTheChosenCommand proves the menu is a shortcut for names,
// not a second implementation: entering "1" must run the first registry entry.
func TestSlashMenuRunsTheChosenCommand(t *testing.T) {
	var out bytes.Buffer
	scanner := bufio.NewScanner(strings.NewReader("3\n"))
	name, ok := chooseCommand(&out, scanner)
	if !ok {
		t.Fatalf("chooseCommand returned ok=false for a valid number:\n%s", out.String())
	}
	if name != commands[2].Name {
		t.Fatalf("chose %q, want %q", name, commands[2].Name)
	}
}

func TestSlashMenuAcceptsNameAndRejectsOutOfRange(t *testing.T) {
	var out bytes.Buffer
	// 99 is out of range, "nope" matches nothing, "plan" is a valid name.
	scanner := bufio.NewScanner(strings.NewReader("99\nnope\nplan\n"))
	name, ok := chooseCommand(&out, scanner)
	if !ok || name != "plan" {
		t.Fatalf("chooseCommand = (%q, %v), want (plan, true); output:\n%s", name, ok, out.String())
	}
	if !strings.Contains(out.String(), "out of range") {
		t.Errorf("out-of-range number was not reported:\n%s", out.String())
	}
}

func TestSlashMenuClosedStdinKeepsPromptOpen(t *testing.T) {
	var out bytes.Buffer
	// An empty line selects nothing; closed stdin must not select anything
	// either. Silently approving or guessing here would be the dangerous case.
	scanner := bufio.NewScanner(strings.NewReader("\n"))
	if name, ok := chooseCommand(&out, scanner); ok {
		t.Fatalf("chooseCommand picked %q from an empty line and closed stdin", name)
	}
}

// runREPL builds a Runner wired to a buffer and feeds it piped input. The
// provider is ollama so no API key prompt is triggered.
func runREPL(t *testing.T, input string) string {
	t.Helper()
	var out bytes.Buffer
	r := newTestRunner(t, &out)
	if err := r.RunInteractive(t.Context(), strings.NewReader(input)); err != nil {
		t.Fatalf("RunInteractive: %v", err)
	}
	return out.String()
}

// newTestRunner builds a Runner on a temporary workspace with a real engine.
// Provider is ollama so no API-key prompt is triggered and no network call is
// made: every input used by these tests either selects a command or exits.
func newTestRunner(t *testing.T, out *bytes.Buffer) *Runner {
	t.Helper()
	dir := t.TempDir()
	engine, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return NewRunner(engine, Config{Provider: "ollama", Out: out})
}

var _ = mcp.VerdictReject
