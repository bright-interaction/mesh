// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/vault"
)

// A rejected local delete is still a local change. The hub may include its live
// version of that path in the same full-reconcile response; applying that upsert
// would resurrect the note locally and erase the delete from the next outbox.
func TestSyncVaultRejectedDeleteStaysAbsentAndRetries(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/delete-me.md"
	old := []byte("hub and old local version\n")

	var requests []syncproto.SyncRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req syncproto.SyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{
			HeadSHA:       "head-after-refusal",
			FullReconcile: true,
			Deltas: []syncproto.Delta{{
				Path: rel, Op: "upsert", ContentB64: b64(string(old)),
			}},
			Rejected: []string{rel},
		})
	}))
	defer srv.Close()

	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(vaultDir, syncState{
		HeadSHA: "base", Hashes: map[string]string{rel: contentHash(old)}, HubURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	sum, err := SyncVault(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Rejected) != 1 || sum.Rejected[0] != rel {
		t.Fatalf("Rejected = %v, want [%s]", sum.Rejected, rel)
	}
	if exists(vaultDir, rel) {
		t.Fatal("the hub's rejected-delete response resurrected the locally deleted note")
	}
	if len(requests) != 1 || len(requests[0].Outbox) != 1 ||
		requests[0].Outbox[0].Path != rel || requests[0].Outbox[0].Op != "delete" {
		t.Fatalf("first outbox = %+v, want one delete for %s", requests, rel)
	}
	outbox, _, err := computeOutbox(vaultDir, readState(vaultDir).Hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 || outbox[0].Path != rel || outbox[0].Op != "delete" {
		t.Fatalf("next outbox = %+v, want the rejected delete to retry", outbox)
	}
}

// A delete delta can race a local edit just like an upsert delta can. Keeping
// the bytes is not sufficient: the persisted base must keep the edit dirty.
func TestSyncVaultIncomingDeleteDoesNotBaselineWindowEdit(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/a.md"
	old := []byte("old\n")
	local := []byte("edited while request was in flight\n")
	write(t, vaultDir, rel, string(old))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(filepath.Join(vaultDir, filepath.FromSlash(rel)), local, 0o644); err != nil {
			t.Errorf("write window edit: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{
			HeadSHA: "head-with-delete",
			Deltas:  []syncproto.Delta{{Path: rel, Op: "delete"}},
		})
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(vaultDir, syncState{
		HeadSHA: "base", Hashes: map[string]string{rel: contentHash(old)}, HubURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatal(err)
	}
	if got := read(t, vaultDir, rel); got != string(local) {
		t.Fatalf("local edit = %q, want %q", got, local)
	}
	outbox, _, err := computeOutbox(vaultDir, readState(vaultDir).Hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 || outbox[0].Path != rel || outbox[0].Op != "upsert" {
		t.Fatalf("next outbox = %+v, want the kept edit to re-push", outbox)
	}
}

// A read error is uncertainty, not absence. In particular, hashing the nil
// bytes returned with EACCES as though they were an empty file lets the guarded
// delete capture and discard a real empty note that appeared during the request.
func TestApplyDeltasDeleteFailsClosedOnUnreadableExistingFile(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/unreadable.md"
	write(t, vaultDir, rel, "")
	abs, err := safeNotePath(vaultDir, rel)
	if err != nil {
		t.Fatal(err)
	}

	oldRead := readDeltaFile
	readDeltaFile = func(path string) ([]byte, error) {
		if path == abs {
			return nil, fs.ErrPermission
		}
		return os.ReadFile(path)
	}
	t.Cleanup(func() { readDeltaFile = oldRead })

	if _, err := applyDeltas(vaultDir, []syncproto.Delta{{Path: rel, Op: "delete"}}, map[string]string{}, nil); err == nil {
		t.Fatal("delete treated an unreadable existing file as absent")
	}
	if b, err := os.ReadFile(abs); err != nil || len(b) != 0 {
		t.Fatalf("unreadable note moved or changed: bytes=%q err=%v", b, err)
	}
	siblings, err := filepath.Glob(filepath.Join(vaultDir, "notes", "unreadable.sync-conflict-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 0 {
		t.Fatalf("failed read relocated the live note to %v", siblings)
	}
}

// Root config files are hub-owned inbound material, not client notes. They may
// be applied from a delta, but must never enter the client's note hash baseline:
// vault.Walk intentionally omits them, so a later round would otherwise infer a
// local delete and ask the hub to delete its own mesh.toml/.gitattributes.
func TestSyncVaultNeverPushesDeletesForInboundRootFiles(t *testing.T) {
	vaultDir := t.TempDir()
	var requests []syncproto.SyncRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req syncproto.SyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		resp := syncproto.SyncResponse{HeadSHA: fmt.Sprintf("head-%d", len(requests))}
		if len(requests) == 1 {
			resp.FullReconcile = true
			resp.Deltas = []syncproto.Delta{
				{Path: "mesh.toml", Op: "upsert", ContentB64: b64("[embedding]\nmodel = \"test\"\ndimensions = 3\n")},
				{Path: ".gitattributes", Op: "upsert", ContentB64: b64("*.md text eol=lf\n")},
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(vaultDir, syncState{HeadSHA: "base", Hashes: map[string]string{}, HubURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	for _, item := range requests[1].Outbox {
		if item.Path == "mesh.toml" || item.Path == ".gitattributes" {
			t.Fatalf("second-round outbox contains hub-owned root delete: %+v", requests[1].Outbox)
		}
	}
	for rel := range readState(vaultDir).Hashes {
		if _, ok := vault.SafeSyncNotePath(rel); !ok {
			t.Fatalf("persisted non-note hash %q", rel)
		}
	}
}

func TestSyncVaultRejectsMalformedRejectedEntriesAtomically(t *testing.T) {
	tests := []struct {
		name     string
		rel      string
		rejected []string
	}{
		{name: "exact duplicate", rel: "notes/a.md", rejected: []string{"notes/a.md", "notes/a.md"}},
		{name: "case alias", rel: "Notes/A.md", rejected: []string{"Notes/A.md", "notes/a.md"}},
		{name: "unicode alias", rel: "notes/café.md", rejected: []string{"notes/café.md", "notes/café.md"}},
		{name: "unicode fold alias", rel: "notes/Σ.md", rejected: []string{"notes/Σ.md", "notes/ς.md"}},
		{name: "not in transmitted outbox", rel: "notes/a.md", rejected: []string{"notes/other.md"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vaultDir := t.TempDir()
			old := []byte("old synced bytes\n")
			local := []byte("dirty local bytes\n")
			write(t, vaultDir, tc.rel, string(local))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{
					HeadSHA: "malformed-head", Rejected: tc.rejected,
				})
			}))
			defer srv.Close()
			if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
				t.Fatal(err)
			}
			if err := writeState(vaultDir, syncState{
				HeadSHA: "base", Hashes: map[string]string{tc.rel: contentHash(old)}, TombSeq: 3, HubURL: srv.URL,
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := SyncVault(vaultDir); err == nil {
				t.Fatal("malformed Rejected response was accepted")
			}
			if got := read(t, vaultDir, tc.rel); got != string(local) {
				t.Fatalf("malformed response changed local bytes to %q", got)
			}
			st := readState(vaultDir)
			if st.HeadSHA != "base" || st.TombSeq != 3 || st.Hashes[tc.rel] != contentHash(old) {
				t.Fatalf("malformed response advanced state: %+v", st)
			}
		})
	}
}

// Rejected protection is keyed by the exact transmitted path. An aliased entry
// must not bypass that guard and let a same-response upsert overwrite the bytes
// the hub claims it rejected.
func TestSyncVaultRejectedAliasCannotBypassLocalProtection(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "Notes/A.md"
	old := []byte("old synced bytes\n")
	local := []byte("dirty local bytes\n")
	write(t, vaultDir, rel, string(local))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{
			HeadSHA:  "malformed-head",
			Rejected: []string{"notes/a.md"},
			Deltas: []syncproto.Delta{{
				Path: rel, Op: "upsert", ContentB64: b64("hub replacement\n"),
			}},
		})
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(vaultDir, syncState{
		HeadSHA: "base", Hashes: map[string]string{rel: contentHash(old)}, HubURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncVault(vaultDir); err == nil {
		t.Fatal("aliased Rejected entry was accepted")
	}
	if got := read(t, vaultDir, rel); got != string(local) {
		t.Fatalf("aliased rejection let the delta overwrite local bytes with %q", got)
	}
	if st := readState(vaultDir); st.HeadSHA != "base" || st.Hashes[rel] != contentHash(old) {
		t.Fatalf("malformed response advanced state: %+v", st)
	}
}

func TestSyncVaultPullFirstRejectsRejectedWithoutTransmittedOutbox(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/local.md"
	write(t, vaultDir, rel, "local survivor\n")
	var requests []syncproto.SyncRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req syncproto.SyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{
			HeadSHA: "malformed-head", FullReconcile: true, Rejected: []string{rel},
		})
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncVault(vaultDir); err == nil {
		t.Fatal("pull-first accepted Rejected despite transmitting no outbox")
	}
	if len(requests) != 1 || len(requests[0].Outbox) != 0 {
		t.Fatalf("requests = %+v, want one pull-only request", requests)
	}
	if got := read(t, vaultDir, rel); got != "local survivor\n" {
		t.Fatalf("malformed pull response changed local bytes to %q", got)
	}
	if st := readState(vaultDir); st.HeadSHA != "" {
		t.Fatalf("malformed pull response advanced state: %+v", st)
	}
}

// A syntactically valid 200 response can still violate the protocol. Unknown
// delta operations must fail the round before they turn their payload into an
// upsert and overwrite a note.
func TestApplyDeltasRejectsUnknownOperation(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/a.md"
	write(t, vaultDir, rel, "local\n")

	_, err := applyDeltas(vaultDir, []syncproto.Delta{{
		Path: rel, Op: "future-op", ContentB64: b64("unexpected replacement\n"),
	}}, map[string]string{rel: contentHash([]byte("local\n"))}, nil)
	if err == nil {
		t.Fatal("unknown delta operation was accepted")
	}
	if got := read(t, vaultDir, rel); got != "local\n" {
		t.Fatalf("unknown delta overwrote the note with %q", got)
	}
}

func TestApplyDeltasRejectsDuplicateFilesystemAliasesAtomically(t *testing.T) {
	tests := []struct {
		name   string
		first  string
		second string
	}{
		{name: "exact duplicate", first: "notes/a.md", second: "notes/a.md"},
		{name: "case alias", first: "Notes/A.md", second: "notes/a.md"},
		{name: "unicode normalization alias", first: "notes/café.md", second: "notes/café.md"},
		{name: "unicode fold alias", first: "notes/Σ.md", second: "notes/ς.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vaultDir := t.TempDir()
			write(t, vaultDir, tc.first, "old\n")
			deltas := []syncproto.Delta{
				{Path: tc.first, Op: "upsert", ContentB64: b64("first mutation\n")},
				{Path: tc.second, Op: "delete"},
			}
			_, err := applyDeltas(vaultDir, deltas, map[string]string{tc.first: contentHash([]byte("old\n"))}, nil)
			if err == nil {
				t.Fatal("aliased duplicate deltas were accepted")
			}
			if got := read(t, vaultDir, tc.first); got != "old\n" {
				t.Fatalf("response mutated disk before duplicate validation: got %q", got)
			}
		})
	}
}

func TestApplyDeltasRejectsNonTextContentAtomically(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "binary", content: []byte("binary\x00markdown")},
		{name: "oversize", content: make([]byte, merge.MaxNoteBytes+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vaultDir := t.TempDir()
			write(t, vaultDir, "notes/first.md", "old first\n")
			write(t, vaultDir, "notes/bad.md", "old bad\n")
			deltas := []syncproto.Delta{
				{Path: "notes/first.md", Op: "upsert", ContentB64: b64("mutated before validation\n")},
				{Path: "notes/bad.md", Op: "upsert", ContentB64: base64.StdEncoding.EncodeToString(tc.content)},
			}
			_, err := applyDeltas(vaultDir, deltas, map[string]string{
				"notes/first.md": contentHash([]byte("old first\n")),
				"notes/bad.md":   contentHash([]byte("old bad\n")),
			}, nil)
			if err == nil {
				t.Fatal("non-text delta content was accepted")
			}
			if got := read(t, vaultDir, "notes/first.md"); got != "old first\n" {
				t.Fatalf("response mutated an earlier path before content validation: %q", got)
			}
		})
	}
}

// A full snapshot cannot truthfully describe the same client pathname as both
// live and tombstoned. Validate that contradiction before applying even the
// first delta, or a malformed response can mutate disk and advance TombSeq.
func TestSyncVaultRejectsLiveTombstoneContradictionAtomically(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/a.md"
	write(t, vaultDir, rel, "old\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{
			HeadSHA:       "malformed-head",
			FullReconcile: true,
			Deltas:        []syncproto.Delta{{Path: rel, Op: "upsert", ContentB64: b64("new\n")}},
			Tombstones:    []string{rel},
			TombstoneSeq:  99,
		})
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(vaultDir, syncState{
		HeadSHA: "base", Hashes: map[string]string{rel: contentHash([]byte("old\n"))}, HubURL: srv.URL, TombSeq: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncVault(vaultDir); err == nil {
		t.Fatal("contradictory live+tombstoned response was accepted")
	}
	if got := read(t, vaultDir, rel); got != "old\n" {
		t.Fatalf("malformed response mutated the note before rejection: %q", got)
	}
	st := readState(vaultDir)
	if st.HeadSHA != "base" || st.TombSeq != 1 {
		t.Fatalf("malformed response advanced state: %+v", st)
	}
}

func TestValidateSyncResponseRejectsTombstoneFilesystemAliases(t *testing.T) {
	tests := []struct {
		name string
		resp syncproto.SyncResponse
	}{
		{
			name: "case-aliased live path",
			resp: syncproto.SyncResponse{
				Deltas:     []syncproto.Delta{{Path: "Notes/A.md", Op: "upsert", ContentB64: b64("live\n")}},
				Tombstones: []string{"notes/a.md"},
			},
		},
		{
			name: "unicode-aliased live path",
			resp: syncproto.SyncResponse{
				Deltas:     []syncproto.Delta{{Path: "notes/café.md", Op: "upsert", ContentB64: b64("live\n")}},
				Tombstones: []string{"notes/café.md"},
			},
		},
		{
			name: "unicode-fold-aliased live path",
			resp: syncproto.SyncResponse{
				Deltas:     []syncproto.Delta{{Path: "notes/Σ.md", Op: "upsert", ContentB64: b64("live\n")}},
				Tombstones: []string{"notes/ς.md"},
			},
		},
		{
			name: "duplicate tombstones",
			resp: syncproto.SyncResponse{Tombstones: []string{"Notes/A.md", "notes/a.md"}},
		},
		{
			name: "hub-owned root is not a note tombstone",
			resp: syncproto.SyncResponse{Tombstones: []string{"mesh.toml"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSyncResponse(t.TempDir(), tc.resp, nil); err == nil {
				t.Fatalf("malformed tombstone response was accepted: %+v", tc.resp)
			}
		})
	}
}

// Upserts had the same pathname TOCTOU as deletes: the guard hashed the old
// inode, then writeFileAtomic renamed over whatever inode occupied the path by
// then. An editor's atomic save in that gap must win and keep the hub copy parked.
func TestApplyDeltasDoesNotOverwriteAtomicRenameAfterHashCheck(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/a.md"
	old := []byte("synced old bytes\n")
	late := []byte("editor atomic-save bytes\n")
	write(t, vaultDir, rel, string(old))

	beforeDurableUpsert = func(path string) {
		beforeDurableUpsert = nil
		tmp := filepath.Join(filepath.Dir(path), ".editor-save.md")
		if err := os.WriteFile(tmp, late, 0o644); err != nil {
			t.Errorf("stage editor save: %v", err)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Errorf("atomic editor save: %v", err)
		}
	}
	t.Cleanup(func() { beforeDurableUpsert = nil })

	parked, err := applyDeltas(vaultDir, []syncproto.Delta{{
		Path: rel, Op: "upsert", ContentB64: b64("hub replacement\n"),
	}}, map[string]string{rel: contentHash(old)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, vaultDir, rel); got != string(late) {
		t.Fatalf("late editor bytes were overwritten: got %q, want %q", got, late)
	}
	if len(parked) != 1 || parked[0].note != rel {
		t.Fatalf("parked = %v, want the hub replacement preserved beside %s", parked, rel)
	}
	if got := read(t, vaultDir, parked[0].sibling); got != "hub replacement\n" {
		t.Fatalf("parked hub copy = %q", got)
	}
}

// These edits land after the response was applied but before sync.json is
// written. Delta and tombstone paths used to be excluded wholesale from the
// window-edit comparison, so the new bytes were recorded as already synced.
func TestKeepWindowEditsDirtyUsesTheAppliedHubState(t *testing.T) {
	tests := []struct {
		name    string
		current map[string]string
		sent    map[string]string
		deltas  []syncproto.Delta
		dropped []string
		want    map[string]string
	}{
		{
			name:    "edit after inbound upsert",
			current: map[string]string{"notes/a.md": contentHash([]byte("late local edit\n"))},
			sent:    map[string]string{"notes/a.md": contentHash([]byte("old\n"))},
			deltas: []syncproto.Delta{{
				Path: "notes/a.md", Op: "upsert", ContentB64: b64("hub version\n"),
			}},
			want: map[string]string{"notes/a.md": contentHash([]byte("hub version\n"))},
		},
		{
			name:    "recreate after tombstone removal",
			current: map[string]string{"notes/a.md": contentHash([]byte("late recreate\n"))},
			sent:    map[string]string{"notes/a.md": contentHash([]byte("old\n"))},
			dropped: []string{"notes/a.md"},
			want:    map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keepWindowEditsDirty(t.TempDir(), tc.current, tc.sent, tc.deltas, tc.dropped)
			if fmt.Sprint(tc.current) != fmt.Sprint(tc.want) {
				t.Fatalf("persisted hashes = %v, want hub state %v", tc.current, tc.want)
			}
		})
	}
}

// A conflict sibling is user data once it exists. A repeated or hostile response
// may name the same sibling again; different bytes must never overwrite it.
func TestWriteConflictSiblingsPreservesExistingDifferentSibling(t *testing.T) {
	vaultDir := t.TempDir()
	const note = "notes/a.md"
	const sibling = "notes/a.sync-conflict-20260827-alice-0123456789abcdef.md"
	write(t, vaultDir, note, "current losing note\n")
	write(t, vaultDir, sibling, "existing resolution work\n")

	unparked, err := writeConflictSiblings(vaultDir, []syncproto.Conflict{{
		Path: note, SiblingPath: sibling,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, vaultDir, sibling); got != "existing resolution work\n" {
		t.Fatalf("existing conflict sibling was overwritten: got %q", got)
	}
	if len(unparked) != 1 || unparked[0] != note {
		t.Fatalf("unparked = %v, want [%s] so the live losing note is protected too", unparked, note)
	}
}

func TestWriteConflictSiblingsDoesNotOverwriteConcurrentResolution(t *testing.T) {
	vaultDir := t.TempDir()
	const note = "notes/a.md"
	const sibling = "notes/a.sync-conflict-20260827-alice-fedcba9876543210.md"
	write(t, vaultDir, note, "current losing note\n")

	beforeConflictSiblingPublish = func(path string) {
		beforeConflictSiblingPublish = nil
		if err := os.WriteFile(path, []byte("resolution saved concurrently\n"), 0o644); err != nil {
			t.Errorf("concurrent resolution: %v", err)
		}
	}
	t.Cleanup(func() { beforeConflictSiblingPublish = nil })

	unparked, err := writeConflictSiblings(vaultDir, []syncproto.Conflict{{
		Path: note, SiblingPath: sibling,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, vaultDir, sibling); got != "resolution saved concurrently\n" {
		t.Fatalf("concurrent conflict resolution was overwritten: got %q", got)
	}
	if len(unparked) != 1 || unparked[0] != note {
		t.Fatalf("unparked = %v, want [%s]", unparked, note)
	}
}

// Guard-parked hub bytes use a deterministic conflict name. That name may
// already contain user-owned resolution work from an earlier attempt; parking a
// fresh inbound delta must never rename-overwrite it.
func TestApplyDeltasDoesNotOverwriteExistingProtectedSibling(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/a.md"
	old := []byte("old synced bytes\n")
	local := []byte("local window edit\n")
	incoming := []byte("incoming hub bytes\n")
	write(t, vaultDir, rel, string(local))

	canonical := merge.SiblingPath(rel, time.Now(), "hub", incoming)
	write(t, vaultDir, canonical, "user resolution already here\n")
	parked, err := applyDeltas(vaultDir, []syncproto.Delta{{
		Path: rel, Op: "upsert", ContentB64: base64.StdEncoding.EncodeToString(incoming),
	}}, map[string]string{rel: contentHash(old)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, vaultDir, canonical); got != "user resolution already here\n" {
		t.Fatalf("existing protected sibling was overwritten with %q", got)
	}
	if got := read(t, vaultDir, rel); got != string(local) {
		t.Fatalf("local edit was overwritten with %q", got)
	}
	if len(parked) != 1 || parked[0].sibling == canonical {
		t.Fatalf("incoming bytes were not parked under a collision-proof sibling: %+v", parked)
	}
	if got := read(t, vaultDir, parked[0].sibling); got != string(incoming) {
		t.Fatalf("parked incoming bytes = %q, want %q", got, incoming)
	}
}

// Joining hub B must not publish B's credentials until its metadata has passed
// validation. Otherwise a failed join leaves B credentials paired with hub A's
// sync base, and a later process restart can silently skip A-only notes on B.
func TestJoinFailureLeavesThePreviousHubPairRestartable(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/a.md"
	body := []byte("already synced to A\n")
	write(t, vaultDir, rel, string(body))

	var aSyncs int
	hubA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sync" {
			http.NotFound(w, r)
			return
		}
		aSyncs++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "head-a"})
	}))
	defer hubA.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: hubA.URL, Token: "token-a"}); err != nil {
		t.Fatal(err)
	}
	wantState := syncState{
		HeadSHA: "head-a", Hashes: map[string]string{rel: contentHash(body)},
		TombSeq: 41, VaultID: "vault-a", HubURL: hubA.URL,
	}
	if err := writeState(vaultDir, wantState); err != nil {
		t.Fatal(err)
	}

	var bSyncs int
	hubB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/join":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(syncproto.JoinResponse{
				ClientToken: "token-b", VaultID: "vault-b", User: "alice",
			})
		case "/v1/vault":
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
		case "/v1/sync":
			bSyncs++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "head-b"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer hubB.Close()

	if _, err := JoinVault(hubB.URL, "invite-b", vaultDir); err == nil {
		t.Fatal("JoinVault succeeded even though hub B metadata failed")
	}
	gotCreds, err := readCredentials(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	if gotCreds.HubURL != hubA.URL || gotCreds.Token != "token-a" {
		t.Fatalf("credentials after failed join = %+v, want the intact hub A credentials", gotCreds)
	}
	gotState := readState(vaultDir)
	if gotState.HeadSHA != wantState.HeadSHA || gotState.TombSeq != wantState.TombSeq ||
		gotState.VaultID != wantState.VaultID || gotState.HubURL != wantState.HubURL ||
		gotState.Hashes[rel] != wantState.Hashes[rel] {
		t.Fatalf("sync state after failed join = %+v, want unchanged %+v", gotState, wantState)
	}

	// Model a new process starting after the failed join.
	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatalf("restart sync: %v", err)
	}
	if aSyncs != 1 || bSyncs != 0 {
		t.Fatalf("restart contacted hub A %d time(s), hub B %d; want A=1 B=0", aSyncs, bSyncs)
	}
}

