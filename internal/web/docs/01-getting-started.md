# Getting started

Mesh is a single binary that turns a folder of markdown notes into a knowledge
graph plus token-cheap retrieval for coding agents. Your notes stay plain files;
the `.mesh/` index is a derived, deletable artifact.

## Import an existing vault (Obsidian, or any markdown folder)

You do not convert anything. Mesh reads the markdown you already have. Point it at
your vault and index it:

```
mesh index /path/to/MyVault     # parse + build the index in place
mesh ui /path/to/MyVault        # open this web app over it
```

That is the whole import for local use. Obsidian `[[wikilinks]]` and tags become
graph edges automatically. Re-run `mesh index` (or use `mesh watch`) after edits.

The order matters: this app READS the index, it does not build it. One process owns
the index and writes it (`mesh watch`, or `mesh sync --watch` if you sync with a
team); everything else, this app and every `mesh mcp` window your agent opens, reads
what that writer persisted. That is why several agent sessions and a browser tab can
be open at once without fighting over the database. The write features here still
work: Reindex waits for the owning writer to catch up, and promoting a review
candidate writes the note and hands the bookkeeping to that writer. If it is not
running, they say so rather than pretending.

Nothing leaves your machine: the index is a `.mesh/` folder next to your notes, and
`mesh ui` binds to `127.0.0.1`. No account, no upload, no service to sign up for.

## No vault yet?

`mesh init my-vault` creates one with a starter index. The Mesh repository also
ships a small sample vault in `vault/`, so `mesh index ./vault && mesh ui ./vault`
from a clone shows you a populated graph in one step.

## Sharing a vault with a team

`mesh join` and `mesh sync` reconcile your vault with a **team-sync hub**, so a
whole team edits one vault with no git on any client. The hub is a commercial
product and is not part of this build. See LICENSING.md in the repository for the
hosted and self-hosted options.

Everything else in this app works without it. Team sync is the only feature that
needs a hub.

## Point your AI agent at it (MCP)

This is what makes Mesh pay off. Add the MCP server to your agent (the **API** tab has
a copy-paste config for this exact vault):

```json
{ "mcpServers": { "mesh": { "command": "mesh", "args": ["mcp", "--vault", "/path/to/vault", "--watch"] } } }
```

The agent then retrieves with cheap tools (`mesh_search`, `mesh_fetch`, ...) instead of
reading whole files, and writes back what it learns with `mesh_append_note`. See the
**API** tab for every tool and the **Agents** doc for the flywheel.

That config is self-sufficient: the MCP server claims this vault's **owning writer**
role when nothing else holds it, so what it writes back is searchable at once and what
you edit in your editor is picked up live. If you also run `mesh watch` or
`mesh sync --watch`, that one owns the index instead and the MCP server reads it.
Either way exactly one process indexes. `mesh doctor <vault>` says which, and prints
`owner: NONE` with the fix when nothing is indexing at all. On an otherwise in-sync
vault that is a notice and doctor still exits 0; it fails when the index has already
drifted and there is no owner to catch it up.

## This app

You are looking at `mesh ui`, the web app over one vault. The left rail:

- **Graph** is the same graph an agent reads over MCP, as a force layout, a galaxy,
  and a 3D galaxy. Hover a note for its card; click to open it. The box up top only
  filters which nodes are visible by name.
- **Search** runs the exact ranking an agent gets over the full text of every note,
  so you can see what it sees. (This is the real search; the graph box is just a filter.)
- **Ask** answers a plain-language question from your notes and code, with citations.
- **Dashboard** shows usage, tokens saved, knowledge health, and the flywheel reuse
  rate (whether your written-back notes actually get used again).
- **Review** is the queue of auto-extracted candidate notes to keep or discard, when
  auto-extraction is enabled.
- **Settings** is optional tuning (semantic search, ranking, rerank). Mesh works with
  none of it.
- **Docs** is what you are reading now (start with the Overview for the full picture).
- **API** documents the agent (MCP) tools and the HTTP API, with a copy-paste config.

The viewer binds to `127.0.0.1` by default, so it is private to your machine. To
expose it on a network, pass a token; you then sign in once with a session cookie.
