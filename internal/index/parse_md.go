// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"regexp"
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
	// Issues found while reading this one file, folded into BuildGraph's list so lint,
	// doctor and health surface them next to the graph-level ones.
	Issues []Issue
}

// Issue is a non-fatal problem found while parsing or building the graph.
type Issue struct {
	Path string
	Kind string // missing-id|duplicate-id|broken-link|ambiguous-link-key|ambiguous-link|unterminated-comment
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
	// A markdown-escaped closing bracket, [[note\]], leaves a trailing backslash on the
	// target, so the link resolved to nothing and showed up as a dangling reference to a
	// note whose name ended in "\". Authors escape brackets when a renderer would
	// otherwise eat them, and they mean the note, not a different one.
	t = strings.TrimRight(t, `\`)
	t = strings.TrimSpace(t)
	if i := strings.IndexByte(t, '#'); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSuffix(t, ".md")
	return strings.ToLower(strings.TrimSpace(t))
}

// pathLinkKey normalizes a note's vault-relative path into the key a slash-bearing
// wikilink resolves against: forward slashes, no .md extension, lowercased. It mirrors
// linkKey so [[decisions/deploy]] and decisions/deploy.md meet on the same string.
func pathLinkKey(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.TrimSuffix(p, ".md")
	return strings.ToLower(strings.TrimSpace(p))
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
// and tags inside fenced code blocks, inline code spans, and HTML comments are
// ignored.
func Parse(path string, data []byte) (*ParsedNote, error) {
	fmText, body, _ := vault.SplitFrontmatter(string(data))
	fm, raw, err := vault.ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	pn := &ParsedNote{Path: path, Key: noteKey(path), FM: fm, Raw: raw, Body: body}

	// An HTML comment is not content: what it hides is invisible to every reader, so the
	// wikilinks and tags inside one are not real. Fences, code spans and comments are all
	// resolved in one pass (vault.StripNonContent) because they can only be told apart
	// together, and the same pass feeds the FTS body, the embedding chunks and migrate,
	// so every part of Mesh agrees on what a note says.
	clean, openComment := vault.StripNonContent(body)
	if openComment > 0 {
		// Everything below an unterminated marker drops out of the graph AND out of
		// search. That used to happen silently, which is the worst version of it: the note
		// looks indexed and half of it simply is not there.
		pn.Issues = append(pn.Issues, Issue{path, "unterminated-comment",
			"<!-- on line " + strconv.Itoa(openComment) + " is never closed, so the rest of the note is hidden from the graph and from search; close it with -->"})
	}
	for i, line := range strings.Split(clean, "\n") {
		ln := i + 1
		if h, ok := parseHeading(line); ok {
			h.Line = ln
			pn.Headings = append(pn.Headings, h)
			pn.appendLinks(line, ln)
			continue
		}
		pn.appendLinks(line, ln)
		pn.appendTags(line, ln)
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
	// A wikilink key is the lowercased basename, so two notes in different folders that
	// share a basename (with distinct frontmatter ids) collide on it. A plain
	// last-wins assignment handed the key to whichever note happened to be walked last,
	// which then absorbed EVERY [[basename]] and related: edge in the vault while the
	// other note silently received none, and no Issue was raised so nothing surfaced it.
	// Track the owning path per key so a genuine collision is reported and refused
	// rather than resolved to an arbitrary winner.
	pathByKey := make(map[string]string, len(notes))
	ambiguousKeys := map[string][]string{}
	// idByPath resolves a wikilink that names a path (contains a slash): the author has
	// disambiguated explicitly, so an exact vault-relative path match wins over the
	// basename key. This is also the escape hatch out of an ambiguous key.
	idByPath := make(map[string]string, len(notes))
	for _, n := range notes {
		id := effectiveID(n)
		issues = append(issues, n.Issues...) // what Parse found in the file itself
		if n.FM.ID == "" {
			issues = append(issues, Issue{n.Path, "missing-id", "no frontmatter id; using filename (run mesh migrate)"})
		}
		if prev, ok := pathByID[id]; ok && prev != n.Path {
			issues = append(issues, Issue{n.Path, "duplicate-id", "id " + id + " already used by " + prev})
		}
		// Only a collision between DIFFERENT ids is ambiguous. Two un-id'd files sharing
		// a basename resolve to the same effectiveID, which is the duplicate-id case
		// above and collapses to one node, so the key still points somewhere correct.
		if prevID, ok := idByKey[n.Key]; ok && prevID != id {
			if len(ambiguousKeys[n.Key]) == 0 {
				ambiguousKeys[n.Key] = []string{pathByKey[n.Key]}
			}
			ambiguousKeys[n.Key] = append(ambiguousKeys[n.Key], n.Path)
			issues = append(issues, Issue{n.Path, "ambiguous-link-key",
				"[[" + n.Key + "]] matches both " + pathByKey[n.Key] + " and " + n.Path + "; links using this name resolve to neither"})
		}
		pathByID[id] = n.Path
		pathByKey[n.Key] = n.Path
		idByKey[n.Key] = id
		idByPath[pathLinkKey(n.Path)] = id
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
			// A slash in the target means the author named a path, so resolve it exactly
			// first: that is both the normal folder-qualified link and the way out of an
			// ambiguous basename below.
			if strings.Contains(key, "/") {
				if tid, ok := idByPath[key]; ok {
					g.AddEdge(graph.Edge{Source: noteNode, Target: "note:" + tid, Relation: "references", Confidence: graph.ConfExtracted, ConfidenceScore: 1, Weight: 1, SourceLoc: locStr(line)})
					return
				}
			}
			// Refuse to bind an ambiguous key to an arbitrary winner: guessing produced a
			// wrong edge (backlinks that cited one note landed on another) which is worse
			// than no edge, and reported nothing. Report it so mesh lint / mesh structure
			// / mesh_health can surface it and the author can qualify the link by path.
			if owners, amb := ambiguousKeys[key]; amb {
				issues = append(issues, Issue{n.Path, "ambiguous-link",
					"[[" + strings.TrimSpace(rawTarget) + "]] matches " + strings.Join(owners, " and ") + "; qualify it with the folder path"})
				return
			}
			tid, ok := idByKey[key]
			if !ok {
				issues = append(issues, Issue{n.Path, "broken-link",
					"[[" + strings.TrimSpace(rawTarget) + "]] " + brokenLinkReason(strings.TrimSpace(rawTarget))})
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
		// do/dont/why are prose, and they are the prose a search card actually shows, so
		// authors write [[links]] in them and reasonably expect those to be links. They
		// were silently dropped: 82 of them resolved to real notes in the live vault and
		// produced no edge at all, while the handful that did not resolve went unreported,
		// so neither the author nor the graph got anything. Read through the same markup
		// scanner as the body, so a backticked `[[note-id]]` stays the syntax example it is.
		for _, field := range []string{n.FM.Do, n.FM.Dont, n.FM.Why} {
			if field == "" {
				continue
			}
			clean, _ := vault.StripNonContent(field)
			p := &ParsedNote{}
			for _, line := range strings.Split(clean, "\n") {
				p.appendLinks(line, 0)
			}
			for _, l := range p.Links {
				addRef(l.Target, 0)
			}
		}
	}
	// Degrees are computed in a final pass so they do not depend on the interleaved
	// AddNode/AddEdge order above (an edge to a later note would otherwise undercount
	// that note's inbound degree). This keeps BuildGraph's degrees identical to
	// LoadGraph's, so the MCP (BuildGraph) and CLI (LoadGraph) retrieval paths agree.
	g.RecomputeDegrees()
	return g, issues
}

// foreignStoreID matches the filename shape of a Claude MEMORY file: a type prefix,
// then snake_case. Vault ids are kebab-case, so the two never collide.
var foreignStoreID = regexp.MustCompile(`^(feedback|project|reference|user)_[a-z0-9]+(_[a-z0-9]+)+$`)

// brokenLinkReason explains a dangling link, naming the cause when the shape gives it away.
//
// "resolves to nothing" is true but leaves the author guessing, and the single most common
// cause here is not a typo: an agent writes a link to one of its own Claude memory files
// ([[feedback_verify_before_shipping]], [[project_mesh_open_core]]). Those live in a
// DIFFERENT store and no note in this vault will ever carry that id, so the advice is to
// stop linking it, not to go hunting for the note. 27 of one vault's dangling links were a
// single memory slug, and the pattern kept coming back because the message never said so.
func brokenLinkReason(target string) string {
	if foreignStoreID.MatchString(target) {
		return "looks like a Claude memory filename, not a note id in this vault. " +
			"Those live in a different store and will never resolve here: drop the brackets " +
			"and leave it as `" + target + "` in the prose, or link the vault note that covers " +
			"the same ground."
	}
	return "resolves to nothing"
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
