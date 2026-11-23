// Package repomap builds deterministic, mtime-cached symbol maps of authorized
// workspace source files using 100% pure-Go parsers and scanners (CGO_ENABLED=0).
package repomap

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/crossparties/tether/internal/mount"
)

const (
	// DefaultBaselineTokens is the target token budget for Tier 3 frozen baseline RepoMap.
	DefaultBaselineTokens = 1024
	// DefaultDeltaTokens is the target token budget for per-turn tail Focused Delta RepoMap.
	DefaultDeltaTokens = 380
	maxRepoMapFileSize = 512 * 1024
)

// Tag represents a single extracted symbol definition in a source file.
type Tag struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Line      int    `json:"line"`
	Signature string `json:"signature"`
}

type cachedFile struct {
	RelPath string
	MtimeNs int64
	Lang    string
	Tags    []Tag
	Refs    []string
}

// RefreshStats records cache-hit vs re-parse counts from Builder.Refresh.
type RefreshStats struct {
	TotalFiles  int
	CacheHits   int
	ParsedFiles int
	PurgedFiles int
}

// Builder indexes authorized workspace source files and emits deterministic,
// token-budgeted RepoMap views backed by .tether/cache.db.
type Builder struct {
	workspace   string
	cacheDB     *sql.DB
	mountEngine *mount.Engine
}

// NewBuilder constructs a pure-Go RepoMap Builder rooted at workspace.
func NewBuilder(workspace string, cacheDB *sql.DB, mountEngine *mount.Engine) *Builder {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		ws = "."
	}
	if abs, err := filepath.Abs(ws); err == nil {
		ws = abs
	}
	if mountEngine == nil {
		mountEngine = mount.NewEngine(ws)
	}
	return &Builder{
		workspace:   ws,
		cacheDB:     cacheDB,
		mountEngine: mountEngine,
	}
}

// EstimateTokens approximates token count deterministically (~4 chars per token).
func EstimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	n := (len(s) + 3) / 4
	if n == 0 {
		return 1
	}
	return n
}

// Refresh scans authorized source files, reuses .tether/cache.db entries whose
// mtime_ns matches disk, parses only changed/new files, and purges stale/ignored rows.
func (b *Builder) Refresh() (RefreshStats, error) {
	_, stats, err := b.loadAndRefresh()
	return stats, err
}

// BuildBaseline builds the deterministic ~800-1024 token RepoMap for Tier 3 frozen head.
func (b *Builder) BuildBaseline(tokenBudget int) string {
	text, _ := b.BuildBaselineDetailed(tokenBudget)
	return text
}

// BuildBaselineDetailed returns the formatted baseline RepoMap and the set of
// relative file paths included in it (used to avoid redundancy in BuildDelta).
func (b *Builder) BuildBaselineDetailed(tokenBudget int) (string, map[string]bool) {
	if tokenBudget <= 0 {
		tokenBudget = DefaultBaselineTokens
	}
	files, _, err := b.loadAndRefresh()
	if err != nil || len(files) == 0 {
		return "", nil
	}

	ranked := rankFiles(files)
	covered := make(map[string]bool)

	var out strings.Builder
	header := "# RepoMap (Baseline)\n"
	out.WriteString(header)
	usedTokens := EstimateTokens(header)

	for _, rf := range ranked {
		if len(rf.file.Tags) == 0 {
			continue
		}
		block := formatFileTags(rf.file, tokenBudget-usedTokens)
		if block == "" {
			continue
		}
		bt := EstimateTokens(block)
		if usedTokens+bt > tokenBudget && len(covered) > 0 {
			break
		}
		out.WriteString(block)
		usedTokens += bt
		covered[rf.file.RelPath] = true
		if usedTokens >= tokenBudget {
			break
		}
	}

	if len(covered) == 0 {
		return "", nil
	}
	return strings.TrimRight(out.String(), "\n"), covered
}

