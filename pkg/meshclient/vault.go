// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/vault"
)

// credentials and sync state live under <vault>/.mesh, which is git-ignored and
// never itself synced. credentials is mode 0600 (it holds the bearer token).
type credentials struct {
	HubURL string `json:"hub_url"`
	Token  string `json:"token"`
}

type syncState struct {
	HeadSHA string            `json:"head_sha"`
	Hashes  map[string]string `json:"hashes"`             // vault-relative path -> content sha
	TombSeq int64             `json:"tomb_seq,omitempty"` // last delete high-water mark seen from the hub
}

func credPath(vaultDir string) string  { return filepath.Join(vaultDir, ".mesh", "credentials") }
func statePath(vaultDir string) string { return filepath.Join(vaultDir, ".mesh", "sync.json") }

func writeCredentials(vaultDir string, c credentials) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, ".mesh"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(credPath(vaultDir), b, 0o600)
}

func readCredentials(vaultDir string) (credentials, error) {
	var c credentials
	b, err := os.ReadFile(credPath(vaultDir))
	if err != nil {
		return c, fmt.Errorf("not joined to a hub (no .mesh/credentials); run: mesh join <hub-url> <invite>")
	}
	return c, json.Unmarshal(b, &c)
}

func writeState(vaultDir string, s syncState) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, ".mesh"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	// Atomic, like every other write in this package. This was the one truncate-then-write
	// left, 30 lines above the writeFileAtomic that exists precisely because a half-written
	// file is a hazard, and mirrored on the hub side. A crash mid-write left a truncated
	// state file whose unmarshal error readState then swallowed, silently resetting the
	// base to empty: the drop gates become no-ops and the next sync is a noisy full
	// re-push with mass conflict siblings. Recoverable, but avoidable for one line.
	return writeFileAtomic(statePath(vaultDir), b)
}

func readState(vaultDir string) syncState {
	s := syncState{Hashes: map[string]string{}}
	if b, err := os.ReadFile(statePath(vaultDir)); err == nil {
		// Log rather than discard. A corrupt state file silently resets the sync base,
		// and the resulting full re-push looks like a mystery rather than a consequence.
		if uerr := json.Unmarshal(b, &s); uerr != nil {
			slog.Warn("sync: sync.json is unreadable; treating the vault as never synced",
				"path", statePath(vaultDir), "err", uerr)
			s = syncState{Hashes: map[string]string{}}
		}
	}
	if s.Hashes == nil {
		s.Hashes = map[string]string{}
	}
	return s
}

