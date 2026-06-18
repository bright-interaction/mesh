# How retrieval works

A search blends three signals into one ranked list, then optionally reranks the
head with a cross-encoder, then packs the best bundle into a token budget.

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

If you set a rerank endpoint (Settings), a cross-encoder rescasts the top results
for precision. It is optional and BYOAI: point it at your own model.

## Budget packing

Pass a token budget and Mesh returns the best subset of cards that fits, so an agent
spends its context on the highest-value notes instead of whole files.

## Scale

The vector signal uses a brute-force cosine scan, which is sub-5ms well past the v1
scale. The pro build adds an HNSW approximate index for very large vaults, gated by
the threshold in Settings.
