#!/usr/bin/env bash
# Produce the public AGPL-3.0 mirror of Mesh's open core at
# github.com/bright-interaction/mesh, so `go install
# github.com/bright-interaction/mesh/cmd/mesh@latest` resolves. See
# docs/RELEASING.md and docs/OPEN-CORE.md.
#
# Open core only: the pro layer (team-sync hub + BYOAI curator) is a separate
# commercial product and is stripped from ALL history here, so it never appears in
# the AGPL repo. The script subtree-splits mesh/, filter-repos out the pro paths,
# and build-checks that the core compiles standalone.
#
# Safe by default: with no --push it produces + build-checks the filtered tree and
# prints what it WOULD push. --push performs the outward mirror (requires the public
# repo to already exist: gh repo create bright-interaction/mesh --public).
set -euo pipefail

PUSH=0
REMOTE_URL="git@github.com:bright-interaction/mesh.git"
PREFIX="mesh"
SPLIT_BRANCH="mesh-public-split"

# The pro layer: stripped from the public mirror's entire history. Keep in sync with
# docs/OPEN-CORE.md. Paths are relative to mesh/ (the subtree-split strips the prefix).
PRO_PATHS=(internal/hub cmd/mesh-hub internal/curator cmd/mesh-curator internal/llm)

for arg in "$@"; do
  case "$arg" in
    --push) PUSH=1 ;;
    --remote=*) REMOTE_URL="${arg#--remote=}" ;;
    -h|--help)
      echo "usage: $0 [--push] [--remote=git@github.com:org/repo.git]"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

command -v git-filter-repo >/dev/null 2>&1 || {
  echo "error: git-filter-repo is required (pip install git-filter-repo)." >&2
  exit 1
}

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

if [ ! -d "$PREFIX" ]; then
  echo "error: $PREFIX/ not found at repo root $ROOT" >&2
  exit 1
fi

# Guard: refuse to mirror if a secret ever landed under mesh/ (defense before an
# outward, irreversible public push).
if git log -p -- "$PREFIX/" | grep -iEq '(api[_-]?key|secret|password|bearer|private[_-]?key)[[:space:]]*[:=][[:space:]]*["'"'"']?[A-Za-z0-9/_+.-]{16,}'; then
  echo "REFUSING: a possible secret appears in mesh/ history. Audit before any public push:" >&2
  echo "  git log -p -- $PREFIX/ | grep -iE 'key|secret|token|password'" >&2
  exit 1
fi

echo "Splitting $PREFIX/ subtree (history-preserving) into $SPLIT_BRANCH ..."
git branch -D "$SPLIT_BRANCH" >/dev/null 2>&1 || true
git subtree split --prefix="$PREFIX" -b "$SPLIT_BRANCH"

# Filter the pro layer out of the split's whole history in a throwaway clone, so the
# public mirror is the open core only and pro code never appears in any commit.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
CLONE="$WORK/mesh-public"
echo "Cloning $SPLIT_BRANCH -> $CLONE for filtering ..."
git clone --quiet --branch "$SPLIT_BRANCH" "file://$ROOT" "$CLONE"

FR_ARGS=()
for p in "${PRO_PATHS[@]}"; do FR_ARGS+=(--path "$p"); done
echo "Stripping pro paths from all history: ${PRO_PATHS[*]}"
( cd "$CLONE" && git filter-repo --force --invert-paths "${FR_ARGS[@]}" )

# Defense in depth: fail if any pro path survived the filter.
for p in "${PRO_PATHS[@]}"; do
  if [ -e "$CLONE/$p" ]; then
    echo "REFUSING: pro path '$p' still present after filter." >&2
    exit 1
  fi
done

echo "Build-checking the filtered open core ..."
if command -v go >/dev/null 2>&1; then
  ( cd "$CLONE" && go build ./... ) && echo "  open core builds standalone: OK"
else
  echo "  (go not found; skipping build check)" >&2
fi

if [ "$PUSH" -eq 0 ]; then
  echo
  echo "DRY RUN. Filtered open core is ready at: $CLONE"
  echo "Would push its main -> $REMOTE_URL"
  echo "Re-run with --push once the public repo exists (gh repo create bright-interaction/mesh --public)."
  echo "Cleanup of the split branch: git branch -D $SPLIT_BRANCH"
  trap - EXIT  # keep $WORK so the operator can inspect the dry-run tree
  exit 0
fi

echo "Pushing filtered open core -> $REMOTE_URL main ..."
( cd "$CLONE" && git push "$REMOTE_URL" HEAD:main )
echo "Done. Now tag a version in a clone of the public repo (see docs/RELEASING.md)."
echo "Cleanup of the split branch: git branch -D $SPLIT_BRANCH"
