package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/ui"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(val string) error {
	*s = append(*s, val)
	return nil
}

// cmdMCP dispatches `tether mcp ...` subcommands while preserving legacy piped
// `tether mcp` stdio server execution for IDE bridges and e2e tests.
func cmdMCP(args []string) {
	if len(args) == 0 && !ui.IsTerminal(os.Stdin) {
		serveMCPServer()
		return
	}

	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fatalf("%v", err)
	}

	code := runMCPCommand(workspace, args, os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

func serveMCPServer() {
	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fatalf("%v", err)
	}
	engine, err := core.NewEngine(workspace)
	if err != nil {
		fatalf("%v", err)
	}
	defer engine.Close()

	server := mcp.NewServer(engine)
	if err := server.Serve(); err != nil {
		fatalf("mcp server: %v", err)
	}
}

func runMCPCommand(workspace string, args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		printMCPUsage(out)
		return 0
	}

	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]

	switch sub {
	case "help", "--help", "-h":
		printMCPUsage(out)
		return 0
	case "serve":
		serveMCPServer()
		return 0
	}

	reg := mcp.NewClientRegistry(workspace)
	switch sub {
	case "add":
		return mcpCmdAdd(reg, rest, out, errOut)
	case "import":
		return mcpCmdImport(reg, rest, out, errOut)
	case "list", "ls":
		return mcpCmdList(reg, rest, out, errOut)
	case "test":
		return mcpCmdTest(reg, rest, out, errOut)
	case "enable":
		return mcpCmdToggle(reg, rest, true, out, errOut)
	case "disable":
		return mcpCmdToggle(reg, rest, false, out, errOut)
	case "remove", "rm", "delete":
		return mcpCmdRemove(reg, rest, out, errOut)
	case "tools":
		return mcpCmdTools(reg, rest, out, errOut)
	default:
		fmt.Fprintf(errOut, "error: unknown mcp subcommand %q\n\n", args[0])
		printMCPUsage(errOut)
		return 1
	}
}

func printMCPUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  tether mcp add <name> (--command <cmd> [--arg <a1>...] [--env K=V...] | --url <url> [--header K=V...]) [--scope global|project] [--preset lazy_readonly|full] [--enable]
  tether mcp import [--source agy|claude|cursor|all] [--scope global|project]
  tether mcp list [--json]
  tether mcp test <name>
  tether mcp enable <name>
  tether mcp disable <name>
  tether mcp remove <name> [--scope global|project]
  tether mcp tools <name> [--enable t1 --disable t2] [--preset lazy_readonly|full|custom]
  tether mcp serve          Start Tether's own MCP stdio server [Deferred / Legacy Bridge]

