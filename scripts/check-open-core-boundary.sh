#!/usr/bin/env bash
# SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
# Copyright (C) 2026 Bright Interaction AB
# Open-core boundary guard. Fails if any OPEN package imports a PRO package in the
# default (open) build, which would make the published fair-code mirror fail to compile
# once split-public-repo.sh strips the pro paths.
#
# Why this exists: on 2026-06-30 the boundary had silently rotted - the flywheel
# features (ask/extract/guards) imported internal/llm (then classified pro) and
# `mesh ui --hub-db` imported internal/hub, so a mirror push would have failed its
# own build gate. internal/llm was reclassified open; internal/hub got a build-tag
# seam (cmd/mesh/ui_hubteam_{pro,stub}.go). This check stops the rot recurring.
#
# Run standalone (cd mesh && scripts/check-open-core-boundary.sh) or via the release
# gate in split-public-repo.sh and the repo pre-commit hook.
#
# The pro PACKAGE import paths below must stay in sync with PRO_PATHS in
# split-public-repo.sh and the exclude set in docs/OPEN-CORE.md. Note: internal/llm
# is OPEN (the BYOAI client shim has no moat; the flywheel that uses it is the open
# product). Pro .go files behind `//go:build pro` (e.g. retrieve_ann_pro.go,
# ui_hubteam_pro.go) never appear in the default build, so they are not listed here.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)/mesh" 2>/dev/null || {
  # Allow running from inside mesh/ directly too.
  [ -f go.mod ] || { echo "error: run from the mesh module" >&2; exit 1; }
}

PRO_PKGS='internal/hub|cmd/mesh-hub|internal/curator|cmd/mesh-curator|internal/hnsw'

leaks="$(go list -deps -f '{{.ImportPath}} {{join .Imports " "}}' ./... 2>/dev/null \
  | awk -v pro="$PRO_PKGS" '{p=$1; if(p ~ ("("pro")"))next; for(i=2;i<=NF;i++) if($i ~ ("("pro")")) print "  LEAK: "p" imports "$i}')"

if [ -n "$leaks" ]; then
  echo "open-core boundary VIOLATED: an open package imports a pro package." >&2
  echo "The fair-code mirror would not compile once split-public-repo.sh strips the pro paths." >&2
  echo "$leaks" >&2
  echo "Fix: reclassify the dep as open, or put a //go:build pro seam between them" >&2
  echo "(see cmd/mesh/ui_hubteam_{pro,stub}.go and docs/OPEN-CORE.md)." >&2
  exit 1
fi

echo "open-core boundary OK: no open package imports a pro package."
