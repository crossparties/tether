package mount

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Rule represents one parsed ignore or built-in secret protection rule.
type Rule struct {
	ID        int64  `json:"id,omitempty"`
	Pattern   string `json:"pattern"`
	IsRegex   bool   `json:"is_regex"`
	IsBuiltin bool   `json:"is_builtin"`
	Enabled   bool   `json:"enabled"`
	Negated   bool   `json:"negated"`
	DirOnly   bool   `json:"dir_only"`
	Invalid   string `json:"invalid,omitempty"`

	baseOnly bool
	re       *regexp.Regexp
}

// ImpactWarning flags an ignore rule that matches critical source files.
type ImpactWarning struct {
	Pattern string `json:"pattern"`
	Ext     string `json:"ext"`
	Count   int    `json:"count"`
	Message string `json:"message"`
}

// SimulationResult summarizes how ignore rules affect workspace files.
type SimulationResult struct {
	TotalFiles      int             `json:"total_files"`
	MatchedCount    int             `json:"matched_count"`
	MatchedSamples  []string        `json:"matched_samples,omitempty"`
	ByExtension     map[string]int  `json:"by_extension,omitempty"`
	Warnings        []ImpactWarning `json:"warnings,omitempty"`
	InvalidPatterns []string        `json:"invalid_patterns,omitempty"`
}

var builtinSecretPatterns = []string{
	".env",
	".env.*",
	"id_rsa*",
	"id_ed25519*",
	"id_ecdsa*",
	"id_dsa*",
	"*.pem",
	"*.key",
	"*.p12",
	"*.pfx",
	"*.kdbx",
	".npmrc",
	".pypirc",
	".netrc",
	"credentials*.json",
	"kubeconfig",
	".git",
	".ssh",
	".tether",
	"session.db",
	"session.db-*",
	"cache.db",
	"cache.db-*",
}

// BuiltinSecretPatterns returns the immutable list of built-in protected patterns.
func BuiltinSecretPatterns() []string {
	out := make([]string, len(builtinSecretPatterns))
	copy(out, builtinSecretPatterns)
	return out
}

// BuiltinRules returns compiled, non-removable built-in secret/substrate rules.
func BuiltinRules() []Rule {
	out := make([]Rule, 0, len(builtinSecretPatterns))
	for _, pat := range builtinSecretPatterns {
		r := compileRule(pat, true, true)
		out = append(out, r)
	}
	return out
}

// MatchesBuiltinSecret reports whether rawPath (lexically cleaned) matches any
// built-in secret or internal substrate pattern. The explicit Planner file
// ".tether/PLAN.md" is exempt here (still subject to symlink containment in Engine).
func MatchesBuiltinSecret(rawPath string) (Rule, bool) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return Rule{}, false
	}
	slash := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(trimmed)), "./")
	if slash == ".tether/PLAN.md" {
		return Rule{}, false
	}
	isDirHint := strings.HasSuffix(strings.TrimSpace(rawPath), "/")
	for _, r := range BuiltinRules() {
		if r.Matches(slash, isDirHint) {
			return r, true
		}
	}
	return Rule{}, false
}

// GlobMayMatchSensitive reports whether an unquoted shell glob pattern overlaps
// with built-in secret targets or matches any ignored path under workspace rules.
func GlobMayMatchSensitive(globToken string, userRules []Rule) bool {
	probeTargets := []string{
		".env", ".env1", ".env.local", ".env.production",
		"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
		"key.pem", "server.key", "cert.p12", "cert.pfx", "vault.kdbx",
		".npmrc", ".pypirc", ".netrc", "credentials.json", "credentials-prod.json",
		"kubeconfig", ".git", ".git/config", ".ssh", ".ssh/id_rsa",
		".tether", ".tether/session.db", ".tether/cache.db",
		"session.db", "session.db-wal", "cache.db", "cache.db-wal",
	}
	cleanGlob := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(globToken)), "./")
	baseGlob := filepath.Base(cleanGlob)
	for _, p := range probeTargets {
		if ok, _ := filepath.Match(cleanGlob, p); ok {
			return true
		}
		if !strings.Contains(cleanGlob, "/") {
			if ok, _ := filepath.Match(baseGlob, filepath.Base(p)); ok {
				return true
			}
		}
	}
	for _, r := range userRules {
		if !r.Enabled || r.Negated || r.Invalid != "" {
			continue
		}
		if r.Matches(cleanGlob, false) {
			return true
		}
	}
	return false
}

