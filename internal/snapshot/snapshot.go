// Package snapshot keeps the previous contents of files the agent is about to
// modify, so a bad edit can be reverted without depending on git.
//
// A snapshot is not a version control system. It captures exactly one thing:
// the state of each file immediately before the first write in a turn, which is
// what /undo needs to restore. Files changed by shell commands are outside its
// reach and are reported as such rather than silently ignored.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State describes what a snapshot holds for one file.
type State string

const (
	// Modified means the file existed and its contents were saved.
	Modified State = "modified"
	// Created means the file did not exist, so undoing means deleting it.
	Created State = "created"
)

// Entry is one file captured in a turn.
type Entry struct {
	// Path is stored relative to the workspace when possible, so a snapshot
	// stays valid if the workspace is moved.
	Path    string `json:"path"`
	State   State  `json:"state"`
	Backup  string `json:"backup,omitempty"`
	Created time.Time `json:"created"`
}

// Index is the manifest of a single turn.
type Index struct {
	Turn    string  `json:"turn"`
	Created time.Time `json:"created"`
	Entries []Entry `json:"entries"`
}

// Store writes snapshots beneath <root>/.tether/snapshots.
type Store struct {
	root string
	now  func() time.Time
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
	index, err := s.load(turn)
	if err != nil {
		return err
	}

	// Resolve relative paths against the workspace, not the process CWD. A
	// relative path means "this file in the project", and the process may have
	// been started from an unrelated directory.
	path, err = s.resolve(path)
	if err != nil {
		return err
	}

	stored, err := s.relativePath(path)
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

	content, readErr := os.ReadFile(path)
	entry := Entry{Path: stored, Created: s.now()}

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
		return fmt.Errorf("read %s: %w", path, readErr)
	}

	index.Entries = append(index.Entries, entry)
	return s.save(turn, index)
}

// Restore puts every file captured in the turn back to its snapshotted state
// and reports the paths it touched. Entries that could not be restored are
// returned alongside the error so the caller can report partial success.
func (s *Store) Restore(turn, workspace string) ([]string, error) {
	index, err := s.load(turn)
	if err != nil {
		return nil, err
	}
	if len(index.Entries) == 0 {
		return nil, nil
	}

	var restored []string
	var failures []string
	for _, e := range index.Entries {
		target := filepath.Join(workspace, filepath.FromSlash(e.Path))

		switch e.State {
		case Created:
			// The file did not exist before; undoing means removing it.
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				failures = append(failures, fmt.Sprintf("%s: %v", e.Path, err))
				continue
			}
		case Modified:
			content, err := os.ReadFile(filepath.Join(s.root, e.Backup))
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", e.Path, err))
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", e.Path, err))
				continue
			}
			if err := os.WriteFile(target, content, 0644); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", e.Path, err))
				continue
			}
		}
		restored = append(restored, e.Path)
	}

	if len(failures) > 0 {
		return restored, fmt.Errorf("could not restore: %s", strings.Join(failures, "; "))
	}
	return restored, nil
}

// LastTurn returns the most recently created turn identifier, or "" when no
// snapshot exists yet.
func (s *Store) LastTurn() (string, error) {
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

// resolve turns a path into an absolute one, interpreting relative paths as
// being inside the workspace rather than relative to the process working
// directory. Without this, "main.go" captured from a different CWD would point
// at the wrong file and undo would restore the wrong contents.
func (s *Store) resolve(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	// Relative paths mean "this file in the project", so anchor them at the
	// workspace. Anchoring at the CWD would make capture and restore disagree
	// whenever the process was started from a subdirectory.
	return filepath.Join(filepath.Dir(filepath.Dir(s.root)), path), nil
}

// relativePath expresses path relative to the snapshot root's workspace when
// possible so the index stays portable.
func (s *Store) relativePath(path string) (string, error) {
	workspace := filepath.Dir(filepath.Dir(s.root)) // <ws>/.tether/snapshots
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(workspace, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		// Outside the workspace: keep the absolute path.
		return filepath.ToSlash(abs), nil
	}
	return filepath.ToSlash(rel), nil
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
