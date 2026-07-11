// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

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
	"sort"
	"strings"
	"sync"
	"unicode"

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

// LowConfidence reports whether the model self-rated this candidate as low confidence.
// The extractor's own "this is probably weak" signal is a cheap precision pre-filter
// before the (costlier) judge pass: a low-confidence candidate is dropped outright.
// Empty/med/high all pass (absence is not a low rating).
func LowConfidence(c Candidate) bool {
	return strings.EqualFold(strings.TrimSpace(c.Confidence), "low")
}

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

The most common mistake is recording a SINGLE INCIDENT as if it were a rule ("found a stale prod container once"). Only emit a note that states a recurring, transferable rule with a concrete mechanism (e.g. "Mollie does not HMAC webhooks, so re-fetch the payment by id"), not a story about this one session. A duplicate-detector and a human reviewer sit after you, so lean toward surfacing a genuinely useful pattern rather than withholding it.

Calibrate against these examples.
KEEP (durable rule with a mechanism, reusable next month):
  {"type":"gotcha","title":"pgx NULL vs empty string breaks $1='' filters","do":"filter with IS NULL or a sentinel, not $1=''","dont":"assume an unset text column equals ''","why":"pgtype.Text{} is NULL, so $1='' never matches it","confidence":"high"}
  {"type":"decision","title":"Mollie webhooks are unsigned, re-fetch the payment by id","do":"on webhook, GET /v2/payments/{id} and trust that, not the body","dont":"act on the webhook payload directly","why":"the callback has no HMAC, so a forged body could self-upgrade an account","confidence":"high"}
REJECT (return [] for these, do NOT emit them):
  - "Fixed the login redirect bug" (a one-off task outcome, no reusable rule)
  - "Use environment variables for secrets" (generic best practice everyone already knows)
  - "The prod container was stale, so I restarted it" (a single incident, not a rule with a mechanism)
  - "Refactored the handler into smaller functions" (routine work, nothing transferable)

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

// dedupeStop are low-signal title words excluded from the similarity token set so two
// titles match on their substantive terms, not their filler.
var dedupeStop = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "must": true, "not": true,
	"are": true, "was": true, "via": true, "use": true, "when": true, "into": true,
	"that": true, "this": true, "from": true, "your": true, "you": true, "all": true,
	"per": true, "but": true, "its": true, "needs": true, "need": true,
}

func titleTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if len(f) >= 3 && !dedupeStop[f] {
			out[f] = true
		}
	}
	return out
}

// TitleSimilarity is the overlap coefficient of two titles' substantive tokens (0..1):
// intersection over the SMALLER token set. Overlap (not Jaccard) is the right measure
// for "does this candidate restate an existing note", because an existing note's title
// is often longer/compound (extra clauses) which would unfairly sink a Jaccard score.
func TitleSimilarity(a, b string) float64 {
	ta, tb := titleTokens(a), titleTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	small := len(ta)
	if len(tb) < small {
		small = len(tb)
	}
	return float64(inter) / float64(small)
}

// DuplicateThreshold is the title-similarity at or above which a candidate is treated
// as already-known (tuned so near-restatements match but distinct notes do not).
const DuplicateThreshold = 0.5

// Occurrence is one extracted candidate tagged with the session it came from, the
// input to recurring-problem detection across many sessions.
type Occurrence struct {
	Cand    Candidate
	Session string
}

// Cluster is a group of similar candidates that recur across sessions: a candidate
// learning that shows up again and again is a SYSTEMIC issue worth a permanent fix,
// not a one-off write-back.
type Cluster struct {
	Rep      Candidate   `json:"rep"`      // the representative (first-seen) candidate
	Sessions []string    `json:"sessions"` // distinct sessions it appeared in
	Count    int         `json:"count"`    // distinct session count
	Members  []Candidate `json:"members"`  // all candidates in the cluster
}

// ClusterRecurring greedily groups occurrences by title similarity (>= threshold) and
// returns the clusters sorted by distinct-session count, descending. A cluster spanning
// multiple sessions is a recurring problem.
func ClusterRecurring(occs []Occurrence, threshold float64) []Cluster {
	if threshold <= 0 {
		threshold = DuplicateThreshold
	}
	var clusters []Cluster
	for _, o := range occs {
		placed := false
		for i := range clusters {
			if TitleSimilarity(clusters[i].Rep.Title, o.Cand.Title) >= threshold {
				clusters[i].Members = append(clusters[i].Members, o.Cand)
				if !contains(clusters[i].Sessions, o.Session) {
					clusters[i].Sessions = append(clusters[i].Sessions, o.Session)
				}
				placed = true
				break
			}
		}
		if !placed {
			clusters = append(clusters, Cluster{Rep: o.Cand, Members: []Candidate{o.Cand}, Sessions: []string{o.Session}})
		}
	}
	for i := range clusters {
		clusters[i].Count = len(clusters[i].Sessions)
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Count > clusters[j].Count })
	return clusters
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

