// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// SSRF defense for connectors. The Jira connector fetches an admin-supplied Site
// URL server-side on the hub, so without this an admin (or anyone who reached the
// admin surface) could point it at 169.254.169.254 / 127.0.0.1 / internal Docker
// service names to probe metadata + internal services and exfil via the imported
// note body or the leaked error snippet. The repo's security rules require: resolve
// DNS, reject private ranges, cap redirects + re-validate each hop.
//
// safeClient is the default for ALL connectors (the public-host ones resolve to
// public IPs, so it is a no-op for them and defense-in-depth for any future
// URL-taking connector). Tests inject their own *http.Client (loopback httptest),
// so this only governs production pulls.

// safeClient returns an http.Client whose dialer rejects non-public destinations
// and which follows at most 3 redirects (each re-dialed through the same guard).
func safeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           safeDialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConns:          10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("stopped after 3 redirects")
			}
			return nil // the redirect target is re-dialed through safeDialContext, so it is re-checked
		},
	}
}

// safeDialContext resolves the host and refuses to connect if ANY resolved address
// is loopback, private, link-local (incl. the 169.254.169.254 cloud-metadata IP),
// ULA, or unspecified. It then dials the vetted IP directly (no re-resolution), so
// a DNS-rebinding flip between check and dial cannot slip a private IP through.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
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
	for _, ip := range ips {
		if blockedIP(ip.IP) {
			return nil, fmt.Errorf("refusing to connect to non-public address %s (SSRF guard)", ip.IP)
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// extraBlockedCIDRs are ranges net.IP's built-in predicates miss but that are still
// unsafe destinations. The load-bearing one for THIS fleet is RFC 6598 CGNAT
// (100.64.0.0/10) = the Tailscale tailnet range: the hub runs on a tagged Tailscale
// node, so a connector URL resolving into 100.64/10 would pivot into the tailnet.
// 192.0.0.0/24 carries Oracle Cloud's metadata IP (192.0.0.192); the TEST-NET +
// benchmark + IPv6-doc ranges round it out.
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

func blockedIP(ip net.IP) bool {
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
