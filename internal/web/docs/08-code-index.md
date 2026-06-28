# The code index (and the note-code bridge)

Alongside the note graph, Mesh keeps a separate index of your source code: every
function, type, method, and class, plus the Go call graph. It is how an agent answers
"where is this defined" and "what calls this" without grepping the tree, and how it
sees the team's knowledge ABOUT a piece of code, not just the code.

## What it indexes

Source files under the roots you configure (`.mesh/config.toml`, the `[code]` section,
or `MESH_CODE_ROOTS`). Go is parsed with the real Go parser (full call graph); ts, tsx,
js, svelte, astro, and py are parsed by a fast pure-Go scanner that locates symbols
(no call graph for those). The code lives in its own tables, isolated from the note
graph, so it never competes with note retrieval.

Refresh it with `mesh code reindex` (the git hooks run this for you), or `mesh_reindex`
from an agent. It rebuilds in seconds and is fully derived, like the rest of the index.

## Find code

```
mesh code search "deploy handler"
```

Returns ranked symbol cards with a `file:line` locator and signature. Identifier-split,
so "deploy handler" finds `DeployHandler`; test symbols are de-ranked. The same is
available to agents as `mesh_code_search`, and `mesh_code_neighbors` returns a Go
symbol's callers and callees.

## The note-code bridge

This is the part that makes the two indexes worth more together than apart. Mesh links
your notes to the code symbols they name, so you can go from a function to the
institutional knowledge about it:

```
mesh code context "RecordReuse"
```

returns the symbol's location AND the decisions/gotchas/post-mortems that reference it.
Agents call `mesh_code_context` before changing a function, so they inherit "here is
why this is written this way" instead of only its signature. In `mesh_code_search`
results, a symbol that notes reference is badged with the note count.

### How the linking stays precise

A note links to a symbol when a distinctive token (a qualified, snake_case, or
mixedCase identifier of five or more characters) either appears in the note TITLE or in
a backtick code span in the body, AND it exactly matches an indexed symbol name (or its
last segment, so a bare `RecordReuse` matches `Store.RecordReuse`). Generic words like
"open" or "git" never link. Body prose is not scanned (too noisy); titles are, because
a title is the clearest signal of what a note is about. Links rebuild on every code
reindex.

Links are scope-aware: a member only ever sees the linked notes they are allowed to
read.
