// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import "testing"

// Close is called from more than one shutdown path (Server.Close, plus any
// caller that closed the store itself), and `close(s.done)` on an already-closed
// channel panics the process. A panic during shutdown loses whatever the rest of
// the shutdown was going to flush, so Close has to be idempotent.
func TestStoreCloseIsIdempotent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first := s.Close()
	second := s.Close() // this panicked before Close became idempotent
	if second != first {
		t.Errorf("second Close returned %v, want the first result %v", second, first)
	}
	if third := s.Close(); third != first {
		t.Errorf("third Close returned %v, want %v", third, first)
	}
}
