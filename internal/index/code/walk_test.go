// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package code

import "testing"

func TestLang(t *testing.T) {
	cases := map[string]string{
		".go": "go", ".ts": "ts", ".TSX": "tsx", ".py": "py",
		".svelte": "svelte", ".astro": "astro", ".txt": "", ".md": "", "": "",
	}
	for ext, want := range cases {
		if got := Lang(ext); got != want {
			t.Errorf("Lang(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestSkipFile(t *testing.T) {
	for _, f := range []string{"app.min.js", "vendor.bundle.js", "types.d.ts"} {
		if !skipFile(f) {
			t.Errorf("skipFile(%q) = false, want true", f)
		}
	}
	for _, f := range []string{"main.go", "client.ts", "App.svelte", "view.py"} {
		if skipFile(f) {
			t.Errorf("skipFile(%q) = true, want false", f)
		}
	}
}

func TestCodeSkipDir(t *testing.T) {
	for _, d := range []string{"node_modules", "dist", ".git", "vendor", ".svelte-kit", "__pycache__"} {
		if !codeSkipDir("/root/"+d, "/root", d) {
			t.Errorf("codeSkipDir(%q) = false, want true", d)
		}
	}
	if codeSkipDir("/root/internal", "/root", "internal") {
		t.Error("internal should not be skipped")
	}
}
