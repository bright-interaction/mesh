// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/vault"
)

type Heading struct {
	Level  int
	Text   string
	Anchor string
	Line   int
}

type Link struct {
	Target string
	Alias  string
	Line   int
}

type Tag struct {
	Name string
	Line int
}

// ParsedNote is the deterministic structure extracted from a single markdown
// file: frontmatter, headings, wikilinks, and tags. No reasoning AI involved.
type ParsedNote struct {
	Path     string
	Key      string // lowercased basename without extension; the wikilink key
	FM       *vault.Frontmatter
	Raw      map[string]any
	Body     string
	Headings []Heading
	Links    []Link
	Tags     []Tag
	Mtime    int64 // file modification time (unix seconds); set by ParseFile from the on-disk file, 0 for byte-only Parse
}

// Issue is a non-fatal problem found while parsing or building the graph.
type Issue struct {
	Path string
	Kind string // missing-id|duplicate-id|broken-link
	Msg  string
}

func noteKey(path string) string {
	b := filepath.Base(path)
	b = strings.TrimSuffix(b, filepath.Ext(b))
	return strings.ToLower(b)
}

// linkKey normalizes a wikilink target to its lookup key: strips [[ ]], a .md
// extension, and any #heading anchor, then lowercases.
func linkKey(target string) string {
	t := strings.TrimSpace(target)
	t = strings.Trim(t, "[]")
	t = strings.TrimSpace(t)
	if i := strings.IndexByte(t, '#'); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSuffix(t, ".md")
	return strings.ToLower(strings.TrimSpace(t))
}

func ParseFile(path string) (*ParsedNote, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pn, err := Parse(path, data)
	if err != nil {
		return nil, err
	}
	// Capture mtime from the file we just read (path is the real, usually absolute,
	// path here), so the stored mtime is correct regardless of the process CWD. The
	// old fileMtime(pn.Path) stat'd the vault-relative path and returned 0 whenever
	// mesh ran outside the vault root - which is the normal MCP case (an agent spawns
	// `mesh mcp --vault <abs>` from its own dir) - silently breaking mesh_changed_since.
	if fi, err := os.Stat(path); err == nil {
		pn.Mtime = fi.ModTime().Unix()
	}
	return pn, nil
}

// Parse extracts the deterministic structure of a markdown document. Wikilinks
// and tags inside fenced code blocks and inline code spans are ignored.
func Parse(path string, data []byte) (*ParsedNote, error) {
	fmText, body, _ := vault.SplitFrontmatter(string(data))
	fm, raw, err := vault.ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	pn := &ParsedNote{Path: path, Key: noteKey(path), FM: fm, Raw: raw, Body: body}

	inFence := false
	for i, line := range strings.Split(body, "\n") {
		ln := i + 1
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		clean := stripInlineCode(line)
		if h, ok := parseHeading(clean); ok {
			h.Line = ln
			pn.Headings = append(pn.Headings, h)
			pn.appendLinks(clean, ln)
			continue
		}
		pn.appendLinks(clean, ln)
		pn.appendTags(clean, ln)
	}
	return pn, nil
}

func (pn *ParsedNote) appendLinks(line string, ln int) {
	for {
		open := strings.Index(line, "[[")
		if open < 0 {
			return
		}
		close := strings.Index(line[open:], "]]")
		if close < 0 {
			return
		}
		inner := line[open+2 : open+close]
		target, alias := inner, ""
		if p := strings.IndexByte(inner, '|'); p >= 0 {
			target, alias = inner[:p], inner[p+1:]
		}
		if t := strings.TrimSpace(target); t != "" {
			pn.Links = append(pn.Links, Link{Target: t, Alias: strings.TrimSpace(alias), Line: ln})
		}
		line = line[open+close+2:]
	}
}

func (pn *ParsedNote) appendTags(line string, ln int) {
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '#' {
			continue
		}
		if i > 0 && !isSpace(runes[i-1]) {
			continue
		}
		j := i + 1
		for j < len(runes) && isTagRune(runes[j]) {
			j++
		}
		if j > i+1 && isLetter(runes[i+1]) {
			pn.Tags = append(pn.Tags, Tag{Name: strings.ToLower(string(runes[i+1 : j])), Line: ln})
		}
		i = j
	}
}

func parseHeading(line string) (Heading, bool) {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(line) || line[i] != ' ' {
		return Heading{}, false
	}
	text := strings.TrimSpace(strings.TrimRight(line[i:], "# "))
	if text == "" {
		return Heading{}, false
	}
	return Heading{Level: i, Text: text, Anchor: vault.Slugify(text)}, true
}

