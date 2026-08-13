// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
)

// searchWatchdog is how long a search over a small vault is allowed to take before
// the test calls it a hang. Generous by three orders of magnitude: the fixed path
// answers these in single-digit milliseconds.
const searchWatchdog = 5 * time.Second

// timedSearch runs one search under a watchdog and reports how long it took. The
// search runs on its own goroutine so a genuine hang fails the test in
// searchWatchdog instead of pinning the test binary until the package timeout.
func timedSearch(t *testing.T, s *Store, query string, limit int) ([]SearchHit, time.Duration) {
	t.Helper()
	type result struct {
		hits []SearchHit
		err  error
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		h, err := s.Search(context.Background(), query, limit)
		ch <- result{h, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("search %q: %v", truncateForMsg(query), r.err)
		}
		return r.hits, time.Since(start)
	case <-time.After(searchWatchdog):
		t.Fatalf("search %q did not return within %s", truncateForMsg(query), searchWatchdog)
		return nil, 0
	}
}

func truncateForMsg(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:60] + fmt.Sprintf("... (%d bytes)", len(s))
}

// TestSearchDoesNotHangOnRepetitiveNote is the regression for the P1 hang.
//
// One multi-megabyte repetitive note is ordinary user data: a running deploy log, a
// concatenated chat export, a generated changelog. Nothing caps the size of an
// indexed note body, and FTS5's snippet() scores every match INSTANCE inside a
// matched document at roughly the square of the occurrence count. Measured with the
// system sqlite3 binary against a Mesh-written index: 5,087 occurrences 0.11s,
// 20,150 occurrences 1.56s, 80,106 occurrences 25.2s, and an 11 MB note spun at 98%
// CPU for over ten minutes and never answered. Under modernc, in process, it is
// worse again.
//
// Nothing could stop it either: the read used Query rather than QueryContext, the
// retriever dropped the caller's context, and sqlite3_interrupt only lands between
// VDBE opcodes while one snippet() call is a single opcode.
func TestSearchDoesNotHangOnRepetitiveNote(t *testing.T) {
	// Enough occurrences to be firmly past the measured knee, small enough that the
	// note itself is only a few hundred KB and indexes fast.
	const occurrences = 60000

	var body strings.Builder
	body.WriteString("---\nid: deploylog\ntype: note\nwhen: 2026-01-01\n---\n# Deploy log\n")
	for i := 0; i < occurrences; i++ {
		body.WriteString("deployline ok\n")
	}
	big, err := Parse("deploylog.md", []byte(body.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	other, _ := Parse("other.md", []byte("---\nid: other\ntype: note\nwhen: 2026-01-01\n---\n# Other\nnothing to see here\n"))
	notes := []*ParsedNote{big, other}
	g, _ := BuildGraph(notes)

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatalf("index: %v", err)
	}

	hits, elapsed := timedSearch(t, s, "deployline", 5)
	if len(hits) == 0 {
		t.Fatal("expected the repetitive note to still match: the cost fix must not cost recall")
	}
	if hits[0].NodeID != "note:deploylog" {
		t.Fatalf("top hit = %s, want note:deploylog", hits[0].NodeID)
	}
	// The excerpt must still be an excerpt with the match marked, not the raw body.
	if !strings.Contains(hits[0].Snippet, "["+"deployline"+"]") {
		t.Errorf("snippet = %q, want the matched term bracketed", truncateForMsg(hits[0].Snippet))
	}
	if len(hits[0].Snippet) > 4096 {
		t.Errorf("snippet is %d bytes: the excerpt must stay bounded, not carry the note body", len(hits[0].Snippet))
	}
	t.Logf("search over one note with %d occurrences of the term took %s", occurrences, elapsed)
}

// TestSearchBoundsAnUnboundedQuery is the regression for the uncapped query text.
//
// Every other search input (limit, budget, neighbors depth, changed_since) was
// clamped and single-sourced; the query itself was not, on any surface. The hub
// accepts a 1 MiB POST /mcp body from any peer with a valid team token at 20 req/s,
// and a 1048578-byte query cost 4 minutes 9 seconds of single-core CPU on a 500-note
// vault, running to completion after the client had hung up, because nothing checked
// ctx.Done(). On the default loopback `mesh ui` bind there is no auth, no rate limit
// and no Origin check on /api/search, so any page in the user's browser could fire
// 65001-byte queries (1.07s of CPU each) in a loop.
func TestSearchBoundsAnUnboundedQuery(t *testing.T) {
	notes := []*ParsedNote{}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("n%02d", i)
		pn, _ := Parse(id+".md", []byte("---\nid: "+id+"\ntype: note\nwhen: 2026-01-01\n---\n# Note "+id+
			"\ndeploy pipeline retention window rollback vault index retrieval snippet corpus\n"))
		notes = append(notes, pn)
	}
	g, _ := BuildGraph(notes)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatalf("index: %v", err)
	}

	// Exactly the shape the hub's 1 MiB body limit admits.
	var qb strings.Builder
	for i := 0; qb.Len() < 1<<20; i++ {
		fmt.Fprintf(&qb, "deploy%d ", i)
	}
	query := qb.String()

	if terms := graph.TokenizeQuery(query); len(terms) > graph.MaxQueryTerms {
		t.Fatalf("a %d-byte query produced %d search terms, want at most %d: the query text is the one input with no cap, and both keyword signals cost time linear in the term count",
			len(query), len(terms), graph.MaxQueryTerms)
	}
	if n := strings.Count(buildFTS5Query(query), " OR ") + 1; n > graph.MaxQueryTerms {
		t.Fatalf("the FTS5 MATCH expression carried %d phrases, want at most %d: FTS5 evaluates one phrase per term",
			n, graph.MaxQueryTerms)
	}

	_, elapsed := timedSearch(t, s, query, 10)
	t.Logf("a %d-byte query returned in %s", len(query), elapsed)
}

