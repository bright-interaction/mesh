// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package syncproto holds the JSON wire types for the Mesh sync protocol, shared
// by the hub (internal/hub) and the client (pkg/meshclient) so there is one
// definition of the contract. Content travels base64-encoded so arbitrary note
// bytes survive JSON.
package syncproto

// JoinRequest redeems a one-time invite for a client token.
type JoinRequest struct {
	Invite string `json:"invite"`
}

// JoinResponse returns the long-lived client token.
type JoinResponse struct {
	ClientToken string `json:"client_token"`
	User        string `json:"user"`
	VaultID     string `json:"vault_id"`
}

// VaultInfo is the metadata a client needs to bootstrap or verify a vault.
type VaultInfo struct {
	VaultID       string `json:"vault_id"`
	HeadSHA       string `json:"head_sha"`
	MeshToml      string `json:"mesh_toml"`
	GCHorizonDays int    `json:"gc_horizon_days"`
	ServerTime    int64  `json:"server_time"`
}

// OutboxItem is one local change a client pushes.
type OutboxItem struct {
	Path       string `json:"path"`
	Op         string `json:"op"` // "upsert" | "delete"
	ContentB64 string `json:"content_b64,omitempty"`
}

// SyncRequest is one pull-based reconcile round.
type SyncRequest struct {
	BaseSHA      string       `json:"base_sha"`
	Outbox       []OutboxItem `json:"outbox"`
	TombstoneSeq int64        `json:"tombstone_seq,omitempty"` // client's high-water delete seq (0 = none seen)
}

// Delta is one change the hub sends back for the client to apply.
type Delta struct {
	Path       string `json:"path"`
	Op         string `json:"op"` // "upsert" | "delete"
	ContentB64 string `json:"content_b64,omitempty"`
}

// Conflict reports that Path could not auto-merge; the client's losing version
// was parked at SiblingPath (delivered among the deltas).
type Conflict struct {
	Path        string `json:"path"`
	SiblingPath string `json:"sibling_path"`
}

// SyncResponse is the reconcile result: the new HEAD, the deltas the client is
// missing relative to its base, and any conflicts. FullReconcile is set when the
// client's base was empty or unknown, so deltas carry the whole vault snapshot.
type SyncResponse struct {
	HeadSHA       string     `json:"head_sha"`
	Deltas        []Delta    `json:"deltas"`
	Conflicts     []Conflict `json:"conflicts"`
	FullReconcile bool       `json:"full_reconcile"`
	// Tombstones is the drop-list sent ONLY on a full reconcile (base empty/unknown):
	// paths the client must delete because they were removed while it was away. On a
	// full reconcile the deltas carry the live snapshot as upserts but no deletes, so
	// without this a stale client would resurrect since-deleted notes.
	Tombstones []string `json:"tombstones,omitempty"`
	// TombstoneSeq is the hub's current delete high-water mark; the client persists it
	// and sends it back as SyncRequest.TombstoneSeq.
	TombstoneSeq int64 `json:"tombstone_seq,omitempty"`
	// Rejected lists outbox paths the hub refused to accept because the client lacks
	// write permission (viewer role, or a read-only folder ACL). The client keeps its
	// local copy; the edit simply did not land upstream. Older clients ignore this.
	Rejected []string `json:"rejected,omitempty"`
}

// CurationJob is a hub-recorded marker that a path had a true conflict and would
// benefit from the BYOAI sync-curator (S2.1). The hub stays AI-free: it only
// records the marker (incl. the losing incoming bytes captured at merge time) and
// serves it; the standalone mesh-curator does the AI and commits back via the
// normal sync path. IncomingB64 is the loser; the winner is read from HeadSHA.
type CurationJob struct {
	ID           int64  `json:"id"`
	Path         string `json:"path"`
	BaseSHA      string `json:"base_sha"`
	HeadSHA      string `json:"head_sha"`
	IncomingB64  string `json:"incoming_b64,omitempty"`
	User         string `json:"user"`
	Status       string `json:"status"`
	Attempts     int64  `json:"attempts,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	ResolvedAt   int64  `json:"resolved_at,omitempty"`
	ResolvedHead string `json:"resolved_head,omitempty"`
}

// CurationJobsResponse lists pending curation jobs (metadata only; fetch one job
// to get its IncomingB64).
type CurationJobsResponse struct {
	Jobs []CurationJob `json:"jobs"`
}
