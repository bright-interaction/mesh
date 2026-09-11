// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"errors"
	"time"

	"github.com/bright-interaction/mesh/internal/watch"
	"github.com/bright-interaction/mesh/pkg/meshclient"
)

// runSyncWatch keeps exactly one index callback and one serial network worker.
// Local indexing never waits on a hub round (including one already in flight).
// The one-slot queue coalesces bursts but retains a follow-up for edits made
// during sync. Existing SyncVault locking still serializes other processes.
func runSyncWatch(ctx context.Context, opt watch.Options, hubInterval time.Duration, syncHub func() (meshclient.Summary, error)) error {
	if opt.OnReindex == nil || syncHub == nil {
		return errors.New("sync watch: local index and hub callbacks are required")
	}
	ctx, cancel := context.WithCancel(ctx)
	requests := make(chan struct{}, 1)
	refresh := make(chan struct{}, 1)
	done := make(chan struct{})
	logf := opt.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	go func() {
		defer close(done)
		var receipts meshclient.WatchReceipts
		for {
			select {
			case <-ctx.Done():
				return
			case <-requests:
			}
			if ctx.Err() != nil {
				return
			}
			sum, err := syncHub()
			if err != nil {
				logf("sync failed (local indexing continues): %v", err)
			} else {
				for _, line := range receipts.Lines(sum) {
					logf("%s", line)
				}
			}
			// Failed rounds can have durably applied some files too.
			select {
			case refresh <- struct{}{}:
			default:
			}
		}
	}()
	defer func() {
		cancel()
		// Drain the current durable sync before the caller closes its owner.
		// SyncVault is not cancellable; shutdown retains its existing completion
		// behavior, including waits on another process's same-vault sync lock.
		<-done
	}()
	local := opt.OnReindex
	var lastHub time.Time // only accessed by the watch callback
	opt.Refresh = refresh
	opt.OnReindex = func(p watch.Pass) (watch.Result, error) {
		result, err := local(p)
		if hubDue(p.Reason, lastHub, hubInterval, time.Now()) {
			lastHub = time.Now()
			select {
			case requests <- struct{}{}:
			default:
			}
		}
		return result, err
	}
	return watch.Run(ctx, opt)
}
