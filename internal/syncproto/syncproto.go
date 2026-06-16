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
	BaseSHA string       `json:"base_sha"`
	Outbox  []OutboxItem `json:"outbox"`
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
}
