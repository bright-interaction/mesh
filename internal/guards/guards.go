// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
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
	"github.com/bright-interaction/mesh/internal/shellpath"
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
			b.WriteString(fmt.Sprintf("\n# SKIPPED %s: pattern uses lookaround/backrefs (not grep -E compatible); refine by hand: %s\n", commentSafe(g.Title), commentSafe(g.Pattern)))
			continue
		}
		includes := ""
		for _, gl := range splitGlobs(g.Globs) {
			if gl = strings.TrimSpace(gl); gl != "" {
				// shellpath.Quote, not a hand-written ' + gl + ': a glob carrying an
				// apostrophe closed the quote and the rest of the glob became shell syntax
				// in a file the user is invited to run as a pre-commit hook. Globs still
				// come out quoted (`*` is not in the safe set), so grep, not the shell,
				// expands them.
				includes += " --include=" + shellpath.Quote(gl)
			}
		}
		msg := strings.ReplaceAll(g.Message, "'", "")
		b.WriteString(fmt.Sprintf("\n# %s  [%s]\nif grep -rnE%s -- %s . >/dev/null 2>&1; then\n  echo 'GUARD: %s'; fail=1\nfi\n",
			commentSafe(g.Title), g.Severity, includes, shellpath.Quote(g.Pattern), msg))
	}
	b.WriteString("\nexit $fail\n")
	return b.String()
}

// commentSafe flattens anything destined for a `#` comment line in the generated hook.
//
// Guard.Title and Guard.Pattern come from note frontmatter in the vault: team-synced,
// connector-ingested, and written by an LLM. A newline in a Title ends the comment, and
// every following line of that Title becomes an executable statement in a file whose
// whole purpose is to be installed as a pre-commit hook. Measured: a Title of
// "harmless\ntouch /tmp/pwned" created /tmp/pwned when the snippet was run.
//
// Message and Severity are already safe (Message is single-quoted with its own quotes
// stripped, Severity is normalised to block/warn above), so this covers the two fields
// that reach a comment raw.
func commentSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

// splitGlobs splits a comma-separated glob list WITHOUT cutting inside a brace set.
//
// `strings.Split(g.Globs, ",")` turned "*.{js,ts}" into "*.{js" and "ts}", and the
// system prompt that produces these fields teaches comma lists and glob syntax in the
// same breath, so the shape is invited. Two failures stacked: the split mangled it, and
// grep's --include uses fnmatch, which has no brace expansion, so even unsplit it would
// match nothing. A guard that silently never fires is worse than no guard, so a brace
// set is expanded here into the separate --include globs grep can actually use.
func splitGlobs(globs string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	for _, r := range globs {
		switch {
		case r == '{':
			depth++
			cur.WriteRune(r)
		case r == '}':
			if depth > 0 {
				depth--
			}
			cur.WriteRune(r)
		case r == ',' && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())

	var expanded []string
	for _, g := range out {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		expanded = append(expanded, expandBraces(g)...)
	}
	return expanded
}

// expandBraces turns "*.{js,ts}" into ["*.js", "*.ts"]. One level is enough for the
// shapes these fields carry; anything unbalanced is passed through untouched so a weird
// glob degrades to "matches nothing" rather than to mangled shell.
func expandBraces(g string) []string {
	o := strings.Index(g, "{")
	if o < 0 {
		return []string{g}
	}
	c := strings.Index(g[o:], "}")
	if c < 0 {
		return []string{g}
	}
	c += o
	prefix, suffix := g[:o], g[c+1:]
	var out []string
	for _, alt := range strings.Split(g[o+1:c], ",") {
		if alt = strings.TrimSpace(alt); alt != "" {
			out = append(out, expandBraces(prefix+alt+suffix)...)
		}
	}
	if len(out) == 0 {
		return []string{prefix + suffix}
	}
	return out
}