// Once the hub reports a vault ID, a legacy base with no ID cannot prove it
// belongs to that vault even when the URL is unchanged: the hub may have been
// rebuilt in place. Explicit re-join must choose the safe full reconcile.
func TestJoinTreatsLegacyUnidentifiedBaseAsUntrusted(t *testing.T) {
	legacy := syncState{
		HeadSHA: "old-head", Hashes: map[string]string{"notes/a.md": "old-hash"},
		TombSeq: 99, HubURL: "https://mesh.example",
	}
	if !joinTargetsAnotherVault(legacy, "https://mesh.example", "https://mesh.example", "new-vault-id") {
		t.Fatal("a legacy base without vault_id was trusted solely because the rebuilt hub kept its URL")
	}
}

// The two files that define a joined vault are crash-sensitive metadata, not
// disposable cache. Keep both on the same atomic+durable writer as sync.json.
func TestCredentialsUseTheAtomicDurableWriter(t *testing.T) {
	body := funcBody(t, "vault.go", "writeCredentials")
	if !strings.Contains(body, "writeFileAtomicPrivate(") {
		t.Fatal("writeCredentials does not use the private atomic writer; a crash can truncate or expose the bearer token")
	}
	if strings.Contains(body, "os.WriteFile(") {
		t.Fatal("writeCredentials still has a truncate-then-write path")
	}
}

