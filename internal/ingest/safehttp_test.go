// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"net"
	"testing"
)

func TestBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "::1", "fe80::1", "fc00::1", "0.0.0.0", "::",
		"100.64.0.1", "100.100.100.100", "192.0.0.192", "::ffff:127.0.0.1", "::ffff:10.0.0.1"}
	for _, s := range blocked {
		if !blockedIP(net.ParseIP(s)) {
			t.Errorf("blockedIP(%s) = false, want true (SSRF target)", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "140.82.112.3", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if blockedIP(net.ParseIP(s)) {
			t.Errorf("blockedIP(%s) = true, want false (public)", s)
		}
	}
	if !blockedIP(nil) {
		t.Error("blockedIP(nil) should be true")
	}
}

func TestJiraSiteValidatedAtSave(t *testing.T) {
	bad := []ConnectorConfig{
		{Type: "jira", Site: "http://acme.atlassian.net"}, // not https
		{Type: "jira", Site: "acme.atlassian.net"},        // no scheme
		{Type: "jira", Site: "https://"},                  // no host
		{Type: "jira", Site: ""},                          // empty
	}
	for _, cc := range bad {
		if _, err := cc.Build(); err == nil {
			t.Errorf("Build(jira site=%q) = nil err, want rejection", cc.Site)
		}
	}
	if _, err := (ConnectorConfig{Type: "jira", Site: "https://acme.atlassian.net"}).Build(); err != nil {
		t.Errorf("valid jira site rejected: %v", err)
	}
}
