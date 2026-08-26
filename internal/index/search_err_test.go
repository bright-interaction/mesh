// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSearchErrPreservesContextSentinelWhenDriverErrIsPlain is the regression guard
// for the exact case searchErr's own doc comment says it exists for: the driver
// reports an interrupted statement (a plain, non-context error) rather than handing
// back the context error directly. searchErr's switch is a disjunction over
// (err, ctx.Err()), but if %w only ever binds err, a match that comes ONLY from
// ctx.Err() drops the context.DeadlineExceeded / context.Canceled sentinel from the
// returned chain. Downstream, retrievalErr classifies purely via errors.Is against
// those sentinels, so a dropped sentinel means a real timeout/cancellation falls
// through to the opaque "internal error" the loud-retrieval-failure feature exists
// to eliminate.
func TestSearchErrPreservesContextSentinelWhenDriverErrIsPlain(t *testing.T) {
	t.Run("deadline exceeded", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("test setup broken: ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
		}

		driverErr := errors.New("sqlite: interrupted")
		result := searchErr(ctx, driverErr)

		if !errors.Is(result, context.DeadlineExceeded) {
			t.Fatalf("searchErr(ctx-with-deadline-exceeded, plain-driver-err) = %v, "+
				"want errors.Is(result, context.DeadlineExceeded) to be true", result)
		}
		if !strings.Contains(result.Error(), "sqlite: interrupted") {
			t.Errorf("driver detail dropped from message: %q", result.Error())
		}
		if !strings.Contains(result.Error(), "search timed out") {
			t.Errorf("human remedy text dropped from message: %q", result.Error())
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("test setup broken: ctx.Err() = %v, want Canceled", ctx.Err())
		}

		driverErr := errors.New("sqlite: interrupted")
		result := searchErr(ctx, driverErr)

		if !errors.Is(result, context.Canceled) {
			t.Fatalf("searchErr(ctx-cancelled, plain-driver-err) = %v, "+
				"want errors.Is(result, context.Canceled) to be true", result)
		}
		if !strings.Contains(result.Error(), "sqlite: interrupted") {
			t.Errorf("driver detail dropped from message: %q", result.Error())
		}
		if !strings.Contains(result.Error(), "search cancelled") {
			t.Errorf("human remedy text dropped from message: %q", result.Error())
		}
	})
}
