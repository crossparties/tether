package mount

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Operation identifies whether a path check is for reading or mutating.
type Operation string

const (
	OpRead  Operation = "read"
	OpWrite Operation = "write"
)

// AccessMode represents the mounting access policy for a workspace path.
type AccessMode string

const (
	AccessRW      AccessMode = "rw"
	AccessGuarded AccessMode = "guarded"
	AccessRO      AccessMode = "ro"
	AccessIgnored AccessMode = "ignored"
)

// Decision is the structured outcome of Engine.AuthorizePath.
type Decision struct {
	Allowed        bool       `json:"allowed"`
	RelPath        string     `json:"rel_path"`
	CanonicalPath  string     `json:"canonical_path"`
	AccessMode     AccessMode `json:"access_mode"`
	Rule           *Rule      `json:"rule,omitempty"`
	MatchedPattern string     `json:"matched_pattern,omitempty"`
	Reason         string     `json:"reason"`
	IsBuiltin      bool       `json:"is_builtin,omitempty"`
	IsOutside      bool       `json:"is_outside,omitempty"`
	IsMissing      bool       `json:"is_missing,omitempty"`
	IsDir          bool       `json:"is_dir,omitempty"`
}

type fileStamp struct {
	path    string
	exists  bool
	modTime time.Time
	size    int64
}

// Engine enforces workspace containment, symlink resolution, built-in secrets,
// .tether/ignore rules, mounted_files overrides, and governance modes through
// a single gate (AuthorizePath).
type Engine struct {
	workspace string
	absRoot   string
	realRoot  string
	readDB    *sql.DB
	writeDB   *sql.DB

	mu          sync.RWMutex
	actorID     string
	now         func() time.Time
	leases      map[string]leaseEntry
	builtin     []Rule
	custom      []Rule
	parseErrs   []error
	loadedStamp fileStamp
}

// NewEngine constructs a read-safe Mount Engine rooted at workspace without
// creating directories or files on disk.
func NewEngine(workspace string) *Engine {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		ws = "."
	}
	absRoot, err := filepath.Abs(ws)
	if err != nil {
		absRoot = filepath.Clean(ws)
	}
	realRoot, err := EvalSymlinksWithAncestor(absRoot)
	if err != nil {
		realRoot = absRoot
	}
	e := &Engine{
		workspace: ws,
		absRoot:   absRoot,
		realRoot:  realRoot,
		builtin:   BuiltinRules(),
	}
	_ = e.Reload()
	return e
}

// Workspace returns the normalized absolute root path.
func (e *Engine) Workspace() string {
	return e.absRoot
}

// BindDB attaches optional SQLite read/write pools for mounted_files and
// mount_ignore_rules persistence without introducing package import cycles.
func (e *Engine) BindDB(readDB, writeDB *sql.DB) {
	e.mu.Lock()
	e.readDB = readDB
	e.writeDB = writeDB
	e.mu.Unlock()
	_ = e.Reload()
}

// Reload refreshes ignore rules from <workspace>/.tether/ignore (falling back to
// <workspace>/.tetherignore, then DB mount_ignore_rules if no ignore file exists).
// Valid rules and built-in rules remain active even if one rule has invalid re: syntax;
// any parse errors are retained in ParseErrors() and returned as a joined error.
func (e *Engine) Reload() error {
	rules, srcPath, parseErrs, err := LoadIgnoreFile(e.absRoot)
	if err != nil {
		return err
	}

	e.mu.RLock()
	readDB := e.readDB
	e.mu.RUnlock()

	if srcPath == "" && readDB != nil {
		if dbRules, derr := LoadRulesFromDB(readDB); derr == nil {
			for _, r := range dbRules {
				if !r.IsBuiltin {
					rules = append(rules, r)
				}
			}
		}
	}

	stamp := currentIgnoreStamp(e.absRoot)

	e.mu.Lock()
	e.custom = rules
	e.parseErrs = parseErrs
	e.loadedStamp = stamp
	e.mu.Unlock()
	if len(parseErrs) > 0 {
		return errors.Join(parseErrs...)
	}
	return nil
}