func TestCredentialsRewriteRepairsWorldReadableMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root permission observations are unreliable")
	}
	vaultDir := t.TempDir()
	if err := writeCredentials(vaultDir, credentials{HubURL: "https://old.example", Token: "secret-old"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(credPath(vaultDir), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(vaultDir, credentials{HubURL: "https://new.example", Token: "secret-new"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(credPath(vaultDir))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("credentials mode = %04o, want 0600; an old permissive mode must not survive token rotation", got)
	}
}

// A durable sync.json must never get ahead of a non-durable directory deletion:
// after a power cut that would resurrect the note while the base says it is gone,
// and the next outbox would push the stale bytes back to the hub.
func TestEverySyncedDeleteUsesTheDurableRemove(t *testing.T) {
	body := funcBody(t, "vault.go", "removeFileDurable")
	if !strings.Contains(body, "captureGuardedFile(") || !strings.Contains(body, "discardGuardedFile(") {
		t.Fatalf("removeFileDurable must atomically capture and durably discard the checked inode; body:\n%s", body)
	}
	capture := funcBody(t, "vault.go", "captureGuardedFile")
	renameAt := strings.Index(capture, "os.Rename(")
	fileSyncAt := strings.Index(capture, ".Sync()")
	dirSyncAt := strings.Index(capture, "syncDir(")
	if fileSyncAt < 0 || renameAt < fileSyncAt || dirSyncAt < renameAt {
		t.Fatalf("captureGuardedFile must fsync the live inode before rename, then fsync the directory; body:\n%s", capture)
	}
	discard := funcBody(t, "vault.go", "discardGuardedFile")
	if removeAt, syncAt := strings.Index(discard, "os.Remove("), strings.Index(discard, "syncDir("); removeAt < 0 || syncAt < removeAt {
		t.Fatalf("discardGuardedFile must remove first and fsync its directory afterward; body:\n%s", discard)
	}
	for _, fn := range []string{"applyDeltas", "dropFullReconcileOrphans", "dropTombstoned"} {
		body := funcBody(t, "vault.go", fn)
		if !strings.Contains(body, "removeFileDurable(") {
			t.Errorf("%s bypasses removeFileDurable; sync state can survive while its deletion does not", fn)
		}
	}
}

// The hash check and pathname removal were separate operations. An editor that
// saves by atomic rename between them installs a new inode at the same path, and
// the sync then unlinks bytes it never checked.
func TestDropTombstonedDoesNotRemoveAtomicRenameAfterHashCheck(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/a.md"
	old := []byte("synced old bytes\n")
	late := []byte("editor atomic-save bytes\n")
	write(t, vaultDir, rel, string(old))

	beforeDurableRemove = func(path string) {
		beforeDurableRemove = nil
		tmp := filepath.Join(filepath.Dir(path), ".editor-save.md")
		if err := os.WriteFile(tmp, late, 0o644); err != nil {
			t.Errorf("stage editor save: %v", err)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Errorf("atomic editor save: %v", err)
		}
	}
	t.Cleanup(func() { beforeDurableRemove = nil })

	dropped, err := dropTombstoned(vaultDir, []string{rel}, map[string]string{rel: contentHash(old)})
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v; the path changed after the hash check", dropped)
	}
	if got := read(t, vaultDir, rel); got != string(late) {
		t.Fatalf("late editor bytes were lost: got %q, want %q", got, late)
	}
}

// An editor can also save after captureGuardedFile has opened and fsynced the
// old inode but before the guarded rename. The guard must detect the pathname's
// new identity, fsync that replacement while it is still live, and preserve it.
func TestDropTombstonedDoesNotRemoveAtomicRenameAfterGuardFsync(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/a.md"
	old := []byte("synced old bytes\n")
	late := []byte("editor save after guard fsync\n")
	write(t, vaultDir, rel, string(old))

	beforeGuardedRename = func(path string) {
		beforeGuardedRename = nil
		tmp := filepath.Join(filepath.Dir(path), ".editor-after-fsync.md")
		if err := os.WriteFile(tmp, late, 0o644); err != nil {
			t.Errorf("write editor temp: %v", err)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Errorf("atomic editor save: %v", err)
		}
	}
	t.Cleanup(func() { beforeGuardedRename = nil })

	dropped, err := dropTombstoned(vaultDir, []string{rel}, map[string]string{rel: contentHash(old)})
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v, want the late edit preserved", dropped)
	}
	if got := read(t, vaultDir, rel); got != string(late) {
		t.Fatalf("live note = %q, want late editor bytes %q", got, late)
	}
}

func TestWriteNewFileDurableNeverPublishesOnNoClobberFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes", "a.md")
	oldLink := linkFile
	linkFile = func(_, _ string) error { return fmt.Errorf("hard links unavailable") }
	t.Cleanup(func() { linkFile = oldLink })

	created, err := writeNewFileDurable(path, []byte("complete bytes\n"), 0o644)
	if err == nil || created {
		t.Fatalf("created=%v err=%v, want a safe failure", created, err)
	}
	if exists(dir, "notes/a.md") {
		t.Fatal("final markdown path was published despite no-clobber failure")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".mesh-new-") {
			t.Fatalf("staging file %s was not cleaned up", entry.Name())
		}
	}
}

