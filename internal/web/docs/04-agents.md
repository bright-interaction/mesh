# Agents and the flywheel

Mesh is built for coding agents. An agent connects over MCP and retrieves cheaply
instead of reading whole files, then writes back what it learned so the next agent
inherits it. That loop is the flywheel.

## Connect an agent

Point your agent at the MCP server:

```
mesh mcp --vault /path/to/vault --watch
```

`--watch` keeps the index fresh as you edit notes, so a file you change is
searchable in the same session.

## The contract

1. Orient with `mesh_god_nodes` (the most-connected notes are the entry points).
2. `mesh_search` with a token budget; reason over the returned cards.
3. `mesh_fetch` only when a card is not enough.
4. Walk with `mesh_neighbors` and `mesh_community` instead of fetching whole files.
5. On resume, `mesh_changed_since` returns only what changed.
6. Write back with `mesh_append_note` (decision / gotcha / post-mortem) so the next
   agent inherits the judgment. Mesh fills in id, timestamp, and placement.
7. If you edited files directly, `mesh_reindex` makes them queryable now.

See the API section for the full tool list and schemas. Editing through your editor
(not the write API) is the intended path; the watcher or `mesh_reindex` keeps the
index in lockstep.
