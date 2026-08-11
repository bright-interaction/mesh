// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshcfg

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// An endpoint URL field (embedding.endpoint, rerank.endpoint, secret_bridge.base_url) is
// a DESTINATION for credentialed traffic, so the set of hosts it may name has to be
// closed the same way keyenv.go closes the set of key NAMES. key_env decides which secret
// gets dereferenced; the endpoint decides who receives it, and both halves are writable
// over PUT /api/config by anyone with the admin role.
//
// What each one is worth to whoever redirects it: secret_bridge.base_url carries the
// Dockyard API key in X-API-Key, the long-lived credential the whole capability design
// exists to keep out of Mesh; rerank.endpoint is POSTed candidate note BODIES with an
// Authorization bearer attached; embedding.endpoint gets note text and the same bearer.
// The SSRF guard in internal/safehttp is not the answer here: it refuses loopback and
// RFC1918, not the public internet, which is exactly where an exfiltration host lives.
//
// The allow-list therefore lives in the ENVIRONMENT, not in config.toml: no HTTP surface
// writes the environment, so an admin who can PUT the endpoint cannot also widen the set
// of hosts it may point at. Unset means no host is settable over HTTP at all, which is
// the fail-closed default and costs an operator nothing: MESH_EMBED_ENDPOINT,
// MESH_RERANK_ENDPOINT and MESH_SECRET_BRIDGE_URL each set their field directly and lock
// it against the config API, which is also how a sovereign http:// endpoint on localhost
// or the tailnet stays supported.
const AllowedEndpointHostsEnv = "MESH_ALLOWED_ENDPOINT_HOSTS"

// AllowedEndpointHosts returns the operator's endpoint host allow-list, lowercased. It
// accepts hosts separated by commas or whitespace, and tolerates a pasted full URL by
// reducing it to its host.
func AllowedEndpointHosts() []string {
	raw := os.Getenv(AllowedEndpointHostsEnv)
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		h := strings.ToLower(strings.TrimSpace(f))
		if strings.Contains(h, "://") {
			u, err := url.Parse(h)
			if err != nil || u.Hostname() == "" {
				continue
			}
			h = u.Hostname()
		}
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

// CheckEndpointURL validates one operator-settable endpoint URL. Empty is allowed and
// clears the field (which disables the stage). Anything else must parse, must be https,
// and must name a host on the allow-list. field is the config key, for the error message.
//
// It parses with net/url, the same parser http.NewRequest uses on the same string, so the
// host this vets is the host that gets dialed; an unparseable URL fails closed rather
// than being handed to the client to interpret.
func CheckEndpointURL(field, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s must be an absolute https URL", field)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%s must use https", field)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("%s must be an absolute https URL", field)
	}
	allowed := AllowedEndpointHosts()
	if len(allowed) == 0 {
		return fmt.Errorf("%s cannot be set here: no endpoint host is allow-listed (the operator sets %s, or the field's own env var)", field, AllowedEndpointHostsEnv)
	}
	for _, a := range allowed {
		if a == host {
			return nil
		}
	}
	return fmt.Errorf("%s host %q is not allow-listed; %s permits %s", field, host, AllowedEndpointHostsEnv, strings.Join(allowed, ", "))
}
