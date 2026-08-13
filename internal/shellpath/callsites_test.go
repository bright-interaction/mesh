// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package shellpath_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every path Mesh prints inside a runnable command must go through shellpath.Quote.
//
// This enumerates the print SITES, which is the thing that kept regressing. The rule
// has been "fixed" three times: once across 27 sites, once for two more the first pass
// missed, and once for five that a parallel branch added while the first pass was in
// flight. Each fix was correct and each was a snapshot, so the next new Printf landed
// unguarded. shellpath's own unit test proves Quote is right, and
// index/remedy_shell_quoting_test.go grades individual remedies through a real /bin/sh,
// but nothing until now could fail when a NEW site appeared.
//
// The check: in any non-test source file, a format string containing "mesh <subcommand>
// ... %s" must pass shellpath.Quote for that verb's argument, and the concatenation form
// ("mesh index " + x) must concatenate a Quote call.

// commandVerb matches a runnable mesh command followed by a %s that stands where a path
// goes. Anchored on the subcommand list rather than on "mesh %s", because plenty of
// prose mentions the word mesh.
var commandVerb = regexp.MustCompile(`\bmesh (?:index|init|embed|watch|doctor|new|install|sync|migrate|orient|lint|ask|search|join|conflicts|flywheel|health|status|code)\b[^"]*?%s`)

// commandConcat matches the other spelling: a literal that ENDS in a runnable command,
// with the path concatenated on. `fmt.Println("  mesh index " + root)`.
var commandConcat = regexp.MustCompile(`\bmesh (?:index|init|embed|watch|doctor|new|install|sync|migrate|orient|lint|ask|search|join|conflicts|flywheel|health|status|code) (?:--vault )?$`)

// allowed lists sites that deliberately print a path raw, with the reason. A new entry
// here is a decision, which is the point: it has to be argued for in review rather than
// happening by omission.
// Keyed by a distinctive substring of the format literal rather than by line number, so
// the entry survives the file moving around it.
var allowed = map[string]string{
	// The MCP config line is JSON handed to an exec with no shell in between, so shell
	// quoting would be wrong: the quotes would become part of the path.
	`"mcp", "--vault"`: "JSON exec'd with no shell in between",
	// The two verbs here are a hub URL and an invite token, neither of which is a path;
	// the vault argument on that line is the literal "my-vault".
	"mesh join %s %s my-vault": "hub URL and invite token, not paths",
}

// allowedSite reports whether lit is a site deliberately left unquoted.
func allowedSite(lit string) bool {
	for k := range allowed {
		if strings.Contains(lit, k) {
			return true
		}
	}
	return false
}

func TestEveryPrintedCommandQuotesItsPath(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// vendor and testdata are not ours; .git is not source.
			switch info.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // a file that does not parse is the compiler's problem, not ours
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, argStart := fmtCall(call)
			if name == "" {
				return true
			}
			for i := argStart; i < len(call.Args); i++ {
				parts, isConcat := flatten(call.Args[i])
				lit := strings.Join(parts, "")
				if lit == "" || allowedSite(lit) {
					continue
				}
				// Verb form: the %s that follows a command must be a Quote arg.
				if loc := commandVerb.FindStringIndex(lit); loc != nil && !isConcat {
					argIdx := i + 1 + countVerbs(lit[:loc[1]-2])
					if argIdx > i && argIdx < len(call.Args) && !isQuoteCall(call.Args[argIdx]) {
						violations = append(violations, describe(fset, call, rel, lit))
					}
					continue
				}
				// Concatenation form: "mesh index " + x   ->   x must be a Quote call.
				if isConcat {
					for j, p := range parts {
						if commandConcat.MatchString(p) && j+1 < len(parts) {
							if !concatOperandQuoted(call.Args[i], j) {
								violations = append(violations, describe(fset, call, rel, p))
							}
						}
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(violations) > 0 {
		t.Errorf("%d printed command(s) interpolate a path without shellpath.Quote.\n"+
			"A path with a space (~/Documents, iCloud Drive) makes the printed command split into\n"+
			"two arguments when the user pastes it. Wrap the argument in shellpath.Quote, or add the\n"+
			"site to `allowed` in this file with the reason it is safe.\n\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// The guard is worthless if it matches nothing, which is exactly how a site-enumerating
// check rots: a refactor renames the helper, every pattern stops matching, and the test
// goes green over a codebase with no quoting at all. Pin the floor.
func TestGuardActuallyMatchesRealSites(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	seen := 0
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, argStart := fmtCall(call); name != "" {
				for i := argStart; i < len(call.Args); i++ {
					parts, _ := flatten(call.Args[i])
					lit := strings.Join(parts, "")
					if commandVerb.MatchString(lit) || commandConcat.MatchString(lit) {
						seen++
					}
				}
			}
			return true
		})
		return nil
	})
	// The tree carried well over twenty such sites when this was written. Ten is a
	// floor that a real refactor can cross legitimately; zero or three cannot.
	if seen < 10 {
		t.Fatalf("the site scanner matched only %d printed commands; the patterns have rotted "+
			"and this guard is no longer checking anything", seen)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", dir, err)
	}
	return dir
}

// fmtCall reports the fmt function name and the index its variadic args start at, or ""
// when the call is not one of the printing functions.
func fmtCall(call *ast.CallExpr) (string, int) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", 0
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return "", 0
	}
	switch sel.Sel.Name {
	case "Printf", "Sprintf", "Errorf":
		return sel.Sel.Name, 0
	case "Fprintf":
		return sel.Sel.Name, 1 // the writer comes first
	case "Println", "Print", "Sprint", "Sprintln", "Fprintln":
		return sel.Sel.Name, 0
	}
	return "", 0
}

