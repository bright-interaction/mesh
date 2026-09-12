// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
)

func TestChangeMonitorSeesExternalCommitsWithoutPinningSnapshotOrReadPool(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.readDB.SetMaxOpenConns(1)
	ctx := context.Background()
	m, err := s.NewChangeMonitor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	first, err := m.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadGraphContext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.conn.ExecContext(ctx, `CREATE TABLE prohibited (id INTEGER)`); err == nil {
		t.Fatal("monitor can write")
	}
	if err := s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO nodes(id,kind,label) VALUES('code:test','symbol','Test')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	second, err := m.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("monitor missed external commit or pinned old snapshot")
	}
	if current, err := m.Version(ctx); err != nil || current != second {
		t.Fatalf("unstable version: %d %v", current, err)
	}
	identity := m.identity
	m.identity, err = os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Version(ctx); !errors.Is(err, ErrMonitorIndexReplaced) {
		t.Fatalf("changed file identity: %v", err)
	}
	m.identity = identity
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Version(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Version(ctx); err == nil {
		t.Fatal("closed monitor returned a usable version")
	}
}
