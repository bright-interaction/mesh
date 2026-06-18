package web

import (
	"embed"
	"net/http"
	"sort"
	"strings"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
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