// ParseErrors returns any syntax/regex compile errors detected during the most recent reload.
func (e *Engine) ParseErrors() []error {
	e.reloadIfChanged()
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]error, len(e.parseErrs))
	copy(out, e.parseErrs)
	return out
}

func (e *Engine) reloadIfChanged() {
	cur := currentIgnoreStamp(e.absRoot)
	e.mu.RLock()
	prev := e.loadedStamp
	e.mu.RUnlock()
	if cur != prev {
		_ = e.Reload()
	}
}

func currentIgnoreStamp(workspace string) fileStamp {
	primary, fallback := IgnoreFilePaths(workspace)
	for _, p := range []string{primary, fallback} {
		if info, err := os.Stat(p); err == nil {
			return fileStamp{
				path:    p,
				exists:  true,
				modTime: info.ModTime(),
				size:    info.Size(),
			}
		}
	}
	return fileStamp{}
}

// Rules returns built-in rules followed by custom user rules.
func (e *Engine) Rules() []Rule {
	e.reloadIfChanged()
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Rule, 0, len(e.builtin)+len(e.custom))
	out = append(out, e.builtin...)
	out = append(out, e.custom...)
	return out
}

// CustomRules returns only the non-builtin ignore rules.
func (e *Engine) CustomRules() []Rule {
	e.reloadIfChanged()
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Rule, len(e.custom))
	copy(out, e.custom)
	return out
}

// SaveCustomRules writes custom ignore rules to .tether/ignore, syncs to
// mount_ignore_rules if bound, and reloads active rules.
func (e *Engine) SaveCustomRules(rules []Rule) error {
	e.mu.RLock()
	writeDB := e.writeDB
	e.mu.RUnlock()
	if err := SaveIgnoreRules(e.absRoot, writeDB, rules); err != nil {
		return err
	}
	return e.Reload()
}

// SyncRulesToDB synchronizes current custom and built-in rules into mount_ignore_rules.
func (e *Engine) SyncRulesToDB() error {
	e.mu.RLock()
	writeDB := e.writeDB
	custom := make([]Rule, len(e.custom))
	copy(custom, e.custom)
	e.mu.RUnlock()
	return syncRulesToDB(writeDB, custom)
}

// SetMountedMode records an explicit per-path access override in mounted_files.
func (e *Engine) SetMountedMode(relPath string, accessMode AccessMode, source string) error {
	e.mu.RLock()
	writeDB := e.writeDB
	e.mu.RUnlock()
	if writeDB == nil {
		return fmt.Errorf("mount engine has no database bound")
	}
	switch accessMode {
	case AccessRW, AccessGuarded, AccessRO, AccessIgnored:
	default:
		return fmt.Errorf("invalid access mode %q", accessMode)
	}
	if source == "" {
		source = "manual"
	}
	norm := strings.Trim(filepath.ToSlash(filepath.Clean(relPath)), "/")
	if norm == "" || norm == "." || strings.HasPrefix(norm, "..") {
		return fmt.Errorf("invalid workspace relative path %q", relPath)
	}
	_, err := writeDB.Exec(`
		INSERT INTO mounted_files (rel_path, access_mode, source, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(rel_path) DO UPDATE SET
			access_mode = excluded.access_mode,
			source = excluded.source,
			updated_at = CURRENT_TIMESTAMP
	`, norm, string(accessMode), source)
	return err
}

