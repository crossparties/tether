package main

import (
	"os"
	"regexp"
	"testing"
)

func TestSubcommandsDispatchedBeforeDefaultAndProposalRemoved(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	if regexp.MustCompile(`case "proposal"|func cmdProposal\(`).MatchString(text) {
		t.Fatal("legacy cmdProposal / proposal dispatcher must be removed")
	}

	defaultIdx := regexp.MustCompile(`(?m)^\tdefault:`).FindStringIndex(text)
	if defaultIdx == nil {
		t.Fatal("could not locate default arm in main()")
	}

	for _, sub := range []string{"init", "mcp", "hook", "mode", "undo", "status", "doctor", "install", "tui", "run", "version", "help"} {
		idx := regexp.MustCompile(`"` + sub + `"`).FindStringIndex(text)
		if idx == nil || idx[0] > defaultIdx[0] {
			t.Fatalf("subcommand %q must be dispatched before default arm", sub)
		}
	}
}
