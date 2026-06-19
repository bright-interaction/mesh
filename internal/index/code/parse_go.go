package code

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// parseGo extracts Go symbols with the standard library AST: functions, methods
// (qualified by receiver type), named types (struct/interface/alias), exported
// package-level const/var, the package name, imports, and per-function callee
// names. The callee names are what gives Go a real call graph; resolution to
// target symbol ids happens at index time once every file's symbols are known.
// A parse error still yields a partial *ast.File, so a single broken file degrades
// to whatever parsed rather than dropping the package.
func parseGo(cf *CodeFile, src []byte) {
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, cf.Path, src, parser.ParseComments|parser.SkipObjectResolution)
	if f == nil {
		return
	}
	cf.Package = f.Name.Name
	for _, imp := range f.Imports {
		cf.Imports = append(cf.Imports, strings.Trim(imp.Path.Value, `"`))
	}
	line := func(p token.Pos) int { return fset.Position(p).Line }
	slice := func(from, to token.Pos) string {
		a, b := fset.Position(from).Offset, fset.Position(to).Offset
		if a < 0 || b > len(src) || a >= b {
			return ""
		}
		return string(src[a:b])
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := Symbol{Name: d.Name.Name, Kind: "func", Start: line(d.Pos()), End: line(d.End()), Doc: docText(d.Doc)}
			sigEnd := d.End()
			if d.Recv != nil && len(d.Recv.List) > 0 {
				sym.Kind = "method"
				sym.Name = recvName(d.Recv.List[0].Type) + "." + d.Name.Name
			}
			if d.Body != nil {
				sigEnd = d.Body.Lbrace // signature is everything up to the opening brace
				sym.Calls = callees(d.Body)
			}
			sym.Signature = truncSig(slice(d.Pos(), sigEnd))
			cf.Symbols = append(cf.Symbols, sym)
		case *ast.GenDecl:
			parseGenDecl(cf, d, line, slice)
		}
	}
}

func parseGenDecl(cf *CodeFile, d *ast.GenDecl, line func(token.Pos) int, slice func(a, b token.Pos) string) {
	switch d.Tok {
	case token.TYPE:
		for _, spec := range d.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			kind, sig := "type", "type "+ts.Name.Name
			switch ts.Type.(type) {
			case *ast.InterfaceType:
				kind, sig = "interface", "type "+ts.Name.Name+" interface"
			case *ast.StructType:
				kind, sig = "struct", "type "+ts.Name.Name+" struct"
			default:
				sig = "type " + slice(ts.Pos(), ts.End()) // alias/defined type keeps its underlying
			}
			doc := docText(ts.Doc)
			if doc == "" {
				doc = docText(d.Doc)
			}
			cf.Symbols = append(cf.Symbols, Symbol{
				Name: ts.Name.Name, Kind: kind,
				Start: line(ts.Pos()), End: line(ts.End()),
				Signature: truncSig(sig), Doc: doc,
			})
		}
	case token.CONST, token.VAR:
		kind := "const"
		if d.Tok == token.VAR {
			kind = "var"
		}
		for _, spec := range d.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if !name.IsExported() {
					continue // package-private const/var is noise nobody searches by name
				}
				doc := docText(vs.Doc)
				if doc == "" {
					doc = docText(d.Doc)
				}
				cf.Symbols = append(cf.Symbols, Symbol{
					Name: name.Name, Kind: kind,
					Start: line(name.Pos()), End: line(name.End()),
					Signature: truncSig(kind + " " + slice(name.Pos(), name.End())), Doc: doc,
				})
			}
		}
	}
}

// recvName unwraps a method receiver type to its bare name: *T, T, T[X], *T[X].
func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	}
	return ""
}

// callees returns the distinct callee identifiers in a function body: f() yields
// "f", x.Method() yields "Method". The bare name is intentionally package-agnostic
// so index-time resolution can match it against every same-named symbol; an
// over-broad name (resolves to many symbols) is dropped at resolution, not here.
func callees(body *ast.BlockStmt) []string {
	seen := map[string]bool{}
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		return true
	})
	return out
}

func docText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	return truncSig(g.Text())
}
