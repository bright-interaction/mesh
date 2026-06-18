# Team sync and editions

## Team sync

Share a vault across a team with no git on any client. Each teammate runs:

```
mesh join https://hub.example.com <invite> my-vault   # clone, no git needed
mesh sync my-vault                                     # push yours, pull theirs
```

`mesh sync` is a three-way reconcile. Two people appending to the same page
auto-merge; a true overwrite of the same lines keeps the hub version and saves yours
to a `*.sync-conflict-*.md` sibling to resolve by hand. Deletes and renames
propagate, and the hub authors git history attributed to each user. Add
`mesh sync --watch` for real-time push.

## Editions

Mesh is open core, dual-licensed:

- The **open core** (this single-user app: vault, graph, retrieval, viewers, CLI,
  the MCP surface, and the sync client) is AGPL-3.0.
- The **team-sync hub** and the BYOAI conflict **curator** are a commercial product,
  available hosted or as a sovereign self-host license with support.

A commercial license to the core is available for uses the AGPL does not fit.
