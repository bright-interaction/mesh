// SPDX-License-Identifier: LicenseRef-BrightInteraction-Commercial
// Copyright (C) 2026 Bright Interaction AB

// Package flarereport wires the pro entry points (mesh-hub) to the house Flare
// instance. It is the ONLY package in this module allowed to import sentry-go:
// the fair-code open core (the mesh CLI and pkg/) must never link error reporting,
// so keep this import isolated here and never import flarereport from open-core
// packages.
package flarereport

import (
	"log/slog"
	"strings"
	"net/http"
	"os"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

// InitFlare wires error reporting to the house Flare instance (Sentry-wire
// protocol) when FLARE_DSN is set in the environment. The DSN is injected by
// the Hephaestus flare-provision deploy step; without it this is a no-op so
// dev runs and self-hosts boot unchanged.
// scrubEvent strips request context that must not reach the shared Flare store: the query
// string (invite/join tokens), cookies (session), and auth headers (bearer/x-api-key).
func scrubEvent(event *sentry.Event) *sentry.Event {
	if event != nil && event.Request != nil {
		event.Request.QueryString = ""
		event.Request.Cookies = ""
		if event.Request.Headers != nil {
			for _, h := range []string{"Authorization", "Cookie", "X-Api-Key", "X-Api-Token", "X-Flare-Key"} {
				delete(event.Request.Headers, h)
			}
		}
		if i := strings.IndexByte(event.Request.URL, '?'); i >= 0 {
			event.Request.URL = event.Request.URL[:i]
		}
	}
	return event
}

func InitFlare(service, release string) bool {
	dsn := os.Getenv("FLARE_DSN")
	if dsn == "" {
		return false
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:        dsn,
		Release:    release,
		ServerName: service,
		// Scrub request context before it egresses to the shared Flare store: query
		// strings can carry tokens (e.g. an invite/join token), cookies carry the session,
		// and headers carry bearer/x-api-key creds. BeforeSend covers error/message events;
		// BeforeSendTransaction covers trace envelopes (registered too so a future
		// EnableTracing cannot silently bypass the scrub).
		BeforeSend:            func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event { return scrubEvent(e) },
		BeforeSendTransaction: func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event { return scrubEvent(e) },
	})
	if err != nil {
		slog.Warn("flare: error reporting disabled (sentry init failed)", "error", err)
		return false
	}
	slog.Info("flare: error reporting enabled", "service", service)
	startHeartbeat(service)
	installLogShipper(service)
	return true
}

// CapturePanic reports a recovered panic to Flare with request context. The hub
// has its own panic-recovery middleware that renders the 500, so this is called
// from inside its recover block and does not re-panic. Safe when InitFlare was
// a no-op: capture calls on an uninitialized hub do nothing.
func CapturePanic(r *http.Request, rec any) {
	hub := sentry.CurrentHub().Clone()
	hub.Scope().SetRequest(r)
	hub.RecoverWithContext(r.Context(), rec)
	hub.Flush(2 * time.Second)
}

// CaptureErr reports a non-panic error to Flare. No-op when reporting is
// disabled. Use for errors that are handled but should page someone.
func CaptureErr(err error) {
	if err == nil {
		return
	}
	sentry.CaptureException(err)
}
