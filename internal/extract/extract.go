// Package extract turns a coding-agent session transcript into candidate write-back
// notes. It is the input side of the flywheel: today write-back is opt-in (the Stop
// hook nudges once and ~most sessions still write nothing), so this lets Mesh pull the
// durable, reusable learnings out of a finished session automatically, for one-click
// review. BYOAI via the existing llm.Client (default `claude -p`, no API key). The
// model is the extractor; this package is the parse + prompt + validation around it.
package extract

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bright-interaction/mesh/internal/llm"
)

// Candidate is one extracted, not-yet-reviewed write-back note. Mirrors the fields
// mesh_append_note takes, so a promoted candidate becomes a note with no remapping.
type Candidate struct {
	Type       string `json:"type"`       // decision | gotcha | post-mortem
	Title      string `json:"title"`      // specific, < ~12 words
	Do         string `json:"do"`         // one line, imperative
	Dont       string `json:"dont"`       // one line, the failure to avoid
	Why        string `json:"why"`        // one line, the reason/evidence
	Confidence string `json:"confidence"` // low | med | high (the model's self-rating)
}

// DigestStats describes what a transcript contained, for the benchmark baseline.
type DigestStats struct {
	Lines        int  `json:"lines"`
	UserMsgs     int  `json:"user_msgs"`
	AsstMsgs     int  `json:"asst_msgs"`
	ToolCalls    int  `json:"tool_calls"`
	HadWriteback bool `json:"had_writeback"` // the agent already called mesh_append_note/write_entity (the current algo)
	DigestChars  int  `json:"digest_chars"`
}

var validType = map[string]bool{"decision": true, "gotcha": true, "post-mortem": true}

// transcript line + message shapes (Claude Code .jsonl). Only the fields we read.
type tLine struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"` // []block or string
	} `json:"message"`
}
type tBlock struct {
	Type  string          `json:"type"` // text | thinking | tool_use | tool_result
	Text  string          `json:"text"`
	Name  string          `json:"name"`  // tool_use
	Input json.RawMessage `json:"input"` // tool_use
}

// Digest streams a transcript .jsonl into a compact, signal-dense summary for the
// extraction prompt: user requests + assistant narration + tool-call names (NOT their
// large outputs). Bounded to maxChars by keeping the first user request (the task) and
// the tail (where conclusions land). Also reports whether the agent already wrote back.
func Digest(path string, maxChars int) (string, DigestStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", DigestStats{}, err
	}
	defer f.Close()

	var st DigestStats
	var firstUser string
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		st.Lines++
		var ln tLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil || ln.Message.Role == "" {
			continue
		}
		role := ln.Message.Role
		blocks := decodeContent(ln.Message.Content)
		for _, bl := range blocks {
			switch bl.Type {
			case "text":
				txt := clip(strings.TrimSpace(bl.Text), 1500)
				if txt == "" {
					continue
				}
				if role == "user" {
					st.UserMsgs++
					if firstUser == "" {
						firstUser = txt
					}
					fmt.Fprintf(&b, "USER: %s\n", txt)
				} else {
					st.AsstMsgs++
					fmt.Fprintf(&b, "ASSISTANT: %s\n", txt)
				}
			case "tool_use":
				st.ToolCalls++
				if bl.Name == "mesh_append_note" || bl.Name == "mesh_write_entity" {
					st.HadWriteback = true
				}
				fmt.Fprintf(&b, "TOOL %s(%s)\n", bl.Name, toolArg(bl.Name, bl.Input))
			}
			// thinking + tool_result are intentionally skipped: verbose and low-signal
			// for extracting durable learnings (the agent narrates outcomes in text).
		}
	}
	if err := sc.Err(); err != nil {
		return "", st, err
	}

	digest := b.String()
	if maxChars > 0 && len(digest) > maxChars {
		// Keep the task (first user request) + the tail (conclusions).
		head := "USER (task): " + clip(firstUser, 2000) + "\n...\n"
		tail := digest[len(digest)-(maxChars-len(head)):]
		digest = head + tail
	}
	st.DigestChars = len(digest)
	return digest, st, nil
}

func decodeContent(raw json.RawMessage) []tBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []tBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return []tBlock{{Type: "text", Text: s}}
	}
	return nil
}

