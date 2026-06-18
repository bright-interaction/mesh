# Efficiency vs classic RAG

How Mesh compares to the standard RAG pattern an agent would otherwise use:
retrieve the top-k passages and stuff them into the prompt. Measured on a real
330-note vault with Mesh's built-in harness (`mesh eval`).

## The headline

Mesh answers the same question for **about a third of the tokens**, with
**equal-or-better recall**, and with **none of the embedding-model /
vector-database / re-embedding machinery**.

| Query type | Classic top-k RAG | Mesh | Saving |
|------------|------------------:|-----:|-------:|
| keyword (25 queries)    | 9,100 tokens  | 2,939 | **~3.1x** |
| paraphrase (20 queries) | 15,110 tokens | 4,619 | **~3.3x** |

Recall: Mesh ties keyword search (23/25) and **beats it on paraphrase**
(15/20 vs 13/20), the case where keyword RAG breaks.

## Why it is cheaper

A classic RAG reads several passages on every query to be safe. Mesh instead
returns ranked **cards** (title + snippet + why it matched) for a few hundred
tokens. The agent reads the cards, reasons over them, and opens exactly **one**
body, the right one, instead of three. The agent is the reranker, which beats a
bolt-on cross-encoder and costs nothing extra.

## The infra it removes

| | Classic embedding RAG | Mesh |
|--|------------------------|------|
| Models | an embedding model (GPU/API), often a reranker | none in the core; optional BYOAI |
| Storage | a vector database | one SQLite file + your markdown |
| A note changes | re-chunk + re-embed (seconds, stale until done) | re-index in ~0.4 ms, searchable at once |
| Query | embed round-trip + ANN lookup (network) | local full-text + graph, sub-millisecond, offline |
| Over time | read-only | agents write back decisions and gotchas, so retrieval improves with use |

## Honest notes

The win is against the realistic top-k RAG, not against a blind "open one
file" read (that is cheaper, but it guesses, and misses paraphrase). On keyword
queries the blind-top-1 metric is a tie; that metric models a pipeline that
trusts position 1 without reading the cards, which is not a capable agent. The
full methodology, raw numbers, and how to reproduce every row live in
`docs/BENCHMARK.md` in the repo.
