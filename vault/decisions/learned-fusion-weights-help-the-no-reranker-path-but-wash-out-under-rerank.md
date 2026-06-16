---
id: learned-fusion-weights-help-the-no-reranker-path-but-wash-out-under-rerank
type: decision
title: Learned fusion weights help the no-reranker path but wash out under rerank
when: "2026-06-16"
created: "2026-06-16"
related:
    - cross-encoder-rerank-is-the-answer-1-lever-that-chunking-was-not
    - blending-fused-score-into-rerank-does-not-beat-pure-rerank
    - vectors-decisively-lift-recall-on-paraphrase-queries
tags:
    - mesh
    - retrieval
    - fusion
    - tuning
    - eval
    - byoai
do: Use mesh tune <cases.json> --test <held-out.json> to fit fusion weights (FTS/graph/vector) to your corpus and apply via MESH_WEIGHT_FTS/GRAPH/VEC. It matters most for a vectors-on, rerank-off deployment, where vector-dominant weights lift answer@1 a lot.
dont: 'Do not bake the Hive-tuned weights (fts=0.2 graph=0 vec=0.8) as a global default: graph=0 is corpus-specific and the held-out gain is only +1/24. And do not expect tuning to help when rerank is on; the cross-encoder owns the head and fusion weights are byte-for-byte washed out.'
why: 'Grid search (rerank off) found vec-dominant weights beat the hand-picked 0.5/0.2/0.3: rerank-OFF answer@1 semantic 3->9, keyword 15->21, held-out 21->22. With rerank ON: identical (10/10, 14/14, 22/22). The hand-picked 0.3 vec weight was too conservative; the right home for the gain is the per-corpus mesh tune lever, not a global default change.'
status: accepted
---

# Learned fusion weights help the no-reranker path but wash out under rerank

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->

<!-- authored by claude+tom -->
