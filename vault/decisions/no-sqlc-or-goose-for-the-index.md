---
id: no-sqlc-or-goose-for-the-index
type: decision
title: No sqlc or goose for the index
when: "2026-06-16"
created: "2026-06-16"
related:
    - modernc-cannot-load-c-extensions
tags:
    - sqlite
    - storage
do: Hand-write parameterized SQL centralized in store.go/persist.go; bump SchemaVersion and drop-rebuild on schema change
dont: Add sqlc or goose; sqlc cannot model FTS5 vtables or dynamic fusion queries
why: The .mesh index is regenerable from the markdown source of truth, so migrations are pointless and retrieval queries are dynamic
---

# No sqlc or goose for the index

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->
