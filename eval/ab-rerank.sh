#!/usr/bin/env bash
# SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
# Copyright (C) 2026 Bright Interaction AB
# Paired evaluation for the BYOAI cross-encoder rerank stage.
#
# Uses one `mesh eval` invocation's explicit NoRerank local arm and configured
# endpoint arm. Unsetting endpoint variables cannot disable persisted config or
# a user-local subscription, so it is not a valid OFF control. Pin HTTP routing
# even when an inherited MESH_RERANK_AGENT selects a subscription CLI.
# Budget is pinned to 0 so packToBudget never runs and cannot perturb cards[0].
# Keep the complete accounting and failure status. This measures retrieval, not
# whole-task savings; query embeddings can still run in either arm. Use frozen
# cases/vault snapshots and separately approve endpoint use and quota/spend.
#
# Prerequisites:
#   - a built `mesh` on PATH (or set $MESH to its path)
#   - an embeddings endpoint + an embedded vault: MESH_EMBED_ENDPOINT/MODEL set,
#     `mesh embed <vault>` already run (rerank pairs best with vectors on)
#   - a rerank endpoint: MESH_RERANK_ENDPOINT/MESH_RERANK_MODEL set
#     (see tools/rerank-server for a local sovereign cross-encoder)
#
# Usage: eval/ab-rerank.sh <vault> <cases.json>
set -euo pipefail

MESH="${MESH:-mesh}"
VAULT="${1:?usage: ab-rerank.sh <vault> <cases.json>}"
CASES="${2:?usage: ab-rerank.sh <vault> <cases.json>}"
: "${MESH_RERANK_ENDPOINT:?set MESH_RERANK_ENDPOINT (e.g. http://127.0.0.1:8787/rerank)}"
: "${MESH_RERANK_MODEL:?set MESH_RERANK_MODEL (e.g. Xenova/ms-marco-MiniLM-L-6-v2)}"

echo "== local ranking vs configured HTTP rerank (one paired evaluation) =="
exec env MESH_RERANK_AGENT=http \
  "$MESH" eval "$CASES" --vault "$VAULT" --budget 0 --require-rerank-win
