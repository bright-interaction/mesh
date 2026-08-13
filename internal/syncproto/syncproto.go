// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package syncproto holds the JSON wire types for the Mesh sync protocol, shared
// by the hub (internal/hub) and the client (pkg/meshclient) so there is one
// definition of the contract. Content travels base64-encoded so arbitrary note
// bytes survive JSON.
package syncproto

// Protocol versioning. The wire contract used to carry no version at all, so the
// hub could not tell a current client from one that predates a RESPONSE field and
// would silently mishandle it. That is not hypothetical: a client that ignores
// SyncResponse.Rejected records hub-refused bytes as its synced base, so the edit
// is lost upstream forever with no error and no retry. Every request now declares
// the protocol it speaks, and the hub refuses (rather than silently degrades) when
// the response semantics need more than the client declared.
//
// Bump ProtoVersion whenever a new RESPONSE field changes what a client must do;
// add a Proto* constant naming the first version that honors it, and gate on that
// constant hub-side rather than on ProtoVersion, so old clients keep working for
// every response that does not need the new field.
const (
	// ProtoVersion is the protocol this build speaks. A request carrying 0 is a
	// pre-versioning client (the field did not exist).
	ProtoVersion = 1
	// ProtoRejected is the first version whose client honors SyncResponse.Rejected
	// by keeping the refused path dirty instead of baselining the un-landed bytes.
	ProtoRejected = 1
	// ProtoHeader carries ProtoVersion on every request for the hub's audit log,
	// so a stale client is visible in the logs even on endpoints with no body.
	ProtoHeader = "X-Mesh-Proto"
)

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

// PATH CONTRACT, both directions. Every Path on this wire (OutboxItem, Delta,
// Conflict, Tombstone, CurationJob) is a vault-relative, forward-slash path to a
// MARKDOWN NOTE, or one of two hub-owned files at the vault root: "mesh.toml" and
// ".gitattributes". No absolute path, no "..", no backslash, no reserved or hidden
// directory (.mesh, .git, .claude, ...), no other extension.
//
// Both ends enforce it independently through vault.SafeSyncPath, because each end
// is the other's untrusted input: a hostile or MITM'd hub can put anything in a
// Delta, and any teammate with the write role (which is every member until an ACL
// says otherwise) can put anything in an Outbox. A path outside the contract is
// refused, and on the push side it comes back in SyncResponse.Rejected rather than
// being dropped in silence.
//
// The rule is derived from what vault.Walk indexes, which is why it is this tight:
// a file the walker never returns can never be re-indexed or re-pushed by the
// client that received it, so landing one is a permanent one-way write onto
// someone else's disk. The dangerous instances are exactly the useful ones for an
// attacker: .mesh/config.toml (its rerank endpoint makes every later search POST
// the query and the matching notes to a URL of their choosing) and
// .claude/settings.json (a hook command run by the teammate's agent).

// OutboxItem is one local change a client pushes. Path must satisfy the path
// contract above; the hub refuses anything else and names it in Rejected.
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
	// Proto is the protocol version the client speaks (see ProtoVersion). 0 means a
	// pre-versioning client: the hub must assume it understands nothing added after
	// the field appeared, and refuse the round rather than send a response it would
	// mishandle.
	Proto int `json:"proto,omitempty"`
}

// Delta is one change the hub sends back for the client to apply. Path must
// satisfy the path contract above; the client skips anything else and logs it,
// rather than failing the round, so one bad entry cannot wedge a sync.
type Delta struct {
	Path       string `json:"path"`
	Op         string `json:"op"` // "upsert" | "delete"
	ContentB64 string `json:"content_b64,omitempty"`
}

// Conflict reports that Path could not auto-merge. The hub keeps its own version
// live at Path and only NAMES the sibling: siblings are per-user resolution
// artifacts that never enter the hub repo, so the client writes its own losing
// bytes to SiblingPath locally. SiblingPath therefore always carries the
// ".sync-conflict-" marker, and the client refuses it if it does not (otherwise
// the field would be a write-anywhere primitive for a hostile hub).
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
	// Rejected lists outbox paths the hub refused to accept: the client lacks write
	// permission (viewer role, or a read-only folder ACL), the note is too large or
	// not text, or the path is outside the path contract above. The client keeps its
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
