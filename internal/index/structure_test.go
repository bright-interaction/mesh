package index

import (
	"os"
	"path/filepath"
	"testing"
)

func writeStructNote(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAnalyzeStructure(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		writeStructNote(t, dir, "alpha.md", "---\nid: alpha\ntype: entity\ntitle: Alpha\n---\nlinks [[beta]] and [[gamma]]\n"),
		writeStructNote(t, dir, "beta.md", "---\nid: beta\ntype: concept\ntitle: Beta\n---\nlinks [[alpha]]\n"),
		writeStructNote(t, dir, "gamma.md", "---\nid: gamma\ntype: decision\ntitle: Gamma\n---\nlinks [[alpha]]\n"),
		writeStructNote(t, dir, "lonely.md", "---\nid: lonely\ntype: note\ntitle: Lonely\n---\nno links at all\n"),
		writeStructNote(t, dir, "weird.md", "---\nid: weird\ntype: widget\ntitle: Weird\n---\nlinks [[alpha]]\n"),
	}
	parsed, perrs := ParseFiles(paths, 1)
	g, _ := BuildGraph(parsed)
	g.DetectCommunities(0)
	rep := AnalyzeStructure(g, parsed, perrs)

	if rep.Notes != 5 {
		t.Fatalf("notes = %d, want 5", rep.Notes)
	}
	kinds := map[string]bool{}
	for _, f := range rep.Findings {
		kinds[f.Kind] = true
	}
	if !kinds["orphan"] {
		t.Error("expected an orphan finding (lonely has no links)")
	}
	if !kinds["unknown-type"] {
		t.Error("expected an unknown-type finding (weird is type widget)")
	}
	if rep.Tier0 != 1 {
		t.Errorf("tier0 = %d, want 1 (gamma is a decision)", rep.Tier0)
	}
	if rep.Score >= 100 || rep.Grade == "" {
		t.Errorf("score = %d grade = %q, want a sub-100 graded result", rep.Score, rep.Grade)
	}
	// A clean two-note vault scores 100/A.
	cdir := t.TempDir()
	cp := []string{
		writeStructNote(t, cdir, "x.md", "---\nid: x\ntype: entity\ntitle: X\n---\n[[y]] [[z]]\n"),
		writeStructNote(t, cdir, "y.md", "---\nid: y\ntype: concept\ntitle: Y\n---\n[[x]] [[z]]\n"),
		writeStructNote(t, cdir, "z.md", "---\nid: z\ntype: concept\ntitle: Z\n---\n[[x]] [[y]]\n"),
	}
	cp2, cerrs := ParseFiles(cp, 1)
	cg, _ := BuildGraph(cp2)
	cg.DetectCommunities(0)
	clean := AnalyzeStructure(cg, cp2, cerrs)
	if clean.Grade != "A" {
		t.Errorf("clean vault grade = %q (score %d), want A", clean.Grade, clean.Score)
	}

	// A note that fails to parse is surfaced as high-severity 'unparseable' and
	// counted against the grade, not silently dropped.
	bdir := t.TempDir()
	bp := []string{
		writeStructNote(t, bdir, "ok.md", "---\nid: ok\ntype: entity\ntitle: OK\n---\n[[ok2]]\n"),
		writeStructNote(t, bdir, "ok2.md", "---\nid: ok2\ntype: concept\ntitle: OK2\n---\n[[ok]]\n"),
		writeStructNote(t, bdir, "broken.md", "---\nupdated: 2026-06-18 (note: with a colon)\ntype: entity\ntitle: Broken\n---\nbody\n"),
	}
	bparsed, berrs := ParseFiles(bp, 1)
	bg, _ := BuildGraph(bparsed)
	bg.DetectCommunities(0)
	brep := AnalyzeStructure(bg, bparsed, berrs)
	if brep.Unparseable != 1 {
		t.Fatalf("unparseable = %d, want 1 (broken.md has invalid frontmatter)", brep.Unparseable)
	}
	var sawUnparseable bool
	for _, f := range brep.Findings {
		if f.Kind == "unparseable" && f.Severity == "high" {
			sawUnparseable = true
		}
	}
	if !sawUnparseable {
		t.Error("expected a high-severity unparseable finding")
	}
}
