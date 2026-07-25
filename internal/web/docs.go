// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"embed"
	tmpl "html/template"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
	"github.com/microcosm-cc/bluemonday"
)

//go:embed docs
var docsFS embed.FS

type docPage struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

func docList() []docPage {
	entries, _ := docsFS.ReadDir("docs")
	pages := make([]docPage, 0, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		slug := strings.TrimSuffix(e.Name(), ".md")
		pages = append(pages, docPage{Slug: slug, Title: docTitle(slug)})
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Slug < pages[j].Slug })
	return pages
}

// docTitle is the first H1 of a doc, falling back to the slug.
func docTitle(slug string) string {
	b, err := docsFS.ReadFile("docs/" + slug + ".md")
	if err != nil {
		return slug
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return slug
}

// renderMD turns trusted, embedded markdown into HTML. The source is authored by us
// and compiled into the binary, never user input, so no sanitisation is needed.
func renderMD(src []byte) string {
	p := parser.NewWithExtensions(parser.CommonExtensions | parser.AutoHeadingIDs)
	doc := p.Parse(src)
	r := html.NewRenderer(html.RendererOptions{Flags: html.CommonFlags})
	return string(markdown.Render(doc, r))
}

// codeLangClass is the exact shape gomarkdown emits for a fenced code block's info
// string, class="language-<info>". The stylesheet keys off it, so it is allow-listed
// back in, but only anchored to this shape: the info string is markdown-controlled and
// the renderer concatenates it into the attribute unescaped.
var codeLangClass = regexp.MustCompile(`^language-[A-Za-z0-9_+.#-]+$`)

// safeLangChars is what may survive from a fenced block's info string into the class.
var safeLangChars = regexp.MustCompile(`^[A-Za-z0-9_+.#-]+$`)

// infoLang extracts the language token from a fence info string, or "" if it is not a
// plain identifier. gomarkdown truncates the info at the first space or tab, so a
// payload has to be whitespace-free, which "lang\"><img/src=x/onerror=alert(1)>" is.
func infoLang(info string) string {
	if i := strings.IndexAny(info, " \t"); i >= 0 {
		info = info[:i]
	}
	if info == "" || !safeLangChars.MatchString(info) {
		return ""
	}
	return info
}

// renderCodeBlockSafe replaces gomarkdown's fenced-code renderer for untrusted input.
// The stock one builds `class="language-` + info + `"` by string concatenation with no
// escaping (html/renderer.go), so an info string containing a quote closes the attribute
// and everything after it becomes markup. Rendering the block ourselves means the info
// string is either a plain identifier or dropped entirely, and it never reaches the
// output as anything but a class token. Returns handled=false for every other node so
// the stock renderer keeps doing the rest.
func renderCodeBlockSafe(w io.Writer, node ast.Node, entering bool) (ast.WalkStatus, bool) {
	cb, ok := node.(*ast.CodeBlock)
	if !ok {
		return ast.GoToNext, false
	}
	if !entering {
		return ast.GoToNext, true // leaf node: the walk visits it twice, emit once
	}
	if lang := infoLang(string(cb.Info)); lang != "" {
		_, _ = io.WriteString(w, `<pre><code class="language-`+lang+`">`)
	} else {
		_, _ = io.WriteString(w, "<pre><code>")
	}
	tmpl.HTMLEscape(w, cb.Literal)
	_, _ = io.WriteString(w, "</code></pre>\n")
	return ast.GoToNext, true
}

// safeHTMLPolicy is the allow-list sanitiser applied to rendered UNTRUSTED markdown.
// gomarkdown's SkipHTML|Safelink flags are NOT a sanitiser: they only drop HTML block
// and span AST nodes, so the two places the renderer writes markdown-controlled text
// straight into an HTML attribute survive them, namely the fenced-code info string
// (class="language-...") and a {#custom-id} heading id. Both let a note body close the
// attribute and inject a live tag, which the SPA then assigns to innerHTML. bluemonday
// re-parses and re-serialises the output, so an injected element is dropped and every
// surviving attribute value is escaped. Built once: a Policy is read-only after
// construction and safe for concurrent use.
var safeHTMLPolicy = newSafeHTMLPolicy()

func newSafeHTMLPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("class").Matching(codeLangClass).OnElements("code", "pre")
	return p
}

// renderMDSafe turns UNTRUSTED markdown into HTML. Note bodies can embed raw HTML
// ingested from GitHub/Jira/Linear/Notion/Slack, so a stored <script>, a javascript:
// link, or an attribute-injected onerror handler would execute in a reader's browser
// if rendered with renderMD. Three layers: SkipHTML drops inline HTML, Safelink
// neutralises unsafe link schemes, and safeHTMLPolicy allow-lists what actually ships.
// HeadingIDs is also removed from the extension set (CommonExtensions turns it on), so
// a {#custom-id} is never honoured at all; AutoHeadingIDs stays because its slugs are
// derived, not attacker-supplied. renderCodeBlockSafe takes over the other unescaped
// attribute path, the fence info string. Use this for anything that flows in from a
// connector; renderMD stays for our embedded docs.
func renderMDSafe(src []byte) string {
	p := parser.NewWithExtensions((parser.CommonExtensions &^ parser.HeadingIDs) | parser.AutoHeadingIDs)
	doc := p.Parse(src)
	r := html.NewRenderer(html.RendererOptions{
		Flags:          html.CommonFlags | html.SkipHTML | html.Safelink,
		RenderNodeHook: renderCodeBlockSafe,
	})
	return string(safeHTMLPolicy.SanitizeBytes(markdown.Render(doc, r)))
}

func (s *Server) handleDocsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"pages": docList()})
}

func (s *Server) handleDoc(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" || strings.ContainsAny(slug, "/.") {
		http.NotFound(w, r)
		return
	}
	b, err := docsFS.ReadFile("docs/" + slug + ".md")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]any{"slug": slug, "title": docTitle(slug), "html": renderMD(b)})
}
