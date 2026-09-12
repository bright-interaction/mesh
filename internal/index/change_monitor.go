// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"

	"github.com/bright-interaction/mesh/internal/vault"
)

// ChangeMonitor samples SQLite's connection-local data_version. Its pinned,
// read-only connection never writes or holds a transaction between samples.
// Every external commit invalidates it, including code/vector-only changes and
// harmless telemetry writes. Values are comparable ONLY on this monitor.
// See https://www.sqlite.org/pragma.html#pragma_data_version.
type ChangeMonitor struct {
	db       *sql.DB
	conn     *sql.Conn
	once     sync.Once
	err      error
	path     string
	identity os.FileInfo
}

// ErrMonitorIndexReplaced disables reuse until the reader is restarted. Hot
// replacement of an open SQLite database is not a supported index rebuild;
// existing pooled read connections may still point at the old file.
var ErrMonitorIndexReplaced = errors.New("mesh: monitored index file was replaced; restart reader")

// NewChangeMonitor uses a separate pool so pinning the monitor cannot starve
// graph reads, even when the caller's ordinary read pool has a one-connection cap.
// Close the monitor before closing the Store. It creates no writer or schema.
func (s *Store) NewChangeMonitor(ctx context.Context) (*ChangeMonitor, error) {
	identity, err := vault.StatContext(ctx, s.dbPath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsnReadOnly(s.dbPath))
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &ChangeMonitor{db: db, conn: conn, path: s.dbPath, identity: identity}, nil
}

func (m *ChangeMonitor) Version(ctx context.Context) (int64, error) {
	info, err := vault.StatContext(ctx, m.path)
	if err != nil {
		// Keep the original identity across a missing/unreadable path. Do not
		// reopen later against a replacement while pooled graph readers still
		// reference the original file.
		return 0, errors.Join(ErrMonitorIndexReplaced, err)
	}
	if !os.SameFile(m.identity, info) {
		return 0, ErrMonitorIndexReplaced
	}
	var version int64
	err = m.conn.QueryRowContext(ctx, "PRAGMA main.data_version").Scan(&version)
	return version, err
}

func (m *ChangeMonitor) Close() error {
	m.once.Do(func() {
		m.err = m.conn.Close()
		if err := m.db.Close(); m.err == nil {
			m.err = err
		}
	})
	return m.err
}
