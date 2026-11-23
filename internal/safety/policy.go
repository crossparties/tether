package safety

import (
	"path/filepath"
	"strings"

	"github.com/crossparties/tether/internal/mount"
)

// RiskLevel classifies whether an agent tool invocation can auto-run or requires explicit human approval.
type RiskLevel string

const (
	RiskLow  RiskLevel = "LOW"
	RiskHigh RiskLevel = "HIGH"
)

// ApprovalChoice represents the user's interactive decision on a high-risk proposal.
type ApprovalChoice int

const (
	ChoiceApproveAndRun  ApprovalChoice = iota // [Setujui & Jalankan]
	ChoiceViewFullDiff                         // [Lihat Diff Lengkap]
	ChoiceRejectAction                         // [Tolak Aksi Ini]
	ChoiceCancelAllTasks                       // [Batalkan Seluruh Task]
)

// ApprovalMenuLabels returns the 4 canonical Indonesian interactive menu labels for the approval gate.
func ApprovalMenuLabels() []string {
	return []string{
		"[Setujui & Jalankan]",
		"[Lihat Diff Lengkap]",
		"[Tolak Aksi Ini]",
		"[Batalkan Seluruh Task]",
	}
}

// Proposal encapsulates a tool or file-mutation action evaluated by the safety policy gate.
type Proposal struct {
	AgentName        string
	ToolName         string
	Target           string
	Risk             RiskLevel
	Reason           string
	DiffPreview      string
	FullDiff         string
	RequiresApproval bool
}

// ClassifyRisk deterministically classifies a tool invocation and target into RiskLow or RiskHigh
// per PRD §3.6:
//   - Low Risk (Auto-Approve): reading files, inspecting directory structures, grep logs,
//     writing to temporary cache files (.tmp) inside workspace.
//   - High Risk (Mandatory Escalation): modifying source code files, deleting files,
//     changing dependency files (go.mod, package.json), and shell command executions.
func ClassifyRisk(toolName, target string) (RiskLevel, string) {
	normTool := strings.ToLower(strings.TrimSpace(toolName))
	normTarget := strings.TrimSpace(target)

	// Always protect sensitive credential/substrate files regardless of tool.
	if isSensitiveTarget(normTarget) {
		return RiskHigh, "Menyentuh file sensitif atau database internal (" + normTarget + ")"
	}

	switch normTool {
	case "read", "/read", "read_file", "view_file", "list", "list_dir", "list_files",
		"glob", "grep", "search", "ripgrep", "kb_search", "todo_manage":
		return RiskLow, "Operasi baca/inspeksi aman (read-only)"

	case "write", "write_file", "edit", "edit_file", "patch", "apply_diff",
		"create_file", "replace", "multi_edit":
		if mount.IsPolicyConfigRel(normTarget) {
			return RiskHigh, "Perubahan kebijakan mount/ignore (" + normTarget + ") memerlukan persetujuan"
		}
		// PRD §3.6: Writing to temporary .tmp cache files inside workspace is Low Risk (Auto-Approve)
		if isSafeTempFile(normTarget) {
			return RiskLow, "Penulisan file cache temporer (.tmp) disetujui otomatis oleh Main Agent"
		}
		if isDependencyManifest(normTarget) {
			return RiskHigh, "Perubahan file dependensi kritis (" + normTarget + ")"
		}
		return RiskHigh, "Modifikasi file di kode sumber (" + normTarget + ")"

	case "delete", "delete_file", "remove", "remove_file", "rm", "unlink":
		return RiskHigh, "Penghapusan file permanen (" + normTarget + ")"

	case "bash", "sh", "shell", "exec", "/exec", "run_command", "command", "terminal":
		return RiskHigh, "Eksekusi perintah shell memerlukan persetujuan (" + normTarget + ")"

	default:
		if strings.HasPrefix(normTool, "mcp_") {
			return RiskHigh, "Eksekusi external MCP tool '" + toolName + "' memerlukan persetujuan"
		}
		if normTool == "" {
			return RiskHigh, "Aksi tidak dikenal memerlukan persetujuan manual"
		}
		return RiskHigh, "Eksekusi tool '" + toolName + "' memerlukan persetujuan"
	}
}