// toolArg returns a short, useful argument summary for a tool call (the command, the
// file), never the full input. Keeps the digest readable and bounded.
func toolArg(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "query", "pattern", "description", "title"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return k + "=" + clip(strings.TrimSpace(v), 120)
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

const extractSystem = `You extract DURABLE, REUSABLE engineering knowledge from a coding-agent session transcript, to store in a team knowledge base (Mesh) so the NEXT agent inherits it.

Output STRICT JSON only: an array of 0 to 3 objects. No prose, no markdown, no code fences. Each object:
{"type": "decision|gotcha|post-mortem", "title": "...", "do": "...", "dont": "...", "why": "...", "confidence": "low|med|high"}

A note QUALIFIES only if it is ALL of:
- non-obvious (a competent engineer would not already assume it),
- reusable beyond this one task (a pattern, pitfall, or decision that recurs),
- durable (still true next month, not a transient state or a one-off fix).

DO NOT emit a note for: routine task completion, restating the obvious, project trivia, generic best practices everyone knows, or speculation. Prefer returning [] over a weak note. Quality over quantity; 0 is a valid and common answer.

Each field is ONE line:
- title: specific, under 12 words (e.g. "pgx NULL vs empty string breaks $1='' filters", not "Database note").
- do: the action to take, imperative.
- dont: the specific failure to avoid.
- why: the reason or the evidence from the session.
- confidence: your honest rating that this is genuinely reusable.

Write plainly: NO em dashes (use a comma, period, or parentheses); down-to-earth expert voice, no buzzwords.`

// Extract asks the model to pull qualifying write-back notes from a digest. Returns an
// empty slice (not an error) when there is nothing worth recording.
func Extract(ctx context.Context, client llm.Client, digest string) ([]Candidate, error) {
	out, err := client.Complete(ctx, extractSystem, "Session transcript digest:\n\n"+digest)
	if err != nil {
		return nil, err
	}
	cands, err := parseCandidates(out)
	if err != nil {
		return nil, err
	}
	return cands, nil
}

// parseCandidates tolerates a model that wraps JSON in fences or stray prose by
// extracting the outermost JSON array, then validates each candidate.
func parseCandidates(out string) ([]Candidate, error) {
	s := strings.TrimSpace(stripFences(out))
	i, j := strings.IndexByte(s, '['), strings.LastIndexByte(s, ']')
	if i < 0 || j < 0 || j < i {
		// A model that correctly found nothing may say so in prose instead of "[]".
		if looksEmpty(s) {
			return nil, nil
		}
		return nil, fmt.Errorf("no JSON array in model output")
	}
	var raw []Candidate
	if err := json.Unmarshal([]byte(s[i:j+1]), &raw); err != nil {
		return nil, fmt.Errorf("parse candidates: %w", err)
	}
	out2 := raw[:0]
	for _, c := range raw {
		c.Type = strings.ToLower(strings.TrimSpace(c.Type))
		c.Title = strings.TrimSpace(c.Title)
		if !validType[c.Type] || c.Title == "" {
			continue // drop malformed/garbage rows rather than fail the whole extraction
		}
		out2 = append(out2, c)
	}
	return out2, nil
}

func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return s
}

func looksEmpty(s string) bool {
	l := strings.ToLower(s)
	return l == "" || l == "[]" || strings.Contains(l, "no durable") || strings.Contains(l, "nothing worth") || strings.Contains(l, "no notes")
}

const judgeSystem = `You are a senior engineer reviewing one candidate note for a team knowledge base. KEEP it only if it is genuinely non-obvious, reusable beyond one task, and durable (still true next month) - i.e. you would be glad the next engineer inherited it. REJECT routine task notes, restated obvious facts, project trivia, generic best practices, and speculation. Be strict; most weak notes should be rejected.

Output STRICT JSON only: {"keep": true|false, "reason": "one short line"}. No prose, no fences.`

// Judge rates whether a candidate is worth keeping, for measuring extraction precision
// in the benchmark. Strict by design (it is the precision gate, not a rubber stamp).
func Judge(ctx context.Context, client llm.Client, c Candidate) (keep bool, reason string, err error) {
	u := fmt.Sprintf("Candidate note:\ntype: %s\ntitle: %s\ndo: %s\ndont: %s\nwhy: %s\nconfidence: %s",
		c.Type, c.Title, c.Do, c.Dont, c.Why, c.Confidence)
	out, err := client.Complete(ctx, judgeSystem, u)
	if err != nil {
		return false, "", err
	}
	s := stripFences(out)
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j < i {
		return false, "", fmt.Errorf("no JSON object in judge output")
	}
	var v struct {
		Keep   bool   `json:"keep"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(s[i:j+1]), &v); err != nil {
		return false, "", err
	}
	return v.Keep, strings.TrimSpace(v.Reason), nil
}
