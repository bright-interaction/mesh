package vault

import (
	"io/fs"
	"path/filepath"
	"strings"
)

var skipDirs = map[string]bool{
	".git": true, ".mesh": true, "node_modules": true,
	"graphify-out": true, "_archive": true,
}

// skipDir reports whether a directory (by name, relative to root) should be
// pruned from a vault traversal. Shared by Walk and Dirs so the watcher watches
// exactly the directories the indexer reads.
func skipDir(path, root, name string) bool {
	if path != root && strings.HasPrefix(name, ".") {
		return true
	}
	return skipDirs[name]
}

// Walk returns every markdown file under root, skipping noise and hidden dirs.
// This is the M0 walker; .meshignore support lands with the index step.
func Walk(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(path, root, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") && !IsConflictSibling(d.Name()) {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// IsConflictSibling reports whether a filename is a sync-conflict artifact
// (e.g. note.sync-conflict-20260616-bob-1a2b3c4d.md). These are a copy of a note
// the team-sync hub parked for a human to resolve; they duplicate the original's
// frontmatter id, so the indexer, watcher, and linter must skip them rather than
// trip on a duplicate id or surface a half-merged version in retrieval.
func IsConflictSibling(name string) bool {
	return strings.Contains(name, ".sync-conflict-")
}

// WalkConflictSiblings returns every sync-conflict sibling under root (the
// inverse of Walk's filter), pruning the same noise/hidden dirs. Used by
// `mesh conflicts` to find the parked loser copies that Walk deliberately hides.
func WalkConflictSiblings(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(path, root, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") && IsConflictSibling(d.Name()) {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// Dirs returns every directory under root that Walk traverses (root included,
// skipped/hidden dirs pruned). The fsnotify watcher needs these because kqueue
// and inotify watch a single directory at a time, not a tree, so it adds one
// watch per indexed directory and skips the same noise Walk does.
func Dirs(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if skipDir(path, root, d.Name()) {
			return fs.SkipDir
		}
		out = append(out, path)
		return nil
	})
	return out, err
}
