# Mesh

A sovereign, single-binary knowledge base whose primary reader is a coding agent.

You edit plain markdown in your IDE. Your agent (Claude Code, Codex) searches it over MCP, and when it finishes a piece of work it **writes back what it learned**, a decision, a gotcha, a post-mortem, so the next agent inherits it. That write-back loop is the point: the knowledge base documents itself and gets smarter every run. Mesh has no reasoning AI inside it; it is the fast engine (parse, index, graph, retrieve), and the agent is the librarian.

It is one Go binary, no cgo, no external services. Retrieving from Mesh is cheaper than having the agent read whole files: it returns ranked cards (title + the matched snippet + why it surfaced) and packs the best bundle that fits a token budget, so the agent reads one note instead of three.

## Honest scope

- **The core, zero models:** cheap card-based retrieval (FTS + graph-BM25 + tier-0, pure Go, no inference, near-zero CPU) plus the agent write-back flywheel, in a single no-glue binary. `mesh_search` hands the agent ranked cards (title + snippet + why); **the agent reads the cards and picks** the 1-2 notes worth fetching. A capable coding agent is already a stronger relevance judge than any bolt-on reranker, so for the agent consumer the agent *is* the reranker, free. This is the whole product for an agent.
- **Optional BYOAI add-ons (off by default, for cost-sensitive or non-agent consumers):**
  - **Vectors (`mesh embed`)** lift recall on paraphrase queries where keyword search breaks (13/20 -> 19/20 on the private corpus behind `docs/BENCHMARK.md`; read that file's caveats before quoting the number). Worth turning on when queries paraphrase the notes; can point at a cloud endpoint for zero local CPU, or be skipped (FTS keyword recall is already 23/25).
  - **Cross-encoder rerank** lifts top-1 precision for a consumer that *trusts the top result without reading the cards* (`answer@1` 3/20 -> 10/20 on paraphrase). That is not a capable agent (which reads the cards and judges); it is a cheap/small downstream model, a blind "fetch top-1" pipeline, or a multi-tenant cloud deployment where offloading ranking to a local judge saves the tenant's billed model from reading and ranking candidates. Off unless an endpoint is configured. See `docs/BENCHMARK.md` for the matched-arm measurements.
- **Also shipped:** a keyboard TUI (`mesh tui`) and a browser app (`mesh ui`) over the same index, plus the client side of sovereign team sync (`mesh join` / `mesh sync` / `mesh conflicts`). The team-sync **server** those talk to is the commercial product and is not in this repository, see [LICENSING.md](LICENSING.md). All optional; the solo, local core stands alone.

## Install

Mesh is a self-contained Go module (`github.com/bright-interaction/mesh`, no cgo,
no external services). One command:

```
go install github.com/bright-interaction/mesh/cmd/mesh@latest
```

That puts the binary in `$(go env GOPATH)/bin`, so make sure that directory is on
your `PATH`.

From source instead (Go 1.26 or newer):

```
git clone https://github.com/bright-interaction/mesh
cd mesh
make install            # builds a static binary to ~/.local/bin/mesh
```

`make install` writes to `~/.local/bin/mesh`; override with `make install
BIN=/usr/local/bin/mesh`. `make build` drops it in `./bin/mesh` instead if you
would rather not install anything.

## Quickstart

This repository ships a small sample vault in `vault/`: the real decisions and
gotchas written while building Mesh. It is the fastest way to see what the tool
returns before you have written any notes of your own. From a clone:

```
mesh index ./vault                                   # parse + build the index
mesh search "rerank" --vault ./vault --budget 4000   # ranked cards, not whole files
```

Six ranked cards come back (abridged here, paths shortened):

```
1. Blending fused score into rerank does not beat pure rerank [tier-0]  (decisions/blending-fused-score-...md)
   # Blending fused score into [rerank] does not beat pure [rerank] ## Context ## Decision ...
   ~ fts
2. Cross-encoder rerank is the answer@1 lever that chunking was not [tier-0]  (decisions/cross-encoder-rerank-...md)
   # Cross-encoder [rerank] is the answer@1 lever that chunking was not ...
   ~ fts
3. Learned fusion weights help the no-reranker path but wash out under rerank [tier-0]  (decisions/learned-fusion-weights-...md)
   ... It matters most for a vectors-on, [rerank]-off deployment, where vector ...
   ~ fts
4. Per-section embeddings do not beat whole-note ... [tier-0]  (decisions/per-section-embeddings-...md)
   ~ linked from Blending fused score into rerank does not beat pure rerank
5. ...
6. ...
packed 6 cards, ~854 tokens (budget 4000)
```

Cards 1 to 3 matched the text. Card 4 did not: it surfaced because the graph
links it to card 1. That one-hop expansion is the part plain full-text search
cannot do. The trailing `~ fts` / `~ linked from` line is the "why it surfaced"
reason, which is what an agent reads before deciding which single note to open.
Six cards for ~854 tokens, instead of six note bodies.

Now your own vault:

```
mesh init my-vault                 # bootstrap a vault (starter index + first build)
mesh new decision "Use Postgres over Mongo" \
  --do "..." --dont "..." --why "..." --vault my-vault   # capture judgment; Mesh fills id/date/placement
mesh index my-vault                # rebuild the index after edits
mesh search "Postgres" --vault my-vault --budget 4000
mesh watch my-vault                # live-reindex as you edit (no manual index; Ctrl-C to stop)
mesh doctor my-vault               # is the index fresh? any drift or lint problems?
```

Search matches the words in your notes, so query with terms the note actually
uses. Semantic (paraphrase) matching is the optional BYOAI vector stage below.

`mesh watch` is the local-first, Obsidian-like immediacy: edit a note in your
editor and it is searchable at once, no commit, no manual `mesh index`. It
reconciles at startup, on every change (debounced), and on a periodic safety
tick that always converges, so a missed file event never leaves the index stale.

Already have a Foam / Obsidian-style vault? Bring it up to the Mesh schema in one idempotent pass:

```
mesh migrate my-vault              # synthesize ids, updated->when, lift ## Related into related:
mesh index my-vault
```

## Optional: semantic search + rerank (BYOAI, sovereign)

The core above needs no models. These two stages are **optional** and **off by
default**; turn them on only for the cases in "Honest scope" (paraphrase recall,
or a cost-sensitive / non-agent consumer). Mesh runs no inference itself, both
call HTTP endpoints **you** control, so they stay on your infrastructure:

```
# 1. Vectors: embed notes via any OpenAI-compatible /embeddings endpoint (Ollama, etc.)
export MESH_EMBED_ENDPOINT=http://localhost:11434/v1
export MESH_EMBED_MODEL=nomic-embed-text
export MESH_EMBED_DOC_PREFIX="search_document: "   # nomic-style asymmetric models
export MESH_EMBED_QUERY_PREFIX="search_query: "
mesh embed my-vault                                 # one vector per note

# 2. Rerank: a cross-encoder sharpens top-1 precision (see tools/rerank-server)
export MESH_RERANK_ENDPOINT=http://127.0.0.1:8787/rerank
export MESH_RERANK_MODEL=Xenova/ms-marco-MiniLM-L-6-v2

mesh status my-vault    # shows which retrieval signals are active
```

Vector search in this repository is a brute-force cosine scan, which stays under
5 ms well past a few thousand notes. The commercial build adds an approximate
(HNSW) index for vaults large enough to need one, see [LICENSING.md](LICENSING.md).

Once set, `mesh search` / `eval` / `mcp` fuse the semantic signal and apply the
rerank automatically. Both degrade safely: no embedder means lexical-only, a
down rerank endpoint falls back to the fused order. Pointing either env var at a
cloud provider sends note content off-box, so keep them local to stay sovereign.
A ready-to-run local cross-encoder server lives in `tools/rerank-server/`.

Got a set of labelled queries for your corpus? `mesh tune cases.json --test
held-out.json` grid-searches the fusion weights to maximize answer@1 and prints
the held-out result plus the `MESH_WEIGHT_FTS/GRAPH/VEC` line to apply the
winner. It tunes the fused ranking, so it helps most when you run vectors
without a reranker (with a reranker on, the cross-encoder owns the top result and
fusion weights wash out). Always pass a held-out `--test` set; tuning to the
queries you report on is how you fool yourself.

## Wire it to your coding agent

Mesh speaks MCP (JSON-RPC) over stdio. Point your agent at:

```json
{ "command": "mesh", "args": ["mcp", "--vault", "/abs/path/to/my-vault", "--watch"] }
```

The agent then gets: `mesh_search` (fused, budget-aware), `mesh_fetch` (a note or one heading by anchor), `mesh_god_nodes` (the hub map to orient), `mesh_changed_since` (deltas on resume), and the write-back tools `mesh_append_note` / `mesh_write_entity`. The retrieval contract (how to query cheaply, and to write back when done) is served as the MCP `initialize` instructions and the `mesh://contract` resource, so any agent uses it well without extra prompting.

The `--watch` flag runs the live reindexer inside the server, so notes you (or a
teammate) edit in your editor become searchable in the same session without a
restart. Watch progress goes to stderr; stdout stays the pure JSON-RPC stream.
Omit it for the classic behavior where the index only refreshes on the agent's
own write-backs.

## Team sync

Share a vault across a team with no git on any client. The sync **client** is part
of the open core; the **team-sync hub** is the commercial / pro product (hosted at
mesh.brightinteraction.com, or self-host under a commercial license, see
[LICENSING.md](LICENSING.md)). Clients pull-reconcile against it:

```
# On each teammate's laptop (against a hosted or licensed hub):
mesh join https://mesh.example.com <invite-token> my-vault   # clone, no git needed
# ... edit notes in your editor ...
mesh sync my-vault                                    # push yours, pull theirs
```

Reconcile-first: `mesh sync` is a three-way merge. Two people adding blocks to the
same page auto-merge; a true overwrite of the same lines keeps the hub version and
saves yours to a `*.sync-conflict-*.md` sibling to resolve by hand. Deletes and
renames propagate; the hub authors git history attributed to each user. Add
`mesh sync --watch` for real-time SSE push (the hub's changes pull in as they
land). A standalone `mesh-curator` worker can reconcile non-trivial conflicts with
the team's own BYOAI model, committing the merged note back through the normal sync
path (the hub itself stays AI-free).

## Commands

Set up and capture:

| Command | Purpose |
|---|---|
| `mesh install` | One-shot setup: register the MCP server with your agent (plus the auto-onboard hook on Claude Code) |
| `mesh init [path]` | Bootstrap a new vault |
| `mesh new <type> "<title>"` | Scaffold a note (id, date, placement, skeleton auto-filled) |
| `mesh migrate [vault]` | Bring a Foam / Obsidian-style vault up to the Mesh schema |
| `mesh ingest <source>` | Pull external knowledge (GitHub, Slack, Linear, Jira, Notion) into the vault, incrementally |
| `mesh extract <transcript>` | Turn an agent session transcript into candidate write-back notes to keep or discard |
| `mesh hooks install` | Wire Claude Code session hooks: read Mesh at session start, nudge write-back at the end |

Index and retrieve:

| Command | Purpose |
|---|---|
| `mesh index [vault]` | Parse + persist the index (`.mesh/mesh.db`) |
| `mesh watch [vault]` | Live-reindex on every change (debounced + periodic reconcile) |
| `mesh embed [vault]` | Embed notes via a BYOAI endpoint (turns on semantic search) |
| `mesh search "<query>"` | Fused, budget-packed retrieval (semantic + rerank when configured) |
| `mesh ask "<question>"` | Answer a question from your notes + code with citations (needs a BYOAI model) |
| `mesh code <search\|context\|reindex>` | Source-code index: find a symbol by name (file:line), or pair it with the notes about it |
| `mesh orient [vault]` | Print a session orientation: entry points, recent changes, how to retrieve |
| `mesh mcp [--vault] [--watch]` | Serve the agent retrieval + write-back surface (live-reindex with `--watch`) |

Inspect and maintain:

| Command | Purpose |
|---|---|
| `mesh status [vault]` | Index row counts + which retrieval signals are active |
| `mesh version` | The commit this binary was built from, plus the Go version. Include it in a security report (see SECURITY.md) |
| `mesh lint [vault]` | Frontmatter / links / filenames (non-zero exit for CI) |
| `mesh doctor [vault]` | Index freshness (drift), counts, health |
| `mesh health [vault]` | Knowledge lifecycle: dead source refs, overdue reviews, contradictions |
| `mesh structure [vault]` | Grade the vault's organization: types, connectivity, tier-0, maps |
| `mesh flywheel [vault]` | Write-back reuse metrics: does written-back knowledge get used again? |
| `mesh guards <list\|suggest>` | Turn gotchas into candidate pre-commit guards (knowledge to enforcement) |
| `mesh scope backfill` | Stamp an explicit access scope on notes that have none (which notes a given member may see) |
| `mesh eval <cases.json>` | Gate-1 retrieval measurement vs FTS baselines |
| `mesh tune <cases.json>` | Learn fusion weights from labelled queries (validated on held-out) |

View:

| Command | Purpose |
|---|---|
| `mesh tui [vault]` | Keyboard three-pane terminal view (notes, ranked search, preview + neighbors) |
| `mesh ui [vault]` | Browser app (graph, search, docs, API reference) over the same index, localhost |
| `mesh serve-ssh [vault]` | Serve the TUI over SSH so a teammate browses the graph with `ssh`, no install (key-auth, fail-closed) |

Team sync. These are the client side and ship here, but they all talk to a
**team-sync hub**, which is the commercial product and is not in this repository
(see [LICENSING.md](LICENSING.md)):

| Command | Purpose |
|---|---|
| `mesh join <hub> <invite> [vault]` | Join a team vault and clone it (no git). Needs a hub. |
| `mesh sync [vault]` | Reconcile with the hub (push local edits, pull teammates'). Needs a hub. |
| `mesh conflicts <list\|diff\|resolve>` | Review and resolve local sync-conflict siblings. Needs a hub. |
| `mesh curator <log\|show\|accept>` | Review what the BYOAI sync-curator merged, and failed on, across the team. Needs a hub plus the commercial curator. |

## Build

```
go build ./...
go test ./...
```

No cgo. Storage is pure-Go `modernc.org/sqlite` in WAL mode; the `.mesh/` index is a derived, deletable artifact, the markdown is the source of truth.

## License & editions

Open core, dual-licensed. This repository (the single-user vault, graph, retrieval,
viewers, CLI, MCP surface, and the sync **client**) is the **Mesh Sustainable Use
License** (fair-code, see `LICENSE`): free to self-host, use internally or
commercially, and run for your own clients; you just cannot resell it as a hosted
service.

The **team-sync hub** and **BYOAI sync-curator** are a commercial product:

- **Hosted** at mesh.brightinteraction.com (managed team sync).
- **Sovereign self-host** under a commercial license + support, for EU / regulated
  orgs running the hub on their own infrastructure.

A commercial license to the core is available for uses the Mesh Sustainable Use
License does not fit. See [LICENSING.md](LICENSING.md) and
[docs/OPEN-CORE.md](docs/OPEN-CORE.md).
