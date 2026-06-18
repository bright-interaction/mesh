# Getting started

Mesh is a single binary that turns a folder of markdown notes into a knowledge
graph plus token-cheap retrieval for coding agents. Your notes stay plain files;
the `.mesh/` index is a derived, deletable artifact.

## Import an existing vault (Obsidian, or any markdown folder)

You do not convert anything. Mesh reads the markdown you already have. Point it at
your vault and index it:

```
mesh index ~/Desktop/Obsidian/MyVault     # parse + build the index in place
mesh ui ~/Desktop/Obsidian/MyVault        # open the web app over it
```

That is the whole import for local use. Obsidian `[[wikilinks]]` and tags become
graph edges automatically. Re-run `mesh index` (or use `mesh watch`) after edits.

## See it in the hosted app (push to a team hub)

The hosted app at `mesh.brightinteraction.com/app` shows a shared **hub** vault. To
get your notes in there, join the hub from your vault once, then sync:

```
mesh join https://mesh.brightinteraction.com <invite-token> ~/Desktop/MyVault
mesh sync ~/Desktop/MyVault               # pushes your notes up; pulls others' down
```

An operator mints `<invite-token>` with `docker exec mesh-hub mesh-hub invite`. After
a sync, click **Reindex now** in Settings (or reload) so the app shows the new notes.
`mesh sync` is also the import: it is a two-way merge, never a destructive overwrite.

## Point your AI agent at it (MCP)

This is what makes Mesh pay off. Add the MCP server to your agent (the **API** tab has
a copy-paste config for this exact vault):

```json
{ "mcpServers": { "mesh": { "command": "mesh", "args": ["mcp", "--vault", "/path/to/vault", "--watch"] } } }
```

The agent then retrieves with cheap tools (`mesh_search`, `mesh_fetch`, ...) instead of
reading whole files, and writes back what it learns with `mesh_append_note`. See the
**API** tab for every tool and the **Agents** doc for the flywheel.

## This app

You are looking at `mesh ui`, the web app over one vault. The left rail:

- **Graph** is the same graph an agent reads over MCP, as a force layout, a galaxy,
  and a 3D galaxy. Hover a note for its card; click to open it. The box up top only
  filters which nodes are visible by name.
- **Search** runs the exact ranking an agent gets over the full text of every note,
  so you can see what it sees. (This is the real search; the graph box is just a filter.)
- **Settings** is optional tuning (semantic search, ranking, rerank). Mesh works with
  none of it.
- **Docs** is what you are reading now.
- **API** documents the agent (MCP) tools and the HTTP API, with a copy-paste config.

The viewer binds to `127.0.0.1` by default, so it is private to your machine. To
expose it on a network, pass a token; you then sign in once with a session cookie.
