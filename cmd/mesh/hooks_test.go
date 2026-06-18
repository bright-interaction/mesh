package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTranscriptHasWriteback(t *testing.T) {
	dir := t.TempDir()
	// An actual tool call: the quoted tool name appears in a tool_use entry.
	call := filepath.Join(dir, "call.jsonl")
	os.WriteFile(call, []byte(`{"type":"tool_use","name":"mesh_append_note","input":{}}`+"\n"), 0o644)
	if !transcriptHasWriteback(call) {
		t.Error("a real mesh_append_note tool call should be detected")
	}
	// A mere mention (the injected contract text says: call mesh_append_note ...) must
	// NOT be mistaken for a write-back, or the Stop nudge would never fire.
	mention := filepath.Join(dir, "mention.jsonl")
	os.WriteFile(mention, []byte(`{"role":"user","content":"call mesh_append_note when done"}`+"\n"), 0o644)
	if transcriptHasWriteback(mention) {
		t.Error("an unquoted mention of the tool must NOT count as a write-back")
	}
	if transcriptHasWriteback(filepath.Join(dir, "missing.jsonl")) {
		t.Error("a missing transcript must not count as a write-back")
	}
}
