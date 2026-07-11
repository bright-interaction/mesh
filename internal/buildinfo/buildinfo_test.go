// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package buildinfo

import "testing"

func TestVerEnvOverride(t *testing.T) {
	if Ver() != Version {
		t.Fatalf("default Ver() = %q, want %q", Ver(), Version)
	}
	t.Setenv("MESH_VERSION", "v1.2.3")
	if Ver() != "v1.2.3" {
		t.Fatalf("Ver() = %q, want the MESH_VERSION override", Ver())
	}
}

func TestSourceOfferGatedOnEnv(t *testing.T) {
	// Unset: the version notice renders, but no (broken) source link.
	t.Setenv("MESH_SOURCE_URL", "")
	in := FooterInline()
	if !contains(in, "Mesh ") {
		t.Fatalf("footer missing version notice: %q", in)
	}
	if contains(in, "Source code") {
		t.Fatalf("footer offered a source link with no MESH_SOURCE_URL set: %q", in)
	}

	// Set: the source link appears, pointing at the configured URL.
	t.Setenv("MESH_SOURCE_URL", "https://example.com/mesh/tree/abc123")
	in = FooterInline()
	if !contains(in, "Source code") || !contains(in, "https://example.com/mesh/tree/abc123") {
		t.Fatalf("footer missing the configured source link: %q", in)
	}
	if !contains(FooterHTML(), "<footer") {
		t.Fatalf("FooterHTML must wrap the inline notice in a <footer>")
	}
}

// A hostile source URL must be HTML-escaped, never break out of the attribute.
func TestSourceURLEscaped(t *testing.T) {
	t.Setenv("MESH_SOURCE_URL", `"><script>alert(1)</script>`)
	if contains(FooterInline(), "<script>alert(1)</script>") {
		t.Fatalf("source URL was not escaped: %q", FooterInline())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