// Summary reports what a sync round did, for the CLI.
type Summary struct {
	Pushed           int
	Pulled           int
	Conflicts        int
	Head             string
	ConflictSiblings []string // merge conflicts: our pushed version parked here
	Protected        []string // external-editor race: incoming hub version parked here
	Dropped          []string // full-reconcile: locals removed because deleted upstream
	Rejected         []string // hub refused these (viewer/ACL/scope/oversize); kept dirty to retry
	Remaining        int      // dirty notes deferred to the next round because the batch was bounded
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// computeOutbox diffs the vault's markdown files on disk against the last-synced
// hashes, returning the changes to push plus the current on-disk hash map.
func computeOutbox(vaultDir string, prev map[string]string) ([]syncproto.OutboxItem, map[string]string, error) {
	files, err := vault.Walk(vaultDir)
	if err != nil {
		return nil, nil, err
	}
	current := map[string]string{}
	var outbox []syncproto.OutboxItem
	for _, f := range files {
		rel, err := filepath.Rel(vaultDir, f)
		if err != nil {
			rel = f
		}
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		h := contentHash(b)
		current[rel] = h
		if prev[rel] != h {
			outbox = append(outbox, syncproto.OutboxItem{Path: rel, Op: "upsert", ContentB64: base64.StdEncoding.EncodeToString(b)})
		}
	}
	for rel := range prev {
		if _, ok := current[rel]; !ok {
			outbox = append(outbox, syncproto.OutboxItem{Path: rel, Op: "delete"})
		}
	}
	return outbox, current, nil
}

// park records a path whose incoming hub version was set aside (sibling) because
// the local file changed during the sync window.
type park struct {
	note    string // the original path whose local change we kept
	sibling string // where the incoming hub version was parked
}

// applyDeltas writes or removes files per the hub's response, guarding against
// the external-editor race (SPEC 6.6): sentHashes is the on-disk state captured
// when the outbox was computed, so if a path changed on disk SINCE then a local
// edit OR delete landed during the sync window. In that case the incoming hub
// version is parked in a sibling and the local change is kept; SyncVault then
// keeps the path "dirty" so the local change re-pushes next sync (it is not
// silently dropped). Each write is atomic (temp + rename); a partial-batch
// failure self-heals because the base is not advanced. Returns the parked paths.
//
// Every delta path goes through safeNotePath first: the path is hub-controlled,
// so an unvalidated Join is arbitrary file write outside the vault. An unsafe
// path is skipped and logged, never fatal, so one bad entry cannot stop the rest
// of a legitimate sync.
func applyDeltas(vaultDir string, deltas []syncproto.Delta, sentHashes map[string]string, rejected map[string]bool) ([]park, error) {
	var parked []park
	for _, d := range deltas {
		abs, perr := safeNotePath(vaultDir, d.Path)
		if perr != nil {
			slog.Warn("sync: refusing hub delta path", "path", d.Path, "err", perr)
			//safe-skip: this is INBOUND hub content we are refusing to write, not a
			// local note. Nothing of the user's is dropped; the local file is untouched.
			continue
		}
		onDisk, readErr := os.ReadFile(abs)
		sentHash, wasSent := sentHashes[d.Path]
		// A local change during the window is either an edit (present but
		// different) or a delete (absent now, but present at send time).
		locallyChanged := (readErr == nil && contentHash(onDisk) != sentHash) ||
			(readErr != nil && wasSent)
		// A REJECTED path is locally changed by definition, whatever the hashes say.
		//
		// syncproto states the contract plainly: "The client keeps its local copy; the
		// edit simply did not land upstream." Without this, it did the opposite. The
		// comparison above is against sentHashes, i.e. the disk state at OUTBOX time, so
		// an edit made BEFORE the sync (which is the normal order: you edit, then you
		// sync) hashes equal and reads as unchanged. If the same round also carried a
		// delta for that path, the hub version was written straight over the user's
		// bytes, with no conflict, no sibling and no curation job, while Summary.Rejected
		// told them it had been kept and to retry.
		//
		// Treating it as changed routes it into the park-and-keep branch below, which is
		// what Protected and keepParkedDirty already exist for: the local bytes stay, the
		// hub version lands in a sibling, and the path re-pushes next round.
		// Only when there ARE local bytes to protect. If the file is absent there is
		// nothing at risk, and forcing the flag would block a legitimate delta from
		// landing at all.
		if rejected[d.Path] && readErr == nil {
			locallyChanged = true
		}

		if d.Op == "delete" {
			if locallyChanged {
				continue // local edit/recreate after send: keep it, skip the delete
			}
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return parked, err
			}
			//safe-skip: the delete was applied above. Nothing is being dropped here.
			continue
		}

		b, err := base64.StdEncoding.DecodeString(d.ContentB64)
		if err != nil {
			return parked, err
		}
		if locallyChanged && contentHash(onDisk) != contentHash(b) {
			// External-editor race: park the incoming version, keep the local
			// change (a local delete keeps the path absent; a local edit keeps it).
			// The sibling is derived from the already-validated delta path, so a
			// validation failure here is a local invariant break (e.g. a symlink
			// planted mid-sync), not hub input: fail the round rather than skip it,
			// which leaves the base unadvanced and self-heals on retry.
			sib := merge.SiblingPath(d.Path, time.Now(), "hub", b)
			sibAbs, perr := safeSiblingPath(vaultDir, sib)
			if perr != nil {
				return parked, perr
			}
			if err := writeFileAtomic(sibAbs, b); err != nil {
				return parked, err
			}
			parked = append(parked, park{note: d.Path, sibling: sib})
			continue
		}
		if err := writeFileAtomic(abs, b); err != nil {
			return parked, err
		}
	}
	return parked, nil
}

