// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package web

import "testing"

// A member signed in before a restart must still be signed in after it: the
// cookie-signing key is derived from a stable secret, not regenerated per process.
func TestMemberCookieSurvivesRestart(t *testing.T) {
	t.Setenv("MESH_UI_COOKIE_SECRET", "")
	t.Setenv("MESH_UI_TOKEN", "stable-shared-secret")

	before := newMemberAuth(nil, nil, nil)
	cookie := before.sign(7) // issued to the member

	after := newMemberAuth(nil, nil, nil) // simulates a server restart / redeploy
	if id, ok := after.clientFromCookie(cookie); !ok || id != 7 {
		t.Fatalf("cookie issued before restart did not validate after (ok=%v id=%d)", ok, id)
	}
	if before.sign(7) != after.sign(7) {
		t.Fatal("member signing key changed across restarts despite a stable secret")
	}
}

// MESH_UI_COOKIE_SECRET takes priority over MESH_UI_TOKEN.
func TestCookieSecretPriority(t *testing.T) {
	t.Setenv("MESH_UI_COOKIE_SECRET", "explicit-secret")
	t.Setenv("MESH_UI_TOKEN", "other")
	a := newMemberAuth(nil, nil, nil)
	t.Setenv("MESH_UI_TOKEN", "changed") // token rotates, but the explicit secret pins the key
	b := newMemberAuth(nil, nil, nil)
	if a.sign(1) != b.sign(1) {
		t.Fatal("MESH_UI_COOKIE_SECRET should pin the key regardless of MESH_UI_TOKEN")
	}
}
