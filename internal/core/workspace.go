package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crossparties/tether/internal/mount"
)

// DetectWorkspace finds the project root (git root or CWD)
func DetectWorkspace(startDir string) (string, error) {
	abs, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}

	// Try git root first
	gitRoot, err := gitRootFrom(abs)
	if err == nil && gitRoot != "" {
		return gitRoot, nil
	}

	// Fallback to CWD
	return abs, nil
}

func gitRootFrom(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// DetectProjectSettings scans project root and immediate subdirs (e.g. web/package.json)
// for config files and accumulates multi-stack tech_stack, check_cmd, build_cmd, and test_cmd.
func DetectProjectSettings(workspace string) map[string]string {
	settings := make(map[string]string)

	var stacks []string
	var buildCmds []string
	var testCmds []string
	var checkCmds []string
	seenStack := make(map[string]bool)
	addStack := func(s string) {
		if s != "" && !seenStack[s] {
			seenStack[s] = true
			stacks = append(stacks, s)
		}
	}

	// 1. Go
	if content, err := os.ReadFile(filepath.Join(workspace, "go.mod")); err == nil {
		addStack("go")
		buildCmds = append(buildCmds, "go build ./...")
		testCmds = append(testCmds, "go test ./...")
		checkCmds = append(checkCmds, "go vet ./...")

		text := string(content)
		if strings.Contains(text, "github.com/gofiber/fiber") {
			settings["framework"] = "fiber"
		} else if strings.Contains(text, "github.com/gin-gonic/gin") {
			settings["framework"] = "gin"
		} else if strings.Contains(text, "github.com/labstack/echo") {
			settings["framework"] = "echo"
		}
	}

	// 2. Svelte / TypeScript / Node (root and immediate subdirs such as web/)
	pkgDirs := []string{"."}
	if entries, err := os.ReadDir(workspace); err == nil {
		var subdirs []string
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") || mount.IsDefaultSkipDirName(name) {
				continue
			}
			if fileExists(filepath.Join(workspace, name, "package.json")) {
				subdirs = append(subdirs, name)
			}
		}
		sort.Strings(subdirs)
		pkgDirs = append(pkgDirs, subdirs...)
	}

	for _, relDir := range pkgDirs {
		pkgPath := filepath.Join(workspace, relDir, "package.json")
		content, err := os.ReadFile(pkgPath)
		if err != nil {
			continue
		}
		text := string(content)
		hasTS := strings.Contains(text, "\"typescript\"") || fileExists(filepath.Join(workspace, relDir, "tsconfig.json"))
		hasSvelte := strings.Contains(text, "\"svelte\"") || strings.Contains(text, "\"@sveltejs/")

		switch {
		case hasSvelte:
			addStack("svelte")
			if relDir != "." && settings["frontend_dir"] == "" {
				settings["frontend_dir"] = relDir
			}
		case hasTS:
			addStack("typescript")
		default:
			addStack("node")
		}

		if settings["framework"] == "" {
			switch {
			case strings.Contains(text, "\"@sveltejs/kit\""):
				settings["framework"] = "sveltekit"
			case hasSvelte:
				settings["framework"] = "svelte"
			case strings.Contains(text, "\"next\""):
				settings["framework"] = "nextjs"
			case strings.Contains(text, "\"react\""):
				settings["framework"] = "react"
			case strings.Contains(text, "\"vue\""):
				settings["framework"] = "vue"
			case strings.Contains(text, "\"express\""):
				settings["framework"] = "express"
			}
		}

		wrap := func(c string) string {
			if relDir == "." {
				return c
			}
			return fmt.Sprintf("cd %s && %s", relDir, c)
		}
		if strings.Contains(text, "\"build\"") {
			buildCmds = append(buildCmds, wrap("npm run build"))
		}
		if strings.Contains(text, "\"test\"") {
			testCmds = append(testCmds, wrap("npm test"))
		}
		if strings.Contains(text, "\"check\"") {
			checkCmds = append(checkCmds, wrap("npm run check"))
		} else if hasTS {
			checkCmds = append(checkCmds, wrap("npx --no-install tsc --noEmit"))
		}
	}

	// 3. Python
	if fileExists(filepath.Join(workspace, "pyproject.toml")) ||
		fileExists(filepath.Join(workspace, "requirements.txt")) ||
		fileExists(filepath.Join(workspace, "setup.py")) {
		addStack("python")
		testCmds = append(testCmds, "pytest")
	}

	// 4. Rust
	if fileExists(filepath.Join(workspace, "Cargo.toml")) {
		addStack("rust")
		buildCmds = append(buildCmds, "cargo build")
		testCmds = append(testCmds, "cargo test")
		checkCmds = append(checkCmds, "cargo check")
	}

	// 5. Java
	if fileExists(filepath.Join(workspace, "pom.xml")) {
		addStack("java")
		buildCmds = append(buildCmds, "mvn compile")
		testCmds = append(testCmds, "mvn test")
	}

	// Infra & aux markers
	auxDetectors := []struct {
		file string
		key  string
	}{
		{"Makefile", "has_makefile"},
		{"Dockerfile", "has_docker"},
		{"docker-compose.yml", "has_docker_compose"},
		{"docker-compose.yaml", "has_docker_compose"},
		{".env", "has_env"},
		{".env.example", "has_env"},
	}
	for _, d := range auxDetectors {
		if fileExists(filepath.Join(workspace, d.file)) {
			settings[d.key] = "true"
		}
	}

	if len(stacks) > 0 {
		settings["tech_stack"] = strings.Join(stacks, ",")
	}
	if len(buildCmds) > 0 {
		settings["build_cmd"] = strings.Join(buildCmds, " && ")
	}
	if len(testCmds) > 0 {
		settings["test_cmd"] = strings.Join(testCmds, " && ")
	}
	if len(checkCmds) > 0 {
		settings["check_cmd"] = strings.Join(checkCmds, " && ")
	}

	return settings
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
