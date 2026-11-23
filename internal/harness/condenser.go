package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/crossparties/tether/internal/core"
)

const (
	defaultCondenseRatio     = 0.72
	defaultCondenseMaxEvents = 40
	defaultCondenseKeepTail  = 4
)

// CondenserConfig controls EpisodicCondenser's dual-threshold triggers.
type CondenserConfig struct {
	ContextWindowTokens int
	ThresholdRatio      float64
	MaxEvents           int
	KeepTailMessages    int
}

// CondenseResult reports whether condensation occurred and what span was replaced.
type CondenseResult struct {
	Condensed      bool   `json:"condensed"`
	Reason         string `json:"reason,omitempty"`
	CondensedCount int    `json:"condensed_count"`
	RemainingCount int    `json:"remaining_count"`
	Summary        string `json:"summary,omitempty"`
}

func (r *Runner) condenserConfig() CondenserConfig {
	r.mu.RLock()
	cfgCtx := r.cfg.ContextWindow
	cfgRatio := r.cfg.CondenseRatio
	cfgMaxEv := r.cfg.CondenseMaxEvents
	eng := r.engine
	r.mu.RUnlock()

	cc := CondenserConfig{
		ContextWindowTokens: defaultContextWindowTokens,
		ThresholdRatio:      defaultCondenseRatio,
		MaxEvents:           defaultCondenseMaxEvents,
		KeepTailMessages:    defaultCondenseKeepTail,
	}
	if cfgCtx > 0 {
		cc.ContextWindowTokens = cfgCtx
	} else if eng != nil {
		if v, err := eng.GetProjectSetting("context_window"); err == nil && v != "" {
			if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
				cc.ContextWindowTokens = n
			}
		}
	}
	if cfgRatio > 0 && cfgRatio <= 1.0 {
		cc.ThresholdRatio = cfgRatio
	} else if eng != nil {
		if v, err := eng.GetProjectSetting("condense_threshold_ratio"); err == nil && v != "" {
			if f, perr := strconv.ParseFloat(v, 64); perr == nil && f > 0 && f <= 1.0 {
				cc.ThresholdRatio = f
			}
		}
	}
	if cfgMaxEv > 0 {
		cc.MaxEvents = cfgMaxEv
	} else if eng != nil {
		if v, err := eng.GetProjectSetting("condense_max_events"); err == nil && v != "" {
			if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
				cc.MaxEvents = n
			}
		}
	}
	return cc
}

// EstimateMessagesTokens approximates total token footprint across messages.
func EstimateMessagesTokens(messages []chatMessage) int {
	total := 0
	for _, m := range messages {
		chars := len(m.Content) + len(m.ReasoningContent) + len(m.Role) + len(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Function.Name) + len(tc.Function.Arguments) + len(tc.ID)
		}
		total += (chars+3)/4 + 4
	}
	return total
}

// ShouldCondense checks the dual-threshold trigger:
// estimated_tokens >= ratio * context_window OR event_count >= max_events.
func (r *Runner) ShouldCondense(messages []chatMessage) (bool, string) {
	if len(messages) <= 2 {
		return false, ""
	}
	cc := r.condenserConfig()
	estTokens := EstimateMessagesTokens(messages)
	tokLimit := int(float64(cc.ContextWindowTokens) * cc.ThresholdRatio)
	eventCount := len(messages)
	if messages[0].Role == "system" {
		eventCount--
	}
	if tokLimit > 0 && estTokens >= tokLimit {
		return true, fmt.Sprintf("estimated_tokens=%d >= %.2f*%d (%d)", estTokens, cc.ThresholdRatio, cc.ContextWindowTokens, tokLimit)
	}
	if cc.MaxEvents > 0 && eventCount >= cc.MaxEvents {
		return true, fmt.Sprintf("event_count=%d >= max_events=%d", eventCount, cc.MaxEvents)
	}
	return false, ""
}

// CondenseHistory summarizes older conversation events into the 5 mandatory
// slots (using usage_id: "condenser"), persists the condensation event BEFORE
// mutating state, and atomically replaces the condensed span while keeping
// Tier 0-3 messages[0] completely immutable.
func (r *Runner) CondenseHistory(ctx context.Context, force bool) (CondenseResult, error) {
	r.mu.RLock()
	snapshot := make([]chatMessage, len(r.history))
	copy(snapshot, r.history)
	r.mu.RUnlock()

	condensed, res, err := r.condenseMessages(ctx, snapshot, force)
	if err != nil || !res.Condensed {
		return res, err
	}

	r.mu.Lock()
	r.history = condensed
	r.invalidateCondensedFileHashesLocked(condensed)
	r.mu.Unlock()
	return res, nil
}

