package eval

import (
	"math"
	"testing"
)

func TestSimplexGridSumsToOne(t *testing.T) {
	for _, vectors := range []bool{true, false} {
		grid := simplexGrid(0.1, vectors)
		if len(grid) == 0 {
			t.Fatalf("empty grid (vectors=%v)", vectors)
		}
		for _, w := range grid {
			sum := w.FTS + w.Graph + w.Vec
			if math.Abs(sum-1.0) > 1e-9 {
				t.Errorf("weights %v sum to %v, want 1.0", w, sum)
			}
			if !vectors && w.Vec != 0 {
				t.Errorf("vector axis should be 0 when vectors=false, got %v", w)
			}
		}
	}
	// 3-axis simplex on a step-0.5 grid: (i,j,k) with i+j+k=2 => 6 points.
	if n := len(simplexGrid(0.5, true)); n != 6 {
		t.Errorf("step-0.5 3D simplex should have 6 points, got %d", n)
	}
	// 2-axis: i in 0..2 => 3 points.
	if n := len(simplexGrid(0.5, false)); n != 3 {
		t.Errorf("step-0.5 2D simplex should have 3 points, got %d", n)
	}
}

func TestDefaultWeights(t *testing.T) {
	if d := defaultWeights(true); d != (WeightSet{0.5, 0.2, 0.3}) {
		t.Errorf("vector default wrong: %v", d)
	}
	if d := defaultWeights(false); d != (WeightSet{0.7, 0.3, 0}) {
		t.Errorf("lexical default wrong: %v", d)
	}
}
