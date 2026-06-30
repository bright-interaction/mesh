// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

// Package guards turns institutional gotchas into candidate enforcement: for a gotcha
// with a concrete anti-pattern, the BYOAI LLM proposes a grep-style pre-commit check
// (pattern + file globs + message). The human pastes the ones that fit into the hook,
// closing the loop from "we learned this" to "the repo enforces it". BYOAI via the
// existing llm.Client (claude -p); the model proposes, the human decides.
package guards

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
)

// Guard is a proposed enforcement for a gotcha.
type Guard struct {
	GotchaID string `json:"gotcha_id"`
	Title    string `json:"title"`
	Applies  bool   `json:"applies"`  // false = not mechanically checkable (judgment/architecture)
	Pattern  string `json:"pattern"`  // RE2/grep -E regex flagging the anti-pattern
	Globs    string `json:"globs"`    // comma file globs the check applies to (e.g. "*.go,*.ts" or "Dockerfile")
	Message  string `json:"message"`  // one-line failure message
	Severity string `json:"severity"` // block | warn
	Reason   string `json:"reason"`   // why applies is true/false
}

const system = `You turn a team engineering gotcha into a pre-commit GUARD: a grep-style regex that flags the anti-pattern in source files, so the mistake cannot be reintroduced.

Output STRICT JSON only (no prose, no fences):
{"applies": true|false, "pattern": "...", "globs": "...", "message": "...", "severity": "block|warn", "reason": "..."}

Set applies=false when the gotcha is NOT mechanically checkable with a simple regex (it is about judgment, architecture, ordering, or runtime behavior). Most architectural gotchas are applies=false; be honest.

When applies=true:
- pattern: a RE2/grep -E regex matching the BAD code/text (low false positives). It MUST be POSIX/RE2 compatible: NO lookahead/lookbehind ((?=, (?!, (?<), no backreferences (grep -E and Go RE2 reject them). Example: gotcha "use bun not npm" -> pattern "npm install|package-lock\\.json".
- globs: comma-separated file globs to scan (e.g. "*.go", "Dockerfile,*.dockerfile", "*.ts,*.tsx"). Pick the narrowest set that catches it.
- message: the one-line failure shown to the developer, ending with what to do instead.
- severity: "block" for correctness/security, "warn" for style.
Keep it conservative: a guard that fires on legitimate code is worse than none.`

// Suggest asks the model to propose a guard for one gotcha.
func Suggest(ctx context.Context, client llm.Client, g index.GotchaRow) (Guard, error) {
	u := fmt.Sprintf("Gotcha:\ntitle: %s\ndo: %s\ndont: %s\nwhy: %s", g.Title, g.Do, g.Dont, g.Why)
	out, err := client.Complete(ctx, system, u)
	if err != nil {
		return Guard{}, err
	}
	gd, err := parseGuard(out)
	if err != nil {
		return Guard{}, err
	}
	gd.GotchaID, gd.Title = g.ID, g.Title
	return gd, nil
}

func parseGuard(out string) (Guard, error) {
	s := strings.TrimSpace(out)
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j < i {
		return Guard{}, fmt.Errorf("no JSON object in guard output")
	}
	var g Guard
	if err := json.Unmarshal([]byte(s[i:j+1]), &g); err != nil {
		return Guard{}, err
	}
	if g.Severity != "warn" {
		g.Severity = "block"
	}
	return g, nil
}

// ShellSnippet renders applicable guards as a copy-paste bash block for a pre-commit
// hook: each greps the staged-relevant globs for its pattern and fails with the message.
// Only guards that applies==true and have a pattern + globs are emitted.
func ShellSnippet(guards []Guard) string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n# Mesh-suggested guards (review before enabling). Generated from gotchas.\nset -u\nfail=0\n")
	for _, g := range guards {
		if !g.Applies || strings.TrimSpace(g.Pattern) == "" || strings.TrimSpace(g.Globs) == "" {
			continue
		}
		// grep -E (POSIX/RE2) rejects lookaround/backreferences; skip such patterns
		// rather than emit a check that errors at runtime.
		if strings.Contains(g.Pattern, "(?") || strings.Contains(g.Pattern, `\1`) {
			b.WriteString(fmt.Sprintf("\n# SKIPPED %s: pattern uses lookaround/backrefs (not grep -E compatible); refine by hand: %s\n", g.Title, g.Pattern))
			continue
		}
		includes := ""
		for _, gl := range strings.Split(g.Globs, ",") {
			if gl = strings.TrimSpace(gl); gl != "" {
				includes += fmt.Sprintf(" --include='%s'", gl)
			}
		}
		msg := strings.ReplaceAll(g.Message, "'", "")
		b.WriteString(fmt.Sprintf("\n# %s  [%s]\nif grep -rnE%s -- %s . >/dev/null 2>&1; then\n  echo 'GUARD: %s'; fail=1\nfi\n",
			g.Title, g.Severity, includes, shellQuote(g.Pattern), msg))
	}
	b.WriteString("\nexit $fail\n")
	return b.String()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
