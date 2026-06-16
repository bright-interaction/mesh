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
	ConflictSiblings []string
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

// applyDeltas writes or removes files per the hub's response. Each write is
// atomic (temp + rename) so a crash mid-sync never leaves a torn note. Whole-
// batch atomicity + the external-editor re-stat guard land in S1.4.
func applyDeltas(vaultDir string, deltas []syncproto.Delta) error {
	for _, d := range deltas {
		abs := filepath.Join(vaultDir, filepath.FromSlash(d.Path))
		if d.Op == "delete" {
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		b, err := base64.StdEncoding.DecodeString(d.ContentB64)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(abs, b); err != nil {
			return err
		}
	}
	return nil
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
	outbox, _, err := computeOutbox(vaultDir, state.Hashes)
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
	if err := applyDeltas(vaultDir, resp.Deltas); err != nil {
		return Summary{}, err
	}
	// Recompute hashes from disk so the next outbox reflects the canonical (post-
	// merge) hub state, not what we optimistically pushed.
	_, current, err := computeOutbox(vaultDir, map[string]string{})
	if err != nil {
		return Summary{}, err
	}
	if err := writeState(vaultDir, syncState{HeadSHA: resp.HeadSHA, Hashes: current}); err != nil {
		return Summary{}, err
	}
	sum := Summary{Pushed: len(outbox), Pulled: len(resp.Deltas), Conflicts: len(resp.Conflicts), Head: resp.HeadSHA}
	for _, c := range resp.Conflicts {
		sum.ConflictSiblings = append(sum.ConflictSiblings, c.SiblingPath)
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
