package safety

import (
	"os"
	"path/filepath"
	"strings"
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
		"glob", "grep", "search", "ripgrep", "session_load", "kb_search",
		"kb_status", "todo_list", "status", "mode_get", "proposal_status":
		return RiskLow, "Operasi baca/inspeksi aman (read-only)"

	case "write", "write_file", "edit", "edit_file", "patch", "apply_diff",
		"create_file", "replace", "multi_edit":
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
	base := strings.ToLower(filepath.Base(filepath.ToSlash(target)))
	switch base {
	case "go.mod", "go.sum",
		"package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock",
		"cargo.toml", "cargo.lock",
		"requirements.txt", "pyproject.toml", "poetry.lock":
		return true
	default:
		return false
	}
}

// isSensitiveTarget reports whether target references credentials, keys, VCS internals,
// or Tether's SQLite state.
func isSensitiveTarget(target string) bool {
	if target == "" {
		return false
	}
	lower := strings.ToLower(filepath.ToSlash(strings.TrimSpace(target)))
	base := filepath.Base(lower)

	if base == ".env" || strings.HasPrefix(base, ".env.") ||
		base == "session.db" || strings.HasPrefix(base, "session.db-") ||
		base == "id_rsa" || base == "id_ed25519" || base == "id_dsa" || base == "id_ecdsa" ||
		base == "credentials.json" || base == ".npmrc" || base == ".netrc" || base == ".pypirc" ||
		strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") ||
		strings.HasSuffix(base, ".p12") || strings.HasSuffix(base, ".pfx") {
		return true
	}
	if strings.Contains(lower, "session.db") ||
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
// artifact path inside workspaceRoot (docs/, .agents/, or *.plan.md).
func IsPlanArtifactTarget(target, workspaceRoot string) bool {
	rel, inside := resolveWithinWorkspace(target, workspaceRoot)
	if !inside || isSensitiveTarget(rel) || isDependencyManifest(rel) {
		return false
	}
	low := strings.ToLower(rel)
	return low == "docs" || strings.HasPrefix(low, "docs/") || strings.Contains(low, "/docs/") ||
		low == ".agents" || strings.HasPrefix(low, ".agents/") || strings.Contains(low, "/.agents/") ||
		strings.HasSuffix(low, ".plan.md")
}

// IsRoutineWorkspaceEdit reports whether toolName + target represents a routine,
// recoverable workspace file edit (not outside workspace, not symlink-escaped,
// not sensitive, and not a dependency manifest/lockfile).
func IsRoutineWorkspaceEdit(toolName, target, workspaceRoot string) (bool, string) {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "write", "write_file", "write_to_file", "edit", "edit_file",
		"replace_file_content", "multi_replace_file_content",
		"create_file", "replace", "multi_edit", "patch", "apply_diff":
	default:
		return false, ""
	}
	if strings.TrimSpace(target) == "" || isSensitiveTarget(target) || isDependencyManifest(target) {
		return false, ""
	}
	rel, inside := resolveWithinWorkspace(target, workspaceRoot)
	if !inside || isSensitiveTarget(rel) || isDependencyManifest(rel) {
		return false, ""
	}
	return true, "Routine workspace edit (" + rel + ") auto-approved (undoable via /undo)"
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
		if ok, reason := IsReadOnlyCommand(cmd); ok {
			return true, reason
		}
		if ok, reason := IsValidationCommand(cmd); ok {
			return true, reason
		}
		return false, ""

	case "read", "/read", "read_file", "view_file", "list", "list_dir", "list_files",
		"glob", "grep", "search", "ripgrep", "session_load", "kb_search",
		"kb_status", "todo_list", "status", "mode_get", "proposal_status":
		if isSensitiveTarget(target) {
			return false, ""
		}
		return true, "Operasi baca/inspeksi aman (read-only)"

	case "write", "write_file", "write_to_file", "edit", "edit_file",
		"replace_file_content", "multi_replace_file_content",
		"create_file", "replace", "multi_edit", "patch", "apply_diff":
		ok, reason := IsRoutineWorkspaceEdit(normTool, target, workspaceRoot)
		if !ok {
			return false, ""
		}
		switch normMode {
		case "review":
			return false, ""
		case "plan":
			if !IsPlanArtifactTarget(target, workspaceRoot) {
				return false, ""
			}
			return true, "Plan artifact write (" + target + ") auto-approved in PLAN mode"
		default: // "code"
			return true, reason
		}

	default:
		return false, ""
	}
}

func resolveWithinWorkspace(target, workspaceRoot string) (string, bool) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "", false
	}
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		root = "."
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	realRoot, err := evalSymlinksWithAncestor(absRoot)
	if err != nil {
		realRoot = absRoot
	}

	var full string
	if filepath.IsAbs(trimmed) {
		full = filepath.Clean(trimmed)
	} else {
		full = filepath.Join(absRoot, trimmed)
	}
	// Reject lexical traversal (e.g. "../outside.go") even if target does not exist yet.
	lexRel, err := filepath.Rel(absRoot, full)
	if err != nil || lexRel == ".." || strings.HasPrefix(lexRel, ".."+string(filepath.Separator)) {
		return "", false
	}

	// Resolve symlinks to block symlink escapes out of workspaceRoot.
	realTarget, err := evalSymlinksWithAncestor(full)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	if rel == "." {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func evalSymlinksWithAncestor(p string) (string, error) {
	clean := filepath.Clean(p)
	var suffix []string
	cur := clean
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, rerr := filepath.EvalSymlinks(cur)
			if rerr != nil {
				return "", rerr
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return clean, nil
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}
