// Package code parses source files into a lightweight symbol graph so Mesh can
// answer "where does this function live / what calls it" without a separate code
// indexer. It is the pure-Go replacement for graphify's source-code role: Go is
// parsed with the standard library go/ast (full defs + call graph + imports);
// every other language uses a pure-Go declaration scanner that locates symbols by
// name+kind+line (no cgo, so the static binary and the Linux hub container build
// unchanged). The package has no database or graph dependency: it turns paths into
// CodeFile values and lets the index package persist them.
package code

import "unicode/utf8"

// CodeFile is one parsed source file. Identity is the root-relative path; source
// files carry no stable frontmatter id, so a rename is a delete+add that the drift
// reconciler resolves by path, exactly as it does for an untitled note.
type CodeFile struct {
	Path    string   // root-relative, e.g. "dockyard/internal/handlers/deploy.go"
	Lang    string   // go|ts|tsx|js|jsx|mjs|cjs|svelte|astro|py
	Mtime   int64    // file mtime, unix seconds
	Package string   // Go package name; "" for other languages
	Imports []string // imported packages/modules, best-effort
	Symbols []Symbol
}

// Symbol is one declaration worth locating: a function, method, type, class, or
// exported const/var. Methods qualify their name with the receiver (Recv) so
// "Server.Search" is distinct from "Store.Search".
type Symbol struct {
	Name      string   // qualified identifier, e.g. "DeployHandler" or "Server.Search"
	Kind      string   // func|method|type|interface|struct|const|var|class|enum
	Start     int      // 1-based start line (the editor deep link target)
	End       int      // 1-based end line; Start when the extent is unknown
	Signature string   // one-line declaration, whitespace-collapsed and truncated
	Doc       string   // leading doc comment, trimmed; "" when absent
	Calls     []string // callee identifiers referenced in the body (Go only)
}

// sigMax caps a stored signature so a pathological multi-line declaration cannot
// bloat the FTS row; the deep link carries the reader to the full source anyway.
const sigMax = 240

// truncSig collapses whitespace in a raw declaration slice and caps its length.
// The cap walks back to a rune boundary so a multibyte rune is never sliced in half,
// which would store invalid UTF-8 into the code_search FTS5 table (the note path's
// NoteDocs already does this; this mirrors it).
func truncSig(s string) string {
	out := collapseWS(s)
	if len(out) <= sigMax {
		return out
	}
	cut := sigMax
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + "..."
}
