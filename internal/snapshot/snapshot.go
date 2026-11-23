// Package snapshot keeps the previous contents of files the agent is about to
// modify, so a bad edit can be reverted without depending on git.
//
// A snapshot is not a version control system. It captures the state of each
// file immediately before the first write in a turn (tagged with turn_id and
// optional crew_id) plus PostHash after the write completes so /undo can safely
// detect and refuse overwriting manual external edits unless force=true.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crossparties/tether/internal/mount"
)

// State describes what a snapshot holds for one file.
type State string

const (
	// Modified means the file existed and its contents were saved.
	Modified State = "modified"
	// Created means the file did not exist, so undoing means deleting it.
	Created State = "created"
)

// ErrExternalModification is returned when a target file on disk no longer
// matches the snapshot's recorded PostHash and force=false.
var ErrExternalModification = errors.New("file modified externally after agent write")

// Entry is one file captured in a turn.
type Entry struct {
	// Path is stored relative to the workspace when possible, so a snapshot
	// stays valid if the workspace is moved.
	Path     string    `json:"path"`
	State    State     `json:"state"`
	Backup   string    `json:"backup,omitempty"`
	CrewID   string    `json:"crew_id,omitempty"`
	PostHash string    `json:"post_hash,omitempty"`
	Created  time.Time `json:"created"`
}

// Index is the manifest of a single turn.
type Index struct {
	Turn    string    `json:"turn"`
	CrewID  string    `json:"crew_id,omitempty"`
	Created time.Time `json:"created"`
	Entries []Entry   `json:"entries"`
}

