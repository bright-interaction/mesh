// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"path/filepath"
	"strings"
)

// This file holds THE path guard for the sync protocol. Every place that turns a
// path off the wire into a filesystem write must call SafeSyncPath, on both sides
// of the trust boundary:
//
//   - pkg/meshclient/safepath.go   (client: hub-supplied deltas, conflicts, tombstones)
//   - internal/hub/repo.go         (hub: client-supplied outbox items and reads)
//   - internal/curator/merge_note.go (curator daemon: hub-supplied job paths)
//   - cmd/mesh/curator.go          (CLI: hub-supplied job paths)
//
// It lives here, in the package that owns what a vault IS, because four private
// copies of the same rule is how this guard drifted: each copy blocked a slightly
// different set, so a path refused by one was landed by another. The rule is
// derived from Walk's own filter (a syncable file is a file Walk could return),
// so the guard cannot drift away from the indexer either.
//
// The threat model is a hostile, compromised or MITM'd hub on one side, and any
// teammate with write access on the other (the default team posture grants write
// to every member). Both are on the far side of a network boundary, so neither
// path may be joined onto a directory unchecked.

// syncRootFiles are the only non-note files the sync protocol legitimately
// carries, both at the vault ROOT and nowhere else. The hub creates both of them
// itself at bootstrap and ships them to every client through the delta stream:
//
//	mesh.toml       the hub-authoritative team config (SPEC 8, embedding homogeneity)
//	.gitattributes  the line-ending policy for the hub's own git repo (SPEC 6.5)
//
// Everything else must be a markdown note. That is the whole point of the check:
// a delta at .claude/settings.json, .envrc or .vscode/tasks.json is code execution
// on a teammate's machine, and Walk never returns such a file, so once created it
// would be invisible to the indexer and to the outbox forever.
var syncRootFiles = map[string]bool{
	"mesh.toml":      true,
	".gitattributes": true,
}

// SafeSyncPath cleans a path that arrived over the sync protocol and reports
// whether it is safe to materialize inside a vault. It returns the cleaned,
// OS-separator form to use; callers must use THAT, never the raw input.
//
// It rejects, in order:
//
//   - the empty path, and any path containing a backslash (a Windows separator
//     that a unix peer would turn into a literal filename)
//   - absolute paths, drive-relative paths, Windows device names and every ".."
//     escape, via filepath.IsLocal, plus "." (the vault dir itself)
//   - any directory component the vault walker skips: a dot-dir (.mesh holds the
//     hub credentials and the sync state, .git the repo, .claude the agent's own
//     instructions), node_modules, _archive. A note under one of those can never
//     be indexed or pushed back, so a delta that creates one is either an attack
//     or a permanent one-way write
//   - any file that is not markdown, except the two hub-owned root files above
//
// Component matching is case-insensitive and ignores trailing dots and spaces,
// because the FILESYSTEM does. ".MESH/config.toml" lands on <vault>/.mesh/config.toml
// on APFS and on Windows, and ".mesh./config.toml" lands there on Windows, so a
// byte-exact comparison of the component guards nothing on the machines this
// software actually runs on.
func SafeSyncPath(p string) (string, bool) {
	if strings.TrimSpace(p) == "" {
		return "", false
	}
	if strings.ContainsRune(p, '\\') {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	// IsLocal is the single check for absolute, "..", drive-relative and (on
	// Windows) reserved device names. It accepts "." (the vault dir itself), which
	// is not a note path and is what "notes/.." cleans down to, so reject it here.
	if clean == "." || !filepath.IsLocal(clean) {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for _, dir := range parts[:len(parts)-1] {
		if !syncableDir(dir) {
			return "", false
		}
	}
	if !syncableFile(parts[len(parts)-1], len(parts) == 1) {
		return "", false
	}
	return clean, true
}

// SafeSyncNotePath is SafeSyncPath for the PUSH direction: a note and nothing else.
// The two root files SafeSyncPath allows are hub-owned, and the hub creates them
// itself at bootstrap. A client never produces one (it walks *.md), so accepting one
// from a client would only ever mean a member repointing the team's canonical
// mesh.toml, which pins the one embedding model and width the whole vault shares.
func SafeSyncNotePath(p string) (string, bool) {
	clean, ok := SafeSyncPath(p)
	if !ok {
		return "", false
	}
	if !strings.EqualFold(filepath.Ext(clean), ".md") {
		return "", false
	}
	return clean, true
}

// syncableDir reports whether a directory component may appear in a sync path. It
// enumerates from skipDirs, the walker's own prune list, so a directory the
// indexer learns to skip is refused here in the same change instead of becoming a
// new invisible-write hole.
func syncableDir(name string) bool {
	n := foldComponent(name)
	if n == "" || strings.HasPrefix(n, ".") {
		return false
	}
	return !skipDirs[n]
}

// syncableFile reports whether a file component may appear in a sync path:
// markdown anywhere, or one of the two hub-owned files at the vault root.
func syncableFile(name string, atRoot bool) bool {
	if atRoot && syncRootFiles[name] {
		return true
	}
	return strings.EqualFold(filepath.Ext(name), ".md")
}

// foldComponent reduces a path component to the name the filesystem will actually
// resolve it to: lower case (APFS, NTFS and HFS+ are case-insensitive by default)
// with trailing dots and spaces removed (Windows strips them silently, so
// ".mesh. " opens .mesh). Used for the reserved-name comparison only; the path we
// return is always the caller's own bytes.
func foldComponent(name string) string {
	return strings.ToLower(strings.TrimRight(name, ". "))
}
