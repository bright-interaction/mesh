package meshclient

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/vault"
)

// credentials and sync state live under <vault>/.mesh, which is git-ignored and
// never itself synced. credentials is mode 0600 (it holds the bearer token).
type credentials struct {
	HubURL string `json:"hub_url"`
	Token  string `json:"token"`
}

type syncState struct {
	HeadSHA string            `json:"head_sha"`
	Hashes  map[string]string `json:"hashes"` // vault-relative path -> content sha
}

func credPath(vaultDir string) string  { return filepath.Join(vaultDir, ".mesh", "credentials") }
func statePath(vaultDir string) string { return filepath.Join(vaultDir, ".mesh", "sync.json") }

func writeCredentials(vaultDir string, c credentials) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, ".mesh"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(credPath(vaultDir), b, 0o600)
}

func readCredentials(vaultDir string) (credentials, error) {
	var c credentials
	b, err := os.ReadFile(credPath(vaultDir))
	if err != nil {
		return c, fmt.Errorf("not joined to a hub (no .mesh/credentials); run: mesh join <hub-url> <invite>")
	}
	return c, json.Unmarshal(b, &c)
}

func writeState(vaultDir string, s syncState) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, ".mesh"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(statePath(vaultDir), b, 0o600)
}

func readState(vaultDir string) syncState {
	s := syncState{Hashes: map[string]string{}}
	if b, err := os.ReadFile(statePath(vaultDir)); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Hashes == nil {
		s.Hashes = map[string]string{}
	}
	return s
}

// Summary reports what a sync round did, for the CLI.
type Summary struct {
	Pushed           int
	Pulled           int
	Conflicts        int
	Head             string
	ConflictSiblings []string // merge conflicts: our pushed version parked here
	Protected        []string // external-editor race: incoming hub version parked here
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// computeOutbox diffs the vault's markdown files on disk against the last-synced
// hashes, returning the changes to push plus the current on-disk hash map.
func computeOutbox(vaultDir string, prev map[string]string) ([]syncproto.OutboxItem, map[string]string, error) {
	files, err := vault.Walk(vaultDir)
	if err != nil {
		return nil, nil, err
	}
	current := map[string]string{}
	var outbox []syncproto.OutboxItem
	for _, f := range files {
		rel, err := filepath.Rel(vaultDir, f)
		if err != nil {
			rel = f
		}
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		h := contentHash(b)
		current[rel] = h
		if prev[rel] != h {
			outbox = append(outbox, syncproto.OutboxItem{Path: rel, Op: "upsert", ContentB64: base64.StdEncoding.EncodeToString(b)})
		}
	}
	for rel := range prev {
		if _, ok := current[rel]; !ok {
			outbox = append(outbox, syncproto.OutboxItem{Path: rel, Op: "delete"})
		}
	}
	return outbox, current, nil
}

// park records a path whose incoming hub version was set aside (sibling) because
// the local file changed during the sync window.
type park struct {
	note    string // the original path whose local change we kept
	sibling string // where the incoming hub version was parked
}

// applyDeltas writes or removes files per the hub's response, guarding against
// the external-editor race (SPEC 6.6): sentHashes is the on-disk state captured
// when the outbox was computed, so if a path changed on disk SINCE then a local
// edit OR delete landed during the sync window. In that case the incoming hub
// version is parked in a sibling and the local change is kept; SyncVault then
// keeps the path "dirty" so the local change re-pushes next sync (it is not
// silently dropped). Each write is atomic (temp + rename); a partial-batch
// failure self-heals because the base is not advanced. Returns the parked paths.
func applyDeltas(vaultDir string, deltas []syncproto.Delta, sentHashes map[string]string) ([]park, error) {
	var parked []park
	for _, d := range deltas {
		abs := filepath.Join(vaultDir, filepath.FromSlash(d.Path))
		onDisk, readErr := os.ReadFile(abs)
		sentHash, wasSent := sentHashes[d.Path]
		// A local change during the window is either an edit (present but
		// different) or a delete (absent now, but present at send time).
		locallyChanged := (readErr == nil && contentHash(onDisk) != sentHash) ||
			(readErr != nil && wasSent)

		if d.Op == "delete" {
			if locallyChanged {
				continue // local edit/recreate after send: keep it, skip the delete
			}
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return parked, err
			}
			continue
		}

		b, err := base64.StdEncoding.DecodeString(d.ContentB64)
		if err != nil {
			return parked, err
		}
		if locallyChanged && contentHash(onDisk) != contentHash(b) {
			// External-editor race: park the incoming version, keep the local
			// change (a local delete keeps the path absent; a local edit keeps it).
			sib := merge.SiblingPath(d.Path, time.Now(), "hub", b)
			if err := writeFileAtomic(filepath.Join(vaultDir, filepath.FromSlash(sib)), b); err != nil {
				return parked, err
			}
			parked = append(parked, park{note: d.Path, sibling: sib})
			continue
		}
		if err := writeFileAtomic(abs, b); err != nil {
			return parked, err
		}
	}
	return parked, nil
}