// keepParkedDirty rewrites current so each guard-parked path keeps its pre-sync
// base hash instead of its (just-applied) disk hash, so the next computeOutbox
// detects the kept local change and re-pushes it (SPEC 6.6: enqueue the local
// change). Without this the local change would be recorded as synced and lost.
func keepParkedDirty(current, base map[string]string, parked []park) {
	for _, p := range parked {
		if old, ok := base[p.note]; ok {
			current[p.note] = old
		} else {
			delete(current, p.note)
		}
	}
}

// keepDeferredDirty rewrites current so each path the bounded push left OUT of this
// batch keeps its pre-sync base hash (or no entry at all if it had none), exactly like
// keepParkedDirty does for a guard-parked path. The new base must describe what was
// actually SENT, not what is on disk: a deferred path recomputed from disk reads as
// synced, so the next computeOutbox emits nothing for it and the note is never pushed.
// A deferred delete keeps its base hash while the file is absent, which is what makes
// the next round re-emit the delete rather than forget it.
func keepDeferredDirty(current, base map[string]string, deferred []string) {
	for _, rel := range deferred {
		if old, ok := base[rel]; ok {
			current[rel] = old
		} else {
			delete(current, rel)
		}
	}
}

// keepWindowEditsDirty stops a local write made DURING the sync window from being
// silently recorded as already synced. current is recomputed from disk after the
// round trip, so any file the user (or the indexer, or another agent) touched
// while the request was in flight would be baselined as if the hub had it: the
// edit is never pushed, never retried, and a later inbound delta for that path
// overwrites it with no conflict sibling, because sentHashes then matches disk.
//
// sentHashes is the on-disk state at send time, so a path whose hash differs from
// it changed during the window. Two cases are NOT window edits and are skipped:
// a path the hub sent a delta for (applyDeltas wrote it, and its own guard plus
// keepParkedDirty already decided what happens), and a path we dropped because it
// was deleted upstream. Everything else is reset to its send-time hash, which is
// what the hub has, so the next computeOutbox re-detects the change: an edit
// re-pushes as an upsert and a window delete re-pushes as a delete.
func keepWindowEditsDirty(current, sentHashes map[string]string, deltas []syncproto.Delta, dropped []string) {
	skip := make(map[string]bool, len(deltas)+len(dropped))
	for _, d := range deltas {
		skip[d.Path] = true
	}
	for _, rel := range dropped {
		skip[rel] = true
	}
	restore := func(rel string) {
		if skip[rel] {
			return
		}
		if old, ok := sentHashes[rel]; ok {
			current[rel] = old
		} else {
			delete(current, rel) // created during the window: no base, so it pushes as new
		}
	}
	for rel, h := range current {
		if sentHashes[rel] != h {
			restore(rel)
		}
	}
	for rel := range sentHashes {
		if _, ok := current[rel]; !ok {
			restore(rel) // deleted during the window: keep the base so the delete pushes
		}
	}
}

