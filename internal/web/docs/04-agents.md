# Agents and the flywheel

Mesh is built for coding agents. An agent connects over MCP and retrieves cheaply
instead of reading whole files, then writes back what it learned so the next agent
inherits it. That loop is the flywheel, and it is the whole point.

## Connect an agent

Point your agent at the MCP server (the API tab has a copy-paste config for this exact
vault):

```
mesh mcp --vault /path/to/vault --watch
```

`--watch` keeps the index fresh as you edit notes, so a file you change is searchable
in the same session. New to a project? `mesh_setup_hooks` (or `mesh hooks install`)
wires Claude Code so the agent reads the mesh at session start and is nudged to write
back before finishing, automatically.

## The retrieval contract

1. Orient with `mesh_god_nodes` (the most-connected notes are the entry points).
2. `mesh_search` with a token budget; reason over the returned cards.
3. `mesh_fetch` only when a card is not enough (optionally one heading via an anchor).
4. Walk with `mesh_neighbors` and `mesh_community` instead of fetching whole files.
5. On resume, `mesh_changed_since` returns only what changed.
6. Write back with `mesh_append_note` (decision / gotcha / post-mortem, with a one-line
   do / dont / why) so the next agent inherits the judgment. Mesh fills in the id,
   timestamp, and placement. Use `mesh_write_entity` for a system/tool/concept page.
7. If you edited files directly, `mesh_reindex` makes them queryable now.

## The full toolset

Knowledge:

- `mesh_search` fused full-text + graph + (optional) vectors, tier-0 first, budgeted.
- `mesh_fetch` a note's markdown (or one heading section).
- `mesh_neighbors` / `mesh_community` walk the graph one hop at a time.
- `mesh_god_nodes` the map (hubs) to orient.
- `mesh_changed_since` deltas since a timestamp.
- `mesh_health` what is rotting (dead references, overdue reviews, contradictions).
  See "Knowledge health".

Source code (see "The code index"):

- `mesh_code_search` find a function/type/method by name (file:line + signature).
- `mesh_code_neighbors` the Go call graph (callers + callees) of a symbol.
- `mesh_code_context` a symbol PLUS the team's notes about it (code + the knowledge
  around it, in one call). Use this before changing a function.

Write-back:

- `mesh_append_note` record a decision / gotcha / post-mortem.
- `mesh_write_entity` create a system / tool / concept page.
- `mesh_reindex` re-read the vault now (after editing files directly).

Secrets (only when a Dockyard vault is attached, see "Secret vault"):

- `mesh_secret_status` is a vault attached, and how to use it.
- `mesh_secret_list` the stored credentials (names + rotation status only, never values).
- `mesh_secret_use` get a short-lived, single-use capability token for a destination and
  call it through the vault's proxy. The real key is injected server-side; you never see it.

Onboarding:

- `mesh_setup_hooks` wire the session hooks (read at start, nudge write-back at end).

## Why the agent is the reranker

Mesh returns cheap ranked cards and lets the agent pick the one note to open. The
agent reasoning over a few cards beats a bolt-on cross-encoder and costs nothing extra.
That is why the core runs zero models. Optional BYOAI add-ons (vectors, rerank) only
sharpen recall and precision; they are off by default.

Editing through your editor (not a write API) is the intended path; the watcher or
`mesh_reindex` keeps the index in lockstep. The API tab lists every tool and its schema.
