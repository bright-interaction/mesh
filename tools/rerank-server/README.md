# Sovereign rerank server for Mesh

A tiny, self-hosted cross-encoder reranker that Mesh calls to sharpen top-1
precision (`answer@1`). It speaks the same wire format as Cohere `/v2/rerank`
and Jina `/v1/rerank`, so the Mesh client is identical whether you point it
here or at a cloud provider.

Backed by [fastembed](https://github.com/qdrant/fastembed) over ONNX Runtime:
no PyTorch, no GPU, ~90 MB model downloaded once, then fully offline on CPU.

## Why rerank

The fused retrieval (FTS + graph + vector cosine) is good at *surfacing* the
right note in the candidate set (recall). It is weaker at putting it at rank 1
on paraphrase queries, because a bi-encoder vector compares the query and a note
independently. A cross-encoder reads the query and each candidate **together**,
so it scores relevance directly. On the private corpus behind `docs/BENCHMARK.md`,
turning this on lifted paraphrase `answer@1` from 3/20 to 10/20 with no recall
change. Your corpus will differ, so measure with `mesh eval` before committing.

It is a mechanical scoring transform, the same category as embeddings. Mesh
still has no reasoning AI inside it.

## Setup

Python 3.11 or newer (`onnxruntime` only publishes prebuilt arm64 wheels from
3.11 up). A plain virtualenv is enough:

```bash
python3 -m venv .venv
.venv/bin/pip install fastembed
```

If your system Python is patched up in a way that breaks native extensions (a
mismatched `libexpat` is the classic one on macOS Homebrew), `uv` sidesteps it by
bringing its own standalone CPython:

```bash
uv venv .venv --python 3.11
uv pip install --python .venv/bin/python fastembed
```

## Run

From the repository root:

```bash
.venv/bin/python tools/rerank-server/server.py
# [rerank] listening on http://127.0.0.1:8787  (POST /rerank)
```

Env knobs: `RERANK_MODEL` (default `Xenova/ms-marco-MiniLM-L-6-v2`),
`RERANK_HOST` (default `127.0.0.1`), `RERANK_PORT` (default `8787`).

## Point Mesh at it

```bash
export MESH_RERANK_ENDPOINT=http://127.0.0.1:8787/rerank
export MESH_RERANK_MODEL=Xenova/ms-marco-MiniLM-L-6-v2
mesh status ./vault              # probes the endpoint: rerank  active (cross-encoder ...)
mesh search "rerank" --vault ./vault   # now rerank-refined
```

`mesh status` sends a real one-document scoring request, so `active` means the
server answered, not merely that the two variables are set. A configured endpoint
that is down prints `rerank UNREACHABLE` with the reason.

`mesh search`, `mesh eval`, and `mesh mcp` all pick it up automatically. Unset the
two env vars to turn it back off. While it IS set, an endpoint Mesh cannot reach
fails the query and says so: a reranker you asked for and did not get would
otherwise return the plain fused order, exit 0, and look exactly like a working
one, which is how a dead reranker went unnoticed while every result was silently
unreranked.

Setting `MESH_RERANK_ENDPOINT` here in the environment is what makes the loopback
address above work with no further configuration: an endpoint from the environment
or a CLI flag is operator input and is dialed as given. Mesh's SSRF guard applies to
the `[rerank]` endpoint in `<vault>/.mesh/config.toml` instead, because the web UI
can rewrite that file; to allow a private endpoint from there, set
`MESH_ALLOW_PRIVATE_LLM_ENDPOINT=1`.

Rerank is independent of embeddings: it reorders the fused FTS + graph (+ vector,
if on) candidates, so it works with or without `mesh embed`. It pairs best with
vectors on, since that is where the paraphrase top-1 gain was measured.

`MESH_RERANK_BLEND` (default `1.0`) sets how much the cross-encoder owns the head
vs the fused score: `score = a*cross-encoder + (1-a)*fused`. On the corpus behind
`docs/BENCHMARK.md` an alpha sweep showed pure rerank (`1.0`) is best; lowering it
traded the paraphrase gain away faster than it recovered keyword cases. Lower it
only on a keyword-heavy corpus where the lexical signal deserves a vote, and
re-measure with `mesh eval <your-cases.json>` before trusting a non-default value.

## Sovereignty / data boundary

When the endpoint is **this local server**, candidate note bodies never leave
your machine. The wire format is also Cohere/Jina-compatible, so you *can* point
`MESH_RERANK_ENDPOINT` at a cloud reranker, but the egress is heavier than
embeddings, not "the same":

- **Embeddings** egress each note body **once**, at `mesh embed` time.
- **Rerank** egresses the top ~30 candidate note bodies on **every query**, and
  those candidates are by construction the most relevant notes, including the
  boosted tier-0 institutional memory (decisions, gotchas, post-mortems).

So a cloud reranker continuously streams your most sensitive notes off-box. Keep
the endpoint local to stay sovereign by default.

## Model options (set `RERANK_MODEL`)

| model | notes |
|-------|-------|
| `Xenova/ms-marco-MiniLM-L-6-v2` | default; fast, English, ~90 MB |
| `Xenova/ms-marco-MiniLM-L-12-v2` | more accurate, ~2x slower |
| `jinaai/jina-reranker-v2-base-multilingual` | multilingual |
| `BAAI/bge-reranker-base` | multilingual, larger (~1.1 GB) |
