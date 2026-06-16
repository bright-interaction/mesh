package merge

import (
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)

func up(s string) Incoming { return Incoming{Op: "upsert", Content: []byte(s)} }
func del() Incoming        { return Incoming{Op: "delete"} }
func ver(s string) Version { return Version{Content: []byte(s), Exists: true} }
func absent() Version      { return Version{} }

func TestMergePathTable(t *testing.T) {
	cases := []struct {
		name string
		base Version
		hub  Version
		in   Incoming
		want Action
	}{
		{"brand new file", absent(), absent(), up("new\n"), ActionUpsert},
		{"fast-forward (only client changed)", ver("a\n"), ver("a\n"), up("a2\n"), ActionUpsert},
		{"converged (incoming == hub)", ver("a\n"), ver("b\n"), up("b\n"), ActionNoop},
		{"client no real change (incoming == base, hub advanced)", ver("a\n"), ver("b\n"), up("a\n"), ActionNoop},
		{"clean delete (hub == base)", ver("a\n"), ver("a\n"), del(), ActionDelete},
		{"delete already gone", ver("a\n"), absent(), del(), ActionNoop},
		{"delete vs edit keeps hub", ver("a\n"), ver("b\n"), del(), ActionNoop},
		{"edit vs tombstone -> conflict", ver("a\n"), absent(), up("a-edited\n"), ActionConflict},
		{"tombstone honored (client unchanged)", ver("a\n"), absent(), up("a\n"), ActionNoop},
		{"true overwrite conflict", ver("base\n"), ver("hub rewrite\n"), up("client rewrite\n"), ActionConflict},
		{"crlf vs lf is not a change", ver("a\n"), ver("line\r\nline2\n"), up("line\nline2\n"), ActionNoop},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MergePath("notes/x.md", c.base, c.hub, c.in, testTime, "alice")
			if got.Action != c.want {
				t.Fatalf("action = %q, want %q (%+v)", got.Action, c.want, got)
			}
			if got.Action == ActionConflict {
				if got.SiblingPath == "" || got.SiblingContent == nil {
					t.Errorf("conflict must set sibling path + content: %+v", got)
				}
			}
		})
	}
}

func TestConflictPreservesIncomingInSibling(t *testing.T) {
	got := MergePath("notes/x.md", ver("base\n"), ver("HUB\n"), up("CLIENT\n"), testTime, "Alice Smith")
	if got.Action != ActionConflict {
		t.Fatalf("want conflict, got %q", got.Action)
	}
	if string(got.SiblingContent) != "CLIENT\n" {
		t.Errorf("sibling must carry the losing client content, got %q", got.SiblingContent)
	}
	prefix := "notes/x.sync-conflict-20260616-alice-smith-"
	if !strings.HasPrefix(got.SiblingPath, prefix) || !strings.HasSuffix(got.SiblingPath, ".md") {
		t.Errorf("sibling path = %q, want prefix %q + 8-hex + .md", got.SiblingPath, prefix)
	}
}

func TestAppendMergeBottom(t *testing.T) {
	base := ver("alpha\n")
	hub := ver("alpha\n\nhub-add\n")
	in := up("alpha\n\nclient-add\n")
	got := MergePath("p.md", base, hub, in, testTime, "u")
	if got.Action != ActionUpsert {
		t.Fatalf("want additive upsert, got %q", got.Action)
	}
	for _, frag := range []string{"alpha", "hub-add", "client-add"} {
		if !strings.Contains(string(got.Content), frag) {
			t.Errorf("merged content missing %q: %q", frag, got.Content)
		}
	}
	if bl := blocks(got.Content); len(bl) != 3 || bl[0].raw != "alpha" {
		t.Errorf("expected 3 blocks with alpha first, got %d (%+v)", len(bl), bl)
	}
}

func TestAppendMergeDualPrepend(t *testing.T) {
	// The flywheel pattern: both sides prepend a new entry above shared content.
	base := ver("alpha\n\nbeta\n")
	hub := ver("hub-top\n\nalpha\n\nbeta\n")
	in := up("client-top\n\nalpha\n\nbeta\n")
	got := MergePath("p.md", base, hub, in, testTime, "u")
	if got.Action != ActionUpsert {
		t.Fatalf("dual-prepend should additive-merge, got %q", got.Action)
	}
	out := string(got.Content)
	for _, frag := range []string{"hub-top", "client-top", "alpha", "beta"} {
		if !strings.Contains(out, frag) {
			t.Errorf("missing %q in %q", frag, out)
		}
	}
	// Base order must be preserved.
	if strings.Index(out, "alpha") > strings.Index(out, "beta") {
		t.Errorf("base order not preserved: %q", out)
	}
}