// GetMountedMode checks mounted_files for relPath or its nearest ancestor directory.
func (e *Engine) GetMountedMode(relPath string) (AccessMode, bool) {
	e.mu.RLock()
	readDB := e.readDB
	e.mu.RUnlock()
	if readDB == nil {
		return "", false
	}
	norm := strings.Trim(filepath.ToSlash(filepath.Clean(relPath)), "/")
	if norm == "" || norm == "." {
		return "", false
	}

	cur := norm
	for {
		var mode string
		err := readDB.QueryRow(`SELECT access_mode FROM mounted_files WHERE rel_path = ?`, cur).Scan(&mode)
		if err == nil {
			return AccessMode(mode), true
		}
		parent := filepath.ToSlash(filepath.Dir(cur))
		if parent == "." || parent == "/" || parent == cur {
			break
		}
		cur = parent
	}
	return "", false
}

// AuthorizePath is the single canonical gate for all path reads and writes.
// It checks lexical containment, symlink canonicalization (including dangling
// target ancestors), built-in secret rules, .tether/ignore rules, mounted_files
// overrides, governance mode ("code" | "plan" | "review"), and file ownership lease locks.
func (e *Engine) AuthorizePath(rawPath string, op Operation, mode string) Decision {
	return e.AuthorizePathForActor(rawPath, op, mode, e.ActorID())
}

