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
so it scores relevance directly. Measured on the Hive vault, turning this on
lifted paraphrase `answer@1` from 3/20 to 10/20 with no recall change.

It is a mechanical scoring transform, the same category as embeddings. Mesh
still has no reasoning AI inside it.

## Setup

This repo's machine has a broken Homebrew Python (libexpat symbol skew, the same
one that forces `DYLD_LIBRARY_PATH=/opt/homebrew/opt/expat/lib` on graphify), so
use `uv`, which brings its own standalone CPython:

```bash
brew install uv
uv venv /tmp/mesh-rerank-venv --python 3.11
uv pip install --python /tmp/mesh-rerank-venv/bin/python fastembed
```

(On a machine with a healthy Python, a plain `python3 -m venv` + `pip install
fastembed` works too. `onnxruntime` needs Python >= 3.11 for a prebuilt arm64
wheel.)

## Run

```bash
/tmp/mesh-rerank-venv/bin/python mesh/tools/rerank-server/server.py
# [rerank] listening on http://127.0.0.1:8787  (POST /rerank)
```

Env knobs: `RERANK_MODEL` (default `Xenova/ms-marco-MiniLM-L-6-v2`),
`RERANK_HOST` (default `127.0.0.1`), `RERANK_PORT` (default `8787`).

## Point Mesh at it

```bash
export MESH_RERANK_ENDPOINT=http://127.0.0.1:8787/rerank
export MESH_RERANK_MODEL=Xenova/ms-marco-MiniLM-L-6-v2
mesh status .          # confirms: rerank  active (cross-encoder ...)
mesh search "..." .    # now rerank-refined
```

`mesh search`, `mesh eval`, and `mesh mcp` all pick it up automatically. Unset
the two env vars to turn it back off (rerank never gates retrieval; if the
endpoint is down, Mesh silently falls back to the fused order).

Rerank is independent of embeddings: it reorders the fused FTS + graph (+ vector,
if on) candidates, so it works with or without `mesh embed`. It pairs best with
vectors on, since that is where the paraphrase top-1 gain was measured.

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
