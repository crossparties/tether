package context

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// DefaultMaxFileBytes is the fallback byte limit for SafeReadWorkspaceFile when maxBytes <= 0.
const DefaultMaxFileBytes int64 = 256 * 1024

// FileEntry holds metadata for a single workspace file indexed for @ autocomplete.
type FileEntry struct {
	RelPath   string
	Ext       string
	SizeBytes int64
	SizeHuman string
}

// Indexer scans a workspace directory and provides fast fuzzy filtering for @ file mentions.
type Indexer struct {
	root    string
	mu      sync.RWMutex
	entries []FileEntry
	scanned bool
}

// NewIndexer creates a new workspace file indexer rooted at root.
func NewIndexer(root string) *Indexer {
	cleanRoot := filepath.Clean(root)
	if abs, err := filepath.Abs(cleanRoot); err == nil {
		cleanRoot = abs
	}
	return &Indexer{
		root: cleanRoot,
	}
}

// Root returns the normalized root directory of the Indexer.
func (idx *Indexer) Root() string {
	return idx.root
}

// isIgnoredDir reports whether a directory name must be skipped during workspace indexing.
func isIgnoredDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".bin", ".tether":
		return true
	default:
		return false
	}
}

// isIgnoredFile reports whether a file name must be excluded from workspace indexing.
func isIgnoredFile(name string) bool {
	if name == "session.db" || strings.HasPrefix(name, "session.db-") || strings.HasPrefix(name, "session.db.") {
		return true
	}
	return false
}

// FormatByteSize formats a byte count into a compact human-readable string (e.g., "420 B", "12.4 KB").
func FormatByteSize(size int64) string {
	if size < 0 {
		size = 0
	}
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	val := float64(size) / 1024.0
	unitIdx := 0
	for val >= 1024.0 && unitIdx < len(units)-1 {
		val /= 1024.0
		unitIdx++
	}
	if val >= 10.0 {
		return fmt.Sprintf("%.0f %s", val, units[unitIdx])
	}
	return fmt.Sprintf("%.1f %s", val, units[unitIdx])
}

// detectExt extracts a normalized extension (or special filename badge) for display.
func detectExt(relPath string) string {
	base := filepath.Base(relPath)
	ext := strings.ToLower(filepath.Ext(base))
	if ext != "" {
		return ext
	}
	return base
}

// Scan walks the workspace root, ignoring excluded directories and session.db* artifacts,
// caches the sorted entries, and returns a copy of the indexed list.
func (idx *Indexer) Scan() ([]FileEntry, error) {
	info, err := os.Stat(idx.root)
	if err != nil {
		return nil, fmt.Errorf("stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %q is not a directory", idx.root)
	}

	var results []FileEntry
	walkErr := filepath.WalkDir(idx.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == idx.root {
				return err
			}
			return nil
		}

		name := d.Name()
		if d.IsDir() {
			if path != idx.root && isIgnoredDir(name) {
				return filepath.SkipDir
			}
			return nil
		}

		if isIgnoredFile(name) {
			return nil
		}

		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(idx.root, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		var sizeBytes int64
		if fi, statErr := d.Info(); statErr == nil {
			sizeBytes = fi.Size()
		}

		results = append(results, FileEntry{
			RelPath:   relSlash,
			Ext:       detectExt(relSlash),
			SizeBytes: sizeBytes,
			SizeHuman: FormatByteSize(sizeBytes),
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].RelPath < results[j].RelPath
	})

	idx.mu.Lock()
	idx.entries = results
	idx.scanned = true
	idx.mu.Unlock()

	out := make([]FileEntry, len(results))
	copy(out, results)
	return out, nil
}