// AuthorizePathForActor enforces AuthorizePath rules while attributing writes
// to actorID ("main" or a specific Crew ID) for File Ownership Lease Lock checks.
func (e *Engine) AuthorizePathForActor(rawPath string, op Operation, mode, actorID string) Decision {
	e.reloadIfChanged()

	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return Decision{
			Allowed:    false,
			AccessMode: AccessIgnored,
			Reason:     "access denied: empty path",
		}
	}

	relSlash, canonical, isOutside, rerr := ResolveWithinWorkspace(trimmed, e.absRoot)
	if rerr != nil {
		return Decision{
			Allowed:    false,
			AccessMode: AccessIgnored,
			IsOutside:  isOutside,
			Reason:     rerr.Error(),
		}
	}
	if relSlash == "." {
		return Decision{
			Allowed:       false,
			RelPath:       ".",
			CanonicalPath: canonical,
			AccessMode:    AccessIgnored,
			IsDir:         true,
			Reason:        "access denied: workspace root '.' cannot be targeted as a file",
		}
	}

	isDir := strings.HasSuffix(trimmed, "/") || strings.HasSuffix(trimmed, string(filepath.Separator))
	isMissing := false
	if info, statErr := os.Lstat(canonical); statErr == nil {
		isDir = info.IsDir()
	} else if os.IsNotExist(statErr) {
		isMissing = true
	}

	// Built-in secret/substrate protection (cannot be negated by user rules).
	// Explicit Planner exception applies ONLY to symlink-verified .tether/PLAN.md.
	if relSlash != ".tether/PLAN.md" {
		e.mu.RLock()
		builtins := e.builtin
		e.mu.RUnlock()
		for _, r := range builtins {
			if r.Matches(relSlash, isDir) {
				rc := r
				return Decision{
					Allowed:        false,
					RelPath:        relSlash,
					CanonicalPath:  canonical,
					AccessMode:     AccessIgnored,
					Rule:           &rc,
					MatchedPattern: r.Pattern,
					IsBuiltin:      true,
					IsMissing:      isMissing,
					IsDir:          isDir,
					Reason:         fmt.Sprintf("access denied: %q matches built-in protected rule %q", relSlash, r.Pattern),
				}
			}
		}
	}

	// User .tether/ignore rules (last match wins; supports !negation).
	e.mu.RLock()
	custom := e.custom
	e.mu.RUnlock()
	var matchedRule *Rule
	for i := range custom {
		r := custom[i]
		if r.Matches(relSlash, isDir) {
			if r.Negated {
				matchedRule = nil
			} else {
				rc := r
				matchedRule = &rc
			}
		}
	}
	if matchedRule != nil {
		return Decision{
			Allowed:        false,
			RelPath:        relSlash,
			CanonicalPath:  canonical,
			AccessMode:     AccessIgnored,
			Rule:           matchedRule,
			MatchedPattern: matchedRule.Pattern,
			IsMissing:      isMissing,
			IsDir:          isDir,
			Reason:         fmt.Sprintf("access denied: %q matches ignore rule %q", relSlash, matchedRule.Pattern),
		}
	}

	accessMode := AccessRW
	if mMode, ok := e.GetMountedMode(relSlash); ok {
		accessMode = mMode
	}
	// Policy/ignore files (.tetherignore, .gitignore) must never default to silent
	// auto-approved rw: keep them readable, and require guarded approval on writes.
	if IsPolicyConfigRel(relSlash) && accessMode == AccessRW {
		accessMode = AccessGuarded
	}

	switch accessMode {
	case AccessIgnored:
		return Decision{
			Allowed:       false,
			RelPath:       relSlash,
			CanonicalPath: canonical,
			AccessMode:    AccessIgnored,
			IsMissing:     isMissing,
			IsDir:         isDir,
			Reason:        fmt.Sprintf("access denied: %q is marked ignored in mounted_files", relSlash),
		}
	case AccessRO:
		if op == OpWrite {
			return Decision{
				Allowed:       false,
				RelPath:       relSlash,
				CanonicalPath: canonical,
				AccessMode:    AccessRO,
				IsMissing:     isMissing,
				IsDir:         isDir,
				Reason:        fmt.Sprintf("write denied: %q is mounted read-only (ro)", relSlash),
			}
		}
	}

	normMode := strings.ToLower(strings.TrimSpace(mode))
	if normMode == "" {
		normMode = "code"
	}
	if op == OpWrite {
		// .tether/PLAN.md is readable in all modes, but writable ONLY in PLAN mode.
		if relSlash == ".tether/PLAN.md" && normMode != "plan" {
			return Decision{
				Allowed:       false,
				RelPath:       relSlash,
				CanonicalPath: canonical,
				AccessMode:    AccessRO,
				IsBuiltin:     true,
				IsMissing:     isMissing,
				IsDir:         isDir,
				Reason:        fmt.Sprintf("write denied: %q is writable only in PLAN mode", relSlash),
			}
		}
		switch normMode {
		case "review":
			return Decision{
				Allowed:       false,
				RelPath:       relSlash,
				CanonicalPath: canonical,
				AccessMode:    accessMode,
				IsMissing:     isMissing,
				IsDir:         isDir,
				Reason:        fmt.Sprintf("write denied: %q blocked in REVIEW mode (read-only)", relSlash),
			}
		case "plan":
			if !IsPlanArtifactRel(relSlash) {
				return Decision{
					Allowed:       false,
					RelPath:       relSlash,
					CanonicalPath: canonical,
					AccessMode:    accessMode,
					IsMissing:     isMissing,
					IsDir:         isDir,
					Reason:        fmt.Sprintf("write denied: %q blocked in PLAN mode (allowed: .tether/PLAN.md, docs/, .agents/, *.plan.md)", relSlash),
				}
			}
		}
		if holder, exp, lerr := e.CheckLease(relSlash, actorID); lerr == nil && holder != "" {
			return Decision{
				Allowed:       false,
				RelPath:       relSlash,
				CanonicalPath: canonical,
				AccessMode:    accessMode,
				IsMissing:     isMissing,
				IsDir:         isDir,
				Reason:        fmt.Sprintf("write denied: %q is lease-locked by crew %q until %s", relSlash, holder, exp.Format(time.RFC3339)),
			}
		}
	}

	return Decision{
		Allowed:       true,
		RelPath:       relSlash,
		CanonicalPath: canonical,
		AccessMode:    accessMode,
		IsMissing:     isMissing,
		IsDir:         isDir,
		Reason:        "authorized",
	}
}

