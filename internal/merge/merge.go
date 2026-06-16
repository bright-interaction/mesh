// Package merge is the pure, deterministic three-way reconcile engine at the
// heart of Milestone S1 team sync. Given a path's three versions (the client's
// base ancestor, the hub's current HEAD, and the client's incoming change) it
// decides what the hub should land: a fast-forward, an additive append-merge, a
// clean delete, or a true conflict that keeps the hub version and parks the
// loser in a *.sync-conflict sibling. It does no I/O: the hub feeds it bytes
// read from git (S1.3) so every case is table-testable.
//
// Two SPEC 6.4/6.5 invariants drive it:
//   - Append-merge first. Two writers that only ADD blocks to a shared page
//     merge with no conflict (the flywheel's core write pattern), order- and
//     side-independent, deduped by content hash. Only a true overwrite of
//     existing content conflicts.
//   - Never lose data. A conflict keeps the hub version live and writes the
//     incoming version to a sibling a human resolves; a delete that races an
//     edit keeps the edit.
//
// Line endings are normalized to LF for hashing and equality only; block bytes
// are preserved (SPEC 6.5: do not rewrite a teammate's CRLF).
package merge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// MaxNoteBytes caps a mergeable note; larger or binary content is rejected
// upstream (SPEC: binaries are out of v1).
const MaxNoteBytes = 1 << 20

// Version is one side's content for a path. Exists is false when the path is
// absent on that side (never created, or deleted).
type Version struct {
	Content []byte
	Exists  bool
}

// Incoming is the client's proposed change for a path.
type Incoming struct {
	Op      string // "upsert" | "delete"
	Content []byte // for upsert
}

// Action is the resolved outcome for a path.
type Action string

const (
	ActionNoop     Action = "noop"     // hub is already correct; nothing to land
	ActionUpsert   Action = "upsert"   // write Content at Path
	ActionDelete   Action = "delete"   // remove Path
	ActionConflict Action = "conflict" // keep hub at Path; write the loser to SiblingPath
)

// Resolution is what the hub should land for one path.
type Resolution struct {
	Path           string
	Action         Action
	Content        []byte // ActionUpsert: the (possibly merged) content to write at Path
	SiblingPath    string // ActionConflict: where the losing version goes
	SiblingContent []byte // ActionConflict: the losing (incoming) content
}

// MergePath resolves one path's three-way state. now + user name the conflict
// sibling deterministically.
func MergePath(path string, base, hub Version, in Incoming, now time.Time, user string) Resolution {
	switch in.Op {
	case "delete":
		return mergeDelete(path, base, hub)
	case "upsert":
		return mergeUpsert(path, base, hub, in, now, user)
	default:
		// Unknown op: fail safe with a no-op rather than guess a write. The API
		// boundary (S1.3) rejects unknown ops at intake; this is defense in depth.
		return Resolution{Path: path, Action: ActionNoop}
	}
}

func mergeDelete(path string, base, hub Version) Resolution {
	if !hub.Exists {
		return Resolution{Path: path, Action: ActionNoop} // already gone
	}
	if sameVersion(base, hub) {
		return Resolution{Path: path, Action: ActionDelete} // hub == base: honor the delete
	}
	// Hub changed since base but the client wants it gone: keep the hub edit (no
	// data loss); the client receives the hub version on the response deltas.
	return Resolution{Path: path, Action: ActionNoop}
}

func mergeUpsert(path string, base, hub Version, in Incoming, now time.Time, user string) Resolution {
	// Converged already, or the client made no real change from base.
	if hub.Exists && normEqual(in.Content, hub.Content) {
		return Resolution{Path: path, Action: ActionNoop}
	}
	if base.Exists && normEqual(in.Content, base.Content) {
		// The client's "change" matches base, so it isn't one; defer to the hub
		// (whether the hub advanced or deleted the path).
		return Resolution{Path: path, Action: ActionNoop}
	}
	if !hub.Exists {
		if base.Exists {
			// Hub deleted a file the client genuinely edited: keep the deletion but
			// preserve the edit in a sibling for a human to resolve.
			return conflict(path, in.Content, now, user)
		}
		// Brand-new file the hub never had: apply it.
		return Resolution{Path: path, Action: ActionUpsert, Content: in.Content}
	}
	if sameVersion(base, hub) {
		// Only the client changed: fast-forward.
		return Resolution{Path: path, Action: ActionUpsert, Content: in.Content}
	}
	// Both sides changed: additive append-merge if possible, else a true conflict.
	if merged, ok := appendMerge(base, hub.Content, in.Content); ok {
		return Resolution{Path: path, Action: ActionUpsert, Content: merged}
	}
	return conflict(path, in.Content, now, user)
}

func conflict(path string, losing []byte, now time.Time, user string) Resolution {
	return Resolution{
		Path:           path,
		Action:         ActionConflict,
		SiblingPath:    SiblingPath(path, now, user, losing),
		SiblingContent: losing,
	}
}

