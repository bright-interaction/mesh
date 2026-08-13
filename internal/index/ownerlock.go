// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The single owning writer, named on disk.
//
// Exactly one process may hold a writable Store against a given mesh.db (see
// OpenReadOnly for why). Until this file existed that invariant was maintained by
// convention only: `mesh watch`, `mesh sync --watch` and `mesh ui --own-index` each
// opened a writable store and hoped nothing else had, and `mesh mcp` was left read-only
// forever because it had no way to find out whether it was alone. The result was the
// worst of both: a shipped .mcp.json that could never index anything, and no protection
// against two watchers either.
//
// So ownership is now a claim a process makes, in a file every writer checks:
//
//	<vault>/.mesh/owner.lock   JSON: pid, host, role, preemptible, started_at
//
// The file's own mtime is the heartbeat (os.Chtimes, no rewrite), so a reader never sees
// a half-written claim and this never becomes another byte writer to make durable. The
// contents are written once, at acquisition.
//
// Liveness, in order of confidence:
//   - same host: ask the OS whether the pid is still there (processAlive). A dead owner's
//     lock is reclaimed at once, which is what a crashed watcher leaves behind.
//   - other host (a vault on a shared filesystem) or an OS that cannot answer: the
//     heartbeat. A lock not touched for OwnerLockStaleAfter belongs to nobody.
//
// Preemption exists for one case. `mesh mcp` elects itself opportunistically, because
// the alternative is a user whose only configured surface never indexes. A DECLARED
// owner (`mesh watch`, `mesh sync --watch`, `mesh ui --own-index`) must still win, so it
// takes the lock from a preemptible holder; that holder notices via Held and drops back
// to reader behaviour. Two declared owners still refuse each other, which is the case
// that was never checked at all before.

// OwnerLockName is the lock file's name inside the vault's .mesh directory.
const OwnerLockName = "owner.lock"

const (
	// OwnerHeartbeatInterval is how often a holder touches the lock file so a peer on
	// another host can tell it is still alive.
	OwnerHeartbeatInterval = 5 * time.Second
	// OwnerLockStaleAfter is how long a lock may go untouched before a peer that cannot
	// check the pid directly treats it as abandoned. Generously above the heartbeat so a
	// busy or briefly suspended owner is never declared dead by a scheduling hiccup.
	OwnerLockStaleAfter = 30 * time.Second
)

// OwnerInfo is the claim recorded in the lock file.
type OwnerInfo struct {
	PID  int    `json:"pid"`
	Host string `json:"host"`
	// Role is the command that claimed it, e.g. "mesh watch" or "mesh mcp --watch",
	// so an error can name the process a human has to go and find.
	Role string `json:"role"`
	// Preemptible marks an opportunistic claim (mesh mcp) that a declared owner may take.
	Preemptible bool  `json:"preemptible"`
	StartedAt   int64 `json:"started_at"`
	// Nonce identifies THIS claim, not the process that made it. Held compares it,
	// because pid + host + start time do not distinguish two claims made by one process
	// (a `mesh ui --own-index` inside the same binary as an elected `mesh mcp`, or any
	// test that plays both roles), and a holder that cannot tell it has been preempted
	// goes on writing beside the new owner, which is the whole failure being fixed.
	Nonce string `json:"nonce"`
}

// Describe renders an owner for an operator-facing message.
func (o OwnerInfo) Describe() string {
	role := o.Role
	if role == "" {
		role = "an unknown mesh process"
	}
	if o.PID == 0 {
		return role
	}
	if o.Host == "" {
		return fmt.Sprintf("%s (pid %d)", role, o.PID)
	}
	return fmt.Sprintf("%s (pid %d on %s)", role, o.PID, o.Host)
}

// ErrOwnerHeld means a live writer already owns this vault's index. Callers branch on it
// with errors.Is: `mesh mcp` falls back to read-only, a declared owner reports it.
var ErrOwnerHeld = errors.New("another mesh process already owns this vault's index")

// OwnerHeldError carries who holds it, so the message can name the process instead of
// telling the operator to go looking.
type OwnerHeldError struct{ Info OwnerInfo }

func (e *OwnerHeldError) Error() string {
	return fmt.Sprintf("%v: %s. Stop it, or run this surface read-only beside it",
		ErrOwnerHeld, e.Info.Describe())
}

func (e *OwnerHeldError) Unwrap() error { return ErrOwnerHeld }

