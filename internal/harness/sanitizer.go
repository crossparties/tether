package harness

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// SanitizeOptions configures cross-provider conversation normalization.
type SanitizeOptions struct {
	TargetProvider    string
	TargetModel       string
	PreserveReasoning bool
}

// SanitizeMessages deterministically and idempotently normalizes a chat transcript
// before API dispatch or after failover/stream reset:
//  1. Strips provider-specific ReasoningContent when targeting non-DeepSeek endpoints
//     and drops any resulting empty assistant messages.
//  2. Maps ToolCalls[i].ID and matching tool.ToolCallID 1-to-1 to <= 40-char [a-zA-Z0-9_-] IDs.
//  3. Normalizes empty/whitespace tool call arguments to "{}".
//  4. Inserts synthetic tool responses for orphaned assistant tool_calls.
//  5. Merges consecutive same-role non-tool turns.
func SanitizeMessages(messages []chatMessage, opts SanitizeOptions) []chatMessage {
	if len(messages) == 0 {
		return nil
	}

	targetProv := strings.TrimSpace(opts.TargetProvider)
	stripReasoning := !opts.PreserveReasoning && targetProv != "" && !strings.EqualFold(targetProv, "deepseek")

	// Pass 1 & 2: Copy messages, strip incompatible reasoning_content, drop empty assistant messages.
	cleaned := make([]chatMessage, 0, len(messages))
	for _, m := range messages {
		cp := m
		if len(m.ToolCalls) > 0 {
			cp.ToolCalls = make([]toolCall, len(m.ToolCalls))
			copy(cp.ToolCalls, m.ToolCalls)
		}
		if stripReasoning {
			cp.ReasoningContent = ""
			cp.Prefix = false
		}
		if cp.Role == "assistant" && strings.TrimSpace(cp.Content) == "" && cp.ReasoningContent == "" && len(cp.ToolCalls) == 0 {
			continue
		}
		cleaned = append(cleaned, cp)
	}
	if len(cleaned) == 0 {
		return nil
	}

	// Pass 3: Deterministic 1-to-1 tool_call_id normalization + empty arguments -> "{}".
	usedIDs := make(map[string]bool, len(cleaned)*2)
	globalIDMap := make(map[string]string)
	var turnIDMap map[string][]string
	var pendingTurnIDs []string
	var pendingEmptyIDs []string

	for i := range cleaned {
		m := &cleaned[i]
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			turnIDMap = make(map[string][]string, len(m.ToolCalls))
			pendingTurnIDs = pendingTurnIDs[:0]
			pendingEmptyIDs = pendingEmptyIDs[:0]
			for j := range m.ToolCalls {
				tc := &m.ToolCalls[j]
				if tc.Type == "" {
					tc.Type = "function"
				}
				if strings.TrimSpace(tc.Function.Arguments) == "" {
					tc.Function.Arguments = "{}"
				}
				origID := tc.ID
				newID := normalizeToolCallID(origID, i, j, tc.Function.Name, usedIDs)
				tc.ID = newID
				pendingTurnIDs = append(pendingTurnIDs, newID)
				if origID == "" {
					pendingEmptyIDs = append(pendingEmptyIDs, newID)
				} else {
					turnIDMap[origID] = append(turnIDMap[origID], newID)
					globalIDMap[origID] = newID
				}
			}
		case m.Role == "tool":
			origID := m.ToolCallID
			if origID != "" {
				if q := turnIDMap[origID]; len(q) > 0 {
					mapped := q[0]
					turnIDMap[origID] = q[1:]
					m.ToolCallID = mapped
					pendingTurnIDs = removePendingID(pendingTurnIDs, mapped)
					pendingEmptyIDs = removePendingID(pendingEmptyIDs, mapped)
				} else if mapped, ok := globalIDMap[origID]; ok {
					m.ToolCallID = mapped
					pendingTurnIDs = removePendingID(pendingTurnIDs, mapped)
					pendingEmptyIDs = removePendingID(pendingEmptyIDs, mapped)
				} else if !isValidToolCallID(origID) {
					m.ToolCallID = hashToolCallID(origID)
				}
			} else if len(pendingEmptyIDs) > 0 {
				m.ToolCallID = pendingEmptyIDs[0]
				pendingEmptyIDs = pendingEmptyIDs[1:]
				pendingTurnIDs = removePendingID(pendingTurnIDs, m.ToolCallID)
			} else if len(pendingTurnIDs) > 0 {
				m.ToolCallID = pendingTurnIDs[0]
				pendingTurnIDs = pendingTurnIDs[1:]
			} else {
				m.ToolCallID = normalizeToolCallID("", i, 0, "tool", usedIDs)
			}
		default:
			if m.Role != "tool" {
				turnIDMap = nil
				pendingTurnIDs = nil
				pendingEmptyIDs = nil
			}
		}
	}

	// Pass 4: Orphaned tool-call repair (ensure every assistant tool_call has a matching tool message).
	repaired := make([]chatMessage, 0, len(cleaned))
	for i := 0; i < len(cleaned); i++ {
		m := cleaned[i]
		repaired = append(repaired, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}

		answered := make(map[string]bool, len(m.ToolCalls))
		j := i + 1
		for j < len(cleaned) && cleaned[j].Role == "tool" {
			repaired = append(repaired, cleaned[j])
			if cleaned[j].ToolCallID != "" {
				answered[cleaned[j].ToolCallID] = true
			}
			j++
		}
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				repaired = append(repaired, chatMessage{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    "[tool execution interrupted or canceled before completion]",
				})
				answered[tc.ID] = true
			}
		}
		i = j - 1
	}

	// Pass 5: Merge consecutive same-role non-tool turns safely.
	out := make([]chatMessage, 0, len(repaired))
	for _, m := range repaired {
		if len(out) > 0 && out[len(out)-1].Role == m.Role && m.Role != "tool" {
			prev := &out[len(out)-1]
			if prev.Content != "" && m.Content != "" {
				prev.Content += "\n\n" + m.Content
			} else if m.Content != "" {
				prev.Content = m.Content
			}
			if m.Role == "assistant" {
				if prev.ReasoningContent != "" && m.ReasoningContent != "" {
					prev.ReasoningContent += "\n" + m.ReasoningContent
				} else if m.ReasoningContent != "" {
					prev.ReasoningContent = m.ReasoningContent
				}
				if len(m.ToolCalls) > 0 {
					prev.ToolCalls = append(prev.ToolCalls, m.ToolCalls...)
				}
				if m.Prefix {
					prev.Prefix = true
				}
			}
			continue
		}
		out = append(out, m)
	}

	return out
}

func isValidToolCallID(id string) bool {
	if len(id) == 0 || len(id) > 40 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlphaNum && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func hashToolCallID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("call_%x", sum[:12]) // 29 chars, strictly [a-z0-9_]
}

func normalizeToolCallID(orig string, msgIdx, tcIdx int, fnName string, used map[string]bool) string {
	if isValidToolCallID(orig) && !used[orig] {
		used[orig] = true
		return orig
	}
	var cand string
	if orig != "" && !isValidToolCallID(orig) {
		cand = hashToolCallID(orig)
	} else {
		cand = hashToolCallID(fmt.Sprintf("%s#%d#%d#%s", orig, msgIdx, tcIdx, fnName))
	}
	for salt := 1; used[cand]; salt++ {
		cand = hashToolCallID(fmt.Sprintf("%s#%d#%d#%s#%d", orig, msgIdx, tcIdx, fnName, salt))
	}
	used[cand] = true
	return cand
}

func removePendingID(ids []string, target string) []string {
	for i, id := range ids {
		if id == target {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}
