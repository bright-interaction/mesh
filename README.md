# Mesh

A sovereign, single-binary knowledge mesh built for one job no existing tool does end to end: feeding a coding agent exactly the institutional memory it needs, on a token budget, and letting that agent write what it learned back so the next run is smarter.

Mesh has no reasoning AI inside it. It is the fast engine (parse, index, graph, retrieve, sync); the agent is the librarian. The full design and the adversarial review that shaped it live in [docs/SPEC.md](docs/SPEC.md).

## Status

Milestone 0, in progress. The v1 cut line is deliberately small:

- **v1 = Milestone 0 + 1 + L.** Migrate and index a markdown vault, serve a token-cheap MCP retrieval contract with tier-0 boosting, run the flywheel locally, and prove the token win on a measurement harness. Label-propagation communities.
- **Deferred:** vectors (M-V), TUI + WebGL viewer (M3), team sync hub (M-S).

### Working today

```
go run ./cmd/mesh index <vault> --dry-run
```

Parses every markdown file, builds the deterministic graph (frontmatter, `[[wikilinks]]`, `#tags`, headings), and prints node/edge stats plus issues (missing ids, broken links). Node identity is the frontmatter `id`, never the file path, so a rename never rots an edge or an agent citation.

## Build

```
go build ./...
go test ./...
```

No cgo. Storage is pure-Go `modernc.org/sqlite` in WAL mode (lands with the store step).
