// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package guards

import (
	"context"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
)

func TestParseGuard(t *testing.T) {
	g, err := parseGuard("```json\n{\"applies\":true,\"pattern\":\"npm install\",\"globs\":\"Dockerfile\",\"message\":\"use bun\",\"severity\":\"block\"}\n```")
	if err != nil || !g.Applies || g.Pattern != "npm install" || g.Severity != "block" {
		t.Fatalf("parseGuard fenced = %+v, %v", g, err)
	}
	g2, _ := parseGuard(`{"applies": false, "reason": "architectural"}`)
	if g2.Applies {
		t.Fatalf("applies should be false: %+v", g2)
	}
	if g2.Severity != "block" { // default when unspecified
		t.Fatalf("default severity = %q", g2.Severity)
	}
}

func TestShellSnippetSkipsNonApplicable(t *testing.T) {
	gs := []Guard{
		{Title: "bun not npm", Applies: true, Pattern: "npm install", Globs: "Dockerfile,*.sh", Message: "use bun", Severity: "block"},
		{Title: "architecture call", Applies: false, Pattern: "", Message: "n/a"},
	}
	out := ShellSnippet(gs)
	if !strings.Contains(out, "npm install") || !strings.Contains(out, "--include='Dockerfile'") || !strings.Contains(out, "use bun") {
		t.Fatalf("snippet missing the applicable guard:\n%s", out)
	}
	if strings.Contains(out, "architecture call") {
		t.Fatalf("snippet included a non-applicable guard:\n%s", out)
	}
}

func TestSuggestWithStub(t *testing.T) {
	stub := llm.Func(func(_ context.Context, _, _ string) (string, error) {
		return `{"applies":true,"pattern":"chi\\.RealIP","globs":"*.go","message":"use ClientIP middleware with trusted proxies","severity":"block","reason":"textual"}`, nil
	})
	g, err := Suggest(context.Background(), stub, index.GotchaRow{ID: "g1", Title: "chi RealIP takeover", Dont: "use chi RealIP"})
	if err != nil || !g.Applies || g.GotchaID != "g1" || g.Pattern == "" {
		t.Fatalf("Suggest = %+v, %v", g, err)
	}
}
