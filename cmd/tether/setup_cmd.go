package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/harness"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/ui"
	"github.com/crossparties/tether/internal/webui"
)

func cmdSetup(args []string) {
	code := runSetupCommand(args, os.Stdin, os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

func runSetupCommand(args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var cliMode, noBrowser, probeOnly bool
	var provider, apiKey, model, baseURL, importSource string
	fs.BoolVar(&cliMode, "cli", false, "Run terminal Stage 1 setup without launching a browser")
	fs.BoolVar(&noBrowser, "no-browser", false, "Never launch a web browser")
	fs.BoolVar(&probeOnly, "probe-only", false, "Only inspect credentials and run 0-token key probes")
	fs.StringVar(&provider, "provider", "", "Provider name to configure globally")
	fs.StringVar(&apiKey, "api-key", "", "API key to store in 0600 credentials.json")
	fs.StringVar(&model, "model", "", "Optional default model override")
	fs.StringVar(&baseURL, "base-url", "", "Optional custom OpenAI-compatible base URL")
	fs.StringVar(&importSource, "import-mcp", "", "Import external IDE MCP drafts globally (agy|claude|cursor|all)")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			printSetupUsage(out)
			return 0
		}
		return 1
	}

	_ = cliMode
	_ = noBrowser

	headless := webui.IsHeadlessEnvironment()
	inFile, isOSFile := in.(*os.File)
	isTTY := isOSFile && ui.IsTerminal(inFile)

	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Title("TETHER SETUP — Stage 1 Global Credentials & Probes"))
	if headless {
		fmt.Fprintln(out, ui.Dim("  Environment: SSH / headless detected (non-blocking terminal mode)"))
	}

	provNorm := strings.ToLower(strings.TrimSpace(provider))
	keyTrim := strings.TrimSpace(apiKey)
	if provNorm != "" && (keyTrim != "" || provNorm == "ollama") {
		if err := saveGlobalKeyEntry(provNorm, keyTrim, baseURL, model); err != nil {
			fmt.Fprintf(errOut, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "  %s saved %s credentials to 0600 credentials.json\n", ui.Success("✓"), ui.Bold(provNorm))
	} else if !probeOnly && isTTY {
		interactiveGlobalKeySetup(inFile, out, errOut)
	} else if !probeOnly && !isTTY {
		fmt.Fprintln(out, ui.Dim("  Non-interactive stdin detected; skipping prompts."))
	}

	// Zero-token startup probe across configured global keys.
	creds, err := harness.LoadAllCredentials()
	if err != nil {
		fmt.Fprintf(errOut, "error reading credentials.json: %v\n", err)
		return 1
	}
	if len(creds) == 0 {
		fmt.Fprintln(out, ui.Warn("▲ No global API keys configured yet."))
		fmt.Fprintln(out, ui.Dim("  Run `tether setup --provider deepseek --api-key <key>` or `tether setup --cli` interactively."))
	} else {
		printMaskedCredentialsSummary(out, creds)
		pool := harness.NewKeyPool(nil, nil)
		if pool.Len() > 0 {
			pctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			results := pool.ProbeAll(pctx)
			cancel()
			fmt.Fprintln(out, ui.Title("  0-Token Key Probe Results:"))
			for _, h := range results {
				mark := ui.Success("✔")
				if !h.Healthy {
					mark = ui.Error("✖")
				}
				fmt.Fprintf(out, "    %s %-18s (%s) status=%d latency=%dms %s\n",
					mark, h.KeyID, h.Provider, h.StatusCode, h.LatencyMs, h.Reason)
			}
		}
	}

	// Optional IDE MCP draft import.
	reg := mcp.NewClientRegistry("")
	if strings.TrimSpace(importSource) != "" {
		res, ierr := reg.ImportFromIDE(importSource, mcp.ScopeGlobal)
		if ierr != nil {
			fmt.Fprintf(errOut, "error importing MCP configs: %v\n", ierr)
			return 1
		}
		printSetupMCPImport(out, res)
	} else if !probeOnly && isTTY && hasDiscoverableIDEConfigs() {
		fmt.Fprint(out, "\nImport external IDE MCP drafts (agy/claude/cursor) as disabled drafts? [y/N]: ")
		reader := bufio.NewReader(inFile)
		line, _ := reader.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		if ans == "y" || ans == "yes" {
			if res, ierr := reg.ImportFromIDE("all", mcp.ScopeGlobal); ierr == nil {
				printSetupMCPImport(out, res)
			}
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Accent("Next (Stage 2 Project):"))
	fmt.Fprintln(out, "  • Terminal fast path: "+ui.Bold("tether init")+"  then  "+ui.Bold("tether"))
	fmt.Fprintln(out, "  • Local Web UI:       "+ui.Bold("tether web --no-browser"))
	fmt.Fprintln(out)
	return 0
}

func saveGlobalKeyEntry(provider, apiKey, baseURL, model string) error {
	creds, err := harness.LoadAllCredentials()
	if err != nil {
		return err
	}
	existing := creds[provider]
	if strings.TrimSpace(baseURL) == "" && strings.TrimSpace(model) == "" && len(existing) == 0 && apiKey != "" {
		return harness.SaveAPIKey(provider, apiKey)
	}
	entry := harness.ProviderKeyEntry{
		Label:    provider,
		APIKey:   apiKey,
		BaseURL:  strings.TrimSpace(baseURL),
		Model:    strings.TrimSpace(model),
		Priority: 1,
		Enabled:  true,
	}
	for i := range existing {
		if existing[i].Priority <= 1 {
			existing[i].Priority = 2
		}
	}
	return harness.SaveProviderKeys(provider, append([]harness.ProviderKeyEntry{entry}, existing...))
}

func interactiveGlobalKeySetup(in *os.File, out, errOut io.Writer) {
	presets := harness.ListProviderPresets()
	choices := make([]ui.Choice, 0, len(presets)+1)
	choices = append(choices, ui.Choice{Label: "skip", Detail: "keep existing global credentials and run probe"})
	for _, p := range presets {
		choices = append(choices, ui.Choice{
			Label:  p.Name,
			Detail: "default model: " + p.DefaultModel,
		})
	}

	fmt.Fprintln(out, ui.Dim("  Select a provider to add/update in ~/.config/tether/credentials.json:"))
	idx, ok := ui.Pick(in, out, choices, 0)
	if !ok || idx == 0 {
		return
	}
	chosen := presets[idx-1]
	reader := bufio.NewReader(in)

	var key string
	if chosen.Name != "ollama" {
		fmt.Fprintf(out, "  Enter %s API key (leave empty to skip): ", chosen.Name)
		line, _ := reader.ReadString('\n')
		key = strings.TrimSpace(line)
		if key == "" {
			return
		}
	}
	if err := saveGlobalKeyEntry(chosen.Name, key, "", ""); err != nil {
		fmt.Fprintf(errOut, "  %s %v\n", ui.Error("✖"), err)
		return
	}
	fmt.Fprintf(out, "  %s saved %s key to 0600 credentials.json\n", ui.Success("✓"), chosen.Name)
}

func printMaskedCredentialsSummary(out io.Writer, creds map[string][]harness.ProviderKeyEntry) {
	providers := make([]string, 0, len(creds))
	for p := range creds {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	fmt.Fprintln(out, ui.Title("  Configured Global Providers:"))
	for _, p := range providers {
		entries := creds[p]
		enabled := 0
		for _, e := range entries {
			if e.Enabled {
				enabled++
			}
		}
		fmt.Fprintf(out, "    • %-12s %d key(s) (%d enabled)\n", p, len(entries), enabled)
	}
}

func hasDiscoverableIDEConfigs() bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	candidates := []string{
		filepath.Join(home, ".gemini", "config", "mcp_config.json"),
		filepath.Join(home, ".claude", "mcp_config.json"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".cursor", "mcp.json"),
	}
	for _, p := range candidates {
		if _, statErr := os.Stat(p); statErr == nil {
			return true
		}
	}
	return false
}

func printSetupMCPImport(out io.Writer, res mcp.ImportResult) {
	if len(res.Imported) == 0 {
		fmt.Fprintln(out, ui.Dim("  • No new external IDE MCP drafts found to import."))
		return
	}
	fmt.Fprintf(out, "  %s imported %d draft MCP server(s) (secrets isolated to credentials.json):\n", ui.Success("✓"), len(res.Imported))
	for _, item := range res.Imported {
		fmt.Fprintf(out, "    • %s (from=%s, preset=%s)\n", item.Name, item.Source, item.Preset)
	}
}

func printSetupUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  tether setup [--cli] [--probe-only] [--provider <p> --api-key <k>] [--import-mcp agy|claude|cursor|all]

Stage 1 Global Onboarding:
  Configures ~/.config/tether/credentials.json (mode 0600), runs 0-token key
  probes (/models, /user/balance, /api/tags), and optionally imports IDE MCP
  configs as disabled drafts without blocking in SSH/headless environments.`)
}

func cmdWeb(args []string) {
	code := runWebCommand(args, os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

func runWebCommand(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var port int
	var noBrowser bool
	var cfg harness.Config
	fs.IntVar(&port, "port", 0, "Localhost TCP port to bind on 127.0.0.1 (0 = ephemeral)")
	fs.BoolVar(&noBrowser, "no-browser", false, "Do not attempt to open a browser window")
	fs.StringVar(&cfg.Provider, "provider", "", "LLM provider override")
	fs.StringVar(&cfg.Model, "model", "", "Model override")
	fs.StringVar(&cfg.BaseURL, "base-url", "", "Base URL override")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fmt.Fprintln(out, "Usage: tether web [--port N] [--no-browser] [--provider <p>] [--model <m>]")
			return 0
		}
		return 1
	}

	workspace, err := core.DetectWorkspace(".")
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	engine, err := core.NewEngine(workspace)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	defer engine.Close()

	runner := harness.NewRunner(engine, cfg)
	defer func() { _ = runner.Close() }()

	srv, err := webui.NewServer(webui.Config{
		Engine:       engine,
		Runner:       runner,
		RunnerConfig: cfg,
		Port:         port,
		Version:      version,
	})
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}

	addr, err := srv.Listen()
	if err != nil {
		fmt.Fprintf(errOut, "error binding 127.0.0.1:%d: %v\n", port, err)
		return 1
	}
	loginURL := srv.BootstrapURL()
	headless := webui.IsHeadlessEnvironment()

	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Title("TETHER WEB UI — Local Authenticated Server"))
	fmt.Fprintf(out, "  Workspace:     %s\n", ui.Path(workspace))
	fmt.Fprintf(out, "  Bind Address:  %s\n", ui.Bold("http://"+addr))
	fmt.Fprintf(out, "  Bootstrap URL: %s\n", ui.Success(loginURL))
	if headless {
		_, boundPort, _ := strings.Cut(addr, ":")
		if p, perr := strconv.Atoi(boundPort); perr == nil && p > 0 {
			fmt.Fprintf(out, "  SSH Tunnel:    %s\n", ui.Dim(fmt.Sprintf("ssh -N -L %d:127.0.0.1:%d <user>@<host>", p, p)))
		}
		fmt.Fprintln(out, ui.Dim("  Headless/SSH detected — copy the Bootstrap URL above (single-use token)."))
	} else if !noBrowser {
		openBrowserBestEffort(loginURL)
	}
	fmt.Fprintln(out, ui.Dim("  Press Ctrl+C to shut down cleanly."))
	fmt.Fprintln(out)

	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- srv.Serve()
	}()

	select {
	case err := <-serveErrCh:
		if err != nil {
			fmt.Fprintf(errOut, "error: web server: %v\n", err)
			return 1
		}
		return 0
	case <-sigCtx.Done():
		fmt.Fprintln(out, ui.Dim("\nShutting down Tether Web UI..."))
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-serveErrCh
		return 0
	}
}

func openBrowserBestEffort(targetURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", targetURL)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", targetURL)
	default:
		if _, err := exec.LookPath("xdg-open"); err == nil {
			cmd = exec.Command("xdg-open", targetURL)
		}
	}
	if cmd != nil {
		_ = cmd.Start()
	}
}
