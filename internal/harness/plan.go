package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/crossparties/tether/internal/mount"
)

// PlanOption represents a structured choice inside a Planner clarification question.
type PlanOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// ClarificationQuestion models an interactive option card during Plan/Architect mode.
type ClarificationQuestion struct {
	ID             string       `json:"id"`
	Question       string       `json:"question"`
	Options        []PlanOption `json:"options"`
	AllowFreeform  bool         `json:"allow_freeform,omitempty"`
	SelectedID     string       `json:"selected_id,omitempty"`
	FreeformAnswer string       `json:"freeform_answer,omitempty"`
}

// PlanReadinessResult describes the outcome of StartBuilding (and optional Critic Crew audit).
type PlanReadinessResult struct {
	Ready       bool   `json:"ready"`
	Mode        string `json:"mode"`
	CriticRan   bool   `json:"critic_ran,omitempty"`
	CriticNotes string `json:"critic_notes,omitempty"`
	PlanBytes   int    `json:"plan_bytes"`
}

// ReadPlan safely reads <workspace>/.tether/PLAN.md via Mount Engine authorization.
func ReadPlan(workspace string) (string, error) {
	return ReadPlanWithMount(nil, workspace)
}

// ReadPlanWithMount safely reads <workspace>/.tether/PLAN.md using the provided DB-bound
// Mount Engine (or an ephemeral workspace engine when m is nil).
func ReadPlanWithMount(m *mount.Engine, workspace string) (string, error) {
	if m == nil {
		m = mount.NewEngine(workspace)
	}
	dec := m.AuthorizePath(".tether/PLAN.md", mount.OpRead, "plan")
	if !dec.Allowed {
		return "", fmt.Errorf("%s", dec.Reason)
	}
	data, err := os.ReadFile(dec.CanonicalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// WritePlan safely writes <workspace>/.tether/PLAN.md via Mount Engine authorization.
func WritePlan(workspace, content string) error {
	return WritePlanWithMount(nil, workspace, content)
}

// WritePlanWithMount safely writes <workspace>/.tether/PLAN.md using the provided DB-bound
// Mount Engine (or an ephemeral workspace engine when m is nil).
func WritePlanWithMount(m *mount.Engine, workspace, content string) error {
	if m == nil {
		m = mount.NewEngine(workspace)
	}
	dec := m.AuthorizePath(".tether/PLAN.md", mount.OpWrite, "plan")
	if !dec.Allowed {
		return fmt.Errorf("%s", dec.Reason)
	}
	if err := os.MkdirAll(filepath.Dir(dec.CanonicalPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dec.CanonicalPath, []byte(content), 0o644)
}

// FormatClarificationAnswers formats structured user selections into compact Markdown
// for appending to .tether/PLAN.md or sending to the Planner turn.
func FormatClarificationAnswers(questions []ClarificationQuestion) string {
	if len(questions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Plan Clarification Decisions\n")
	for _, q := range questions {
		answer := strings.TrimSpace(q.FreeformAnswer)
		if answer == "" && strings.TrimSpace(q.SelectedID) != "" {
			for _, opt := range q.Options {
				if opt.ID == q.SelectedID {
					answer = opt.Label
					if opt.Description != "" {
						answer += " — " + opt.Description
					}
					break
				}
			}
			if answer == "" {
				answer = q.SelectedID
			}
		}
		if answer == "" {
			continue
		}
		fmt.Fprintf(&b, "- **%s**: %s\n", strings.TrimSpace(q.Question), answer)
	}
	return strings.TrimRight(b.String(), "\n")
}

// StartBuilding validates .tether/PLAN.md readiness, optionally runs a read-only
// Critic Crew audit, and transitions the engine to "code" mode if ready.
func (r *Runner) StartBuilding(ctx context.Context, runCritic bool) (PlanReadinessResult, error) {
	if r == nil || r.engine == nil {
		return PlanReadinessResult{}, fmt.Errorf("runner has no active workspace engine")
	}
	planText, err := ReadPlanWithMount(r.engine.Mount(), r.engine.Workspace)
	if err != nil {
		return PlanReadinessResult{}, err
	}
	trimmed := strings.TrimSpace(planText)
	if trimmed == "" {
		return PlanReadinessResult{
			Ready: false,
			Mode:  r.engine.GetMode(),
		}, fmt.Errorf(".tether/PLAN.md is empty; create a plan before starting build")
	}

	res := PlanReadinessResult{
		Ready:     true,
		Mode:      r.engine.GetMode(),
		PlanBytes: len(trimmed),
	}

	if runCritic {
		criticRes, cerr := r.CreateCrew(ctx, CrewCreateRequest{
			Name:            "plan-critic",
			PurposeCategory: "review",
			Mode:            "readonly",
			Task: "Review the following implementation plan (.tether/PLAN.md) against the repository. " +
				"Inspect relevant files read-only and reply starting with `APPROVED:` if ready to implement, " +
				"or `BLOCKED:` with concrete missing prerequisites.\n\n" + trimmed,
		})
		if cerr != nil {
			return res, fmt.Errorf("critic crew failed: %w", cerr)
		}
		defer func() {
			_ = r.CloseCrew(criticRes.CrewID, criticRes.Output)
		}()
		res.CriticRan = true
		res.CriticNotes = strings.TrimSpace(criticRes.Output)
		if strings.HasPrefix(strings.ToUpper(res.CriticNotes), "BLOCKED:") {
			res.Ready = false
			return res, nil
		}
	}

	if err := r.engine.SetMode("code"); err != nil {
		return res, err
	}
	res.Mode = "code"
	r.QueueTailNotice("Plan approved — transitioned from PLAN to CODE mode; proceed with implementation.")
	return res, nil
}
