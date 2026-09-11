// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package latency

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type capture struct {
	mu sync.Mutex
	bytes.Buffer
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Buffer.Write(p)
}

func (c *capture) records(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(c.Buffer.Bytes()))
	for {
		var r map[string]any
		if err := decoder.Decode(&r); err == io.EOF {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
}

func TestFastTraceSilentAndEndIdempotent(t *testing.T) {
	c := new(capture)
	tr := start("test", "initial", slog.New(slog.NewJSONHandler(c, nil)), time.Hour, time.Hour)
	tr.Phase("next")
	tr.End()
	tr.End()
	tr.Phase("after_end")
	tr.progress() // a callback already queued before Stop must remain silent
	if got := c.records(t); len(got) != 0 {
		t.Fatalf("fast/ended trace logged: %v", got)
	}
}

func TestSummaryAccountsForEveryPhaseAndRepeatedLabels(t *testing.T) {
	c := new(capture)
	tr := start("test", "scan", slog.New(slog.NewJSONHandler(c, nil)), 0, time.Hour)
	tr.Phase("publish")
	tr.Phase("scan")
	tr.End()
	tr.End()
	tr.progress()
	records := c.records(t)
	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}
	r := records[0]
	if r["msg"] != "mesh: slow operation returned" || r["last_phase"] != "scan" {
		t.Fatalf("bad summary: %v", r)
	}
	phases := r["phases"].(map[string]any)
	if len(phases) != 2 {
		t.Fatalf("retries not aggregated: %v", phases)
	}
	sum := phases["scan"].(float64) + phases["publish"].(float64)
	if sum != r["elapsed"].(float64) {
		t.Fatalf("phases %v do not account for elapsed %v", phases, r["elapsed"])
	}
	for _, key := range []string{"error", "path", "note_id", "content", "title"} {
		if _, ok := r[key]; ok {
			t.Fatalf("unsafe diagnostic key %s", key)
		}
	}
}

func TestProgressReportsActivePhaseAndStops(t *testing.T) {
	c := new(capture)
	tr := start("test", "scan", slog.New(slog.NewJSONHandler(c, nil)), 0, 5*time.Millisecond)
	defer tr.End()
	tr.Phase("blocked_sql")
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for len(c.records(t)) == 0 {
		select {
		case <-deadline:
			t.Fatal("no in-flight progress record")
		case <-tick.C:
		}
	}
	tr.End()
	records := c.records(t)
	if records[0]["phase"] != "blocked_sql" {
		t.Fatalf("wrong active phase: %v", records[0])
	}
	last := records[len(records)-1]
	if last["msg"] != "mesh: slow operation returned" {
		t.Fatalf("no final summary: %v", last)
	}
	if records[0]["trace_id"] != last["trace_id"] || records[0]["pid"] != last["pid"] {
		t.Fatal("lost trace identity")
	}
	tr.progress()
	if len(c.records(t)) != len(records) {
		t.Fatal("progress logged after End")
	}
}

func TestConcurrentPhaseProgressAndEnd(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for n := 0; n < 50; n++ {
		tr := start("test", "initial", logger, 0, time.Hour)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); tr.Phase("work"); tr.progress(); tr.End() }()
		}
		wg.Wait()
		tr.mu.Lock()
		if !tr.ended {
			t.Fatal("trace not ended")
		}
		tr.mu.Unlock()
	}
}

func BenchmarkFastTrace(b *testing.B) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b.ReportAllocs()
	for b.Loop() {
		tr := start("test", "validate", logger, time.Hour, time.Hour)
		tr.Phase("scan")
		tr.Phase("publish")
		tr.End()
	}
}
