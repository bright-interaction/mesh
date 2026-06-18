package main

import (
	"encoding/json"
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

func TestHooksMergeIdempotentAndUninstall(t *testing.T) {
	hooks := map[string]any{}
	appendHook(hooks, "SessionStart", map[string]any{"matcher": "startup", "hooks": []any{cmdEntry(`mesh orient --hook --vault /v`)}})
	settings := map[string]any{"hooks": hooks, "model": "keepme"}

	if !settingsHasCommand(settings, "orient --hook") {
		t.Fatal("settingsHasCommand should find the installed orient command")
	}
	if settingsHasCommand(settings, "hooks stop-check") {
		t.Fatal("stop-check was not installed; should not be reported present")
	}
	// unrelated keys must be preserved through a marshal round-trip.
	out, _ := json.Marshal(settings)
	var rt map[string]any
	json.Unmarshal(out, &rt)
	if rt["model"] != "keepme" {
		t.Error("unrelated settings keys must be preserved")
	}
}
