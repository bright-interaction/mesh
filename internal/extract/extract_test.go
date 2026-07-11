// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package extract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/llm"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDigest(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"fix the deploy"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"editing the Dockerfile"},{"type":"tool_use","name":"Bash","input":{"command":"go build ./..."}}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hidden reasoning"},{"type":"tool_use","name":"mesh_append_note","input":{"title":"x"}}]}}`,
		`{"type":"summary","summary":"noise"}`,
	)
	d, st, err := Digest(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"USER: fix the deploy", "ASSISTANT: editing the Dockerfile", "TOOL Bash(command=go build ./...)", "TOOL mesh_append_note("} {
		if !strings.Contains(d, want) {
			t.Errorf("digest missing %q in:\n%s", want, d)
		}
	}
	if strings.Contains(d, "hidden reasoning") {
		t.Error("thinking blocks should be excluded from the digest")
	}
	if !st.HadWriteback {
		t.Error("HadWriteback should be true (mesh_append_note was called)")
	}
	if st.UserMsgs != 1 || st.AsstMsgs != 1 || st.ToolCalls != 2 {
		t.Errorf("stats = %+v", st)
	}
}

func TestParseCandidates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"clean", `[{"type":"gotcha","title":"T","do":"d","dont":"x","why":"w","confidence":"high"}]`, 1},
		{"fenced", "```json\n[{\"type\":\"decision\",\"title\":\"T\"}]\n```", 1},
		{"prose-wrapped", `Here you go: [{"type":"post-mortem","title":"T"}] hope that helps`, 1},
		{"empty-array", `[]`, 0},
		{"prose-empty", `No durable, reusable knowledge in this session.`, 0},
		{"drops-bad-type", `[{"type":"note","title":"T"},{"type":"gotcha","title":"Keep"}]`, 1},
		{"drops-no-title", `[{"type":"gotcha","title":""}]`, 0},
	}
	for _, c := range cases {
		got, err := parseCandidates(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(got) != c.want {
			t.Errorf("%s: got %d candidates, want %d (%+v)", c.name, len(got), c.want, got)
		}
	}
}

func TestTitleSimilarity(t *testing.T) {
	// A near-restatement of an existing note scores above the duplicate threshold.
	cand := "SSRF denylists must include 100.64.0.0/10 for Tailscale"
	existing := "SSRF denylist must include 100.64.0.0/10 (Tailscale); the backup repo script had drifted"
	if s := TitleSimilarity(cand, existing); s < DuplicateThreshold {
		t.Errorf("near-duplicate similarity = %.2f, want >= %.2f", s, DuplicateThreshold)
	}
	// A distinct note scores below it.
	other := "Mollie webhooks require re-fetch, not signature verification"
	if s := TitleSimilarity(cand, other); s >= DuplicateThreshold {
		t.Errorf("distinct-note similarity = %.2f, want < %.2f", s, DuplicateThreshold)
	}
}

func TestClusterRecurring(t *testing.T) {
	occs := []Occurrence{
		{Cand: Candidate{Type: "gotcha", Title: "SSRF denylist must include Tailscale CGNAT range"}, Session: "s1"},
		{Cand: Candidate{Type: "gotcha", Title: "SSRF denylists must include 100.64.0.0/10 for Tailscale"}, Session: "s2"},
		{Cand: Candidate{Type: "decision", Title: "Mollie webhooks require re-fetch not signatures"}, Session: "s1"},
	}
	clusters := ClusterRecurring(occs, DuplicateThreshold)
	if len(clusters) != 2 {
		t.Fatalf("got %d clusters, want 2", len(clusters))
	}
	// The SSRF pair recurs across 2 sessions and sorts first.
	if clusters[0].Count != 2 {
		t.Fatalf("top cluster session count = %d, want 2 (%+v)", clusters[0].Count, clusters[0])
	}
	// The Mollie one is a one-off.
	if clusters[1].Count != 1 {
		t.Fatalf("second cluster count = %d, want 1", clusters[1].Count)
	}
}

func TestExtractAndJudgeWithStub(t *testing.T) {
	stub := llm.Func(func(_ context.Context, system, _ string) (string, error) {
		if strings.Contains(system, "reviewing one candidate") {
			return `{"keep": true, "reason": "non-obvious + reusable"}`, nil
		}
		return `[{"type":"gotcha","title":"Bun not npm after migration","do":"use bun","dont":"npm silently breaks","why":"lockfile","confidence":"high"}]`, nil
	})
	cands, err := Extract(context.Background(), stub, "digest")
	if err != nil || len(cands) != 1 {
		t.Fatalf("extract = %v, %v", cands, err)
	}
	keep, reason, err := Judge(context.Background(), stub, cands[0])
	if err != nil || !keep || reason == "" {
		t.Fatalf("judge = %v %q %v", keep, reason, err)
	}
}
