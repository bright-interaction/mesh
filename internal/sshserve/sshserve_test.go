package sshserve

import (
	"context"
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
