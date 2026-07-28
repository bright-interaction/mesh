// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 65 notes in one live vault were created with a long, specific title and "TODO" in every
// guidance field, then never filled. They looked like knowledge in search results and
// carried none, and were eventually deleted wholesale. The write is the only moment the
// caller still has the context to say what it learned, so that is where to insist.
func TestAppendNoteRefusesAStubWithNoGuidance(t *testing.T) {
	s := newTestServer(t)
	for _, typ := range []string{"gotcha", "decision", "post-mortem"} {
		t.Run(typ, func(t *testing.T) {
			_, rerr := s.dispatch(context.Background(), request{
				JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
				Params: mustJSON(map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
					"type": typ, "title": "Something specific happened during a deploy",
					"do": "TODO", "dont": "TODO", "why": "TODO",
				}}),
			})
			if rerr == nil {
				t.Fatal("a flywheel note with TODO in every guidance field was accepted; " +
					"that is exactly the stub this guard exists to prevent")
			}
			if !strings.Contains(rerr.Message, "do/dont/why") {
				t.Errorf("the refusal must say what to supply, got: %s", rerr.Message)
			}
		})
	}
}

// One line is enough. The guard must not turn into a demand for all three, or the cost of
// writing anything down goes up and people stop writing notes at all.
func TestAppendNoteAcceptsPartialGuidance(t *testing.T) {
	s := newTestServer(t)
	_, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
			"type": "gotcha", "title": "Only a dont was known at the time",
			"dont": "do not retry through a held write lock",
		}}),
	})
	if rerr != nil {
		t.Fatalf("a note with one real guidance line must be accepted, got: %v", rerr)
	}
}

// Types without the flywheel requirement (entity, concept, map, note) are reference pages,
// not advice, so they must stay writable without guidance fields.
func TestAppendNoteStillAcceptsNonFlywheelTypes(t *testing.T) {
	s := newTestServer(t)
	_, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
			"type": "note", "title": "A plain reference note",
		}}),
	})
	if rerr != nil {
		t.Fatalf("a non-flywheel type must not require guidance, got: %v", rerr)
	}
}
