---
id: milestone-s-design-local-first-sovereign-online-hub-byoai-sync-curator-agent
type: decision
title: 'Milestone S design: local-first + sovereign online hub + BYOAI sync-curator agent'
when: "2026-06-16"
created: "2026-06-16"
related:
    - the-agent-is-the-reranker-rerank-and-embeddings-are-optional-add-ons-not-core
    - defer-team-sync-to-milestone-s
tags:
    - mesh
    - sync
    - milestone-s
    - hub
    - architecture
    - collaboration
    - byoai
do: 'Build team sync as Mesh-native + local-first: every client keeps the vault locally (works offline like Obsidian) and indexes locally; an OPTIONAL sovereign mesh-hub (self-hosted, git-backed for history) syncs markdown note deltas with one-command onboarding (mesh join <hub> <invite-token>, minimal config). Reconcile-first (a periodic full reconcile is the primary consistency mechanism; SSE push is a best-effort speedup). For non-trivial merges, an OPTIONAL BYOAI reconciliation AGENT (the team''s own AI) reads the incoming changes + existing context via Mesh retrieval and organizes them: dedup, cross-link [[wikilinks]], place new notes, update related, fold overlaps, then push one coherent Mesh update.'
dont: 'Do not put AI inside Mesh or the hub: the hub is mechanical (deltas, git history via commit-plumbing, append-merge for additive write-backs + per-path LWW with a *.sync-conflict sibling for true overwrites). The AI-organizes-contributions step is a BYOAI agent invoked as a sync-reconciliation job, NOT baked in. Do not require Syncthing or git-on-clients (kills the single-binary, no-cross-platform-git-pain goal). Do not sync the .mesh index or vectors (derived per-client; markdown is the only synced source of truth).'
why: 'Tom''s collaboration goal: minimal-config onboarding, vaults sync with each other, local-first like Obsidian, plus an online option where an AI catches each contributor''s updates and compiles/structures them while pushing a Mesh update and reconciling concurrent changes. Mesh-native hub fits single-binary + sovereignty + minimal footprint (no second daemon); reconcile-first tames the spec''s riskiest subsystem (reliable delivery); the zero-model core means only markdown syncs, so the embedding-homogeneity-at-join problem evaporates. The sync-curator is the flywheel at team scale.'
status: accepted
---

# Milestone S design: local-first + sovereign online hub + BYOAI sync-curator agent

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->

<!-- authored by claude+tom -->
