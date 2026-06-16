---
id: cross-encoder-rerank-is-the-answer-1-lever-that-chunking-was-not
type: decision
title: Cross-encoder rerank is the answer@1 lever that chunking was not
when: "2026-06-16"
created: "2026-06-16"
related:
    - per-section-embeddings-do-not-beat-whole-note-on-the-hive-corpus
    - vectors-decisively-lift-recall-on-paraphrase-queries
    - weighted-sum-fusion-beats-rrf-for-precision-at-1
tags:
    - mesh
    - retrieval
    - rerank
    - cross-encoder
    - byoai
    - eval
do: Reach for a BYOAI cross-encoder rerank (reorder the top-30 fused candidates) when top-1 precision is the problem; it reads query+note jointly so it fixes answer@1 where bi-encoder vectors and chunking cannot. Run it locally (tools/rerank-server, fastembed/ONNX, no torch, no egress) to stay sovereign.
dont: 'Do not expect chunking/embedding tricks to move answer@1 (proven flat), and do not point MESH_RERANK_ENDPOINT at a cloud reranker casually: unlike one-time embedding egress, rerank streams the top ~30 candidate note bodies (including tier-0 institutional memory) off-box on every query.'
why: 'Measured A/B on the embedded Hive vault (same binary, MESH_RERANK_* toggled, reproduce via eval/ab-rerank.sh): paraphrase answer@1 3/20 -> 10/20, keyword 15/25 -> 14/25 (a real cross-encoder trade, confirmed not a tier-0 artifact), recall unchanged. A cross-encoder scores relevance directly instead of comparing independent embeddings.'
status: accepted
---

# Cross-encoder rerank is the answer@1 lever that chunking was not

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->

<!-- authored by claude+tom -->
