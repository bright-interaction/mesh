// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestFreeTextToolsRejectAnOverlongQuery pins the entry-point half of the uncapped
// query defect. limit, budget, neighbors depth and changed_since were all clamped and
// single-sourced; the query TEXT was not, on any surface. POST /mcp accepts a 1 MiB
// body from any peer holding a valid team token at 20 req/s per peer, and a
// 1048578-byte query took 4 minutes 9 seconds of single-core CPU on a 500-note vault,
// running to completion after the client had hung up.
//
// The refusal has to name the measurement and the remedy, because the caller that
// trips it is usually an agent that will otherwise retry the same call forever.
func TestFreeTextToolsRejectAnOverlongQuery(t *testing.T) {
	s := serverWithFiles(t, map[string]string{
		"notes/a.md": "---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\ndeploy pipeline rollback\n",
	})
	long := strings.Repeat("deploy ", searchQueryMaxBytes)

	for _, tool := range freeTextQueryTools() {
		t.Run(tool, func(t *testing.T) {
			args, _ := json.Marshal(map[string]any{"name": tool, "arguments": map[string]any{"query": long}})
			_, rerr := s.dispatch(WithLocalOperator(context.Background()),
				request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: args})
			if rerr == nil {
				t.Fatalf("%s accepted a %d-byte query; it must be refused", tool, len(long))
			}
			if rerr.Code != codeInvalidParams {
				t.Errorf("%s error code = %d, want %d (invalid params)", tool, rerr.Code, codeInvalidParams)
			}
			for _, want := range []string{"too long", strconv.Itoa(len(long)), strconv.Itoa(searchQueryMaxBytes)} {
				if !strings.Contains(rerr.Message, want) {
					t.Errorf("%s refusal %q does not mention %q; an error a stranger can trigger must name the measurement and the remedy", tool, rerr.Message, want)
				}
			}
		})
	}

	// A normal question is unaffected.
	out := toolText(t, call(t, s, "tools/call", map[string]any{
		"name": "mesh_search", "arguments": map[string]any{"query": "deploy pipeline rollback"}}))
	if _, ok := out["cards"]; !ok {
		t.Fatalf("an ordinary query must still search, got %v", out)
	}
}

// freeTextQueryTools enumerates the advertised tools that take a free-text "query"
// from the tool SPECS rather than from a list in this file, so a tool added later
// without the cap fails here instead of shipping uncapped. Mesh already has four
// guard tests that went green over the exact defect they exist to catch, each
// because it re-states its subject list by hand.
func freeTextQueryTools() []string {
	var out []string
	for _, spec := range ToolSpecs() {
		name, _ := spec["name"].(string)
		schema, _ := spec["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["query"]; ok && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// TestQueryCapIsSingleSourced keeps the exported alias the web surface reads pinned
// to the internal constant. The numeric caps were exported this way precisely so the
// two surfaces over one retriever could not drift apart by editing one number, and
// the text cap has to follow the same rule.
func TestQueryCapIsSingleSourced(t *testing.T) {
	if SearchQueryMaxBytes != searchQueryMaxBytes {
		t.Fatalf("SearchQueryMaxBytes = %d, searchQueryMaxBytes = %d: the exported alias must be the same number the tools enforce",
			SearchQueryMaxBytes, searchQueryMaxBytes)
	}
	// checkQueryLength is the one place the refusal is worded; make sure it is what
	// the tools actually call rather than a copy per tool.
	if got := runtime.FuncForPC(reflect.ValueOf(checkQueryLength).Pointer()).Name(); !strings.HasSuffix(got, ".checkQueryLength") {
		t.Fatalf("unexpected shared checker %q", got)
	}
}
