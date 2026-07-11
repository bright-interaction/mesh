# Open-core architecture

Mesh is split into a public fair-code core and a private commercial pro layer. This
doc is the source of truth for what lives where and how the public mirror is produced.

## The boundary

The split is a pure package boundary: **nothing in the open core imports the pro
packages**, so the core builds and ships on its own. This is enforced, not assumed:
`scripts/check-open-core-boundary.sh` walks the open build's import graph (`go list
-deps`) and fails if any open package imports a pro package. It runs as a pre-flight
gate in `split-public-repo.sh` and in the repo pre-commit hook, so the boundary
cannot silently rot (it did once, 2026-06-30: the flywheel features pulled in
`internal/llm` and `mesh ui --hub-db` pulled in `internal/hub`; see the fix below).

### Open core (public repo, Mesh Sustainable Use License)

The whole single-user + hub-client experience:

- `cmd/mesh`: the CLI (`index`, `search`, `mcp`, `tui`, `ui`, `watch`, `join`, `sync`, `doctor`, `tune`, `migrate`).
- `internal/vault`, `internal/index`, `internal/graph`: parse, the in-memory graph, Louvain communities.
- `internal/retrieve`, `internal/rerank`, `internal/embed`, `internal/tokenize`: fused FTS + graph-BM25 + BYOAI vectors/rerank retrieval. The open core uses brute-force cosine for the vector signal (sub-5ms well past v1 scale).
- `internal/llm`: the BYOAI chat boundary (a stdlib-only client shim over `claude -p` / the Anthropic Messages API / any OpenAI-compatible endpoint). It is open because it is a thin client with no defensible moat, and the flywheel features built on it are the open product.
- `internal/ask`, `internal/extract`, `internal/guards`: the BYOAI flywheel - grounded Q&A over notes+code (`mesh ask`), auto-extraction of write-back candidates (`mesh extract` + the Review queue), and gotcha pre-commit guards. All use `internal/llm`.
- `internal/mcp`: the agent retrieval + write-back surface (read tools, `mesh_append_note`/`mesh_write_entity`, `mesh_reindex`).
- `internal/web`, `internal/tui`, `internal/sshserve`: the 2D/3D viewers, the terminal UI, and the SSH viewer (`mesh serve-ssh`: the TUI over SSH, key-auth, fail-closed).
- `internal/merge`, `internal/syncproto`, `pkg/meshclient`: the sync **client** and wire protocol, so the open core can join a hub.
- `internal/meshcfg`, `internal/eval`, `internal/textdiff`: config, eval harness, diff.

### Pro layer (private, commercial, NOT published)

The collaboration server and the AI layer, the monetized value:

- `internal/hub`, `cmd/mesh-hub`: the team-sync hub server (the durable moat: a managed/licensed service, not code value).
- `internal/curator`, `cmd/mesh-curator`: the BYOAI sync-curator (AI conflict merge). It imports the open `internal/llm` - pro importing open is fine.
- `internal/hnsw` + `internal/retrieve/retrieve_ann_pro.go`: the HNSW ANN index, the "1000+ vectors" scale gate. The core has a build-tag seam (`annSearcher`/`buildANN`, nil in the open build) so it compiles brute-force-only; the pro build wires HNSW with `-tags pro`. This is gated by ABSENCE (the impl is stripped from the mirror), not a removable flag.
- `cmd/mesh/ui_hubteam_pro.go`: the team-mode wiring for `mesh ui --hub-db`, which reads the pro hub store. Same build-tag seam as HNSW: the open core ships `cmd/mesh/ui_hubteam_stub.go` (an `openHubTeam` that refuses team mode with a "needs the pro build" error), and this pro file - the only `cmd/mesh` file importing `internal/hub` - is stripped from the mirror. Plain `mesh ui` (solo vault) stays fully open.
- `cmd/mesh/conflicts_test.go` and the `pkg/meshclient` hub-harness tests (`e2e_test.go`, `events_test.go`, `tombstone_test.go`): integration tests that stand up a real hub to exercise the open sync client and conflicts command, so they need the pro hub and are stripped with it. The meshclient set strips together because `e2e_test.go` defines the `setupHub` helper the other two reuse. The production code they cover ships open; the private monorepo runs the full suite. (`pkg/meshclient/vault_test.go` is hub-free and stays in the mirror.)
- Future: cross-vault federation, team-scale collaboration analytics, pro graph functions. These belong here, behind the service/license, not as a flag in the open binary.

Authoritative exclude set for the public mirror (keep in sync with `split-public-repo.sh` `PRO_PATHS` and `check-open-core-boundary.sh`):

```
internal/hub  cmd/mesh-hub  internal/curator  cmd/mesh-curator
internal/hnsw  internal/retrieve/retrieve_ann_pro.go  internal/retrieve/retrieve_ann_pro_test.go
cmd/mesh/ui_hubteam_pro.go
cmd/mesh/conflicts_test.go
pkg/meshclient/e2e_test.go  pkg/meshclient/events_test.go  pkg/meshclient/tombstone_test.go
```

`internal/llm` is NOT excluded - it is open core (see the open-core list above).

The split script also strips a few **internal-only docs** (not code): the milestone
working-plans (`docs/S1-PLAN.md`, `docs/S2-PLAN.md`, `docs/M3-PLAN.md`), the deployment
runbook (`deploy/DEPLOY.md`), and the full internal spec (`docs/SPEC.md`, which cross-
references other Bright Interaction projects). They describe Bright Interaction's own
infrastructure and monorepo rather than the project, so they stay private; the public
architecture story lives in `README.md` and the in-app docs (`internal/web/docs`).

The pro binaries build with `-tags pro` (wires HNSW + the hub team-mode impl); the open mirror builds with the default tags (brute-force only, team mode stubbed out).

## How the public mirror is produced

The private monorepo (`automations/mesh`, this tree) is the source of truth and the
build source for the hosted SaaS + licensed binaries. The public fair-code repo is a
history-filtered mirror of the core:

1. `scripts/split-public-repo.sh` runs `git subtree split --prefix=mesh`, then
   `git filter-repo --invert-paths` to strip the pro paths from **all** history (so
   pro code never appears in the fair-code repo, not even in old commits).
2. It build-checks the filtered tree (`go build ./...`) to prove the core compiles
   without the pro layer.
3. Dry-run by default; `--push` mirrors to `github.com/bright-interaction/mesh`.

See `docs/RELEASING.md` for the operator steps (repo creation, tag).

## Revenue model

- **Free / clout:** the fair-code core. Individuals, OSS, top-of-funnel.
- **Hosted team hub** (primary, build first): subscription, low-friction funnel from
  the free core via `mesh join`.
- **Sovereign self-host license:** the hub + curator on the customer's own infra
  with support + SLA, for EU / regulated buyers (Bright Interaction's lane).

The fair-code license keeps the core free to self-host, use internally or
commercially, and run for your own clients, but bars reselling it as a hosted
service; the dual commercial license is how a buyer who wants to offer Mesh as a
hosted product (or otherwise needs terms the fair-code license does not grant) pays
instead.
