// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package latency reports slow local work without recording user data or changing
// its cancellation/publication contract. Labels must be static code constants.
package latency

import (
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var nextID atomic.Uint64

// Trace is one operation. Phase and End are safe against the progress timer.
// Nested operations have separate process-local IDs; durations overlap and must
// not be added together. End means returned, not succeeded or durably committed.
type Trace struct {
	mu               sync.Mutex
	logger           *slog.Logger
	operation, phase string
	id               uint64
	start, since     time.Time
	phases           map[string]time.Duration
	timer            *time.Timer
	slow, interval   time.Duration
	ended            bool
}

// Start stays silent for operations shorter than one second. For work still in
// flight, a progress record every ten seconds identifies the currently active
// phase even when that phase never returns. Always defer End immediately.
func Start(operation, phase string) *Trace {
	return start(operation, phase, slog.Default(), time.Second, 10*time.Second)
}

func start(operation, phase string, logger *slog.Logger, slow, interval time.Duration) *Trace {
	now := time.Now()
	t := &Trace{logger: logger, operation: operation, phase: phase,
		id: nextID.Add(1), start: now, since: now, phases: make(map[string]time.Duration),
		slow: slow, interval: interval}
	t.mu.Lock()
	t.timer = time.AfterFunc(interval, t.progress)
	t.mu.Unlock()
	return t
}

// Phase closes the previous phase and enters the next. Repeated phases (for
// example collision retries) accumulate under the same label, bounding memory.
func (t *Trace) Phase(phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return
	}
	now := time.Now()
	t.phases[t.phase] += now.Sub(t.since)
	t.phase, t.since = phase, now
}

func (t *Trace) progress() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return
	}
	now := time.Now()
	t.logger.Info("mesh: slow operation in progress", "operation", t.operation,
		"pid", os.Getpid(), "trace_id", t.id, "phase", t.phase,
		"elapsed", now.Sub(t.start), "phase_elapsed", now.Sub(t.since))
	t.timer.Reset(t.interval)
}

// End stops progress reporting on every return path, including errors. The
// summary deliberately contains no error text, file paths, IDs or note content.
func (t *Trace) End() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return
	}
	t.ended = true
	t.timer.Stop()
	now := time.Now()
	t.phases[t.phase] += now.Sub(t.since)
	if now.Sub(t.start) < t.slow {
		return
	}
	attrs := make([]any, 0, len(t.phases))
	for phase, dur := range t.phases {
		attrs = append(attrs, slog.Duration(phase, dur))
	}
	t.logger.Info("mesh: slow operation returned", "operation", t.operation,
		"pid", os.Getpid(), "trace_id", t.id, "last_phase", t.phase,
		"elapsed", now.Sub(t.start), slog.Group("phases", attrs...))
}
