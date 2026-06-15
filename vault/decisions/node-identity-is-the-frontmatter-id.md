---
id: node-identity-is-the-frontmatter-id
type: decision
title: Node identity is the frontmatter id
when: "2026-06-16"
created: "2026-06-16"
tags:
    - graph
    - identity
do: Resolve wikilinks to the target's frontmatter id at parse time; store edges as note:<id>
dont: Derive node ids from slugs or file paths
why: Identity must survive file renames so the graph and agent citations stay valid
---

# Node identity is the frontmatter id

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->
