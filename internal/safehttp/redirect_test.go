// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package safehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The custom-header strip compared the next hop against the PREVIOUS hop. Go's
// makeHeadersCopier re-copies from the initial request on every hop and runs before
// CheckRedirect, so a header deleted at hop 1 came back at hop 2 - and hop 2, being
// attacker-host to attacker-host, looked same-host and was not stripped. One open
// redirect on a genuinely trusted host was enough to recover the key.
func TestAPIKeyStaysStrippedAcrossMultipleRedirectHops(t *testing.T) {
	var got []string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-Api-Key"))
		if r.URL.Path == "/hop1" {
			http.Redirect(w, r, "/hop2", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/hop1", http.StatusFound)
	}))
	defer origin.Close()

	req, err := http.NewRequest("GET", origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "SUPERSECRET")

	resp, err := LoopbackAllowed(10 * time.Second).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if len(got) != 2 {
		t.Fatalf("attacker saw %d requests, want 2 (hop1, hop2): %v", len(got), got)
	}
	for i, v := range got {
		if v != "" {
			t.Errorf("hop %d leaked the API key to the attacker host: %q", i+1, v)
		}
	}
}
