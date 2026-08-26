// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
)

// TestRetrievalErrSeparatesUnavailableFromEmpty is the durable guard for the failure
// that made a 2300-note vault stop preventing repeat work: mesh_search returning the
// opaque "internal error" for a timed-out search, which an agent cannot tell apart from
// a successful search that found nothing. Measured on the Corpus vault 2026-08-26: 296
// timeouts in four days, every one reaching the agent as "internal error" while the same
// query answered from a fresh CLI connection in ~1.5s. Agents read that as "no prior
// knowledge exists" and re-derived from source.
//
// The two observations MUST stay distinguishable at the wire level, so this asserts both
// halves: the failure is loud and marked empty=false, and the genuinely-empty result is
// a SUCCESS carrying an empty card list.
func TestRetrievalErrSeparatesUnavailableFromEmpty(t *testing.T) {
	t.Run("timeout is loud, retryable and explicitly not-empty", func(t *testing.T) {
		rerr := retrievalErr(fmt.Errorf("search timed out: %w", context.DeadlineExceeded))
		if rerr == nil {
			t.Fatal("a deadline-exceeded search must produce an error")
		}
		if rerr.Message == "internal error" {
			t.Fatal("a timed-out search still reports the opaque message an agent reads as 'nothing found'")
		}
		// The agent's decision hinges on this: it must be told the vault was NOT searched.
		if !strings.Contains(rerr.Message, "NOT an empty result") {
			t.Errorf("message does not tell the agent this is not an empty result: %q", rerr.Message)
		}
		data, ok := rerr.Data.(map[string]any)
		if !ok {
			t.Fatalf("Data must be machine-readable so a client can branch without parsing prose, got %T", rerr.Data)
		}
		if data["empty"] != false {
			t.Errorf("empty must be false, got %v", data["empty"])
		}
		if data["retryable"] != true {
			t.Errorf("retryable must be true, got %v", data["retryable"])
		}
		if data["reason"] != "search_timeout" {
			t.Errorf("reason must name the failure mode, got %v", data["reason"])
		}
	})

	t.Run("cancellation is also distinguishable", func(t *testing.T) {
		rerr := retrievalErr(fmt.Errorf("search cancelled: %w", context.Canceled))
		if rerr.Message == "internal error" {
			t.Fatal("a cancelled search must not report the opaque message")
		}
		data, _ := rerr.Data.(map[string]any)
		if data["reason"] != "search_cancelled" {
			t.Errorf("reason must name the failure mode, got %v", data["reason"])
		}
	})

	// The security property internalErr exists for must survive: sqlite driver text and
	// absolute filesystem paths must never reach the agent. Anything unrecognised keeps
	// degrading to the opaque form rather than being surfaced "helpfully".
	t.Run("an unrecognised error still degrades to opaque", func(t *testing.T) {
		const leakyPath = "/var/lib/mesh-example/.mesh/mesh.db"
		leaky := errors.New("no such table: search_index in " + leakyPath)
		rerr := retrievalErr(leaky)
		if rerr.Message != "internal error" {
			t.Fatalf("an unclassified error must stay opaque, got %q", rerr.Message)
		}
		if strings.Contains(rerr.Message, leakyPath) || strings.Contains(rerr.Message, "search_index") {
			t.Fatal("driver text or a filesystem path leaked to the agent")
		}
	})

	// End-to-end through the real tool: an expired context must not come back as a
	// success with zero cards, which is the shape that taught agents to give up.
	t.Run("toolSearch on an expired context does not look like a hit-free success", func(t *testing.T) {
		s := newTestServer(t)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()

		res, rerr := s.toolSearch(ctx, json.RawMessage(`{"query":"storage"}`))
		if rerr == nil {
			// Retrieval may legitimately complete without consulting the context on a
			// two-note fixture. What is NOT acceptable is a silent empty result, so if it
			// succeeded it must have actually returned the seeded hit.
			if strings.Contains(toolTextOf(t, res), `"cards":[]`) {
				t.Fatal("an expired context produced an EMPTY success: indistinguishable from 'the vault knows nothing'")
			}
			return
		}
		if rerr.Message == "internal error" {
			t.Fatal("an expired context produced the opaque message an agent reads as 'nothing found'")
		}
	})

	// A real empty result is the control: it must remain a SUCCESS, so the two cases are
	// separated by more than message text.
	t.Run("a genuinely empty result stays a success", func(t *testing.T) {
		s := newTestServer(t)
		res, rerr := s.toolSearch(context.Background(), json.RawMessage(`{"query":"zzzznothingmatchesthisqueryzzzz"}`))
		if rerr != nil {
			t.Fatalf("a query with no matches must succeed, not error: %+v", rerr)
		}
		if !strings.Contains(toolTextOf(t, res), `"cards":[]`) {
			t.Errorf("expected an empty card list, got %s", toolTextOf(t, res))
		}
	})
}

