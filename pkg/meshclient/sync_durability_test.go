// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

// TestSyncVaultBoundedBatchPushesTheRemainder pins the round-by-round contract of
// the bounded push.
//
// The bound truncates the outbox to maxOutboxPerSync, then the new sync base was
// recomputed from the WHOLE DISK. The truncated notes are on disk, so their current
// hashes became the base, the next computeOutbox saw prev[rel] == h and emitted
// nothing, and keepWindowEditsDirty could not rescue them either (sentHashes is the
// full on-disk map, so current[rel] == sentHashes[rel] and no restore fired). The
// remainder was silently recorded as synced and NEVER pushed to the team, while
// Summary reported convergence.
//
// Deletes are appended last in the outbox, so on a large dirty batch they truncate
// first: a locally deleted note stayed alive on the hub forever. Phase 2 covers that.
//
// Asserted by CONTENT (which paths were sent in which round), not just by count.
func TestSyncVaultBoundedBatchPushesTheRemainder(t *testing.T) {
	const extra = 25
	vaultDir := t.TempDir()
	total := maxOutboxPerSync + extra
	want := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		rel := fmt.Sprintf("notes/n%05d.md", i)
		write(t, vaultDir, rel, fmt.Sprintf("# note %d\nv1\n", i))
		want[rel] = true
	}

	var rounds [][]outboxRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds = append(rounds, recordOutbox(t, r))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: fmt.Sprintf("head%d", len(rounds))})
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(vaultDir, syncState{HeadSHA: "base", Hashes: map[string]string{}, HubURL: srv.URL}); err != nil {
		t.Fatal(err)
	}

	var remaining []int
	var deferred [][]string
	for i := 0; i < 3; i++ {
		sum, err := SyncVault(vaultDir)
		if err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		remaining = append(remaining, sum.Remaining)
		deferred = append(deferred, append([]string(nil), sum.Deferred...))
	}

	wantSizes := []int{maxOutboxPerSync, extra, 0}
	if got := sizes(rounds); !equalInts(got, wantSizes) {
		t.Fatalf("outbox sizes per round = %v, want %v (the remainder must push next round, not be baselined as synced)", got, wantSizes)
	}
	if wantRemaining := []int{extra, 0, 0}; !equalInts(remaining, wantRemaining) {
		t.Errorf("Summary.Remaining per round = %v, want %v", remaining, wantRemaining)
	}
	if len(deferred[0]) != extra || len(deferred[1]) != 0 || len(deferred[2]) != 0 {
		t.Fatalf("Summary.Deferred sizes per round = [%d %d %d], want [%d 0 0]", len(deferred[0]), len(deferred[1]), len(deferred[2]), extra)
	}
	firstSent := map[string]bool{}
	for _, it := range rounds[0] {
		firstSent[it.path] = true
	}
	for _, rel := range deferred[0] {
		if firstSent[rel] {
			t.Fatalf("Summary.Deferred named %s even though the first round sent it", rel)
		}
	}
	sent := map[string]string{}
	for i, round := range rounds {
		for _, it := range round {
			if prev, dup := sent[it.path]; dup {
				t.Fatalf("%s was pushed twice (round %d op %s, again in round %d op %s)", it.path, i, prev, i+1, it.op)
			}
			sent[it.path] = it.op
		}
	}
	for rel := range want {
		op, ok := sent[rel]
		if !ok {
			t.Fatalf("%s was never pushed to the hub across %d rounds; the truncated remainder was baselined as synced", rel, len(rounds))
		}
		if op != "upsert" {
			t.Fatalf("%s pushed as %q, want upsert", rel, op)
		}
	}
	if len(sent) != len(want) {
		t.Fatalf("pushed %d distinct paths, want %d", len(sent), len(want))
	}

	// Phase 2: deletes are appended LAST, so they are the first thing truncated.
	// Dirty every note again and delete one, which makes the delete outbox item
	// number maxOutboxPerSync+extra. It must still reach the hub next round.
	const deletedNote = "notes/n00007.md"
	for rel := range want {
		if rel == deletedNote {
			continue
		}
		write(t, vaultDir, rel, "v2\n")
	}
	if err := os.Remove(filepath.Join(vaultDir, filepath.FromSlash(deletedNote))); err != nil {
		t.Fatal(err)
	}
	rounds = nil
	for i := 0; i < 2; i++ {
		if _, err := SyncVault(vaultDir); err != nil {
			t.Fatalf("phase 2 round %d: %v", i+1, err)
		}
	}
	if got := sizes(rounds); !equalInts(got, []int{maxOutboxPerSync, extra}) {
		t.Fatalf("phase 2 outbox sizes = %v, want [%d %d]", got, maxOutboxPerSync, extra)
	}
	deleteRound := -1
	for i, round := range rounds {
		for _, it := range round {
			if it.path == deletedNote && it.op == "delete" {
				deleteRound = i
			}
		}
	}
	if deleteRound != 1 {
		t.Fatalf("the delete for %s landed in round %d (-1 = never); a truncated delete must push next round or the note lives on the hub forever", deletedNote, deleteRound+1)
	}
}