func stripInlineCode(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	var b strings.Builder
	inCode := false
	for _, r := range line {
		if r == '`' {
			inCode = !inCode
			b.WriteByte(' ')
			continue
		}
		if inCode {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isSpace(r rune) bool  { return r == ' ' || r == '\t' }
func isLetter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }
func isTagRune(r rune) bool {
	return isLetter(r) || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '/'
}

// BuildGraph resolves a set of parsed notes into the in-memory graph. Node
// identity is the frontmatter id (falling back to the filename, with an issue
// raised so `mesh migrate` can fix it). Wikilinks resolve by basename to the
// target note's id, so edges survive a file rename (spec section 3.6).
func BuildGraph(notes []*ParsedNote) (*graph.Graph, []Issue) {
	g := graph.NewSized(len(notes))
	var issues []Issue

	idByKey := make(map[string]string, len(notes))
	pathByID := make(map[string]string, len(notes))
	for _, n := range notes {
		id := effectiveID(n)
		if n.FM.ID == "" {
			issues = append(issues, Issue{n.Path, "missing-id", "no frontmatter id; using filename (run mesh migrate)"})
		}
		if prev, ok := pathByID[id]; ok && prev != n.Path {
			issues = append(issues, Issue{n.Path, "duplicate-id", "id " + id + " already used by " + prev})
		}
		pathByID[id] = n.Path
		idByKey[n.Key] = id
	}

	for _, n := range notes {
		id := effectiveID(n)
		noteNode := "note:" + id
		title := n.FM.Title
		if title == "" {
			title = n.Key
		}
		attrs := map[string]any{"type": string(n.FM.Type), "scope": strings.Join(n.FM.EffectiveScopes(), ",")}
		for k, v := range map[string]string{"when": n.FM.When, "do": n.FM.Do, "dont": n.FM.Dont, "why": n.FM.Why} {
			if v != "" {
				attrs[k] = v
			}
		}
		g.AddNode(&graph.Node{ID: noteNode, Kind: "note", Label: title, NoteID: id, NotePath: n.Path, Attrs: attrs})

		for _, h := range n.Headings {
			if h.Anchor == "" {
				continue
			}
			hid := noteNode + "#" + h.Anchor
			g.AddNode(&graph.Node{ID: hid, Kind: "heading", Label: h.Text, NoteID: id, NotePath: n.Path, Anchor: h.Anchor, SourceLoc: locStr(h.Line)})
			g.AddEdge(graph.Edge{Source: noteNode, Target: hid, Relation: "contains", Confidence: graph.ConfExtracted, ConfidenceScore: 1, Weight: 1})
		}

		seenTag := map[string]bool{}
		addTag := func(name string) {
			name = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "#")))
			if name == "" || seenTag[name] {
				return
			}
			seenTag[name] = true
			tid := "tag:" + name
			g.AddNode(&graph.Node{ID: tid, Kind: "tag", Label: name})
			g.AddEdge(graph.Edge{Source: noteNode, Target: tid, Relation: "tagged", Confidence: graph.ConfExtracted, ConfidenceScore: 1, Weight: 1})
		}
		for _, t := range n.Tags {
			addTag(t.Name)
		}
		for _, t := range n.FM.Tags {
			addTag(t)
		}

		addRef := func(rawTarget string, line int) {
			key := linkKey(rawTarget)
			if key == "" {
				return
			}
			tid, ok := idByKey[key]
			if !ok {
				issues = append(issues, Issue{n.Path, "broken-link", "[[" + strings.TrimSpace(rawTarget) + "]] resolves to nothing"})
				return
			}
			g.AddEdge(graph.Edge{Source: noteNode, Target: "note:" + tid, Relation: "references", Confidence: graph.ConfExtracted, ConfidenceScore: 1, Weight: 1, SourceLoc: locStr(line)})
		}
		for _, l := range n.Links {
			addRef(l.Target, l.Line)
		}
		for _, r := range n.FM.Related {
			addRef(r, 0)
		}
	}
	// Degrees are computed in a final pass so they do not depend on the interleaved
	// AddNode/AddEdge order above (an edge to a later note would otherwise undercount
	// that note's inbound degree). This keeps BuildGraph's degrees identical to
	// LoadGraph's, so the MCP (BuildGraph) and CLI (LoadGraph) retrieval paths agree.
	g.RecomputeDegrees()
	return g, issues
}

func effectiveID(n *ParsedNote) string {
	if n.FM.ID != "" {
		return n.FM.ID
	}
	return n.Key
}

func locStr(line int) string {
	if line <= 0 {
		return ""
	}
	return "L" + strconv.Itoa(line)
}
