// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// A member signed in before a restart must still be signed in after it: the
// cookie-signing key is derived from a stable secret, not regenerated per process.
func TestMemberCookieSurvivesRestart(t *testing.T) {
	t.Setenv("MESH_UI_COOKIE_SECRET", "")
	t.Setenv("MESH_UI_TOKEN", "stable-shared-secret")

	before := newMemberAuth(nil, nil, nil)
	cookie := before.sign(7, 1700000000) // issued to the member

	after := newMemberAuth(nil, nil, nil) // simulates a server restart / redeploy
	if id, ok := after.clientFromCookie(cookie, 1700000000); !ok || id != 7 {
		t.Fatalf("cookie issued before restart did not validate after (ok=%v id=%d)", ok, id)
	}
	// Compared at a fixed expiry: sign() stamps time.Now, so two raw values differ by
	// design. What has to survive a restart is the key, hence the signature.
	if before.signAt(7, 1700000000, 1800000000) != after.signAt(7, 1700000000, 1800000000) {
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
	if a.signAt(1, 1700000000, 1800000000) != b.signAt(1, 1700000000, 1800000000) {
		t.Fatal("MESH_UI_COOKIE_SECRET should pin the key regardless of MESH_UI_TOKEN")
	}
}

// signV1 reproduces the old cookie format: the client rowid alone, no expiry. A value in
// that format has to stop verifying, because nothing but the holder's own browser MaxAge
// ever retired it, which made it a permanent bearer credential.
func signV1(m *memberAuth, id int64) string {
	ids := strconv.FormatInt(id, 10)
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte("mesh-member-v1:" + ids))
	return ids + "." + hex.EncodeToString(mac.Sum(nil))
}

// The member cookie must carry a server-side expiry inside the signed payload, and the
// old unexpiring v1 format must be dead.
func TestMemberCookieExpiry(t *testing.T) {
	t.Setenv("MESH_UI_COOKIE_SECRET", "pinned-for-the-test")
	m := newMemberAuth(nil, nil, nil)
	now := time.Now().Unix()

	const created = int64(1700000000)

	// A cookie signed for an expiry in the past, then hand-edited to claim a future one.
	forged := m.signAt(42, created, now-1)
	extended := "42." + strconv.FormatInt(now+3600, 10) + forged[len(forged)-65:]

	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"freshly issued cookie is accepted", m.sign(42, created), true},
		{"cookie expiring in an hour is accepted", m.signAt(42, created, now+3600), true},
		{"expired cookie is refused", m.signAt(42, created, now-1), false},
		{"long-expired cookie is refused", m.signAt(42, created, now-90*24*3600), false},
		{"old v1 format (no expiry) is refused", signV1(m, 42), false},
		{"expiry cannot be extended without the key", extended, false},
		{"id cannot be swapped without the key", "43." + m.signAt(42, created, now+3600)[3:], false},
		{"garbage is refused", "not-a-cookie", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := m.clientFromCookie(tc.value, created)
			if ok != tc.want {
				t.Fatalf("clientFromCookie(%q) ok = %v, want %v", tc.value, ok, tc.want)
			}
			if ok && id != 42 {
				t.Fatalf("clientFromCookie returned id %d, want 42", id)
			}
		})
	}
}

// The rowid-reuse leg: sqlite hands the deleted maximum rowid back to the next INSERT on
// a table declared without AUTOINCREMENT, so a revoked member's client id can be reissued
// to a brand-new member. The revoked member's cookie is still unexpired and still signed
// with the server's key, so before created_at was bound into the payload it authenticated
// as whoever now holds that id. Binding created_at is what kills it.
func TestMemberCookieDoesNotSurviveARowidReuse(t *testing.T) {
	t.Setenv("MESH_UI_COOKIE_SECRET", "pinned-for-the-test")
	m := newMemberAuth(nil, nil, nil)

	const revokedCreated = int64(1700000000)
	const successorCreated = int64(1700009999) // same rowid, different member, later join

	cookie := m.sign(42, revokedCreated) // minted while the revoked member still existed

	if _, ok := m.clientFromCookie(cookie, revokedCreated); !ok {
		t.Fatal("cookie should validate against the created_at it was minted for")
	}
	if _, ok := m.clientFromCookie(cookie, successorCreated); ok {
		t.Fatal("revoked member's cookie authenticated against the reused rowid's new occupant")
	}
}

// End to end through member(): the roleFor stub reports the SUCCESSOR's created_at, which
// is exactly what the hub store returns after the rowid is recycled.
func TestMemberRejectsCookieAfterRowidReuse(t *testing.T) {
	t.Setenv("MESH_UI_COOKIE_SECRET", "pinned-for-the-test")

	createdAt := int64(1700000000)
	m := newMemberAuth(
		func(string) (int64, string, bool) { return 0, "", false },
		func(int64) map[string]bool { return nil },
		func(id int64) (string, int64, bool) {
			if id == 42 {
				return "member", createdAt, true
			}
			return "", 0, false
		},
	)
	cookie := m.sign(42, createdAt)

	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: memberCookie, Value: cookie})
	if id, _, ok := m.member(req); !ok || id != 42 {
		t.Fatalf("live member rejected: ok=%v id=%d", ok, id)
	}

	// The member is revoked and the rowid is handed to a new client that joined later.
	createdAt = 1700009999
	if _, _, ok := m.member(req); ok {
		t.Fatal("cookie from the revoked member still authenticated after the rowid was reused")
	}
}