// dropFullReconcileOrphans removes local notes that a full reconcile proves are
// gone from the team vault. A full reconcile's deltas carry the live snapshot as
// upserts with NO deletes (the client's base was empty or too old to diff), so
// without this a stale client keeps, and can later resurrect, every note deleted
// while it was away (the offline-past-horizon resurrection bug).
//
// The deletion is content-safe: it only ever removes a file that is byte-identical
// to the EXACT version we last synced (base[rel]), which is the version the hub
// then deleted. The snapshot tells us the path is no longer live; matching base
// tells us the on-disk bytes are that dead version and not something the user has
// since edited or recreated at the same path. So a local recreate with any
// different content, including content the hub silently rejected as non-text, is
// never destroyed: its hash differs from base and it is kept (and re-pushes). The
// hub tombstone list is a confirming signal only; the base-hash match is the
// load-bearing safety, which also means tombstone GC can never cause data loss.
//
// Known limitation (safe direction): a client whose sync state was reset so base
// no longer knows a path cannot prove the on-disk bytes are the dead version, so
// it keeps the file rather than risk destroying local content. Such a note can
// re-share on the next push (a re-share, never a loss). The horizon GC assumes
// base is intact for offline clients.
func dropFullReconcileOrphans(vaultDir string, deltas []syncproto.Delta, tombstones []string, base map[string]string) ([]string, error) {
	keep := make(map[string]bool, len(deltas))
	for _, d := range deltas {
		if d.Op == "upsert" {
			keep[d.Path] = true
		}
	}
	tomb := make(map[string]bool, len(tombstones))
	for _, p := range tombstones {
		tomb[p] = true
	}
	files, err := vault.Walk(vaultDir) // excludes .mesh + conflict siblings already
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, f := range files {
		rel, err := filepath.Rel(vaultDir, f)
		if err != nil {
			rel = f
		}
		rel = filepath.ToSlash(rel)
		if keep[rel] {
			continue // present in the authoritative snapshot: alive
		}
		baseHash, syncedBefore := base[rel]
		// A TOMBSTONE IS REQUIRED, not merely "absent from the snapshot".
		//
		// Absence plus a matching base hash proves "these bytes are the version the hub
		// last had". It does NOT prove the hub deleted the note, because a hub whose
		// history rolled BACKWARDS produces byte-identical evidence. Restore the hub
		// volume from a backup (the most common DR action there is) and every client
		// would delete everything created since that backup: no tombstone exists, because
		// nobody deleted anything. The paths were then dropped from the outbox too, so
		// the clients could not even re-push to heal the hub.
		//
		// Requiring the tombstone inverts that outcome: on a rollback the clients keep
		// their notes and push them back. The cost is that a note whose tombstone has
		// been GC'd can linger locally, which this design already accepts, because data
		// loss outranks a rare resurrection.
		//
		// Safe against the full path specifically: on a full reconcile the hub sends
		// AllTombstones() (internal/hub/sync.go), not a since-seq slice, so a genuine
		// delete always carries its tombstone here.
		if !tomb[rel] {
			//safe-skip: KEEP decision. Skipping here preserves the local file.
			continue
		}
		// SAFETY GATE: only remove bytes identical to the exact version we last
		// synced. A path with no base hash (state reset) or any local change since
		// fails here and is kept, so we never destroy unacknowledged local content.
		if !syncedBefore {
			//safe-skip: KEEP decision. Skipping here preserves the local file.
			continue
		}
		onDisk, readErr := os.ReadFile(f)
		if readErr != nil {
			//safe-skip: KEEP decision. Unreadable means we cannot prove the bytes match
			// the synced version, so the file stays. Failing closed preserves data.
			continue
		}
		if contentHash(onDisk) != baseHash {
			continue // locally edited or recreated since last sync: keep it
		}
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return dropped, err
		}
		dropped = append(dropped, rel)
	}
	return dropped, nil
}

