# Mesh

A sovereign, single-binary knowledge base whose primary reader is a coding agent.

You edit plain markdown in your IDE. Your agent (Claude Code, Codex) searches it over MCP, and when it finishes a piece of work it **writes back what it learned**, a decision, a gotcha, a post-mortem, so the next agent inherits it. That write-back loop is the point: the knowledge base documents itself and gets smarter every run. Mesh has no reasoning AI inside it; it is the fast engine (parse, index, graph, retrieve), and the agent is the librarian.

It is one Go binary, no cgo, no external services. Retrieving from Mesh is cheaper than having the agent read whole files: it returns ranked cards (title + the matched snippet + why it surfaced) and packs the best bundle that fits a token budget, so the agent reads one note instead of three.

## Honest scope

- **What's proven:** cheap card-based retrieval (matches plain full-text search on quality, at a fraction of the tokens of reading files in full) plus the agent write-back flywheel, in a single no-glue binary.
- **What's not claimed:** Mesh fuses full-text with a graph-BM25 signal and a tier-0 boost for institutional-memory notes. On the vaults measured so far that fusion *matches* full-text search; it does not beat it. It is kept on as a non-harmful re-ranker, not sold as a retrieval-quality win. See `docs/SPEC.md` for the Gate-1 measurement and its adversarial review.
- **Deferred:** vector embeddings, a TUI + web graph viewer, and team sync (a self-hosted hub). Solo, local v1 first.

## Install

Mesh currently lives in the `bright-interaction/automations` monorepo, so build it from a checkout:

```
cd automations/mesh
go build -o mesh ./cmd/mesh
cp mesh ~/.local/bin/   # or anywhere on your PATH
```

(A `go install ...@latest` path lands once Mesh is split into its own public module.)

## Quickstart

```
mesh init my-vault                 # bootstrap a vault (starter index + first build)
mesh new decision "Use Postgres over Mongo" \
  --do "..." --dont "..." --why "..." --vault my-vault   # capture judgment; Mesh fills id/date/placement
mesh index my-vault                # rebuild the index after edits
mesh search "datastore choice" --vault my-vault --budget 4000
mesh doctor my-vault               # is the index fresh? any drift or lint problems?
```

Already have a Foam / Obsidian-style vault? Bring it up to the Mesh schema in one idempotent pass:

```
mesh migrate my-vault              # synthesize ids, updated->when, lift ## Related into related:
mesh index my-vault
```

## Wire it to your coding agent

Mesh speaks MCP (JSON-RPC) over stdio. Point your agent at:

```json
{ "command": "mesh", "args": ["mcp", "--vault", "/abs/path/to/my-vault"] }
```

The agent then gets: `mesh_search` (fused, budget-aware), `mesh_fetch` (a note or one heading by anchor), `mesh_god_nodes` (the hub map to orient), `mesh_changed_since` (deltas on resume), and the write-back tools `mesh_append_note` / `mesh_write_entity`. The retrieval contract (how to query cheaply, and to write back when done) is served as the MCP `initialize` instructions and the `mesh://contract` resource, so any agent uses it well without extra prompting.

## Commands

| Command | Purpose |
|---|---|
| `mesh init [path]` | Bootstrap a new vault |
| `mesh new <type> "<title>"` | Scaffold a note (id, date, placement, skeleton auto-filled) |
| `mesh migrate [vault]` | Bring a Hive/Foam-style vault up to the Mesh schema |
| `mesh index [vault]` | Parse + persist the index (`.mesh/mesh.db`) |
| `mesh search "<query>"` | Fused, budget-packed retrieval |
| `mesh status [vault]` | Index row counts |
| `mesh lint [vault]` | Frontmatter / links / filenames (non-zero exit for CI) |
| `mesh doctor [vault]` | Index freshness (drift), counts, health |
| `mesh eval <cases.json>` | Gate-1 retrieval measurement vs FTS baselines |
| `mesh mcp [--vault]` | Serve the agent retrieval + write-back surface |

## Build

```
go build ./...
go test ./...
```

No cgo. Storage is pure-Go `modernc.org/sqlite` in WAL mode; the `.mesh/` index is a derived, deletable artifact, the markdown is the source of truth.
