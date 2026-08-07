// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

// fakeActivityHub serves /v1/curation/activity the way internal/hub does, and is the
// only thing these tests assert against, so a client that agrees with it agrees with the
// hub: newest first; ?limit clamped to 1..200 with 50 as the default; ?cursor resuming
// strictly below a position a previous response handed out; and the next position in the
// X-Mesh-Curation-Cursor header, ABSENT once the history ended inside the page.
//
// The header name here is a literal on purpose. Setting it from the client's own
// constant would make the walk tests pass whatever that constant said, which is the one
// thing they exist to catch.
type fakeActivityHub struct {
	rows    []syncproto.CurationJob // newest first
	mu      sync.Mutex
	queries []string // raw query string of every request, in order
}

func (h *fakeActivityHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.queries = append(h.queries, r.URL.RawQuery)
		h.mu.Unlock()

		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit < 1 {
			limit = 50
		}
		limit = min(limit, 200)

		start := 0
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			at := -1
			for i, j := range h.rows {
				if cursor == activityCursorOf(j) {
					at = i
					break
				}
			}
			if at < 0 {
				http.Error(w, "invalid cursor", http.StatusBadRequest)
				return
			}
			start = at + 1
		}
		end := min(start+limit, len(h.rows))
		page := h.rows[start:end]
		// A page that came back exactly full carries a cursor even when it emptied the
		// history: the hub cannot tell a full page from the last one, so following that
		// cursor lands on an empty, cursorless page rather than repeating rows.
		if len(page) == limit {
			w.Header().Set("X-Mesh-Curation-Cursor", activityCursorOf(page[len(page)-1]))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.CurationJobsResponse{Jobs: page})
	})
}

func (h *fakeActivityHub) sentQueries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.queries...)
}

// activityCursorOf is the hub's wire form: "<touched_at>.<id>".
func activityCursorOf(j syncproto.CurationJob) string {
	touched := j.ResolvedAt
	if touched == 0 {
		touched = j.CreatedAt
	}
	return fmt.Sprintf("%d.%d", touched, j.ID)
}

// activityHistory builds a newest-first history of `total` terminal jobs whose OLDEST
// row is the failed one. That is the real shape (failures are rare, so they sink), and
// with total above the hub's 200-row page clamp it is a row no un-paged client can see.
func activityHistory(total int) []syncproto.CurationJob {
	rows := make([]syncproto.CurationJob, 0, total)
	for i := 0; i < total; i++ {
		id := int64(total - i)
		j := syncproto.CurationJob{
			ID:         id,
			Path:       fmt.Sprintf("notes/n%04d.md", i),
			Status:     "resolved",
			CreatedAt:  1000 + id,
			ResolvedAt: 1000 + id,
		}
		if i == total-1 {
			j.Path, j.Status, j.ResolvedAt = "notes/stuck.md", "failed", 0
		}
		rows = append(rows, j)
	}
	return rows
}

func newActivityClient(t *testing.T, rows []syncproto.CurationJob) (*Client, *fakeActivityHub) {
	t.Helper()
	h := &fakeActivityHub{rows: rows}
	ts := httptest.NewServer(h.handler())
	t.Cleanup(ts.Close)
	return New(ts.URL, "test-token"), h
}