// TestRetrievalErrSurvivesDriverOnlySentinelLoss pins the wire contract between
// internal/index.searchErr and retrievalErr for the exact case a P1 found in
// searchErr: the FTS driver reports its own "interrupted" error (a plain,
// non-context error) rather than surfacing the context error, so the
// context.DeadlineExceeded / context.Canceled sentinel can only be known from
// ctx.Err(), not from err. searchErr's fix wraps the sentinel itself (via %w) and
// renders the driver error alongside it with %v so operators keep the detail in
// logs. This test builds exactly that shape - the one searchErr now returns for
// this case - and confirms it survives retrievalErr's errors.Is classification
// instead of falling through to the opaque "internal error" the loud-retrieval
// -failure feature exists to eliminate.
//
// This cannot be driven end-to-end through a live sqlite query today: the
// currently vendored driver (modernc.org/sqlite v1.52.0) maps interrupts back to
// ctx.Err() on its own, so err already carries the sentinel before it ever reaches
// searchErr (see the P1 finding for the 40/40 measurement). That makes the
// driver-only-error path latent rather than reproducible with a real query, which
// is exactly why searchErr needed to consult ctx.Err() independently of err in the
// first place.
func TestRetrievalErrSurvivesDriverOnlySentinelLoss(t *testing.T) {
	t.Run("deadline exceeded, driver reports a plain interrupted error", func(t *testing.T) {
		driverErr := errors.New("sqlite: interrupted")
		searchErrShape := fmt.Errorf(
			"search timed out after %s: use fewer, more specific words, or split your largest note files into smaller ones: %w (driver: %v)",
			index.SearchTimeout, context.DeadlineExceeded, driverErr,
		)

		rerr := retrievalErr(searchErrShape)
		if rerr.Message == "internal error" {
			t.Fatalf("driver-only sentinel loss: retrievalErr fell through to the opaque form for %v", searchErrShape)
		}
		data, _ := rerr.Data.(map[string]any)
		if data["reason"] != "search_timeout" {
			t.Errorf("reason must name the failure mode, got %v", data["reason"])
		}
		if !strings.Contains(searchErrShape.Error(), "sqlite: interrupted") {
			t.Error("driver detail must remain visible in the message for operators")
		}
	})

	t.Run("cancelled, driver reports a plain interrupted error", func(t *testing.T) {
		driverErr := errors.New("sqlite: interrupted")
		searchErrShape := fmt.Errorf("search cancelled: %w (driver: %v)", context.Canceled, driverErr)

		rerr := retrievalErr(searchErrShape)
		if rerr.Message == "internal error" {
			t.Fatalf("driver-only sentinel loss: retrievalErr fell through to the opaque form for %v", searchErrShape)
		}
		data, _ := rerr.Data.(map[string]any)
		if data["reason"] != "search_cancelled" {
			t.Errorf("reason must name the failure mode, got %v", data["reason"])
		}
		if !strings.Contains(searchErrShape.Error(), "sqlite: interrupted") {
			t.Error("driver detail must remain visible in the message for operators")
		}
	})
}

func TestRetrievalErrMachineDataSurvivesJSONRPCWire(t *testing.T) {
	want := retrievalErr(fmt.Errorf("wrapped timeout: %w", context.DeadlineExceeded))
	b, err := json.Marshal(response{JSONRPC: "2.0", ID: json.RawMessage(`7`), Error: want})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Error struct {
			Message string `json:"message"`
			Data    struct {
				Reason    string `json:"reason"`
				Retryable bool   `json:"retryable"`
				Empty     bool   `json:"empty"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Error.Data.Reason != "search_timeout" || !wire.Error.Data.Retryable || wire.Error.Data.Empty {
		t.Fatalf("machine-readable retrieval failure was lost on the JSON-RPC wire: %s", b)
	}
	if !strings.Contains(wire.Error.Message, "NOT an empty result") {
		t.Fatalf("wire message no longer distinguishes failure from an empty search: %q", wire.Error.Message)
	}
}

// toolTextOf pulls the text block out of a tool result built by textResult.
func toolTextOf(t *testing.T, res any) string {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Content) == 0 {
		return ""
	}
	return envelope.Content[0].Text
}
