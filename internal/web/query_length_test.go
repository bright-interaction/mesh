// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/mcp"
)

// TestSearchAPIRejectsAnOverlongQuery pins the web half of the uncapped query text,
// the twin of internal/mcp's TestFreeTextToolsRejectAnOverlongQuery. This is the more
// exposed of the two: on the default loopback `mesh ui` bind there is no auth, no
// rate limiter on /api/search and no Origin check, so any page open in the user's
// browser could fire these in a loop, and a 65001-byte q cost 1.07s of CPU each.
func TestSearchAPIRejectsAnOverlongQuery(t *testing.T) {
	s := bulkServer(t, 5)
	h := s.Handler()
	long := strings.Repeat("a", mcp.SearchQueryMaxBytes+1)

	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/search?q="+long, nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("/api/search with a %d-byte q = %d, want %d", len(long), rec.Code, http.StatusBadRequest)
	}
	body := rec.Body.String()
	for _, want := range []string{"too long", strconv.Itoa(len(long)), strconv.Itoa(mcp.SearchQueryMaxBytes)} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %q does not mention %q; an error a stranger can trigger must name the measurement and the remedy", strings.TrimSpace(body), want)
		}
	}

	// An ordinary query is unaffected.
	code, got := doJSON(t, h, "GET", "/api/search?q=cappable+corpus+filler", "")
	if code != http.StatusOK {
		t.Fatalf("an ordinary search = %d, want 200", code)
	}
	if _, ok := got["cards"]; !ok {
		t.Fatalf("an ordinary search must still return cards, got %v", got)
	}
}

// TestAskAPIRejectsAnOverlongQuestion covers the third caller of retrieval that takes
// free text. Its 16 KiB MaxBytesReader bounds the REQUEST, not the question, so the
// question itself takes the same cap the other two surfaces do.
func TestAskAPIRejectsAnOverlongQuestion(t *testing.T) {
	s := bulkServer(t, 5)
	h := s.Handler()
	payload, _ := json.Marshal(map[string]any{"question": strings.Repeat("b", mcp.SearchQueryMaxBytes+1)})

	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/ask", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("/api/ask with an over-long question = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, strings.TrimSpace(rec.Body.String()))
	}
	if !strings.Contains(rec.Body.String(), "too long") {
		t.Errorf("refusal %q must say the question is too long", strings.TrimSpace(rec.Body.String()))
	}
}
