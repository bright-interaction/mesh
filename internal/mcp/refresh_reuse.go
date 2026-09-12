// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"errors"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

// readerVersion is called only under reloadMu. Probe failure is a cache miss,
// not a reason to stop refreshing. Reconnection invalidates the old stamp because
// SQLite data_version values from different connections cannot be compared.
func (s *Server) readerVersion(ctx context.Context) (int64, bool) {
	if s.refreshClosed {
		return 0, false
	}
	if s.changeMonitor == nil {
		s.viewReusable = false
		monitor, err := s.store.NewChangeMonitor(ctx)
		if err != nil {
			return 0, false
		}
		s.changeMonitor = monitor
	}
	version, err := s.changeMonitor.Version(ctx)
	if err != nil {
		s.viewReusable = false
		if errors.Is(err, index.ErrMonitorIndexReplaced) {
			return 0, false // do not relabel pooled old-file readers with a new monitor
		}
		_ = s.changeMonitor.Close()
		s.changeMonitor = nil
		return 0, false
	}
	return version, true
}

// The monitor brackets ALL database reads in graph/retriever construction. A
// commit during construction leaves the installed snapshot usable but uncached;
// the next pass must reload. No after-the-fact stamp may bless an older graph.
// Configuration is different: the fingerprint names the immutable inputs actually
// consumed by construction, so a racing A -> B -> A file edit cannot label a B
// retriever as A. Config read failures also disable reuse.
func (s *Server) rememberReaderVersion(ctx context.Context, before int64, valid bool, in *retrieve.ConfigInputs) {
	s.viewReusable = false
	if !valid || !s.viewBuildComplete {
		return
	}
	fingerprint, reusable := in.Fingerprint()
	if !reusable {
		return
	}
	after, ok := s.readerVersion(ctx)
	if !ok || before != after {
		return
	}
	s.viewVersion, s.viewConfig, s.viewReusable = after, fingerprint, true
}
