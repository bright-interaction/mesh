// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"time"
)

// StreamEvents subscribes to the hub's SSE nudge stream for a joined vault and
// sends a (non-blocking) signal on nudge for every "head changed" event. It
// reconnects until ctx is cancelled, so a dropped stream self-heals. The nudge is
// stateless: the caller just runs a reconcile, so a missed or coalesced event is
// harmless (the periodic reconcile is the safety net). StreamEvents returns nil
// on ctx cancellation; it returns an error only if the vault is not joined (no
// credentials). A server-side rejection is not fatal here: an auth-class status
// (401/403/410) may be transient during a hub restart, so it backs off longer
// and logs rather than giving up, while the caller's periodic reconcile keeps the
// vault converging regardless.
func StreamEvents(ctx context.Context, vaultDir string, nudge chan<- struct{}) error {
	creds, err := readCredentials(vaultDir)
	if err != nil {
		return err
	}
	const netBackoff = 3 * time.Second
	const authBackoff = 30 * time.Second
	for ctx.Err() == nil {
		status := streamOnce(ctx, creds, nudge)
		if ctx.Err() != nil {
			return nil
		}
		backoff := netBackoff
		// An auth-class rejection means the token is (or looks) revoked. Do not
		// hammer the endpoint every few seconds: back off longer and surface it,
		// but keep retrying in case it was a transient restart-window 401.
		if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusGone {
			backoff = authBackoff
			Logf("mesh: hub rejected the event stream (status %d); token may be revoked, retrying in %s", status, authBackoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
	}
	return nil
}

// Logf is an optional sink for non-fatal StreamEvents diagnostics (e.g. an
// auth-class rejection). Defaults to a no-op so the library stays quiet unless a
// caller (cmd/mesh) wires in a logger.
var Logf = func(string, ...any) {}

// streamOnce holds one SSE connection open until it drops or ctx is cancelled. It
// uses a client with NO timeout (the stream is long-lived); ctx is the only
// lifetime control. It returns the HTTP status it observed (0 if the request
// never completed), so the caller can pick an appropriate reconnect backoff.
func streamOnce(ctx context.Context, creds credentials, nudge chan<- struct{}) int {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(creds.HubURL, "/")+"/v1/events", nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+creds.Token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	for sc.Scan() {
		// Each SSE message ends with a blank line; a "changed" event carries a
		// "data:" line (the new HEAD). Keepalives are comment lines (": ...") with
		// no data field, so nudging only on a data line ignores them.
		if strings.HasPrefix(sc.Text(), "data:") {
			select {
			case nudge <- struct{}{}:
			default: // a reconcile is already pending; coalesce
			}
		}
	}
	return resp.StatusCode
}
