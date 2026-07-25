// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"path"
	"strings"
)

// This file is the INSTRUCTION BOUNDARY for the LLM sink.
//
// Connector-ingested notes (internal/ingest) carry text written by whoever could post
// to the upstream system: a GitHub issue comment, a Slack message, a Notion page. The
// HTML sink (internal/web) and the TTY sink (internal/textdiff) both already treat
// those bytes as untrusted for their own renderer. The agent is a sink too, and its
// injection is "text that reads like an instruction", so the same bytes need a marker
// here: an explicit envelope that says the enclosed span is DATA, plus the provenance
// that says where it came from. The contract (contract.go) tells the agent what the
// envelope means; this file decides what goes inside it.
//
// It is deliberately cheap because it runs on the hot path of every search: a path
// prefix test per card and, for the notes that ARE imported, one wrapper around the
// snippet. A note that is not imported is untouched and costs zero extra tokens.

const (
	// untrustedOpenPrefix / untrustedClose delimit third-party content. The tag name
	// is spelled out (not a bare fence) so it survives truncation and is unambiguous
	// to the model even when a snippet lands mid-context.
	untrustedOpenPrefix = "<untrusted-external-content"
	untrustedClose      = "</untrusted-external-content>"

	// importedPathPrefix is where connector ingest writes third-party notes:
	// ingest.RenderDoc always renders to imported/<connector>/<id>.md, and it is the
	// only writer of that tree. It is the provenance signal available on a search
	// card, which carries a Path but no source field.
	importedPathPrefix = "imported/"

	// importSourcePrefix is what ingest.RenderDoc stamps into the note frontmatter
	// (source: import:<connector>). The fetch path reads the file, so it can use the
	// authoritative frontmatter instead of inferring from the path.
	importSourcePrefix = "import:"
)

// importedSource returns the provenance source for a vault-relative note path
// ("import:<connector>") and whether that path is third-party ingested content.
// Fail-safe by construction: an unrecognised path is simply not labelled, and a
// hand-written note that happens to live under imported/ is labelled untrusted,
// which is the harmless direction.
func importedSource(notePath string) (string, bool) {
	p := path.Clean(strings.ReplaceAll(strings.TrimSpace(notePath), "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	if !strings.HasPrefix(p, importedPathPrefix) {
		return "", false
	}
	connector, rest, found := strings.Cut(strings.TrimPrefix(p, importedPathPrefix), "/")
	if !found || connector == "" || rest == "" {
		return "", false
	}
	return importSourcePrefix + connector, true
}

// wrapUntrusted puts text inside the data envelope, tagged with its provenance. url
// is optional (it is the upstream permalink when the frontmatter carried one).
func wrapUntrusted(source, url, text string) string {
	var b strings.Builder
	b.Grow(len(text) + 128)
	b.WriteString(untrustedOpenPrefix)
	b.WriteString(` source="`)
	b.WriteString(sanitizeAttr(source))
	b.WriteString(`"`)
	if u := strings.TrimSpace(url); u != "" {
		b.WriteString(` url="`)
		b.WriteString(sanitizeAttr(u))
		b.WriteString(`"`)
	}
	b.WriteString(">\n")
	// Strip any envelope tag the ingested text itself contains, otherwise a hostile
	// note can close the envelope early and continue OUTSIDE it, which is exactly the
	// boundary escape this whole file exists to prevent.
	b.WriteString(stripEnvelopeTags(text))
	b.WriteString("\n")
	b.WriteString(untrustedClose)
	return b.String()
}

// sanitizeAttr keeps an attribute value on one line and free of the quote that would
// end it, so provenance can never break out of the opening tag.
func sanitizeAttr(v string) string {
	repl := strings.NewReplacer(`"`, "'", "<", "(", ">", ")", "\n", " ", "\r", " ")
	return strings.TrimSpace(repl.Replace(v))
}

// stripEnvelopeTags neutralises any literal envelope tag inside the payload by
// breaking the angle bracket. It is a substring replace rather than a parse: the
// payload is markdown, and the only thing that must not survive verbatim is a
// sequence the agent would read as the boundary marker.
func stripEnvelopeTags(text string) string {
	if !strings.Contains(text, untrustedOpenPrefix) && !strings.Contains(text, untrustedClose) {
		return text
	}
	repl := strings.NewReplacer(
		untrustedClose, "(/untrusted-external-content)",
		untrustedOpenPrefix, "(untrusted-external-content",
	)
	return repl.Replace(text)
}

// frontmatterProvenance pulls source + source_url out of a note's leading YAML
// frontmatter without a full parse. The fetch path already holds the bytes and needs
// exactly two scalar keys, so a scan of the frontmatter block is cheaper (and cannot
// fail) compared to unmarshalling the whole document.
func frontmatterProvenance(body string) (source, sourceURL string) {
	rest, ok := strings.CutPrefix(body, "---\n")
	if !ok {
		return "", ""
	}
	block, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", ""
	}
	for _, ln := range strings.Split(block, "\n") {
		key, val, found := strings.Cut(ln, ":")
		if !found {
			continue
		}
		v := strings.Trim(strings.TrimSpace(val), `"'`)
		switch strings.TrimSpace(key) {
		case "source":
			source = v
		case "source_url":
			sourceURL = v
		}
	}
	return source, sourceURL
}
