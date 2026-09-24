// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func tuneCommandFixture(t *testing.T, brokenFTS bool) (string, string) {
	t.Helper()
	// No provider/model/network calls, irrespective of the developer's environment.
	t.Setenv("MESH_EMBED_ENDPOINT", "")
	t.Setenv("MESH_EMBED_MODEL", "")
	t.Setenv("MESH_RERANK_AGENT", "http")
	t.Setenv("MESH_RERANK_ENDPOINT", "")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.md"), []byte("---\nid: tune-fixture\ntype: note\ntitle: Storage fixture\n---\nSQLite storage.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := index.Reindex(store, root); err != nil {
		t.Fatal(err)
	}
	if brokenFTS {
		// FTS accepts NULL payloads, but Search must refuse a hit whose title
		// cannot be scanned. Schema admission and graph setup remain valid.
		if err := store.Write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE search_index SET title=NULL WHERE node_id='note:tune-fixture'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	cases := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(cases, []byte(`[{"query":"sqlite storage","relevant":["tune-fixture"]}]`), 0600); err != nil {
		t.Fatal(err)
	}
	return root, cases
}

func TestTuneCommandRetrievalFailureHasNoVerdict(t *testing.T) {
	root, cases := tuneCommandFixture(t, true)
	cmd := tuneCmd()
	cmd.SetArgs([]string{cases, "--test", cases, "--vault", root, "--step", "0.5"})
	cmd.SetErr(io.Discard)
	cmd.SetOut(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	out, err := captureStdout(t, cmd.Execute)
	if err == nil {
		t.Fatalf("broken retrieval produced a successful tuning result: %s", out)
	}
	if !strings.Contains(err.Error(), "tune candidate") || !strings.Contains(err.Error(), "train case 1") {
		t.Fatalf("failure did not come from tuning retrieval: %v", err)
	}
	if strings.Contains(out, "VERDICT") || strings.Contains(out, "export MESH_WEIGHT") {
		t.Fatalf("failed tuning printed an actionable verdict: %s", out)
	}
}

func TestTuneCommandCancellationHasNoVerdict(t *testing.T) {
	root, cases := tuneCommandFixture(t, false)
	cmd := tuneCmd()
	cmd.SetArgs([]string{cases, "--test", cases, "--vault", root, "--step", "0.5"})
	cmd.SetErr(io.Discard)
	cmd.SetOut(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := captureStdout(t, func() error { return cmd.ExecuteContext(ctx) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled tuning returned %v; output %s", err, out)
	}
	if strings.Contains(out, "VERDICT") {
		t.Fatalf("cancelled tuning printed a verdict: %s", out)
	}
}

func TestTuneCommandSuccessfulFixtureStillReportsTie(t *testing.T) {
	root, cases := tuneCommandFixture(t, false)
	cmd := tuneCmd()
	cmd.SetArgs([]string{cases, "--test", cases, "--vault", root, "--step", "0.5"})
	cmd.SetErr(io.Discard)
	cmd.SetOut(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	out, err := captureStdout(t, cmd.Execute)
	if err != nil || !strings.Contains(out, "3 candidates") || !strings.Contains(out, "VERDICT: learned weights TIE") {
		t.Fatalf("successful fixture lost its original scoring behavior: %v\n%s", err, out)
	}
}
