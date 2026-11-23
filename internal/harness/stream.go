package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultStreamIdleTimeout = 45 * time.Second
	maxAPIErrorBodyBytes     = 64 * 1024
	maxAPICompletionBytes    = 10 * 1024 * 1024
)

type apiError struct {
	StatusCode  int
	RetryAfter  time.Duration
	Body        string
	IdleTimeout bool
	Err         error
}

func (e *apiError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("API error (%d): %s", e.StatusCode, e.Body)
}

func (e *apiError) Unwrap() error {
	return e.Err
}

type apiUsage struct {
	promptTokens     int
	completionTokens int
	cacheHit         int
	cacheMiss        int
	ttft             time.Duration
}

type wireUsage struct {
	PromptTokens             int `json:"prompt_tokens"`
	CompletionTokens         int `json:"completion_tokens"`
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	PromptCacheHitTokens     int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails      struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func normalizeWireUsage(u wireUsage) apiUsage {
	hit := u.PromptCacheHitTokens
	if hit == 0 {
		hit = u.PromptTokensDetails.CachedTokens
	}
	if hit == 0 {
		hit = u.CacheReadInputTokens
	}
	prompt := u.PromptTokens
	if prompt == 0 && u.InputTokens > 0 {
		prompt = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	}
	if prompt == 0 && (hit > 0 || u.PromptCacheMissTokens > 0) {
		prompt = hit + u.PromptCacheMissTokens
	}
	miss := u.PromptCacheMissTokens
	if miss == 0 && prompt >= hit {
		miss = prompt - hit
	}
	comp := u.CompletionTokens
	if comp == 0 {
		comp = u.OutputTokens
	}
	return apiUsage{
		promptTokens:     prompt,
		completionTokens: comp,
		cacheHit:         hit,
		cacheMiss:        miss,
	}
}

func parseRetryAfter(val string) time.Duration {
	s := strings.TrimSpace(val)
	if s == "" {
		return 0
	}
	if secs, err := strconv.Atoi(s); err == nil {
		if secs <= 0 {
			return 0
		}
		d := time.Duration(secs) * time.Second
		if d > maxRetryAfterHonored {
			return maxRetryAfterHonored
		}
		return d
	}
	if t, err := http.ParseTime(s); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0
		}
		if d > maxRetryAfterHonored {
			return maxRetryAfterHonored
		}
		return d
	}
	return 0
}

func emitDelta(onDelta func(kind, text string), kind, text string) {
	if onDelta != nil {
		onDelta(kind, text)
	}
}

// RunWithOutputStream is RunWithOutput with live token streaming. onDelta receives
// "content" and "reasoning" text deltas as the model produces them, and "endstep"
// when a step finished with tool calls (so the UI can close the in-progress block).
// Passing a nil onDelta behaves exactly like RunWithOutput.
func (r *Runner) RunWithOutputStream(ctx context.Context, prompt string, out io.Writer, onDelta func(kind, text string)) (string, error) {
	return r.runWithOptions(ctx, prompt, out, onDelta)
}

type streamChunk struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

func (r *Runner) doPostJSON(ctx context.Context, snap callSnapshot, targetURL string, payload map[string]any, acceptSSE bool) (*http.Response, error) {
	reqBody, _ := json.Marshal(payload)
	reqCtx := ctx
	var reqCancel context.CancelFunc
	var hdrMu sync.Mutex
	gotHeaders := false
	hdrTimedOut := false
	var hdrTimer *time.Timer

	if acceptSSE {
		reqCtx, reqCancel = context.WithCancel(ctx)
		hdrTimer = time.AfterFunc(snap.idleTimeout, func() {
			hdrMu.Lock()
			if !gotHeaders {
				hdrTimedOut = true
				reqCancel()
			}
			hdrMu.Unlock()
		})
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(reqBody))
	if err != nil {
		if hdrTimer != nil {
			hdrTimer.Stop()
		}
		if reqCancel != nil {
			reqCancel()
		}
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if acceptSSE {
		req.Header.Set("Accept", "text/event-stream")
	}
	if snap.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+snap.apiKey)
	}

	client := r.client
	if acceptSSE {
		var transport http.RoundTripper
		if r.client != nil {
			transport = r.client.Transport
		}
		client = &http.Client{Transport: transport}
	}

	resp, err := client.Do(req)
	if hdrTimer != nil {
		hdrMu.Lock()
		gotHeaders = true
		timedOutHdr := hdrTimedOut
		hdrMu.Unlock()
		hdrTimer.Stop()
		if err != nil {
			reqCancel()
			if timedOutHdr && ctx.Err() == nil {
				return nil, &apiError{
					IdleTimeout: true,
					Err:         fmt.Errorf("API stream header idle timeout after %s", snap.idleTimeout),
				}
			}
		} else {
			// Cancel derived context only after body is closed by wrapping resp.Body.
			resp.Body = cancelOnCloseReadCloser{ReadCloser: resp.Body, cancel: reqCancel}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	return resp, nil
}

type cancelOnCloseReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnCloseReadCloser) Close() error {
	err := c.ReadCloser.Close()
	if c.cancel != nil {
		c.cancel()
	}
	return err
}

func readAPIError(resp *http.Response) *apiError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIErrorBodyBytes))
	return &apiError{
		StatusCode: resp.StatusCode,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		Body:       string(body),
		Err:        fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body)),
	}
}