// OwnerLock is a held claim. Release it (or exit) to give the vault up.
type OwnerLock struct {
	path string
	info OwnerInfo

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// AcquireOwnerLock claims the single-owning-writer role for this process.
//
// meshDir is the vault's .mesh directory. role names the command for other processes'
// error messages. preemptible=true marks an opportunistic claim a declared owner may
// take (only `mesh mcp` sets it).
//
// It returns an error wrapping ErrOwnerHeld when a live writer already holds the vault.
// Every other error is a real filesystem failure.
func AcquireOwnerLock(meshDir, role string, preemptible bool) (*OwnerLock, error) {
	if err := ensureVaultMeshDir(meshDir); err != nil {
		return nil, err
	}
	path := filepath.Join(meshDir, OwnerLockName)
	host, _ := os.Hostname()
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	me := OwnerInfo{
		PID: os.Getpid(), Host: host, Role: role, Preemptible: preemptible,
		StartedAt: time.Now().Unix(), Nonce: hex.EncodeToString(nonce),
	}
	body, err := json.Marshal(me)
	if err != nil {
		return nil, err
	}
	// Two attempts: the second one runs only after we removed a lock nobody holds, and a
	// peer that raced us to the same conclusion has to lose one of them.
	for attempt := 0; attempt < 2; attempt++ {
		f, oerr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if oerr == nil {
			_, werr := f.Write(body)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				os.Remove(path)
				return nil, errors.Join(werr, cerr)
			}
			l := &OwnerLock{path: path, info: me, stop: make(chan struct{}), done: make(chan struct{})}
			go l.beat()
			return l, nil
		}
		if !errors.Is(oerr, fs.ErrExist) {
			return nil, oerr
		}
		info, live := readOwner(path)
		// A live holder keeps it, unless it is an opportunistic claim and we are the
		// declared owner. Note the asymmetry: preemptible callers never preempt, so two
		// `mesh mcp` servers cannot take the vault from each other in a loop.
		if live && !(info.Preemptible && !preemptible) {
			return nil, &OwnerHeldError{Info: info}
		}
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return nil, rerr
		}
	}
	info, _ := readOwner(path)
	return nil, &OwnerHeldError{Info: info}
}

// OwnerStatus reports who owns the vault's index, and whether that claim is live. It is
// what `mesh doctor`, `mesh status` and mesh_health call: "no owning writer" is a
// FAILURE of the vault's setup (nothing will index the notes you write), and it used to
// be completely invisible.
func OwnerStatus(meshDir string) (OwnerInfo, bool) {
	return readOwner(filepath.Join(meshDir, OwnerLockName))
}

// NoOwnerIssue is the health-finding kind for a vault nothing is indexing. It is a
// FAILURE, not a warning: every note written or edited while it holds stays out of
// search, and the only signal a user got before this existed was mesh_append_note
// eventually reporting owner_down, ten seconds after the fact.
const NoOwnerIssue = "no-owner"

// NoOwnerRemedy is the one wording for that failure, so `mesh doctor`, `mesh status` and
// mesh_health cannot drift into three different pieces of advice. Pass the vault root
// where naming it helps (the CLI); pass "" where the path must not leave the process (a
// remote MCP client), and the commands are printed with a placeholder instead.
func NoOwnerRemedy(vaultRoot string) string {
	arg := " <vault>"
	if vaultRoot != "" {
		arg = " " + vaultRoot
	}
	return "no owning writer detected: nothing is indexing this vault, so notes you write or edit " +
		"stay invisible to search. Fix: point your agent at `mesh mcp --vault" + arg + " --watch`, which " +
		"elects itself the owner when nothing else holds the vault, or run `mesh watch" + arg + "` " +
		"(or `mesh sync --watch" + arg + "` on a team vault) and leave it running."
}

// Info returns the claim this lock recorded.
func (l *OwnerLock) Info() OwnerInfo { return l.info }

// Held reports whether the lock file still names THIS process. A declared owner can take
// a preemptible claim away mid-session, and the demoted holder must stop behaving like
// the owner the moment that happens, so every write path asks this rather than assuming
// that having acquired once means still holding.
func (l *OwnerLock) Held() bool {
	if l == nil {
		return false
	}
	info, _ := readOwner(l.path)
	return info.Nonce != "" && info.Nonce == l.info.Nonce
}

// Release stops the heartbeat and removes the lock file, but only when it still names
// this process: releasing a lock a declared owner has since taken would hand the vault
// to nobody while a perfectly good writer is running.
func (l *OwnerLock) Release() error {
	if l == nil {
		return nil
	}
	l.stopOnce.Do(func() {
		close(l.stop)
		<-l.done
	})
	if !l.Held() {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// beat touches the lock file so a peer that cannot inspect our pid (another host, or an
// OS whose process check cannot answer) can still tell we are alive. It never rewrites
// the contents: mtime carries the signal, so no reader can catch a torn claim.
func (l *OwnerLock) beat() {
	defer close(l.done)
	t := time.NewTicker(OwnerHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			if err := os.Chtimes(l.path, now, now); err != nil {
				// The file is gone (released, or a declared owner took over). Stop
				// beating; Held is what the rest of the process reads anyway.
				if errors.Is(err, fs.ErrNotExist) {
					return
				}
			}
		}
	}
}

// readOwner reads the claim at path and decides whether it is live. A missing file is
// "no owner". An unreadable or unparseable one is treated as live only while it is fresh,
// so a corrupt lock cannot wedge a vault forever, and a partially visible one cannot be
// stolen out from under a healthy owner either.
func readOwner(path string) (OwnerInfo, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return OwnerInfo{}, false
	}
	fresh := time.Since(fi.ModTime()) < OwnerLockStaleAfter
	b, err := os.ReadFile(path)
	if err != nil {
		return OwnerInfo{}, fresh
	}
	var info OwnerInfo
	if err := json.Unmarshal(b, &info); err != nil || info.PID <= 0 {
		return OwnerInfo{}, fresh
	}
	host, _ := os.Hostname()
	if info.Host == host || info.Host == "" {
		if alive, known := processAlive(info.PID); known {
			return info, alive
		}
	}
	return info, fresh
}
