// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package rerank

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionCLIPresetsUseCheapModelsAndDisableAgentContext(t *testing.T) {
	codex, err := NewSubscriptionCLI("codex", "")
	if err != nil {
		t.Fatal(err)
	}
	if codex.Model() != "subscription/codex/gpt-5.6-luna" {
		t.Fatalf("codex model = %q", codex.Model())
	}
	joined := strings.Join(codex.argv, " ")
	for _, want := range []string{"exec", "--ephemeral", "--ignore-user-config", "--sandbox read-only", "--disable plugins", "--disable shell_tool", "model_reasoning_effort=low"} {
		if !strings.Contains(joined, want) {
			t.Errorf("codex preset missing %q: %s", want, joined)
		}
	}

	claude, err := NewSubscriptionCLI("claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if claude.Model() != "subscription/claude/claude-haiku-4-5-20251001" {
		t.Fatalf("claude model = %q", claude.Model())
	}
	joined = strings.Join(claude.argv, " ")
	for _, want := range []string{"--safe-mode", "--restricted", "--strict-mcp-config", "--no-session-persistence", "--max-turns 1", "--effort low", "--system-prompt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude preset missing %q: %s", want, joined)
		}
	}
}

func TestSubscriptionCLIRanksCompactCardsMarksChildAndCaches(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt")
	markerPath := filepath.Join(dir, "marker")
	countPath := filepath.Join(dir, "count")
	script := filepath.Join(dir, "rank.sh")
	body := "#!/bin/sh\n" +
		"cat > \"$1\"\n" +
		"printf '%s' \"$MESH_LLM_CHILD\" > \"$2\"\n" +
		"printf x >> \"$3\"\n" +
		"printf '[1,0]'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &CLI{
		provider: "test", model: "tiny", argv: []string{script, promptPath, markerPath, countPath},
		timeout: 10 * time.Second, candidateCap: 2, cardCharCap: 20,
		cache: make(map[[32]byte][]Result),
	}
	candidates := []Candidate{
		{Index: 0, Title: "First", Snippet: "first compact snippet with a body that must be truncated", Reason: "fts"},
		{Index: 1, Title: "Second", Snippet: "second compact snippet", Reason: "graph"},
	}
	for run := 0; run < 2; run++ {
		results, err := c.RerankCandidates(context.Background(), "find second", candidates)
		if err != nil {
			t.Fatal(err)
		}
		if results[1].Score <= results[0].Score {
			t.Fatalf("ranking not mapped back to candidate indexes: %#v", results)
		}
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prompt), "must be truncated") || !strings.Contains(string(prompt), "untrusted data") {
		t.Fatalf("prompt was not compact/injection-labelled: %s", prompt)
	}
	marker, _ := os.ReadFile(markerPath)
	if string(marker) != "1" {
		t.Fatalf("MESH_LLM_CHILD = %q, want 1", marker)
	}
	count, _ := os.ReadFile(countPath)
	if string(count) != "x" {
		t.Fatalf("identical request ran command %d times, want once", len(count))
	}
}

func TestSubscriptionCLIProbeDoesNotSpendAModelCall(t *testing.T) {
	dir := t.TempDir()
	called := filepath.Join(dir, "called")
	script := filepath.Join(dir, "rank.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf x > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &CLI{argv: []string{script, called}}
	if err := c.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatalf("status probe executed the model command: %v", err)
	}
	if !strings.Contains(c.ProbeReport(), "authentication") {
		t.Fatalf("probe report hides its limitation: %q", c.ProbeReport())
	}
}

func TestParseRankingRejectsAnythingButCompletePermutation(t *testing.T) {
	for _, raw := range []string{
		"```json\n[0,1]\n```",
		"[0]",
		"[0,0]",
		"[0,2]",
		"Here you go: [0,1]",
	} {
		if _, err := parseRanking(raw, 2); err == nil {
			t.Errorf("parseRanking(%q) accepted malformed output", raw)
		}
	}
	if got, err := parseRanking("[1,0]", 2); err != nil || got[1].Score <= got[0].Score {
		t.Fatalf("valid ranking rejected/mis-scored: %#v, %v", got, err)
	}
}

func TestSubscriptionCLIConfigCaps(t *testing.T) {
	t.Setenv("MESH_RERANK_CANDIDATES", "7")
	t.Setenv("MESH_RERANK_RESULTS", "3")
	t.Setenv("MESH_RERANK_CARD_CHARS", "333")
	t.Setenv("MESH_RERANK_CMD_TIMEOUT", "12")
	c, err := NewSubscriptionCLI("codex", "gpt-custom")
	if err != nil {
		t.Fatal(err)
	}
	if c.CandidateLimit() != 7 || c.ResultLimit() != 3 || c.cardCharCap != 333 || c.timeout != 12*time.Second {
		t.Fatalf("env caps not applied: candidates=%d results=%d chars=%d timeout=%s", c.CandidateLimit(), c.ResultLimit(), c.cardCharCap, c.timeout)
	}
}

func TestDefaultSubscriptionPromptHasHardSmallEnvelope(t *testing.T) {
	c, err := NewSubscriptionCLI("codex", "")
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]Candidate, c.CandidateLimit())
	for i := range candidates {
		candidates[i] = Candidate{Index: i, Title: strings.Repeat("title ", 100), Snippet: strings.Repeat("snippet ", 300), Reason: strings.Repeat("reason ", 100)}
	}
	compact := make([]Candidate, len(candidates))
	for i, candidate := range candidates {
		compact[i] = Candidate{Index: i, Title: truncateUTF8(candidate.Title, maxCLITitleChars), Snippet: truncateUTF8(candidate.Snippet, c.cardCharCap), Reason: truncateUTF8(candidate.Reason, maxCLIReasonChars)}
	}
	prompt, err := buildCLIPrompt(strings.Repeat("query ", 1000), compact)
	if err != nil {
		t.Fatal(err)
	}
	// This is a byte ceiling, not a tokenizer claim. Even adversarially long fields
	// stay below ~3k tokens under the conservative four-chars-per-token estimate.
	if len(prompt) > 12_000 {
		t.Fatalf("default subscription prompt is %d bytes, want <= 12000", len(prompt))
	}
}
