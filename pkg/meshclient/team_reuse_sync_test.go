// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/teamtelemetry"
)

func TestSyncHydratesLegacyJoinBeforeScopingTeamReuse(t *testing.T) {
	var syncReq syncproto.SyncRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vault":
			_ = json.NewEncoder(w).Encode(syncproto.VaultInfo{VaultID: "team-stable", HeadSHA: "hub-head"})
		case "/v1/sync":
			if err := json.NewDecoder(r.Body).Decode(&syncReq); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "hub-head"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(root, credentials{HubURL: ts.URL, Token: "legacy-token"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(root, syncState{HeadSHA: "legacy-head", Hashes: map[string]string{}, HubURL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncVault(root); err != nil {
		t.Fatal(err)
	}
	if syncReq.BaseSHA != "" {
		t.Fatalf("legacy unidentified base was trusted after identity appeared: %q", syncReq.BaseSHA)
	}
	creds, err := readCredentials(root)
	if err != nil || creds.VaultID != "team-stable" || creds.Token != "legacy-token" {
		t.Fatalf("hydrated credentials = %+v, err=%v", creds, err)
	}
	state := readState(root)
	if state.VaultID != "team-stable" || state.HubURL != ts.URL {
		t.Fatalf("hydrated state = %+v", state)
	}
}

func TestSyncUploadsAndDeletesOnlyAcknowledgedReuseEvents(t *testing.T) {
	var got syncproto.SyncRequest
	ack := true
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		resp := syncproto.SyncResponse{HeadSHA: "base"}
		if ack && len(got.ReuseEvents) == 1 {
			resp.AckedReuseEventIDs = []string{got.ReuseEvents[0].EventID}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	root := t.TempDir()
	if err := writeCredentials(root, credentials{HubURL: ts.URL, Token: "token", VaultID: "team-a"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(root, syncState{HeadSHA: "base", Hashes: map[string]string{}, HubURL: ts.URL, VaultID: "team-a"}); err != nil {
		t.Fatal(err)
	}
	if err := teamtelemetry.RecordForJoinedVault(root, "n1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncVault(root); err != nil {
		t.Fatal(err)
	}
	if len(got.ReuseEvents) != 1 || got.ReuseEvents[0].NoteID != "n1" {
		t.Fatalf("uploaded events = %+v", got.ReuseEvents)
	}
	if pending, _ := teamtelemetry.Pending(root, "team-a", 10); len(pending) != 0 {
		t.Fatalf("acknowledged event remained: %+v", pending)
	}

	ack = false // an old hub ignores the additive request field and sends no ack
	if err := teamtelemetry.RecordForJoinedVault(root, "n2", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncVault(root); err != nil {
		t.Fatal(err)
	}
	if pending, _ := teamtelemetry.Pending(root, "team-a", 10); len(pending) != 1 || pending[0].NoteID != "n2" {
		t.Fatalf("unacknowledged event was discarded: %+v", pending)
	}
}