const judgeSystem = `You are a senior engineer reviewing one candidate note for a team knowledge base. KEEP it only if it is genuinely non-obvious, reusable beyond one task, and durable (still true next month) - i.e. you would be glad the next engineer inherited it. REJECT routine task notes, restated obvious facts, project trivia, generic best practices, and speculation. Be strict; most weak notes should be rejected.

Output STRICT JSON only: {"keep": true|false, "reason": "one short line"}. No prose, no fences.`

// The panel lenses each stress ONE of the three qualifying criteria the extractor prompt
// names. A single generalist judge is lenient (it rubber-stamps almost everything, so a
// self-grade reports ~100%); three judges each focused on a different failure mode
// disagree usefully, which is what makes the precision number honest and the gate strong.
const (
	judgeLensNonObvious = judgeSystem + "\nFor THIS decision weigh ONLY non-obviousness: keep only if it states something a competent engineer would NOT already assume; reject the obvious."
	judgeLensReusable   = judgeSystem + "\nFor THIS decision weigh ONLY reusability: keep only if it transfers beyond this one task or session; reject one-offs and single-incident stories."
	judgeLensDurable    = judgeSystem + "\nFor THIS decision weigh ONLY durability: keep only if it is still true next month; reject transient state and one-time fixes."
)

var judgeLenses = []struct{ Name, System string }{
	{"non-obvious", judgeLensNonObvious},
	{"reusable", judgeLensReusable},
	{"durable", judgeLensDurable},
}

// PanelMajority keeps a candidate when at least 2 of the 3 lenses approve (robust to one
// lens being wrong); PanelUnanimous requires all three (the strictest, honest bar).
const (
	PanelMajority  = 2
	PanelUnanimous = 0
)

// Vote is one lens's verdict on a candidate.
type Vote struct {
	Lens   string `json:"lens"`
	Keep   bool   `json:"keep"`
	Reason string `json:"reason"`
	Err    string `json:"err,omitempty"`
}

// PanelVerdict aggregates the lens votes for a candidate.
type PanelVerdict struct {
	Keep  bool   `json:"keep"`
	KeepN int    `json:"keep_n"`
	Total int    `json:"total"`
	Votes []Vote `json:"votes"`
}

// judgeOnce runs one judge with a given system prompt over a candidate.
func judgeOnce(ctx context.Context, client llm.Client, system string, c Candidate) (keep bool, reason string, err error) {
	u := fmt.Sprintf("Candidate note:\ntype: %s\ntitle: %s\ndo: %s\ndont: %s\nwhy: %s\nconfidence: %s",
		c.Type, c.Title, c.Do, c.Dont, c.Why, c.Confidence)
	out, err := client.Complete(ctx, system, u)
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

// Judge rates a candidate with the single generalist rubric. Retained for callers/tests
// that want one vote; JudgePanel is the stronger, default precision gate.
func Judge(ctx context.Context, client llm.Client, c Candidate) (keep bool, reason string, err error) {
	return judgeOnce(ctx, client, judgeSystem, c)
}

// JudgePanel runs each qualifying-criterion lens as an INDEPENDENT judge call and keeps
// the candidate when at least keepThreshold lenses vote keep (keepThreshold<=0 means
// unanimity, the strict default). judges are cycled across lenses: pass one client for a
// cheap prompt-diverse panel, or N clients for true model diversity. A lens that ERRORS
// counts as a keep vote (fail-open: never silently drop knowledge on an LLM hiccup, the
// human still vetoes) with its error recorded; only when EVERY lens errors is it an error.
// Lenses run concurrently, so the panel costs about one judge's latency, not three.
func JudgePanel(ctx context.Context, judges []llm.Client, c Candidate, keepThreshold int) (PanelVerdict, error) {
	if len(judges) == 0 {
		return PanelVerdict{}, fmt.Errorf("no judges")
	}
	votes := make([]Vote, len(judgeLenses))
	var wg sync.WaitGroup
	for i, lens := range judgeLenses {
		wg.Add(1)
		go func(i int, name, system string) {
			defer wg.Done()
			keep, reason, err := judgeOnce(ctx, judges[i%len(judges)], system, c)
			v := Vote{Lens: name, Keep: keep, Reason: reason}
			if err != nil {
				v.Keep = true // fail-open: a flaky judge must not drop a candidate
				v.Err = err.Error()
			}
			votes[i] = v
		}(i, lens.Name, lens.System)
	}
	wg.Wait()

	keepN, errN := 0, 0
	for _, v := range votes {
		if v.Keep {
			keepN++
		}
		if v.Err != "" {
			errN++
		}
	}
	if errN == len(votes) {
		return PanelVerdict{Votes: votes, Total: len(votes)}, fmt.Errorf("all judge lenses failed: %s", votes[0].Err)
	}
	threshold := keepThreshold
	if threshold <= 0 {
		threshold = len(judgeLenses)
	}
	return PanelVerdict{Keep: keepN >= threshold, KeepN: keepN, Total: len(votes), Votes: votes}, nil
}
