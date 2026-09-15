package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// DetectProjectSettings scans project root for config files and detects tech stack
func DetectProjectSettings(workspace string) map[string]string {
	settings := make(map[string]string)

	// Detect by config files
	detectors := []struct {
		file      string
		key       string
		value     string
		buildCmd  string
		testCmd   string
	}{
		{"go.mod", "tech_stack", "go", "go build ./...", "go test ./..."},
		{"Cargo.toml", "tech_stack", "rust", "cargo build", "cargo test"},
		{"package.json", "tech_stack", "node", "npm run build", "npm test"},
		{"pom.xml", "tech_stack", "java", "mvn compile", "mvn test"},
		{"Makefile", "has_makefile", "true", "make", "make test"},
		{"Dockerfile", "has_docker", "true", "", ""},
		{"docker-compose.yml", "has_docker_compose", "true", "", ""},
		{"docker-compose.yaml", "has_docker_compose", "true", "", ""},
		{".env", "has_env", "true", "", ""},
		{".env.example", "has_env", "true", "", ""},
	}

	for _, d := range detectors {
		if fileExists(filepath.Join(workspace, d.file)) {
			settings[d.key] = d.value
			if d.buildCmd != "" {
				settings["build_cmd"] = d.buildCmd
			}
			if d.testCmd != "" {
				settings["test_cmd"] = d.testCmd
			}
		}
	}

	// Detect framework from go.mod
	gomod := filepath.Join(workspace, "go.mod")
	if content, err := os.ReadFile(gomod); err == nil {
		text := string(content)
		if strings.Contains(text, "github.com/gofiber/fiber") {
			settings["framework"] = "fiber"
		} else if strings.Contains(text, "github.com/gin-gonic/gin") {
			settings["framework"] = "gin"
		} else if strings.Contains(text, "github.com/labstack/echo") {
			settings["framework"] = "echo"
		}
	}

	// Detect framework from package.json
	pkgjson := filepath.Join(workspace, "package.json")
	if content, err := os.ReadFile(pkgjson); err == nil {
		text := string(content)
		if strings.Contains(text, "\"next\"") {
			settings["framework"] = "nextjs"
		} else if strings.Contains(text, "\"react\"") {
			settings["framework"] = "react"
		} else if strings.Contains(text, "\"vue\"") {
			settings["framework"] = "vue"
		} else if strings.Contains(text, "\"express\"") {
			settings["framework"] = "express"
		}
	}

	return settings
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
