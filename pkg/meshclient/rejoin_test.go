// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

// scriptedHub answers the three routes JoinVault drives and records every outbox it
// receives. vaultID identifies the vault it claims to serve.
//
// Scripted rather than a real hub, because this test is about which BASE the client
// pushes against, which is entirely a client-side decision, and because the file has to
// stay importable without the pro hub package.
func scriptedHub(t *testing.T, vaultID string, rounds *[][]outboxRecord) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/join":
			_ = json.NewEncoder(w).Encode(syncproto.JoinResponse{
				ClientToken: "mesh_client_" + vaultID, User: "tom", VaultID: vaultID,
			})
		case "/v1/vault":
			_ = json.NewEncoder(w).Encode(syncproto.VaultInfo{
				VaultID: vaultID, HeadSHA: "head-" + vaultID, MeshToml: "[embedding]\n",
			})
		case "/v1/sync":
			var req syncproto.SyncRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode sync request: %v", err)
				return
			}
			items := make([]outboxRecord, 0, len(req.Outbox))
			for _, it := range req.Outbox {
				items = append(items, outboxRecord{path: it.Path, op: it.Op})
			}
			*rounds = append(*rounds, items)
			// Echo the client's delete floor back, which is what the real hub does
			// (internal/hub/sync.go: the high-water mark starts at the client's own seq
			// and is only raised by tombstones the client was actually sent). That echo
			// is why a carried-over TombSeq does not self-heal.
			_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{
				HeadSHA: "head-" + vaultID, FullReconcile: true, TombstoneSeq: req.TombstoneSeq,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestJoinAnotherHubResetsTheSyncBase pins the one thing a re-join must do to the
// vault's sync state.
//
// JoinVault overwrote .mesh/credentials and never touched .mesh/sync.json, so hub A's
// hash map stayed in place as the push baseline for hub B. computeOutbox diffs the disk
// against that map, so every note not edited since the last sync with A hashed equal and
// was skipped; the full reconcile kept the local files, and the recompute wrote the same
// hashes back as the new base. The notes were recorded as synced and never sent, and it
// does not self-heal, because nothing re-dirties a path both sides believe is settled.
// The output read "joined and cloned" then "pushed 0, pulled 0" over a vault the new hub
// had never seen a byte of. Reachable on a rebuilt hub, a hub migration, or a hub.db
// re-bootstrap, doing exactly what the docs say.
//
// TombSeq has the same shape and is worse: the hub echoes the client's floor back
// (internal/hub/sync.go), so hub B's own deletes stay invisible until its delete
// sequence passes hub A's. It is asserted here too.
func TestJoinAnotherHubResetsTheSyncBase(t *testing.T) {
	vaultDir := t.TempDir()
	write(t, vaultDir, "notes/alpha.md", "# alpha\n\nwritten once, synced to hub A\n")
	write(t, vaultDir, "notes/beta.md", "# beta\n\nalso synced to hub A\n")

	var roundsA [][]outboxRecord
	hubA := scriptedHub(t, "vault-a", &roundsA)
	if _, err := JoinVault(hubA.URL, "invite-a", vaultDir); err != nil {
		t.Fatalf("join hub A: %v", err)
	}
	if got := len(roundsA); got != 1 || len(roundsA[0]) != 2 {
		t.Fatalf("join to hub A pushed %v, want one round of 2 notes", sizes(roundsA))
	}
	// Give the old state a delete high-water mark, the way a real hub with a delete
	// ledger would have. Hub B's ledger starts at 0, so carrying this over hides every
	// delete B has ever recorded.
	st := readState(vaultDir)
	st.TombSeq = 42
	if err := writeState(vaultDir, st); err != nil {
		t.Fatal(err)
	}

	// Same directory, same notes, different hub. Nothing on disk changed, which is the
	// point: this is a hub migration, not an edit.
	var roundsB [][]outboxRecord
	hubB := scriptedHub(t, "vault-b", &roundsB)
	if _, err := JoinVault(hubB.URL, "invite-b", vaultDir); err != nil {
		t.Fatalf("join hub B: %v", err)
	}
	if len(roundsB) != 1 {
		t.Fatalf("join to hub B ran %d sync rounds, want 1", len(roundsB))
	}
	sent := map[string]string{}
	for _, it := range roundsB[0] {
		sent[it.path] = it.op
	}
	for _, rel := range []string{"notes/alpha.md", "notes/beta.md"} {
		if op, ok := sent[rel]; !ok {
			t.Errorf("%s was never pushed to the new hub; hub A's hash map was still the push "+
				"baseline, so the note is recorded as synced somewhere it has never existed", rel)
		} else if op != "upsert" {
			t.Errorf("%s pushed as %q, want upsert", rel, op)
		}
	}

	after := readState(vaultDir)
	if after.VaultID != "vault-b" {
		t.Errorf("sync.json vault_id = %q, want %q; without it the next re-join cannot tell "+
			"a rebuilt hub from the one this base describes", after.VaultID, "vault-b")
	}
	if after.HubURL != hubB.URL {
		t.Errorf("sync.json hub_url = %q, want %q", after.HubURL, hubB.URL)
	}
	if after.TombSeq != 0 {
		t.Errorf("sync.json tomb_seq = %d after switching hubs, want 0; hub A's delete "+
			"high-water mark hides every delete hub B has already recorded", after.TombSeq)
	}
}

// TestReJoiningTheSameHubKeepsTheSyncBase is the other side of the gate, and the reason
// the reset is conditional rather than unconditional. Re-joining the SAME vault (a fresh
// invite after a token rotation) must not throw the base away: a zero base re-pushes
// every note in the vault, which on a shared hub is a mass of pointless merges and,
// where a note diverged in the meantime, conflict siblings the user never caused.
func TestReJoiningTheSameHubKeepsTheSyncBase(t *testing.T) {
	vaultDir := t.TempDir()
	write(t, vaultDir, "notes/alpha.md", "# alpha\n\nsynced already\n")

	var rounds [][]outboxRecord
	hub := scriptedHub(t, "vault-a", &rounds)
	if _, err := JoinVault(hub.URL, "invite-1", vaultDir); err != nil {
		t.Fatalf("first join: %v", err)
	}
	if _, err := JoinVault(hub.URL, "invite-2", vaultDir); err != nil {
		t.Fatalf("second join: %v", err)
	}
	if len(rounds) != 2 {
		t.Fatalf("ran %d sync rounds, want 2", len(rounds))
	}
	if len(rounds[1]) != 0 {
		t.Errorf("re-joining the same vault re-pushed %v; an unchanged note must stay "+
			"unchanged, or every token rotation floods the hub", rounds[1])
	}
}
