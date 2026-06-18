# Releasing Mesh as a public `go install` tool

Mesh's module path is already `github.com/bright-interaction/mesh`, but the code
lives in the `bright-interaction/automations` monorepo under `mesh/`. Go's
`go install pkg@version` resolves a module from a repo whose path matches the
module path, so a public release is simply: **mirror the `mesh/` subtree to its own
repo `github.com/bright-interaction/mesh`, then tag a version.** No code changes.

This is an outward, hard-to-reverse step (it exposes the source publicly), so it is
a deliberate operator action, not part of `git psync`. Run it once, then each later
release is a re-run plus a new tag.

## One-time: create the public repo and seed it

`scripts/split-public-repo.sh` does the mechanical part (subtree split + push). It
does NOT create the GitHub repo or push without `--push`, so a dry run is safe.

1. Decide the license. The repo ships `LICENSE` (MIT, `Copyright (c) 2026 Bright
   Interaction`). Change it before the first public push if MIT is not the call.
2. Create the public repo (one command, outward):
   ```
   gh repo create bright-interaction/mesh --public \
     --description "Sovereign single-binary knowledge mesh for coding agents (MCP)"
   ```
3. Mirror the subtree (preserves `mesh/`'s history) and push:
   ```
   ./scripts/split-public-repo.sh --push
   ```
4. Verify the install resolves before tagging:
   ```
   go install github.com/bright-interaction/mesh/cmd/mesh@latest
   ```

## Cut a version

Tags live on the PUBLIC repo (not the monorepo - a monorepo tag cannot satisfy a
module path that differs from the repo path).

```
# in a clone of github.com/bright-interaction/mesh:
git tag v0.1.0
git push origin v0.1.0

# then verify the pinned install:
go install github.com/bright-interaction/mesh/cmd/mesh@v0.1.0
```

Suggested first tag: **v0.1.0** (pre-1.0: the CLI + MCP surface are stable in shape
but the API is not yet frozen).

## Each subsequent release

```
./scripts/split-public-repo.sh --push      # re-mirror the latest mesh/ subtree
# then tag the new version on the public repo as above
```

## What stays private

The hub deployment, ops scripts, `.env`s, and the rest of the monorepo never leave
`bright-interaction/automations`. Only the `mesh/` subtree (the tool's source) is
mirrored. Double-check no secret has ever been committed under `mesh/` before the
first public push (`git log -p -- mesh/ | grep -i -E 'key|secret|token|password'`).
