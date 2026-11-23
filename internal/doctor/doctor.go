package doctor

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/mount"
	"github.com/crossparties/tether/internal/ui"
)

// Check captures one diagnostic line from `tether doctor` / `POST /api/v1/doctor`.
type Check struct {
	Section string `json:"section"`
	Status  string `json:"status"` // "pass" | "warn" | "fail" | "info" | "fixed"
	Label   string `json:"label"`
	Value   string `json:"value"`
}

// Report aggregates all diagnostic checks and summary counts.
type Report struct {
	Workspace string  `json:"workspace"`
	Fix       bool    `json:"fix"`
	Checks    []Check `json:"checks"`
	Passed    int     `json:"passed"`
	Warnings  int     `json:"warnings"`
	Failed    int     `json:"failed"`
	Text      string  `json:"text"`
}

// Run performs read-only diagnostics on workspace (unless fix=true) and returns
// both structured checks and CLI-formatted text.
func Run(workspace string, fix bool, version string) Report {
	if strings.TrimSpace(version) == "" {
		version = "0.3.0"
	}
	rep := Report{
		Workspace: workspace,
		Fix:       fix,
	}

	var out bytes.Buffer
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "  "+ui.Bold("╺┳╸┏━╸╺┳╸┓ ┏┓┏━╸┏━┓")+"   "+ui.Title("TETHER DOCTOR"))
	fmt.Fprintln(&out, "   "+ui.Dim("┃ ┣╸  ┃ ┣━┫┣╸ ┣┳┛")+"   "+ui.Dim("Environment & Substrate Diagnostic"))
	if fix {
		fmt.Fprintln(&out, "   "+ui.Dim("╹ ┗━╸ ╹ ╹ ┗┗━╸╹┗━")+"   "+ui.Dim("Running substrate diagnostics with --fix..."))
	} else {
		fmt.Fprintln(&out, "   "+ui.Dim("╹ ┗━╸ ╹ ╹ ┗┗━╸╹┗━")+"   "+ui.Dim("Read-only substrate check (pass --fix to repair)..."))
	}
	fmt.Fprintln(&out)

	home, _ := os.UserHomeDir()
	credPath := filepath.Join(home, ".config", "tether", "credentials.json")
	if envCfg := strings.TrimSpace(os.Getenv("TETHER_CONFIG_DIR")); envCfg != "" {
		credPath = filepath.Join(filepath.Clean(envCfg), "credentials.json")
	}

	add := func(section, status, mark, label, value string) {
		rep.Checks = append(rep.Checks, Check{
			Section: section,
			Status:  status,
			Label:   label,
			Value:   value,
		})
		switch status {
		case "pass":
			rep.Passed++
		case "warn":
			rep.Warnings++
		case "fail":
			rep.Failed++
		}
		fmt.Fprintln(&out, doctorLine(mark, label, value))
	}

	if fix {
		fmt.Fprintln(&out, ui.Title("[--fix] Applying Repairs"))
		engine, err := core.NewEngine(workspace)
		if err != nil {
			add("fix", "fail", ui.Error("✖"), "Substrate Repair:", fmt.Sprintf("Failed (%v)", err))
		} else {
			_ = engine.EnsureGitignore()
			if created, _, ierr := mount.InitIgnoreFile(workspace, engine.WriteDB()); ierr != nil {
				add("fix", "info", ui.Warn("▲"), ".tether/ignore:", fmt.Sprintf("could not seed (%v)", ierr))
			} else if created {
				add("fix", "fixed", ui.Success("✔"), ".tether/ignore:", "seeded default ignore rules")
			}
			purged, _ := engine.ReconcileKB(nil)
			indexed, _ := engine.IndexDirectory(workspace, nil)
			add("fix", "fixed", ui.Success("✔"), "Substrate & Cache:", fmt.Sprintf(".tether/ initialized/repaired (purged %d stale, indexed %d docs)", purged, indexed))
			_ = engine.Close()
		}

		if info, serr := os.Stat(credPath); serr == nil && info.Mode().Perm()&0077 != 0 {
			_ = os.Chmod(filepath.Dir(credPath), 0700)
			if cerr := os.Chmod(credPath, 0600); cerr != nil {
				add("fix", "info", ui.Warn("▲"), "Credentials Perms:", fmt.Sprintf("chmod 0600 failed: %v", cerr))
			} else {
				add("fix", "fixed", ui.Success("✔"), "Credentials Perms:", "tightened ~/.config/tether/credentials.json to 0600")
			}
		}
		fmt.Fprintln(&out)
	}

	// 1. Storage & SQLite (strictly read-only!)
	fmt.Fprintln(&out, ui.Title("[1/4] Core Substrate & Storage"))
	legacyDBPath := filepath.Join(workspace, "session.db")
	if _, err := os.Stat(legacyDBPath); err == nil {
		add("storage", "warn", ui.Warn("▲"), "Legacy Root DB:", "session.db found at repo root (run 'tether doctor --fix' to migrate to .tether/)")
	}

	dbPath := filepath.Join(workspace, ".tether", "session.db")
	if _, err := os.Stat(dbPath); err == nil {
		roDB, oerr := core.OpenReadOnlyDB(dbPath)
		if oerr != nil {
			add("storage", "fail", ui.Error("✖"), "session.db:", fmt.Sprintf("failed read-only open (%v)", oerr))
		} else {
			var userVer int
			if qerr := roDB.QueryRow("PRAGMA user_version;").Scan(&userVer); qerr != nil {
				add("storage", "fail", ui.Error("✖"), "session.db:", fmt.Sprintf("read-only check failed (%v)", qerr))
			} else if userVer < core.SchemaVersion {
				add("storage", "warn", ui.Warn("▲"), "session.db:", fmt.Sprintf("Schema v%d < v%d (run 'tether doctor --fix' to upgrade)", userVer, core.SchemaVersion))
			} else {
				add("storage", "pass", ui.Success("✔"), "session.db:", fmt.Sprintf("accessible (?mode=ro, Schema v%d)", userVer))
			}
			_ = roDB.Close()
		}
	} else {
		add("storage", "warn", ui.Warn("▲"), "session.db:", ".tether/session.db not found (run 'tether init' or 'tether doctor --fix')")
	}

	cachePath := filepath.Join(workspace, ".tether", "cache.db")
	if _, err := os.Stat(cachePath); err == nil {
		roCache, oerr := core.OpenReadOnlyDB(cachePath)
		if oerr != nil {
			add("storage", "fail", ui.Error("✖"), "cache.db:", fmt.Sprintf("failed read-only open (%v)", oerr))
		} else {
			var files int
			if qerr := roCache.QueryRow("SELECT COUNT(DISTINCT file_path) FROM kb_documents").Scan(&files); qerr != nil {
				add("storage", "warn", ui.Warn("▲"), "cache.db:", fmt.Sprintf("incomplete cache schema (%v)", qerr))
			} else {
				add("storage", "pass", ui.Success("✔"), "cache.db:", fmt.Sprintf("accessible (?mode=ro, %d docs indexed)", files))
			}
			_ = roCache.Close()
		}
	} else {
		add("storage", "warn", ui.Warn("▲"), "cache.db:", ".tether/cache.db not found (run 'tether doctor --fix')")
	}

	gitignorePath := filepath.Join(workspace, ".gitignore")
	if content, err := os.ReadFile(gitignorePath); err == nil {
		text := string(content)
		hasAll := strings.Contains(text, ".tether/") ||
			(strings.Contains(text, ".tether/session.db") && strings.Contains(text, ".tether/cache.db"))
		if hasAll {
			add("storage", "pass", ui.Success("✔"), "Git Hygiene:", ".tether/ runtime files ignored in .gitignore")
		} else {
			add("storage", "fail", ui.Error("✖"), "Git Hygiene:", ".tether/session.db* or .tether/cache.db* missing from .gitignore!")
		}
	} else {
		add("storage", "warn", ui.Warn("▲"), "Git Hygiene:", ".gitignore not found")
	}

	// 2. Mount Rules (.tether/ignore) & Credentials Security
	fmt.Fprintln(&out, "\n"+ui.Title("[2/4] Mount Rules & Credentials Security"))
	srcPath, exists, rules, ruleErrs := mount.InspectIgnoreFile(workspace)
	if !exists {
		add("mount", "warn", ui.Warn("▲"), "Ignore Rules:", ".tether/ignore not found (built-in secret protection still active; run 'tether init')")
	} else if len(ruleErrs) > 0 {
		for _, rerr := range ruleErrs {
			add("mount", "fail", ui.Error("✖"), "Ignore Syntax:", rerr.Error())
		}
	} else {
		relSrc := filepath.Base(srcPath)
		if filepath.Base(filepath.Dir(srcPath)) == ".tether" {
			relSrc = ".tether/" + relSrc
		}
		add("mount", "pass", ui.Success("✔"), "Ignore Rules:", fmt.Sprintf("%s valid (%d custom + %d built-in rules)", relSrc, len(rules), len(mount.BuiltinSecretPatterns())))
	}

	if info, err := os.Stat(credPath); err == nil {
		perm := info.Mode().Perm()
		if perm&0077 != 0 {
			add("mount", "warn", ui.Warn("▲"), "Credentials Perms:", fmt.Sprintf("~/.config/tether/credentials.json mode %04o is looser than 0600 (run 'tether doctor --fix')", perm))
		} else {
			add("mount", "pass", ui.Success("✔"), "Credentials Perms:", fmt.Sprintf("~/.config/tether/credentials.json (%04o)", perm))
		}
	} else {
		add("mount", "info", ui.Dim("-"), "Credentials Perms:", ui.Dim("~/.config/tether/credentials.json not present"))
	}

	// 3. Agent Configurations & External MCP
	fmt.Fprintln(&out, "\n"+ui.Title("[3/4] Agent Wiring & Hooks"))
	agyHooksPath := filepath.Join(home, ".gemini", "config", "hooks.json")
	if _, err := os.Stat(agyHooksPath); err == nil {
		add("agents", "pass", ui.Success("✔"), "Antigravity (agy):", "Configured in ~/.gemini/config/hooks.json")
	} else {
		add("agents", "warn", ui.Warn("▲"), "Antigravity (agy):", "not configured (run 'tether install agy')")
	}

	claudeConfigPath := filepath.Join(home, ".claude", "mcp_config.json")
	if _, err := os.Stat(claudeConfigPath); err == nil {
		add("agents", "pass", ui.Success("✔"), "Claude Code:", "Configured in ~/.claude/mcp_config.json")
	} else {
		add("agents", "info", ui.Dim("-"), "Claude Code:", ui.Dim("Not installed / config not found"))
	}

	mcpReg := mcp.NewClientRegistry(workspace)
	if fix {
		if fixed, rerr := mcpReg.RepairRawSecretLeaks(); rerr == nil && fixed > 0 {
			add("agents", "fixed", ui.Success("✔"), "External MCP Secrets:", fmt.Sprintf("migrated %d inline secret(s) to 0600 credentials.json", fixed))
		}
	}
	extServers, extErr := mcpReg.ListServers()
	if extErr != nil {
		add("agents", "fail", ui.Error("✖"), "External MCP:", extErr.Error())
	} else if len(extServers) == 0 {
		add("agents", "info", ui.Dim("-"), "External MCP:", ui.Dim("none configured"))
	} else {
		enabledCnt := 0
		for _, s := range extServers {
			if s.Enabled {
				enabledCnt++
			}
		}
		if leaks, lerr := mcpReg.InspectRawSecretLeaks(); lerr == nil && len(leaks) > 0 {
			add("agents", "warn", ui.Warn("▲"), "External MCP:", fmt.Sprintf("%d configured (%d enabled) — raw secret(s) in mcp.json (%s); run 'tether doctor --fix'", len(extServers), enabledCnt, strings.Join(leaks, ", ")))
		} else {
			add("agents", "pass", ui.Success("✔"), "External MCP:", fmt.Sprintf("%d configured (%d enabled, %d draft)", len(extServers), enabledCnt, len(extServers)-enabledCnt))
		}
	}

	// 4. Protocol & Binary
	fmt.Fprintln(&out, "\n"+ui.Title("[4/4] Binary & Runtime Health"))
	if selfPath, err := os.Executable(); err == nil {
		add("runtime", "pass", ui.Success("✔"), "Binary Location:", ui.Path(selfPath))
	}
	add("runtime", "pass", ui.Success("✔"), "Tether Version:", "v"+version)

	fmt.Fprintln(&out, "\n"+ui.Dim("───────────────────────────────────────────────────────────────────────────"))
	failStr := ui.Dim("0")
	if rep.Failed > 0 {
		failStr = ui.Error(fmt.Sprintf("%d", rep.Failed))
	}
	fmt.Fprintf(&out, "Diagnostic Summary: %s Passed, %s Warnings, %s Failed\n",
		ui.Success(fmt.Sprintf("%d", rep.Passed)),
		ui.Warn(fmt.Sprintf("%d", rep.Warnings)),
		failStr)
	if rep.Failed > 0 || rep.Warnings > 0 {
		fmt.Fprintln(&out, "\n"+ui.Warn("🛠  Remediation: Run 'tether doctor --fix' or 'tether init' to repair missing/invalid items."))
	} else {
		fmt.Fprintln(&out, ui.Success("✓ Substrate is healthy and ready for agent execution."))
	}
	fmt.Fprintln(&out, ui.Dim("───────────────────────────────────────────────────────────────────────────"))
	fmt.Fprintln(&out)

	rep.Text = out.String()
	return rep
}

// Render writes the formatted CLI report to out.
func (r Report) Render(out io.Writer) {
	if out != nil {
		_, _ = io.WriteString(out, r.Text)
	}
}

func doctorLine(mark, label, value string) string {
	return fmt.Sprintf("  %s %s %s", mark, ui.Pad(label, 22), value)
}
