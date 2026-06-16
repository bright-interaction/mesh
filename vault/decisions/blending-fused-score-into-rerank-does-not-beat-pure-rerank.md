---
id: blending-fused-score-into-rerank-does-not-beat-pure-rerank
type: decision
title: Blending fused score into rerank does not beat pure rerank
when: "2026-06-16"
created: "2026-06-16"
related:
    - cross-encoder-rerank-is-the-answer-1-lever-that-chunking-was-not
    - per-section-embeddings-do-not-beat-whole-note-on-the-hive-corpus
tags:
    - mesh
    - retrieval
    - rerank
    - eval
    - negative-result
    - tuning
do: Keep MESH_RERANK_BLEND at the default 1.0 (pure cross-encoder rerank of the head). Reach for a lower blend only on a keyword-heavy corpus where the lexical/graph signal is strong, and re-measure with eval/ab-rerank.sh on a held-out set before trusting a non-default value.
dont: Do not assume blending the fused score back in recovers the one keyword answer@1 the cross-encoder trades away. An alpha sweep showed lowering the blend hurt paraphrase faster than it helped keyword; the keyword case only returned at a<=0.4, which gutted the paraphrase win.
why: 'Tuning-set sweep: pure rerank (a=1.0) had the best combined and best paraphrase (10/20 + 14/25); every partial blend was worse on paraphrase. Held-out set (24 fresh queries, never tuned on): no-rerank 21/24 -> rerank 22/24, a=1.0 tied-best. The keyword -1 is the irreducible cost of reranking, not a tunable artifact. Mixing two ranking signals lets mediocre-on-both notes rise.'
status: accepted
---

# Blending fused score into rerank does not beat pure rerank

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->

<!-- authored by claude+tom -->
