package embed

import (
	"context"
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	if c := Cosine([]float32{1, 0, 0}, []float32{1, 0, 0}); math.Abs(c-1) > 1e-9 {
		t.Errorf("identical vectors cosine = %v, want 1", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{0, 1}); math.Abs(c) > 1e-9 {
		t.Errorf("orthogonal cosine = %v, want 0", c)
	}
	if c := Cosine([]float32{1, 1}, []float32{2, 2}); math.Abs(c-1) > 1e-9 {
		t.Errorf("parallel cosine = %v, want 1", c)
	}
	if c := Cosine([]float32{1}, []float32{1, 2}); c != 0 {
		t.Errorf("mismatched lengths cosine = %v, want 0", c)
	}
}

func TestStubDeterministicAndNormalized(t *testing.T) {
	s := Stub{D: 32}
	a, _ := s.Embed(context.Background(), []string{"modernc sqlite storage"})
	b, _ := s.Embed(context.Background(), []string{"modernc sqlite storage"})
	if len(a) != 1 || len(b) != 1 {
		t.Fatal("expected one vector each")
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("stub not deterministic at %d", i)
		}
	}
	if c := Cosine(a[0], a[0]); math.Abs(c-1) > 1e-6 {
		t.Errorf("self-cosine of a normalized vector should be 1, got %v", c)
	}
	// related text should be closer than unrelated text
	rel, _ := s.Embed(context.Background(), []string{"sqlite storage engine"})
	unrel, _ := s.Embed(context.Background(), []string{"marketing copy newsletter"})
	if Cosine(a[0], rel[0]) <= Cosine(a[0], unrel[0]) {
		t.Errorf("shared-token text should score higher than unrelated")
	}
}
