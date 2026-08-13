// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package sshserve

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestServeFailsClosedWithoutAuth is the security gate: no authorized_keys and no
// explicit anonymous opt-in must refuse to start, before opening the vault or
// binding a port.
func TestServeFailsClosedWithoutAuth(t *testing.T) {
	err := Serve(context.Background(), t.TempDir(), Options{Addr: "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "refusing to start without auth") {
		t.Fatalf("expected fail-closed auth error, got %v", err)
	}
}

// TestServeRefusesAnonymousBeyondLoopback is the other half of that gate, and the
// half that was missing. --allow-anonymous hands every connection the whole vault
// with no credential; its help said "localhost demos only" and the CLI default was
// ":2222", so the demo listened on every interface and anyone on the LAN (or on the
// internet, for a VPS) could `ssh -p 2222 <host>` and browse the entire graph in the
// TUI. The identical shape is already refused one command over: `mesh mcp --http`
// will not bind past loopback without a token.
//
// Each address here is one a user really types, and none of them may reach a bind.
func TestServeRefusesAnonymousBeyondLoopback(t *testing.T) {
	for _, addr := range []string{
		":2222",           // the old default: every interface
		"0.0.0.0:2222",    // the same thing, spelled out
		"[::]:2222",       // and over IPv6
		"192.168.1.7:222", // a LAN interface
	} {
		err := Serve(context.Background(), t.TempDir(), Options{Addr: addr, AllowAnon: true})
		if err == nil {
			t.Errorf("Serve(%q, AllowAnon) returned nil: an unauthenticated bind beyond loopback must be refused", addr)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to bind") || !strings.Contains(err.Error(), "--allow-anonymous") {
			t.Errorf("Serve(%q, AllowAnon) error = %v, want the fail-closed refusal naming the remedy", addr, err)
		}
	}
}

// TestServeAllowsAuthenticatedBindBeyondLoopback pins the other direction, so the
// guard cannot be "fixed" into refusing the legitimate deployment: with an
// authorized_keys file, serving teammates on a routable address is the whole point
// of the command. A bad key file is rejected by wish itself, which is why this one
// gets past the address check and fails on the keys instead.
func TestServeAllowsAuthenticatedBindBeyondLoopback(t *testing.T) {
	dir := t.TempDir()
	err := Serve(context.Background(), dir, Options{
		Addr:         "0.0.0.0:0",
		AuthKeysPath: filepath.Join(dir, "authorized_keys"), // deliberately absent
	})
	if err != nil && strings.Contains(err.Error(), "refusing to bind") {
		t.Fatalf("an authenticated bind beyond loopback must not be refused by the anonymous guard, got %v", err)
	}
}
