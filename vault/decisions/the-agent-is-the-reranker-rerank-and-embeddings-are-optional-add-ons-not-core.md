---
id: the-agent-is-the-reranker-rerank-and-embeddings-are-optional-add-ons-not-core
type: decision
title: The agent is the reranker; rerank and embeddings are optional add-ons, not core
when: "2026-06-16"
created: "2026-06-16"
related:
    - cross-encoder-rerank-is-the-answer-1-lever-that-chunking-was-not
    - learned-fusion-weights-help-the-no-reranker-path-but-wash-out-under-rerank
tags:
    - mesh
    - retrieval
    - architecture
    - rerank
    - byoai
    - cost
do: 'Keep the tool a capable agent uses zero-model: FTS + graph + budget-packed cards + the write-back flywheel. The agent reads the ranked cards and picks the 1-2 notes to fetch; the agent IS the reranker. Ship rerank + embeddings as off-by-default optional BYOAI add-ons.'
dont: Do not put a cross-encoder reranker in a capable agent's path or sell answer@1 as an agent win. The agent is a stronger relevance judge than a small cross-encoder and reads the cheap cards anyway, so the reranker is redundant-to-harmful for it (it can rank a generic concept page above the actual tool; the agent will not).
why: answer@1 measures a consumer that trusts the top-1 result WITHOUT reading the cards. That is not a capable agent. The rerank 3->10 paraphrase gain accrues to a cheap/small downstream model, a blind fetch-top-1 pipeline, or a multi-tenant cloud tenant offloading ranking to a local judge to cut billed tokens. The core that serves the agent needs no models and near-zero CPU; that is the point of Mesh as the agent's context tool.
status: accepted
---

# The agent is the reranker; rerank and embeddings are optional add-ons, not core

## Context
<!-- TODO: what forced a decision; the situation and constraints -->

## Decision
<!-- TODO: what was decided, stated plainly -->

## Consequences
<!-- TODO: trade-offs accepted; what this makes easy or hard -->

## Related
<!-- linked notes from the related: field render in the graph -->

<!-- authored by claude+tom -->