func (r *Runner) condenseMessages(ctx context.Context, messages []chatMessage, force bool) ([]chatMessage, CondenseResult, error) {
	if len(messages) <= 2 {
		return messages, CondenseResult{}, nil
	}
	reason := "manual /compact"
	if !force {
		ok, why := r.ShouldCondense(messages)
		if !ok {
			return messages, CondenseResult{}, nil
		}
		reason = why
	}

	cc := r.condenserConfig()
	splitIdx := findSafeCondenseSplit(messages, cc.KeepTailMessages)
	if splitIdx <= 1 || (splitIdx == 2 && isEpisodicSummaryMsg(messages[1])) {
		return messages, CondenseResult{}, nil
	}

	head := messages[0]
	oldSpan := messages[1:splitIdx]
	tailSpan := messages[splitIdx:]

	summary := r.summarizeSpanWithCondenser(ctx, oldSpan)

	// Persist condensation event to session.db BEFORE atomically replacing active span.
	r.mu.RLock()
	eng := r.engine
	threadID := r.effectiveThreadIDLocked()
	crewID := r.crewID
	r.mu.RUnlock()

	if eng != nil && threadID != "" {
		payloadBytes, _ := json.Marshal(map[string]any{
			"reason":          reason,
			"condensed_count": len(oldSpan),
			"summary":         summary,
		})
		if _, err := eng.AppendThreadEvent(core.ConversationEvent{
			ThreadID:    threadID,
			CrewID:      crewID,
			Role:        "condensation",
			UsageID:     "condenser",
			PayloadJSON: string(payloadBytes),
			TokenEst:    (len(summary) + 3) / 4,
		}); err != nil {
			return messages, CondenseResult{}, fmt.Errorf("persist condensation event: %w", err)
		}
	}

	summaryMsg := chatMessage{
		Role:    "user",
		Content: "[Episodic Condensation Summary]\n" + summary,
	}
	out := make([]chatMessage, 0, 2+len(tailSpan))
	out = append(out, head, summaryMsg)
	out = append(out, tailSpan...)

	res := CondenseResult{
		Condensed:      true,
		Reason:         reason,
		CondensedCount: len(oldSpan),
		RemainingCount: len(out),
		Summary:        summary,
	}
	if payload, merr := json.Marshal(res); merr == nil {
		r.notifyEvent("condenser.ran", string(payload))
	}
	return out, res, nil
}

func isEpisodicSummaryMsg(m chatMessage) bool {
	return m.Role == "user" && strings.HasPrefix(m.Content, "[Episodic Condensation Summary]")
}

// findSafeCondenseSplit chooses an index >= 2 in messages so messages[1:splitIdx]
// can be condensed without splitting any assistant(tool_calls) from its tool responses
// or infinitely re-condensing an existing 1-message summary.
func findSafeCondenseSplit(messages []chatMessage, keepTail int) int {
	if keepTail < 2 {
		keepTail = 2
	}
	n := len(messages)
	if n <= 3 {
		return -1
	}
	minSplit := 2
	if isEpisodicSummaryMsg(messages[1]) {
		minSplit = 3
	}
	lastGroupStart := -1
	for i := n - 1; i >= minSplit; i-- {
		if messages[i].Role != "tool" {
			lastGroupStart = i
			break
		}
	}
	if lastGroupStart < minSplit {
		return -1
	}

	target := n - keepTail
	if target > lastGroupStart {
		target = lastGroupStart
	}

	// Prefer starting tail on a user message boundary at or before target.
	for i := target; i >= minSplit; i-- {
		if messages[i].Role == "user" {
			return i
		}
	}
	// Fallback: any non-tool boundary at or before target so tool responses never orphan.
	for i := target; i >= minSplit; i-- {
		if messages[i].Role != "tool" {
			return i
		}
	}
	// Long single-turn tool loop: target landed before minSplit (e.g. messages[1] is already
	// a summary and messages[2] starts a huge tool chain). Advance forward past target while
	// preserving at least the latest assistant+tool group in tail.
	forwardStart := max(minSplit, target+1)
	for i := forwardStart; i <= lastGroupStart; i++ {
		if messages[i].Role == "user" {
			return i
		}
	}
	for i := forwardStart; i <= lastGroupStart; i++ {
		if messages[i].Role != "tool" {
			return i
		}
	}
	return -1
}

func (r *Runner) summarizeSpanWithCondenser(ctx context.Context, oldSpan []chatMessage) string {
	detSummary := buildDeterministicCondenseSummary(oldSpan)
	if !r.hasConfiguredProvider() {
		return detSummary
	}

	var transcript strings.Builder
	for _, m := range oldSpan {
		content := clipTextRunes(strings.TrimSpace(m.Content), 600)
		if content == "" && len(m.ToolCalls) > 0 {
			var names []string
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			content = "tool_calls: " + strings.Join(names, ", ")
		}
		fmt.Fprintf(&transcript, "[%s] %s\n", m.Role, content)
	}

	condenseMsgs := []chatMessage{
		{
			Role: "system",
			Content: "Summarize the conversation span into EXACTLY these 5 numbered markdown sections:\n" +
				"1. Goal:\n2. Modified Files & Line State:\n3. Active Errors:\n4. Rejected/Undone Changes:\n5. Next Step:\n" +
				"Be factual and concise. Omit filler.",
		},
		{
			Role:    "user",
			Content: transcript.String(),
		},
	}

	msg, _, err := r.callAPIWithFailoverUsageID(ctx, "condenser", condenseMsgs, nil, nil, nil)
	if err != nil {
		return detSummary
	}
	candidate := strings.TrimSpace(msg.Content)
	if hasAllCondenserSlots(candidate) {
		return candidate
	}
	return detSummary
}

