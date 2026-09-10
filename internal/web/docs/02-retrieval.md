# How retrieval works

A search blends three signals into one ranked list, optionally reranks a bounded
head, then packs the best bundle into a token budget.

## The signals

- **Full-text (FTS5)**: classic keyword match over note bodies.
- **Graph proximity**: notes near a match in the link graph get a boost, so a hit
  pulls in its neighbours one hop out.
- **Semantic (vectors)**: when embeddings are configured (Settings), cosine
  similarity over note vectors catches matches that share meaning, not words.

The three are fused with weights you can tune (Settings, or `mesh tune`). Decisions,
gotchas, and post-mortems are surfaced first (tier-0), because that is what an agent
most needs to inherit.

## Rerank

Rerank is optional and BYOAI. Configure a cross-encoder endpoint in Settings, or
set `MESH_RERANK_AGENT=codex|claude` in the local Mesh process to reuse that
developer's subscription login. Subscription mode sends at most 12 compact cards
to a low-effort model and returns 5 by default; it does not require Ollama or an API
key and never sends full note bodies. Its default `auto` policy keeps exact and
clearly separated full-text matches local, calling Luna/Haiku only for ambiguous
candidate slates. A provider failure is returned as an explicit local fallback and
opens a short circuit instead of launching another failing child on every search.

`mesh economics` reports content-free local counters for routes, calls, provider-
reported model usage when available (tokenizer estimates otherwise), cache hits,
fallbacks and returned context. A labelled `mesh eval`
run can require the reranker to beat the identical local route on both quality and
combined tokens with `--require-rerank-win`. MCP also attributes the first fetch
after each search to its rank and route without retaining the query or note content.

## Budget packing

Pass a token budget and Mesh returns the best subset of cards that fits, so an agent
spends its context on the highest-value notes instead of whole files.

## Scale

The vector signal uses a brute-force cosine scan, which is sub-5ms well past the v1
scale. The pro build adds an HNSW approximate index for very large vaults, gated by
the threshold in Settings.
