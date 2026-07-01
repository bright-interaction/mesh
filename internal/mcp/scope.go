// SPDX-License-Identifier: AGPL-3.0-or-later
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