func isSafeTempFile(target string) bool {
	if target == "" {
		return false
	}
	slash := filepath.ToSlash(strings.TrimSpace(target))
	if strings.Contains(slash, "..") || strings.HasPrefix(slash, "/") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(slash), ".tmp")
}

func isDependencyManifest(target string) bool {
	return mount.IsDependencyManifest(target)
}

// isSensitiveTarget reports whether target references credentials, keys, VCS internals,
// or Tether's SQLite state.
func isSensitiveTarget(target string) bool {
	if target == "" {
		return false
	}
	if _, ok := mount.MatchesBuiltinSecret(target); ok {
		return true
	}
	lower := strings.ToLower(filepath.ToSlash(strings.TrimSpace(target)))
	if strings.Contains(lower, "session.db") || strings.Contains(lower, "cache.db") ||
		lower == ".git" || strings.HasPrefix(lower, ".git/") || strings.Contains(lower, "/.git/") || strings.HasSuffix(lower, "/.git") ||
		lower == ".ssh" || strings.HasPrefix(lower, ".ssh/") || strings.Contains(lower, "/.ssh/") || strings.HasSuffix(lower, "/.ssh") ||
		lower == ".tether" || strings.HasPrefix(lower, ".tether/") || strings.Contains(lower, "/.tether/") || strings.HasSuffix(lower, "/.tether") {
		return true
	}
	return false
}

// EvaluateProposal builds a complete Proposal with automatic risk classification and diff previews.
func EvaluateProposal(agentName, toolName, target, diffPreview, fullDiff string) Proposal {
	risk, reason := ClassifyRisk(toolName, target)
	if strings.TrimSpace(agentName) == "" {
		agentName = "Coder"
	}
	if diffPreview == "" && fullDiff != "" {
		diffPreview = summarizeDiffPreview(fullDiff, 12)
	}
	return Proposal{
		AgentName:        agentName,
		ToolName:         toolName,
		Target:           target,
		Risk:             risk,
		Reason:           reason,
		DiffPreview:      diffPreview,
		FullDiff:         fullDiff,
		RequiresApproval: risk == RiskHigh,
	}
}

// summarizeDiffPreview extracts up to maxLines from fullDiff for compact box rendering.
func summarizeDiffPreview(fullDiff string, maxLines int) string {
	if maxLines <= 0 {
		maxLines = 10
	}
	lines := strings.Split(strings.TrimRight(fullDiff, "\n"), "\n")
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n")
	}
	kept := append([]string{}, lines[:maxLines]...)
	kept = append(kept, "... (gunakan [Lihat Diff Lengkap] untuk melihat seluruh perubahan)")
	return strings.Join(kept, "\n")
}

// IsReadOnlyCommand reports whether a shell command is a pure read-only
// inspection command (including piped/chained inspection commands, find without
// -exec/-delete, sed -n, read-only git, etc.) without mutative redirection or
// side effects.
func IsReadOnlyCommand(cmdStr string) (bool, string) {
	allSafe, hasReadOnly, hasValidation, label := analyzeShellCommand(cmdStr)
	if !allSafe || !hasReadOnly || hasValidation {
		return false, ""
	}
	return true, "Inspeksi read-only (" + label + ") disetujui otomatis oleh Main Agent (Tier-1 Low Risk)"
}

// IsValidationCommand reports whether a shell command only builds, tests, vets, or
// format-checks the project (e.g., go test/build/vet, gofmt -l, npm test,
// npx tsc --noEmit, cargo test/check/clippy, pytest, piped through head/tail/grep
// or with cd <subdir> && ...) without mutating files.
func IsValidationCommand(cmdStr string) (bool, string) {
	allSafe, _, hasValidation, label := analyzeShellCommand(cmdStr)
	if !allSafe || !hasValidation {
		return false, ""
	}
	return true, "Validation (" + label + ") auto-approved by Main Agent (Tier-1 Low Risk)"
}

// IsPlanArtifactTarget reports whether target is a safe documentation or plan
// artifact path inside workspaceRoot (.tether/PLAN.md, docs/, .agents/, or *.plan.md).
func IsPlanArtifactTarget(target, workspaceRoot string) bool {
	d := mount.NewEngine(workspaceRoot).AuthorizePath(target, mount.OpWrite, "plan")
	if !d.Allowed || d.IsDir || d.AccessMode == mount.AccessGuarded {
		return false
	}
	return !isDependencyManifest(d.RelPath)
}

