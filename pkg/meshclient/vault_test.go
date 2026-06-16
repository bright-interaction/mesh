package meshclient

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// TestApplyDeltasExternalEditorGuard: a local edit made after the outbox was
// computed must not be clobbered; the incoming hub version is parked.
func TestApplyDeltasExternalEditorGuard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.md"), []byte("local unsynced edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := map[string]string{"x.md": contentHash([]byte("what we sent\n"))} // disk != sent => changed since send
	deltas := []syncproto.Delta{{Path: "x.md", Op: "upsert", ContentB64: b64("hub version\n")}}

	parked, err := applyDeltas(dir, deltas, sent)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "x.md")); string(got) != "local unsynced edit\n" {
		t.Errorf("local edit must be kept, got %q", got)
	}
	if len(parked) != 1 || parked[0].note != "x.md" {
		t.Fatalf("expected the incoming version parked for x.md, got %v", parked)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, parked[0].sibling)); string(got) != "hub version\n" {
		t.Errorf("incoming hub version must be parked in the sibling, got %q", got)
	}
}

// TestApplyDeltasDeleteRaceNoResurrect: a file deleted locally during the sync
// window must NOT be resurrected by an incoming upsert (the CRITICAL finding).
func TestApplyDeltasDeleteRaceNoResurrect(t *testing.T) {
	dir := t.TempDir()
	// File is absent on disk (user deleted it after the outbox was computed) but
	// was present at send time.
	sent := map[string]string{"x.md": contentHash([]byte("was here at send\n"))}
	deltas := []syncproto.Delta{{Path: "x.md", Op: "upsert", ContentB64: b64("hub version\n")}}

	parked, err := applyDeltas(dir, deltas, sent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.md")); err == nil {
		t.Error("a locally-deleted file must not be resurrected by an incoming upsert")
	}
	if len(parked) != 1 || parked[0].note != "x.md" {
		t.Fatalf("expected the incoming version parked, got %v", parked)
	}
}

// TestKeepParkedDirty: a parked path must be reset to its pre-sync base hash so
// the next outbox re-pushes the kept local change (no silent divergence).
func TestKeepParkedDirty(t *testing.T) {
	base := map[string]string{"edited.md": "BASE", "deleted.md": "BASE"}
	current := map[string]string{"edited.md": "DISKEDIT", "deleted.md": "SHOULD-NOT-MATTER", "new.md": "X"}
	parked := []park{{note: "edited.md", sibling: "edited.sync-conflict.md"}, {note: "gone.md", sibling: "gone.sync-conflict.md"}}

	keepParkedDirty(current, base, parked)

	if current["edited.md"] != "BASE" {
		t.Errorf("parked edited.md must revert to base hash, got %q", current["edited.md"])
	}
	if _, ok := current["gone.md"]; ok {
		t.Error("a parked path absent from base must be removed from current (so it re-pushes as new/delete)")
	}
	if current["new.md"] != "X" {
		t.Error("unrelated paths must be untouched")
	}
}

// TestApplyDeltasNormalOverwrite: a file unchanged since send takes the hub
// version (no parking).
func TestApplyDeltasNormalOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.md"), []byte("as sent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := map[string]string{"x.md": contentHash([]byte("as sent\n"))}
	deltas := []syncproto.Delta{{Path: "x.md", Op: "upsert", ContentB64: b64("hub version\n")}}

	parked, err := applyDeltas(dir, deltas, sent)
	if err != nil {
		t.Fatal(err)
	}
	if len(parked) != 0 {
		t.Errorf("no parking expected for an unchanged-since-send file, got %v", parked)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "x.md")); string(got) != "hub version\n" {
		t.Errorf("expected the hub version, got %q", got)
	}
}

// TestApplyDeltasDeleteGuard: a locally re-edited file is not deleted by an
// incoming delete delta.
func TestApplyDeltasDeleteGuard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.md"), []byte("re-created locally\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := map[string]string{"x.md": contentHash([]byte("old\n"))} // changed since send
	deltas := []syncproto.Delta{{Path: "x.md", Op: "delete"}}

	if _, err := applyDeltas(dir, deltas, sent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.md")); err != nil {
		t.Error("a locally re-edited file must survive an incoming delete")
	}
}
