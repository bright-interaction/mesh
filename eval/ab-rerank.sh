#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Bright Interaction AB
# Deterministic A/B for the BYOAI cross-encoder rerank stage.
#
# Runs `mesh eval` twice on a labelled case set, rerank OFF then ON, with only
# the MESH_RERANK_* env vars toggled, so the cross-encoder's effect on answer@1
# is reproducible from a committed artifact (not a manual two-run toggle).
# Budget is pinned to 0 so packToBudget never runs and cannot perturb cards[0].
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

echo "== rerank OFF =="
env -u MESH_RERANK_ENDPOINT -u MESH_RERANK_MODEL \
  "$MESH" eval "$CASES" --vault "$VAULT" --budget 0 | grep -E "recall|answer@1"

echo "== rerank ON  (cross-encoder ${MESH_RERANK_MODEL}) =="
"$MESH" eval "$CASES" --vault "$VAULT" --budget 0 | grep -E "recall|answer@1"