// dropTombstoned removes local notes named in an incremental tombstone drop-list,
// content-safely: only a file byte-identical to the EXACT version we last synced
// (base[rel]) is removed. A path never synced locally, locally edited, or recreated
// since fails the gate and is kept, so an unacknowledged local change is never lost.
// This is the incremental sibling of dropFullReconcileOrphans' safety gate.
func dropTombstoned(vaultDir string, tombstones []string, base map[string]string) ([]string, error) {
	var dropped []string
	for _, rel := range tombstones {
		baseHash, ok := base[rel]
		if !ok {
			continue // never synced here: nothing to prune
		}
		// The drop-list is hub-controlled, so validate before touching the disk.
		abs, perr := safeNotePath(vaultDir, rel)
		if perr != nil {
			slog.Warn("sync: refusing hub tombstone path", "path", rel, "err", perr)
			//safe-skip: KEEP decision. We refuse a hub-supplied delete path we cannot
			// validate, so the local file survives.
			continue
		}
		onDisk, err := os.ReadFile(abs)
		if err != nil {
			continue // already gone or unreadable
		}
		if contentHash(onDisk) != baseHash {
			continue // locally edited or recreated since last sync: keep it
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return dropped, err
		}
		dropped = append(dropped, rel)
	}
	return dropped, nil
}

// writeConflictSiblings preserves the client's losing version of each conflicted
// path in a local sibling BEFORE applyDeltas overwrites the path with the hub's
// winning version. Siblings are local resolution artifacts, never pushed.
//
// Both fields are hub-controlled. Path must be a safe note path and SiblingPath
// must be a safe path that actually carries the sync-conflict marker, so the hub
// cannot use the sibling field to write a local copy over an arbitrary note (or
// over a file outside the vault entirely).
// Returns the paths whose local version could NOT be parked. The caller must protect
// those from applyDeltas, or the hub's winning version silently overwrites the user's
// losing version with no copy anywhere.
func writeConflictSiblings(vaultDir string, conflicts []syncproto.Conflict) ([]string, error) {
	var unparked []string
	for _, cf := range conflicts {
		noteAbs, perr := safeNotePath(vaultDir, cf.Path)
		if perr != nil {
			// Refuse the hostile path, but do NOT fail the round: a malicious or buggy
			// hub could otherwise wedge sync entirely with one bad path. Report it so
			// the caller protects the local bytes from being overwritten instead.
			slog.Warn("sync: refusing hub conflict path", "path", cf.Path, "err", perr)
			unparked = append(unparked, cf.Path)
			continue
		}
		sibAbs, perr := safeSiblingPath(vaultDir, cf.SiblingPath)
		if perr != nil {
			// Same reasoning: no sibling to park it in, so protect the local bytes
			// rather than losing them or wedging the client.
			slog.Warn("sync: refusing hub conflict sibling path", "path", cf.SiblingPath, "err", perr)
			unparked = append(unparked, cf.Path)
			continue
		}
		local, err := os.ReadFile(noteAbs)
		if err != nil {
			//safe-skip: nothing local to preserve (e.g. we deleted it), so there are no
			// bytes at risk.
			continue
		}
		if err := writeFileAtomic(sibAbs, local); err != nil {
			return unparked, err
		}
	}
	return unparked, nil
}

