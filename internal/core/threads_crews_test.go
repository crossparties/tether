package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestConversationThreadsAndEventsCRUD(t *testing.T) {
	e := newTestEngine(t)

	th, err := e.CreateThread("th-1", "Refactor Auth", "plan")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if th.ID != "th-1" || th.Title != "Refactor Auth" || th.Mode != "plan" {
		t.Fatalf("unexpected thread: %+v", th)
	}

	if err := e.UpdateThreadTitle("th-1", "Refactor Auth v2"); err != nil {
		t.Fatalf("UpdateThreadTitle: %v", err)
	}
	loaded, err := e.GetThread("th-1")
	if err != nil || loaded.Title != "Refactor Auth v2" {
		t.Fatalf("GetThread after rename: %+v err=%v", loaded, err)
	}

	// Monotonic seq + crew filtering + >16KB payload compression (must remain valid JSON).
	ev1, err := e.AppendThreadEvent(ConversationEvent{
		ThreadID:    "th-1",
		Role:        "user",
		UsageID:     "main",
		PayloadJSON: `{"role":"user","content":"start"}`,
	})
	if err != nil || ev1.Seq != 1 {
		t.Fatalf("AppendThreadEvent #1: %+v err=%v", ev1, err)
	}

	bigPayload, _ := json.Marshal(map[string]any{
		"role":    "tool",
		"content": strings.Repeat("A", 20000),
	})
	ev2, err := e.AppendThreadEvent(ConversationEvent{
		ThreadID:    "th-1",
		CrewID:      "crew-abc",
		Role:        "tool",
		PayloadJSON: string(bigPayload),
	})
	if err != nil || ev2.Seq != 2 || ev2.UsageID != "crew:crew-abc" {
		t.Fatalf("AppendThreadEvent #2: %+v err=%v", ev2, err)
	}
	if len(ev2.PayloadJSON) > maxStoredEventPayloadBytes {
		t.Fatalf("expected compressed payload <= %d bytes, got %d", maxStoredEventPayloadBytes, len(ev2.PayloadJSON))
	}
	if !json.Valid([]byte(ev2.PayloadJSON)) {
		t.Fatalf("compressed payload must remain valid JSON: %s", ev2.PayloadJSON)
	}

	allEvs, err := e.ListThreadEvents("th-1", 50)
	if err != nil || len(allEvs) != 2 {
		t.Fatalf("ListThreadEvents: len=%d err=%v", len(allEvs), err)
	}
	crewEvs, err := e.ListThreadEventsForActor("th-1", "crew-abc", 50)
	if err != nil || len(crewEvs) != 1 || crewEvs[0].Seq != 2 {
		t.Fatalf("ListThreadEventsForActor: %+v err=%v", crewEvs, err)
	}

	threads, err := e.ListThreads(10)
	if err != nil || len(threads) == 0 {
		t.Fatalf("ListThreads: len=%d err=%v", len(threads), err)
	}

	if err := e.DeleteThread("th-1"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	after, err := e.ListThreadEvents("th-1", 50)
	if err != nil || len(after) != 0 {
		t.Fatalf("expected 0 events after DeleteThread, got %d err=%v", len(after), err)
	}
}

func TestCrewSessionsCRUD(t *testing.T) {
	e := newTestEngine(t)

	exp := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	err := e.CreateCrewSession(CrewSession{
		ID:              "crew-1",
		ParentThreadID:  e.SessionID,
		Name:            "implementer",
		SkillName:       "senior-programmer",
		PurposeCategory: "implement",
		Status:          "active",
		LeaseFiles:      []string{"internal/core/threads.go"},
		LeaseExpiresAt:  exp,
	})
	if err != nil {
		t.Fatalf("CreateCrewSession: %v", err)
	}

	got, err := e.GetCrewSession("crew-1")
	if err != nil {
		t.Fatalf("GetCrewSession: %v", err)
	}
	if got.Name != "implementer" || len(got.LeaseFiles) != 1 || got.LeaseFiles[0] != "internal/core/threads.go" {
		t.Fatalf("unexpected crew session: %+v", got)
	}

	list, err := e.ListCrewSessions(e.SessionID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListCrewSessions: len=%d err=%v", len(list), err)
	}

	if err := e.CloseCrewSession("crew-1", "closed", "done implementing"); err != nil {
		t.Fatalf("CloseCrewSession: %v", err)
	}
	closed, err := e.GetCrewSession("crew-1")
	if err != nil || closed.Status != "closed" || len(closed.LeaseFiles) != 0 || closed.Summary != "done implementing" {
		t.Fatalf("unexpected closed crew session: %+v err=%v", closed, err)
	}
}

func TestListThreadEventsForLoadAndOversizedToolCallArgs(t *testing.T) {
	ws := t.TempDir()
	e, err := NewEngine(ws)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer e.Close()

	if _, err := e.CreateThread("thr-510", "510 event reload", "code"); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Insert 509 regular events + 1 final assistant tool_call with >16KB arguments.
	for i := 1; i <= 509; i++ {
		if _, err := e.AppendThreadEvent(ConversationEvent{
			ThreadID:    "thr-510",
			Role:        "user",
			UsageID:     "main",
			PayloadJSON: fmt.Sprintf(`{"role":"user","content":"msg-%03d"}`, i),
			TokenEst:    4,
		}); err != nil {
			t.Fatalf("AppendThreadEvent %d: %v", i, err)
		}
	}

	hugeBody := strings.Repeat("x", 20*1024)
	hugeArgs, _ := json.Marshal(map[string]any{"path": "big.txt", "content": hugeBody})
	hugeMsg, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"content": "",
		"tool_calls": []map[string]any{{
			"id":   "tc-huge",
			"type": "function",
			"function": map[string]any{
				"name":      "write_file",
				"arguments": string(hugeArgs),
			},
		}},
	})
	if _, err := e.AppendThreadEvent(ConversationEvent{
		ThreadID:    "thr-510",
		Role:        "assistant",
		UsageID:     "main",
		PayloadJSON: string(hugeMsg),
		TokenEst:    5000,
	}); err != nil {
		t.Fatalf("AppendThreadEvent 510: %v", err)
	}

	loaded, err := e.ListThreadEventsForLoad("thr-510", "", 500)
	if err != nil {
		t.Fatalf("ListThreadEventsForLoad: %v", err)
	}
	if len(loaded) != 500 {
		t.Fatalf("expected newest 500 events, got %d", len(loaded))
	}
	if loaded[0].Seq != 11 || loaded[len(loaded)-1].Seq != 510 {
		t.Fatalf("expected seq 11..510 in ASC order, got %d..%d", loaded[0].Seq, loaded[len(loaded)-1].Seq)
	}

	last := loaded[len(loaded)-1]
	var decoded struct {
		Role      string `json:"role"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(last.PayloadJSON), &decoded); err != nil {
		t.Fatalf("decode oversized tool_call payload: %v raw=%s", err, last.PayloadJSON)
	}
	if decoded.Role != "assistant" || len(decoded.ToolCalls) != 1 || decoded.ToolCalls[0].ID != "tc-huge" || decoded.ToolCalls[0].Function.Name != "write_file" {
		t.Fatalf("expected assistant role & tool_call metadata preserved, got %+v", decoded)
	}
}