// BuildDelta builds a compact (~300-400 token) RepoMap focused on active/mentioned
// files and their 1-hop symbol neighbors not already covered by excludeBaseline.
func (b *Builder) BuildDelta(focusFiles []string, excludeBaseline map[string]bool, tokenBudget int) string {
	if tokenBudget <= 0 {
		tokenBudget = DefaultDeltaTokens
	}
	if len(focusFiles) == 0 {
		return ""
	}
	files, _, err := b.loadAndRefresh()
	if err != nil || len(files) == 0 {
		return ""
	}

	byPath := make(map[string]cachedFile, len(files))
	defToFiles := make(map[string][]string)
	for _, f := range files {
		byPath[f.RelPath] = f
		for _, t := range f.Tags {
			if t.Name != "" {
				defToFiles[t.Name] = append(defToFiles[t.Name], f.RelPath)
			}
		}
	}

	focusSet := make(map[string]bool)
	neighborScore := make(map[string]int)
	for _, raw := range focusFiles {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
		if trimmed == "" {
			continue
		}
		dec := b.mountEngine.AuthorizePath(trimmed, mount.OpRead, "code")
		if !dec.Allowed || dec.IsDir {
			continue
		}
		if f, ok := byPath[dec.RelPath]; ok {
			focusSet[f.RelPath] = true
			for _, ref := range f.Refs {
				for _, target := range defToFiles[ref] {
					if target != f.RelPath {
						neighborScore[target] += 2
					}
				}
			}
		}
	}
	if len(focusSet) == 0 {
		return ""
	}

	type deltaCandidate struct {
		file  cachedFile
		score int
	}
	var cands []deltaCandidate
	for _, f := range files {
		if len(f.Tags) == 0 || (excludeBaseline != nil && excludeBaseline[f.RelPath]) {
			continue
		}
		if focusSet[f.RelPath] {
			cands = append(cands, deltaCandidate{file: f, score: 1000 + len(f.Tags)})
		} else if sc := neighborScore[f.RelPath]; sc > 0 {
			cands = append(cands, deltaCandidate{file: f, score: sc})
		}
	}
	if len(cands) == 0 {
		return ""
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].file.RelPath < cands[j].file.RelPath
	})

	var out strings.Builder
	header := "# RepoMap (Focused Delta)\n"
	out.WriteString(header)
	usedTokens := EstimateTokens(header)
	included := 0

	for _, c := range cands {
		block := formatFileTags(c.file, tokenBudget-usedTokens)
		if block == "" {
			continue
		}
		bt := EstimateTokens(block)
		if usedTokens+bt > tokenBudget && included > 0 {
			break
		}
		out.WriteString(block)
		usedTokens += bt
		included++
		if usedTokens >= tokenBudget {
			break
		}
	}
	if included == 0 {
		return ""
	}
	return strings.TrimRight(out.String(), "\n")
}

type cachePayload struct {
	Tags []Tag    `json:"tags"`
	Refs []string `json:"refs,omitempty"`
}

