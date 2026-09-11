// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package updatecheck

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckFindsNewReleaseAndCachesIt(t *testing.T) {
	t.Setenv("MESH_NO_UPDATE_CHECK", "")
	now := time.Unix(1_800_000_000, 0)
	var calls atomic.Int32
	checker := &Checker{
		Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			if r.URL.String() != DefaultEndpoint {
				t.Errorf("endpoint = %s", r.URL)
			}
			return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"Version":"v0.12.0"}`)), Header: make(http.Header)}, nil
		})},
		Endpoint: DefaultEndpoint, CachePath: t.TempDir() + "/update.json", MaxAge: 24 * time.Hour, Now: func() time.Time { return now },
	}
	for i := 0; i < 2; i++ {
		got, err := checker.Check(context.Background(), "v0.11.0")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Available || got.Latest != "v0.12.0" || !strings.Contains(got.Command, "@v0.12.0") {
			t.Fatalf("notice = %+v", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("network calls = %d, want one cached call", calls.Load())
	}
}

func TestCheckSkipsDeveloperAndDisabledBuilds(t *testing.T) {
	var calls atomic.Int32
	checker := &Checker{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, context.Canceled
	})}, Endpoint: DefaultEndpoint}
	if got, err := checker.Check(context.Background(), "dev"); err != nil || got.Available {
		t.Fatalf("developer check = %+v, %v", got, err)
	}
	t.Setenv("MESH_NO_UPDATE_CHECK", "true")
	if got, err := checker.Check(context.Background(), "v0.1.0"); err != nil || got.Available {
		t.Fatalf("disabled check = %+v, %v", got, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("disabled/skipped builds made %d network calls", calls.Load())
	}
}

func TestNoticeRejectsInvalidOrOlderVersions(t *testing.T) {
	for _, latest := range []string{"garbage", "v0.10.0", "v0.11.0"} {
		if got := notice("v0.11.0", latest); got.Available {
			t.Errorf("latest %q produced an upgrade notice: %+v", latest, got)
		}
	}
}
