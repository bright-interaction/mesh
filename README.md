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

`mesh doctor` is the one to put in CI, so treat its exit code as the contract: **0 only
when the index is fresh and every note is actually in it**. It exits non-zero when the
index is stale, when there is no index yet, when any note fails to parse, and when two
notes claim the same id. The last two matter most: such a note is invisible to search and
to the graph, so doctor names the offending files and reports `status: BROKEN` instead of
a healthy looking summary.

A missing **owning writer** is the one thing doctor reports without failing on. It always
prints the owner line (`owner: NONE` plus how to start one), but on a fresh, in-sync vault
that is a NOTICE and the exit code stays 0, because `mesh init` leaves exactly that state
and CI checkouts legitimately have nothing running. It is only a failure in combination:
an index that has already drifted with nothing running to catch it up exits non-zero as
`status: STALE`. `mesh status` never fails on a missing owner at all; it reports counts.

A note id is vault-wide, not per folder. Two files that resolve to the same id (two
`README.md` with no frontmatter `id:`, or a note copied as a template that kept its
`id:` line) cannot both be indexed, so Mesh keeps one, quarantines the other, and says
which is which: `mesh index`, `mesh init`, `mesh doctor` and `mesh health` all name the
file and exit non-zero. The fix is to give one of the two a different id, in its
frontmatter or by renaming the file, then reindex.

Already have a Foam / Obsidian-style vault? Bring it up to the Mesh schema in one idempotent pass:

```
mesh migrate my-vault              # dry run: shows what it would change, writes nothing
mesh migrate my-vault --apply      # synthesize ids, updated->when, lift ## Related into related:
mesh index my-vault
```

`mesh migrate` and `mesh scope backfill` rewrite every note in the vault in place,
so both are a **dry run unless you pass `--apply`**. They also exit non-zero if any
file failed, so a partial rewrite never reads as success in a script.

## Recovery: the index is derived, so it is always safe to delete

`.mesh/mesh.db` is built from your markdown. The markdown is the source of truth and
nothing lives only in the index, so deleting it never loses a note.

If a command reports `file is not a database (26)` or `database disk image is malformed
(11)`, the index file is corrupt. Those are SQLite's two ways of saying the same thing:
26 means the file is not SQLite at all (something wrote over it), 11 means the header is
fine and a page inside it is not (a crash mid-write, a bad sector, a copy taken while a
write was in flight). Both are handled the same way. Rebuild it:

```
mesh index my-vault                # detects the corrupt file, discards it, rebuilds
```

Or do it by hand, which is the same thing:

```
rm -f my-vault/.mesh/mesh.db my-vault/.mesh/mesh.db-wal my-vault/.mesh/mesh.db-shm
mesh index my-vault
```

Two related failures that are **not** corruption and must not be fixed by deleting
anything:

- `database is locked (SQLITE_BUSY)` means another mesh process (usually a
  `mesh sync --watch` or `mesh mcp --watch` daemon) holds the write lock. Wait and
  retry, or stop the daemon. A full reindex only holds it for a few seconds.
- `no index at <path>` means there is no database yet. Run `mesh index <vault>`.
- `index schema mismatch` means the database was written by a different version of Mesh.
  The index is derived, so Mesh rebuilds rather than migrating, and only a WRITABLE open
  can do that. `mesh doctor`, `mesh ui` (without `--own-index`) and the TUI open the index
  read-only, so they report the mismatch and stop rather than answering from a schema they
  do not match. `mesh mcp` is **not** in that list: it elects itself the vault's owning
  writer when nothing else holds the lock and opens the index writable, so starting it
  against an index stamped with an older schema version rebuilds that index in place.
  Nothing is lost (the index is derived from your notes), but do not expect `mesh mcp` to
  leave an out-of-date index alone. Run `mesh index <vault>` once after upgrading.

Embeddings are the one thing a rebuild costs you: they are kept across schema upgrades
precisely because re-creating them is a paid API call, but they cannot survive a file
that SQLite cannot read. After recovering a corrupt index, re-run `mesh embed` if you
use semantic search.

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