// FuzzyFilter ranks and returns up to limit workspace files matching query.
// If Scan has not been called yet, FuzzyFilter triggers an initial Scan automatically.
func (idx *Indexer) FuzzyFilter(query string, limit int) []FileEntry {
	idx.mu.RLock()
	scanned := idx.scanned
	entries := idx.entries
	idx.mu.RUnlock()

	if !scanned {
		scannedEntries, err := idx.Scan()
		if err == nil {
			entries = scannedEntries
		}
	}

	if limit <= 0 {
		limit = 5
	}
	if len(entries) == 0 {
		return nil
	}

	q := strings.TrimSpace(strings.TrimPrefix(query, "@"))
	q = strings.ToLower(filepath.ToSlash(q))

	if q == "" {
		count := limit
		if count > len(entries) {
			count = len(entries)
		}
		out := make([]FileEntry, count)
		copy(out, entries[:count])
		return out
	}

	type scoredEntry struct {
		entry FileEntry
		score int
	}

	var matches []scoredEntry
	for _, e := range entries {
		if sc, ok := scoreFileMatch(e.RelPath, q); ok {
			matches = append(matches, scoredEntry{entry: e, score: sc})
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if len(matches[i].entry.RelPath) != len(matches[j].entry.RelPath) {
			return len(matches[i].entry.RelPath) < len(matches[j].entry.RelPath)
		}
		return matches[i].entry.RelPath < matches[j].entry.RelPath
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}

	out := make([]FileEntry, len(matches))
	for i, m := range matches {
		out[i] = m.entry
	}
	return out
}

func scoreFileMatch(relPath, lowerQuery string) (int, bool) {
	lowerPath := strings.ToLower(relPath)
	base := filepath.Base(lowerPath)

	if lowerPath == lowerQuery {
		return 1000, true
	}
	if base == lowerQuery {
		return 950, true
	}

	if strings.HasPrefix(base, lowerQuery) {
		return 850 - (len(base) - len(lowerQuery)), true
	}
	if strings.HasPrefix(lowerPath, lowerQuery) {
		return 800 - (len(lowerPath) - len(lowerQuery)), true
	}

	if pos := strings.Index(base, lowerQuery); pos >= 0 {
		return 700 - pos*2 - (len(base) - len(lowerQuery)), true
	}
	if pos := strings.Index(lowerPath, lowerQuery); pos >= 0 {
		bonus := 0
		if pos > 0 && (lowerPath[pos-1] == '/' || lowerPath[pos-1] == '_' || lowerPath[pos-1] == '-') {
			bonus = 40
		}
		return 600 + bonus - pos, true
	}

	qRunes := []rune(lowerQuery)
	pRunes := []rune(lowerPath)
	qi := 0
	score := 200
	lastMatchIdx := -2

	for pi, pr := range pRunes {
		if qi < len(qRunes) && pr == qRunes[qi] {
			if pi == lastMatchIdx+1 {
				score += 15
			}
			if pi == 0 || pRunes[pi-1] == '/' || pRunes[pi-1] == '_' || pRunes[pi-1] == '-' || pRunes[pi-1] == '.' {
				score += 20
			}
			lastMatchIdx = pi
			qi++
		}
	}

	if qi == len(qRunes) {
		score -= len(pRunes)
		return score, true
	}

	return 0, false
}

// ExtractAtToken inspects input around cursorPos (byte index) to detect an active @file mention token.
func ExtractAtToken(input string, cursorPos int) (query string, startByte, endByte int, active bool) {
	if len(input) == 0 {
		return "", 0, 0, false
	}
	if cursorPos < 0 {
		cursorPos = 0
	}
	if cursorPos > len(input) {
		cursorPos = len(input)
	}

	tokenStart := cursorPos
	for tokenStart > 0 {
		r := input[tokenStart-1]
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			break
		}
		tokenStart--
	}

	tokenEnd := cursorPos
	for tokenEnd < len(input) {
		r := input[tokenEnd]
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			break
		}
		tokenEnd++
	}

	if tokenStart >= len(input) || input[tokenStart] != '@' {
		return "", 0, 0, false
	}

	if cursorPos <= tokenStart {
		return "", 0, 0, false
	}

	rawToken := input[tokenStart+1 : tokenEnd]
	if strings.HasPrefix(rawToken, "@") {
		return "", 0, 0, false
	}

	return rawToken, tokenStart, tokenEnd, true
}

