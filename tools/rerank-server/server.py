#!/usr/bin/env python3
"""Sovereign local cross-encoder rerank server for Mesh (BYOAI).

Serves the de-facto-standard rerank wire format (Cohere /v2/rerank, Jina
/v1/rerank) over a tiny stdlib HTTP server, backed by a small ONNX cross-encoder
via Qdrant's fastembed (no torch, no GPU, runs offline on CPU after the model
downloads once).

This server's own knobs (no MESH_ prefix): RERANK_MODEL, RERANK_HOST, RERANK_PORT.

Point the Mesh CLIENT (the Go binary) at it with the MESH_-prefixed vars:

    export MESH_RERANK_ENDPOINT=http://127.0.0.1:8787/rerank
    export MESH_RERANK_MODEL=Xenova/ms-marco-MiniLM-L-6-v2

Contract:
    POST /rerank  {"query": str, "documents": [str, ...], "top_n"?: int}
    -> 200 {"results": [{"index": int, "relevance_score": float}, ...], "model": str}
       results sorted by relevance_score descending; index is the document's
       position in the request. GET /health -> {"status":"ok","model":...}.

Setup (this machine's Homebrew Python has a broken libexpat, so use uv):
    brew install uv
    uv venv /tmp/mesh-rerank-venv --python 3.11
    uv pip install --python /tmp/mesh-rerank-venv/bin/python fastembed
    /tmp/mesh-rerank-venv/bin/python mesh/tools/rerank-server/server.py
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from fastembed.rerank.cross_encoder import TextCrossEncoder

MODEL_NAME = os.environ.get("RERANK_MODEL", "Xenova/ms-marco-MiniLM-L-6-v2")
HOST = os.environ.get("RERANK_HOST", "127.0.0.1")
PORT = int(os.environ.get("RERANK_PORT", "8787"))

print(f"[rerank] loading {MODEL_NAME} (first run downloads the ONNX model) ...", flush=True)
ENC = TextCrossEncoder(model_name=MODEL_NAME)
# Warm the model so the first real request is not penalized.
list(ENC.rerank("warmup", ["warmup document"]))
print("[rerank] model ready", flush=True)


def rerank(query, documents):
    # fastembed yields one score per document, in input order, higher = better.
    scores = list(ENC.rerank(query, documents))
    items = [{"index": i, "relevance_score": float(s)} for i, s in enumerate(scores)]
    items.sort(key=lambda x: x["relevance_score"], reverse=True)
    return items


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode("utf-8")
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.rstrip("/") == "/health":
            self._send(200, {"status": "ok", "model": MODEL_NAME})
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self):
        if self.path.rstrip("/") not in ("/rerank", "/v1/rerank", "/v2/rerank"):
            self._send(404, {"error": "not found"})
            return
        try:
            length = int(self.headers.get("content-length", 0))
            raw = self.rfile.read(length) if length else b"{}"
            req = json.loads(raw.decode("utf-8") or "{}")
        except (ValueError, json.JSONDecodeError) as e:
            self._send(400, {"error": f"invalid json: {e}"})
            return

        query = req.get("query")
        documents = req.get("documents")
        top_n = req.get("top_n")
        if not isinstance(query, str) or not query.strip():
            self._send(400, {"error": "'query' must be a non-empty string"})
            return
        if not isinstance(documents, list) or not all(isinstance(d, str) for d in documents):
            self._send(400, {"error": "'documents' must be a list of strings"})
            return
        if not documents:
            self._send(200, {"results": [], "model": MODEL_NAME})
            return
        try:
            results = rerank(query, documents)
        except Exception as e:  # surface model errors as 500
            self._send(500, {"error": f"rerank failed: {e}"})
            return
        if isinstance(top_n, int) and top_n > 0:
            results = results[:top_n]
        self._send(200, {"results": results, "model": MODEL_NAME})

    def log_message(self, fmt, *args):
        sys.stderr.write("[rerank] " + (fmt % args) + "\n")


def main():
    httpd = ThreadingHTTPServer((HOST, PORT), Handler)
    print(f"[rerank] listening on http://{HOST}:{PORT}  (POST /rerank)", flush=True)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\n[rerank] shutting down", flush=True)
        httpd.shutdown()


if __name__ == "__main__":
    main()
