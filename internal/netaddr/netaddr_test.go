// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package netaddr

import "testing"

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:2222", true},
		{"127.0.0.1", true},
		{"127.1.2.3:80", true},
		{"localhost:7474", true},
		{"localhost", true},
		{"[::1]:2222", true},
		{"::1", true},
		// The whole point of the package: a bare port binds every interface.
		{":2222", false},
		{"", false},
		{"0.0.0.0:2222", false},
		{"[::]:2222", false},
		{"192.168.1.20:2222", false},
		{"10.0.0.5:2222", false},
		{"example.com:2222", false},
		// A name we cannot resolve here is not provably local, so fail closed.
		{"LOCALHOST:2222", false},
	}
	for _, tc := range tests {
		if got := IsLoopback(tc.addr); got != tc.want {
			t.Errorf("IsLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
