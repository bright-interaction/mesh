// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"net"
	"net/http"
	"time"

	"github.com/bright-interaction/mesh/internal/safehttp"
)

// SSRF defense for connectors. The Jira connector fetches an admin-supplied Site URL
// server-side on the hub, so without this an admin (or anyone who reached the admin
// surface) could point it at 169.254.169.254 / 127.0.0.1 / internal Docker service
// names to probe metadata + internal services and exfil via the imported note body or
// the leaked error snippet. The guard now lives in the shared internal/safehttp package
// (embed/rerank/llm use it too); these are thin forwarders so the connectors and their
// tests keep the same local names.
//
// safeClient is the default for ALL connectors (the public-host ones resolve to public
// IPs, so it is a no-op for them and defense-in-depth for any future URL-taking
// connector). Tests inject their own *http.Client (loopback httptest), so this only
// governs production pulls.

func safeClient(timeout time.Duration) *http.Client { return safehttp.Client(timeout) }

func blockedIP(ip net.IP) bool { return safehttp.BlockedIP(ip) }
