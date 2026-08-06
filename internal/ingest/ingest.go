// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package ingest pulls knowledge from where a team already keeps it (GitHub, Slack,
// ...) into THEIR vault, on THEIR hardware - the sovereign version of what cloud
// search tools do. Each imported item becomes a markdown note with provenance
// frontmatter (source=import:<connector>, source_url, imported_at, author), written
// to a deterministic path so a re-pull upserts instead of duplicating. The more
// sources flow in, the higher the switching cost - this is the data-gravity moat.
package ingest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
	"gopkg.in/yaml.v3"
)

// Doc is one upstream item to import.
type Doc struct {
	ExternalID string // stable per-source id (drives the deterministic filename)
	Title      string
	Body       string
	Author     string
	SourceURL  string
	CreatedAt  string // YYYY-MM-DD
}

// Connector pulls docs from one external source since a timestamp (zero = all).
// Key is a stable per-instance id (e.g. "github:owner/repo") used to remember the
// last successful pull for incremental sync.
//
// Pull MUST paginate the source to exhaustion for the window. It returns truncated=
// true only when it could not (it hit maxIngestPages with more data still upstream);
// in that case the caller must NOT advance the high-water mark, so the un-pulled tail
// is re-fetched next run instead of being silently skipped forever.
type Connector interface {
	Name() string
	Key() string
	Pull(ctx context.Context, since time.Time) (docs []Doc, truncated bool, err error)
}

// maxIngestPages bounds a single connector pull so a misbehaving cursor cannot loop
// forever. At the per-page sizes connectors use (100-200) this is ~20-40k items per
// incremental run, far above any real delta; hitting it sets truncated=true so the
// mark is held and the rest is pulled next run.
const maxIngestPages = 200

// Result reports what a run wrote.
type Result struct {
	Connector string `json:"connector"`
	Pulled    int    `json:"pulled"`
	Written   int    `json:"written"`
	Folder    string `json:"folder"`
	Truncated bool   `json:"truncated"` // hit the page cap; mark not advanced, more to pull
}

