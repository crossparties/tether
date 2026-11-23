package harness

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBtwSideChatZeroContextPollution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Isolated /btw answer without touching main history.",
					},
				},
			},
		})
	}))
	defer srv.Close()

	var out bytes.Buffer
	r := newTestRunner(t, &out)
	r.cfg.BaseURL = srv.URL

	// Seed main runner history with 2 messages
	r.history = []chatMessage{
		{Role: "system", Content: "main system"},
		{Role: "user", Content: "main task prompt"},
	}
	beforeHistoryLen := len(r.history)
	beforeCtx, _ := r.engine.GetRecentContext(20)

	ans, err := r.RunSideChat(t.Context(), "what is a goroutine?")
	if err != nil {
		t.Fatalf("RunSideChat failed: %v", err)
	}
	if !strings.Contains(ans, "Isolated /btw answer") {
		t.Fatalf("unexpected /btw answer: %q", ans)
	}

	// Verify r.history was NOT mutated
	if len(r.history) != beforeHistoryLen {
		t.Fatalf("/btw polluted r.history: len changed from %d to %d", beforeHistoryLen, len(r.history))
	}

	// Verify session.db context was NOT mutated
	afterCtx, _ := r.engine.GetRecentContext(20)
	if len(afterCtx) != len(beforeCtx) {
		t.Fatalf("/btw polluted session.db context: count changed from %d to %d", len(beforeCtx), len(afterCtx))
	}
}

func TestReadWorkspaceFileBlocksTraversal(t *testing.T) {
	var out bytes.Buffer
	r := newTestRunner(t, &out)

	insideFile := filepath.Join(r.engine.Workspace, "hello.txt")
	if err := os.WriteFile(insideFile, []byte("safe workspace content"), 0644); err != nil {
		t.Fatalf("write inside file: %v", err)
	}

	got, err := r.ReadWorkspaceFile("@hello.txt")
	if err != nil {
		t.Fatalf("expected valid workspace file read to succeed, got: %v", err)
	}
	if got != "safe workspace content" {
		t.Fatalf("got %q, want %q", got, "safe workspace content")
	}

	if _, err := r.ReadWorkspaceFile("../../etc/passwd"); err == nil {
		t.Fatalf("expected path traversal '../../etc/passwd' to be rejected")
	}
}
