// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
)

// ingestState persists the last successful pull time per connector instance so a
// re-run is incremental. It lives in <vault>/.mesh (a derived, local artifact).
type ingestState struct {
	LastRun map[string]int64 `json:"last_run"` // connector Key() -> unix seconds
}

func statePath(vaultRoot string) string {
	return filepath.Join(vaultRoot, ".mesh", "ingest-state.json")
}

func loadState(vaultRoot string) (ingestState, error) {
	st := ingestState{LastRun: map[string]int64{}}
	b, err := os.ReadFile(statePath(vaultRoot))
	if err != nil {
		return st, nil // absent = first run
	}
	_ = json.Unmarshal(b, &st)
	if st.LastRun == nil {
		st.LastRun = map[string]int64{}
	}
	return st, nil
}

// stateFileMode keeps ingest-state.json owner-only, matching .mesh/credentials and
// .mesh/sync.json beside it. It records a connector Key() per line, and those keys name
// the source systems this vault pulls from, which is not something every uid on the box
// needs. 0644 here was reachable in practice: this file's own MkdirAll used to create
// .mesh at 0755, and MkdirAll is a NO-OP on an existing directory, so whichever writer
// ran first decided the mode for every other writer's files.
const stateFileMode = 0o600

// stateDirMode agrees with pkg/meshclient, which creates the same .mesh directory at
// 0700. Two writers of one directory disagreeing about its mode is how a private file
// ends up in a world-traversable directory: the loser's intent is discarded silently.
const stateDirMode = 0o700

func saveState(vaultRoot string, st ingestState) error {
	dir := filepath.Join(vaultRoot, ".mesh")
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// Temp+rename rather than os.WriteFile, for two reasons. Durability: a torn write
	// here loses the high-water marks and silently re-pulls every window. And repair:
	// os.WriteFile's perm argument applies only when it CREATES the file, so it would
	// leave an existing 0644 ingest-state.json at 0644 forever, whereas the rename
	// replaces the inode and the new one carries stateFileMode.
	tmp, err := os.CreateTemp(dir, ".ingest-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Chmod(stateFileMode)
	serr := tmp.Sync()
	clerr := tmp.Close()
	if err := firstErr(werr, cerr, serr, clerr); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, statePath(vaultRoot)); err != nil {
		os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// syncDir fsyncs a directory so a rename into it survives a power loss. Best effort:
// some filesystems refuse to open a directory for sync, and the state file is derived
// data, so a failure here must not fail the pull that just succeeded.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		slog.Debug("ingest: cannot open dir to fsync", "dir", dir, "err", err)
		return
	}
	if err := d.Sync(); err != nil {
		slog.Debug("ingest: dir fsync failed", "dir", dir, "err", err)
	}
	d.Close()
}
