package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brightinteraction/mesh/internal/index"
	"github.com/brightinteraction/mesh/internal/retrieve"
)

func TestRunGateBeatsBaselineOnLongNotes(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) *index.ParsedNote {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		pn, err := index.Parse(rel, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return pn
	}
	bigProse := strings.Repeat("modernc sqlite storage engine decision prose paragraph. ", 200)
	notes := []*index.ParsedNote{
		write("a.md", "---\nid: storage\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: use modernc sqlite for storage\n---\n# Storage\n"+bigProse),
		write("b.md", "---\nid: other1\ntype: note\nwhen: 2026-01-01\n---\n# Other1\n"+bigProse),
		write("c.md", "---\nid: other2\ntype: note\nwhen: 2026-01-01\n---\n# Other2\n"+bigProse),
	}
	g, _ := index.BuildGraph(notes)
	s, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	lg, _ := s.LoadGraph()
	r := retrieve.New(s, lg)

	rep := RunGate(s, r, dir, []Case{{Query: "modernc sqlite storage", Relevant: []string{"storage"}}}, 400)
	if rep.MeshSurfaced != 1 {
		t.Errorf("mesh should surface the relevant note, got %d/%d", rep.MeshSurfaced, rep.N)
	}
	// Honest claim: on long notes, reading Mesh's cards + one body costs less than
	// naively reading the top-3 full bodies.
	if rep.MeshMedian >= rep.FTSTop3Median {
		t.Errorf("mesh median (%.0f) should beat naive read-top-3 (%.0f) on long notes", rep.MeshMedian, rep.FTSTop3Median)
	}
	// The matched single-body baseline must be tracked (the fix the review demanded).
	if rep.FTSTop1Median <= 0 {
		t.Errorf("matched fts-top1 baseline should be measured, got %.0f", rep.FTSTop1Median)
	}
}
