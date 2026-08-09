// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/mcp"
)

// bulkServer builds a vault of n notes that all match one query, so a test can see what
// an uncapped /api/search would actually serve. The bodies are deliberately long: real
// notes are, and a corpus of one-line notes packs under any budget, which would make the
// budget half of this file pass whether the budget is applied or not.
func bulkServer(t *testing.T, n int) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	prose := strings.Repeat("cappable corpus filler deploy pipeline retention window rollback ", 40)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bulk-%04d", i)
		body := "---\nid: " + id + "\ntype: note\nwhen: 2026-01-01\n---\n# " + id + "\n" + prose + "\n"
		if err := os.WriteFile(filepath.Join(dir, "notes", id+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestSearchAPICapsLimit pins the cap the web twin never got. Its MCP counterpart has
// clamped for a while (internal/mcp: clampLimit(a.Limit, searchLimitDefault,
// searchLimitMax)), but GET /api/search read the limit straight off the query string and
// passed budget=0, and retrieve.Retrieve only FLOORS the limit and skips packToBudget
// entirely when the budget is 0. Measured on a 400-note vault, ?q=deploy returned 12
// cards / 974 tokens while ?q=deploy&limit=100000 returned 401 cards / 30950 tokens /
// 116654 bytes, all of it corpus-sized retrieval work per request.
func TestSearchAPICapsLimit(t *testing.T) {
	s := bulkServer(t, mcp.SearchLimitMax+60)
	h := s.Handler()

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"unbounded limit is clamped", "&limit=100000", mcp.SearchLimitMax},
		{"limit just over the cap is clamped", fmt.Sprintf("&limit=%d", mcp.SearchLimitMax+1), mcp.SearchLimitMax},
		{"no limit uses the shared default", "", mcp.SearchLimitDefault},
		{"a small explicit limit is honored", "&limit=5", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, got := doJSON(t, h, "GET", "/api/search?q=cappable+corpus+filler"+tc.query, "")
			if code != http.StatusOK {
				t.Fatalf("search = %d", code)
			}
			cards, _ := got["cards"].([]any)
			if len(cards) > tc.want {
				t.Fatalf("/api/search%s returned %d cards, want at most %d", tc.query, len(cards), tc.want)
			}
		})
	}
}

// TestSearchAPIAppliesTheSharedBudget pins the second half of the same defect. budget
// defaulted to 0 here, which is the one value that makes retrieve skip packToBudget
// completely, so nothing bounded the RESPONSE SIZE even once the card count was
// reasonable. It defaults to the MCP surface's budget now, and the packer is what keeps
// a 100-card reply from being a 100-card dump.
func TestSearchAPIAppliesTheSharedBudget(t *testing.T) {
	s := bulkServer(t, mcp.SearchLimitMax+60)
	h := s.Handler()

	code, got := doJSON(t, h, "GET", fmt.Sprintf("/api/search?q=cappable+corpus+filler&limit=%d", mcp.SearchLimitMax), "")
	if code != http.StatusOK {
		t.Fatalf("search = %d", code)
	}
	tokens, ok := got["tokens"].(float64)
	if !ok {
		t.Fatalf("no token count in the response: %v", got)
	}
	if int(tokens) > mcp.SearchBudgetDefault {
		t.Fatalf("response packed %d tokens, over the shared budget of %d", int(tokens), mcp.SearchBudgetDefault)
	}
}
