package ingest

import (
	"encoding/json"
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

func saveState(vaultRoot string, st ingestState) error {
	if err := os.MkdirAll(filepath.Join(vaultRoot, ".mesh"), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(vaultRoot), b, 0o644)
}