// A failed hub response must leave the durable base untouched. A later process
// can then recompute and replay the identical outbox rather than silently
// treating bytes the hub never accepted as synced.
func TestSyncVaultFailedResponseReplaysOutboxAfterRestart(t *testing.T) {
	vaultDir := t.TempDir()
	const rel = "notes/a.md"
	old := []byte("old synced bytes\n")
	local := []byte("offline edit to retry\n")
	write(t, vaultDir, rel, string(local))

	var requests []syncproto.SyncRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req syncproto.SyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, req)
		if len(requests) == 1 {
			http.Error(w, "temporary hub failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "accepted-head", TombstoneSeq: 4})
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(vaultDir, syncState{
		HeadSHA: "base", Hashes: map[string]string{rel: contentHash(old)}, TombSeq: 3, HubURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncVault(vaultDir); err == nil {
		t.Fatal("503 sync unexpectedly succeeded")
	}
	failedState := readState(vaultDir)
	if failedState.HeadSHA != "base" || failedState.TombSeq != 3 || failedState.Hashes[rel] != contentHash(old) {
		t.Fatalf("failed response advanced durable state: %+v", failedState)
	}
	// Calling the exported operation again models restart: no in-memory outbox is
	// relied upon; it must be reproduced solely from disk plus sync.json.
	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if len(requests) != 2 || len(requests[0].Outbox) != 1 || len(requests[1].Outbox) != 1 {
		t.Fatalf("requests = %+v, want one replayed outbox item in each", requests)
	}
	first, second := requests[0].Outbox[0], requests[1].Outbox[0]
	if first != second || first.Path != rel || first.Op != "upsert" || first.ContentB64 != base64.StdEncoding.EncodeToString(local) {
		t.Fatalf("outbox was not replayed identically: first=%+v second=%+v", first, second)
	}
	st := readState(vaultDir)
	if st.HeadSHA != "accepted-head" || st.TombSeq != 4 || st.Hashes[rel] != contentHash(local) {
		t.Fatalf("successful retry did not persist the accepted base: %+v", st)
	}
}

// Same-vault rounds share sync.json and must be serialized. This exercises the
// exported operation, not merely the lock helper: without the lock both requests
// carry the same base and replay the same outbox item.
func TestSyncVaultSerializesConcurrentRoundsForOneVault(t *testing.T) {
	vaultDir := t.TempDir()
	write(t, vaultDir, "notes/a.md", "local\n")

	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(release)

	var mu sync.Mutex
	var requests []syncproto.SyncRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req syncproto.SyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		mu.Lock()
		requests = append(requests, req)
		n := len(requests)
		mu.Unlock()
		if n == 1 {
			close(firstEntered)
			<-releaseFirst
		} else if n == 2 {
			close(secondEntered)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: fmt.Sprintf("head-%d", n)})
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(vaultDir, syncState{HeadSHA: "base", Hashes: map[string]string{}, HubURL: srv.URL}); err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 2)
	go func() {
		_, err := SyncVault(vaultDir)
		errs <- err
	}()
	<-firstEntered
	go func() {
		_, err := SyncVault(vaultDir)
		errs <- err
	}()

	overlapped := false
	select {
	case <-secondEntered:
		overlapped = true
	case <-time.After(time.Second):
		// The second call is blocked behind the first; let the first finish so it
		// can persist head-1 before the second computes its request.
	}
	release()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if overlapped {
		t.Fatal("two SyncVault calls entered the hub concurrently for the same vault")
	}

	mu.Lock()
	got := append([]syncproto.SyncRequest(nil), requests...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2", len(got))
	}
	if got[0].BaseSHA != "base" || len(got[0].Outbox) != 1 {
		t.Fatalf("first request = %+v, want base and one upsert", got[0])
	}
	if got[1].BaseSHA != "head-1" || len(got[1].Outbox) != 0 {
		t.Fatalf("second request = %+v, want head-1 and no replayed outbox", got[1])
	}
}

// An empty/unreadable state is not proof that every local path is new. If the
// client pushes before learning the hub's tombstones, a stale offline copy is
// accepted as a fresh upsert and clears the delete marker. Pull first, quarantine
// tombstoned unknown bytes, then push only genuine survivors.
func TestSyncVaultUnknownBaseDoesNotResurrectTombstones(t *testing.T) {
	for _, stateKind := range []string{"missing", "corrupt"} {
		t.Run(stateKind, func(t *testing.T) {
			vaultDir := t.TempDir()
			write(t, vaultDir, "notes/deleted.md", "stale deleted bytes\n")
			write(t, vaultDir, "notes/local.md", "genuinely local note\n")

			var requests []syncproto.SyncRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req syncproto.SyncRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				requests = append(requests, req)
				resp := syncproto.SyncResponse{HeadSHA: fmt.Sprintf("head-%d", len(requests))}
				if len(requests) == 1 {
					resp.FullReconcile = true
					resp.Tombstones = []string{"notes/deleted.md"}
					resp.TombstoneSeq = 7
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()
			if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t", VaultID: "vault"}); err != nil {
				t.Fatal(err)
			}
			if stateKind == "corrupt" {
				if err := os.WriteFile(statePath(vaultDir), []byte(`{"head_sha":`), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			sum, err := SyncVault(vaultDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 {
				t.Fatalf("requests = %d, want pull-first plus survivor push", len(requests))
			}
			if len(requests[0].Outbox) != 0 {
				t.Fatalf("pull-first outbox = %+v; stale tombstoned bytes reached the hub before its delete ledger", requests[0].Outbox)
			}
			if len(requests[1].Outbox) != 1 || requests[1].Outbox[0].Path != "notes/local.md" || requests[1].Outbox[0].Op != "upsert" {
				t.Fatalf("phase-two outbox = %+v, want only notes/local.md", requests[1].Outbox)
			}
			if exists(vaultDir, "notes/deleted.md") {
				t.Fatal("tombstoned note remained live and can auto-resurrect")
			}
			siblings, err := filepath.Glob(filepath.Join(vaultDir, "notes", "deleted.sync-conflict-*.md"))
			if err != nil {
				t.Fatal(err)
			}
			if len(siblings) != 1 {
				t.Fatalf("tombstoned bytes were not preserved in exactly one conflict sibling: %v", siblings)
			}
			if b, err := os.ReadFile(siblings[0]); err != nil || string(b) != "stale deleted bytes\n" {
				t.Fatalf("quarantined bytes = %q, err=%v", b, err)
			}
			if !exists(vaultDir, "notes/local.md") {
				t.Fatal("fresh non-tombstoned local note was discarded")
			}
			if len(sum.Protected) == 0 {
				t.Fatalf("quarantine was not surfaced in Summary: %+v", sum)
			}
		})
	}
}

// Quarantine is not a one-time pathname check. An editor can atomically save a
// stale buffer back to the original path between phase one and the survivor
// push; phase two must remember the authoritative tombstone and never upload it.
func TestSyncVaultUnknownBaseDoesNotResurrectTombstoneSavedBetweenPhases(t *testing.T) {
	vaultDir := t.TempDir()
	write(t, vaultDir, "notes/deleted.md", "stale first copy\n")
	write(t, vaultDir, "notes/local.md", "genuinely local note\n")

	var requests []syncproto.SyncRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req syncproto.SyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, req)
		resp := syncproto.SyncResponse{HeadSHA: fmt.Sprintf("head-%d", len(requests))}
		if len(requests) == 1 {
			resp.FullReconcile = true
			resp.Tombstones = []string{"notes/deleted.md"}
			resp.TombstoneSeq = 7
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t", VaultID: "vault"}); err != nil {
		t.Fatal(err)
	}

	oldHook := afterUntrustedTombstoneQuarantine
	afterUntrustedTombstoneQuarantine = func() {
		write(t, vaultDir, "notes/deleted.md", "editor saved stale buffer between phases\n")
	}
	t.Cleanup(func() { afterUntrustedTombstoneQuarantine = oldHook })

	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want pull-first plus survivor push", len(requests))
	}
	for _, item := range requests[1].Outbox {
		if item.Path == "notes/deleted.md" {
			t.Fatalf("phase two resurrected the just-learned tombstone: %+v", requests[1].Outbox)
		}
	}
	if exists(vaultDir, "notes/deleted.md") {
		t.Fatal("between-phase stale save remained at the live sync path")
	}
	siblings, err := filepath.Glob(filepath.Join(vaultDir, "notes", "deleted.sync-conflict-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 2 {
		t.Fatalf("both stale copies must be preserved in quarantine siblings: %v", siblings)
	}
}