// streamRequest performs one chat-completions call over SSE and assembles the same
// chatMessage the buffered path returns (content, reasoning_content, tool_calls).
// It uses a per-chunk idle watchdog instead of a total-body timeout so long active
// multi-minute streams continue while stalled streams abort and recover cleanly.
func (r *Runner) streamRequest(ctx context.Context, snap callSnapshot, targetURL string, payload map[string]any, onDelta func(kind, text string)) (chatMessage, string, apiUsage, error) {
	reqStart := time.Now()
	resp, err := r.doPostJSON(ctx, snap, targetURL, payload, true)
	if err != nil {
		return chatMessage{}, "", apiUsage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return chatMessage{}, "", apiUsage{}, readAPIError(resp)
	}

	var content, reasoning, raw strings.Builder
	calls := map[int]*toolCall{}
	gotEvent := false
	finishReason := ""
	var usage apiUsage
	var ttftRecorded bool
	reader := bufio.NewReaderSize(resp.Body, 64*1024)

	var chunkTimedOut atomic.Bool
	gotDone := false
	for {
		if chunkTimedOut.Load() {
			return chatMessage{}, "", apiUsage{}, &apiError{
				IdleTimeout: true,
				Err:         fmt.Errorf("API stream idle timeout after %s", snap.idleTimeout),
			}
		}
		chunkTimer := time.AfterFunc(snap.idleTimeout, func() {
			chunkTimedOut.Store(true)
			_ = resp.Body.Close()
		})
		line, rerr := reader.ReadString('\n')
		chunkTimer.Stop()

		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				gotDone = true
				rerr = io.EOF
				break
			}
			var chunk streamChunk
			if json.Unmarshal([]byte(data), &chunk) != nil {
				break
			}
			gotEvent = true
			if !ttftRecorded {
				ttftRecorded = true
				usage.ttft = time.Since(reqStart)
			}
			if chunk.Usage != nil {
				u := normalizeWireUsage(*chunk.Usage)
				if u.promptTokens > 0 || u.completionTokens > 0 || u.cacheHit > 0 || u.cacheMiss > 0 {
					u.ttft = usage.ttft
					usage = u
				}
			}
			for _, ch := range chunk.Choices {
				if ch.FinishReason != "" {
					finishReason = ch.FinishReason
				}
				if ch.Delta.ReasoningContent != "" {
					reasoning.WriteString(ch.Delta.ReasoningContent)
					emitDelta(onDelta, "reasoning", ch.Delta.ReasoningContent)
				}
				if ch.Delta.Content != "" {
					content.WriteString(ch.Delta.Content)
					emitDelta(onDelta, "content", ch.Delta.Content)
				}
				for _, tc := range ch.Delta.ToolCalls {
					cur := calls[tc.Index]
					if cur == nil {
						cur = &toolCall{Type: "function"}
						calls[tc.Index] = cur
					}
					if tc.ID != "" {
						cur.ID = tc.ID
					}
					if tc.Type != "" {
						cur.Type = tc.Type
					}
					if tc.Function.Name != "" {
						cur.Function.Name += tc.Function.Name
					}
					cur.Function.Arguments += tc.Function.Arguments
				}
			}
		case trimmed != "" && !strings.HasPrefix(trimmed, ":") && !strings.HasPrefix(trimmed, "event:"):
			// Not SSE: some compatible servers ignore "stream" and return plain JSON.
			raw.WriteString(line)
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return chatMessage{}, "", apiUsage{}, fmt.Errorf("API stream interrupted: %w", rerr)
			}
			if !gotDone && chunkTimedOut.Load() {
				return chatMessage{}, "", apiUsage{}, &apiError{
					IdleTimeout: true,
					Err:         fmt.Errorf("API stream idle timeout after %s", snap.idleTimeout),
				}
			}
			if rerr != io.EOF {
				return chatMessage{}, "", apiUsage{}, fmt.Errorf("API stream interrupted: %w", rerr)
			}
			break
		}
	}

	if !gotEvent {
		if raw.Len() == 0 {
			return chatMessage{}, "", apiUsage{}, fmt.Errorf("invalid API response: empty stream")
		}
		msg, fr, u, perr := parseBufferedCompletion([]byte(raw.String()), onDelta)
		u.ttft = time.Since(reqStart)
		return msg, fr, u, perr
	}

	msg := chatMessage{Role: "assistant", Content: content.String(), ReasoningContent: reasoning.String()}
	if len(calls) > 0 {
		idx := make([]int, 0, len(calls))
		for i := range calls {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			msg.ToolCalls = append(msg.ToolCalls, *calls[i])
		}
		emitDelta(onDelta, "endstep", "")
	}
	return msg, finishReason, usage, nil
}

// parseBufferedCompletion decodes a non-streamed chat-completions body, shared between
// callAPI and SSE fallback when a server answers a streaming request with plain JSON.
func parseBufferedCompletion(body []byte, onDelta func(kind, text string)) (chatMessage, string, apiUsage, error) {
	var parsed struct {
		Choices []struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage *wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Choices) == 0 {
		return chatMessage{}, "", apiUsage{}, fmt.Errorf("invalid API response: %s", string(body))
	}
	var usage apiUsage
	if parsed.Usage != nil {
		usage = normalizeWireUsage(*parsed.Usage)
	}
	msg := parsed.Choices[0].Message
	if msg.Content != "" {
		emitDelta(onDelta, "content", msg.Content)
	}
	if len(msg.ToolCalls) > 0 {
		emitDelta(onDelta, "endstep", "")
	}
	return msg, parsed.Choices[0].FinishReason, usage, nil
}
