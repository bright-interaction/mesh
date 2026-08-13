// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package netaddr answers the one question every Mesh listener has to settle
// before it decides whether it may serve without a credential: does this bind
// address reach only this machine?
//
// It exists because that question had two independent implementations (the MCP
// HTTP server in cmd/mesh, the web viewer in internal/web) and the third listener
// never asked it at all: `mesh serve-ssh --allow-anonymous` defaulted to ":2222"
// and handed the whole vault to anyone on the LAN, while its own help said
// "localhost demos only". One implementation means the next listener inherits the
// answer instead of reinventing it, or forgetting it.
package netaddr

import "net"

// IsLoopback reports whether a host:port bind address reaches only the local
// machine. A bare port (":2222") or an empty host is NOT loopback: those bind
// every interface, so the LAN, and on a VPS the internet, can reach them. An
// unparseable host is treated as non-loopback, so the callers that gate an
// unauthenticated serve on this fail closed.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false // ":2222" binds all interfaces
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
