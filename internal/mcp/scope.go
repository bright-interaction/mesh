// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/vault"
)

// ScopeFilter is the per-request access-control context the hub injects so the MCP
// tools enforce scope: read filtering (search/fetch/neighbors/community/god_nodes)
// and write gating + stamping (append_note/write_entity). A nil filter (solo run, or
// a hub with no scopes configured) imposes no restriction, so behavior is unchanged.
type ScopeFilter struct {
	AllowedRead map[string]bool         // scopes the caller may read; nil = unrestricted
	WriteScope  string                  // scope new notes are stamped with ("" = caller must pass one)
	CanWrite    func(scope string) bool // may the caller write a note carrying this scope?
}

type scopeKey struct{}

// WithScopeFilter returns a context carrying sf, read by the tools via scopeFromCtx.
func WithScopeFilter(ctx context.Context, sf *ScopeFilter) context.Context {
	return context.WithValue(ctx, scopeKey{}, sf)
}

func scopeFromCtx(ctx context.Context) *ScopeFilter {
	if ctx == nil {
		return nil
	}
	sf, _ := ctx.Value(scopeKey{}).(*ScopeFilter)
	return sf
}

type writeCapKey struct{}

// WithWriteCapability marks the context with whether the caller may create or
// modify notes AT ALL (role-based, independent of scope). The hosted hub sets
// it on every request so a read-only (viewer) client is blocked from writing
// even when scope enforcement is off - the scope gate alone leaves sf nil in
// that mode and would otherwise skip the only write check. The trusted local /
// solo binary never sets it (the caller owns the vault), so writes stay open
// there.
func WithWriteCapability(ctx context.Context, canWrite bool) context.Context {
	return context.WithValue(ctx, writeCapKey{}, canWrite)
}

// writeAllowed returns (canWrite, explicitlySet). When no policy was set (local
// solo run), writes are allowed; a hosted request always sets an explicit value.
func writeAllowed(ctx context.Context) (canWrite, set bool) {
	if ctx == nil {
		return true, false
	}
	v, ok := ctx.Value(writeCapKey{}).(bool)
	if !ok {
		return true, false
	}
	return v, true
}

type localOperatorKey struct{}

// WithLocalOperator marks a request as coming from the TRUSTED LOCAL TRANSPORT:
// the stdio pipe of a `mesh mcp` process the operator spawned themselves. It is
// set by ServeStdio and by nothing else, so it is the only positive signal that
// the caller already owns this filesystem.
//
// It exists because the previous "are we hosted?" test was the WriteCapability
// marker, which only the hub sets. `mesh mcp --http` sets neither, so a tool
// meant to be local-only (mesh_setup_hooks, which writes .claude/settings.json
// under a caller-supplied directory on the SERVER host) failed OPEN on that
// transport: a bearer-token holder could drive server-side directory creation
// and edit a real project's hook config. Gating on the presence of the local
// marker instead of the absence of a hosted one fails closed for every transport
// that is not stdio.
func WithLocalOperator(ctx context.Context) context.Context {
	return context.WithValue(ctx, localOperatorKey{}, true)
}

// localOperator reports whether this request arrived on the trusted local stdio
// transport. Anything else (mesh mcp --http, the hosted hub, a bare context)
// is treated as remote.
func localOperator(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(localOperatorKey{}).(bool)
	return v
}

// allowsRead reports whether a note carrying these scopes is readable. A nil filter
// or nil AllowedRead = unrestricted. No scopes = the fail-safe dev default. The intersect
// logic lives in one place (vault.ScopeAllows) so it cannot drift per surface.
func (sf *ScopeFilter) allowsRead(scopes []string) bool {
	if sf == nil {
		return true
	}
	return vault.ScopeAllows(scopes, sf.AllowedRead)
}

// allowsNode reports whether a graph note node is readable under the filter, reading
// the scope it was indexed with (BuildGraph sets Attrs["scope"]).
func (sf *ScopeFilter) allowsNode(n *graph.Node) bool {
	if sf == nil || sf.AllowedRead == nil {
		return true
	}
	return sf.allowsRead(nodeScopes(n))
}

// nodeScopes returns a node's access scopes from its indexed Attrs (comma-joined),
// defaulting to the fail-safe dev when absent.
func nodeScopes(n *graph.Node) []string {
	if n == nil {
		return []string{"dev"}
	}
	sc, _ := n.Attrs["scope"].(string)
	var out []string
	for _, p := range strings.Split(sc, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return []string{"dev"}
	}
	return out
}