Notes:
  • Bare 'tether mcp' with piped stdin also runs the stdio server [Deferred / Legacy Bridge].
  • Secrets in --env / --header and IDE imports are stored only in 0600 credentials.json.`)
}

// parseFlagsFlexible parses flags even when positional arguments precede or interleave flags.
func parseFlagsFlexible(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	cur := args
	for len(cur) > 0 {
		if err := fs.Parse(cur); err != nil {
			return nil, err
		}
		rem := fs.Args()
		if len(rem) == 0 {
			break
		}
		positional = append(positional, rem[0])
		cur = rem[1:]
	}
	return positional, nil
}

func splitKVFlags(items []string, kind string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	for _, raw := range items {
		k, v, ok := strings.Cut(raw, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --%s %q (expected KEY=VALUE)", kind, raw)
		}
		out[k] = v
	}
	return out, nil
}

func splitCommaAndRepeated(items []string) []string {
	var out []string
	for _, item := range items {
		for part := range strings.SplitSeq(item, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func mcpCmdAdd(reg *mcp.ClientRegistry, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("mcp add", flag.ContinueOnError)
	var cmdStr, urlStr, transport, scope, preset string
	var enable bool
	var argList, envList, headerList stringSliceFlag

	fs.StringVar(&cmdStr, "command", "", "Command to spawn for stdio MCP server")
	fs.Var(&argList, "arg", "Command argument (repeatable)")
	fs.Var(&envList, "env", "Environment variable KEY=VALUE (repeatable)")
	fs.StringVar(&urlStr, "url", "", "HTTP/SSE MCP endpoint URL")
	fs.Var(&headerList, "header", "HTTP header KEY=VALUE (repeatable)")
	fs.StringVar(&transport, "transport", "", "Transport: stdio | http | sse")
	fs.StringVar(&scope, "scope", mcp.ScopeProject, "Registry scope: global | project")
	fs.StringVar(&preset, "preset", "", "Tool preset: lazy_readonly | full | custom")
	fs.BoolVar(&enable, "enable", false, "Enable immediately instead of adding as draft")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	if len(pos) != 1 {
		fmt.Fprintln(errOut, "error: usage: tether mcp add <name> (--command ... | --url ...)")
		return 1
	}
	name := pos[0]

	if strings.TrimSpace(cmdStr) != "" && strings.TrimSpace(urlStr) != "" {
		fmt.Fprintln(errOut, "error: specify either --command or --url, not both")
		return 1
	}
	if strings.TrimSpace(cmdStr) == "" && strings.TrimSpace(urlStr) == "" {
		fmt.Fprintln(errOut, "error: either --command or --url is required")
		return 1
	}

	envMap, err := splitKVFlags(envList, "env")
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	headerMap, err := splitKVFlags(headerList, "header")
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}

	cfg := mcp.ExternalMCPConfig{
		Name:      name,
		Transport: transport,
		Command:   strings.TrimSpace(cmdStr),
		Args:      []string(argList),
		Env:       envMap,
		URL:       strings.TrimSpace(urlStr),
		Headers:   headerMap,
		Enabled:   enable,
		Preset:    preset,
		Scope:     scope,
	}

	saved, err := reg.AddServer(cfg)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}

	status := "disabled (draft)"
	if saved.Enabled {
		status = "enabled"
	}
	fmt.Fprintf(out, "%s %s (%s, transport=%s, preset=%s, %s)\n",
		ui.Success("✓ Added MCP server"), ui.Bold(saved.Name), saved.Scope, saved.Transport, saved.Preset, status)
	if !saved.Enabled {
		fmt.Fprintf(out, "  Run `tether mcp test %s` and `tether mcp enable %s` to activate.\n", saved.Name, saved.Name)
	}
	return 0
}

func mcpCmdImport(reg *mcp.ClientRegistry, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("mcp import", flag.ContinueOnError)
	var source, scope string
	fs.StringVar(&source, "source", "all", "IDE source: agy | claude | cursor | all")
	fs.StringVar(&scope, "scope", mcp.ScopeGlobal, "Target scope: global | project")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	if len(pos) > 0 {
		fmt.Fprintf(errOut, "error: unexpected argument %q\n", pos[0])
		return 1
	}

	res, err := reg.ImportFromIDE(source, scope)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}

	if len(res.Imported) == 0 {
		fmt.Fprintln(out, "No new external MCP servers imported.")
	} else {
		fmt.Fprintf(out, "%s %d draft MCP server(s) (enabled=false):\n", ui.Success("✓ Imported"), len(res.Imported))
		for _, item := range res.Imported {
			secretNote := ""
			if item.SecretsMoved > 0 {
				secretNote = fmt.Sprintf(", %d secret(s) isolated to credentials.json", item.SecretsMoved)
			}
			fmt.Fprintf(out, "  • %s (from=%s, scope=%s, transport=%s, preset=%s%s)\n",
				ui.Bold(item.Name), item.Source, item.Scope, item.Transport, item.Preset, secretNote)
		}
	}
	for _, sk := range res.Skipped {
		fmt.Fprintf(out, "  %s skipped %s (%s): %s\n", ui.Dim("-"), sk.Name, sk.Source, sk.Reason)
	}
	return 0
}

func mcpCmdList(reg *mcp.ClientRegistry, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("mcp list", flag.ContinueOnError)
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "Output JSON with secrets masked as **********")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	if len(pos) > 0 {
		fmt.Fprintf(errOut, "error: unexpected argument %q\n", pos[0])
		return 1
	}

	masked, err := reg.MaskedView()
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	if masked == nil {
		masked = []mcp.ExternalMCPConfig{}
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(masked); err != nil {
			fmt.Fprintf(errOut, "error: %v\n", err)
			return 1
		}
		return 0
	}

	if len(masked) == 0 {
		fmt.Fprintln(out, "No external MCP servers configured.")
		sugg := mcp.EvaluateSerenaSuggestionWithRegistry(reg, "code", false)
		if sugg.Recommended {
			fmt.Fprintf(out, "💡 %s\n", sugg.Recommendation)
		}
		return 0
	}

	for _, s := range masked {
		state := ui.Dim("disabled")
		if s.Enabled {
			state = ui.Success("enabled")
		}
		target := s.URL
		if s.Transport == mcp.TransportStdio {
			target = strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
		}
		fmt.Fprintf(out, "• %s [%s | %s | %s | preset=%s] -> %s\n",
			ui.Bold(s.Name), s.Scope, s.Transport, state, s.Preset, target)
		if len(s.Env) > 0 {
			fmt.Fprintf(out, "    env: %s\n", formatMaskedKV(s.Env))
		}
		if len(s.Headers) > 0 {
			fmt.Fprintf(out, "    headers: %s\n", formatMaskedKV(s.Headers))
		}
		if len(s.EnabledTools) > 0 {
			fmt.Fprintf(out, "    enabled_tools: %s\n", strings.Join(s.EnabledTools, ", "))
		}
		if len(s.DisabledTools) > 0 {
			fmt.Fprintf(out, "    disabled_tools: %s\n", strings.Join(s.DisabledTools, ", "))
		}
	}
	return 0
}

func formatMaskedKV(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func mcpCmdTest(reg *mcp.ClientRegistry, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("mcp test", flag.ContinueOnError)
	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	if len(pos) != 1 {
		fmt.Fprintln(errOut, "error: usage: tether mcp test <name>")
		return 1
	}

	mgr := mcp.NewClientManagerFromRegistry(reg)
	defer func() { _ = mgr.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, terr := mgr.TestServer(ctx, pos[0])
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "%s %s\n", ui.Warn("▲"), w)
	}
	if terr != nil || !res.OK {
		errMsg := res.Error
		if errMsg == "" && terr != nil {
			errMsg = terr.Error()
		}
		fmt.Fprintf(errOut, "✖ MCP test failed for %s: %s\n", pos[0], errMsg)
		return 1
	}

	fmt.Fprintf(out, "%s %s (%s, transport=%s, latency=%dms)\n",
		ui.Success("✓ MCP test OK:"), ui.Bold(res.Name), res.Scope, res.Transport, res.LatencyMs)
	fmt.Fprintf(out, "  tools: %d discovered, %d active under preset %q\n",
		res.RawToolCount, res.FilteredToolCount, res.Preset)
	for _, t := range res.Tools {
		fmt.Fprintf(out, "    - %s (%s)\n", t.Name, t.OriginalName)
	}
	return 0
}

func mcpCmdToggle(reg *mcp.ClientRegistry, args []string, enable bool, out, errOut io.Writer) int {
	verb := "disable"
	if enable {
		verb = "enable"
	}
	fs := flag.NewFlagSet("mcp "+verb, flag.ContinueOnError)
	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	if len(pos) != 1 {
		fmt.Fprintf(errOut, "error: usage: tether mcp %s <name>\n", verb)
		return 1
	}

	updated, err := reg.SetServerEnabled(pos[0], enable)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	state := "disabled"
	if updated.Enabled {
		state = "enabled"
	}
	fmt.Fprintf(out, "%s %s (%s, preset=%s)\n",
		ui.Success("✓ MCP server "+state+":"), ui.Bold(updated.Name), updated.Scope, updated.Preset)
	return 0
}

func mcpCmdRemove(reg *mcp.ClientRegistry, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("mcp remove", flag.ContinueOnError)
	var scope string
	fs.StringVar(&scope, "scope", "", "Optional scope: global | project")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	if len(pos) != 1 {
		fmt.Fprintln(errOut, "error: usage: tether mcp remove <name> [--scope global|project]")
		return 1
	}

	if err := reg.RemoveServer(pos[0], scope); err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "%s %s\n", ui.Success("✓ Removed MCP server"), ui.Bold(pos[0]))
	return 0
}

func mcpCmdTools(reg *mcp.ClientRegistry, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("mcp tools", flag.ContinueOnError)
	var enableFlags, disableFlags stringSliceFlag
	var preset string
	fs.Var(&enableFlags, "enable", "Tool name(s) to enable (repeatable or comma-separated)")
	fs.Var(&disableFlags, "disable", "Tool name(s) to disable (repeatable or comma-separated)")
	fs.StringVar(&preset, "preset", "", "Update preset: lazy_readonly | full | custom")

	pos, err := parseFlagsFlexible(fs, args)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	if len(pos) != 1 {
		fmt.Fprintln(errOut, "error: usage: tether mcp tools <name> [--enable t1 --disable t2] [--preset lazy_readonly|full|custom]")
		return 1
	}

	en := splitCommaAndRepeated(enableFlags)
	dis := splitCommaAndRepeated(disableFlags)

	var srv mcp.ExternalMCPConfig
	if len(en) > 0 || len(dis) > 0 || strings.TrimSpace(preset) != "" {
		var uerr error
		srv, uerr = reg.UpdateServerTools(pos[0], en, dis, preset)
		if uerr != nil {
			fmt.Fprintf(errOut, "error: %v\n", uerr)
			return 1
		}
		fmt.Fprintf(out, "%s %s (preset=%s)\n", ui.Success("✓ Updated tools for"), ui.Bold(srv.Name), srv.Preset)
	} else {
		var ok bool
		var gerr error
		srv, ok, gerr = reg.GetServer(pos[0])
		if gerr != nil {
			fmt.Fprintf(errOut, "error: %v\n", gerr)
			return 1
		}
		if !ok {
			fmt.Fprintf(errOut, "error: MCP server %q not found\n", pos[0])
			return 1
		}
		fmt.Fprintf(out, "MCP server %s (preset=%s, enabled=%v)\n", ui.Bold(srv.Name), srv.Preset, srv.Enabled)
	}

	if mcp.IsSerenaServer(srv) && srv.Preset == mcp.PresetLazyReadOnly {
		fmt.Fprintf(out, "  lazy_readonly allowlist: %s\n", strings.Join(mcp.SerenaReadOnlySemanticTools, ", "))
	}
	if len(srv.EnabledTools) > 0 {
		fmt.Fprintf(out, "  enabled_tools: %s\n", strings.Join(srv.EnabledTools, ", "))
	}
	if len(srv.DisabledTools) > 0 {
		fmt.Fprintf(out, "  disabled_tools: %s\n", strings.Join(srv.DisabledTools, ", "))
	}
	return 0
}
