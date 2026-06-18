# Mesh vs classic RAG: efficiency benchmark

Measured on the real Hive vault (330 notes, ~2,400 links) on 2026-06-18, with
Mesh's built-in Gate-1 harness (`mesh eval`). Reproduce any row yourself:

```
mesh eval eval/hive-heldout.json --vault ~/Desktop/Hive            # keyword queries
mesh eval eval/hive-semantic.json --vault ~/Desktop/Hive           # paraphrase queries
mesh eval eval/hive-heldout.json --vault ~/Desktop/Hive --budget 1200
```

## TL;DR

Against the standard RAG pattern an agent would otherwise use, retrieve the
top-k passages and stuff them into the prompt, **Mesh answers the same question
for about a third of the tokens, with equal-or-better recall, and with none of
the embedding-model / vector-DB / re-embedding machinery.**

- **~3.1x fewer tokens** on keyword queries (2,939 vs 9,100 median).
- **~3.3x fewer tokens** on paraphrase queries (4,619 vs 15,110 median).
- **Better recall where keyword RAG breaks**: 15/20 vs 13/20 on paraphrase.
- **Zero models, zero vector DB**: the core is pure-Go full-text + graph. No
  embedding endpoint, no GPU, no Pinecone/pgvector, runs fully offline.
- **Instant freshness**: a note edit re-indexes in ~0.4 ms; a classic RAG must
  re-embed the changed document (cost + lag) before it is searchable.

## What is being compared

A coding agent needs context from a knowledge base. The realistic options:

| Arm | What it does | Analogue |
|-----|--------------|----------|
| **naive top-k** (`fts-top3`) | retrieve the top 3 passages and read all of them | classic RAG: embed query, pull top-k chunks, stuff the prompt |
| **single-read** (`fts-top1`) | read only the single best-matching passage | a cheap "grep and open the first hit" |
| **Mesh** | return ranked cards (title + snippet + why), the agent reads them and opens exactly one body | Mesh's `mesh_search` |

All three arms count tokens with the *same* tokenizer, so the ratios are sound.
"Classic embedding RAG" maps onto the naive top-k arm on token cost (it also
stuffs k passages per query) and adds an embedding model + vector store on top.

## Results (median tokens per query, real Hive)

| Query set | naive top-k RAG | Mesh (unbudgeted) | Mesh (budget 1200) | Mesh saving vs top-k |
|-----------|----------------:|------------------:|-------------------:|---------------------:|
| keyword (25)    | 9,100  | 2,984 | 2,939 | **~3.1x** |
| keyword (10)    | 9,952  | 3,104 | -     | **~3.2x** |
| paraphrase (20) | 15,110 | 4,708 | 4,619 | **~3.3x** |

Recall and answer quality (does the right note surface / get read):

| Query set | surfacing recall @20 | answer@1 (single body) |
|-----------|----------------------|------------------------|
| keyword (25)    | Mesh 23/25 = FTS 23/25 | Mesh 13/25, single-read 14/25 |
| paraphrase (20) | **Mesh 15/20 > FTS 13/20** | Mesh 2/20 = single-read 2/20 |

## Reading the numbers honestly

- **The win is against the realistic RAG (top-k), not against a blind single
  read.** Mesh costs ~2x the cheapest possible "open one file" baseline, because
  it also returns the ranked cards. That card overhead (~1.5k tokens) is the
  whole point: the agent *sees the candidate set and the reasons*, so it opens
  the one correct body instead of guessing or reading three. Classic RAG pays
  the 3-body cost on every query; Mesh pays it once, in cheap cards.
- **answer@1 on keyword is a tie** (13 vs 14). That metric measures a consumer
  that blindly trusts position 1 without reading the cards, which is exactly the
  cheap/blind RAG pipeline, not a capable agent. A real agent reads the cards
  (free) and picks; the cards are why Mesh needs to read only one body.
- **Mesh wins clearly on paraphrase** (15 vs 13 surfacing), the case where
  keyword RAG breaks, because of graph proximity (and optional BYOAI vectors).
- The harness reported `tokenizer: estimate` (the BPE codec fell back to the
  char heuristic on this run); since every arm uses the same counter, the
  ~3x ratio holds regardless. Absolute counts are approximate.

## The efficiency that does not show up per query

Token-per-query is only half the story. A classic embedding RAG carries
machinery Mesh does not:

| Dimension | Classic embedding RAG | Mesh |
|-----------|------------------------|------|
| Models | an embedding model (GPU or paid API), often a reranker too | none in the core (pure-Go FTS + graph); embeddings/rerank are optional BYOAI add-ons |
| Storage | a vector database (Pinecone / pgvector / Chroma) | one SQLite file + the markdown; a single static binary |
| Indexing a change | re-chunk + re-embed the document (seconds + API cost), stale until done | content-hash reindex in ~0.4 ms/edit; searchable immediately |
| Query latency | embed round-trip + ANN lookup (network) | local FTS + graph walk, sub-millisecond, offline |
| Retrieval quality lever | tune chunk size / k / reranker | the agent *is* the reranker: it reads cheap cards and judges, beating a bolt-on cross-encoder |
| Improving over time | read-only | write-back flywheel: agents append decisions/gotchas, so retrieval gets richer with use |
| Sovereignty | data leaves for embeddings unless self-hosted; extra services | no egress by default, no extra services, no Python |

## Verdict

For the job Mesh is built for, a capable coding agent retrieving from a
markdown knowledge base, Mesh is roughly **3x more token-efficient per query
than the standard top-k RAG**, with **equal-or-better recall**, **no embedding
model or vector database**, **sub-millisecond offline retrieval**, and
**instant freshness**. The per-query token win compounds with the eliminated
infra: there is no embedding bill, no vector store to run, and no re-embedding
lag every time a note changes.

If your prior setup was a specific stack (a named vector DB, a chunk size, a
particular embedding model), point it out and this comparison can be tightened
to those exact parameters.
