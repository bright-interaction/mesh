#!/usr/bin/env bash
# Mirror the mesh/ subtree of the bright-interaction/automations monorepo to its own
# public repo github.com/bright-interaction/mesh, so `go install
# github.com/bright-interaction/mesh/cmd/mesh@latest` resolves. See docs/RELEASING.md.
#
# Safe by default: with no --push it only computes the subtree split and prints what
# it WOULD push. --push performs the outward mirror (requires the public repo to
# already exist; create it with `gh repo create bright-interaction/mesh --public`).
set -euo pipefail

PUSH=0
REMOTE_URL="git@github.com:bright-interaction/mesh.git"
PREFIX="mesh"
SPLIT_BRANCH="mesh-public-split"

for arg in "$@"; do
  case "$arg" in
    --push) PUSH=1 ;;
    --remote=*) REMOTE_URL="${arg#--remote=}" ;;
    -h|--help)
      echo "usage: $0 [--push] [--remote=git@github.com:org/repo.git]"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

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
git subtree split --prefix="$PREFIX" -b "$SPLIT_BRANCH"
SPLIT_SHA="$(git rev-parse "$SPLIT_BRANCH")"
echo "split HEAD: $SPLIT_SHA"

if [ "$PUSH" -eq 0 ]; then
  echo
  echo "DRY RUN. Would push $SPLIT_BRANCH -> $REMOTE_URL main"
  echo "Re-run with --push once the public repo exists (gh repo create bright-interaction/mesh --public)."
  echo "Cleanup: git branch -D $SPLIT_BRANCH"
  exit 0
fi

echo "Pushing $SPLIT_BRANCH -> $REMOTE_URL main ..."
git push "$REMOTE_URL" "$SPLIT_BRANCH:main"
echo "Done. Now tag a version in a clone of the public repo (see docs/RELEASING.md)."
echo "Cleanup: git branch -D $SPLIT_BRANCH"
