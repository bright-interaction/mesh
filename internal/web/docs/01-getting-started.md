# Getting started

Mesh is a single binary that turns a folder of markdown notes into a knowledge
graph plus token-cheap retrieval for coding agents. Your notes stay plain files;
the `.mesh/` index is a derived, deletable artifact.

## The basics

```
mesh init my-vault          # scaffold a vault
mesh new decision "Use X"   # create a note (id, date, skeleton filled in)
mesh index                  # parse + persist the index
mesh search "how do we Y"   # fused full-text + graph search
```

## This app

You are looking at `mesh ui`, the local web app over one vault. The sections in the
left rail:

- **Graph** is the same graph an agent reads over MCP, as a force layout, a galaxy,
  and a 3D galaxy. Hover a note for its card; click to open it in your editor.
- **Search** runs the exact ranking an agent gets, so you can see what it sees.
- **Settings** configures retrieval, embeddings, and rerank, writing your solo
  `.mesh/config.toml`.
- **Docs** is what you are reading now.
- **API** documents the agent (MCP) tools and the HTTP API.

The viewer binds to `127.0.0.1` by default, so it is private to your machine. To
expose it on a network, pass a token (see the API section).
