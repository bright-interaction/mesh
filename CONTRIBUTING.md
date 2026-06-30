# Contributing to Mesh

Thanks for your interest in Mesh. This is the open core (AGPL-3.0); the team-sync
hub and the BYOAI curator are a separate commercial product and live elsewhere (see
[LICENSING.md](LICENSING.md)).

## Getting set up

Mesh is a single Go binary, no cgo.

```
go build ./...
go test ./...
```

Storage is pure-Go `modernc.org/sqlite`; the `.mesh/` index is a derived, deletable
artifact and the markdown is the source of truth. There is nothing else to install.

## Sending a change

1. Open an issue first for anything non-trivial, so we can agree on the shape before
   you spend time on it.
2. Keep the change focused. One concern per pull request.
3. `gofmt` your code and make sure `go build ./...` and `go test ./...` pass.
4. Write a clear commit message: what changed and why.

## Contribution license (important)

Mesh is dual-licensed: AGPL-3.0 for the open core, plus a commercial license for the
hub/curator and for users the AGPL does not fit. For that model to work, every
contribution has to be available under both licenses.

**By opening a pull request, you agree that:**

- Your contribution is licensed to the project and to everyone under the
  **AGPL-3.0-or-later** (inbound = outbound), and
- You grant **Bright Interaction AB** a perpetual, worldwide, non-exclusive,
  royalty-free, irrevocable right to also distribute your contribution under the
  project's separate **commercial license**, and
- You have the right to grant the above (the work is yours, or you have permission to
  contribute it, and it does not knowingly infringe anyone else's rights).

You keep the copyright to your contribution. This grant only lets the project offer
your work under both the AGPL and the commercial license, which is what keeps the open
core sustainable. If you cannot agree to this, please do not open a pull request;
open an issue instead and we will find another way to incorporate the idea.

## Reporting a vulnerability

Please do not file security issues in public. See [SECURITY.md](SECURITY.md).
