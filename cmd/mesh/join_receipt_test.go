// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

// fakeHub answers the three routes JoinVault drives, with a scripted sync response.
// A scripted hub rather than a real one on purpose: this test is about what the JOIN
// COMMAND does with a Summary, so the round has to be able to carry a conflict and a
// refusal on demand, and the file has to stay importable without the pro hub.
func fakeHub(t *testing.T, sync syncproto.SyncResponse) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/join":
			_ = json.NewEncoder(w).Encode(syncproto.JoinResponse{
				ClientToken: "mesh_client_faketoken", User: "stranger", VaultID: "vault-one",
			})
		case "/v1/vault":
			_ = json.NewEncoder(w).Encode(syncproto.VaultInfo{
				VaultID: "vault-one", HeadSHA: sync.HeadSHA, MeshToml: "[embedding]\n",
			})
		case "/v1/sync":
			_ = json.NewEncoder(w).Encode(sync)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestJoinReceiptNamesConflictsAndRefusals is the `mesh join` half of the shared-receipt
// rule, and it is written from the newcomer's path because that is where it hurt.
//
// `mesh init ~/notes`, write an index.md, then `mesh join <hub> <invite> ~/notes`. The
// join ends in a sync round: the hub's index.md wins, the local bytes are parked in a
// .sync-conflict sibling, and anything the hub refuses is kept local and stays dirty.
// joinCmd read Head and Pulled off the Summary and printed neither fact, so the whole
// receipt was "joined and cloned ... 5 files pulled" over a file the user had just lost
// sight of. Every field asserted here was already in the Summary; the command simply
// dropped them on the floor.
func TestJoinReceiptNamesConflictsAndRefusals(t *testing.T) {
	vaultDir := t.TempDir()
	const mine = "index.md"
	if err := os.WriteFile(filepath.Join(vaultDir, mine), []byte("# my notes\n\nmy own index\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const sibling = "index.sync-conflict-20260813-stranger-0123456789abcdef.md"
	const refused = "team/private.md"
	srv := fakeHub(t, syncproto.SyncResponse{
		HeadSHA: "abcdef1234567890",
		Deltas: []syncproto.Delta{{
			Path: mine, Op: "upsert",
			ContentB64: base64.StdEncoding.EncodeToString([]byte("# team index\n\nthe hub version\n")),
		}},
		Conflicts: []syncproto.Conflict{{Path: mine, SiblingPath: sibling}},
		Rejected:  []string{refused},
	})

	out, err := capture(t, func() error {
		c := joinCmd()
		c.SetArgs([]string{srv.URL, "invite-token", vaultDir})
		return c.Execute()
	})
	if err != nil {
		t.Fatalf("mesh join: %v\n%s", err, out)
	}

	for _, want := range []string{
		"joined and cloned",
		"synced: pushed",             // the shared headline, so the round is reported at all
		"conflict: hub version kept", // the user's own index.md lost
		sibling,                      // and this is where their bytes went
		"rejected by hub",            // the hub refused a path
		refused,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`mesh join` receipt never mentions %q, so the user is not told what the "+
				"join round did to their notes:\n%s", want, out)
		}
	}
}