func TestCombinedSummaryCarriesTheFinalExactDeferredSet(t *testing.T) {
	first := Summary{Remaining: 2, Deferred: []string{"notes/a.md", "notes/b.md"}, Head: "pull"}
	second := Summary{Pushed: 1, Remaining: 1, Deferred: []string{"notes/b.md"}, Head: "push"}
	got := combineSummaries(first, second)
	if got.Remaining != 1 || len(got.Deferred) != 1 || got.Deferred[0] != "notes/b.md" {
		t.Fatalf("combined deferred state = count %d paths %v, want final round's exact tail", got.Remaining, got.Deferred)
	}
	second.Deferred[0] = "mutated.md"
	if got.Deferred[0] != "notes/b.md" {
		t.Fatal("combined Summary aliases the caller's deferred slice")
	}
}

// TestSyncVaultProtectsDeferredNotesFromInboundDeltas covers the other half of the
// bounded push: keeping a deferred note dirty is worthless if the same round destroys
// its bytes.
//
// The hub never saw a deferred note, so a delta it sends for that path is built on a
// base that does not contain the local change. applyDeltas compares disk against
// sentHashes, which is the on-disk map with dirty files included, so it finds them
// equal, concludes nothing changed locally, and writes the hub version over the local
// edit with no conflict and no sibling. Reachable whenever a truncated round is also a
// full reconcile, i.e. a reset sync.json on a vault that has since grown.
func TestSyncVaultProtectsDeferredNotesFromInboundDeltas(t *testing.T) {
	vaultDir := t.TempDir()
	total := maxOutboxPerSync + 2
	for i := 0; i < total; i++ {
		write(t, vaultDir, fmt.Sprintf("notes/n%05d.md", i), fmt.Sprintf("local %d\n", i))
	}
	// Lexical walk order puts the highest-numbered notes last in the outbox, so the
	// last one is deferred by the bound.
	deferredNote := fmt.Sprintf("notes/n%05d.md", total-1)
	localBody := fmt.Sprintf("local %d\n", total-1)

	var rounds [][]outboxRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds = append(rounds, recordOutbox(t, r))
		resp := syncproto.SyncResponse{HeadSHA: fmt.Sprintf("head%d", len(rounds))}
		if len(rounds) == 1 {
			// The hub answers with its own version of the note we could not send.
			resp.Deltas = []syncproto.Delta{{
				Path: deferredNote, Op: "upsert",
				ContentB64: b64("hub version\n"),
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	// A non-empty but GC'd base makes a real hub answer FullReconcile without
	// triggering the unknown-base pull-first phase that this test does not target.
	if err := writeState(vaultDir, syncState{HeadSHA: "gc-d-base", Hashes: map[string]string{}, HubURL: srv.URL}); err != nil {
		t.Fatal(err)
	}

	sum, err := SyncVault(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, vaultDir, deferredNote); got != localBody {
		t.Fatalf("the deferred local edit was overwritten by the hub: %s = %q, want %q", deferredNote, got, localBody)
	}
	if len(sum.Protected) != 1 {
		t.Fatalf("the incoming hub version must be parked in a sibling, Protected = %v", sum.Protected)
	}
	if got := read(t, vaultDir, sum.Protected[0]); got != "hub version\n" {
		t.Errorf("sibling %s = %q, want the hub version", sum.Protected[0], got)
	}

	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("expected 2 sync rounds, got %d", len(rounds))
	}
	found := false
	for _, it := range rounds[1] {
		if it.path == deferredNote && it.op == "upsert" {
			found = true
		}
	}
	if !found {
		t.Errorf("%s must re-push next round so the team gets the local edit; round 2 sent %d items", deferredNote, len(rounds[1]))
	}
}

// TestWriteFileAtomicSurvivesAndCleansUp is the behavioural half of the durability
// change: the write must still produce exactly the right bytes, leave no temp file
// behind, and report no error on a platform where a directory fsync is refused.
// A power-loss repro is not possible in a unit test, so the fsync calls themselves
// are pinned by TestWriteFileAtomicFsyncsBeforeAndAfterRename below.
func TestWriteFileAtomicSurvivesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes", "a.md")
	cases := []struct {
		name string
		body string
	}{
		{name: "new file", body: "v1\n"},
		{name: "overwrite", body: "v2 longer body\n"},
		{name: "empty body", body: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := writeFileAtomic(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("writeFileAtomic: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.body {
				t.Errorf("content = %q, want %q", got, tc.body)
			}
			ents, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".mesh-tmp-") {
					t.Errorf("temp file %s left behind", e.Name())
				}
			}
		})
	}
}