var mandatoryCondenserSlots = []string{
	"Goal",
	"Modified Files & Line State",
	"Active Errors",
	"Rejected/Undone Changes",
	"Next Step",
}

func hasAllCondenserSlots(s string) bool {
	low := strings.ToLower(s)
	for _, slot := range mandatoryCondenserSlots {
		if !strings.Contains(low, strings.ToLower(slot)) {
			return false
		}
	}
	return true
}

func buildDeterministicCondenseSummary(span []chatMessage) string {
	goal := "(none recorded)"
	var modifiedFiles []string
	seenFiles := make(map[string]bool)
	var activeErrors []string
	var rejectedOrUndone []string
	nextStep := "Continue active implementation and verification."

	for _, m := range span {
		trimmed := strings.TrimSpace(m.Content)
		if m.Role == "user" && goal == "(none recorded)" && trimmed != "" && !strings.HasPrefix(trimmed, "[Episodic Condensation Summary]") {
			firstLine := strings.Split(trimmed, "\n")[0]
			goal = clipTextRunes(firstLine, 180)
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "write_file" || tc.Function.Name == "edit_file" {
				var p struct {
					Path string `json:"path"`
				}
				if json.Unmarshal([]byte(tc.Function.Arguments), &p) == nil && p.Path != "" && !seenFiles[p.Path] {
					seenFiles[p.Path] = true
					modifiedFiles = append(modifiedFiles, p.Path)
				}
			}
		}
		low := strings.ToLower(trimmed)
		if m.Role == "tool" && (strings.Contains(low, "error") || strings.Contains(low, "failed") || strings.Contains(low, "denied by tether guard")) {
			activeErrors = append(activeErrors, clipTextRunes(strings.Split(trimmed, "\n")[0], 140))
		}
		if strings.Contains(low, "rejected by the user") || strings.Contains(low, "undo") || strings.Contains(low, "reverted") {
			rejectedOrUndone = append(rejectedOrUndone, clipTextRunes(strings.Split(trimmed, "\n")[0], 140))
		}
		if m.Role == "assistant" && trimmed != "" {
			nextStep = clipTextRunes(strings.Split(trimmed, "\n")[0], 160)
		}
	}

	sort.Strings(modifiedFiles)
	filesLine := "None"
	if len(modifiedFiles) > 0 {
		filesLine = strings.Join(modifiedFiles, ", ")
	}
	errLine := "None"
	if len(activeErrors) > 0 {
		if len(activeErrors) > 3 {
			activeErrors = activeErrors[len(activeErrors)-3:]
		}
		errLine = strings.Join(activeErrors, "; ")
	}
	rejLine := "None"
	if len(rejectedOrUndone) > 0 {
		if len(rejectedOrUndone) > 3 {
			rejectedOrUndone = rejectedOrUndone[len(rejectedOrUndone)-3:]
		}
		rejLine = strings.Join(rejectedOrUndone, "; ")
	}

	return fmt.Sprintf(
		"1. Goal: %s\n2. Modified Files & Line State: %s\n3. Active Errors: %s\n4. Rejected/Undone Changes: %s\n5. Next Step: %s",
		goal, filesLine, errLine, rejLine, nextStep,
	)
}

func (r *Runner) invalidateCondensedFileHashesLocked(remaining []chatMessage) {
	if len(r.activeFileHashes) == 0 {
		return
	}
	var joined strings.Builder
	for _, m := range remaining {
		joined.WriteString(m.Content)
		joined.WriteByte('\n')
	}
	text := joined.String()
	for rel := range r.activeFileHashes {
		marker := fmt.Sprintf("<file path=%q>", rel)
		if !strings.Contains(text, marker) {
			delete(r.activeFileHashes, rel)
		}
	}
}

func (r *Runner) hasConfiguredProvider() bool {
	r.mu.RLock()
	prov := r.cfg.Provider
	key := r.cfg.APIKey
	url := r.cfg.BaseURL
	r.mu.RUnlock()
	if strings.EqualFold(prov, "ollama") {
		return url != "" && !strings.HasPrefix(url, "http://localhost:11434")
	}
	return strings.TrimSpace(key) != "" && strings.TrimSpace(url) != ""
}