// writeFileAtomic writes b to path via a temp file in the same directory then
// rename, so a reader never sees a partially written note, and fsyncs both the
// data and the parent directory so the note survives a power cut.
//
// Temp+rename alone buys crash-atomicity for a concurrent READER, not durability:
// the rename can reach the disk while the data blocks it points at have not, and
// the note write and the sync.json write can land in either order. If sync.json
// survives and the note's bytes do not, the note reverts locally while state.Hashes
// records the new hash, and the next round pushes the OLD bytes as an upsert that
// the hub fast-forwards, reverting the team's copy with no conflict and no sibling.
//
// There are FOUR copies of this write in the tree, not two. The comment here used to
// name only internal/hub/repo.go, and that undercount is exactly how this estate
// keeps fixing one twin and shipping the other: the durability pass landed on this
// file and repo.go, and the other two kept writing note bytes with no fsync at all.
// The full list, all four fsync-before-rename plus syncDir-after-rename now:
//
//	pkg/meshclient/vault.go     writeFileAtomic  (this one; applies the hub's deltas)
//	internal/hub/repo.go        writeFileAtomic  (lands a note in the hub worktree)
//	cmd/mesh/conflicts.go       writeFileAtomic  (mesh conflicts resolve, take-mine)
//	internal/curator/merge_note.go writeAtomic   (writes the curator's LLM merge)
//
// Keep all four in step. Do not trust this list on its own: it is a comment and a
// comment cannot fail a build. cmd/mesh/atomic_write_durability_test.go DISCOVERS
// every temp-create + rename writer in the tree from the AST and fails on any one
// that does not fsync, so a fifth copy is caught the day it is written.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mesh-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(b)
	// Flush the data to the device BEFORE the rename publishes the name.
	serr := tmp.Sync()
	cerr := tmp.Close()
	if werr != nil {
		os.Remove(tmpName)
		return werr
	}
	if serr != nil {
		os.Remove(tmpName)
		return serr
	}
	if cerr != nil {
		os.Remove(tmpName)
		return cerr
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename that just landed in it survives a power
// cut. Best effort on purpose: opening or fsyncing a directory handle is a no-op or
// an error on some platforms and network filesystems (Windows cannot open one at
// all), and that is not a failed write, so the bytes still count as written and the
// refusal is only logged. The data itself is already fsynced by the caller.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		slog.Debug("sync: cannot open the note directory to fsync it", "dir", dir, "err", err)
		return
	}
	if err := d.Sync(); err != nil {
		slog.Debug("sync: directory fsync not supported here", "dir", dir, "err", err)
	}
	d.Close()
}