// ShouldSkipDir reports whether a workspace-relative directory must be skipped
// during directory walks (indexing, glob, grep, autocomplete).
func (e *Engine) ShouldSkipDir(relDir string) bool {
	e.reloadIfChanged()
	norm := strings.Trim(filepath.ToSlash(filepath.Clean(relDir)), "/")
	if norm == "" || norm == "." {
		return false
	}

	e.mu.RLock()
	builtins := e.builtin
	custom := e.custom
	e.mu.RUnlock()

	for _, r := range builtins {
		if r.Matches(norm, true) {
			return true
		}
	}

	// Evaluate custom rules in order; skip dir unless no later negation could match inside norm.
	ignored := false
	ignoredAt := -1
	for i, r := range custom {
		if !r.Enabled || r.Invalid != "" {
			continue
		}
		if r.Negated {
			if r.Matches(norm, true) {
				ignored = false
				ignoredAt = -1
			}
			continue
		}
		if r.Matches(norm, true) {
			ignored = true
			ignoredAt = i
		}
	}
	if !ignored {
		return false
	}
	for _, r := range custom[ignoredAt+1:] {
		if r.Enabled && r.Invalid == "" && r.Negated && negationMayMatchInsideDir(r, norm) {
			return false
		}
	}
	return true
}

func negationMayMatchInsideDir(r Rule, normDir string) bool {
	if r.IsRegex || r.baseOnly {
		return true
	}
	pat := strings.TrimSpace(strings.TrimPrefix(r.Pattern, "!"))
	pat = strings.TrimLeft(pat, "/")
	if idx := strings.IndexAny(pat, "*?[\\"); idx >= 0 {
		patPrefix := strings.TrimSuffix(pat[:idx], "/")
		if patPrefix == "" {
			return true
		}
		return strings.HasPrefix(patPrefix+"/", normDir+"/") || strings.HasPrefix(normDir+"/", patPrefix+"/")
	}
	return strings.HasPrefix(pat+"/", normDir+"/")
}

// IsDefaultSkipDirName reports standard dependency/build cache directory names
// skipped during non-ignored indexing, context discovery, and stack detection.
func IsDefaultSkipDirName(name string) bool {
	switch name {
	case "node_modules", "vendor", "__pycache__", "dist", "build", "target", ".bin", ".venv", "venv":
		return true
	default:
		return false
	}
}

// ShouldIndexRepoMap reports whether rawPath should be included in the AST RepoMap.
// It enforces full AuthorizePath(OpRead) containment/ignore checks and filters out
// Markdown/documentation files so docs stay in FTS5 cache.db instead of RepoMap.
func (e *Engine) ShouldIndexRepoMap(rawPath string) bool {
	d := e.AuthorizePath(rawPath, OpRead, "code")
	if !d.Allowed || d.IsDir {
		return false
	}
	if d.RelPath == ".tether/PLAN.md" {
		return false
	}
	switch strings.ToLower(filepath.Ext(d.RelPath)) {
	case ".md", ".mdx", ".markdown", ".rst":
		return false
	}
	return true
}

// IsPlanArtifactRel reports whether a normalized workspace-relative path is an
// authorized planning artifact (.tether/PLAN.md, docs/**, .agents/**, *.plan.md).
func IsPlanArtifactRel(relSlash string) bool {
	if relSlash == ".tether/PLAN.md" {
		return true
	}
	if IsDependencyManifest(relSlash) || IsPolicyConfigRel(relSlash) {
		return false
	}
	low := strings.ToLower(filepath.ToSlash(relSlash))
	return low == "docs" || strings.HasPrefix(low, "docs/") || strings.Contains(low, "/docs/") ||
		low == ".agents" || strings.HasPrefix(low, ".agents/") || strings.Contains(low, "/.agents/") ||
		strings.HasSuffix(low, ".plan.md")
}

// IsPolicyConfigRel reports whether a path targets a mount/git ignore policy file
// (.tetherignore or .gitignore) that must be guarded against silent autonomous edits.
func IsPolicyConfigRel(relPath string) bool {
	base := strings.ToLower(filepath.Base(filepath.ToSlash(strings.TrimSpace(relPath))))
	return base == ".tetherignore" || base == ".gitignore"
}