// IsRoutineWorkspaceEdit reports whether toolName + target represents a routine,
// recoverable workspace file edit (not outside workspace, not symlink-escaped,
// not sensitive, not ignored/ro/guarded, and not a dependency manifest/lockfile).
func IsRoutineWorkspaceEdit(toolName, target, workspaceRoot string) (bool, string) {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "write", "write_file", "write_to_file", "edit", "edit_file",
		"replace_file_content", "multi_replace_file_content",
		"create_file", "replace", "multi_edit", "patch", "apply_diff":
	default:
		return false, ""
	}
	if strings.TrimSpace(target) == "" || isDependencyManifest(target) {
		return false, ""
	}
	d := mount.NewEngine(workspaceRoot).AuthorizePath(target, mount.OpWrite, "code")
	if !d.Allowed || d.IsDir || d.AccessMode == mount.AccessGuarded || isDependencyManifest(d.RelPath) {
		return false, ""
	}
	return true, "Routine workspace edit (" + d.RelPath + ") auto-approved (undoable via /undo)"
}

// IsNonCriticalTool reports whether a tool invocation is non-critical in the active
// governance mode ("code", "plan", or "review") and safe to execute in one step
// without interrupting the user for interactive approval.
func IsNonCriticalTool(mode, toolName, target, command, workspaceRoot string) (bool, string) {
	normMode := strings.ToLower(strings.TrimSpace(mode))
	if normMode == "" {
		normMode = "code"
	}
	normTool := strings.ToLower(strings.TrimSpace(toolName))

	switch normTool {
	case "bash", "sh", "shell", "exec", "/exec", "run_command", "command", "terminal":
		cmd := strings.TrimSpace(command)
		if cmd == "" {
			cmd = strings.TrimSpace(target)
		}
		if ok, _ := CheckShellCommand(cmd, workspaceRoot); !ok {
			return false, ""
		}
		if ok, reason := IsReadOnlyCommand(cmd); ok {
			return true, reason
		}
		if ok, reason := IsValidationCommand(cmd); ok {
			return true, reason
		}
		return false, ""

	case "read", "/read", "read_file", "view_file", "list", "list_dir", "list_files":
		trimmedTarget := strings.TrimSpace(target)
		if (normTool == "list" || normTool == "list_dir" || normTool == "list_files") &&
			(trimmedTarget == "" || trimmedTarget == ".") {
			return true, "Operasi baca/inspeksi aman (read-only)"
		}
		d := mount.NewEngine(workspaceRoot).AuthorizePath(trimmedTarget, mount.OpRead, normMode)
		if !d.Allowed {
			return false, ""
		}
		return true, "Operasi baca/inspeksi aman (read-only)"

	case "glob", "grep", "search", "ripgrep":
		if isSensitiveTarget(target) {
			return false, ""
		}
		trimmedTarget := strings.TrimSpace(target)
		if strings.Contains(trimmedTarget, "..") || filepath.IsAbs(trimmedTarget) || strings.HasPrefix(trimmedTarget, "./") {
			if d := mount.NewEngine(workspaceRoot).AuthorizePath(trimmedTarget, mount.OpRead, normMode); !d.Allowed {
				return false, ""
			}
		}
		return true, "Operasi baca/inspeksi aman (read-only)"

	case "kb_search", "todo_manage":
		if isSensitiveTarget(target) {
			return false, ""
		}
		return true, "Operasi baca/inspeksi aman (read-only)"

	case "write", "write_file", "write_to_file", "edit", "edit_file",
		"replace_file_content", "multi_replace_file_content",
		"create_file", "replace", "multi_edit", "patch", "apply_diff":
		switch normMode {
		case "review":
			return false, ""
		case "plan":
			if !IsPlanArtifactTarget(target, workspaceRoot) {
				return false, ""
			}
			return true, "Plan artifact write (" + target + ") auto-approved in PLAN mode"
		default: // "code"
			return IsRoutineWorkspaceEdit(normTool, target, workspaceRoot)
		}

	default:
		return false, ""
	}
}
