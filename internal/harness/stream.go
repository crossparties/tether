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
	"strings"
)

// RunWithOutputStream is RunWithOutput with live token streaming. onDelta receives
// "content" and "reasoning" text deltas as the model produces them, and "endstep"
// when a step finished with tool calls (so the UI can close the in-progress block).
// Passing a nil onDelta behaves exactly like RunWithOutput.
func (r *Runner) RunWithOutputStream(ctx context.Context, prompt string, out io.Writer, onDelta func(kind, text string)) (string, error) {
	r.onDelta = onDelta
	defer func() { r.onDelta = nil }()
	return r.RunWithOutput(ctx, prompt, out)
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
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
	Usage *struct {
		PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
		PromptTokensDetails   struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// streamRequest performs one chat-completions call over SSE and assembles the same
// chatMessage the buffered path returns (content, reasoning_content, tool_calls).
// It uses a client without a total-body timeout: a long answer must not be cut at
// 120s, and cancellation is carried by ctx instead.
func (r *Runner) streamRequest(ctx context.Context, payload map[string]interface{}) (chatMessage, error) {
	reqBody, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.BaseURL, bytes.NewReader(reqBody))
	if err != nil {
		return chatMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if r.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	}

	client := &http.Client{Transport: r.client.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return chatMessage{}, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return chatMessage{}, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}

	var content, reasoning, raw strings.Builder
	calls := map[int]*toolCall{}
	gotEvent := false
	reader := bufio.NewReaderSize(resp.Body, 64*1024)

	for {
		line, rerr := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				rerr = io.EOF
				break
			}
			var chunk streamChunk
			if json.Unmarshal([]byte(data), &chunk) != nil {
				break
			}
			gotEvent = true
			if chunk.Usage != nil {
				hit := chunk.Usage.PromptCacheHitTokens
				if hit == 0 {
					hit = chunk.Usage.PromptTokensDetails.CachedTokens
				}
				if hit > 0 || chunk.Usage.PromptCacheMissTokens > 0 {
					r.lastCacheHitTokens = hit
					r.lastCacheMissTokens = chunk.Usage.PromptCacheMissTokens
				}
			}
			for _, ch := range chunk.Choices {
				if ch.Delta.ReasoningContent != "" {
					reasoning.WriteString(ch.Delta.ReasoningContent)
					r.onDelta("reasoning", ch.Delta.ReasoningContent)
				}
				if ch.Delta.Content != "" {
					content.WriteString(ch.Delta.Content)
					r.onDelta("content", ch.Delta.Content)
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
			if rerr != io.EOF {
				return chatMessage{}, fmt.Errorf("API stream interrupted: %w", rerr)
			}
			break
		}
	}

	if !gotEvent {
		if raw.Len() == 0 {
			return chatMessage{}, fmt.Errorf("invalid API response: empty stream")
		}
		return r.parseBufferedCompletion([]byte(raw.String()))
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
		r.onDelta("endstep", "")
	}
	return msg, nil
}

// parseBufferedCompletion decodes a non-streamed chat-completions body, used when a
// server answers a streaming request with plain JSON.
func (r *Runner) parseBufferedCompletion(body []byte) (chatMessage, error) {
	var parsed struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Choices) == 0 {
		return chatMessage{}, fmt.Errorf("invalid API response: %s", string(body))
	}
	msg := parsed.Choices[0].Message
	// Surface the whole answer as a single delta so the UI still shows it.
	if msg.Content != "" {
		r.onDelta("content", msg.Content)
	}
	if len(msg.ToolCalls) > 0 {
		r.onDelta("endstep", "")
	}
	return msg, nil
}
