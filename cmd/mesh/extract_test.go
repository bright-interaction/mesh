package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bright-interaction/mesh/internal/extract"
	"github.com/bright-interaction/mesh/internal/index"
)

// knownInVault must return true for a candidate that restates an existing note (so the
// review queue does not re-surface knowledge the vault already has) and false for a
// genuinely new one. Exercises the full path: retriever over the vault + TitleSimilarity.
func TestKnownInVaultDedup(t *testing.T) {
	dir := t.TempDir()
	note := "---\nid: ssrf-tailscale\ntype: gotcha\n---\n# SSRF denylist must include 100.64.0.0/10 (Tailscale CGNAT)\nResolve the host and reject the Tailscale CGNAT range in SSRF denylists.\n"
	if err := os.WriteFile(filepath.Join(dir, "ssrf.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rtr, closeRtr := buildVaultRetriever(store, dir)
	defer closeRtr()
	if rtr == nil {
		t.Fatal("retriever failed to build over the vault")
	}
	ctx := context.Background()

	dup := extract.Candidate{Type: "gotcha", Title: "SSRF denylists must include 100.64.0.0/10 for Tailscale", Do: "add the CGNAT range"}
	if known, of := knownInVault(ctx, rtr, dup); !known {
		t.Errorf("restatement not detected as known (matched %q)", of)
	}

	fresh := extract.Candidate{Type: "decision", Title: "Mollie webhooks require re-fetch, not signature verification", Do: "re-fetch the payment by id"}
	if known, of := knownInVault(ctx, rtr, fresh); known {
		t.Errorf("a genuinely new candidate was wrongly deduped (matched %q)", of)
	}
}
