// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"fmt"
	"path/filepath"

	"github.com/bright-interaction/mesh/internal/vault"
)

// Every path in a sync response is HUB-CONTROLLED input, not local state: a
// compromised, hostile or MITM'd hub can put anything in Delta.Path,
// Conflict.Path, Conflict.SiblingPath or a tombstone entry. Joining those onto
// the vault dir unchecked is arbitrary file create/overwrite on the laptop
// (~/.zshrc, ~/.ssh/authorized_keys, a launchd plist), so the client validates
// every one of them before ANY Join/Read/Write/Remove. This mirrors the hub's own
// safeRelPath (internal/hub/repo.go) on the client side of the trust boundary.

// safeRelPath cleans a hub-supplied vault-relative path and reports whether it is
// safe to materialize under the vault.
//
// The rule itself lives in vault.SafeSyncPath, which the hub, the curator daemon
// and the `mesh curator` CLI call too. It used to be a private copy here, and the
// copies disagreed: this one compared the reserved ".mesh" component byte for
// byte, so ".MESH/config.toml" walked straight past it and landed on the real
// <vault>/.mesh/config.toml on APFS and on Windows, where a planted
// Retrieval.RerankEndpoint ships every query and the full text of the matching
// notes to an attacker's URL on every search. Neither copy constrained the
// extension either, so a delta at .claude/settings.json installed a hook command
// on a teammate's machine.
func safeRelPath(p string) (string, bool) {
	return vault.SafeSyncPath(p)
}

// safeNotePath resolves a hub-supplied NOTE path to an absolute path inside
// vaultDir. It additionally refuses the ".sync-conflict-" marker: vault.Walk
// hides those files, so a note landed at such a path would be invisible to the
// indexer and permanently unsyncable (and it would collide with a real conflict
// sibling).
func safeNotePath(vaultDir, p string) (string, error) {
	if vault.IsConflictSibling(p) {
		return "", fmt.Errorf("path %q uses the reserved sync-conflict marker", p)
	}
	return resolveInVault(vaultDir, p)
}

// safeSiblingPath resolves a hub-supplied CONFLICT SIBLING path. The inverse of
// safeNotePath: the marker is required, so a hub cannot use the sibling field to
// overwrite an arbitrary note (siblings are local resolution artifacts).
func safeSiblingPath(vaultDir, p string) (string, error) {
	if !vault.IsConflictSibling(filepath.Base(p)) {
		return "", fmt.Errorf("path %q is not a sync-conflict sibling", p)
	}
	return resolveInVault(vaultDir, p)
}

// resolveInVault joins a validated relative path onto the vault and proves the
// result still lands inside it once symlinks are resolved. The lexical check
// alone is not enough: a symlinked directory anywhere on the path (planted by an
// earlier delta, or by anything else on the machine) redirects the write outside
// the vault even though the path never contains "..". We resolve the deepest
// EXISTING ancestor, re-attach the not-yet-created remainder, and require the
// result to stay under the resolved vault root.
func resolveInVault(vaultDir, p string) (string, error) {
	clean, ok := safeRelPath(p)
	if !ok {
		return "", fmt.Errorf("unsafe hub-supplied path %q", p)
	}
	root, err := filepath.Abs(vaultDir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	abs := filepath.Join(root, clean)
	rest := ""
	for cur := abs; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			rel, rerr := filepath.Rel(root, filepath.Join(resolved, rest))
			if rerr != nil || !filepath.IsLocal(rel) {
				return "", fmt.Errorf("hub-supplied path %q escapes the vault via a symlink", p)
			}
			return abs, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur || len(parent) < len(root) {
			// Walked above the vault root without finding anything that exists:
			// the root itself is gone, so there is nothing safe to write into.
			return "", fmt.Errorf("vault %s does not exist", vaultDir)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