// ReplaceAtToken replaces the active @token span [startByte:endByte] in input with "@<relPath> "
// and returns the updated input string along with the new cursor byte position.
func ReplaceAtToken(input string, startByte, endByte int, relPath string) (newInput string, newCursorByte int) {
	if startByte < 0 {
		startByte = 0
	}
	if startByte > len(input) {
		startByte = len(input)
	}
	if endByte < startByte {
		endByte = startByte
	}
	if endByte > len(input) {
		endByte = len(input)
	}

	cleanRel := filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(relPath), "@"))
	replacement := "@" + cleanRel

	prefix := input[:startByte]
	suffix := input[endByte:]

	if len(suffix) > 0 && (suffix[0] == ' ' || suffix[0] == '\t') {
		newInput = prefix + replacement + suffix
		newCursorByte = len(prefix) + len(replacement) + 1
		return newInput, newCursorByte
	}

	replacementWithSpace := replacement + " "
	newInput = prefix + replacementWithSpace + suffix
	newCursorByte = len(prefix) + len(replacementWithSpace)
	return newInput, newCursorByte
}

// SafeReadWorkspaceFile safely reads a file inside workspaceRoot, rejecting path traversal (".."),
// symlink escapes outside workspaceRoot, directories, binary files, and files exceeding maxBytes.
func SafeReadWorkspaceFile(workspaceRoot, relOrAbsPath string, maxBytes int64) (content string, cleanRel string, err error) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return "", "", errors.New("workspace root is empty")
	}
	targetRaw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(relOrAbsPath), "@"))
	if targetRaw == "" {
		return "", "", errors.New("file path is empty")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}

	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace root: %w", err)
	}
	realRoot := absRoot
	if resolved, evalErr := filepath.EvalSymlinks(absRoot); evalErr == nil {
		realRoot = resolved
	}

	var candidatePath string
	if filepath.IsAbs(targetRaw) {
		candidatePath = filepath.Clean(targetRaw)
	} else {
		candidatePath = filepath.Clean(filepath.Join(realRoot, targetRaw))
	}

	relCheck, err := filepath.Rel(realRoot, candidatePath)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("access denied: path %q escapes workspace root", relOrAbsPath)
	}

	realTarget, err := filepath.EvalSymlinks(candidatePath)
	if err != nil {
		return "", "", fmt.Errorf("resolve file path %q: %w", relOrAbsPath, err)
	}

	relReal, err := filepath.Rel(realRoot, realTarget)
	if err != nil || relReal == ".." || strings.HasPrefix(relReal, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("access denied: symlink %q points outside workspace root", relOrAbsPath)
	}

	cleanRel = filepath.ToSlash(relReal)
	if isIgnoredFile(filepath.Base(cleanRel)) {
		return "", "", fmt.Errorf("access denied: reading internal database %q is restricted", cleanRel)
	}

	info, err := os.Stat(realTarget)
	if err != nil {
		return "", "", fmt.Errorf("stat %q: %w", cleanRel, err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("path %q is a directory, not a regular file", cleanRel)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("path %q is not a regular file", cleanRel)
	}
	if info.Size() > maxBytes {
		return "", cleanRel, fmt.Errorf("file %q size (%d bytes) exceeds maximum allowed (%d bytes)", cleanRel, info.Size(), maxBytes)
	}

	f, err := os.Open(realTarget)
	if err != nil {
		return "", cleanRel, fmt.Errorf("open %q: %w", cleanRel, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", cleanRel, fmt.Errorf("read %q: %w", cleanRel, err)
	}
	if int64(len(data)) > maxBytes {
		return "", cleanRel, fmt.Errorf("file %q exceeds maximum allowed size (%d bytes)", cleanRel, maxBytes)
	}

	if isBinaryContent(data) {
		return "", cleanRel, fmt.Errorf("file %q appears to be a binary file", cleanRel)
	}

	return string(data), cleanRel, nil
}

func isBinaryContent(data []byte) bool {
	sample := data
	if len(sample) > 8000 {
		sample = sample[:8000]
	}
	for _, b := range sample {
		if b == 0 {
			return true
		}
	}
	nonPrintable := 0
	totalRunes := 0
	for _, r := range string(sample) {
		totalRunes++
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			nonPrintable++
		}
	}
	return totalRunes > 0 && float64(nonPrintable)/float64(totalRunes) > 0.30
}
