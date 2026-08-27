// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"errors"
	"runtime"
	"sync"
)

// FileError pairs a path with the error that stopped it from parsing.
type FileError struct {
	Path string
	Err  error
}

// ErrDuplicateNoteID marks a file the incremental reconcile QUARANTINED because another
// file already claims its note id. It is a distinct condition from unparseable
// frontmatter and has a distinct remedy (give one of the two notes a new id), so
// consumers match it with errors.Is rather than reading the message.
var ErrDuplicateNoteID = errors.New("duplicate note id")

// ParseFiles parses paths concurrently with a bounded worker pool and returns
// the notes in the original path order, so the graph it feeds is deterministic
// regardless of worker count. Parsing (read + scan) is the dominant, I/O-bound
// cost and is embarrassingly parallel.
//
// BuildGraph stays serial on purpose: it is a fast id-resolving merge into one
// shared graph, where mutex contention from N goroutines would cost more than
// the work it parallelizes.
//
// workers <= 0 means runtime.NumCPU().
func ParseFiles(paths []string, workers int) ([]*ParsedNote, []FileError) {
	notes, errs, _ := ParseFilesContext(context.Background(), paths, workers)
	return notes, errs
}

// ParseFilesContext is ParseFiles with caller-controlled cancellation. It stops
// scheduling new files as soon as ctx is cancelled and joins the worker pool before it
// returns. An underlying filesystem read that the OS cannot interrupt may finish later,
// but its result is isolated in a private buffered channel and cannot mutate this pass's
// results. A cancelled pass returns no partial note set.
func ParseFilesContext(ctx context.Context, paths []string, workers int) ([]*ParsedNote, []FileError, error) {
	return parseFilesContext(ctx, paths, workers, func(path string) (*ParsedNote, error) {
		return ParseFileContext(ctx, path)
	})
}

func parseFilesContext(ctx context.Context, paths []string, workers int, parseFile func(string) (*ParsedNote, error)) ([]*ParsedNote, []FileError, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(paths) == 0 {
		return nil, nil, nil
	}
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(paths) {
		workers = len(paths)
	}

	type result struct {
		pn  *ParsedNote
		err error
	}
	// Each worker writes only the slot named by its job. The WaitGroup joins every
	// worker before collection, so the disjoint writes need no lock and cannot race
	// the ordered read below.
	results := make([]result, len(paths))

	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case i, ok := <-jobs:
					if !ok {
						return
					}
					// A send and cancellation can become ready together. Refuse the job
					// after receipt as well, so no new file starts after cancellation.
					if ctx.Err() != nil {
						return
					}
					pn, err := parseFile(paths[i])
					results[i] = result{pn: pn, err: err}
				}
			}
		}()
	}

schedule:
	for i := range paths {
		select {
		case <-ctx.Done():
			break schedule
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	notes := make([]*ParsedNote, 0, len(paths))
	var errs []FileError
	for i, r := range results {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if r.err != nil {
			errs = append(errs, FileError{Path: paths[i], Err: r.err})
			continue
		}
		notes = append(notes, r.pn)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return notes, errs, nil
}