// ParseRule parses a single ignore rule line (.gitignore glob or re: regex).
func ParseRule(rawLine string) (Rule, bool) {
	line := trimUnescapedTrailingSpace(strings.TrimRight(rawLine, "\r"))
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return Rule{}, false
	}
	return compileRule(line, false, true), true
}

// ParseIgnoreContent parses .tether/ignore contents into compiled Rules and any syntax errors.
func ParseIgnoreContent(content string) ([]Rule, []error) {
	var rules []Rule
	var errs []error
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	for i, raw := range lines {
		r, ok := ParseRule(raw)
		if !ok {
			continue
		}
		if r.Invalid != "" {
			errs = append(errs, fmt.Errorf("line %d (%s): %s", i+1, r.Pattern, r.Invalid))
		}
		rules = append(rules, r)
	}
	return rules, errs
}

func compileRule(rawPattern string, isBuiltin, enabled bool) Rule {
	pat := trimUnescapedTrailingSpace(strings.TrimSpace(rawPattern))
	rule := Rule{
		Pattern:   pat,
		IsBuiltin: isBuiltin,
		Enabled:   enabled,
	}
	if pat == "" {
		rule.Invalid = "empty pattern"
		return rule
	}

	work := pat
	if !isBuiltin && strings.HasPrefix(work, "!") {
		rule.Negated = true
		work = strings.TrimPrefix(work, "!")
	}
	if strings.HasPrefix(work, `\#`) || strings.HasPrefix(work, `\!`) {
		work = work[1:]
	}

	if strings.HasPrefix(work, "re:") {
		rule.IsRegex = true
		expr := strings.TrimPrefix(work, "re:")
		if expr == "" {
			rule.Invalid = "empty regex after re:"
			return rule
		}
		if isBuiltin && !strings.HasPrefix(expr, "(?i)") {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			rule.Invalid = fmt.Sprintf("invalid regex: %v", err)
			return rule
		}
		rule.re = re
		return rule
	}

	re, dirOnly, baseOnly, err := compileGitignoreGlob(work, isBuiltin)
	if err != nil {
		rule.Invalid = err.Error()
		return rule
	}
	rule.DirOnly = dirOnly
	rule.baseOnly = baseOnly
	rule.re = re
	return rule
}

func trimUnescapedTrailingSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		if len(s) >= 2 && s[len(s)-2] == '\\' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

func compileGitignoreGlob(glob string, caseInsensitive bool) (re *regexp.Regexp, dirOnly, baseOnly bool, err error) {
	if strings.HasSuffix(glob, "/") {
		dirOnly = true
		glob = strings.TrimRight(glob, "/")
	}
	if glob == "" {
		return nil, dirOnly, false, fmt.Errorf("empty glob pattern")
	}
	anchored := strings.HasPrefix(glob, "/")
	glob = strings.TrimLeft(glob, "/")
	if glob == "" {
		return nil, dirOnly, false, fmt.Errorf("empty glob pattern")
	}
	if strings.Contains(glob, "/") {
		anchored = true
	}
	baseOnly = !anchored

	body := globBodyToRegex(glob)
	prefix := "^"
	if caseInsensitive {
		prefix = "(?i)^"
	}
	compiled, cerr := regexp.Compile(prefix + body + "$")
	if cerr != nil {
		return nil, dirOnly, baseOnly, fmt.Errorf("invalid glob %q: %w", glob, cerr)
	}
	return compiled, dirOnly, baseOnly, nil
}

