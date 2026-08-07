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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

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
	// Resolve through a Batch, not doc-by-doc: two upstream ids that transliterate onto
	// one slug would otherwise both land on one path and the pull would report both as
	// written while only the last one survived.
	OrderForResolution(docs)
	batch := NewBatch(c.Name(), onDisk)
	for _, d := range docs {
		rel, content, err := batch.Render(d)
		if err != nil {
			return Result{}, err
		}
		// Land the note through the vault's durable writer (temp file, fsync, rename,
		// directory fsync) rather than os.WriteFile, which truncates the existing note
		// first and fsyncs nothing. A re-pull upserts over a note that is already in the
		// index and already synced to the team, so the truncate window is a window where
		// the note reads as empty, and an un-fsynced import is content the hub can be
		// told about before it is on the device.
		if err := vault.WriteNoteAtomic(filepath.Join(vaultRoot, rel), content); err != nil {
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
//
// This resolves ONE doc and therefore cannot see two docs of the same pull colliding
// on one path. A caller that renders a whole pull must go through Batch, which does.
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

// Batch resolves a run of docs from ONE connector into note paths, keeping two
// documents that slug onto the SAME note distinct.
//
// The collision is a direct consequence of folding diacritics: two distinct upstream
// ids can now transliterate to one slug, where the old letter-dropping rule kept them
// apart ("cafe-1" and the e-acute spelling of it both slug to "cafe-1" now; before,
// the second one slugged to "caf-1"). The two spellings of a Swedish word, an a-ring
// and an a-diaeresis, do the same.
//
// Nothing downstream notices on its own. The CLI writes both notes to one path, and
// the hub keys its Change.Upserts by path in a MAP, so the second document silently
// REPLACES the first, one upstream item is missing from the vault, and the run reports
// both as written.
//
// RenderDocResolved cannot see this: it is handed one doc at a time plus an existence
// predicate that cannot tell "the note I wrote a moment ago" from "a different
// document's note". Batch carries the one piece of state that makes the collision
// visible, which ExternalID has claimed which path, and moves the loser to a
// deterministic fingerprinted path instead of dropping it.
//
// Which of the two keeps the plain name is decided ONCE, by the first batch that sees
// them together, and is stable from then on regardless of the order the connector
// returns docs in: Render pins a doc that already sits at its fingerprinted path
// before it looks at the contested one.
type Batch struct {
	connector string
	exists    func(relPath string) bool
	// claimed maps a vault-relative note path to the ExternalID that owns it in THIS
	// batch. The same ExternalID twice is one document pulled twice, so it keeps its
	// path (the import is a deterministic upsert); a different one is the collision.
	claimed map[string]string
}

// NewBatch starts a resolution batch for one connector. exists reports whether a
// vault-relative slash path is already present, exactly as for RenderDocResolved (nil
// means "assume nothing is there", which disables the legacy-path fallback too).
func NewBatch(connectorName string, exists func(relPath string) bool) *Batch {
	return &Batch{connector: connectorName, exists: exists, claimed: map[string]string{}}
}

// Render resolves one doc within the batch and returns its vault-relative path and
// provenance-stamped markdown, WITHOUT touching disk.
func (b *Batch) Render(d Doc) (relPath string, content []byte, err error) {
	// Pin a doc a previous run already moved off a contested slug BEFORE anything looks
	// at the plain path. Without this the winner is whichever of the two the connector
	// happened to return first, so a flipped order on the next pull would swap their
	// files and leave a third, stale copy behind.
	altID := disambiguatedID(b.connector, d.ExternalID)
	if b.exists != nil && b.exists(importedPath(b.connector, altID)) {
		return b.claim(altID, d)
	}
	rel, content, err := RenderDocResolved(b.connector, d, b.exists)
	if err != nil {
		return "", nil, err
	}
	if owner, taken := b.claimed[rel]; !taken || owner == d.ExternalID {
		b.claimed[rel] = d.ExternalID
		return rel, content, nil
	}
	return b.claim(altID, d)
}

// claim renders d under id and records the claim, refusing rather than overwriting if
// some other ExternalID already holds that path.
func (b *Batch) claim(id string, d Doc) (string, []byte, error) {
	rel, content, err := renderDocAs(b.connector, d, id)
	if err != nil {
		return "", nil, err
	}
	if owner, taken := b.claimed[rel]; taken && owner != d.ExternalID {
		// Only reachable through a SHA-256 prefix collision on two ExternalIDs. Refusing
		// keeps the guarantee ("no import is silently replaced by another") true even
		// there, and the error names the pair so the operator can see what happened.
		return "", nil, fmt.Errorf("ingest: %q and %q both resolve to %s; refusing to overwrite one import with the other", owner, d.ExternalID, rel)
	}
	b.claimed[rel] = d.ExternalID
	return rel, content, nil
}

// OrderForResolution reorders docs IN PLACE so the ids whose slug is a faithful spelling
// of themselves come first. It is a stable sort, so nothing else about the pull order
// moves. Every caller that renders a whole pull through a Batch must apply it first.
//
// It decides which side of a collision keeps the readable path. Without it, an item that
// has been imported for months under an all-ASCII id ("cafe-1") loses its note the day a
// diacritic spelling of the same id ("cafe-1" with an e-acute) shows up EARLIER in the
// same pull: the diacritic doc resolves onto the existing note, claims it, and the ASCII
// doc is the one that gets moved. The ambiguous spelling must always be the one that
// moves, and which spelling is ambiguous does not depend on arrival order.
func OrderForResolution(docs []Doc) {
	sort.SliceStable(docs, func(i, j int) bool {
		return !ambiguousSlug(docs[i].ExternalID) && ambiguousSlug(docs[j].ExternalID)
	})
}

// ambiguousSlug reports whether externalID contains a rune Slugify cannot carry through
// verbatim. Every non-ASCII rune either folds onto an ASCII letter (a-ring -> "a") or is
// dropped, so two different ids can produce one slug; that is exactly the class the
// diacritic fold made collidable.
//
// Two pure-ASCII ids can still collide with each other ("ENG__123!!" and "eng-123" both
// slug to "eng-123"). Batch keeps both of those, it just has no principled way to say
// which one should move, so arrival order decides. Nothing is ever lost either way.
func ambiguousSlug(externalID string) bool {
	for _, r := range externalID {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return false
}

// fingerprintBytes is how much of the ExternalID digest goes into a disambiguating
// suffix: 6 bytes = 12 hex characters = 48 bits, which puts a birthday collision past
// 16 million distinct ids sharing one connector and one slug. Wide enough that the
// refusal in claim stays theoretical, short enough to keep the filename readable.
const fingerprintBytes = 6

// disambiguatedID is the collision-free id for a doc: its normal slug plus a short
// deterministic fingerprint of the RAW ExternalID. It is a function of the ExternalID
// alone, so a doc that lost a collision lands on the same path on every later pull.
//
// It is only ever used for the LOSER of an observed collision, never as the default,
// because the default path is what a person reads in their vault and what every
// already-imported note is sitting at.
func disambiguatedID(connectorName, externalID string) string {
	sum := sha256.Sum256([]byte(externalID))
	fp := hex.EncodeToString(sum[:fingerprintBytes])
	if slug := vault.Slugify(externalID); slug != "" {
		return importedID(connectorName, slug+"-"+fp)
	}
	return importedID(connectorName, fp)
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