func (b *Builder) loadAndRefresh() ([]cachedFile, RefreshStats, error) {
	var stats RefreshStats
	if err := b.ensureTable(); err != nil {
		return nil, stats, err
	}

	cached := make(map[string]cachedFile)
	if b.cacheDB != nil {
		rows, err := b.cacheDB.Query(`SELECT file_path, mtime_ns, lang, tags_json FROM repomap_tags`)
		if err != nil {
			return nil, stats, err
		}
		for rows.Next() {
			var fp, lang, rawJSON string
			var mtime int64
			if err := rows.Scan(&fp, &mtime, &lang, &rawJSON); err != nil {
				_ = rows.Close()
				return nil, stats, err
			}
			tags, refs := decodeCachedPayload(rawJSON)
			cached[fp] = cachedFile{
				RelPath: fp,
				MtimeNs: mtime,
				Lang:    lang,
				Tags:    tags,
				Refs:    refs,
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, stats, err
		}
		_ = rows.Close()
	}

	type diskTarget struct {
		relPath string
		absPath string
		lang    string
		mtimeNs int64
	}
	var targets []diskTarget

	walkErr := filepath.WalkDir(b.workspace, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path == b.workspace {
				return nil
			}
			name := d.Name()
			if name == ".git" || name == ".tether" || strings.HasPrefix(name, ".") || mount.IsDefaultSkipDirName(name) {
				return filepath.SkipDir
			}
			if relDir, err := filepath.Rel(b.workspace, path); err == nil {
				if b.mountEngine.ShouldSkipDir(filepath.ToSlash(relDir)) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(b.workspace, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		lang := detectLanguage(relSlash)
		if lang == "" {
			return nil
		}
		if !b.mountEngine.ShouldIndexRepoMap(relSlash) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > maxRepoMapFileSize {
			return nil
		}
		targets = append(targets, diskTarget{
			relPath: relSlash,
			absPath: path,
			lang:    lang,
			mtimeNs: info.ModTime().UnixNano(),
		})
		return nil
	})
	if walkErr != nil {
		return nil, stats, walkErr
	}

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].relPath < targets[j].relPath
	})

	activeSet := make(map[string]bool, len(targets))
	out := make([]cachedFile, 0, len(targets))
	var toUpsert []cachedFile

	for _, t := range targets {
		activeSet[t.relPath] = true
		stats.TotalFiles++

		if prev, ok := cached[t.relPath]; ok && prev.MtimeNs == t.mtimeNs && prev.MtimeNs != 0 && prev.Lang == t.lang {
			stats.CacheHits++
			out = append(out, prev)
			continue
		}

		src, err := os.ReadFile(t.absPath)
		if err != nil {
			continue
		}
		tags, refs := extractTags(t.relPath, t.lang, src)
		cf := cachedFile{
			RelPath: t.relPath,
			MtimeNs: t.mtimeNs,
			Lang:    t.lang,
			Tags:    tags,
			Refs:    refs,
		}
		stats.ParsedFiles++
		out = append(out, cf)
		toUpsert = append(toUpsert, cf)
	}

	var toPurge []string
	for fp := range cached {
		if !activeSet[fp] {
			toPurge = append(toPurge, fp)
		}
	}
	sort.Strings(toPurge)
	stats.PurgedFiles = len(toPurge)

	if b.cacheDB != nil && (len(toUpsert) > 0 || len(toPurge) > 0) {
		if err := b.persistCacheChanges(toUpsert, toPurge); err != nil {
			return out, stats, err
		}
	}
	return out, stats, nil
}

func (b *Builder) ensureTable() error {
	if b.cacheDB == nil {
		return nil
	}
	_, err := b.cacheDB.Exec(`
		CREATE TABLE IF NOT EXISTS repomap_tags (
			file_path TEXT PRIMARY KEY,
			mtime_ns INTEGER NOT NULL DEFAULT 0,
			lang TEXT NOT NULL DEFAULT '',
			tags_json TEXT NOT NULL DEFAULT '[]'
		)
	`)
	return err
}

func (b *Builder) persistCacheChanges(upserts []cachedFile, purges []string) error {
	tx, err := b.cacheDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if len(purges) > 0 {
		delStmt, err := tx.Prepare(`DELETE FROM repomap_tags WHERE file_path = ?`)
		if err != nil {
			return err
		}
		for _, fp := range purges {
			if _, err := delStmt.Exec(fp); err != nil {
				_ = delStmt.Close()
				return err
			}
		}
		_ = delStmt.Close()
	}

	if len(upserts) > 0 {
		insStmt, err := tx.Prepare(`
			INSERT INTO repomap_tags (file_path, mtime_ns, lang, tags_json)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(file_path) DO UPDATE SET
				mtime_ns = excluded.mtime_ns,
				lang = excluded.lang,
				tags_json = excluded.tags_json
		`)
		if err != nil {
			return err
		}
		for _, cf := range upserts {
			payload, _ := json.Marshal(cachePayload{Tags: cf.Tags, Refs: cf.Refs})
			if _, err := insStmt.Exec(cf.RelPath, cf.MtimeNs, cf.Lang, string(payload)); err != nil {
				_ = insStmt.Close()
				return err
			}
		}
		_ = insStmt.Close()
	}

	return tx.Commit()
}

