// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/connectauth"
	"github.com/bright-interaction/mesh/internal/index"
)

// This is an offline, synthetic restore drill, not evidence of a production
// backup, live user permissions, or revocations made AFTER the snapshot. Every
// database is closed before copying; no HTTP server, watcher or model is started.
func TestOfflineRestorePreservesReviewAndConnectionState(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	notes := map[string]string{
		"rule.md":    "---\nid: recovery-rule\ntype: decision\ntitle: Coppergate recovery rule\nwhen: \"2026-09-23\"\nscope: [ops]\nrelated: [recovery-context]\n---\n# Coppergate recovery rule\n\n## Do\nVerify the snapshot.\n\n## Don't\nNever restore stale grants over newer revocations.\n",
		"context.md": "---\nid: recovery-context\ntype: note\ntitle: Recovery context\nwhen: \"2026-09-23\"\nscope: [ops]\n---\n# Recovery context\n\nPreserve pending reviews separately from published notes.\n",
	}
	for name, body := range notes {
		writeNote(t, source, name, body)
	}
	if out, err := runCLI(t, indexCmd(), source); err != nil {
		t.Fatalf("source index: %v\n%s", err, out)
	}
	queueItem := index.PendingNote{ID: "pending-offline-restore", Type: "gotcha", Title: "Unreviewed restoration finding", Do: "Review before promotion.", Dont: "Do not treat a candidate as accepted guidance.", Why: "No Markdown copy exists yet.", Confidence: "observed", Source: "synthetic-restore", CreatedAt: 1790110000}
	store, err := index.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddPending(queueItem); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	const audience = "https://mesh.example.test/app"
	const subject = "member:restore-fixture:immutable-generation"
	const authRel = ".mesh/auth/connections.db"
	auth, err := connectauth.Open(filepath.Join(source, authRel))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auth.Close() })
	approve := func(scope string) connectauth.Tokens {
		t.Helper()
		device, err := auth.Begin(ctx, "mesh-ide", scope, audience)
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.Decide(ctx, device.UserCode, subject, audience, scope, true); err != nil {
			t.Fatal(err)
		}
		tokens, err := auth.Poll(ctx, device.DeviceCode, "mesh-ide", audience)
		if err != nil {
			t.Fatal(err)
		}
		return tokens
	}
	active := approve(connectauth.ScopeRead)
	revoked := approve(connectauth.ScopeFull)
	if err := auth.Revoke(ctx, revoked.GrantID, subject, audience); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	// Copy an explicit fixture manifest, not a live vault or arbitrary .mesh tree.
	files := []string{"rule.md", "context.md", ".mesh/mesh.db", authRel}
	backup, restored := t.TempDir(), t.TempDir()
	for _, rel := range files {
		copyClosedRestoreFixture(t, source, backup, rel)
		copyClosedRestoreFixture(t, backup, restored, rel)
	}
	authBefore, err := os.ReadFile(filepath.Join(restored, authRel))
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{source, restored} {
		for _, rel := range []string{".mesh/mesh.db", authRel} {
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Stat(filepath.Join(root, rel+suffix)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("closed fixture has unexpected sidecar %s%s: %v", rel, suffix, err)
				}
			}
		}
	}

	if out, err := runCLI(t, indexCmd(), restored); err != nil {
		t.Fatalf("restored index: %v\n%s", err, out)
	}
	if out, err := runCLI(t, doctorCmd(), restored); err != nil || !strings.Contains(out, "status: OK") {
		t.Fatalf("restored doctor: %v\n%s", err, out)
	}
	for name, body := range notes {
		for _, root := range []string{source, backup, restored} {
			got, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || string(got) != body {
				t.Fatalf("restore altered Markdown at %s: %v", filepath.Join(root, name), err)
			}
		}
	}
	authAfter, err := os.ReadFile(filepath.Join(restored, authRel))
	if err != nil || !bytes.Equal(authBefore, authAfter) {
		t.Fatalf("index/doctor altered the separate connection database: %v", err)
	}
	reader, err := index.OpenReadOnly(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	items, err := reader.ListPending()
	if err != nil || !reflect.DeepEqual(items, []index.PendingNote{queueItem}) {
		t.Fatalf("pending review not preserved: %+v, %v", items, err)
	}
	for _, tc := range []struct {
		scope string
		want  int
	}{{"ops", 1}, {"sales", 0}} {
		hits, err := reader.SearchScoped(ctx, "coppergate", 5, map[string]bool{tc.scope: true})
		if err != nil || len(hits) != tc.want {
			t.Fatalf("restored scope %s: %d hits, want %d, err %v", tc.scope, len(hits), tc.want, err)
		}
		if len(hits) > 0 && hits[0].Path != "rule.md" {
			t.Fatalf("restored note did not retain a vault-relative path: %q", hits[0].Path)
		}
	}
	g, err := reader.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, edge := range g.Neighbors("note:recovery-rule") {
		if edge.Target == "note:recovery-context" && edge.Relation == "references" {
			linked = true
		}
	}
	if !linked {
		t.Fatal("restored knowledge relationship is missing")
	}
	restoredAuth, err := connectauth.Open(filepath.Join(restored, authRel))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredAuth.Close()
	grant, err := restoredAuth.LookupAccess(ctx, active.AccessToken, audience)
	if err != nil || grant.Scope != connectauth.ScopeRead || grant.Subject != subject {
		t.Fatalf("restored active grant changed identity or scope: %v", err)
	}
	if _, err := restoredAuth.LookupAccess(ctx, revoked.AccessToken, audience); !errors.Is(err, connectauth.ErrGrant) {
		t.Fatalf("revoked access accepted after restore: %v", err)
	}
	if _, err := restoredAuth.Refresh(ctx, revoked.RefreshToken, "mesh-ide", audience); !errors.Is(err, connectauth.ErrGrant) {
		t.Fatalf("revoked refresh accepted after restore: %v", err)
	}
	if _, err := restoredAuth.Refresh(ctx, active.RefreshToken, "mesh-ide", audience); err != nil {
		t.Fatalf("active grant could not renew after restore: %v", err)
	}

	// Negative control: Markdown-only rebuild is useful, but is NOT full recovery.
	markdownOnly := t.TempDir()
	for name := range notes {
		copyClosedRestoreFixture(t, backup, markdownOnly, name)
	}
	if out, err := runCLI(t, indexCmd(), markdownOnly); err != nil {
		t.Fatalf("Markdown-only index: %v\n%s", err, out)
	}
	partial, err := index.OpenReadOnly(markdownOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()
	missing, err := partial.ListPending()
	if err != nil || len(missing) != 0 {
		t.Fatalf("Markdown-only rebuild unexpectedly recovered pending reviews: %v", err)
	}
	if _, err := os.Stat(filepath.Join(markdownOnly, authRel)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("knowledge rebuild unexpectedly created connection state: %v", err)
	}
}

// The caller must close fixture databases first. This is intentionally NOT a
// production hot-backup utility; copying a live SQLite main file is unsafe.
func copyClosedRestoreFixture(t *testing.T, from, to, rel string) {
	t.Helper()
	info, err := os.Lstat(filepath.Join(from, rel))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("fixture source must be a regular file %s: %v", rel, err)
	}
	data, err := os.ReadFile(filepath.Join(from, rel))
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(to, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
