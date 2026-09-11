// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/syncwire"
)

func TestSyncNegotiatesZstdResponseThenStreamsZstdRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if got := r.Header.Get("Accept-Encoding"); got != syncwire.EncodingZstd {
			t.Errorf("call %d Accept-Encoding = %q", call, got)
		}
		var body = r.Body
		if call == 1 {
			if got := r.Header.Get("Content-Encoding"); got != "" {
				t.Errorf("first Content-Encoding = %q, want plain compatibility request", got)
			}
		} else {
			if got := r.Header.Get("Content-Encoding"); got != syncwire.EncodingZstd {
				t.Errorf("second Content-Encoding = %q", got)
			}
			zr, err := syncwire.NewZstdReader(r.Body)
			if err != nil {
				t.Errorf("zstd request: %v", err)
				return
			}
			defer zr.Close()
			body = zr.IOReadCloser()
		}
		var req syncproto.SyncRequest
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		w.Header().Set(syncproto.SyncEncodingHeader, syncwire.EncodingZstd)
		w.Header().Set("Content-Encoding", syncwire.EncodingZstd)
		zw, err := syncwire.NewZstdWriter(w)
		if err != nil {
			t.Errorf("zstd response: %v", err)
			return
		}
		_ = json.NewEncoder(zw).Encode(syncproto.SyncResponse{HeadSHA: "head"})
		_ = zw.Close()
	}))
	defer server.Close()

	client := New(server.URL, "token")
	first, err := client.Sync(syncproto.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.RequestZstd {
		t.Fatal("hub request-zstd advertisement was not captured")
	}
	client.UseZstdSyncRequests(first.RequestZstd)
	if _, err := client.Sync(syncproto.SyncRequest{BaseSHA: "head"}); err != nil {
		t.Fatal(err)
	}
}

func TestSyncSafelyFallsBackAfterOldHubRejectsZstdBeforeDecode(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Content-Encoding") == syncwire.EncodingZstd {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("{\"error\":\"bad request\"}\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "old-hub"})
	}))
	defer server.Close()

	client := New(server.URL, "token")
	client.UseZstdSyncRequests(true)
	resp, err := client.Sync(syncproto.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || resp.RequestZstd {
		t.Fatalf("calls=%d request_zstd=%v, want safe plain retry and capability cleared", calls.Load(), resp.RequestZstd)
	}
}
