// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/syncproto"
)

func TestPreflightMatchesHubContentGate(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
	}{
		{"empty", "", ""},
		{"limit", strings.Repeat("a", merge.MaxNoteBytes), ""},
		{"oversize", strings.Repeat("a", merge.MaxNoteBytes+1), "1048576"},
		{"literal escape", `example: \u0000`, ""},
		{"NUL", "example: \x00", "NUL"},
		// Preserve the existing hub contract; this sprint does not invent a new
		// UTF-8 or YAML policy and cannot reject something its shared gate accepts.
		{"other bytes", "\xff\a\v\x1b", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := []syncproto.OutboxItem{{Path: "note.md", Op: "upsert", ContentB64: base64.StdEncoding.EncodeToString([]byte(tc.body))}}
			kept, blocked, err := preflightOutbox(items)
			if err != nil {
				t.Fatal(err)
			}
			if merge.IsText([]byte(tc.body)) != (len(kept) == 1) {
				t.Fatal("client/hub gate drift")
			}
			if tc.reason == "" {
				if len(kept) != 1 || len(blocked) != 0 {
					t.Fatalf("valid content blocked: %+v", blocked)
				}
			} else if len(kept) != 0 || len(blocked) != 1 || !strings.Contains(blocked[0].Reason, tc.reason) || blocked[0].Hash != contentHash([]byte(tc.body)) {
				t.Fatalf("wrong block: %+v", blocked)
			}
		})
	}
	kept, blocked, err := preflightOutbox([]syncproto.OutboxItem{{Path: "old.md", Op: "delete"}})
	if err != nil || len(kept) != 1 || len(blocked) != 0 {
		t.Fatal("delete filtered")
	}
	if _, _, err := preflightOutbox([]syncproto.OutboxItem{{Path: "bad.md", Op: "upsert", ContentB64: "!"}}); err == nil {
		t.Fatal("invalid internal encoding accepted")
	}
}

func TestBlockedContentNeverUploadsAndCorrectionResumes(t *testing.T) {
	for _, initialHead := range []string{"known-base", ""} {
		t.Run("base="+initialHead, func(t *testing.T) {
			root := t.TempDir()
			const invalid = "draft\x00example"
			write(t, root, "bad.md", invalid)
			write(t, root, "good.md", "good")
			requests := 0
			goodSent, fixedSent := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req syncproto.SyncRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				requests++
				for _, it := range req.Outbox {
					body, _ := base64.StdEncoding.DecodeString(it.ContentB64)
					if it.Path == "bad.md" {
						if string(body) != "fixed" {
							t.Errorf("invalid content reached hub: %q", body)
						}
						fixedSent++
					} else if it.Path == "good.md" {
						goodSent++
					}
				}
				_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "head", FullReconcile: req.BaseSHA == ""})
			}))
			defer srv.Close()
			if err := writeCredentials(root, credentials{HubURL: srv.URL, Token: "test", VaultID: "vault"}); err != nil {
				t.Fatal(err)
			}
			if err := writeState(root, syncState{HeadSHA: initialHead, Hashes: map[string]string{}, HubURL: srv.URL, VaultID: "vault"}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				got, err := SyncVault(root)
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Blocked) != 1 || got.Blocked[0].Path != "bad.md" || got.Remaining != 0 || len(got.Rejected) != 0 {
					t.Fatalf("wrong receipt: %+v", got)
				}
				if readState(root).Hashes["bad.md"] != "" || read(t, root, "bad.md") != invalid {
					t.Fatal("blocked note baselined or changed")
				}
			}
			if goodSent != 1 || fixedSent != 0 {
				t.Fatalf("uploads good=%d bad=%d", goodSent, fixedSent)
			}
			write(t, root, "bad.md", "fixed")
			got, err := SyncVault(root)
			if err != nil || len(got.Blocked) != 0 || fixedSent != 1 || got.Pushed != 1 {
				t.Fatalf("correction did not resume: %+v %v", got, err)
			}
			if readState(root).Hashes["bad.md"] != contentHash([]byte("fixed")) {
				t.Fatal("accepted correction not baselined")
			}
			wantRequests := 4
			if initialHead == "" {
				wantRequests++
			}
			if requests != wantRequests {
				t.Fatalf("unexpected extra rounds: %d", requests)
			}
		})
	}
}

func TestBlockedEditSurvivesInboundUpsertAndDelete(t *testing.T) {
	for _, op := range []string{"upsert", "delete"} {
		t.Run(op, func(t *testing.T) {
			root := t.TempDir()
			const local = "irreplaceable\x00draft"
			write(t, root, "note.md", local)
			oldHash := contentHash([]byte("old"))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req syncproto.SyncRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if len(req.Outbox) != 0 {
					t.Error("blocked edit transmitted")
				}
				d := syncproto.Delta{Path: "note.md", Op: op}
				if op == "upsert" {
					d.ContentB64 = base64.StdEncoding.EncodeToString([]byte("hub"))
				}
				_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "new", Deltas: []syncproto.Delta{d}})
			}))
			defer srv.Close()
			if err := writeCredentials(root, credentials{HubURL: srv.URL, Token: "t", VaultID: "v"}); err != nil {
				t.Fatal(err)
			}
			if err := writeState(root, syncState{HeadSHA: "old", Hashes: map[string]string{"note.md": oldHash}, HubURL: srv.URL, VaultID: "v"}); err != nil {
				t.Fatal(err)
			}
			got, err := SyncVault(root)
			if err != nil {
				t.Fatal(err)
			}
			if read(t, root, "note.md") != local || readState(root).Hashes["note.md"] != oldHash || len(got.Blocked) != 1 {
				t.Fatalf("blocked edit lost/baselined: %+v", got)
			}
			if op == "upsert" && (len(got.Protected) != 1 || read(t, root, got.Protected[0]) != "hub") {
				t.Fatal("hub version not preserved")
			}
		})
	}
}