// SyncVault runs one reconcile round against the joined hub: push local edits,
// apply the hub's deltas, and persist the new base. It does not reindex; the
// caller (cmd/mesh) runs index.Reconcile afterward.
func SyncVault(vaultDir string) (Summary, error) {
	creds, err := readCredentials(vaultDir)
	if err != nil {
		return Summary{}, err
	}
	state := readState(vaultDir)
	outbox, sentHashes, err := computeOutbox(vaultDir, state.Hashes)
	if err != nil {
		return Summary{}, err
	}
	// Bound the push. The hub caps one request at maxSyncOutboxItems and answers 413 with
	// "split the push into smaller batches", but the client had no batching, so a vault
	// with more dirty notes than the cap 413'd on EVERY round and no state ever advanced:
	// permanently unsyncable, with the remedy addressed to a caller that could not follow
	// it. Reachable by `mesh join` into a directory that already holds a large vault, or a
	// deleted .mesh/sync.json on a vault that has since grown.
	//
	// Truncating rather than looping keeps this change small and each round durable: the
	// batch lands, state advances, and the next round picks up the remainder. A watch
	// daemon converges on its own; a manual `mesh sync` reports what is left.
	//
	// The remainder only comes back if it stays DIRTY. The new base below is recomputed
	// from the whole disk, and the truncated notes are still on disk, so their current
	// hashes would become the base: the next computeOutbox sees prev[rel] == h and emits
	// nothing, and keepWindowEditsDirty cannot rescue them either (sentHashes is the full
	// on-disk map, so current[rel] == sentHashes[rel] and no restore fires). The deferred
	// notes were then never pushed to the team while Summary reported convergence, and
	// since deletes are appended last they truncate FIRST, so a locally deleted note stayed
	// alive on the hub forever. deferred carries those paths to keepDeferredDirty below.
	var deferred []string
	if len(outbox) > maxOutboxPerSync {
		for _, it := range outbox[maxOutboxPerSync:] {
			deferred = append(deferred, it.Path)
		}
		outbox = outbox[:maxOutboxPerSync]
		slog.Info("sync: pushing a bounded batch; the rest follow next round",
			"batch", len(outbox), "remaining", len(deferred))
	}
	resp, err := New(creds.HubURL, creds.Token).Sync(syncproto.SyncRequest{BaseSHA: state.HeadSHA, Outbox: outbox, TombstoneSeq: state.TombSeq})
	if err != nil {
		return Summary{}, err
	}
	// Build the protected set BEFORE applying deltas. Three things go in it, for the same
	// reason: applyDeltas must not overwrite local bytes that nothing else is holding.
	//   - paths the hub REJECTED (the contract says the client keeps its local copy)
	//   - paths the bounded batch DEFERRED (the hub never saw those bytes this round)
	//   - paths whose conflict sibling could not be written (nothing holds the loser)
	rejectedSet := make(map[string]bool, len(resp.Rejected)+len(deferred))
	for _, rel := range resp.Rejected {
		rejectedSet[rel] = true
	}
	// A deferred path sits in exactly the position a rejected one does: the hub did not
	// see our version, so any delta it sends for that path is built on a base that does
	// not contain the local change. Unprotected, applyDeltas compares disk against
	// sentHashes, finds them equal (sentHashes is the on-disk map, dirty files included),
	// concludes nothing changed locally, and writes the hub version straight over the
	// deferred edit with no conflict and no sibling. keepDeferredDirty cannot undo that:
	// the local bytes are already gone. Reachable whenever a truncated round is also a
	// full reconcile (a reset sync.json on a grown vault), because then the hub sends a
	// delta for every path while our push is capped.
	for _, rel := range deferred {
		rejectedSet[rel] = true
	}
	// Preserve our losing versions locally before deltas overwrite the paths.
	unparked, err := writeConflictSiblings(vaultDir, resp.Conflicts)
	if err != nil {
		return Summary{}, err
	}
	for _, rel := range unparked {
		rejectedSet[rel] = true // no sibling holds this local version: protect it
	}
	parked, err := applyDeltas(vaultDir, resp.Deltas, sentHashes, rejectedSet)
	if err != nil {
		return Summary{}, err
	}
	// A full reconcile's deltas are upserts-only, so remove locals the snapshot
	// proves were deleted upstream (else they linger and can resurrect). Must run
	// before the recompute below so the dropped paths leave the persisted hashes.
	var dropped []string
	if resp.FullReconcile {
		dropped, err = dropFullReconcileOrphans(vaultDir, resp.Deltas, resp.Tombstones, state.Hashes)
		if err != nil {
			return Summary{}, err
		}
	} else if len(resp.Tombstones) > 0 {
		// Incremental drop-list: prune notes the hub deleted since our last seq, content-
		// safely. A scoped client may never receive the delete delta, so without this it
		// keeps the deleted note locally (serving stale knowledge) and can resurrect it.
		dropped, err = dropTombstoned(vaultDir, resp.Tombstones, state.Hashes)
		if err != nil {
			return Summary{}, err
		}
	}
	// Recompute hashes from disk so the next outbox reflects the canonical (post-
	// merge) hub state, not what we optimistically pushed; then keep any
	// guard-parked path dirty so its kept local change re-pushes next sync.
	_, current, err := computeOutbox(vaultDir, map[string]string{})
	if err != nil {
		return Summary{}, err
	}
	keepParkedDirty(current, state.Hashes, parked)
	keepWindowEditsDirty(current, sentHashes, resp.Deltas, dropped)
	// AFTER keepWindowEditsDirty on purpose: that one resets a path to its send-time
	// hash, which for a deferred path is exactly the hash that would mark it synced.
	// Proven by ablation: swapping these two lines puts the never-pushed remainder back.
	keepDeferredDirty(current, state.Hashes, deferred)
	// Hub-rejected paths (viewer role, folder ACL, out-of-scope, or oversize/binary
	// content) were NOT landed by the hub. Keep them dirty exactly like a parked path
	// so the next sync re-attempts the push, and never record the local (un-landed)
	// bytes as the synced base. Without this the rejected edit is silently treated as
	// synced, is invisible to the team forever, and is never retried when the user
	// later gains write permission.
	for _, rel := range resp.Rejected {
		if old, ok := state.Hashes[rel]; ok {
			current[rel] = old
		} else {
			delete(current, rel)
		}
	}
	if err := writeState(vaultDir, syncState{HeadSHA: resp.HeadSHA, Hashes: current, TombSeq: resp.TombstoneSeq}); err != nil {
		return Summary{}, err
	}
	sum := Summary{Pushed: len(outbox) - len(resp.Rejected), Pulled: len(resp.Deltas), Conflicts: len(resp.Conflicts), Head: resp.HeadSHA, Dropped: dropped, Rejected: resp.Rejected}
	// Tell the caller work remains, rather than letting a bounded batch look complete.
	sum.Remaining = len(deferred)
	for _, c := range resp.Conflicts {
		sum.ConflictSiblings = append(sum.ConflictSiblings, c.SiblingPath)
	}
	for _, p := range parked {
		sum.Protected = append(sum.Protected, p.sibling)
	}
	return sum, nil
}

