package main

import (
	"os"

	"regexp"
	"testing"
)

// "tether proposal ..." must be routed to cmdProposal. Previously every unknown
// argument fell through to `default: cmdRun(...)`, so `tether proposal approve 1`
// was sent to the LLM as a chat prompt instead of performing a deterministic
// administrative action.
func TestProposalIsDispatchedNotSentToLLM(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	if !regexp.MustCompile(`case "proposal", "proposals":`).MatchString(text) {
		t.Fatal(`main() has no dispatcher case for "proposal" — the subcommand falls through to cmdRun and reaches the LLM`)
	}
	if !regexp.MustCompile(`cmdProposal\(os\.Args\[2:\]\)`).MatchString(text) {
		t.Fatal("the proposal dispatcher case does not call cmdProposal(os.Args[2:])")
	}

	// The default arm is what swallows unknown commands; make sure the proposal
	// case appears before it.
	proposalIdx := regexp.MustCompile(`case "proposal", "proposals":`).FindStringIndex(text)
	defaultIdx := regexp.MustCompile(`(?m)^\tdefault:`).FindStringIndex(text)
	if proposalIdx == nil || defaultIdx == nil {
		t.Fatal("could not locate dispatch case and default arm")
	}
	if proposalIdx[0] > defaultIdx[0] {
		t.Fatal("the proposal case is declared after default, so it can never match")
	}
}

func TestProposalCLIWritesWithoutLLM(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	start := regexp.MustCompile(`func cmdProposal\(`).FindStringIndex(text)
	if start == nil {
		t.Fatal("cmdProposal is not defined")
	}
	// Take the function body up to the next top-level func.
	rest := text[start[0]:]
	end := regexp.MustCompile(`\nfunc `).FindStringIndex(rest[1:])
	body := rest
	if end != nil {
		body = rest[:end[0]+1]
	}

	for _, forbidden := range []string{"harness.NewRunner", "runner.Run(", ".CallTool(", "cmdRun("} {
		if regexp.MustCompile(regexp.QuoteMeta(forbidden)).MatchString(body) {
			t.Fatalf("cmdProposal must not invoke the model path (%q found); the user's decision has to go straight to the database", forbidden)
		}
	}
	for _, required := range []string{"ApproveProposalByID", "RejectProposalByID", "ListProposals"} {
		if !regexp.MustCompile(regexp.QuoteMeta(required)).MatchString(body) {
			t.Fatalf("cmdProposal does not use %s", required)
		}
	}

}