func decodeCachedPayload(raw string) ([]Tag, []string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var tags []Tag
		_ = json.Unmarshal([]byte(trimmed), &tags)
		return tags, nil
	}
	var p cachePayload
	if err := json.Unmarshal([]byte(trimmed), &p); err == nil {
		return p.Tags, p.Refs
	}
	return nil, nil
}

type rankedFile struct {
	file  cachedFile
	score int
}

func rankFiles(files []cachedFile) []rankedFile {
	defFiles := make(map[string]map[string]bool)
	for _, f := range files {
		for _, t := range f.Tags {
			if t.Name == "" {
				continue
			}
			if defFiles[t.Name] == nil {
				defFiles[t.Name] = make(map[string]bool)
			}
			defFiles[t.Name][f.RelPath] = true
		}
	}

	inRefs := make(map[string]int, len(files))
	for _, f := range files {
		seenTargets := make(map[string]bool)
		for _, ref := range f.Refs {
			for target := range defFiles[ref] {
				if target != f.RelPath && !seenTargets[target] {
					seenTargets[target] = true
					inRefs[target]++
				}
			}
		}
	}

	ranked := make([]rankedFile, 0, len(files))
	for _, f := range files {
		if len(f.Tags) == 0 {
			continue
		}
		score := inRefs[f.RelPath]*20 + len(f.Tags)*2
		if !isTestFile(f.RelPath) {
			score += 30
		}
		if isEntrypointFile(f.RelPath) {
			score += 25
		}
		ranked = append(ranked, rankedFile{file: f, score: score})
	}

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].file.RelPath < ranked[j].file.RelPath
	})
	return ranked
}

func isTestFile(rel string) bool {
	low := strings.ToLower(rel)
	base := filepath.Base(low)
	return strings.HasSuffix(low, "_test.go") ||
		strings.Contains(low, ".test.") ||
		strings.Contains(low, ".spec.") ||
		strings.HasPrefix(base, "test_")
}

func isEntrypointFile(rel string) bool {
	low := strings.ToLower(filepath.Base(rel))
	switch low {
	case "main.go", "engine.go", "runner.go", "server.go", "app.svelte", "+page.svelte", "index.ts", "main.py", "lib.rs", "main.rs":
		return true
	}
	return false
}

func formatFileTags(f cachedFile, remainingTokens int) string {
	if len(f.Tags) == 0 || remainingTokens <= 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", f.RelPath)
	const maxTagsPerFile = 14
	written := 0
	for _, t := range f.Tags {
		if written >= maxTagsPerFile {
			fmt.Fprintf(&b, "  ... (%d more symbols)\n", len(f.Tags)-written)
			break
		}
		line := fmt.Sprintf("  L%d: %s\n", t.Line, t.Signature)
		if EstimateTokens(b.String()+line) > remainingTokens && written > 0 {
			break
		}
		b.WriteString(line)
		written++
	}
	if written == 0 {
		return ""
	}
	return b.String()
}

func detectLanguage(relSlash string) string {
	switch strings.ToLower(filepath.Ext(relSlash)) {
	case ".go":
		return "go"
	case ".ts":
		return "ts"
	case ".tsx":
		return "tsx"
	case ".js":
		return "js"
	case ".jsx":
		return "jsx"
	case ".svelte":
		return "svelte"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	default:
		return ""
	}
}

func extractTags(relPath, lang string, src []byte) ([]Tag, []string) {
	switch lang {
	case "go":
		return extractGoTags(relPath, src)
	case "ts", "tsx", "js", "jsx", "svelte":
		return extractJSTSOrSvelteTags(lang, src)
	case "python":
		return extractPythonTags(src)
	case "rust":
		return extractRustTags(src)
	default:
		return nil, nil
	}
}