// TestCurationActivityPageReachesBelowTheFirstPage is the regression for the missing
// half of the client. The hub clamps one page to 200 rows and grew a ?cursor so the
// rows below it stay reachable; the client never sent one, so `mesh curator log` could
// still only ever see the newest page, and the failed job at the bottom of a long
// history (the only kind that needs a human) was invisible from every client.
//
// The control case is the point: ONE call, with any limit at all, cannot see the target.
func TestCurationActivityPageReachesBelowTheFirstPage(t *testing.T) {
	const total = 250 // past the hub's 200-row page clamp
	const target = "notes/stuck.md"

	t.Run("one page cannot reach it whatever the limit", func(t *testing.T) {
		for _, limit := range []int{0, 50, 200, 1000} {
			t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
				cl, _ := newActivityClient(t, activityHistory(total))
				jobs, next, err := cl.CurationActivityPage("", limit)
				if err != nil {
					t.Fatal(err)
				}
				if len(jobs) > CurationActivityPageSize {
					t.Fatalf("one page returned %d rows, above the hub's %d clamp", len(jobs), CurationActivityPageSize)
				}
				if pathsContain(jobs, target) {
					t.Fatalf("the fixture is wrong: %s must not fit on the first page", target)
				}
				if next == "" {
					t.Fatal("the hub had more history but handed back no cursor; the fixture does not model the hub")
				}
			})
		}
	})

	tests := []struct {
		name     string
		pageSize int
	}{
		{name: "the smallest page the hub serves", pageSize: 1},
		{name: "an odd page that never divides the history", pageSize: 7},
		{name: "the hub's page clamp", pageSize: 200},
		{name: "a limit above the clamp is clamped, not honoured", pageSize: 1000},
		{name: "no limit at all uses the hub default", pageSize: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl, _ := newActivityClient(t, activityHistory(total))
			var walked []string
			seen := map[string]int{}
			cursor := ""
			for req := 0; req < total+10; req++ {
				jobs, next, err := cl.CurationActivityPage(cursor, tc.pageSize)
				if err != nil {
					t.Fatal(err)
				}
				for _, j := range jobs {
					walked = append(walked, j.Path)
					seen[j.Path]++
				}
				if next == "" {
					break
				}
				if next == cursor {
					t.Fatalf("request %d handed back the cursor it was given (%s); the walk cannot advance", req, next)
				}
				cursor = next
			}
			if len(walked) != total {
				t.Fatalf("the walk visited %d row(s), want the whole history of %d", len(walked), total)
			}
			for p, n := range seen {
				if n != 1 {
					t.Fatalf("the walk visited %s %d times, want exactly 1", p, n)
				}
			}
			if walked[0] != "notes/n0000.md" || walked[total-1] != target {
				t.Fatalf("the walk ran %s .. %s, want newest-first ending at %s", walked[0], walked[total-1], target)
			}
		})
	}
}

// TestCurationActivityPageStopsOnTheCursorNotTheRowCount pins the half of the contract
// that is easy to get wrong in the caller: the hub fences rows the token may not read,
// so a page can be SHORT or EMPTY with readable rows still below it. Only the absent
// cursor ends a walk. A caller that stops on an empty page loses everything under a
// fenced run, which is the same unreachable-tail defect one layer along.
func TestCurationActivityPageStopsOnTheCursorNotTheRowCount(t *testing.T) {
	tests := []struct {
		name       string
		rows       []syncproto.CurationJob
		cursor     string
		wantCursor bool
	}{
		{
			name:       "an empty page still carries a cursor",
			rows:       nil,
			cursor:     "1700000000.9",
			wantCursor: true,
		},
		{
			name:       "a short page still carries a cursor",
			rows:       []syncproto.CurationJob{{ID: 4, Path: "notes/a.md", Status: "failed", CreatedAt: 5}},
			cursor:     "1700000000.9",
			wantCursor: true,
		},
		{
			name:       "the end of the history carries none",
			rows:       []syncproto.CurationJob{{ID: 3, Path: "notes/b.md", Status: "resolved", CreatedAt: 4, ResolvedAt: 4}},
			wantCursor: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.wantCursor {
					w.Header().Set("X-Mesh-Curation-Cursor", tc.cursor)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(syncproto.CurationJobsResponse{Jobs: tc.rows})
			}))
			defer ts.Close()
			jobs, next, err := New(ts.URL, "t").CurationActivityPage("", 50)
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != len(tc.rows) {
				t.Fatalf("decoded %d row(s), want %d", len(jobs), len(tc.rows))
			}
			if tc.wantCursor && next != tc.cursor {
				t.Fatalf("next cursor = %q, want %q (a walk keyed on the row count stops here and never reaches the tail)", next, tc.cursor)
			}
			if !tc.wantCursor && next != "" {
				t.Fatalf("next cursor = %q, want empty at the end of the history", next)
			}
		})
	}
}

