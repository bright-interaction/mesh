// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

func TestReportingContextReadsMatchLegacyAPIs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = s.Write(func(tx *sql.Tx) error {
		statements := []string{
			`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime)
			 VALUES('n1','n1.md','gotcha','N1','hash','{"Author":"Alice"}',1)`,
			`INSERT INTO metrics(key,value) VALUES
			 ('queries',6),('fetches',4),('writes',3),('fetch:n1',5)`,
			`INSERT INTO note_health(note_id,path,issue,detail,detected_at)
			 VALUES('n1','n1.md','overdue','2026-01-01',1)`,
			`INSERT INTO note_reuse(note_id,authored_at,source,reuse_count,first_reuse,last_reuse)
			 VALUES('n1',100,'agent',2,3700,3800)`,
		}
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p := PendingNote{Type: "gotcha", Title: "Review me", CreatedAt: 42}
	if err := s.AddPending(p); err != nil {
		t.Fatal(err)
	}
	pendingID := PendingID(p.Type, p.Title)
	ctx := context.Background()

	legacyMetric, legacyErr := s.Metric("queries")
	contextMetric, contextErr := s.MetricContext(ctx, "queries")
	assertReportingEqual(t, "Metric", legacyMetric, legacyErr, contextMetric, contextErr)

	legacyCount, legacyErr := s.Count("notes")
	contextCount, contextErr := s.CountContext(ctx, "notes")
	assertReportingEqual(t, "Count", legacyCount, legacyErr, contextCount, contextErr)

	legacyTypes, legacyErr := s.NotesByType()
	contextTypes, contextErr := s.NotesByTypeContext(ctx)
	assertReportingEqual(t, "NotesByType", legacyTypes, legacyErr, contextTypes, contextErr)

	legacyTop, legacyErr := s.TopFetched(8)
	contextTop, contextErr := s.TopFetchedContext(ctx, 8)
	assertReportingEqual(t, "TopFetched", legacyTop, legacyErr, contextTop, contextErr)

	legacyContributors, legacyErr := s.ContributorCounts()
	contextContributors, contextErr := s.ContributorCountsContext(ctx)
	assertReportingEqual(t, "ContributorCounts", legacyContributors, legacyErr, contextContributors, contextErr)

	legacyHealth, legacyErr := s.HealthCounts()
	contextHealth, contextErr := s.HealthCountsContext(ctx)
	assertReportingEqual(t, "HealthCounts", legacyHealth, legacyErr, contextHealth, contextErr)

	legacyFlywheel, legacyErr := s.FlywheelStats()
	contextFlywheel, contextErr := s.FlywheelStatsContext(ctx)
	assertReportingEqual(t, "FlywheelStats", legacyFlywheel, legacyErr, contextFlywheel, contextErr)

	legacyPendingCount, legacyErr := s.PendingCount()
	contextPendingCount, contextErr := s.PendingCountContext(ctx)
	assertReportingEqual(t, "PendingCount", legacyPendingCount, legacyErr, contextPendingCount, contextErr)

	legacyPending, legacyErr := s.ListPending()
	contextPending, contextErr := s.ListPendingContext(ctx)
	assertReportingEqual(t, "ListPending", legacyPending, legacyErr, contextPending, contextErr)

	legacyItem, legacyErr := s.GetPending(pendingID)
	contextItem, contextErr := s.GetPendingContext(ctx, pendingID)
	assertReportingEqual(t, "GetPending", legacyItem, legacyErr, contextItem, contextErr)
}

func TestReportingContextReadsHonorCanceledContext(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		read func() error
	}{
		{"Metric", func() error { _, err := s.MetricContext(ctx, "queries"); return err }},
		{"Count", func() error { _, err := s.CountContext(ctx, "notes"); return err }},
		{"NotesByType", func() error { _, err := s.NotesByTypeContext(ctx); return err }},
		{"TopFetched", func() error { _, err := s.TopFetchedContext(ctx, 8); return err }},
		{"ContributorCounts", func() error { _, err := s.ContributorCountsContext(ctx); return err }},
		{"HealthCounts", func() error { _, err := s.HealthCountsContext(ctx); return err }},
		{"FlywheelStats", func() error { _, err := s.FlywheelStatsContext(ctx); return err }},
		{"PendingCount", func() error { _, err := s.PendingCountContext(ctx); return err }},
		{"ListPending", func() error { _, err := s.ListPendingContext(ctx); return err }},
		{"GetPending", func() error { _, err := s.GetPendingContext(ctx, "missing"); return err }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.read(); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
		})
	}
}

func assertReportingEqual[T any](t *testing.T, name string, legacy T, legacyErr error, contextual T, contextErr error) {
	t.Helper()
	if !errors.Is(contextErr, legacyErr) || !errors.Is(legacyErr, contextErr) {
		t.Fatalf("%s errors differ: legacy=%v context=%v", name, legacyErr, contextErr)
	}
	if !reflect.DeepEqual(contextual, legacy) {
		t.Fatalf("%s differs: legacy=%#v context=%#v", name, legacy, contextual)
	}
}