func extractGoTags(relPath string, src []byte) ([]Tag, []string) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relPath, src, 0)
	if err != nil && file == nil {
		return nil, nil
	}

	var tags []Tag
	defined := make(map[string]bool)

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			line := fset.Position(d.Pos()).Line
			sig := formatGoFuncSig(src, fset, d)
			tags = append(tags, Tag{
				Name:      d.Name.Name,
				Kind:      "func",
				Line:      line,
				Signature: sig,
			})
			defined[d.Name.Name] = true

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Name == nil {
						continue
					}
					line := fset.Position(sp.Pos()).Line
					kind := "type"
					switch sp.Type.(type) {
					case *ast.StructType:
						kind = "struct"
					case *ast.InterfaceType:
						kind = "interface"
					}
					tags = append(tags, Tag{
						Name:      sp.Name.Name,
						Kind:      kind,
						Line:      line,
						Signature: fmt.Sprintf("type %s %s", sp.Name.Name, kind),
					})
					defined[sp.Name.Name] = true

				case *ast.ValueSpec:
					if d.Tok != token.CONST && d.Tok != token.VAR {
						continue
					}
					for _, id := range sp.Names {
						if id == nil || id.Name == "_" || !id.IsExported() {
							continue
						}
						tags = append(tags, Tag{
							Name:      id.Name,
							Kind:      strings.ToLower(d.Tok.String()),
							Line:      fset.Position(id.Pos()).Line,
							Signature: fmt.Sprintf("%s %s", strings.ToLower(d.Tok.String()), id.Name),
						})
						defined[id.Name] = true
					}
				}
			}
		}
	}

	refSet := make(map[string]bool)
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if ok && id.Name != "" && id.Name != "_" && !defined[id.Name] && len(id.Name) > 2 {
			refSet[id.Name] = true
		}
		return true
	})

	return sortAndDedupeTags(tags), sortedKeys(refSet, 64)
}

func formatGoFuncSig(src []byte, fset *token.FileSet, fn *ast.FuncDecl) string {
	start := fset.Position(fn.Pos()).Offset
	end := fset.Position(fn.Type.End()).Offset
	if fn.Body != nil {
		bodyStart := fset.Position(fn.Body.Pos()).Offset
		if bodyStart > start && bodyStart <= len(src) {
			end = bodyStart
		}
	}
	if start >= 0 && end > start && end <= len(src) {
		sig := strings.Join(strings.Fields(string(src[start:end])), " ")
		return truncateSig(sig, 96)
	}
	return "func " + fn.Name.Name + "()"
}

func extractJSTSOrSvelteTags(lang string, src []byte) ([]Tag, []string) {
	text := string(src)
	if lang == "svelte" {
		script, baseLine := extractSvelteScripts(text)
		tags, refs := scanCStyleDecls(script, baseLine, jsKeywords)
		return tags, refs
	}
	return scanCStyleDecls(text, 1, jsKeywords)
}

func extractSvelteScripts(src string) (string, int) {
	lower := strings.ToLower(src)
	var b strings.Builder
	firstLine := 1
	found := false
	pos := 0
	for {
		openIdx := strings.Index(lower[pos:], "<script")
		if openIdx < 0 {
			break
		}
		openAbs := pos + openIdx
		tagEnd := strings.IndexByte(lower[openAbs:], '>')
		if tagEnd < 0 {
			break
		}
		contentStart := openAbs + tagEnd + 1
		closeIdx := strings.Index(lower[contentStart:], "</script>")
		if closeIdx < 0 {
			break
		}
		if !found {
			firstLine = 1 + strings.Count(src[:contentStart], "\n")
			found = true
		} else {
			b.WriteByte('\n')
		}
		b.WriteString(src[contentStart : contentStart+closeIdx])
		pos = contentStart + closeIdx + len("</script>")
	}
	return b.String(), firstLine
}

var jsKeywords = map[string]bool{
	"export": true, "default": true, "declare": true, "async": true,
	"function": true, "class": true, "interface": true, "type": true,
	"enum": true, "const": true, "let": true, "var": true,
}

func extractRustTags(src []byte) ([]Tag, []string) {
	return scanRustDecls(string(src))
}

