// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/hooks"
)

// installedProject returns a project dir wired the way `mesh install` leaves it:
// an .mcp.json holding the mesh server (pointing at an absolute binary path, as the
// real one does) plus the session hooks in .claude/settings.json.
func installedProject(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	proj := t.TempDir()
	if _, _, err := hooks.InstallMCP(proj, "/vault", "/abs/path/to/mesh"); err != nil {
		t.Fatal(err)
	}
	if _, err := hooks.Install(hooks.Options{ProjectDir: proj, Vault: "/vault", Bin: "/abs/path/to/mesh", EnforceWriteback: true}); err != nil {
		t.Fatal(err)
	}
	return proj
}

func mcpJSON(t *testing.T, proj string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(proj, ".mcp.json"))
	if err != nil {
		return ""
	}
	return string(data)
}

// TestHooksUninstallReceiptNamesTheRegistrationItLeaves is the honesty half of the
// uninstall fix. `mesh hooks uninstall` printed "removed 2 mesh hook(s) from ..." and
// stopped there, which reads as a complete uninstall. It is not: the MCP registration
// survives, holding the ABSOLUTE path of the mesh binary, so someone who trials Mesh,
// runs the only command that sounds like an uninstall, and then deletes the binary is
// left with a permanently failing MCP server their agent retries on every launch.
//
// The receipt has to name what it did NOT remove, and name the command that does.
func TestHooksUninstallReceiptNamesTheRegistrationItLeaves(t *testing.T) {
	proj := installedProject(t)

	c := hooksUninstallCmd()
	c.SetArgs([]string{"--dir", proj})
	out, err := capture(t, c.Execute)
	if err != nil {
		t.Fatalf("hooks uninstall: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removed 3 mesh hook(s)") { // 2 SessionStart groups + 1 Stop
		t.Errorf("receipt should still report the hooks it removed, got:\n%s", out)
	}
	for _, want := range []string{".mcp.json", "mesh install --remove"} {
		if !strings.Contains(out, want) {
			t.Errorf("receipt does not mention %q, so it still reads as a complete uninstall:\n%s", want, out)
		}
	}
	// And the claim must be true: the registration really is still there.
	if !strings.Contains(mcpJSON(t, proj), "/abs/path/to/mesh") {
		t.Error("this test assumes hooks uninstall leaves the MCP registration; it no longer does")
	}
}

// TestInstallRemoveDropsTheRegistrationAndTheHooks is the capability half: before
// this flag existed there was no way, anywhere in the binary, to undo the MCP
// registration `mesh install` wrote.
func TestInstallRemoveDropsTheRegistrationAndTheHooks(t *testing.T) {
	proj := installedProject(t)

	c := installCmd()
	c.SetArgs([]string{"--remove", "--dir", proj})
	out, err := capture(t, c.Execute)
	if err != nil {
		t.Fatalf("install --remove: %v\n%s", err, out)
	}
	if got := mcpJSON(t, proj); strings.Contains(got, "/abs/path/to/mesh") || strings.Contains(got, `"mesh"`) {
		t.Errorf(".mcp.json still registers mesh after `install --remove`:\n%s", got)
	}
	if st, _ := hooks.GetStatus(proj); st.Installed || st.ReadHook || st.WriteHook {
		t.Errorf("session hooks survived `install --remove`: %+v", st)
	}
	// The receipt has to say what it deliberately kept, or the next question is
	// "did that just delete my notes?".
	for _, want := range []string{"removed the Mesh MCP server", "Left alone on purpose", "vault"} {
		if !strings.Contains(out, want) {
			t.Errorf("receipt is missing %q:\n%s", want, out)
		}
	}
}

// TestInstallRemoveIsSafeOnAProjectThatNeverHadMesh: a user who runs the uninstall in
// the wrong directory, or twice, must get a plain report and exit 0, not an error and
// not a half-done removal.
func TestInstallRemoveIsSafeOnAProjectThatNeverHadMesh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	proj := t.TempDir()

	c := installCmd()
	c.SetArgs([]string{"--remove", "--dir", proj})
	out, err := capture(t, c.Execute)
	if err != nil {
		t.Fatalf("install --remove on a clean dir should not fail: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no Mesh MCP server registered") {
		t.Errorf("receipt should say there was nothing registered:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(proj, ".mcp.json")); err == nil {
		t.Error("uninstall created a config file in a project that had none")
	}
}