// SiblingPath names a conflict sibling next to path, e.g.
// notes/x.md -> notes/x.sync-conflict-20260616-alice-1a2b3c4d.md.
// The short hash of the losing content makes the name unique per distinct
// version (so two conflicts on the same path/user/day never overwrite each
// other) AND idempotent (re-syncing the same conflict reuses the same sibling
// rather than spawning a new one every time).
func SiblingPath(path string, now time.Time, user string, losing []byte) string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	sum := sha256.Sum256([]byte(normalize(losing)))
	short := hex.EncodeToString(sum[:])[:16] // 64-bit: collisions cryptographically implausible
	return fmt.Sprintf("%s.sync-conflict-%s-%s-%s%s", stem, now.Format("20060102"), sanitizeUser(user), short, ext)
}

// appendMerge unions two additive edits to a shared file. It succeeds only when
// both hub and incoming preserve every base block (i.e. each side only ADDED
// blocks). Incoming's novel blocks are inserted into the hub sequence anchored by
// the block that precedes them in incoming (or at the top if none), so a
// top-prepend lands at the top and a bottom-append at the bottom. Identical
// blocks added by both sides collapse to one (content-hash dedup).
func appendMerge(base Version, hub, incoming []byte) ([]byte, bool) {
	if !base.Exists {
		return nil, false // no common ancestor; cannot be a clean additive merge
	}
	baseBlocks := blocks(base.Content)
	hubBlocks := blocks(hub)
	inBlocks := blocks(incoming)
	baseCounts := hashCounts(baseBlocks)
	if !preservesAll(hubBlocks, baseCounts) || !preservesAll(inBlocks, baseCounts) {
		return nil, false // a base block was edited or removed: a real conflict
	}

	result := append([]block(nil), hubBlocks...)
	placed := hashSet(hubBlocks)
	for i, b := range inBlocks {
		if placed[b.hash] {
			continue
		}
		anchor := "" // empty => insert at the front
		for j := i - 1; j >= 0; j-- {
			if placed[inBlocks[j].hash] {
				anchor = inBlocks[j].hash
				break
			}
		}
		result = insertAfter(result, anchor, b)
		placed[b.hash] = true
	}
	return render(result), true
}

// IsText reports whether content is small enough and free of NUL bytes to be a
// mergeable markdown note (binaries are rejected; SPEC v1 has no blob support).
func IsText(content []byte) bool {
	return len(content) <= MaxNoteBytes && !bytes.Contains(content, []byte{0})
}

// ---- blocks ----

type block struct {
	raw  string // verbatim block text (preserved on output)
	hash string // sha256 of the LF-normalized, per-line right-trimmed text
}

// blocks splits content into paragraph blocks on blank-line boundaries.
func blocks(content []byte) []block {
	var out []block
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		raw := strings.Join(cur, "\n")
		cur = nil
		if strings.TrimSpace(raw) != "" {
			out = append(out, block{raw: raw, hash: hashBlock(raw)})
		}
	}
	// Split the ORIGINAL bytes (not normalized) so each block's raw keeps its
	// line endings verbatim; a CRLF line becomes "...\r" after splitting on "\n"
	// and the \r is preserved on output. Hashing (below) normalizes, so EOL style
	// never affects block identity.
	for _, ln := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(ln) == "" {
			flush()
			continue
		}
		cur = append(cur, ln)
	}
	flush()
	return out
}

func hashBlock(raw string) string {
	var b strings.Builder
	for i, ln := range strings.Split(raw, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		// Strip trailing \r so a CRLF block and its LF twin hash identically.
		b.WriteString(strings.TrimRight(ln, " \t\r"))
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(b.String())))
	return hex.EncodeToString(sum[:])
}

func render(bl []block) []byte {
	parts := make([]string, len(bl))
	for i, b := range bl {
		parts[i] = strings.TrimRight(b.raw, "\n")
	}
	return []byte(strings.Join(parts, "\n\n") + "\n")
}

func hashSet(bl []block) map[string]bool {
	s := make(map[string]bool, len(bl))
	for _, b := range bl {
		s[b.hash] = true
	}
	return s
}

func hashCounts(bl []block) map[string]int {
	m := make(map[string]int, len(bl))
	for _, b := range bl {
		m[b.hash]++
	}
	return m
}

// preservesAll reports whether side keeps at least as many copies of every base
// block as base has. Counting (not set membership) matters when a note has
// byte-identical duplicate blocks: removing one of them is a real edit that must
// fall through to a conflict, not be silently accepted as additive.
func preservesAll(side []block, baseCounts map[string]int) bool {
	have := hashCounts(side)
	for h, n := range baseCounts {
		if have[h] < n {
			return false
		}
	}
	return true
}

func insertAfter(list []block, anchorHash string, b block) []block {
	if anchorHash == "" {
		return append([]block{b}, list...)
	}
	for i, x := range list {
		if x.hash == anchorHash {
			out := make([]block, 0, len(list)+1)
			out = append(out, list[:i+1]...)
			out = append(out, b)
			out = append(out, list[i+1:]...)
			return out
		}
	}
	return append(list, b) // anchor not found (shouldn't happen); append at end
}

// ---- helpers ----

func normalize(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func normEqual(a, b []byte) bool { return normalize(a) == normalize(b) }

func sameVersion(a, b Version) bool {
	if a.Exists != b.Exists {
		return false
	}
	if !a.Exists {
		return true
	}
	return normEqual(a.Content, b.Content)
}

func sanitizeUser(user string) string {
	user = strings.ToLower(strings.TrimSpace(user))
	var b strings.Builder
	for _, r := range user {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "user"
	}
	return out
}
