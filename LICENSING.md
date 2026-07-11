# Licensing

Mesh is **open core**, dual-licensed.

## The open core (this repository)

Everything in the public `github.com/bright-interaction/mesh` repository is
licensed under the **Mesh Sustainable Use License** (see `LICENSE`), a "fair-code"
license (https://faircode.io).

Copyright the Mesh authors and Bright Interaction AB.

Fair-code means the source is open and free to read, run, and modify. You may run
Mesh on your own hardware, use it internally and commercially, run it for your own
clients' knowledge and context, and redistribute the source with this license. The
one limit that matters in practice: **you may not resell Mesh itself or run it as a
hosted service for third parties** (a competing "Mesh cloud"). That commercial case
needs a separate license.

The open core is a complete, sovereign, single-user (and hosted-hub client) tool:
the markdown vault, the graph + community detection, the MCP retrieval surface, the
2D/3D viewers, the CLI, the live watcher, and the sync **client** (`mesh join` /
`mesh sync`) so you can join a team hub.

## The commercial / pro layer (not in this repository)

The **team-sync hub server** and the **BYOAI sync-curator**, plus pro
collaboration and large-scale features, are a separate commercial product. They
are not published here. They are available two ways:

- **Hosted** at mesh.brightinteraction.com (managed, backed up, scaled by us).
- **Self-host commercial license** for sovereign / regulated orgs that need to run
  the hub on their own infrastructure with a support + SLA contract.

## Commercial license for the core

If the Mesh Sustainable Use License does not fit your use (for example you want to
offer Mesh to third parties as a hosted or managed service, or embed it in a product
you resell), a commercial license to the core is available. Contact Bright Interaction.

## Contributing

Contributions to the open core are welcome under a Contributor License Agreement
that lets Bright Interaction continue to offer the dual (Mesh Sustainable Use
License + commercial) license.
By opening a PR you agree your contribution may be distributed under both. The full
terms are in [`CONTRIBUTING.md`](CONTRIBUTING.md).