// keepParkedDirty rewrites current so each guard-parked path keeps its pre-sync
// base hash instead of its (just-applied) disk hash, so the next computeOutbox
// detects the kept local change and re-pushes it (SPEC 6.6: enqueue the local
// change). Without this the local change would be recorded as synced and lost.
func keepParkedDirty(current, base map[string]string, parked []park) {
	for _, p := range parked {
		if old, ok := base[p.note]; ok {
			current[p.note] = old
		} else {
			delete(current, p.note)
		}
	}
}

// writeConflictSiblings preserves the client's losing version of each conflicted
// path in a local sibling BEFORE applyDeltas overwrites the path with the hub's
// winning version. Siblings are local resolution artifacts, never pushed.
func writeConflictSiblings(vaultDir string, conflicts []syncproto.Conflict) error {
	for _, cf := range conflicts {
		local, err := os.ReadFile(filepath.Join(vaultDir, filepath.FromSlash(cf.Path)))
		if err != nil {
			continue // nothing local to preserve (e.g. we deleted it)
		}
		if err := writeFileAtomic(filepath.Join(vaultDir, filepath.FromSlash(cf.SiblingPath)), local); err != nil {
			return err
		}
	}
	return nil
}

// writeFileAtomic writes b to path via a temp file in the same directory then
// rename, so a reader never sees a partially written note.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mesh-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil {
		os.Remove(tmpName)
		return werr
	}
	if cerr != nil {
		os.Remove(tmpName)
		return cerr
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// SyncVault runs one reconcile round against the joined hub: push local edits,
// apply the hub's deltas, and persist the new base. It does not reindex; the
// caller (cmd/mesh) runs index.Reconcile afterward.
func SyncVault(vaultDir string) (Summary, error) {
	creds, err := readCredentials(vaultDir)
	if err != nil {
		return Summary{}, err
	}
	state := readState(vaultDir)
	outbox, sentHashes, err := computeOutbox(vaultDir, state.Hashes)
	if err != nil {
		return Summary{}, err
	}
	resp, err := New(creds.HubURL, creds.Token).Sync(syncproto.SyncRequest{BaseSHA: state.HeadSHA, Outbox: outbox})
	if err != nil {
		return Summary{}, err
	}
	// Preserve our losing versions locally before deltas overwrite the paths.
	if err := writeConflictSiblings(vaultDir, resp.Conflicts); err != nil {
		return Summary{}, err
	}
	parked, err := applyDeltas(vaultDir, resp.Deltas, sentHashes)
	if err != nil {
		return Summary{}, err
	}
	// Recompute hashes from disk so the next outbox reflects the canonical (post-
	// merge) hub state, not what we optimistically pushed; then keep any
	// guard-parked path dirty so its kept local change re-pushes next sync.
	_, current, err := computeOutbox(vaultDir, map[string]string{})
	if err != nil {
		return Summary{}, err
	}
	keepParkedDirty(current, state.Hashes, parked)
	if err := writeState(vaultDir, syncState{HeadSHA: resp.HeadSHA, Hashes: current}); err != nil {
		return Summary{}, err
	}
	sum := Summary{Pushed: len(outbox), Pulled: len(resp.Deltas), Conflicts: len(resp.Conflicts), Head: resp.HeadSHA}
	for _, c := range resp.Conflicts {
		sum.ConflictSiblings = append(sum.ConflictSiblings, c.SiblingPath)
	}
	for _, p := range parked {
		sum.Protected = append(sum.Protected, p.sibling)
	}
	return sum, nil
}

// JoinVault redeems an invite, stores credentials, verifies embedding
// homogeneity against the hub-authoritative mesh.toml, and clones the vault via a
// reconcile from an empty base.
func JoinVault(hubURL, invite, vaultDir string) (Summary, error) {
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		return Summary{}, err
	}
	c := New(hubURL, "")
	jr, err := c.Join(invite)
	if err != nil {
		return Summary{}, err
	}
	if err := writeCredentials(vaultDir, credentials{HubURL: strings.TrimRight(hubURL, "/"), Token: jr.ClientToken}); err != nil {
		return Summary{}, err
	}
	c.Token = jr.ClientToken
	vi, err := c.Vault()
	if err != nil {
		return Summary{}, err
	}
	if err := checkHomogeneity(vi.MeshToml); err != nil {
		return Summary{}, err
	}
	// No local state yet -> base "" -> the hub returns a full snapshot.
	return SyncVault(vaultDir)
}

// checkHomogeneity fails closed if the vault's canonical embedding model (from
// the synced mesh.toml) conflicts with the operator's configured MESH_EMBED_MODEL
// (SPEC 8: one embedding space per team).
func checkHomogeneity(meshToml string) error {
	model := tomlString(meshToml, "model")
	env := strings.TrimSpace(os.Getenv("MESH_EMBED_MODEL"))
	if model != "" && env != "" && model != env {
		return fmt.Errorf("embedding model mismatch: this vault uses %q but MESH_EMBED_MODEL=%q; align them before joining (fail closed)", model, env)
	}
	return nil
}

// tomlString pulls a simple top-level/`section` string value (key = "value")
// from the small, hub-written mesh.toml. Not a general TOML parser.
func tomlString(toml, key string) string {
	for _, line := range strings.Split(toml, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}