func scanCStyleDecls(text string, startLine int, kw map[string]bool) ([]Tag, []string) {
	tokens := scanTokens(text, startLine, true)
	var tags []Tag
	refSet := make(map[string]bool)
	defined := make(map[string]bool)

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.kind != tokIdent {
			continue
		}
		if !kw[tok.val] {
			if len(tok.val) > 2 && !isCommonNoiseWord(tok.val) {
				refSet[tok.val] = true
			}
			continue
		}
		switch tok.val {
		case "class", "interface", "type", "enum":
			if i+1 < len(tokens) && tokens[i+1].kind == tokIdent {
				name := tokens[i+1].val
				tags = append(tags, Tag{
					Name:      name,
					Kind:      tok.val,
					Line:      tok.line,
					Signature: fmt.Sprintf("%s %s", tok.val, name),
				})
				defined[name] = true
				i++
			}
		case "function":
			if i+1 < len(tokens) && tokens[i+1].kind == tokIdent {
				name := tokens[i+1].val
				tags = append(tags, Tag{
					Name:      name,
					Kind:      "func",
					Line:      tok.line,
					Signature: fmt.Sprintf("function %s(%s)", name, collectParenPreview(tokens, i+2)),
				})
				defined[name] = true
				i++
			} else if i+2 < len(tokens) && tokens[i+1].val == "*" && tokens[i+2].kind == tokIdent {
				name := tokens[i+2].val
				tags = append(tags, Tag{
					Name:      name,
					Kind:      "func",
					Line:      tok.line,
					Signature: fmt.Sprintf("function* %s()", name),
				})
				defined[name] = true
				i += 2
			}
		case "const", "let", "var":
			if tok.col == 1 || (i > 0 && (tokens[i-1].val == "export" || tokens[i-1].val == "declare")) {
				if i+2 < len(tokens) && tokens[i+1].kind == tokIdent && tokens[i+2].val == "=" {
					name := tokens[i+1].val
					isArrow := hasArrowSoon(tokens, i+3)
					if isArrow || (i > 0 && tokens[i-1].val == "export") || isUpperSnake(name) {
						kind := tok.val
						sig := fmt.Sprintf("%s %s", tok.val, name)
						if isArrow {
							kind = "func"
							sig = fmt.Sprintf("const %s = (...)", name)
						}
						tags = append(tags, Tag{
							Name:      name,
							Kind:      kind,
							Line:      tok.line,
							Signature: sig,
						})
						defined[name] = true
					}
				}
			}
		}
	}

	for name := range defined {
		delete(refSet, name)
	}
	return sortAndDedupeTags(tags), sortedKeys(refSet, 64)
}

func scanRustDecls(text string) ([]Tag, []string) {
	tokens := scanTokens(text, 1, true)
	var tags []Tag
	refSet := make(map[string]bool)
	defined := make(map[string]bool)

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.kind != tokIdent {
			continue
		}
		switch tok.val {
		case "fn", "struct", "enum", "trait", "type", "mod":
			if i+1 < len(tokens) && tokens[i+1].kind == tokIdent {
				name := tokens[i+1].val
				sig := fmt.Sprintf("%s %s", tok.val, name)
				if tok.val == "fn" {
					sig = fmt.Sprintf("fn %s(%s)", name, collectParenPreview(tokens, i+2))
				}
				tags = append(tags, Tag{
					Name:      name,
					Kind:      tok.val,
					Line:      tok.line,
					Signature: sig,
				})
				defined[name] = true
				i++
			}
		case "impl":
			if i+1 < len(tokens) && tokens[i+1].kind == tokIdent {
				name := tokens[i+1].val
				// Capture `impl Type` or `impl Trait for Type`
				for j := i + 1; j < len(tokens) && j < i+8; j++ {
					if tokens[j].val == "{" || tokens[j].val == ";" {
						break
					}
					if tokens[j].val == "for" && j+1 < len(tokens) && tokens[j+1].kind == tokIdent {
						name = tokens[j+1].val
						break
					}
				}
				tags = append(tags, Tag{
					Name:      name,
					Kind:      "impl",
					Line:      tok.line,
					Signature: "impl " + name,
				})
			}
		case "const":
			if i+1 < len(tokens) && tokens[i+1].kind == tokIdent && isUpperSnake(tokens[i+1].val) {
				name := tokens[i+1].val
				tags = append(tags, Tag{
					Name:      name,
					Kind:      "const",
					Line:      tok.line,
					Signature: "const " + name,
				})
				defined[name] = true
			}
		default:
			if len(tok.val) > 2 && !isCommonNoiseWord(tok.val) {
				refSet[tok.val] = true
			}
		}
	}

	for name := range defined {
		delete(refSet, name)
	}
	return sortAndDedupeTags(tags), sortedKeys(refSet, 64)
}

