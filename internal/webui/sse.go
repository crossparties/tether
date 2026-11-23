package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sseRingCapacity = 512

// SSEEvent is one numbered event stored in the multiplexed SSE ring buffer.
type SSEEvent struct {
	ID        int64           `json:"id"`
	Event     string          `json:"event"`
	Data      json.RawMessage `json:"data"`
	Timestamp string          `json:"timestamp"`
}

// EventHub multiplexes live runtime events to SSE subscribers and retains a
// bounded 512-event ring buffer for Last-Event-ID replay.
type EventHub struct {
	mu       sync.RWMutex
	nextID   int64
	nextSub  uint64
	ring     []SSEEvent
	capacity int
	subs     map[uint64]chan SSEEvent
	closed   bool
}

// NewEventHub constructs a multiplexed EventHub with a 512-event ring buffer.
func NewEventHub(capacity int) *EventHub {
	if capacity <= 0 {
		capacity = sseRingCapacity
	}
	return &EventHub{
		capacity: capacity,
		subs:     make(map[uint64]chan SSEEvent),
	}
}

// Publish assigns a strictly increasing integer ID, appends to the ring buffer,
// and fans out to active subscribers without blocking on slow consumers.
func (h *EventHub) Publish(event string, payload any) SSEEvent {
	var raw json.RawMessage
	switch v := payload.(type) {
	case json.RawMessage:
		raw = v
	case []byte:
		raw = json.RawMessage(v)
	case string:
		trimmed := strings.TrimSpace(v)
		if json.Valid([]byte(trimmed)) && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) {
			raw = json.RawMessage(trimmed)
		} else {
			raw, _ = json.Marshal(map[string]any{"detail": v})
		}
	default:
		b, err := json.Marshal(v)
		if err != nil {
			b, _ = json.Marshal(map[string]any{"error": err.Error()})
		}
		raw = b
	}

	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		raw = compact.Bytes()
	} else {
		raw, _ = json.Marshal(map[string]any{"detail": string(raw)})
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return SSEEvent{}
	}
	h.nextID++
	ev := SSEEvent{
		ID:        h.nextID,
		Event:     event,
		Data:      raw,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if len(h.ring) >= h.capacity {
		copy(h.ring, h.ring[1:])
		h.ring[len(h.ring)-1] = ev
	} else {
		h.ring = append(h.ring, ev)
	}
	for id, ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// Drop stalled subscriber rather than stalling Runner/chat execution.
			close(ch)
			delete(h.subs, id)
		}
	}
	return ev
}

// ReplaySince returns all buffered events with ID > lastID in ascending order.
func (h *EventHub) ReplaySince(lastID int64) []SSEEvent {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var out []SSEEvent
	for _, ev := range h.ring {
		if ev.ID > lastID {
			out = append(out, ev)
		}
	}
	return out
}

// LatestID returns the highest published event ID.
func (h *EventHub) LatestID() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.nextID
}

// Subscribe registers a new subscriber channel.
func (h *EventHub) Subscribe() (uint64, chan SSEEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, nil, false
	}
	h.nextSub++
	id := h.nextSub
	ch := make(chan SSEEvent, 128)
	h.subs[id] = ch
	return id, ch, true
}

// Unsubscribe removes a subscriber channel safely.
func (h *EventHub) Unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		close(ch)
		delete(h.subs, id)
	}
}

// Close closes all active SSE subscriber channels.
func (h *EventHub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, ch := range h.subs {
		close(ch)
		delete(h.subs, id)
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}

	rawLastID := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if rawLastID == "" {
		rawLastID = strings.TrimSpace(r.URL.Query().Get("lastEventId"))
	}
	var lastID int64
	hasLastID := false
	if rawLastID != "" {
		if parsed, err := strconv.ParseInt(rawLastID, 10, 64); err == nil && parsed >= 0 {
			lastID = parsed
			hasLastID = true
		}
	}

	subID, ch, ok := s.hub.Subscribe()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "event hub closed")
		return
	}
	defer s.hub.Unsubscribe(subID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	_, run := s.activeState()
	threadID := ""
	if run != nil {
		threadID = run.ThreadID()
	}
	latestID := s.hub.LatestID()
	connPayload, _ := json.Marshal(map[string]any{
		"thread_id":     threadID,
		"last_event_id": latestID,
	})
	fmt.Fprintf(w, "event: connected\ndata: %s\n\n", connPayload)
	_ = rc.Flush()

	var maxSentID int64
	if hasLastID {
		for _, ev := range s.hub.ReplaySince(lastID) {
			writeSSEFrame(w, ev)
			if ev.ID > maxSentID {
				maxSentID = ev.ID
			}
		}
		_ = rc.Flush()
	}

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.shutdownCh:
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			if hasLastID && ev.ID <= maxSentID {
				continue
			}
			writeSSEFrame(w, ev)
			if ev.ID > maxSentID {
				maxSentID = ev.ID
			}
			_ = rc.Flush()
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			_ = rc.Flush()
		}
	}
}

func writeSSEFrame(w http.ResponseWriter, ev SSEEvent) {
	data := strings.TrimSpace(string(ev.Data))
	if data == "" {
		data = "{}"
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Event, data)
}
