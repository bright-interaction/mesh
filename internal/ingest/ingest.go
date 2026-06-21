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
type Connector interface {
	Name() string
	Key() string
	Pull(ctx context.Context, since time.Time) ([]Doc, error)
}

// Result reports what a run wrote.
type Result struct {
	Connector string `json:"connector"`
	Pulled    int    `json:"pulled"`
	Written   int    `json:"written"`
	Folder    string `json:"folder"`
}

// Run pulls from c and upserts each doc as a provenance-stamped note under
// imported/<connector>/ in vaultRoot. Idempotent: a re-pull overwrites the same
// deterministic file, so source_url dedupe is automatic.
func Run(ctx context.Context, vaultRoot string, c Connector, since time.Time) (Result, error) {
	docs, err := c.Pull(ctx, since)
	if err != nil {
		return Result{}, err
	}
	folder := filepath.Join("imported", c.Name())
	dir := filepath.Join(vaultRoot, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	now := time.Now().Format("2006-01-02")
	written := 0
	for _, d := range docs {
		id := c.Name() + "-" + vault.Slugify(d.ExternalID)
		fm := &vault.Frontmatter{
			ID:         id,
			Type:       vault.TypeNote,
			Title:      d.Title,
			When:       firstNonEmpty(d.CreatedAt, now),
			Created:    firstNonEmpty(d.CreatedAt, now),
			Tags:       vault.StringList{"imported", c.Name()},
			Author:     d.Author,
			Source:     "import:" + c.Name(),
			SourceURL:  d.SourceURL,
			ImportedAt: now,
		}
		content, err := renderImported(fm, d.Body)
		if err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(content), 0o644); err != nil {
			return Result{}, err
		}
		written++
	}
	return Result{Connector: c.Name(), Pulled: len(docs), Written: written, Folder: folder}, nil
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
	st.LastRun[c.Key()] = startedAt.Unix()
	_ = saveState(vaultRoot, st)
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

// httpError is a small helper for connectors to format non-2xx responses.
func httpError(source string, status int, body []byte) error {
	snippet := string(body)
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	return fmt.Errorf("%s: http %d: %s", source, status, snippet)
}
