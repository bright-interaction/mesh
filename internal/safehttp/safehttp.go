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
func Client(timeout time.Duration) *http.Client { return newClient(timeout, false, "") }

// LoopbackAllowed is like Client but permits private/loopback destinations. Use ONLY
// for an operator-configured sovereign endpoint (e.g. a self-hosted Ollama on
// 127.0.0.1), gated behind an operator env var, never a member-editable config field.
func LoopbackAllowed(timeout time.Duration) *http.Client { return newClient(timeout, true, "") }

// llmRemedy is appended to the guard's refusal on a BYOAI endpoint. A stranger
// following the README pointed an endpoint at their own machine and got a bare
// "refusing to connect to non-public address" with no way forward, because the opt-in
// was named in no doc, no help string and no error. Every refusal a user can trigger
// has to name its remedy.
const llmRemedy = "; this looks like a model server on your own machine, so either pass the endpoint " +
	"in the process environment / on the command line (operator input, allowed by default) or set " +
	"MESH_ALLOW_PRIVATE_LLM_ENDPOINT=1 to allow a private endpoint that came from the config file or the web UI"

// LLMClient returns the HTTP client for a BYOAI endpoint (embeddings, rerank, LLM)
// whose URL arrived from a MEMBER-writable source: the hub, .mesh/config.toml, or
// PUT /api/config. It is SSRF-guarded so such an endpoint cannot probe the host, the
// Tailscale tailnet, or cloud metadata and exfil vault content. An OPERATOR (not a
// member via the config API) may set MESH_ALLOW_PRIVATE_LLM_ENDPOINT=1 to permit a
// sovereign localhost endpoint. The config API never writes this var, so a member
// cannot flip the guard off.
//
// For an endpoint that came from a CLI flag or the process environment use
// OperatorLLMClient instead: those two sources are already operator authority.
func LLMClient(timeout time.Duration) *http.Client {
	if AllowPrivateLLMEndpoint() {
		return LoopbackAllowed(timeout)
	}
	return newClient(timeout, false, llmRemedy)
}

// OperatorLLMClient returns the HTTP client for a BYOAI endpoint the OPERATOR supplied
// directly: a CLI flag, or a process environment variable such as
// MESH_EMBED_ENDPOINT / MESH_RERANK_ENDPOINT / MESH_CURATOR_ENDPOINT. Neither source is
// reachable from any HTTP surface (the config API writes config.toml, never a flag and
// never the environment), so an endpoint arriving that way carries exactly the same
// operator authority as MESH_ALLOW_PRIVATE_LLM_ENDPOINT itself and a loopback
// destination is allowed.
//
// This split is the whole point: the SSRF guard exists for the endpoint a member can
// WRITE, and blanket-applying it to the operator's own command line is what made every
// documented local BYOAI setup (a localhost Ollama, tools/rerank-server on 127.0.0.1)
// unusable out of the box.
func OperatorLLMClient(timeout time.Duration) *http.Client { return LoopbackAllowed(timeout) }

// AllowPrivateLLMEndpoint reports whether the operator opted into private BYOAI
// endpoints (a localhost Ollama, an in-tailnet model server) via the env var.
func AllowPrivateLLMEndpoint() bool {
	v := strings.TrimSpace(os.Getenv("MESH_ALLOW_PRIVATE_LLM_ENDPOINT"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

const (
	maxIdleConns        = 10
	maxIdleConnsPerHost = 2
	idleConnTimeout     = 90 * time.Second
)

// Transports own connection pools and are safe for concurrent use. Keep one pool per
// exact dial policy: sharing across trust policies would let a config-controlled public
// endpoint inherit the operator client's loopback permission, while creating a pool per
// short-lived retriever would let retired pools accumulate until their idle timeout.
var (
	publicTransport      = newTransport(false, "")
	publicLLMTransport   = newTransport(false, llmRemedy)
	operatorLLMTransport = newTransport(true, "")
)

// newClient builds the guarded client. remedy is appended to a refusal so the caller's
// own opt-in is named in the error the user actually sees; pass "" when there is none.
func newClient(timeout time.Duration, allowPrivate bool, remedy string) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: transportFor(allowPrivate, remedy),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("stopped after 3 redirects")
			}
			// Go strips Authorization/Cookie on a cross-host redirect but NOT custom auth
			// headers. Backends here send credentials in custom headers (x-api-key for
			// Anthropic, X-Flare-Key for ingest), so on a redirect to a different host those
			// would be replayed to the target and leak the key. Strip them when the host
			// changes; a same-host redirect keeps them so a normal 30x still authenticates.
			// Compare against the ORIGINAL host, not the previous hop. Go's
			// makeHeadersCopier re-copies from the initial request's header map on every
			// hop and runs before CheckRedirect, so a header deleted at hop 1 is restored
			// at hop 2 - and hop 2 back to the attacker's own host is same-host, so the
			// old previous-hop test did not fire and handed the key straight over. Two
			// hops are free inside the 3-redirect budget, so one open redirect on a
			// trusted host was enough. Once off-origin, stay stripped for the whole chain.
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				for _, h := range []string{"X-Api-Key", "Anthropic-Version", "X-Flare-Key", "X-Api-Token"} {
					req.Header.Del(h)
				}
			}
			return nil // the redirect target is re-dialed through the guard, so it is re-checked
		},
	}
}

func transportFor(allowPrivate bool, remedy string) *http.Transport {
	switch {
	case allowPrivate && remedy == "":
		return operatorLLMTransport
	case !allowPrivate && remedy == "":
		return publicTransport
	case !allowPrivate && remedy == llmRemedy:
		return publicLLMTransport
	default:
		// newClient is package-private and has exactly the three policy combinations
		// above. Fail closed if a new caller forgets to allocate a distinct shared pool.
		panic("safehttp: unsupported transport policy")
	}
}

func newTransport(allowPrivate bool, remedy string) *http.Transport {
	return &http.Transport{
		DialContext:           dialContext(allowPrivate, remedy),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		// A response body can be closed correctly and still leave its connection in
		// the keep-alive pool. Expire quiet model-server sockets instead of retaining
		// them for the lifetime of a long-running MCP process.
		IdleConnTimeout: idleConnTimeout,
	}
}

// dialContext resolves the host and refuses to connect if ANY resolved address is
// blocked (unless allowPrivate). It then dials the vetted IP directly (no
// re-resolution), closing the DNS-rebind window.
func dialContext(allowPrivate bool, remedy string) func(ctx context.Context, network, addr string) (net.Conn, error) {
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
					return nil, fmt.Errorf("refusing to connect to non-public address %s (SSRF guard)%s", ip.IP, remedy)
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
