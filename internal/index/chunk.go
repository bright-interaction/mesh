package index

import "strings"

// maxChunkChars caps a single chunk so a runaway section never blows past a
// small embedding model's context window (nomic-embed-text tops out ~8k tokens;
// ~6k chars stays comfortably inside while keeping sections whole in practice).
const maxChunkChars = 6000

// ChunkText splits a note into retrieval units. Chunk 0 is the header (title +
// the flywheel fields do/dont/why + tags, the institutional memory that lives
// in frontmatter, not the body); the rest are one chunk per heading section,
// each carrying the title as context so an isolated chunk is still
// self-describing. The default embed path joins these into one whole-note
// vector; `mesh embed --per-section` stores them separately and scores a note
// by its best-matching section (max-pool). Per-section showed no recall or
// answer@1 lift on the Hive corpus at ~18x the cost, so whole-note is default
// (see the dogfood decision note), but the structured join is itself a better
// text representation than the collapsed search body.
func ChunkText(pn *ParsedNote) []string {
	title := titleOf(pn)

	header := title
	for _, v := range []string{pn.FM.Do, pn.FM.Dont, pn.FM.Why} {
		if v != "" {
			header += "\n" + v
		}
	}
	if len(pn.FM.Tags) > 0 {
		header += "\n" + strings.Join(pn.FM.Tags, " ")
	}
	chunks := []string{truncate(collapse(header))}

	body := htmlCommentRe.ReplaceAllString(pn.Body, " ")
	var cur []string
	flush := func() {
		seg := strings.TrimSpace(strings.Join(cur, "\n"))
		cur = cur[:0]
		if seg == "" {
			return
		}
		chunks = append(chunks, truncate(title+"\n"+seg))
	}
	for _, line := range strings.Split(body, "\n") {
		if _, ok := parseHeading(stripInlineCode(line)); ok && len(cur) > 0 {
			flush()
		}
		cur = append(cur, line)
	}
	flush()
	return chunks
}

// collapse squeezes runs of whitespace to single spaces (header chunk only; body
// sections keep their newlines so headings stay legible to the embedder).
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string) string {
	if len(s) <= maxChunkChars {
		return s
	}
	return s[:maxChunkChars]
}
