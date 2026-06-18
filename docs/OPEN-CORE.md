# Open-core architecture

Mesh is split into a public AGPL core and a private commercial pro layer. This doc
is the source of truth for what lives where and how the public mirror is produced.

## The boundary

The split is a pure package boundary: **nothing in the core imports the pro
packages**, so the core builds and ships on its own. Verified by the import graph
(`go list`): the pro packages are only imported by their own binaries.

### Open core (public repo, AGPL-3.0)

The whole single-user + hub-client experience:

- `cmd/mesh`: the CLI (`index`, `search`, `mcp`, `tui`, `ui`, `watch`, `join`, `sync`, `doctor`, `tune`, `migrate`).
- `internal/vault`, `internal/index`, `internal/graph`: parse, the in-memory graph, Louvain communities.
- `internal/retrieve`, `internal/rerank`, `internal/embed`, `internal/tokenize`: fused FTS + graph-BM25 + BYOAI vectors/rerank retrieval. The open core uses brute-force cosine for the vector signal (sub-5ms well past v1 scale).
- `internal/mcp`: the agent retrieval + write-back surface (read tools, `mesh_append_note`/`mesh_write_entity`, `mesh_reindex`).
- `internal/web`, `internal/tui`, `internal/sshserve`: the 2D/3D viewers, the terminal UI, and the SSH viewer (`mesh serve-ssh`: the TUI over SSH, key-auth, fail-closed).
- `internal/merge`, `internal/syncproto`, `pkg/meshclient`: the sync **client** and wire protocol, so the open core can join a hub.
- `internal/meshcfg`, `internal/eval`, `internal/textdiff`: config, eval harness, diff.

### Pro layer (private, commercial, NOT published)

The collaboration server and the AI layer, the monetized value:

- `internal/hub`, `cmd/mesh-hub`: the team-sync hub server (the durable moat: a managed/licensed service, not code value).
- `internal/curator`, `cmd/mesh-curator`, `internal/llm`: the BYOAI sync-curator (AI conflict merge).
- `internal/hnsw` + `internal/retrieve/retrieve_ann_pro.go`: the HNSW ANN index, the "1000+ vectors" scale gate. The core has a build-tag seam (`annSearcher`/`buildANN`, nil in the open build) so it compiles brute-force-only; the pro build wires HNSW with `-tags pro`. This is gated by ABSENCE (the impl is stripped from the mirror), not a removable flag.
- Future: cross-vault federation, team-scale collaboration analytics, pro graph functions. These belong here, behind the service/license, not as a flag in the open binary.

Authoritative exclude set for the public mirror:

```
internal/hub  cmd/mesh-hub  internal/curator  cmd/mesh-curator  internal/llm
internal/hnsw  internal/retrieve/retrieve_ann_pro.go  internal/retrieve/retrieve_ann_pro_test.go
```

The pro binaries build with `-tags pro` (wires HNSW); the open mirror builds with the default tags (brute-force only).

## How the public mirror is produced

The private monorepo (`automations/mesh`, this tree) is the source of truth and the
build source for the hosted SaaS + licensed binaries. The public AGPL repo is a
history-filtered mirror of the core:

1. `scripts/split-public-repo.sh` runs `git subtree split --prefix=mesh`, then
   `git filter-repo --invert-paths` to strip the pro paths from **all** history (so
   pro code never appears in the AGPL repo, not even in old commits).
2. It build-checks the filtered tree (`go build ./...`) to prove the core compiles
   without the pro layer.
3. Dry-run by default; `--push` mirrors to `github.com/bright-interaction/mesh`.

See `docs/RELEASING.md` for the operator steps (repo creation, tag).

## Revenue model

- **Free / clout:** the AGPL core. Individuals, OSS, top-of-funnel.
- **Hosted team hub** (primary, build first): subscription, low-friction funnel from
  the free core via `mesh join`.
- **Sovereign self-host license:** the hub + curator on the customer's own infra
  with support + SLA, for EU / regulated buyers (Bright Interaction's lane).

AGPL on the core deters a competitor from running a closed, rehosted Mesh; the dual
commercial license is how customers who can't accept AGPL pay instead.
