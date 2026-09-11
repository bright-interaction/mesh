// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func TestReadOnlyRefreshAndAcknowledgementNeverCallEmbeddingModel(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "model deliberately unavailable", http.StatusServiceUnavailable)
	}))
	defer endpoint.Close()
	t.Setenv("MESH_EMBED_ENDPOINT", endpoint.URL)
	t.Setenv("MESH_EMBED_MODEL", "refresh-test")
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	if err := os.WriteFile(path, []byte("---\nid: note\ntype: note\n---\nPersisted locally.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := index.Reindex(writer, root); err != nil {
		t.Fatal(err)
	}
	hash, err := writer.NoteRetrievalHash("note:note")
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.ReplaceVectors("refresh-test", []index.VectorRow{{NodeID: "note:note", Vec: []float32{1, 0}, NoteHash: hash}}); err != nil {
		t.Fatal(err)
	}
	reader, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := reader.WaitReady(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := reader.refresh(); err != nil {
			t.Fatal(err)
		}
		if err := reader.awaitOwnerIndexed(context.Background(), "note", path); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("local refresh/ack invoked model %d times", calls.Load())
	}
	_, r := reader.snapshot()
	if !r.VectorsActive() {
		t.Fatal("test did not activate the embedding configuration")
	}
	if err := r.EmbedderProbe(); err == nil {
		t.Fatal("explicit health check hid unavailable endpoint")
	}
	if calls.Load() != 1 {
		t.Fatalf("explicit health check made %d calls, want 1", calls.Load())
	}
}
