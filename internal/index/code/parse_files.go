// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package code

import (
	"os"
	"runtime"
	"sync"
)

// FileRef pairs a file's absolute path (for reading) with its root-relative path
// (its stored identity). The orchestrator resolves Rel against whichever code root
// the file came from, so files from several repos coexist without path collisions.
type FileRef struct {
	Abs string
	Rel string
}

// FileError pairs a path with the error that stopped it from parsing, mirroring
// index.FileError so a code reindex reports skips the same way a note reindex does.
type FileError struct {
	Path string
	Err  error
}

// ParseCodeFiles parses refs concurrently with a bounded worker pool, preserving
// input order so the symbol graph is deterministic regardless of worker count.
// Reading + scanning is I/O-bound and embarrassingly parallel; the index-time
// merge that resolves call edges stays serial (it owns one shared name index).
// A file that parses to nothing (generated, too large, no symbols) is dropped, not
// errored. workers <= 0 means runtime.NumCPU().
func ParseCodeFiles(refs []FileRef, workers int) ([]*CodeFile, []FileError) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if len(refs) > 0 && workers > len(refs) {
		workers = len(refs)
	}

	type result struct {
		cf  *CodeFile
		err error
	}
	results := make([]result, len(refs))

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ref := refs[i]
			var mtime int64
			if fi, err := os.Stat(ref.Abs); err == nil {
				mtime = fi.ModTime().Unix()
			}
			cf, err := ParseFile(ref.Abs, ref.Rel, Lang(extOf(ref.Rel)), mtime)
			results[i] = result{cf: cf, err: err}
		}(i)
	}
	wg.Wait()

	files := make([]*CodeFile, 0, len(refs))
	var errs []FileError
	for i, r := range results {
		if r.err != nil {
			errs = append(errs, FileError{Path: refs[i].Abs, Err: r.err})
			continue
		}
		if r.cf != nil {
			files = append(files, r.cf)
		}
	}
	return files, errs
}

func extOf(path string) string {
	for i := len(path) - 1; i >= 0 && path[i] != '/'; i-- {
		if path[i] == '.' {
			return path[i:]
		}
	}
	return ""
}
