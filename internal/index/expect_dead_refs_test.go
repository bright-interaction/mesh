// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
	"gopkg.in/yaml.v3"
)

// Some notes exist BECAUSE a file was deleted: a retirement record, a post-mortem about
// removing a service. Their paths are supposed to be gone, so dead_ref flags them forever
// and is right every time. Six such notes sat permanently red in the live vault, and a
// check with permanent known-good findings is one people learn to skim past, which is how
// the genuine findings underneath get missed.
func TestExpectDeadRefsSuppressesOnlyTheOptedInNote(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A file that IS indexed, so its directory counts as judgeable.
	write("live.md", "---\nid: live\ntype: note\ntitle: Live\n---\n\n# Live\ncites internal/handlers/present.go\n")
	// Records a deletion, so it opts out.
	write("retired.md", "---\nid: retired\ntype: gotcha\ntitle: Retired\nexpect_dead_refs: true\n---\n\n"+
		"# Retired\nthe service is gone, see internal/handlers/removed.go\n")
	// Does NOT opt out, so it must still be reported.
	write("stale.md", "---\nid: stale\ntype: gotcha\ntitle: Stale\n---\n\n"+
		"# Stale\nstill claims internal/handlers/alsogone.go\n")
	// Quotes ONE synthetic path (a fabricated stack frame) while citing real files around
	// it. The list form must cover the named path and nothing else, or the only way to get
	// this note out of the findings is `true`, which stops checking the real ones too.
	write("narrative.md", "---\nid: narrative\ntype: post-mortem\ntitle: Narrative\n"+
		"expect_dead_ref_paths:\n    - internal/handlers/synthetic.go\n---\n\n"+
		"# Narrative\nthe model saw a frame at internal/handlers/synthetic.go\n"+
		"and the real handler is internal/handlers/deleted.go\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := ReindexFull(s, dir); err != nil {
		t.Fatal(err)
	}
	// Seed a code index so dead_ref runs at all and internal/handlers counts as a
	// directory we index (an unindexed directory is deliberately never judged).
	if _, err := s.writeDB.Exec(
		`INSERT OR REPLACE INTO code_files(path,lang,package,mtime,retrieval_hash) VALUES(?,?,?,?,?)`,
		"repo/internal/handlers/present.go", "go", "handlers", time.Now().Unix(), "h"); err != nil {
		t.Fatal(err)
	}

	findings, err := s.ComputeHealth(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var flagged []string
	for _, f := range findings {
		if f.Issue == "dead_ref" {
			flagged = append(flagged, f.NoteID+":"+f.Detail)
		}
	}
	joined := strings.Join(flagged, " ")
	if strings.Contains(joined, "retired") {
		t.Errorf("a note that opted out was still reported: %v", flagged)
	}
	if !strings.Contains(joined, "stale") {
		t.Errorf("the opt-out must be per-note, but a note that did NOT opt out went unreported: %v", flagged)
	}
	if strings.Contains(joined, "narrative:internal/handlers/synthetic.go") {
		t.Errorf("a path named in the expect_dead_refs list was still reported: %v", flagged)
	}
	if !strings.Contains(joined, "narrative:internal/handlers/deleted.go") {
		t.Errorf("the list form must exempt ONLY the paths it names, but the other dead ref "+
			"in the same note went unreported: %v", flagged)
	}
}

// The per-path exemption is a SEPARATE key because frontmatter is read by long-running
// daemons and a synced hub that are routinely a binary behind. yaml.v3 ignores a key it does
// not know but REFUSES a known key of the wrong type, and a note that will not parse is
// dropped from search and the graph entirely rather than merely mis-indexed.
//
// Overloading expect_dead_refs with a list was tried first and did exactly that on the live
// vault: both notes went unparseable to every reader still on the old binary, and `[[flare]]`
// stopped resolving across 88 links. This pins the property that made the second attempt
// safe, which no amount of testing the CURRENT binary against itself would have caught.
func TestPerPathExemptionUsesAKeyOlderReadersCanIgnore(t *testing.T) {
	const note = "---\nid: n\ntype: gotcha\ntitle: N\nexpect_dead_ref_paths:\n    - src/checkout.js\n---\n\n# N\n"

	// An older reader's whitelist: every key it knew, and NOT the new one.
	var older struct {
		ID             string `yaml:"id"`
		Type           string `yaml:"type"`
		Title          string `yaml:"title"`
		ExpectDeadRefs bool   `yaml:"expect_dead_refs,omitempty"`
	}
	fmText, _, _ := vault.SplitFrontmatter(note)
	if err := yaml.Unmarshal([]byte(fmText), &older); err != nil {
		t.Fatalf("a reader that predates this key must still parse the note, or the note "+
			"vanishes from search and the graph for everyone not yet upgraded: %v", err)
	}
	if older.ID != "n" || older.Title != "N" {
		t.Fatalf("older reader decoded the wrong note: %+v", older)
	}

	// And the current reader honours it, through the index's JSON storage encoding.
	fm, _, err := vault.ParseFrontmatter([]byte(fmText))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(fm)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		ExpectDeadRefs     bool     `json:"ExpectDeadRefs"`
		ExpectDeadRefPaths []string `json:"ExpectDeadRefPaths"`
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("stored frontmatter no longer decodes: %v\n%s", err, b)
	}
	if stored.ExpectDeadRefs {
		t.Error("a per-path exemption must not exempt the whole note")
	}
	if !vault.ExemptsDeadRef(stored.ExpectDeadRefPaths, "src/checkout.js") {
		t.Errorf("the declared path is not exempt after storage (json: %s)", b)
	}
	if vault.ExemptsDeadRef(stored.ExpectDeadRefPaths, "src/real.js") {
		t.Error("a path that was not declared must still be checked")
	}
}