// flatten renders a string literal or a concatenation of them as its parts, reporting
// whether it was a concatenation. Non-literal operands become "" placeholders so the
// caller can still see where a value was spliced in.
func flatten(e ast.Expr) ([]string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				return nil, false
			}
			return []string{s}, false
		}
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return nil, false
		}
		l, _ := flatten(v.X)
		r, _ := flatten(v.Y)
		if l == nil {
			l = []string{""}
		}
		if r == nil {
			r = []string{""}
		}
		return append(l, r...), true
	}
	return nil, false
}

// concatOperandQuoted reports whether the operand following literal part `idx` in a
// concatenation is a shellpath.Quote call.
func concatOperandQuoted(e ast.Expr, idx int) bool {
	var operands []ast.Expr
	var walk func(ast.Expr)
	walk = func(x ast.Expr) {
		if b, ok := x.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			walk(b.X)
			walk(b.Y)
			return
		}
		operands = append(operands, x)
	}
	walk(e)
	if idx+1 < len(operands) {
		return isQuoteCall(operands[idx+1])
	}
	return false
}

// isQuoteCall reports whether e is shellpath.Quote(...). It deliberately does NOT accept
// any other quoting helper: a second implementation is the defect this package exists to
// end, and a private copy reappearing should fail here.
func isQuoteCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "shellpath" {
		return false
	}
	// PathArg as well as Quote. Accepting only Quote made the guard reject the safer of
	// the two helpers, so adopting PathArg at a positional site failed the build.
	return sel.Sel.Name == "Quote" || sel.Sel.Name == "PathArg"
}

func describe(fset *token.FileSet, call *ast.CallExpr, rel, lit string) string {
	pos := fset.Position(call.Pos())
	if len(lit) > 72 {
		lit = lit[:72] + "..."
	}
	return rel + ":" + strconv.Itoa(pos.Line) + ": " + strconv.Quote(lit)
}

// countVerbs counts the format verbs in s, so the %s that follows a printed command can
// be mapped to its argument. %% is an escaped percent and consumes no argument; a
// reordering directive (%[2]s) means the mapping cannot be trusted, and returning -1
// makes the caller skip rather than guess.
func countVerbs(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 >= len(s) {
			continue
		}
		switch s[i+1] {
		case '%':
			i++ // escaped percent, not a verb
		case '[':
			return -1 // explicit argument index; do not guess
		default:
			// Skip flags and width/precision to land on the verb letter itself.
			j := i + 1
			for j < len(s) && strings.ContainsRune("+-# 0123456789.*", rune(s[j])) {
				// A '*' takes its width or precision from an ARGUMENT, so %*d consumes
				// two and %.*f likewise. Counting them as one put every later index off
				// by one, which reads the wrong argument in both directions.
				if s[j] == '*' {
					n++
				}
				j++
			}
			if j < len(s) {
				n++
				i = j
			}
		}
	}
	return n
}