// TestWriteFileAtomicFsyncsBeforeAndAfterRename guards the durability half of the
// client's atomic write. Temp+rename buys crash-atomicity for a concurrent READER,
// not durability across power loss: the rename can be durable while the data blocks
// are not, and the note write and the sync.json write can reach the disk in either
// order. If sync.json survives and the note's bytes do not, the note reverts locally
// while state.Hashes records the new hash, and the next round pushes the OLD bytes
// as an upsert that the hub fast-forwards, reverting the team's copy with no conflict
// and no sibling.
//
// fsync is not observable from a unit test, so this asserts on the source the same
// way TestNoSilentDropInByteCarryingLoops does. internal/hub/repo.go carries the
// identical twin and has its own copy of this guard; this estate keeps fixing one
// twin and leaving the other, so both are pinned separately (the hub file is not in
// this package's mirror, so it cannot be read from here).
func TestWriteFileAtomicFsyncsBeforeAndAfterRename(t *testing.T) {
	body := funcBody(t, "vault.go", "writeFileAtomic")
	renameAt := strings.Index(body, "os.Rename(")
	if renameAt < 0 {
		t.Fatal("writeFileAtomic in vault.go no longer renames; this guard needs rewriting")
	}
	fileSyncAt := -1
	if loc := regexp.MustCompile(`\b(tmp|f)\.Sync\(\)`).FindStringIndex(body); loc != nil {
		fileSyncAt = loc[0]
	}
	dirSyncAt := strings.Index(body, "syncDir(")
	switch {
	case fileSyncAt < 0:
		t.Error("writeFileAtomic in vault.go does not fsync the temp file; a rename can be durable " +
			"while the data blocks are not, so the note reverts after a power cut while sync.json " +
			"records the new hash and the stale bytes re-push over the team's copy")
	case fileSyncAt > renameAt:
		t.Error("the temp file fsync must happen BEFORE the rename, otherwise the rename can publish unwritten blocks")
	}
	switch {
	case dirSyncAt < 0:
		t.Error("writeFileAtomic in vault.go does not fsync the parent directory; the rename itself " +
			"can be lost, leaving the old note (or no note) behind")
	case dirSyncAt < renameAt:
		t.Error("the directory fsync must happen AFTER the rename, otherwise it does not cover the new directory entry")
	}
}

// outboxRecord is one pushed item, reduced to what the assertions care about.
type outboxRecord struct {
	path string
	op   string
}

func recordOutbox(t *testing.T, r *http.Request) []outboxRecord {
	t.Helper()
	var req syncproto.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode sync request: %v", err)
		return nil
	}
	out := make([]outboxRecord, 0, len(req.Outbox))
	for _, it := range req.Outbox {
		out = append(out, outboxRecord{path: it.Path, op: it.Op})
	}
	return out
}

func sizes(rounds [][]outboxRecord) []int {
	out := make([]int, 0, len(rounds))
	for _, r := range rounds {
		out = append(out, len(r))
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// funcBody returns the CODE of one top-level function in a file of this package,
// with every comment stripped.
//
// Stripping matters, and a raw line scan does not do it. A body that merely
// mentions tmp.Sync() in a comment while the call itself is gone contains no
// fsync at all, and a text scan that reads comments reports it as guarded:
// verified by ablation, where deleting both calls and leaving "// tmp.Sync()
// removed, see history" behind kept the guard green. Parsing without
// parser.ParseComments leaves the comments out of the AST, so printing the body
// yields code only.
func funcBody(t *testing.T, file, fn string) string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, filepath.Join(".", file), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		var buf bytes.Buffer
		if perr := printer.Fprint(&buf, fset, fd.Body); perr != nil {
			t.Fatalf("%s: print %s: %v", file, fn, perr)
		}
		return buf.String()
	}
	t.Fatalf("%s: func %s not found", file, fn)
	return ""
}
