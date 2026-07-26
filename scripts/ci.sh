#!/usr/bin/env bash
# SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
# Copyright (C) 2026 Bright Interaction AB
#
# Every check CI runs, in one script you can also run yourself:
#
#   bash scripts/ci.sh
#
# Run it before opening a pull request and CI will not tell you anything new.
#
# WHY THE LOGIC LIVES HERE AND NOT IN .github/workflows/ci.yml
#
# Two reasons, and the second is not obvious.
#
# 1. A contributor can run it. Checks that only exist inside a workflow file are
#    checks you cannot reproduce locally, so you discover them after pushing.
#
# 2. It keeps .github/workflows/ci.yml STABLE, which is what makes automated
#    publishing possible at all. This repository is a filtered mirror, published by
#    a CI job authenticated with a repository deploy key. GitHub does not allow a
#    deploy key (or an OAuth token without the `workflow` scope) to create or update
#    anything under .github/workflows/. So every time a CI step changed, the publish
#    was rejected at the final push, after every gate had already passed, with an
#    error that read like a mystery. With the steps in this script, changing a check
#    touches scripts/ci.sh (an ordinary file the deploy key may write) and the
#    workflow file stays untouched, so the publish goes through.
#
# The consequence to remember: editing .github/workflows/ci.yml is now a rare,
# deliberate act (an action version bump, a trigger change) that needs a push from a
# credential with the `workflow` scope. Editing THIS file needs nothing special.
set -uo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null)" 2>/dev/null || true
[ -f go.mod ] || cd mesh 2>/dev/null || true
[ -f go.mod ] || { echo "error: run this from the Mesh module root (the directory holding go.mod)" >&2; exit 1; }

FAILED=()
step() {
  local name="$1"; shift
  printf '\n=== %s ===\n' "$name"
  if "$@"; then
    printf 'ok: %s\n' "$name"
  else
    printf 'FAIL: %s\n' "$name" >&2
    FAILED+=("$name")
  fi
}

# Are we in the published mirror, or in the private monorepo the mirror is cut from?
# internal/hub is the commercial hub server: it exists only upstream and is stripped
# from every published commit, so its presence is a reliable marker. Two checks below
# have to mean different things in the two places, and getting that wrong makes the
# script either useless upstream or toothless downstream.
IN_MIRROR=1
[ -d internal/hub ] && IN_MIRROR=0

# Every source file the MIRROR publishes is under the Mesh Sustainable Use License, and
# the header is how a reader (and a downstream packager) knows it. A file without one is
# either an unlicensed contribution or a file that belongs in the commercial repo
# instead, and both have happened: a commercially-headered package once reached a
# publish candidate unnoticed. One awk pass per file, no network.
#
# Upstream the same tree also holds the commercial layer, which is correctly labelled
# with the commercial identifier and is stripped before publication. So upstream a file
# must carry SOME recognised Mesh header, and in the mirror it must specifically carry
# the open one. That difference is the point: the strict form is what guards the
# published artifact.
license_headers() {
  local missing pattern
  if [ "$IN_MIRROR" = "1" ]; then
    pattern='SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License'
  else
    pattern='SPDX-License-Identifier: LicenseRef-(Mesh-Sustainable-Use-License|BrightInteraction-Commercial)'
  fi
  missing=$(git ls-files '*.go' '*.js' '*.css' '*.sql' '*.py' '*.html' '*.sh' | while read -r f; do
    awk -v pat="$pattern" 'NR<=5 && $0 ~ pat { found=1; exit } END { exit !found }' "$f" \
      || printf '%s\n' "$f"
  done)
  if [ -n "$missing" ]; then
    echo "These files lack a Mesh SPDX header in their first 5 lines:" >&2
    echo "$missing" >&2
    echo "Add it in that file type's comment syntax (// for Go/JS/CSS, -- for SQL, # for Python/shell," >&2
    echo "<!-- --> for HTML), or the file does not belong here." >&2
    return 1
  fi
  if [ "$IN_MIRROR" = "1" ]; then
    echo "every tracked source file carries the open SPDX header"
  else
    echo "every tracked source file carries a Mesh SPDX header (open or commercial; the mirror requires open)"
  fi
}

# The two scanners are installed on demand. Locally that needs network; set
# MESH_CI_SKIP_SCANNERS=1 to skip them when offline, and say so rather than passing
# quietly, because a skipped security scan that reports "ok" is worse than no scan.
scan() {
  local name="$1" mod="$2"; shift 2
  if [ "${MESH_CI_SKIP_SCANNERS:-0}" = "1" ]; then
    echo "SKIPPED ($name): MESH_CI_SKIP_SCANNERS=1"
    return 0
  fi
  go install "$mod" >/dev/null 2>&1 || { echo "could not install $name (offline?)" >&2; return 1; }
  "$(go env GOPATH)/bin/$name" "$@"
}

step "build"            go build ./...
step "vet"              go vet ./...
step "test"             go test ./... -count=1
step "open-core boundary" bash scripts/check-open-core-boundary.sh
step "license headers"  license_headers
# In the mirror, every published commit is this project's, so scanning the full history
# is exactly right and is the last gate before a credential becomes world-readable.
# Upstream, the history belongs to a whole monorepo of unrelated projects, so a full scan
# reports hundreds of findings that have nothing to do with Mesh and would train everyone
# to ignore the check. There the working tree is what matters, and the publish pipeline
# scans the real filtered payload's full history separately before it pushes.
if [ "$IN_MIRROR" = "1" ]; then
  step "secret scan (full history)" scan gitleaks github.com/zricethezav/gitleaks/v8@latest detect --source . --no-banner --redact
else
  step "secret scan (working tree; the publish pipeline scans the filtered history)" \
    scan gitleaks github.com/zricethezav/gitleaks/v8@latest detect --source . --no-git --no-banner --redact
fi
step "vulnerability scan" scan govulncheck golang.org/x/vuln/cmd/govulncheck@latest ./...

printf '\n'
if [ ${#FAILED[@]} -ne 0 ]; then
  # Report EVERY failure, not just the first. A run that stops at the first problem
  # costs a full round trip per issue.
  printf 'ci: %d check(s) failed: %s\n' "${#FAILED[@]}" "${FAILED[*]}" >&2
  exit 1
fi
echo "ci: all checks passed"
