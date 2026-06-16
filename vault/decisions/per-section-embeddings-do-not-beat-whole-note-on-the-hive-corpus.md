---
id: per-section-embeddings-do-not-beat-whole-note-on-the-hive-corpus
type: decision
title: Per-section embeddings do not beat whole-note on the Hive corpus
when: "2026-06-16"
created: "2026-06-16"
related:
    - vectors-decisively-lift-recall-on-paraphrase-queries
    - weighted-sum-fusion-beats-rrf-for-precision-at-1
tags:
    - mesh
    - retrieval
    - embeddings
    - eval
    - negative-result
do: Keep whole-note (one structured title+flywheel+titled-sections vector per note) as the embed default; reach for mesh embed --per-section only on long heterogeneous corpora and re-measure before trusting it.
dont: Do not assume max-pool over per-section chunks lifts answer@1; on Hive it gave identical recall (20/20 semantic, 24/25 keyword), identical keyword answer@1 (15/25), and slightly worse semantic answer@1 (3/20 -> 2/20) at ~18x the embedding cost (330 -> 5867 vectors, 20s -> 2m10s).
why: Hive notes already carry a strong title + flywheel header, so the whole-note vector captures the gist; max-pool then adds top-1 noise by letting a wrong note spike to rank 1 on one tangential section. answer@1 needs reranking or better fusion, not finer chunks.
status: accepted
---

# Per-section embeddings do not beat whole-note on the Hive corpus

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->

<!-- authored by claude+tom -->
