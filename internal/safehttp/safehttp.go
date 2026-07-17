// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package safehttp provides SSRF-guarded HTTP clients shared across every mesh path
// that fetches a user- or config-controlled URL: the ingest connectors, and the BYOAI
// embedding / rerank / LLM endpoints. The guard resolves the destination host, refuses
// any resolved address that is loopback, private, link-local (incl. 169.254.169.254
// cloud metadata), CGNAT/Tailscale (100.64/10), ULA, multicast, or unspecified, then
// dials the vetted IP directly so a DNS-rebind flip between check and dial cannot slip
// a private IP through. Redirects are capped at 3, each re-dialed through the guard.
//
// This was previously private to internal/ingest and covered only the connectors; the
// embedding/rerank/LLM clients used a bare http.Client, so a config-set endpoint could
// probe the host/tailnet/metadata and exfil note content. Lifting it here closes that.
package safehttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client returns an http.Client that refuses non-public destinations (the SSRF guard)
// and follows at most 3 redirects, each re-dialed through the guard.
func Client(timeout time.Duration) *http.Client { return newClient(timeout, false) }

// LoopbackAllowed is like Client but permits private/loopback destinations. Use ONLY
// for an operator-configured sovereign endpoint (e.g. a self-hosted Ollama on
// 127.0.0.1), gated behind an operator env var, never a member-editable config field.
func LoopbackAllowed(timeout time.Duration) *http.Client { return newClient(timeout, true) }

// LLMClient returns the HTTP client for BYOAI endpoints (embeddings, rerank, LLM). It
// is SSRF-guarded by default so a config-set endpoint cannot probe the host, the
// Tailscale tailnet, or cloud metadata and exfil vault content. An OPERATOR (not a
// member via the config API) may set MESH_ALLOW_PRIVATE_LLM_ENDPOINT=1 to permit a
// sovereign localhost endpoint. The config API never writes this var, so a member
// cannot flip the guard off.
func LLMClient(timeout time.Duration) *http.Client {
	if AllowPrivateLLMEndpoint() {
		return LoopbackAllowed(timeout)
	}
	return Client(timeout)
}

// AllowPrivateLLMEndpoint reports whether the operator opted into private BYOAI
// endpoints (a localhost Ollama, an in-tailnet model server) via the env var.
func AllowPrivateLLMEndpoint() bool {
	v := strings.TrimSpace(os.Getenv("MESH_ALLOW_PRIVATE_LLM_ENDPOINT"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func newClient(timeout time.Duration, allowPrivate bool) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialContext(allowPrivate),
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConns:          10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("stopped after 3 redirects")
			}
			// Go strips Authorization/Cookie on a cross-host redirect but NOT custom auth
			// headers. Backends here send credentials in custom headers (x-api-key for
			// Anthropic, X-Flare-Key for ingest), so on a redirect to a different host those
			// would be replayed to the target and leak the key. Strip them when the host
			// changes; a same-host redirect keeps them so a normal 30x still authenticates.
			if len(via) > 0 && req.URL.Host != via[len(via)-1].URL.Host {
				for _, h := range []string{"X-Api-Key", "Anthropic-Version", "X-Flare-Key", "X-Api-Token"} {
					req.Header.Del(h)
				}
			}
			return nil // the redirect target is re-dialed through the guard, so it is re-checked
		},
	}
}

// dialContext resolves the host and refuses to connect if ANY resolved address is
// blocked (unless allowPrivate). It then dials the vetted IP directly (no
// re-resolution), closing the DNS-rebind window.
func dialContext(allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for %q", host)
		}
		if !allowPrivate {
			for _, ip := range ips {
				if BlockedIP(ip.IP) {
					return nil, fmt.Errorf("refusing to connect to non-public address %s (SSRF guard)", ip.IP)
				}
			}
		}
		d := &net.Dialer{Timeout: 10 * time.Second}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// extraBlockedCIDRs are ranges net.IP's built-in predicates miss but that are still
// unsafe destinations. The load-bearing one for this fleet is RFC 6598 CGNAT
// (100.64.0.0/10) = the Tailscale tailnet range: the hub runs on a tagged Tailscale
// node, so a URL resolving into 100.64/10 would pivot into the tailnet. 192.0.0.0/24
// carries Oracle Cloud's metadata IP (192.0.0.192); the TEST-NET + benchmark + IPv6-doc
// ranges round it out.
var extraBlockedCIDRs = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"100.64.0.0/10",   // RFC 6598 CGNAT / Tailscale (covers 100.100.100.100 too)
		"192.0.0.0/24",    // RFC 6890 IETF assignments (Oracle Cloud metadata 192.0.0.192)
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // RFC 2544 benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"2001:db8::/32",   // IPv6 documentation
	} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// BlockedIP reports whether ip is a non-public / unsafe SSRF destination.
func BlockedIP(ip net.IP) bool {
	if ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() || // 10/8, 172.16/12, 192.168/16, fc00::/7
		ip.IsLinkLocalUnicast() || // 169.254/16 (cloud metadata), fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() { // 0.0.0.0, ::
		return true
	}
	for _, n := range extraBlockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