func TestAppendMergeDedupIdenticalAddition(t *testing.T) {
	// Both sides added the SAME new block: it must appear once.
	base := ver("alpha\n")
	hub := ver("alpha\n\nshared-new\n")
	in := up("alpha\n\nshared-new\n")
	got := MergePath("p.md", base, hub, in, testTime, "u")
	if got.Action != ActionNoop {
		// incoming == hub here, so it converges to noop (already identical).
		t.Fatalf("identical sides should be noop, got %q", got.Action)
	}

	// Now both add the same block but hub also has a distinct one.
	hub = ver("alpha\n\nshared\n")
	in = up("alpha\n\nshared\n")
	got = MergePath("p.md", base, hub, in, testTime, "u")
	if got.Action != ActionNoop {
		t.Fatalf("got %q", got.Action)
	}
}

func TestAppendMergeRejectsEditedBaseBlock(t *testing.T) {
	// Hub rewrote a base block -> not purely additive -> true conflict.
	base := ver("alpha\n\nbeta\n")
	hub := ver("alpha-EDITED\n\nbeta\n")
	in := up("alpha\n\nbeta\n\nclient-add\n")
	got := MergePath("p.md", base, hub, in, testTime, "u")
	if got.Action != ActionConflict {
		t.Fatalf("editing a base block must conflict, got %q", got.Action)
	}
}

func TestRenameAsDeletePlusAdd(t *testing.T) {
	// A rename arrives as delete(old) + add(new) with the same content; node
	// identity (frontmatter id) keeps the graph stable, git records the rename.
	body := "---\nid: n\n---\n# N\nbody\n"
	delOld := MergePath("old.md", ver(body), ver(body), del(), testTime, "u")
	if delOld.Action != ActionDelete {
		t.Errorf("old path should delete, got %q", delOld.Action)
	}
	addNew := MergePath("new.md", absent(), absent(), up(body), testTime, "u")
	if addNew.Action != ActionUpsert || string(addNew.Content) != body {
		t.Errorf("new path should upsert the content, got %q (%q)", addNew.Action, addNew.Content)
	}
}

func TestIsText(t *testing.T) {
	if !IsText([]byte("# normal markdown\n")) {
		t.Error("normal markdown should be mergeable")
	}
	if IsText([]byte("bin\x00ary")) {
		t.Error("NUL byte content must be rejected")
	}
	if IsText(make([]byte, MaxNoteBytes+1)) {
		t.Error("oversize content must be rejected")
	}
}

func TestSiblingPathUniqueAndIdempotent(t *testing.T) {
	a1 := SiblingPath("a/b/c.md", testTime, "Bob", []byte("version one\n"))
	a2 := SiblingPath("a/b/c.md", testTime, "Bob", []byte("version one\n"))
	b := SiblingPath("a/b/c.md", testTime, "Bob", []byte("version two\n"))

	if a1 != a2 {
		t.Errorf("same losing content must yield the same sibling (idempotent): %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("different losing content must yield different siblings (no overwrite): %q", a1)
	}
	prefix := "a/b/c.sync-conflict-20260616-bob-"
	if !strings.HasPrefix(a1, prefix) || !strings.HasSuffix(a1, ".md") {
		t.Errorf("sibling = %q, want prefix %q + 8-hex + .md", a1, prefix)
	}
}

func TestCRLFPreservedInMergeOutput(t *testing.T) {
	// A CRLF base+hub with an LF-added block: the hub's CRLF bytes must survive,
	// and the LF addition merges (EOL-agnostic block identity).
	base := ver("line1\r\nline2\r\n")
	hub := ver("line1\r\nline2\r\n\r\nhub add\r\n")
	in := up("line1\nline2\n\nclient add\n")
	got := MergePath("p.md", base, hub, in, testTime, "u")
	if got.Action != ActionUpsert {
		t.Fatalf("want additive merge, got %q", got.Action)
	}
	if !strings.Contains(string(got.Content), "line1\r\nline2\r") {
		t.Errorf("CRLF bytes of the base block were not preserved: %q", got.Content)
	}
	for _, frag := range []string{"hub add", "client add"} {
		if !strings.Contains(string(got.Content), frag) {
			t.Errorf("missing %q in merged output: %q", frag, got.Content)
		}
	}
}

func TestRemovingDuplicateBaseBlockConflicts(t *testing.T) {
	// base has two identical blocks; client removes one. That is a real edit, so
	// it must NOT be accepted as additive (which would silently keep both).
	base := ver("dup\n\ndup\n\nkeep\n")
	hub := ver("dup\n\ndup\n\nkeep\n\nhub add\n")
	in := up("dup\n\nkeep\n") // client deleted one "dup"
	got := MergePath("p.md", base, hub, in, testTime, "u")
	if got.Action != ActionConflict {
		t.Fatalf("removing a duplicate base block must conflict, got %q", got.Action)
	}
}

func TestUnknownOpIsNoop(t *testing.T) {
	got := MergePath("p.md", ver("a\n"), ver("a\n"), Incoming{Op: "frobnicate", Content: []byte("x\n")}, testTime, "u")
	if got.Action != ActionNoop {
		t.Errorf("unknown op must fail safe to noop, got %q", got.Action)
	}
}
