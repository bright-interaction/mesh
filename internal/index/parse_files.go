package index

import (
	"runtime"
	"sync"
)

// FileError pairs a path with the error that stopped it from parsing.
type FileError struct {
	Path string
	Err  error
}

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
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if len(paths) > 0 && workers > len(paths) {
		workers = len(paths)
	}

	type result struct {
		pn  *ParsedNote
		err error
	}
	// Each goroutine owns a disjoint slot, so there is no shared mutable state
	// and no lock: the only synchronization is the WaitGroup and the semaphore.
	results := make([]result, len(paths))

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			pn, err := ParseFile(paths[i])
			results[i] = result{pn: pn, err: err}
		}(i)
	}
	wg.Wait()

	notes := make([]*ParsedNote, 0, len(paths))
	var errs []FileError
	for i, r := range results {
		if r.err != nil {
			errs = append(errs, FileError{Path: paths[i], Err: r.err})
			continue
		}
		notes = append(notes, r.pn)
	}
	return notes, errs
}
