---
id: mesh-watch-is-s0-local-first-live-reindex-the-mcp-server-must-watch-in-process-to-see-edits
type: decision
title: mesh watch is S0 local-first live reindex; the MCP server must watch in-process to see edits
when: "2026-06-16"
created: "2026-06-16"
related:
    - milestone-s-design-local-first-sovereign-online-hub-byoai-sync-curator-agent
tags:
    - milestone-s
    - watch
    - sync
    - mcp
do: Build live freshness as a shared internal/watch core (fsnotify + debounce + periodic reconcile safety net) and run it BOTH as standalone 'mesh watch' AND in-process via 'mesh mcp --watch'; gate reindex on content-hash drift (index.Reconcile) so cosmetic touches cost nothing
dont: Ship only standalone 'mesh watch' and assume a live MCP session sees the edits; the server caches graph+retriever in memory and only reloaded on its own write-backs, so a human's editor edit stayed invisible to the agent until the next append
why: S0's whole point is Obsidian-like immediacy for the agent's actual interface; reconcile-first (periodic full reconcile primary, fsnotify a best-effort speedup) mirrors the Milestone S design and always converges
---

# mesh watch is S0 local-first live reindex; the MCP server must watch in-process to see edits

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->
