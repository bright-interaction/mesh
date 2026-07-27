// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"testing"
)

// do/dont/why are the prose a search card actually shows, so a [[link]] written there is
// the author asking for a link. Dropping it gave nobody anything: no edge for the graph,
// and no broken-link notice when the target did not exist. 82 such links resolved in the
// live vault and produced no edge at all.
func TestFrontmatterProseLinksBecomeEdges(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("target.md", "---\nid: target\ntype: note\ntitle: Target\n---\n\n# Target\n")
	write("other.md", "---\nid: other\ntype: note\ntitle: Other\n---\n\n# Other\n")
	write("third.md", "---\nid: third\ntype: note\ntitle: Third\n---\n\n# Third\n")
	write("src.md", "---\nid: src\ntype: gotcha\ntitle: Src\n"+
		"do: 'Always check [[target]] first.'\n"+
		"dont: 'Never skip [[other]].'\n"+
		"why: 'Because [[third]] explains it.'\n"+
		"---\n\n# Src\n")

	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, notes, err := ReindexFull(st, dir)
	if err != nil {
		t.Fatal(err)
	}
	g, issues := BuildGraph(notes)

	for _, want := range []string{"target", "other", "third"} {
		found := false
		for _, e := range g.Neighbors("note:src") {
			if e.Target == "note:"+want && e.Relation == "references" {
				found = true
			}
		}
		if !found {
			t.Errorf("a [[%s]] link written in do/dont/why produced no edge; the author asked for a link and got nothing", want)
		}
	}
	for _, is := range issues {
		if is.Kind == "broken-link" {
			t.Errorf("unexpected broken-link issue: %v", is)
		}
	}
}

// A note that documents wikilink syntax quotes it in backticks. Reading frontmatter prose
// through the same markup scanner as the body means those examples stay examples, instead
// of becoming a fresh crop of broken links the moment this extraction turned on.
func TestBacktickedFrontmatterExampleIsNotALink(t *testing.T) {
	dir := t.TempDir()
	body := "---\nid: doc\ntype: gotcha\ntitle: Doc\n" +
		"do: 'Write `[[note-id]]` inline where the connection is made.'\n" +
		"---\n\n# Doc\n"
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, notes, err := ReindexFull(st, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, issues := BuildGraph(notes)
	for _, is := range issues {
		if is.Kind == "broken-link" {
			t.Errorf("a backticked syntax example in do/dont/why was treated as a real link: %v", is)
		}
	}
}
