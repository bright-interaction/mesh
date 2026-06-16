---
id: vectors-decisively-lift-recall-on-paraphrase-queries
type: decision
title: Vectors decisively lift recall on paraphrase queries
when: "2026-06-16"
created: "2026-06-16"
related:
    - weighted-sum-fusion-beats-rrf-for-precision-at-1
tags:
    - retrieval
    - vectors
    - eval
do: Keep vectors on (BYOAI); they recover the relevant note FTS misses when the query is paraphrased
dont: Judge vector value on FTS-friendly evals where queries echo note vocabulary - vectors look marginal there and the win is invisible
why: 'On a 20-query semantic-stress set (low lexical overlap, eval/hive-semantic.json), vectors lifted surfacing recall 13/20 to 19/20 (+46%) vs FTS-only; the win appears exactly where keyword search breaks. Weighted-sum fusion handles this: a note FTS missed still surfaces via its vector score'
---

# Vectors decisively lift recall on paraphrase queries

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->
