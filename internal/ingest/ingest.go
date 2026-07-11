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
	for _, d := range docs {
		rel, content, err := RenderDoc(c.Name(), d)
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
	now := time.Now().Format("2006-01-02")
	id := connectorName + "-" + vault.Slugify(d.ExternalID)
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
	return filepath.ToSlash(filepath.Join("imported", connectorName, id+".md")), []byte(s), nil
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
