// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/pkg/meshclient"
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
			var req syncproto.SyncRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode sync request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			resp := sync
			if len(req.Outbox) == 0 {
				// Join's unknown-base recovery is pull-first. Conflicts and
				// rejections acknowledge pushed paths, so script them only on the
				// survivor phase where those paths were actually transmitted.
				resp.Deltas = nil
				resp.Conflicts = nil
				resp.Rejected = nil
				resp.FullReconcile = true
			} else {
				sent := make(map[string]bool, len(req.Outbox))
				for _, item := range req.Outbox {
					sent[item.Path] = true
				}
				for _, rejected := range resp.Rejected {
					if !sent[rejected] {
						t.Errorf("fake hub rejected %q, which was not in outbox %+v", rejected, req.Outbox)
						http.Error(w, "invalid scripted rejection", http.StatusInternalServerError)
						return
					}
				}
			}
			_ = json.NewEncoder(w).Encode(resp)
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
	if err := os.MkdirAll(filepath.Join(vaultDir, "team"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vaultDir, filepath.FromSlash(refused)), []byte("private local note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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

func TestJoinPrintsDurableReceiptBeforeReportingIndexFailure(t *testing.T) {
	indexErr := errors.New("owner did not catch up")
	out, err := capture(t, func() error {
		return finishJoin(context.Background(), "/joined-vault", meshclient.Summary{
			Head: "abcdef1234567890", Pulled: 2,
		}, func(context.Context, string, string) error { return indexErr })
	})
	if !errors.Is(err, indexErr) {
		t.Fatalf("finishJoin error = %v, want index failure", err)
	}
	receiptAt := strings.Index(out, "joined and cloned")
	staleAt := strings.Index(out, "index stale")
	if receiptAt < 0 || staleAt < 0 || receiptAt > staleAt {
		t.Fatalf("durable join receipt must precede index-liveness error:\n%s", out)
	}
	if !strings.Contains(out, "synced:") || !strings.Contains(out, "the invite was redeemed") {
		t.Fatalf("receipt does not distinguish completed join from stale index:\n%s", out)
	}
}
