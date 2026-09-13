// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bright-interaction/mesh/internal/buildinfo"
	"github.com/bright-interaction/mesh/internal/index"
)

type freshnessContextKey struct{}

// The versioned viewer contract intentionally distinguishes an observed index
// revision from disk freshness. An owner claim is an observation, not proof of
// recent successful indexing; team responses disclose no host/PID/owner paths.
func (s *Server) viewerReadiness(r *http.Request) map[string]any {
	freshness := "unknown"
	if r.Context().Value(freshnessContextKey{}) == true {
		freshness = "current-to-observed-index"
	}
	mode := "owner"
	if s.store.ReadOnly() {
		mode = "read-only"
	}
	owner := "unknown"
	if s.member == nil {
		owner = "not-observed"
		if _, live := index.OwnerStatus(filepath.Join(s.vaultRoot, ".mesh")); live {
			owner = "observed"
		}
	}
	return map[string]any{"name": "mesh", "apiVersion": 1, "release": buildinfo.ReleaseVer(), "mode": mode, "freshness": freshness, "indexOwner": owner}
}

// ensureFresh observes persisted retrieval changes, not filesystem freshness.
// No reindex, owner acquisition, model call or background polling occurs here.
// Holding the graph gate makes simultaneous browser reads share one reload and
// prevents an older load from overwriting an in-process reindex/promotion.
func (s *Server) ensureFresh(ctx context.Context) error {
	release, err := s.acquireGraphUpdate(ctx)
	if err != nil {
		return err
	}
	defer release()
	if s.viewClosed {
		return errors.New("viewer closed")
	}
	checkIdentity := func() error {
		current, err := os.Stat(filepath.Join(s.vaultRoot, ".mesh", "mesh.db"))
		if err != nil || s.indexIdentity == nil || !os.SameFile(s.indexIdentity, current) {
			return errors.Join(index.ErrMonitorIndexReplaced, err)
		}
		return nil
	}
	if s.changeMonitor == nil {
		s.viewReusable = false
		if err := checkIdentity(); err != nil {
			return err
		}
		s.changeMonitor, err = s.store.NewChangeMonitor(ctx)
		if err != nil {
			return err
		}
		// Bracket creation against the reader's ORIGINAL file. Subsequent
		// requests use the monitor's own identity check without extra stats.
		if err := checkIdentity(); err != nil {
			_ = s.changeMonitor.Close()
			s.changeMonitor = nil
			return err
		}
	}
	// Do not recreate a failed monitor. In particular, a replaced SQLite file
	// cannot safely be combined with pooled connections to the old inode.
	before, err := s.changeMonitor.ReaderVersion(ctx)
	if err != nil {
		s.viewReusable = false
		return err
	}
	if s.viewReusable && before == s.viewVersion {
		return nil
	}
	s.viewReusable = false
	load := s.loadFreshGraph
	if load == nil {
		load = s.store.LoadGraphContext
	}
	for attempt := 0; attempt < 2; attempt++ {
		g, err := load(ctx)
		if err != nil {
			return err
		}
		after, err := s.changeMonitor.ReaderVersion(ctx)
		if err != nil {
			return err
		}
		if before == after {
			s.publishGraph(g) // also invalidates vectors/retriever, including vector-only commits
			s.viewVersion, s.viewReusable = after, true
			return nil
		}
		before = after
	}
	// A bounded retry avoids retaining a mixed graph during continuous writes.
	// No later revision is allowed to bless a graph built from earlier tables.
	return errors.New("index changed during viewer refresh")
}

func (s *Server) freshRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.ensureFresh(r.Context()); err != nil {
			// No local paths or database details cross a scoped/team boundary.
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Mesh index refresh unavailable; retry or restart the viewer", http.StatusServiceUnavailable)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), freshnessContextKey{}, true)))
	}
}