// IsDependencyManifest reports whether target names a critical dependency manifest or lockfile.
func IsDependencyManifest(target string) bool {
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

// ResolveWithinWorkspace checks lexical containment and resolves symlinks (including
// dangling target ancestors) to guarantee rawPath resolves inside workspaceRoot.
func ResolveWithinWorkspace(rawPath, workspaceRoot string) (relSlash, canonicalAbs string, isOutside bool, err error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", "", false, fmt.Errorf("access denied: empty path")
	}
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		root = "."
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", false, fmt.Errorf("access denied: resolve workspace root: %w", err)
	}
	realRoot, err := EvalSymlinksWithAncestor(absRoot)
	if err != nil {
		return "", "", false, fmt.Errorf("access denied: canonicalize workspace root: %w", err)
	}

	var full string
	if filepath.IsAbs(trimmed) {
		full = filepath.Clean(trimmed)
		if !isLexicallyWithin(absRoot, full) && !isLexicallyWithin(realRoot, full) {
			return "", "", true, fmt.Errorf("access denied: path %q escapes workspace root", rawPath)
		}
	} else {
		full = filepath.Join(absRoot, trimmed)
		if !isLexicallyWithin(absRoot, full) {
			return "", "", true, fmt.Errorf("access denied: path %q escapes workspace root", rawPath)
		}
	}

	realTarget, err := EvalSymlinksWithAncestor(full)
	if err != nil {
		return "", "", false, fmt.Errorf("access denied: resolve path %q: %w", rawPath, err)
	}
	if !isLexicallyWithin(realRoot, realTarget) {
		return "", "", true, fmt.Errorf("access denied: symlink %q escapes workspace root", rawPath)
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil {
		return "", "", true, fmt.Errorf("access denied: relative path %q: %w", rawPath, err)
	}
	return filepath.ToSlash(rel), realTarget, false, nil
}

func isLexicallyWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// EvalSymlinksWithAncestor canonicalizes existing symlinks AND dangling target
// ancestors (up to 16 symlink hops) so escapes through nonexistent children of
// symlinks (e.g. foo/evil-link/secret) are detected and fail closed on loops.
func EvalSymlinksWithAncestor(p string) (string, error) {
	cur := filepath.Clean(p)
	var suffix []string
	symlinkHops := 0

	for {
		info, err := os.Lstat(cur)
		if err == nil {
			resolved, rerr := filepath.EvalSymlinks(cur)
			if rerr == nil {
				for i := len(suffix) - 1; i >= 0; i-- {
					resolved = filepath.Join(resolved, suffix[i])
				}
				return filepath.Clean(resolved), nil
			}
			// If cur itself is a symlink pointing to a non-existent target, follow
			// Readlink manually so dangling symlink targets still get canonicalized.
			if info.Mode()&os.ModeSymlink != 0 {
				if symlinkHops >= 16 {
					return "", fmt.Errorf("too many symlink hops resolving %q", p)
				}
				linkTarget, lerr := os.Readlink(cur)
				if lerr != nil {
					return "", lerr
				}
				if !filepath.IsAbs(linkTarget) {
					realParent, perr := filepath.EvalSymlinks(filepath.Dir(cur))
					if perr != nil {
						return "", perr
					}
					linkTarget = filepath.Join(realParent, linkTarget)
				}
				for i := len(suffix) - 1; i >= 0; i-- {
					linkTarget = filepath.Join(linkTarget, suffix[i])
				}
				cur = filepath.Clean(linkTarget)
				suffix = nil
				symlinkHops++
				continue
			}
			return "", rerr
		}
		if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			for i := len(suffix) - 1; i >= 0; i-- {
				cur = filepath.Join(cur, suffix[i])
			}
			return filepath.Clean(cur), nil
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}
