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
# Run it yourself from the repo root: scripts/check-open-core-boundary.sh
# CI runs it on every push and pull request.
#
# The pro PACKAGE import paths below must stay in sync with the exclude set in
# docs/OPEN-CORE.md (and, in the private monorepo, with the release script that strips
# them). Note: internal/llm is OPEN (the BYOAI client shim has no moat; the flywheel
# that uses it is the open product). Pro .go files behind `//go:build pro` (e.g.
# retrieve_ann_pro.go, ui_hubteam_pro.go) never appear in the default build, so they
# are not listed here.
set -euo pipefail

# The module root. In the public repo that is the repo root; in the monorepo the
# module lives one level down, so try that too.
cd "$(git rev-parse --show-toplevel 2>/dev/null)" 2>/dev/null || true
[ -f go.mod ] || cd mesh 2>/dev/null || true
[ -f go.mod ] || { echo "error: run this from the Mesh module root (the directory holding go.mod)" >&2; exit 1; }

# internal/flarereport is pro too: it is the pro binaries' error-reporting shim, it
# carries the commercial license header, and it is not part of the published mirror. An
# open package importing it would break the mirror's build exactly like internal/hub does.
PRO_PKGS='internal/hub|cmd/mesh-hub|internal/curator|cmd/mesh-curator|internal/hnsw|internal/flarereport'

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
