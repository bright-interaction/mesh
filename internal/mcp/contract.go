package mcp

// contractText is the agent-usage contract: how any agent should retrieve from
// Mesh cheaply. Served as the initialize instructions and the mesh://contract
// resource.
const contractText = `Mesh retrieval contract (optimize for tokens):

1. ORIENT: call mesh_god_nodes for the map - the most-connected notes are the entry points.
2. SEARCH: call mesh_search with your query and a token budget. It fuses full-text + graph proximity, expands one hop along links, and surfaces decisions/gotchas/post-mortems first ([tier-0]). It returns ranked cards (title, path, snippet, reason, tier0) and packs the best bundle that fits the budget. Reason over the cards; do NOT read whole files yet.
3. FETCH: only when a card is not enough, call mesh_fetch(id) for the full note, or mesh_fetch(id, anchor) for one heading section.
4. DELTAS: on resume, mesh_changed_since(unix) returns only notes modified since a timestamp.
5. WRITE BACK (the flywheel): when you finish work, call mesh_append_note with type decision|gotcha|post-mortem and one-line do/dont/why so the next agent inherits what you learned. Mesh fills id, timestamp, placement, and filename - you supply only judgment. Use mesh_write_entity for a system/tool/concept page.

Always prefer cards over full fetches, and always write back non-obvious decisions and gotchas.`