// TestCurationActivityPageSendsTheHubsQueryShape pins the request side against the
// hub's parser: the parameter is ?cursor, it is sent verbatim in the hub's
// "<touched_at>.<id>" form, and limit is only sent when the caller set one (the hub
// applies its own default otherwise). internal/hub answers 400 to a cursor it cannot
// parse, so a client that mangled it would fail loudly rather than silently restart at
// the newest page; the last case pins that the 400 surfaces as an error and not as an
// empty log.
func TestCurationActivityPageSendsTheHubsQueryShape(t *testing.T) {
	tests := []struct {
		name      string
		cursor    string
		limit     int
		wantQuery string
	}{
		{name: "newest page, hub default limit", wantQuery: ""},
		{name: "newest page with a limit", limit: 200, wantQuery: "limit=200"},
		{name: "a cursor with no limit", cursor: "1700000000.42", wantQuery: "cursor=1700000000.42"},
		{name: "cursor and limit together", cursor: "1700000000.42", limit: 200, wantQuery: "cursor=1700000000.42&limit=200"},
		{name: "a non-positive limit is left to the hub", cursor: "1700000000.42", limit: 0, wantQuery: "cursor=1700000000.42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl, hub := newActivityClient(t, activityHistory(3))
			if _, _, err := cl.CurationActivityPage(tc.cursor, tc.limit); err != nil && tc.cursor == "" {
				t.Fatal(err)
			}
			got := hub.sentQueries()
			if len(got) != 1 {
				t.Fatalf("sent %d request(s), want 1", len(got))
			}
			if got[0] != tc.wantQuery {
				t.Fatalf("query = %q, want %q", got[0], tc.wantQuery)
			}
		})
	}

	t.Run("a cursor the hub refuses is an error, not an empty log", func(t *testing.T) {
		cl, _ := newActivityClient(t, activityHistory(3))
		jobs, next, err := cl.CurationActivityPage("not-a-cursor", 50)
		if err == nil {
			t.Fatalf("a refused cursor returned %d row(s) and cursor %q with no error", len(jobs), next)
		}
	})
}

// TestCurationActivityCursorHeaderMatchesTheHub pins the client's header name against
// the hub's own constant. They are declared in two packages that cannot import each
// other (internal/hub is the pro overlay and pkg/meshclient is open core, and the
// open-core boundary check refuses that import), so nothing but this test stops the two
// from drifting. Drift is silent in the worst way: the client would read "" from every
// response, conclude the history ended, and go back to seeing only the newest page.
//
// The hub source is parsed WITHOUT parser.ParseComments, so comments never enter the
// AST and only a real const declaration can satisfy it.
func TestCurationActivityCursorHeaderMatchesTheHub(t *testing.T) {
	got, ok := hubStringConst(t, "curation.go", "curationActivityCursorHeader")
	if !ok {
		t.Skip("internal/hub is not in this tree (open-core mirror); the pin runs where the hub lives")
	}
	if got != curationActivityCursorHeader {
		t.Fatalf("the hub sends its activity cursor in %q but this client reads %q; every page would look like the last one", got, curationActivityCursorHeader)
	}
}

// hubStringConst returns the value of a string const declared in internal/hub, and false
// when that package is absent from the tree.
func hubStringConst(t *testing.T, file, name string) (string, bool) {
	t.Helper()
	src := filepath.Join("..", "..", "internal", "hub", file)
	if _, err := os.Stat(src); err != nil {
		return "", false
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, src, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	for _, decl := range parsed.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, isLit := vs.Values[i].(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					t.Fatalf("%s: const %s is not a string literal", src, name)
				}
				v, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					t.Fatalf("%s: const %s: %v", src, name, uerr)
				}
				return v, true
			}
		}
	}
	t.Fatalf("%s: const %s not found; the hub's activity cursor contract moved and this client did not", src, name)
	return "", false
}

func pathsContain(jobs []syncproto.CurationJob, path string) bool {
	for _, j := range jobs {
		if strings.EqualFold(j.Path, path) {
			return true
		}
	}
	return false
}
