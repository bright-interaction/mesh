---
id: weighted-sum-fusion-beats-rrf-for-precision-at-1
type: decision
title: Weighted-sum fusion beats RRF for precision-at-1
when: "2026-06-16"
created: "2026-06-16"
related:
    - lean-binary-prove-gate-1-first
tags:
    - retrieval
    - fusion
do: Keep FTS-dominant weighted-sum fusion (FTS 0.5 / graph 0.2 / vec 0.3); add vectors as a minor weighted signal
dont: 'Switch to Reciprocal Rank Fusion here: measured on 25 held-out queries it regressed answer@1 from 14/25 to 8/25'
why: RRF rewards multi-signal consensus over a strong single-signal match; on an FTS-friendly corpus the relevant note is often a strong lexical hit absent from graph/vector, so precision@1 needs one signal to dominate, not consensus
---

# Weighted-sum fusion beats RRF for precision-at-1

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->