// Run pulls from c and upserts each doc as a provenance-stamped note under
// imported/<connector>/ in vaultRoot. Idempotent: a re-pull overwrites the same
// deterministic file, so source_url dedupe is automatic.
func Run(ctx context.Context, vaultRoot string, c Connector, since time.Time) (Result, error) {
	docs, truncated, err := c.Pull(ctx, since)
	if err != nil {
		return Result{}, err
	}
	folder := filepath.Join("imported", c.Name())
	dir := filepath.Join(vaultRoot, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	_ = dir // ensured above; RenderDoc returns a path under it
	written := 0
	onDisk := func(rel string) bool {
		_, serr := os.Stat(filepath.Join(vaultRoot, filepath.FromSlash(rel)))
		return serr == nil
	}
	for _, d := range docs {
		rel, content, err := RenderDocResolved(c.Name(), d, onDisk)
		if err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(filepath.Join(vaultRoot, rel), content, 0o644); err != nil {
			return Result{}, err
		}
		written++
	}
	return Result{Connector: c.Name(), Pulled: len(docs), Written: written, Folder: folder, Truncated: truncated}, nil
}

// RenderDoc renders one imported Doc to its vault-relative path and provenance-
// stamped markdown, WITHOUT touching disk. The hub uses this to commit imports
// through its git repo (a Change) instead of writing files; Run uses it for the
// local CLI path. The path is deterministic per (connector, ExternalID) so a
// re-pull upserts the same note.
func RenderDoc(connectorName string, d Doc) (relPath string, content []byte, err error) {
	return renderDocAs(connectorName, d, importedID(connectorName, vault.Slugify(d.ExternalID)))
}

// RenderDocResolved is RenderDoc with the legacy-path fallback: when the note this
// doc would write to does not exist yet but the note the SAME doc was ingested to
// under the pre-fold slug does, it re-renders that note in place, keeping both its
// path and its frontmatter id.
//
// Why it is needed: the id is derived from vault.Slugify(ExternalID), and Slugify
// learned to fold diacritics to their ASCII base ("arende-1" where it used to drop
// the letter and produce "rende-1"). All-ASCII ids are byte-identical across that
// change, so nothing moves for GitHub or Notion. But ExternalID is upstream or
// operator data, not ours: the Slack connector builds it from the configured
// channel string, and Jira/Linear pass through a remote issue key. Nothing in this
// package constrains any of them to ASCII, so without this fallback the first pull
// after the upgrade would mint a SECOND note for an already-ingested item instead of
// updating it, and every later pull would keep both alive.
//
// Re-rendering under the legacy id rather than moving the note to the new one is
// deliberate: the frontmatter id is the graph identity (nodes are note:<id>), so
// keeping it preserves every edge and every agent citation that already points at
// the imported note. New items still get the new, guessable slug.
//
// exists reports whether a vault-relative slash path is already present. Run passes
// an os.Stat over the vault; a caller that writes through a repo instead of the
// filesystem (the hub commits imports as a Change) passes its own lookup.
func RenderDocResolved(connectorName string, d Doc, exists func(relPath string) bool) (relPath string, content []byte, err error) {
	id := importedID(connectorName, vault.Slugify(d.ExternalID))
	if exists == nil {
		return renderDocAs(connectorName, d, id)
	}
	legacySlug := legacySlugify(d.ExternalID)
	legacyID := importedID(connectorName, legacySlug)
	// An empty legacy slug is not a match candidate: every doc whose ExternalID was
	// entirely non-ASCII collapsed onto the same "<connector>-.md" bucket back then,
	// so adopting it would make unrelated docs overwrite each other forever.
	if legacySlug == "" || legacyID == id {
		return renderDocAs(connectorName, d, id)
	}
	if !exists(importedPath(connectorName, id)) && exists(importedPath(connectorName, legacyID)) {
		return renderDocAs(connectorName, d, legacyID)
	}
	return renderDocAs(connectorName, d, id)
}

func importedID(connectorName, slug string) string { return connectorName + "-" + slug }

func importedPath(connectorName, id string) string {
	return filepath.ToSlash(filepath.Join("imported", connectorName, id+".md"))
}

// legacySlugify reproduces the ASCII-only slug Mesh minted before vault.Slugify
// folded diacritics: anything outside [a-z0-9] became a separator, so non-ASCII
// letters were dropped. It is a FROZEN copy of a historical on-disk format used only
// to recognise notes already written, so it must not be refactored to call Slugify:
// any future change there would silently change which old files we match.
func legacySlugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func renderDocAs(connectorName string, d Doc, id string) (relPath string, content []byte, err error) {
	now := time.Now().Format("2006-01-02")
	fm := &vault.Frontmatter{
		ID:         id,
		Type:       vault.TypeNote,
		Title:      d.Title,
		When:       firstNonEmpty(d.CreatedAt, now),
		Created:    firstNonEmpty(d.CreatedAt, now),
		Tags:       vault.StringList{"imported", connectorName},
		Author:     d.Author,
		Source:     "import:" + connectorName,
		SourceURL:  d.SourceURL,
		ImportedAt: now,
	}
	s, err := renderImported(fm, d.Body)
	if err != nil {
		return "", nil, err
	}
	return importedPath(connectorName, id), []byte(s), nil
}

// Opts controls an incremental run.
type Opts struct {
	Full  bool      // ignore stored high-water mark; pull everything
	Since time.Time // explicit override (wins over stored state when non-zero)
}

// RunIncremental pulls only what changed since the connector's last successful run
// (a high-water mark persisted in <vault>/.mesh/ingest-state.json), then advances
// the mark. --full or an explicit Since override the stored mark. The mark is
// stamped from BEFORE the pull, so anything that lands mid-pull is caught next time.
func RunIncremental(ctx context.Context, vaultRoot string, c Connector, opts Opts) (Result, error) {
	st, _ := loadState(vaultRoot)
	since := opts.Since
	if since.IsZero() && !opts.Full {
		if ts := st.LastRun[c.Key()]; ts > 0 {
			since = time.Unix(ts, 0)
		}
	}
	startedAt := time.Now()
	res, err := Run(ctx, vaultRoot, c, since)
	if err != nil {
		return res, err
	}
	// Only advance the high-water mark when the whole window was pulled. If the pull
	// was truncated (hit the page cap), holding the mark means the un-pulled tail is
	// re-fetched next run (upserts are idempotent) instead of being skipped forever
	// because the mark jumped past it.
	if !res.Truncated {
		st.LastRun[c.Key()] = startedAt.Unix()
		if serr := saveState(vaultRoot, st); serr != nil {
			return res, fmt.Errorf("ingest: pull succeeded but persisting the high-water mark failed (next run will re-pull): %w", serr)
		}
	}
	return res, nil
}

func renderImported(fm *vault.Frontmatter, body string) (string, error) {
	y, err := yaml.Marshal(fm)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(y)
	b.WriteString("---\n\n# ")
	b.WriteString(fm.Title)
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	if fm.SourceURL != "" {
		b.WriteString("\n[source](")
		b.WriteString(fm.SourceURL)
		b.WriteString(")\n")
	}
	return b.String(), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// maxResponseBytes caps a single connector HTTP response so a hostile or
// misconfigured endpoint cannot OOM the process (the client timeouts bound time,
// not bytes). 32 MiB comfortably holds a full page of issues/messages.
const maxResponseBytes = 32 << 20

// readBody reads an HTTP response body with a hard size cap, closing it.
func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

// httpError is a small helper for connectors to format non-2xx responses.
func httpError(source string, status int, body []byte) error {
	snippet := string(body)
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	return fmt.Errorf("%s: http %d: %s", source, status, snippet)
}
