// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package buildinfo

import "testing"

// The primary install path is `go install <module>/cmd/mesh@vX.Y.Z`, which passes no
// ldflags. Before this, every released binary answered "dev" and a bug report could not
// be tied to a version. Precedence is most-explicit-first.
func TestVerPrecedence(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	t.Setenv("MESH_VERSION", "from-env")
	Version = "from-ldflags"
	if got := Ver(); got != "from-env" {
		t.Errorf("MESH_VERSION must win: got %q", got)
	}

	t.Setenv("MESH_VERSION", "")
	if got := Ver(); got != "from-ldflags" {
		t.Errorf("ldflags stamp must beat the embedded fallback: got %q", got)
	}

	// With neither, Ver falls through to the embedded module version. Under `go test`
	// that is usually empty or "(devel)", both of which must yield "dev" rather than a
	// confusing bare "(devel)" leaking into user-facing output.
	Version = "dev"
	if got := Ver(); got == "(devel)" || got == "" {
		t.Errorf("Ver must never return %q to a user", got)
	}
}

func TestReleaseVerKeepsSemverSeparateFromCommitIdentity(t *testing.T) {
	origVersion, origRelease := Version, ReleaseVersion
	t.Cleanup(func() { Version, ReleaseVersion = origVersion, origRelease })
	t.Setenv("MESH_VERSION", "")
	t.Setenv("MESH_RELEASE_VERSION", "")
	Version = "43c76c970"
	ReleaseVersion = "v0.11.0"
	if got := Ver(); got != "43c76c970" {
		t.Fatalf("Ver = %q, want exact build identity", got)
	}
	if got := ReleaseVer(); got != "v0.11.0" {
		t.Fatalf("ReleaseVer = %q, want public semver", got)
	}

	t.Setenv("MESH_RELEASE_VERSION", "v0.12.0")
	if got := ReleaseVer(); got != "v0.12.0" {
		t.Fatalf("release env override = %q", got)
	}
}
