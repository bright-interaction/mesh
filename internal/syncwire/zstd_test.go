// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package syncwire

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestAcceptsZstd(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"zstd", true},
		{"gzip, ZSTD;q=0.5", true},
		{"*;q=1", true},
		{"zstd;q=0", false},
		{"*;q=1, zstd;q=0", false},
		{"gzip", false},
		{"zstd;q=bogus", false},
		{"zstd;q=2", false},
	} {
		if got := AcceptsZstd(tc.header); got != tc.want {
			t.Errorf("AcceptsZstd(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestZstdRoundTripAndDecodedLimit(t *testing.T) {
	source := []byte(strings.Repeat("Mesh sync payload\n", 1000))
	var encoded bytes.Buffer
	zw, err := NewZstdWriter(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(source); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := NewZstdReader(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if _, err := ReadAllLimited(zr, int64(len(source)-1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize decode error = %v, want ErrTooLarge", err)
	}
}