func extractPythonTags(src []byte) ([]Tag, []string) {
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	var tags []Tag
	refSet := make(map[string]bool)
	defined := make(map[string]bool)

	inTriple := ""
	for idx, raw := range lines {
		lineNo := idx + 1
		clean, nextTriple := stripPythonLine(raw, inTriple)
		inTriple = nextTriple
		trimmed := strings.TrimSpace(clean)
		if trimmed == "" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))

		switch {
		case strings.HasPrefix(trimmed, "class "):
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "class "))
			name := leadingIdent(rest)
			if name != "" {
				tags = append(tags, Tag{
					Name:      name,
					Kind:      "class",
					Line:      lineNo,
					Signature: truncateSig(strings.TrimSuffix(trimmed, ":"), 84),
				})
				defined[name] = true
			}
		case strings.HasPrefix(trimmed, "async def "):
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "async def "))
			name := leadingIdent(rest)
			if name != "" {
				tags = append(tags, Tag{
					Name:      name,
					Kind:      "func",
					Line:      lineNo,
					Signature: truncateSig(strings.TrimSuffix(trimmed, ":"), 84),
				})
				defined[name] = true
			}
		case strings.HasPrefix(trimmed, "def "):
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "def "))
			name := leadingIdent(rest)
			if name != "" {
				tags = append(tags, Tag{
					Name:      name,
					Kind:      "func",
					Line:      lineNo,
					Signature: truncateSig(strings.TrimSuffix(trimmed, ":"), 84),
				})
				defined[name] = true
			}
		case indent == 0:
			if eq := strings.IndexByte(trimmed, '='); eq > 0 && !strings.Contains(trimmed[:eq], "(") {
				name := strings.TrimSpace(trimmed[:eq])
				if isUpperSnake(name) {
					tags = append(tags, Tag{
						Name:      name,
						Kind:      "const",
						Line:      lineNo,
						Signature: name,
					})
					defined[name] = true
				}
			}
		}

		for _, tok := range scanIdentTokens(clean) {
			if len(tok) > 2 && !isCommonNoiseWord(tok) {
				refSet[tok] = true
			}
		}
	}

	for name := range defined {
		delete(refSet, name)
	}
	return sortAndDedupeTags(tags), sortedKeys(refSet, 64)
}