// TestNoUncancellableFTSRead is the guard, and it enumerates its subjects from the
// package's own AST rather than from a list somebody has to remember to update.
// Mesh has four hand-maintained guard tests that were each green over the exact
// defect they exist to catch, because each re-states its subject list and the list
// drifted. This one cannot drift: any FTS read added to this package later, on any
// surface, is found by walking the syntax tree.
//
// Two invariants, both of which the shipped code violated:
//   - an FTS MATCH read must go through a *Context method, so the caller's
//     cancellation and the server-side deadline actually reach SQLite;
//   - no SQL in this package may call FTS5's snippet(), whose cost grows with the
//     square of a document's match count and which cannot be interrupted.
func TestNoUncancellableFTSRead(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("parsed no packages: the guard would pass vacuously")
	}

	// Every string literal in the package is inspected wherever it appears, not only
	// where it is passed to a call. The scoped FTS query is assembled into a local
	// variable and only then handed to the driver, so a guard that looked at call
	// ARGUMENTS was blind to exactly the SQL it exists to police. That was verified by
	// putting snippet() back and watching this test stay green.
	checked, sawMatchSQL := 0, false
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			checked++
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if strings.Contains(lit.Value, "snippet(") {
					t.Errorf("%s: SQL calls FTS5 snippet(); build the excerpt in Go (see buildExcerpt) so the cost cannot grow with the square of a document's match count",
						fset.Position(lit.Pos()))
				}
				if strings.Contains(lit.Value, "MATCH") {
					sawMatchSQL = true
				}
				return true
			})

			// The functions that hold FTS MATCH SQL must read through a *Context method.
			// Scoped per enclosing function rather than per call so the SQL and the call
			// do not have to be the same expression.
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || !holdsMatchSQL(fn) {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch sel.Sel.Name {
					case "Query", "QueryRow":
						t.Errorf("%s: %s holds FTS MATCH SQL and reads it with %s, which cannot be cancelled; use %sContext with the caller's context (see withSearchDeadline)",
							fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name, sel.Sel.Name)
					}
					return true
				})
			}
		}
	}
	if checked == 0 {
		t.Fatal("walked no files: the guard would pass vacuously")
	}
	if !sawMatchSQL {
		t.Fatal("found no FTS MATCH SQL in this package: the guard is looking at the wrong thing and would pass vacuously")
	}
}

// holdsMatchSQL reports whether a function contains a string literal with an FTS5
// MATCH clause, i.e. whether it is one of the package's FTS readers.
func holdsMatchSQL(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "MATCH") {
			found = true
		}
		return !found
	})
	return found
}