// JoinVault redeems an invite, stores credentials, verifies embedding
// homogeneity against the hub-authoritative mesh.toml, and clones the vault via a
// reconcile from an empty base.
func JoinVault(hubURL, invite, vaultDir string) (Summary, error) {
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		return Summary{}, err
	}
	c := New(hubURL, "")
	jr, err := c.Join(invite)
	if err != nil {
		return Summary{}, err
	}
	if err := writeCredentials(vaultDir, credentials{HubURL: strings.TrimRight(hubURL, "/"), Token: jr.ClientToken}); err != nil {
		return Summary{}, err
	}
	c.Token = jr.ClientToken
	vi, err := c.Vault()
	if err != nil {
		return Summary{}, err
	}
	if err := checkHomogeneity(vi.MeshToml); err != nil {
		return Summary{}, err
	}
	// No local state yet -> base "" -> the hub returns a full snapshot.
	return SyncVault(vaultDir)
}

// checkHomogeneity fails closed if the vault's canonical embedding space (from the
// synced mesh.toml [embedding] section) conflicts with the operator's configured
// MESH_EMBED_MODEL / MESH_EMBED_DIM (SPEC 8: one embedding space per team). Both
// axes matter: two endpoints can serve the same model NAME at different widths (a
// requantized or truncated variant), which passes the name check but would later
// cosine across incompatible dimensions and emit a silent uniform garbage signal.
func checkHomogeneity(meshToml string) error {
	model := tomlSectionString(meshToml, "embedding", "model")
	env := strings.TrimSpace(os.Getenv("MESH_EMBED_MODEL"))
	if model != "" && env != "" && model != env {
		return fmt.Errorf("embedding model mismatch: this vault uses %q but MESH_EMBED_MODEL=%q; align them before joining (fail closed)", model, env)
	}
	if vd := tomlSectionString(meshToml, "embedding", "dim"); vd != "" && vd != "0" {
		if ed := strings.TrimSpace(os.Getenv("MESH_EMBED_DIM")); ed != "" && ed != "0" && ed != vd {
			return fmt.Errorf("embedding dim mismatch: this vault uses dim %s but MESH_EMBED_DIM=%s; align them before joining (fail closed)", vd, ed)
		}
	}
	return nil
}

// tomlSectionString pulls a simple `key = "value"` from inside a named [section]
// of the small, hub-written mesh.toml (keys before any section header are the
// top-level scope; pass section "" for those). Section-aware so a future section
// reusing a key name (e.g. a [rerank] model) cannot shadow the [embedding] one.
// Not a general TOML parser.
func tomlSectionString(toml, section, key string) string {
	cur := ""
	for _, line := range strings.Split(toml, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if cur != section {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}

// maxOutboxPerSync mirrors the hub's maxSyncOutboxItems. Kept slightly under it so a
// client a version ahead of its hub still fits.
const maxOutboxPerSync = 4000
