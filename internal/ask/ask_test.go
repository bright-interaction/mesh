package ask

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

func TestAnswerGroundsAndCites(t *testing.T) {
	dir := t.TempDir()
	note := "---\nid: mollie-gotcha\ntype: gotcha\n---\n# Mollie webhooks need re-fetch\nMollie does not HMAC webhook bodies; re-fetch the payment by id to authenticate it.\n"
	if err := os.WriteFile(filepath.Join(dir, "mollie.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := index.Reindex(store, dir)
	if err != nil {
		t.Fatal(err)
	}
	rtr := retrieve.NewFromEnv(store, g)

	var gotContext string
	stub := llm.Func(func(_ context.Context, _, user string) (string, error) {
		gotContext = user
		return "Re-fetch the payment by id; Mollie does not sign webhooks [1].", nil
	})
	res, err := Answer(context.Background(), rtr, store, stub, "how do we authenticate Mollie webhooks?", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Answer, "Re-fetch") {
		t.Fatalf("answer = %q", res.Answer)
	}
	if len(res.Citations) == 0 || res.Citations[0].Kind != "note" {
		t.Fatalf("expected a note citation, got %+v", res.Citations)
	}
	// The retrieved note must have been put in the LLM context (grounding).
	if !strings.Contains(gotContext, "Mollie") {
		t.Fatalf("retrieved note was not in the LLM context: %q", gotContext)
	}
}

func TestEmptyQuestion(t *testing.T) {
	if _, err := Answer(context.Background(), nil, nil, llm.Func(func(context.Context, string, string) (string, error) { return "", nil }), "  ", 0, nil); err == nil {
		t.Error("empty question must error")
	}
}
