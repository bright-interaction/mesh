// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpgradePrebuiltVerifiesAndAtomicallyReplaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("automatic replacement is deliberately unsupported on Windows")
	}
	name, err := upgradeBinaryName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	target := []byte("#!/bin/sh\nprintf '%s\\n' '{\"name\":\"mesh\",\"release\":\"v0.13.0\"}'\n")
	sum := sha256.Sum256(target)
	var downloadCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/about":
			_ = json.NewEncoder(w).Encode(upgradeAbout{ReleaseVersion: "v0.13.0"})
		case "/download/SHA256SUMS":
			if r.Header.Get("Authorization") != "Bearer joined-token" {
				t.Errorf("checksum request authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + name + "\n"))
		case "/download/" + name:
			downloadCalls++
			if r.Header.Get("Authorization") != "Bearer joined-token" {
				t.Errorf("binary request authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write(target)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	root := t.TempDir()
	if err := writeCredentials(root, credentials{HubURL: ts.URL, Token: "joined-token", VaultID: "team"}); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "mesh")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := UpgradePrebuilt(context.Background(), UpgradeOptions{
		VaultDir: root, Executable: exe, CurrentRelease: "v0.12.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Changed || got.Latest != "v0.13.0" || downloadCalls != 1 {
		t.Fatalf("upgrade = %+v, downloads=%d", got, downloadCalls)
	}
	if b, err := os.ReadFile(exe); err != nil || string(b) != string(target) {
		t.Fatalf("installed bytes match=%v err=%v", string(b) == string(target), err)
	}
}

func TestUpgradePrebuiltChecksumFailurePreservesCurrentExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("automatic replacement is deliberately unsupported on Windows")
	}
	name, err := upgradeBinaryName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/about":
			_, _ = w.Write([]byte(`{"release_version":"v0.13.0"}`))
		case "/download/SHA256SUMS":
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  " + name + "\n"))
		case "/download/" + name:
			_, _ = w.Write([]byte("not the promised binary"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	exe := filepath.Join(t.TempDir(), "mesh")
	if err := os.WriteFile(exe, []byte("known-good"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = UpgradePrebuilt(context.Background(), UpgradeOptions{
		HubURL: ts.URL, Executable: exe, CurrentRelease: "v0.12.0",
	})
	if err == nil || !strings.Contains(err.Error(), "does not match SHA256SUMS") {
		t.Fatalf("checksum error = %v", err)
	}
	if b, readErr := os.ReadFile(exe); readErr != nil || string(b) != "known-good" {
		t.Fatalf("current executable changed: %q err=%v", b, readErr)
	}
}

func TestUpgradePrebuiltCheckOnlyDoesNotDownload(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_ = json.NewEncoder(w).Encode(upgradeAbout{ReleaseVersion: "v0.13.0"})
	}))
	defer ts.Close()
	got, err := UpgradePrebuilt(context.Background(), UpgradeOptions{
		HubURL: ts.URL, CurrentRelease: "v0.12.0", CheckOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Changed || got.UpToDate || len(paths) != 1 || paths[0] != "/about" {
		t.Fatalf("check = %+v paths=%v", got, paths)
	}
}

func TestUpgradeHubRequiresHTTPSOutsideLoopback(t *testing.T) {
	if _, err := validateUpgradeHubURL("http://mesh.example"); err == nil {
		t.Fatal("plain remote HTTP was accepted")
	}
}

func TestBoundedUpgradeBufferCapsIdentityOutput(t *testing.T) {
	var b boundedUpgradeBuffer
	b.max = 4
	if n, err := b.Write([]byte("123456")); err != nil || n != 6 {
		t.Fatalf("Write = (%d, %v), want (6, nil)", n, err)
	}
	if got := b.String(); got != "1234" || !b.overflow {
		t.Fatalf("buffer = %q overflow=%v", got, b.overflow)
	}
	if n, err := b.Write([]byte("more")); err != nil || n != 4 || b.Len() != 4 {
		t.Fatalf("overflow Write = (%d, %v), len=%d", n, err, b.Len())
	}
}
