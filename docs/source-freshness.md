# Source freshness through the index owner

Local commit hooks and the custom `git psync` wrapper cannot observe a managed
server-side merge. Also, an ordinary `mesh code reindex` cannot become a second
writer beside a running owner. The former refresh hook suppressed that error.

`mesh code reindex <vault> --through-owner` queues a full refresh of the owner's
`[code]` configuration and requires a positive completion receipt. It makes no
model calls, accepts no root/language/full overrides, and works with an idle vault
or a running owner. Upgrade the owner before using it: old owners discard unknown
queue operations, which produces a timeout, never a false acknowledgement.
Each request binds the observed code configuration hash; a configuration change
before execution discards the obsolete request without acknowledging other roots.

Queued bursts share one full parse per drain. Full parsing catches Git rewrites
whose mtimes fall in the same second. File-read failures preserve the previous
code index; normal source-scanner syntax tolerance is unchanged (this is not a
compiler). Failed requests remain queued without starving unrelated bookkeeping.
Unchanged failed configurations have a one-minute owner retry cooldown, so note
events cannot continuously restart an expensive failed scan. Owner replacement
allows one immediate recovery attempt. Configured roots must be real directories,
not symlinks: the source walker does not descend symlink roots.
Symbols commit before bridge links; the receipt follows both. Readers may observe
the normal intermediate code/bridge state, not a new cross-table atomic snapshot.
Successful callers remove their receipt; abandoned receipts expire after 24 hours
during later refreshes. Cancellation is cooperative between existing parse/write
phases, not a hard deadline on in-flight filesystem I/O.

## Workspace catch-up service (opt-in)

The version-controlled helper is `scripts/git-hooks/mesh-code-refresh.sh` in the
Automation HQ monorepo. It is shared by local hooks and the scheduled backstop.
Set `git config mesh.codeIndexRoot /absolute/path/to/dedicated-index-worktree` in
that repository. The path must be a clean, detached linked worktree in the same
repository and match the vault's sole configured code root (`--expect-root`).
Do not use an authored checkout. No sibling-path guessing, force checkout, or
recursive clean is used. Dirty/untracked/ignored files and divergent history stop
the refresh without deleting anything.

For an approved installation, copy the reviewed helper to the path named by
`ops/launchd/com.brightinteraction.mesh-code-refresh.plist`, customize its paths,
and register it with launchd. It runs at startup and every 300 seconds in foreground
mode; launchd does not overlap instances. Hooks remain background accelerators.
Other operating systems can schedule the same foreground command:

```sh
MESH_CODE_REFRESH_FOREGROUND=1 sh scripts/git-hooks/mesh-code-refresh.sh 0 scheduled
```

The worker fetches main into a private ref and only advances
`<git-common-dir>/mesh-code-indexed.sha` after acknowledged indexing. A failed
refresh is retried even if checkout HEAD already equals main. A successful idle
run fetches/checks state but does not parse or call a model. The checkpoint is
scoped to source SHA, target, vault and configuration. Direct external rebuilds of
the code index are outside this checkpoint: remove that one derived checkpoint
after such a rebuild to require fresh acknowledgement.

Inspect `~/.local/state/mesh-code-index.log` and the service's exit status. A stale
lock is never stolen based only on age: confirm its process is gone, then remove
the empty, exact `<git-common-dir>/mesh-code-reindex.lock` directory. Network
authentication is noninteractive. Connection failure detection is configured,
but an uninterruptible command can still retain the single-flight lock.

Activation changes a local service and advances the explicitly configured derived
checkout. Keep it separate from code review/testing and obtain operator approval.
Rollback: unload the catch-up service and restore its saved helper/previous Mesh
binary using the normal graceful owner handoff. Notes and source are not deleted.

Tests: `scripts/test-mesh-code-refresh.sh` exercises real throwaway Git repositories
and a stubbed acknowledgement. Set `MESH_TEST_BINARY` to a freshly built binary to
also run the complete remote-merge to live-owner to searchable-symbol journey.
`go test ./internal/index ./cmd/mesh -run
'^TestCodeRefresh'` exercises real owner routing, receipts and source queries.