// CompileWorkspaceGlob compiles a glob pattern for workspace file discovery (glob/grep tools),
// matching full workspace-relative slash paths where "*" crosses single path segments and "**"
// matches across directories.
func CompileWorkspaceGlob(pattern string) (*regexp.Regexp, error) {
	p := strings.TrimSpace(filepath.ToSlash(pattern))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil, fmt.Errorf("empty glob pattern")
	}
	body := globBodyToRegex(p)
	return regexp.Compile("^" + body + "$")
}

func globBodyToRegex(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); {
		if strings.HasPrefix(glob[i:], "**/") {
			b.WriteString("(?:.*/)?")
			i += 3
			continue
		}
		if strings.HasPrefix(glob[i:], "**") {
			b.WriteString(".*")
			i += 2
			continue
		}
		c := glob[i]
		switch c {
		case '*':
			b.WriteString("[^/]*")
			i++
		case '?':
			b.WriteString("[^/]")
			i++
		case '\\':
			if i+1 < len(glob) {
				b.WriteString(regexp.QuoteMeta(string(glob[i+1])))
				i += 2
			} else {
				b.WriteString(`\\`)
				i++
			}
		case '[':
			end := strings.IndexByte(glob[i+1:], ']')
			if end > 0 {
				cls := glob[i+1 : i+1+end]
				if strings.HasPrefix(cls, "!") {
					cls = "^" + cls[1:]
				}
				b.WriteByte('[')
				b.WriteString(cls)
				b.WriteByte(']')
				i += end + 2
			} else {
				b.WriteString(`\[`)
				i++
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	return b.String()
}

// Matches reports whether relSlash matches this rule. For non-regex rules, it evaluates
// ancestor directory prefixes as well so directory rules (e.g. ".git/" or "dist/") match
// everything inside them.
func (r Rule) Matches(relSlash string, isDir bool) bool {
	if !r.Enabled || r.Invalid != "" || r.re == nil {
		return false
	}
	norm := strings.Trim(filepath.ToSlash(relSlash), "/")
	if norm == "" || norm == "." {
		return false
	}
	if r.IsRegex {
		return r.re.MatchString(norm)
	}

	parts := strings.Split(norm, "/")
	// Check ancestor directories (always treated as directories).
	for i := 0; i < len(parts)-1; i++ {
		prefix := strings.Join(parts[:i+1], "/")
		if r.baseOnly {
			if r.re.MatchString(parts[i]) {
				return true
			}
		} else if r.re.MatchString(prefix) {
			return true
		}
	}

	// Check the leaf path itself.
	if r.DirOnly && !isDir {
		return false
	}
	if r.baseOnly {
		return r.re.MatchString(parts[len(parts)-1])
	}
	return r.re.MatchString(norm)
}

// IgnoreFilePaths returns the primary (.tether/ignore) and fallback (.tetherignore) paths.
func IgnoreFilePaths(workspace string) (primary, fallback string) {
	return filepath.Join(workspace, ".tether", "ignore"), filepath.Join(workspace, ".tetherignore")
}

// LoadIgnoreFile reads <workspace>/.tether/ignore, falling back to <workspace>/.tetherignore
// if .tether/ignore does not exist. It is strictly read-only.
func LoadIgnoreFile(workspace string) (rules []Rule, sourcePath string, parseErrs []error, err error) {
	primary, fallback := IgnoreFilePaths(workspace)
	data, rerr := os.ReadFile(primary)
	if rerr == nil {
		rules, parseErrs = ParseIgnoreContent(string(data))
		return rules, primary, parseErrs, nil
	}
	if !os.IsNotExist(rerr) {
		return nil, primary, nil, rerr
	}

	data, ferr := os.ReadFile(fallback)
	if ferr == nil {
		rules, parseErrs = ParseIgnoreContent(string(data))
		return rules, fallback, parseErrs, nil
	}
	if os.IsNotExist(ferr) {
		return nil, "", nil, nil
	}
	return nil, fallback, nil, ferr
}

// InspectIgnoreFile returns read-only diagnostic status for `tether doctor`.
func InspectIgnoreFile(workspace string) (sourcePath string, exists bool, rules []Rule, errs []error) {
	rules, sourcePath, errs, err := LoadIgnoreFile(workspace)
	if err != nil {
		return sourcePath, false, nil, []error{err}
	}
	return sourcePath, sourcePath != "", rules, errs
}

var sourceCodeExts = []string{".go", ".ts", ".tsx", ".js", ".jsx", ".svelte", ".py", ".rs", ".java"}

// ConvertGitignoreContent extracts safe noise-directory / artifact rules from a .gitignore
// file into .tether/ignore content, excluding Tether runtime entries, rules already covered
// by built-in secret rules, and rules that would block normal source code files.
func ConvertGitignoreContent(gitignoreText string) (converted string, skipped []string) {
	var kept []string
	seen := make(map[string]bool)

	for _, raw := range strings.Split(strings.ReplaceAll(gitignoreText, "\r\n", "\n"), "\n") {
		line := trimUnescapedTrailingSpace(strings.TrimRight(raw, "\r"))
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, ".tether") || strings.HasPrefix(trimmed, "/.tether") {
			continue
		}
		if r, ok := MatchesBuiltinSecret(strings.TrimPrefix(trimmed, "/")); ok && r.Pattern != "" {
			continue
		}
		rule, ok := ParseRule(trimmed)
		if !ok || rule.Invalid != "" {
			if trimmed != "" {
				skipped = append(skipped, trimmed)
			}
			continue
		}
		if ruleMatchesSourceCode(rule) {
			skipped = append(skipped, trimmed)
			continue
		}
		if !seen[trimmed] {
			seen[trimmed] = true
			kept = append(kept, trimmed)
		}
	}

	header := "# Tether mount ignore rules (.gitignore syntax; prefix with re: for Go regex)\n"
	if len(kept) == 0 {
		return header, skipped
	}
	return header + strings.Join(kept, "\n") + "\n", skipped
}

func ruleMatchesSourceCode(rule Rule) bool {
	if rule.Negated {
		return false
	}
	for _, ext := range sourceCodeExts {
		if rule.Matches("sample"+ext, false) || rule.Matches("src/sample"+ext, false) {
			return true
		}
	}
	return false
}

// ConvertGitignoreToTetherIgnore reads <workspace>/.gitignore and converts it to
// .tether/ignore format without modifying disk.
func ConvertGitignoreToTetherIgnore(workspace string) (converted string, skipped []string, err error) {
	data, err := os.ReadFile(filepath.Join(workspace, ".gitignore"))
	if err != nil {
		return "", nil, err
	}
	converted, skipped = ConvertGitignoreContent(string(data))
	return converted, skipped, nil
}

// DefaultIgnoreContent returns safe starter .tether/ignore rules for `tether init`
// when no .tether/ignore or .tetherignore exists yet.
func DefaultIgnoreContent(workspace string) string {
	defaults := []string{
		"node_modules/",
		"dist/",
		"build/",
		"target/",
		"__pycache__/",
		".venv/",
		"venv/",
	}
	seen := make(map[string]bool)
	var lines []string
	if converted, _, err := ConvertGitignoreToTetherIgnore(workspace); err == nil {
		for _, raw := range strings.Split(converted, "\n") {
			t := strings.TrimSpace(raw)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if !seen[t] {
				seen[t] = true
				lines = append(lines, t)
			}
		}
	}
	for _, d := range defaults {
		if !seen[d] {
			seen[d] = true
			lines = append(lines, d)
		}
	}
	return "# Tether mount ignore rules (.gitignore syntax; prefix with re: for Go regex)\n" + strings.Join(lines, "\n") + "\n"
}

// InitIgnoreFile creates <workspace>/.tether/ignore if absent (migrating <workspace>/.tetherignore
// if present, or seeding from .gitignore + safe defaults) and syncs rules to mount_ignore_rules.
func InitIgnoreFile(workspace string, writeDB *sql.DB) (created bool, path string, err error) {
	primary, fallback := IgnoreFilePaths(workspace)
	if _, statErr := os.Stat(primary); statErr == nil {
		if writeDB != nil {
			m := NewEngine(workspace)
			m.BindDB(nil, writeDB)
			if err := m.SyncRulesToDB(); err != nil {
				return false, primary, err
			}
		}
		return false, primary, nil
	}

	var content string
	if raw, ferr := os.ReadFile(fallback); ferr == nil {
		content = string(raw)
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
	} else {
		content = DefaultIgnoreContent(workspace)
	}

	if err := os.MkdirAll(filepath.Dir(primary), 0755); err != nil {
		return false, primary, err
	}
	if err := os.WriteFile(primary, []byte(content), 0644); err != nil {
		return false, primary, err
	}
	if writeDB != nil {
		m := NewEngine(workspace)
		m.BindDB(nil, writeDB)
		if err := m.SyncRulesToDB(); err != nil {
			return true, primary, err
		}
	}
	return true, primary, nil
}

// SaveIgnoreRules writes custom (non-builtin) rules cleanly to <workspace>/.tether/ignore
// and synchronizes mount_ignore_rules when writeDB is non-nil. It rejects invalid rules
// before mutating disk so broken syntax cannot silently corrupt active ignore rules.
func SaveIgnoreRules(workspace string, writeDB *sql.DB, customRules []Rule) error {
	primary, _ := IgnoreFilePaths(workspace)
	var lines []string
	lines = append(lines, "# Tether mount ignore rules (.gitignore syntax; prefix with re: for Go regex)")
	for _, r := range customRules {
		if r.IsBuiltin {
			continue
		}
		pat := strings.TrimSpace(r.Pattern)
		if pat == "" {
			continue
		}
		compiled, ok := ParseRule(pat)
		if !ok {
			continue
		}
		if compiled.Invalid != "" {
			return fmt.Errorf("invalid ignore rule %q: %s", pat, compiled.Invalid)
		}
		if !r.Enabled {
			lines = append(lines, "# disabled: "+pat)
			continue
		}
		lines = append(lines, pat)
	}
	if err := os.MkdirAll(filepath.Dir(primary), 0755); err != nil {
		return err
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(primary, []byte(body), 0644); err != nil {
		return err
	}
	if writeDB != nil {
		return syncRulesToDB(writeDB, customRules)
	}
	return nil
}

// LoadRulesFromDB reads persisted rules from mount_ignore_rules.
func LoadRulesFromDB(readDB *sql.DB) ([]Rule, error) {
	if readDB == nil {
		return nil, nil
	}
	rows, err := readDB.Query(`
		SELECT id, pattern, is_regex, is_builtin, enabled
		FROM mount_ignore_rules
		ORDER BY is_builtin DESC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var id int64
		var pat string
		var isRegex, isBuiltin, enabled int
		if err := rows.Scan(&id, &pat, &isRegex, &isBuiltin, &enabled); err != nil {
			return nil, err
		}
		compiledPat := pat
		if isRegex != 0 {
			if strings.HasPrefix(compiledPat, "!") {
				if !strings.HasPrefix(compiledPat[1:], "re:") {
					compiledPat = "!re:" + compiledPat[1:]
				}
			} else if !strings.HasPrefix(compiledPat, "re:") {
				compiledPat = "re:" + compiledPat
			}
		}
		r := compileRule(compiledPat, isBuiltin != 0, enabled != 0)
		r.ID = id
		r.Pattern = pat
		if isRegex != 0 {
			r.IsRegex = true
		}
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rules, nil
}

func syncRulesToDB(writeDB *sql.DB, customRules []Rule) error {
	if writeDB == nil {
		return nil
	}
	tx, err := writeDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM mount_ignore_rules`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO mount_ignore_rules (pattern, is_regex, is_builtin, enabled) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, b := range BuiltinRules() {
		if _, err := stmt.Exec(b.Pattern, boolToInt(b.IsRegex), 1, 1); err != nil {
			return err
		}
	}
	for _, r := range customRules {
		if r.IsBuiltin {
			continue
		}
		pat := strings.TrimSpace(r.Pattern)
		if pat == "" {
			continue
		}
		if _, err := stmt.Exec(pat, boolToInt(r.IsRegex), 0, boolToInt(r.Enabled)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SimulateRule tests a single raw rule pattern against workspace files and returns
// match counts, sample paths, extension counts, and warnings for critical source extensions.
func SimulateRule(workspace, rawPattern string) (SimulationResult, error) {
	rule, ok := ParseRule(rawPattern)
	if !ok {
		return SimulationResult{}, fmt.Errorf("empty or comment pattern")
	}
	return SimulateIgnoreRules(workspace, []Rule{rule})
}

// SimulateIgnoreRules evaluates a rule slice against workspace files (skipping .git/ and .tether/)
// and flags warnings when matching critical source extensions (*.go, *.ts, *.svelte, *.py, *.rs).
func SimulateIgnoreRules(workspace string, rules []Rule) (SimulationResult, error) {
	res := SimulationResult{
		ByExtension: make(map[string]int),
	}

	var compiled []Rule
	for _, r := range rules {
		c := r
		if c.re == nil && c.Invalid == "" && strings.TrimSpace(c.Pattern) != "" {
			c = compileRule(c.Pattern, c.IsBuiltin, c.Enabled)
		}
		if c.Invalid != "" {
			res.InvalidPatterns = append(res.InvalidPatterns, fmt.Sprintf("%s: %s", c.Pattern, c.Invalid))
		}
		compiled = append(compiled, c)
	}

	absRoot, err := filepath.Abs(workspace)
	if err != nil {
		return res, err
	}

	extCritical := map[string]bool{
		".go": true, ".ts": true, ".tsx": true, ".svelte": true, ".py": true, ".rs": true,
	}
	firstMatcherByExt := make(map[string]string)

	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != absRoot && (name == ".git" || name == ".tether") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(absRoot, path)
		if rerr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		res.TotalFiles++

		matched := false
		matchedPat := ""
		for _, rule := range compiled {
			if rule.Matches(relSlash, false) {
				if rule.Negated {
					matched = false
					matchedPat = ""
				} else {
					matched = true
					matchedPat = rule.Pattern
				}
			}
		}
		if matched {
			res.MatchedCount++
			if len(res.MatchedSamples) < 25 {
				res.MatchedSamples = append(res.MatchedSamples, relSlash)
			}
			ext := strings.ToLower(filepath.Ext(relSlash))
			if ext == "" {
				ext = "(none)"
			}
			res.ByExtension[ext]++
			if extCritical[ext] && firstMatcherByExt[ext] == "" {
				firstMatcherByExt[ext] = matchedPat
			}
		}
		return nil
	})
	if walkErr != nil {
		return res, walkErr
	}

	var exts []string
	for ext := range firstMatcherByExt {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	for _, ext := range exts {
		cnt := res.ByExtension[ext]
		pat := firstMatcherByExt[ext]
		res.Warnings = append(res.Warnings, ImpactWarning{
			Pattern: pat,
			Ext:     ext,
			Count:   cnt,
			Message: fmt.Sprintf("rule %q matches %d %s source file(s)", pat, cnt, ext),
		})
	}
	return res, nil
}
