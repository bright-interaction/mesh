// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func retrieverWithStaleGraph(t *testing.T, stale, current []noteSrc) *Retriever {
	t.Helper()
	parse := func(srcs []noteSrc) []*index.ParsedNote {
		notes := make([]*index.ParsedNote, 0, len(srcs))
		for _, src := range srcs {
			pn, err := index.Parse(src.path, []byte(src.body))
			if err != nil {
				t.Fatal(err)
			}
			notes = append(notes, pn)
		}
		return notes
	}
	staleNotes := parse(stale)
	staleGraph, issues := index.BuildGraph(staleNotes)
	if len(issues) != 0 {
		t.Fatalf("stale graph issues: %+v", issues)
	}
	staleGraph.DetectCommunities(0)

	currentNotes := parse(current)
	currentGraph, issues := index.BuildGraph(currentNotes)
	if len(issues) != 0 {
		t.Fatalf("current graph issues: %+v", issues)
	}
	currentGraph.DetectCommunities(0)
	store, err := index.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.IndexVault(currentNotes, currentGraph); err != nil {
		t.Fatal(err)
	}
	return New(store, staleGraph)
}

func currentSupersedesTarget() noteSrc {
	return noteSrc{"public/target.md", "---\nid: public-note\ntype: note\nwhen: 2026-01-01\nscope: [public]\n---\n# Public target\n" + supersedesQuery + "\n"}
}

func currentSuperseder(path, scope string, supersedes bool) noteSrc {
	fm := "---\nid: replacement-note\ntype: note\nwhen: 2026-01-02\nscope: [" + scope + "]\n"
	if supersedes {
		fm += "supersedes: [public-note]\n"
	}
	return noteSrc{path, fm + "---\n# Replacement\nunrelated replacement prose\n"}
}

func findPublicCard(t *testing.T, r *Retriever, opt Options) Card {
	t.Helper()
	cards, err := r.Retrieve(context.Background(), supersedesQuery, opt)
	if err != nil {
		t.Fatal(err)
	}
	return publicNoteCard(t, cards)
}

// The in-memory graph and the notes/nodes tables are deliberately published on
// independent schedules. Supersession is retrieval-critical, so the relation must come
// from the same current SQLite generation as the target and superseder ACL metadata.
func TestSupersedesFollowsCurrentIndexNotStaleGraph(t *testing.T) {
	target := currentSupersedesTarget()
	plain := currentSuperseder("public/replacement.md", "public", false)
	linked := currentSuperseder("public/replacement.md", "public", true)
	opt := scopedFTSOnlyOptions(map[string]bool{"public": true})
	opt.NoRerank = true

	t.Run("new current relation is visible beside stale graph", func(t *testing.T) {
		r := retrieverWithStaleGraph(t, []noteSrc{target, plain}, []noteSrc{target, linked})
		if got := findPublicCard(t, r, opt).SupersededBy; got != "replacement-note" {
			t.Fatalf("SupersededBy = %q, want current replacement-note", got)
		}
	})

	t.Run("removed current relation is not resurrected by stale graph", func(t *testing.T) {
		r := retrieverWithStaleGraph(t, []noteSrc{target, linked}, []noteSrc{target, plain})
		if got := findPublicCard(t, r, opt).SupersededBy; got != "" {
			t.Fatalf("stale graph relation leaked into current card: %q", got)
		}
	})

	t.Run("deleted superseder fails closed", func(t *testing.T) {
		r := retrieverWithStaleGraph(t, []noteSrc{target, linked}, []noteSrc{target})
		if got := findPublicCard(t, r, opt).SupersededBy; got != "" {
			t.Fatalf("deleted superseder leaked into current card: %q", got)
		}
	})

	t.Run("current scope tightening fails closed", func(t *testing.T) {
		secret := currentSuperseder("public/replacement.md", "secret", true)
		r := retrieverWithStaleGraph(t, []noteSrc{target, linked}, []noteSrc{target, secret})
		if got := findPublicCard(t, r, opt).SupersededBy; got != "" {
			t.Fatalf("newly fenced superseder leaked into current card: %q", got)
		}
	})

	t.Run("current folder move fails closed", func(t *testing.T) {
		moved := currentSuperseder("secret/replacement.md", "public", true)
		r := retrieverWithStaleGraph(t, []noteSrc{target, linked}, []noteSrc{target, moved})
		pathOpt := opt
		pathOpt.AllowPath = func(path string) bool { return strings.HasPrefix(path, "public/") }
		if got := findPublicCard(t, r, pathOpt).SupersededBy; got != "" {
			t.Fatalf("folder-fenced superseder leaked into current card: %q", got)
		}
	})
}

func TestFolderFencedSupersedesIsByteIdenticalToNoRelation(t *testing.T) {
	target := currentSupersedesTarget()
	fenced := buildVaultFrom(t, []noteSrc{target, currentSuperseder("secret/replacement.md", "public", true)})
	control := buildVaultFrom(t, []noteSrc{target, currentSuperseder("secret/replacement.md", "public", false)})
	opt := scopedFTSOnlyOptions(map[string]bool{"public": true})
	opt.NoRerank = true
	opt.AllowPath = func(path string) bool { return strings.HasPrefix(path, "public/") }

	got := findPublicCard(t, fenced, opt)
	want := findPublicCard(t, control, opt)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("folder-fenced superseder changed public card bytes or rank:\nfenced: %s\ncontrol: %s", gotJSON, wantJSON)
	}
}

// superseded_by used to join the graph BM25 corpus as an ordinary string attr. That
// made a fenced correction's id searchable through the public note it retired even
// when the field and 0.5x demotion were hidden later in the pipeline.
func TestFencedSupersederIDDoesNotEnterPublicGraphSignal(t *testing.T) {
	const secretID = "classified-replacement-needle"
	target := currentSupersedesTarget()
	secret := func(linked bool) noteSrc {
		fm := "---\nid: " + secretID + "\ntype: note\nwhen: 2026-01-02\nscope: [secret]\n"
		if linked {
			fm += "supersedes: [public-note]\n"
		}
		return noteSrc{"secret-source.md", fm + "---\n# Restricted replacement\nunrelated\n"}
	}
	fenced := buildVaultFrom(t, []noteSrc{target, secret(true)})
	control := buildVaultFrom(t, []noteSrc{target, secret(false)})
	opt := Options{
		Limit: 10, NoRerank: true,
		AllowedScopes: map[string]bool{"public": true},
		WeightGraph:   1,
	}

	got, err := fenced.Retrieve(context.Background(), secretID, opt)
	if err != nil {
		t.Fatal(err)
	}
	want, err := control.Retrieve(context.Background(), secretID, opt)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("fenced superseder id changed the public graph result:\nfenced: %s\ncontrol: %s", gotJSON, wantJSON)
	}
}
