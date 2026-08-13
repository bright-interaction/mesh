// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// idScanBytes bounds how much of a file NoteIDForFile reads looking for the closing
// frontmatter marker. Frontmatter sits at the very top of a note and Mesh's own writer
// renders well under a kilobyte of it, so this reads the whole block for every real note
// while a large vault costs a bounded head read per file instead of a full slurp.
const idScanBytes = 64 << 10

// NoteIDForFile returns the note id a markdown file claims: its frontmatter `id:` when it
// declares one, otherwise the lowercased basename without the extension. This MUST agree
// with index.effectiveID, which is what the indexer keys notes.id and the graph's
// "note:"+id node on; internal/index/dupid_consistency_test.go enumerates a vault and
// asserts the two answer the same thing for every file in it.
//
// A file whose frontmatter will not parse, or whose block is not closed within the head
// read, falls back to the basename. That is deliberately conservative for the caller that
// matters (CreateNote, claiming a free id): the fallback can only make an id look TAKEN
// that might have been free, never free that is taken.
func NoteIDForFile(path string) string {
	key := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	f, err := os.Open(path)
	if err != nil {
		return key
	}
	defer f.Close()
	head := make([]byte, idScanBytes)
	n, err := io.ReadFull(f, head)
	if n == 0 && err != nil {
		return key
	}
	fmText, _, had := SplitFrontmatter(string(head[:n]))
	if !had {
		return key
	}
	// Only the id is decoded. A note with, say, an unquoted colon in `updated:` fails to
	// unmarshal as a whole, and asking for the full Frontmatter would throw away the id
	// of every note that has any other schema problem.
	var probe struct {
		ID string `yaml:"id"`
	}
	if err := yaml.Unmarshal([]byte(fmText), &probe); err != nil {
		return key
	}
	if id := strings.TrimSpace(probe.ID); id != "" {
		return id
	}
	return key
}

// otherFileNamedForID returns the vault-relative path of a file <id>.md that sits
// somewhere in the vault OTHER than ownPath, or "" when ownPath is the only one. It is
// the post-create check that closes the window ClaimedIDs alone cannot: a second
// CreateNote in a DIFFERENT type directory that made its own scan before this one created
// its file. Every note CreateNote writes is named <id>.md, so a stat per vault directory
// finds any such racer, and a handful of stats keeps the check cheap enough to run on the
// authoring hot path.
func otherFileNamedForID(root, id, ownPath string) string {
	dirs, err := Dirs(root)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		p := filepath.Join(d, id+".md")
		if p == ownPath {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			if rel, rerr := filepath.Rel(root, p); rerr == nil {
				return rel
			}
			return p
		}
	}
	return ""
}

// ClaimedIDs maps every note id currently claimed anywhere in the vault to the first
// vault-relative path (in walk order) that claims it.
//
// It exists because a note id is vault-GLOBAL while a note file lives in one type
// directory. CreateNote used to prove an id free by creating decisions/<id>.md with
// O_EXCL, which says nothing about gotchas/<id>.md, so a gotcha and a decision with the
// same title produced two files with one id and a success receipt for both. Which of the
// two you could then retrieve flipped depending on whether the watcher or `mesh index`
// ran last, and one of them was invisible either way.
// An unreadable file or directory is SKIPPED rather than failing the scan. Walk returns
// the first such error, and using it here made an unreadable folder anywhere in the vault
// fail every note write, which is a strictly worse outcome than the id scan being
// incomplete: the write is the thing the flywheel cannot afford to lose, the note file
// itself is written atomically either way, and a duplicate that slips through an
// unreadable corner is still caught and quarantined by the indexer.
func ClaimedIDs(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable directory or vanished entry: skip it, keep scanning.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if skipDir(path, root, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") || IsConflictSibling(d.Name()) {
			return nil
		}
		id := NoteIDForFile(path)
		if id == "" {
			return nil
		}
		rel := path
		if r, rerr := filepath.Rel(root, path); rerr == nil {
			rel = r
		}
		if _, taken := out[id]; !taken {
			out[id] = rel
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	return out, nil
}
