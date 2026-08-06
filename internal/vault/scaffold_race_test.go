// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// CreateNote used to pick a free path with os.Stat and then write it with os.WriteFile.
// Under any concurrent caller (mesh mcp --http, the hub's /mcp, the web pending API) two
// write-backs with the same title both saw the base path free, both rendered the same id,
// and the second truncated the first. Every caller got a success receipt; only one note
// survived. For a knowledge store whose whole flywheel is agents writing back what they
// learned, losing the write-back silently is the worst available outcome.
func TestCreateNoteConcurrentSameTitleLosesNothing(t *testing.T) {
	const n = 8
	root := t.TempDir()

	var wg sync.WaitGroup
	results := make([]*CreateResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together to make the collision likely
			results[i], errs[i] = CreateNote(root, NewNoteSpec{
				Type:  TypeGotcha,
				Title: "race condition note",
				Do:    fmt.Sprintf("do-%d", i),
				Dont:  "dont",
				Why:   "why",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	ids := map[string]bool{}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if ids[results[i].ID] {
			t.Errorf("call %d got id %q, already handed to another caller", i, results[i].ID)
		}
		ids[results[i].ID] = true
	}

	// Every success receipt must name a file that actually exists.
	for i, r := range results {
		if _, err := os.Stat(r.Path); err != nil {
			t.Errorf("call %d was told it wrote %s, but: %v", i, r.Path, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, DirForType(TypeGotcha)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("%d notes on disk, want %d: %d write-backs were destroyed", len(entries), n, n-len(entries))
	}
}
