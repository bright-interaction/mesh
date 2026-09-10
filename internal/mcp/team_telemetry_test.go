// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bright-interaction/mesh/internal/teamtelemetry"
)

func TestLocalTeamTelemetryCountsOnlyTrustedFetchesOfWritebacks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "decisions", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("agent.md", "---\nid: agent-note\ntype: decision\nsource: agent\n---\n# Agent handoff\n")
	write("manual.md", "---\nid: manual-note\ntype: note\nsource: human\n---\n# Manual reference\n")
	seedIndex(t, root)
	if err := os.WriteFile(filepath.Join(root, ".mesh", "credentials"), []byte(`{"vault_id":"team-a"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	fetch := func(ctx context.Context, id string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"id": id})
		if _, rerr := srv.toolFetch(ctx, raw); rerr != nil {
			t.Fatalf("fetch %s: %v", id, rerr)
		}
	}

	// A bare/shared HTTP context cannot identify the human reader, and a manual
	// reference is not a write-back. Neither may become team flywheel evidence.
	fetch(context.Background(), "agent-note")
	fetch(WithLocalOperator(context.Background()), "manual-note")
	if got, err := teamtelemetry.Pending(root, "team-a", 10); err != nil || len(got) != 0 {
		t.Fatalf("ineligible fetches queued events: got=%v err=%v", got, err)
	}

	fetch(WithLocalOperator(context.Background()), "agent-note")
	got, err := teamtelemetry.Pending(root, "team-a", 10)
	if err != nil || len(got) != 1 || got[0].NoteID != "agent-note" {
		t.Fatalf("trusted write-back fetch event = %v, err=%v", got, err)
	}
}