// RestoreResult reports per-file restore success or external-modification conflict.
type RestoreResult struct {
	Turn         string `json:"turn"`
	Path         string `json:"path"`
	CrewID       string `json:"crew_id,omitempty"`
	Restored     bool   `json:"restored"`
	Conflict     bool   `json:"conflict,omitempty"`
	ExpectedHash string `json:"expected_hash,omitempty"`
	ActualHash   string `json:"actual_hash,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Store writes snapshots beneath <root>/.tether/snapshots.
type Store struct {
	root string
	now  func() time.Time
	mu   sync.Mutex
}

// New returns a Store rooted at the workspace directory.
func New(workspace string) *Store {
	return &Store{root: filepath.Join(workspace, ".tether", "snapshots"), now: time.Now}
}

// dir returns the directory for a turn.
func (s *Store) dir(turn string) string {
	return filepath.Join(s.root, sanitize(turn))
}

// Capture records the current state of path under the given turn. Capturing the
// same file twice in one turn is a no-op, so the state that undo restores is
// always the state before the first write — not an intermediate edit.
func (s *Store) Capture(turn, path string) error {
	return s.CaptureForCrew(turn, "", path)
}

// CaptureForCrew records the pre-write state of path tagged with turn and crewID.
func (s *Store) CaptureForCrew(turn, crewID, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.load(turn)
	if err != nil {
		return err
	}
	if crewID != "" && index.CrewID == "" {
		index.CrewID = crewID
	}

	resolvedPath, err := s.resolve(path)
	if err != nil {
		return err
	}
	stored, err := s.relativePath(resolvedPath)
	if err != nil {
		return err
	}
	for _, e := range index.Entries {
		if e.Path == stored {
			return nil // already captured this turn
		}
	}

	dir := s.dir(turn)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	content, readErr := os.ReadFile(resolvedPath)
	entry := Entry{Path: stored, CrewID: crewID, Created: s.now()}

	switch {
	case readErr == nil:
		entry.State = Modified
		entry.Backup = filepath.Join(sanitize(turn), sanitize(stored)+".bak")
		if err := os.WriteFile(filepath.Join(s.root, entry.Backup), content, 0644); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	case os.IsNotExist(readErr):
		entry.State = Created
	default:
		return fmt.Errorf("read %s: %w", resolvedPath, readErr)
	}

	index.Entries = append(index.Entries, entry)
	return s.save(turn, index)
}

// RecordPostHash records the SHA-256 hex digest of path on disk right after an
// agent tool write completes so subsequent restores can detect external edits.
func (s *Store) RecordPostHash(turn, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.load(turn)
	if err != nil {
		return err
	}
	resolvedPath, err := s.resolve(path)
	if err != nil {
		return err
	}
	stored, err := s.relativePath(resolvedPath)
	if err != nil {
		return err
	}

	hash, err := fileSHA256(resolvedPath)
	if err != nil {
		return err
	}

	for i := range index.Entries {
		if index.Entries[i].Path == stored {
			index.Entries[i].PostHash = hash
			return s.save(turn, index)
		}
	}
	return nil
}

// LoadIndex reads the snapshot manifest for turn.
func (s *Store) LoadIndex(turn string) (*Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(turn)
}

// Restore puts every file captured in the turn back to its snapshotted state
// and reports the paths it touched, refusing to overwrite externally modified
// files whose current disk hash differs from PostHash.
func (s *Store) Restore(turn, workspace string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.load(turn)
	if err != nil {
		return nil, err
	}
	if len(index.Entries) == 0 {
		return nil, nil
	}

	var restored []string
	var failures []string
	hasConflict := false

	for _, e := range index.Entries {
		res := s.restoreEntry(turn, e, e.PostHash, workspace, false)
		if res.Restored {
			restored = append(restored, res.Path)
			continue
		}
		if res.Conflict {
			hasConflict = true
		}
		if res.Error != "" {
			failures = append(failures, fmt.Sprintf("%s: %s", res.Path, res.Error))
		}
	}

	if len(failures) > 0 {
		if hasConflict {
			return restored, fmt.Errorf("%w: %s", ErrExternalModification, strings.Join(failures, "; "))
		}
		return restored, fmt.Errorf("could not restore: %s", strings.Join(failures, "; "))
	}
	return restored, nil
}

// RestoreFile restores one file from turn, guarding against external modifications
// unless force is true.
func (s *Store) RestoreFile(turn, relPath, workspace string, force bool) (RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.load(turn)
	if err != nil {
		return RestoreResult{Turn: turn, Path: relPath, Error: err.Error()}, err
	}
	normTarget := filepath.ToSlash(filepath.Clean(strings.TrimSpace(relPath)))
	for _, e := range index.Entries {
		if e.Path == normTarget || e.Path == relPath {
			res := s.restoreEntry(turn, e, e.PostHash, workspace, force)
			if res.Conflict {
				return res, fmt.Errorf("%w: %s (expected=%s actual=%s)", ErrExternalModification, res.Path, res.ExpectedHash, res.ActualHash)
			}
			if res.Error != "" {
				return res, errors.New(res.Error)
			}
			return res, nil
		}
	}
	return RestoreResult{Turn: turn, Path: relPath, Error: "file not found in snapshot turn"}, fmt.Errorf("file %q not captured in turn %q", relPath, turn)
}

// RestoreCrew restores every file modified by crewID back to its pre-crew state,
// checking the latest PostHash of each file for external edits unless force is true.
func (s *Store) RestoreCrew(crewID, workspace string, force bool) ([]RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	crewID = strings.TrimSpace(crewID)
	if crewID == "" {
		return nil, fmt.Errorf("crew_id required")
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var matchingTurns []*Index
	for _, dirEntry := range entries {
		if !dirEntry.IsDir() {
			continue
		}
		idx, err := s.load(dirEntry.Name())
		if err != nil || len(idx.Entries) == 0 {
			continue
		}
		matches := idx.CrewID == crewID
		if !matches {
			for _, e := range idx.Entries {
				if e.CrewID == crewID {
					matches = true
					break
				}
			}
		}
		if matches {
			matchingTurns = append(matchingTurns, idx)
		}
	}
	if len(matchingTurns) == 0 {
		return nil, nil
	}

	sort.Slice(matchingTurns, func(i, j int) bool {
		if !matchingTurns[i].Created.Equal(matchingTurns[j].Created) {
			return matchingTurns[i].Created.Before(matchingTurns[j].Created)
		}
		return matchingTurns[i].Turn < matchingTurns[j].Turn
	})

	type filePlan struct {
		earliestTurn  string
		earliestEntry Entry
		latestHash    string
	}
	plans := make(map[string]*filePlan)
	var orderedPaths []string

	for _, idx := range matchingTurns {
		for _, e := range idx.Entries {
			if e.CrewID != "" && e.CrewID != crewID {
				continue
			}
			p, exists := plans[e.Path]
			if !exists {
				plans[e.Path] = &filePlan{
					earliestTurn:  idx.Turn,
					earliestEntry: e,
					latestHash:    e.PostHash,
				}
				orderedPaths = append(orderedPaths, e.Path)
			} else if e.PostHash != "" {
				p.latestHash = e.PostHash
			}
		}
	}

	var results []RestoreResult
	var failures []string
	hasConflict := false

	for _, relPath := range orderedPaths {
		p := plans[relPath]
		res := s.restoreEntry(p.earliestTurn, p.earliestEntry, p.latestHash, workspace, force)
		res.CrewID = crewID
		results = append(results, res)
		if res.Conflict {
			hasConflict = true
		}
		if res.Error != "" {
			failures = append(failures, fmt.Sprintf("%s: %s", res.Path, res.Error))
		}
	}

	if len(failures) > 0 {
		if hasConflict {
			return results, fmt.Errorf("%w: %s", ErrExternalModification, strings.Join(failures, "; "))
		}
		return results, fmt.Errorf("could not restore crew %s: %s", crewID, strings.Join(failures, "; "))
	}
	return results, nil
}

func (s *Store) restoreEntry(turn string, e Entry, expectedPostHash, workspace string, force bool) RestoreResult {
	res := RestoreResult{
		Turn:         turn,
		Path:         e.Path,
		CrewID:       e.CrewID,
		ExpectedHash: expectedPostHash,
	}
	target, err := s.safeTargetPath(workspace, e.Path)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	if !force && expectedPostHash != "" {
		actualHash, err := fileSHA256(target)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		res.ActualHash = actualHash
		if actualHash != expectedPostHash {
			res.Conflict = true
			res.Error = fmt.Sprintf("modified externally (expected=%s actual=%s)", shortHash(expectedPostHash), shortHash(actualHash))
			return res
		}
	}

	switch e.State {
	case Created:
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			res.Error = err.Error()
			return res
		}
	case Modified:
		backupPath, err := s.safeBackupPath(e.Backup)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		content, err := os.ReadFile(backupPath)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			res.Error = err.Error()
			return res
		}
		if err := s.rejectSymlinkSegments(workspace, e.Path); err != nil {
			res.Error = err.Error()
			return res
		}
		if err := os.WriteFile(target, content, 0644); err != nil {
			res.Error = err.Error()
			return res
		}
	}
	res.Restored = true
	return res
}

func (s *Store) safeTargetPath(workspace, rawPath string) (string, error) {
	relSlash, _, isOutside, err := mount.ResolveWithinWorkspace(rawPath, workspace)
	if err != nil || isOutside || relSlash == "" || relSlash == "." {
		if err == nil {
			err = fmt.Errorf("snapshot target %q escapes workspace", rawPath)
		}
		return "", err
	}
	if err := s.rejectSymlinkSegments(workspace, relSlash); err != nil {
		return "", err
	}
	return filepath.Join(workspace, filepath.FromSlash(relSlash)), nil
}

func (s *Store) rejectSymlinkSegments(workspace, relSlash string) error {
	cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relSlash)))
	if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, "../") || filepath.IsAbs(cleanRel) {
		return fmt.Errorf("snapshot target %q escapes workspace", relSlash)
	}
	cur := workspace
	for _, seg := range strings.Split(cleanRel, "/") {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in snapshot restore path: %s", cur)
		}
	}
	return nil
}

func (s *Store) safeBackupPath(backupRel string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(backupRel))))
	if clean == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(backupRel) {
		return "", fmt.Errorf("invalid snapshot backup path %q", backupRel)
	}
	relSlash, canonical, isOutside, err := mount.ResolveWithinWorkspace(clean, s.root)
	if err != nil || isOutside || relSlash == "" || relSlash == "." {
		if err == nil {
			err = fmt.Errorf("snapshot backup %q escapes snapshot store", backupRel)
		}
		return "", err
	}
	if err := s.rejectSymlinkSegments(s.root, relSlash); err != nil {
		return "", err
	}
	return canonical, nil
}

// LastTurn returns the most recently created turn identifier, or "" when no
// snapshot exists yet.
func (s *Store) LastTurn() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	var newest string
	var newestTime time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		index, err := s.load(entry.Name())
		if err != nil || len(index.Entries) == 0 {
			continue
		}
		if newest == "" || index.Created.After(newestTime) {
			newest = index.Turn
			newestTime = index.Created
		}
	}
	return newest, nil
}

// load reads a turn index, returning an empty one when it does not exist yet.
func (s *Store) load(turn string) (*Index, error) {
	path := filepath.Join(s.dir(turn), "index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Index{Turn: turn, Created: s.now()}, nil
		}
		return nil, err
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("parse snapshot index: %w", err)
	}
	return &index, nil
}

// save writes the turn index atomically enough for our purposes.
func (s *Store) save(turn string, index *Index) error {
	dir := s.dir(turn)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "index.json"), data, 0644)
}

func (s *Store) resolve(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(filepath.Dir(filepath.Dir(s.root)), path), nil
}

func (s *Store) relativePath(path string) (string, error) {
	workspace := filepath.Dir(filepath.Dir(s.root))
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(workspace, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(abs), nil
	}
	return filepath.ToSlash(rel), nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing", nil
		}
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// sanitize makes a value safe to use as a single path element.
func sanitize(v string) string {
	v = strings.ReplaceAll(v, string(filepath.Separator), "_")
	v = strings.ReplaceAll(v, "/", "_")
	v = strings.ReplaceAll(v, "..", "_")
	if v == "" {
		return "default"
	}
	return v
}
