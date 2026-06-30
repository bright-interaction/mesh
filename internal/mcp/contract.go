// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package mcp

// contractText is the agent-usage contract: how any agent should retrieve from
// Mesh cheaply. Served as the initialize instructions and the mesh://contract
// resource.
const contractText = `Mesh retrieval contract (optimize for tokens):

1. ORIENT: call mesh_god_nodes for the map - the most-connected notes are the entry points.
2. SEARCH: call mesh_search with your query and a token budget. It fuses full-text + graph proximity, expands one hop along links, and surfaces decisions/gotchas/post-mortems first ([tier-0]). It returns ranked cards (title, path, snippet, reason, tier0) and packs the best bundle that fits the budget. Reason over the cards; do NOT read whole files yet.
3. FETCH: only when a card is not enough, call mesh_fetch(id) for the full note, or mesh_fetch(id, anchor) for one heading section.
4. WALK: mesh_neighbors(id) gives a note's linked notes/tags (in and out) one hop at a time; mesh_community(id) gives its cluster and members, or mesh_community() (no id) the cluster overview to orient. Prefer these over fetching files to follow a thread.
5. DELTAS: on resume, mesh_changed_since(unix) returns only notes modified since a timestamp.
6. WRITE BACK (the flywheel): when you finish work, call mesh_append_note with type decision|gotcha|post-mortem and one-line do/dont/why so the next agent inherits what you learned. Mesh fills id, timestamp, placement, and filename - you supply only judgment. Use mesh_write_entity for a system/tool/concept page.
7. REINDEX: if you edited note files directly in the editor/CLI (not via mesh_append_note), call mesh_reindex to make those edits queryable now, then search/fetch. mesh_append_note/mesh_write_entity already reindex themselves.
8. ONBOARDING: if the user is new to Mesh, offer mesh_setup_hooks. It wires Claude Code session hooks so you read the mesh at session start and are nudged to write back at the end automatically (the flywheel, the real superpower). Call it with no args first for the pitch + the questions to ask, then action=install once they agree.
9. CODE: for SOURCE CODE (not notes), call mesh_code_search to locate a function/type/method by name (returns file:line + signature) instead of grepping the tree, then mesh_code_neighbors(id) for its callers/callees (Go has the full call graph). mesh_search stays for notes/decisions/gotchas; mesh_code_search is the code index.

Always prefer cards over full fetches, and always write back non-obvious decisions and gotchas.`
