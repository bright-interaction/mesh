// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package teamtelemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCred(t *testing.T, root, vaultID string) {
	t.Helper()
	dir := filepath.Join(root, ".mesh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"hub_url":"https://hub.invalid","token":"top-secret","vault_id":"` + vaultID + `"}`
	if err := os.WriteFile(filepath.Join(dir, "credentials"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateSpoolIsContentFreeScopedAndAcked(t *testing.T) {
	root := t.TempDir()
	writeCred(t, root, "team-a")
	when := time.Unix(1_800_000_000, 0)
	if err := RecordForJoinedVault(root, "decision-1", when); err != nil {
		t.Fatal(err)
	}
	events, err := Pending(root, "team-a", 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("pending = %+v, %v", events, err)
	}
	if events[0].NoteID != "decision-1" || events[0].FetchedAt != when.Unix() {
		t.Fatalf("event = %+v", events[0])
	}
	dir := scopeDir(root, "team-a")
	if mode := mustStat(t, dir).Mode().Perm(); mode != 0o700 {
		t.Fatalf("outbox mode = %o", mode)
	}
	b, err := os.ReadFile(filepath.Join(dir, events[0].EventID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"top-secret", "hub.invalid", "team-a", "query", "body", "user", "email"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("spool leaked %q: %s", forbidden, b)
		}
	}
	if mode := mustStat(t, filepath.Join(dir, events[0].EventID+".json")).Mode().Perm(); mode != 0o600 {
		t.Fatalf("event mode = %o", mode)
	}

	// Rejoining scopes future uploads to the new vault id; old events never cross.
	writeCred(t, root, "team-b")
	if got, err := Pending(root, "team-b", 10); err != nil || len(got) != 0 {
		t.Fatalf("new team inherited old events: %+v, %v", got, err)
	}
	if err := Ack(root, "team-a", []string{events[0].EventID}); err != nil {
		t.Fatal(err)
	}
	if got, _ := Pending(root, "team-a", 10); len(got) != 0 {
		t.Fatalf("ack left events: %+v", got)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}
