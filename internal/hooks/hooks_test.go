package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func opts(dir string, enforce bool) Options {
	return Options{ProjectDir: dir, Vault: "/v", Bin: "/bin/mesh", EnforceWriteback: enforce}
}

func TestInstallIdempotentAndUninstall(t *testing.T) {
	dir := t.TempDir()
	// pre-existing unrelated settings must be preserved.
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"model":"keepme"}`), 0o644)

	r, err := Install(opts(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Added) != 2 {
		t.Fatalf("first install should add 2 hooks (read + write), got %d", len(r.Added))
	}
	st, _ := GetStatus(dir)
	if !st.Installed || !st.ReadHook || !st.WriteHook {
		t.Fatalf("status after install = %+v", st)
	}
	// unrelated key preserved.
	data, _ := os.ReadFile(st.SettingsPath)
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["model"] != "keepme" {
		t.Error("unrelated settings key was dropped")
	}

	// idempotent: a second install adds nothing.
	r2, _ := Install(opts(dir, true))
	if len(r2.Added) != 0 {
		t.Errorf("re-install should add nothing, got %v", r2.Added)
	}

	removed, _, err := Uninstall(dir)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 { // 2 SessionStart groups + 1 Stop group
		t.Errorf("uninstall removed %d, want 3", removed)
	}
	st2, _ := GetStatus(dir)
	if st2.Installed || st2.ReadHook || st2.WriteHook {
		t.Errorf("status after uninstall = %+v", st2)
	}
}

func TestReadOnlyInstall(t *testing.T) {
	dir := t.TempDir()
	r, _ := Install(opts(dir, false)) // enforceWriteback=false
	if len(r.Added) != 1 {
		t.Fatalf("read-only install should add only the read hook, got %d", len(r.Added))
	}
	st, _ := GetStatus(dir)
	if !st.ReadHook || st.WriteHook {
		t.Errorf("read-only status = %+v (want read=true write=false)", st)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	o := opts(dir, true)
	o.DryRun = true
	r, _ := Install(o)
	if r.Preview == "" || !strings.Contains(r.Preview, "SessionStart") {
		t.Error("dry run should return a preview containing the hooks")
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.json")); err == nil {
		t.Error("dry run must not write the settings file")
	}
}
