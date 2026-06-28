# What Mesh is (the whole picture)

Mesh turns a folder of markdown notes into a knowledge graph and a token-cheap
retrieval engine for coding agents, then lets those agents write back what they learn
so the next one starts smarter. One Go binary, no database to run, no models inside the
core. Your notes stay plain markdown files; everything else is a derived index you can
delete and rebuild.

If you read one page, read this one. It explains how every part fits together; the
other pages go deep on each.

## The one-sentence model

Your markdown is the source of truth. Mesh indexes it into a graph, serves cheap
ranked answers to an agent over a standard protocol (MCP), and the agent records new
decisions and gotchas back into the markdown, so the vault gets more useful every time
it is used.

## The mental model in five layers

1. **The vault** is your folder of markdown notes. Wikilinks (`[[other-note]]`) and
   tags become graph edges. Mesh never rewrites your files; it only reads them.
2. **The index** (`.mesh/mesh.db`, a single SQLite file) is built from the vault:
   full-text search, the link graph, optional embeddings, optional source-code
   symbols. It is derived and disposable. Delete it and `mesh index` rebuilds it.
3. **Retrieval** blends full-text, graph proximity, and (optional) semantic vectors
   into one ranked list, surfaces decisions/gotchas/post-mortems first, and packs the
   best answer into a token budget. See "How retrieval works".
4. **The agent surface (MCP)** is how a coding agent uses all of the above: cheap
   tools to search, fetch, walk the graph, look up code, ask a question, and write
   back. The agent does the reasoning; Mesh is the fast engine. See "Agents".
5. **The team layer** (optional) is a sovereign sync hub plus this web app, so a whole
   team shares one vault with roles, access scopes, and audit. See "Team and editions".

## What it does, in plain terms

- **Finds the right knowledge cheaply.** Instead of an agent reading whole files to be
  safe, Mesh returns small ranked cards (title, snippet, why it matched) and the agent
  opens only the one note it needs. About a third of the tokens of classic RAG, with
  no embedding model or vector database required. See "Efficiency vs classic RAG".
- **Connects notes to code.** A separate source-code index lets you find a function by
  name and, crucially, see the team's notes ABOUT that function next to it (the
  note-code bridge). See "The code index".
- **Keeps the knowledge honest.** A lifecycle check flags notes that have gone stale,
  reference deleted code, are overdue for review, or contradict each other. See
  "Knowledge health".
- **Improves with use (the flywheel).** Agents write back what they learn. Mesh
  measures whether those notes actually get reused, and can auto-extract candidate
  notes from a finished session for one-click review. See "The flywheel".
- **Enforces what you have learned.** High-confidence gotchas can be turned into
  candidate pre-commit guards, so a mistake you wrote down cannot quietly come back.
  See "Guards".
- **Answers questions in plain language.** Ask a question and get an answer grounded
  only in your team's notes and code, with citations, never invented. See "Ask".

## How a day with Mesh actually looks

- You keep writing markdown notes the way you already do (Obsidian, plain files,
  whatever). Wikilinks and tags do the structuring.
- Your coding agent reads the mesh at the start of a session, retrieves cheaply while
  it works, and writes back any non-obvious decision or gotcha before it finishes.
- You (or your team) browse the same graph in this web app, search it, ask it
  questions, review auto-extracted notes, and watch the Dashboard to see whether the
  knowledge is compounding.

## What Mesh deliberately is NOT

- **Not an AI in a box.** The core runs zero models. The agent is the intelligence;
  Mesh is the index, the graph, and the retrieval. Optional AI add-ons (embeddings,
  rerank, the sync-curator, ask, extraction) are bring-your-own-model and off by
  default.
- **Not a database to operate.** No vector DB, no server required for solo use. One
  binary and your files.
- **Not a lock-in format.** It is your markdown. Stop using Mesh and you still have
  every note.

## Where to go next

- "Getting started" to import a vault and open this app.
- "Agents" for the agent (MCP) loop and the full tool list.
- "The code index", "Knowledge health", "The flywheel", "Guards", and "Ask" for each
  capability.
- The "API" tab for every tool, its schema, and a copy-paste agent config.