mesh status my-vault    # PROBES both endpoints and says which signals really work
```

Both endpoints above are local, and that is the supported default: an endpoint you
pass on the command line or set in the environment is operator input, so Mesh dials
it as given, `localhost` and `127.0.0.1` included. The SSRF guard applies to the
endpoint someone else could have written for you: the `[embedding]` / `[rerank]`
fields in `.mesh/config.toml`, which the web UI's settings page rewrites over
`PUT /api/config`. If your endpoint lives there and is private, opt in explicitly:

```
export MESH_ALLOW_PRIVATE_LLM_ENDPOINT=1   # allow a private/loopback endpoint that
                                           # came from config.toml or the web UI
```

Mesh names that variable in the refusal itself, so you never have to find this
paragraph to get unstuck.

Vector search in this repository is a brute-force cosine scan, which stays under
5 ms well past a few thousand notes. The commercial build adds an approximate
(HNSW) index for vaults large enough to need one, see [LICENSING.md](LICENSING.md).

Once set, `mesh search` / `eval` / `mcp` fuse the semantic signal and apply the
rerank automatically. Turning a stage OFF is safe (no embedder means lexical-only),
but a stage you turned ON and that cannot be reached is an error, not a quiet
downgrade: `mesh search` fails and names the endpoint, rather than handing back
unreranked results that look reranked. Unset `MESH_RERANK_ENDPOINT` and
`MESH_RERANK_MODEL` to turn rerank off for real. Pointing either env var at a
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

That is the whole setup. The server elects itself the vault's **owning writer**
when nothing else holds the vault (a claim in `<vault>/.mesh/owner.lock`), so a
note it writes back is queryable immediately and no separate daemon is needed.
Start a `mesh watch` or `mesh sync --watch` beside it and that one takes
ownership instead; the MCP server notices, reads the index rather than writing
it, and routes its write-backs through the owner. Exactly one process indexes
either way, which is the point.

The `--watch` flag runs the live reindexer inside the server, so notes you (or a
teammate) edit in your editor become searchable in the same session without a
restart. Watch progress goes to stderr; stdout stays the pure JSON-RPC stream.
Omit it and the index only refreshes on the agent's own write-backs. On a vault
somebody else owns, `--watch` re-reads what that owner indexed rather than
indexing itself.

Not sure anything is indexing? `mesh doctor <vault>` names the owner, or prints
`owner: NONE` with the fix when there is none. That on its own is a notice, not a
failing exit; doctor fails when the index has drifted with no owner to catch it up.

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

## Upgrading: behaviour changes you will notice

An audit pass changed how several commands behave. Each of these is deliberate, and each
one is the kind of change that is confusing if you meet it without warning.

**`mesh migrate` and `mesh scope backfill` are dry runs by default.** They rewrite every
note in the vault in place with no backup, so writing is now opt-in via `--apply`. This is
the one most likely to catch you: a script that calls either of them bare no longer
rewrites anything, and it exits 0, so it looks like it worked. Add `--apply`.

**Read-only surfaces refuse an index written by an older Mesh.** `mesh doctor`, `mesh ui`
(without `--own-index`) and the TUI open the index read-only, and only a writable open can
rebuild a changed schema, so nothing ever migrated an upgraded index. They now say so and
name the fix (`mesh index <vault>`) instead of answering from a schema they do not match.
That matters most for `mesh_health` and `mesh doctor`, which reported a CLEAN vault over an
index whose quarantine table their binary expected and the file did not have. Run
`mesh index <vault>` once after upgrading; your notes are untouched. `mesh mcp` is the
exception: it now elects itself the owning writer, opens the index writable, and therefore
rebuilds a schema-mismatched index rather than refusing it.

**`mesh doctor` exits 1 when a note does not parse.** It used to report `status: healthy`
while holding zero indexed notes, because it counted only what had made it into the index
and an unparseable note never gets there. It now reports `status: BROKEN` and names how
many notes are invisible to search. If you gate CI on `mesh doctor`, it can now fail.

**`mesh index` and `mesh init` exit 1 when two notes claim one id.** Only one of the two
can be indexed. Both commands used to pick a winner silently, so `mesh init` printed
"1 notes" for two files and exited 0, and the loser was missing from search with no
signal at all. They now name the file they left out and fail, and the winner is stable:
whichever file already holds the id in the index keeps it, so a rebuild and a live
`mesh watch` never disagree about which note the id means.

**`mesh index` can delete a corrupt index.** A `.mesh/mesh.db` that SQLite refuses to open
used to dead-end every command including the one that rebuilds it. `mesh index` now
removes and rebuilds it, strictly when the failure is a corrupt database and never for any
other open error, such as a busy lock. Your notes are the source of truth; the index is
derived (see Recovery above).

**`mesh conflicts resolve --take-mine` exits non-zero when the hub refuses your push.** It
used to delete your parked conflict sibling and print that it had pushed, even when the
hub had rejected the path for a role, ACL, scope or size reason. It now keeps the sibling,
names the refusal, and fails. Wrappers treating exit 0 as "resolved" should be rechecked.

**`mesh curator log --status` rejects unknown values** rather than silently matching
nothing. Valid values are `failed` and `resolved`.

**`GET /api/search` defaults changed** to `limit=20` and `budget=8000`, matching the MCP
tool. Results are token-packed now; previously `budget=0` skipped packing entirely and
`limit` had no ceiling. `limit` is capped at 100.

**The hub's curation activity endpoint takes `?cursor`**, so failed jobs older than the
newest page are reachable. A malformed cursor returns 400 rather than being ignored.

**Search no longer hangs on a very large repetitive note.** One multi-megabyte note of
repeated text, a pasted deploy log or a concatenated transcript, could pin a core at 100%
for minutes with nothing able to cancel it, because SQLite was being asked to pick the
result excerpt and that costs roughly the square of the number of matches inside a single
document. Mesh builds the excerpt itself now. Matching and ranking still see the whole
note, so nothing becomes less findable, and the excerpt looks the same. A search that
somehow still runs long fails after 10 seconds with a message rather than hanging.

**The search query itself is now capped**, on `mesh_search`, `mesh_code_search`,
`mesh_code_context`, `GET /api/search` and `POST /api/ask`. A query over 4096 bytes is
refused with a message naming the limit, and a query is read as at most 64 distinct terms
on every surface, the CLI included. Repeating a word never changed which notes matched,
only how long the search took.

## Commands

Set up and capture:

| Command | Purpose |
|---|---|
| `mesh install` | One-shot setup: register the MCP server with your agent (plus the auto-onboard hook on Claude Code) |
| `mesh install --remove` | The inverse: drop the mesh entry from that client's config (and the session hooks on Claude Code). Run it before deleting the binary |
| `mesh init [path]` | Bootstrap a new vault |
| `mesh new <type> "<title>"` | Scaffold a note (id, date, placement, skeleton auto-filled) |
| `mesh migrate [vault]` | Bring a Foam / Obsidian-style vault up to the Mesh schema (dry run unless `--apply`) |
| `mesh ingest <source>` | Pull external knowledge (GitHub, Slack, Linear, Jira, Notion) into the vault, incrementally |
| `mesh extract <transcript>` | Turn an agent session transcript into candidate write-back notes to keep or discard |
| `mesh hooks install` | Wire Claude Code session hooks: read Mesh at session start, nudge write-back at the end |

Index and retrieve:

| Command | Purpose |
|---|---|
| `mesh index [vault]` | Parse + persist the index (`.mesh/mesh.db`). Non-zero if a note was left out because another note claims its id |
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
| `mesh doctor [vault]` | Index freshness (drift), counts, health. Non-zero if the index is stale or any note is invisible to search |
| `mesh health [vault]` | Knowledge lifecycle: dead source refs, overdue reviews, contradictions, plus notes missing from the index |
| `mesh structure [vault]` | Grade the vault's organization: types, connectivity, tier-0, maps |
| `mesh flywheel [vault]` | Write-back reuse metrics: does written-back knowledge get used again? |
| `mesh guards <list\|suggest>` | Turn gotchas into candidate pre-commit guards (knowledge to enforcement) |
| `mesh scope backfill` | Stamp an explicit access scope on notes that have none (which notes a given member may see; dry run unless `--apply`) |
| `mesh eval <cases.json>` | Gate-1 retrieval measurement vs FTS baselines |
| `mesh tune <cases.json>` | Learn fusion weights from labelled queries (validated on held-out) |

View:

| Command | Purpose |
|---|---|
| `mesh tui [vault]` | Keyboard three-pane terminal view (notes, ranked search, preview + neighbors) |
| `mesh ui [vault]` | Browser app (graph, search, docs, API reference) over the same index, localhost |
| `mesh serve-ssh [vault]` | Serve the TUI over SSH so a teammate browses the graph with `ssh`, no install (key-auth, fail-closed: binds `127.0.0.1:2222` by default, and `--allow-anonymous` is refused off loopback) |

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
