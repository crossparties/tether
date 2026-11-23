package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/croosparties/tether/internal/core"
)

// Two sub-agents run in parallel; each must report its own start, tool steps and verdict
// under its own ID so the UI can show every worker's process separately.
func TestSubagentObserverReportsEachWorkersProcess(t *testing.T) {
	engine, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	var mu sync.Mutex
	perTask := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := body.Messages[1].Content // the task text identifies the worker
		mu.Lock()
		perTask[key]++
		n := perTask[key]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		msg := map[string]interface{}{"role": "assistant"}
		if n == 1 {
			msg["tool_calls"] = []map[string]interface{}{{"id": "t", "type": "function",
				"function": map[string]interface{}{"name": "kb_status", "arguments": "{}"}}}
		} else {
			msg["content"] = "Findings for " + key + "\n- point one\n- point two\n\nSTATUS: READY_FOR_REVIEW"
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []map[string]interface{}{{"message": msg}}})
	}))
	defer srv.Close()

	r := NewRunner(engine, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "k", Out: &strings.Builder{}})
	var emu sync.Mutex
	events := map[int][]SubagentEvent{}
	r.SetSubagentObserver(func(ev SubagentEvent) {
		emu.Lock()
		events[ev.ID] = append(events[ev.ID], ev)
		emu.Unlock()
	})

	var wg sync.WaitGroup
	for _, c := range []struct{ role, task string }{{"tether_qa", "audit tests"}, {"tether_architect", "review layers"}} {
		wg.Add(1)
		go func(role, task string) {
			defer wg.Done()
			if _, err := r.SpawnSubagentWithOutput(context.Background(), role, task, nil); err != nil {
				t.Errorf("%s: %v", role, err)
			}
		}(c.role, c.task)
	}
	wg.Wait()

	if len(events) != 2 {
		t.Fatalf("expected events under 2 distinct IDs, got %d", len(events))
	}
	for id, evs := range events {
		first, last := evs[0], evs[len(evs)-1]
		if first.Kind != "start" || last.Kind != "done" {
			t.Fatalf("worker %d: lifecycle should be start..done, got %s..%s", id, first.Kind, last.Kind)
		}
		tools := 0
		for _, e := range evs {
			if e.Kind == "tool" && strings.Contains(e.Line, "kb_status") {
				tools++
			}
		}
		if tools != 1 {
			t.Fatalf("worker %d: expected 1 forwarded tool line, got %d (%+v)", id, tools, evs)
		}
		if last.Status != "READY_FOR_REVIEW" || last.Steps < 2 || len(last.Summary) == 0 || last.Err != "" {
			t.Fatalf("worker %d: bad verdict %+v", id, last)
		}
		for _, l := range last.Summary {
			if strings.HasPrefix(strings.ToUpper(l), "STATUS:") {
				t.Fatalf("summary must not repeat the STATUS trailer: %v", last.Summary)
			}
		}
	}
}
