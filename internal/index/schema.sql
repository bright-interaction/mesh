-- Mesh SQLite index schema (spec section 3.2). Pure-Go modernc.org/sqlite, WAL.
-- The index is a derived, deletable artifact; the markdown vault is the source
-- of truth. No vec0 virtual table: modernc cannot load C extensions, so vectors
-- (milestone V) are a flat []float32 blob scored with brute-force cosine.

CREATE TABLE IF NOT EXISTS notes (
  id             TEXT PRIMARY KEY,           -- frontmatter id (stable identity)
  path           TEXT UNIQUE NOT NULL,       -- vault-relative path (lookup, not identity)
  type           TEXT NOT NULL,              -- note|post-mortem|decision|gotcha|entity
  title          TEXT NOT NULL,
  retrieval_hash TEXT NOT NULL,              -- SHA256(body + retrieval-critical frontmatter)
  frontmatter    TEXT NOT NULL,              -- whitelisted-key JSON; never raw YAML
  summary_short  TEXT,                       -- BYOAI one-liner (LOCAL/advisory)
  summary_card   TEXT,                       -- BYOAI ~5-line (LOCAL/advisory)
  mtime          INTEGER NOT NULL,
  updated        TEXT,                        -- frontmatter when/updated
  contributor    TEXT,                        -- last author (LOCAL in v1)
  local_version  INTEGER NOT NULL DEFAULT 0,  -- monotonic local counter (mesh_changed_since)
  hub_version    INTEGER NOT NULL DEFAULT 0   -- DEFERRED: set only when sync (milestone S) is live
);

CREATE TABLE IF NOT EXISTS nodes (
  id         TEXT PRIMARY KEY,               -- note:<frontmatter-id> | tag:<name> | note:<id>#<anchor>
  kind       TEXT NOT NULL,                  -- note|heading|block|tag|external|rationale|decision|gotcha
  label      TEXT NOT NULL,
  note_id    TEXT,                           -- owning note's frontmatter id
  note_path  TEXT,                           -- denormalized for fast file open
  anchor     TEXT,                           -- heading slug / block id
  source_loc TEXT,                           -- "L<line>" for the editor deep link
  community  INTEGER,
  attrs      TEXT                            -- JSON bag (do/dont/why/when, captured_at, author)
);
CREATE INDEX IF NOT EXISTS idx_nodes_kind ON nodes(kind);
CREATE INDEX IF NOT EXISTS idx_nodes_community ON nodes(community);
CREATE INDEX IF NOT EXISTS idx_nodes_note_id ON nodes(note_id);

CREATE TABLE IF NOT EXISTS edges (
  source           TEXT NOT NULL,            -- node id
  target           TEXT NOT NULL,
  relation         TEXT NOT NULL,            -- contains|references|tagged|ordered_after|
                                             -- rationale_for|decision_for|supersedes
  confidence       TEXT NOT NULL,            -- EXTRACTED|INFERRED|AMBIGUOUS
  confidence_score REAL NOT NULL DEFAULT 1.0,
  weight           REAL NOT NULL DEFAULT 1.0,
  source_loc       TEXT,
  PRIMARY KEY (source, target, relation)
);
CREATE INDEX IF NOT EXISTS idx_edges_target ON edges(target);

-- Vectors: milestone V. Flat blob, brute-force cosine, homogeneous per vault.
CREATE TABLE IF NOT EXISTS vectors (
  node_id      TEXT NOT NULL,
  chunk_ix     INTEGER NOT NULL,
  model        TEXT NOT NULL,                -- canonical embedding model id (homogeneity guard)
  dim          INTEGER NOT NULL,
  embedding    BLOB NOT NULL,                -- []float32 little-endian
  content_hash TEXT NOT NULL DEFAULT '',     -- sha256 of the embedding input, so an unchanged chunk is not re-embedded
  note_hash    TEXT NOT NULL DEFAULT '',     -- the note's retrieval_hash when embedded; a mismatch with notes.retrieval_hash means this vector is stale and is excluded from retrieval
  PRIMARY KEY (node_id, chunk_ix)
);

CREATE VIRTUAL TABLE IF NOT EXISTS search_index USING fts5(
  node_id UNINDEXED, kind UNINDEXED, anchor UNINDEXED,
  title, body,
  tokenize = 'unicode61 remove_diacritics 2'
);

-- Persisted corpus stats so graph-BM25 does not recompute IDF/avgdl per query.
CREATE TABLE IF NOT EXISTS corpus_stats (key TEXT PRIMARY KEY, value REAL NOT NULL);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
