// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a per-key token bucket, the same shape as the hub's. It exists here
// rather than being imported from internal/hub because internal/web is the open core
// and must not depend on the pro hub package.
//
// Idle keys are swept so a long-running server cannot accumulate one bucket per
// attacker source address.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens refilled per second
	burst   float64 // bucket capacity
	// now is injectable so tests can advance time without sleeping.
	now func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// idleBucketTTL is how long a bucket survives with no traffic before the sweep drops
// it. Longer than the time it takes a full bucket to refill, so dropping a bucket can
// never hand a caller back tokens it had already spent.
const idleBucketTTL = 10 * time.Minute

func newRateLimiter(ratePerSec, burst float64) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSec,
		burst:   burst,
		now:     time.Now,
	}
}

// allow consumes one token for key, reporting whether the caller may proceed.
func (l *rateLimiter) allow(key string) bool {
	if l == nil {
		return true // not wired (a hand-built Server in a test): fail open, not panic
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
		l.sweepLocked(now)
	} else {
		b.tokens += now.Sub(b.last).Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepLocked drops buckets that have been idle past the TTL. Called only when a new
// key appears, so the cost is bounded by the number of distinct keys, not by traffic.
func (l *rateLimiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > idleBucketTTL {
			delete(l.buckets, k)
		}
	}
}

// trustedProxyHopsEnv names how many reverse-proxy hops sit in front of mesh ui. In
// prod that is one (Caddy). It has to be configured rather than sniffed: X-Forwarded-For
// is caller-controlled, so trusting the whole chain would let an attacker rotate the
// rate-limit key at will, while ignoring it entirely collapses every request behind the
// proxy into one bucket and turns the limiter into a global outage switch.
const trustedProxyHopsEnv = "MESH_UI_TRUSTED_PROXY_HOPS"

// peerKey is the rate-limit key for a request: the client address as seen past the
// configured number of trusted proxy hops. With 0 hops (the default, a direct bind) it
// is r.RemoteAddr's host. With n hops it is the nth entry from the right of
// X-Forwarded-For, which is the last address the trusted chain could not have forged.
// Falls back to RemoteAddr whenever the header is shorter than the configured chain.
func peerKey(r *http.Request) string {
	hops := 0
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(trustedProxyHopsEnv))); err == nil && v > 0 {
		hops = v
	}
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if hops == 0 {
		return remote
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	idx := len(parts) - hops
	if idx < 0 || idx >= len(parts) {
		return remote // header absent or shorter than the trusted chain: trust the socket
	}
	addr := strings.TrimSpace(parts[idx])
	if addr == "" {
		return remote
	}
	return addr
}