func stripPythonLine(line, inTriple string) (string, string) {
	var b strings.Builder
	i := 0
	for i < len(line) {
		if inTriple != "" {
			if strings.HasPrefix(line[i:], inTriple) {
				i += len(inTriple)
				inTriple = ""
				continue
			}
			i++
			continue
		}
		if strings.HasPrefix(line[i:], `"""`) {
			inTriple = `"""`
			i += 3
			continue
		}
		if strings.HasPrefix(line[i:], `'''`) {
			inTriple = `'''`
			i += 3
			continue
		}
		c := line[i]
		if c == '#' {
			break
		}
		if c == '\'' || c == '"' {
			q := c
			i++
			for i < len(line) {
				if line[i] == '\\' && i+1 < len(line) {
					i += 2
					continue
				}
				if line[i] == q {
					i++
					break
				}
				i++
			}
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), inTriple
}

type tokenKind int

const (
	tokIdent tokenKind = iota
	tokPunct
)

type lexToken struct {
	kind tokenKind
	val  string
	line int
	col  int
}

func scanTokens(src string, startLine int, backtickStrings bool) []lexToken {
	var out []lexToken
	line := startLine
	col := 1
	n := len(src)

	for i := 0; i < n; {
		c := src[i]
		if c == '\n' {
			line++
			col = 1
			i++
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' {
			col++
			i++
			continue
		}
		if i+1 < n && src[i] == '/' && src[i+1] == '/' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < n && src[i] == '/' && src[i+1] == '*' {
			i += 2
			col += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				if src[i] == '\n' {
					line++
					col = 1
				} else {
					col++
				}
				i++
			}
			if i+1 < n {
				i += 2
				col += 2
			}
			continue
		}
		if c == '"' || c == '\'' || (backtickStrings && c == '`') {
			q := c
			i++
			col++
			for i < n {
				if src[i] == '\\' && i+1 < n {
					i += 2
					col += 2
					continue
				}
				if src[i] == '\n' {
					line++
					col = 1
					i++
					if q != '`' {
						break
					}
					continue
				}
				if src[i] == q {
					i++
					col++
					break
				}
				i++
				col++
			}
			continue
		}
		if isIdentStart(c) {
			start := i
			startCol := col
			for i < n && isIdentPart(src[i]) {
				i++
				col++
			}
			out = append(out, lexToken{kind: tokIdent, val: src[start:i], line: line, col: startCol})
			continue
		}
		if i+1 < n && src[i] == '=' && src[i+1] == '>' {
			out = append(out, lexToken{kind: tokPunct, val: "=>", line: line, col: col})
			i += 2
			col += 2
			continue
		}
		out = append(out, lexToken{kind: tokPunct, val: string(c), line: line, col: col})
		i++
		col++
	}
	return out
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func collectParenPreview(tokens []lexToken, startIdx int) string {
	if startIdx >= len(tokens) || tokens[startIdx].val != "(" {
		return ""
	}
	var parts []string
	depth := 0
	for i := startIdx; i < len(tokens) && i < startIdx+16; i++ {
		v := tokens[i].val
		if v == "(" {
			depth++
			if depth == 1 {
				continue
			}
		} else if v == ")" {
			depth--
			if depth == 0 {
				break
			}
		}
		if depth >= 1 {
			parts = append(parts, v)
		}
	}
	joined := strings.Join(parts, " ")
	joined = strings.ReplaceAll(joined, " , ", ", ")
	joined = strings.ReplaceAll(joined, " : ", ": ")
	return truncateSig(joined, 48)
}

func hasArrowSoon(tokens []lexToken, startIdx int) bool {
	for i := startIdx; i < len(tokens) && i < startIdx+24; i++ {
		switch tokens[i].val {
		case "=>":
			return true
		case ";", "{":
			return false
		}
	}
	return false
}

func isUpperSnake(s string) bool {
	if len(s) < 2 {
		return false
	}
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
			if !unicode.IsUpper(r) {
				return false
			}
		} else if !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return hasLetter
}

func leadingIdent(s string) string {
	end := 0
	for end < len(s) && isIdentPart(s[end]) {
		end++
	}
	return s[:end]
}

func scanIdentTokens(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if isIdentStart(s[i]) {
			start := i
			for i < len(s) && isIdentPart(s[i]) {
				i++
			}
			out = append(out, s[start:i])
		} else {
			i++
		}
	}
	return out
}

func isCommonNoiseWord(s string) bool {
	switch s {
	case "return", "const", "let", "var", "import", "from", "export", "default",
		"true", "false", "null", "undefined", "None", "True", "False", "self", "this",
		"string", "number", "boolean", "void", "any", "async", "await", "for", "while",
		"else", "elif", "if", "pub", "use", "mut", "impl", "match", "struct", "enum":
		return true
	}
	return false
}

func truncateSig(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func sortAndDedupeTags(tags []Tag) []Tag {
	if len(tags) == 0 {
		return nil
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Line != tags[j].Line {
			return tags[i].Line < tags[j].Line
		}
		if tags[i].Name != tags[j].Name {
			return tags[i].Name < tags[j].Name
		}
		return tags[i].Kind < tags[j].Kind
	})
	out := make([]Tag, 0, len(tags))
	for _, t := range tags {
		if len(out) > 0 && out[len(out)-1].Line == t.Line && out[len(out)-1].Name == t.Name && out[len(out)-1].Kind == t.Kind {
			continue
		}
		out = append(out, t)
	}
	return out
}

func sortedKeys(m map[string]bool, limit int) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	return keys
}
