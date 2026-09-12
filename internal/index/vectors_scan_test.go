// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
	"time"
)

// This driver deliberately reuses and poisons its buffer. SQLite currently
// allocates each BLOB, which would hide accidental retention of borrowed bytes.
type vectorScanRows struct {
	index  int
	buffer []byte
	closed chan struct{}
}

func (r *vectorScanRows) Columns() []string { return []string{"node_id", "embedding"} }
func (r *vectorScanRows) Close() error {
	for i := range r.buffer {
		r.buffer[i] = 0xff
	}
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}
func (r *vectorScanRows) Next(dest []driver.Value) error {
	if r.index == 3 {
		return io.EOF
	}
	copy(r.buffer, encodeVec([]float32{float32(r.index), float32(r.index + 1)}))
	dest[0], dest[1] = "note:a", r.buffer
	r.index++
	return nil
}

type vectorScanConnector struct{ rows *vectorScanRows }

func (c vectorScanConnector) Connect(context.Context) (driver.Conn, error) {
	return vectorScanConn{c.rows}, nil
}
func (c vectorScanConnector) Driver() driver.Driver { return vectorScanDriver{} }

type vectorScanDriver struct{}

func (vectorScanDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type vectorScanConn struct{ rows *vectorScanRows }

func (vectorScanConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (vectorScanConn) Close() error              { return nil }
func (vectorScanConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (c vectorScanConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return c.rows, nil
}

type vectorScanQueryer struct {
	metadata   *sql.DB
	rows       *sql.DB
	sqlContext context.Context
}

func (q vectorScanQueryer) QueryRowContext(_ context.Context, query string, args ...any) *sql.Row {
	return q.metadata.QueryRowContext(q.sqlContext, query, args...)
}
func (q vectorScanQueryer) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	if query != liveVectorsQuery {
		return nil, errors.New("unexpected vector query")
	}
	return q.rows.QueryContext(q.sqlContext, query, args...)
}

func vectorScanFixture(t *testing.T, ctx context.Context) (vectorScanQueryer, *vectorScanRows) {
	t.Helper()
	s := openVecStore(t)
	replaceVectorSnapshot(t, s, "scan-model", []float32{0, 1})
	r := &vectorScanRows{buffer: make([]byte, 8), closed: make(chan struct{})}
	db := sql.OpenDB(vectorScanConnector{r})
	t.Cleanup(func() { db.Close() })
	return vectorScanQueryer{s.readDB, db, ctx}, r
}

func TestLoadVectorsOwnsDecodedStorageAfterDriverBufferReuse(t *testing.T) {
	q, rows := vectorScanFixture(t, context.Background())
	model, dim, got, err := loadVectorsContext(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][][]float32{"note:a": {{0, 1}, {1, 2}, {2, 3}}}
	if model != "scan-model" || dim != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("load = %q/%d/%v, want independent decoded chunks %v", model, dim, got, want)
	}
	select {
	case <-rows.closed:
	default:
		t.Fatal("driver rows not closed")
	}
	got["note:a"][0][0] = 99
	if got["note:a"][1][0] != 1 || got["note:a"][2][0] != 2 {
		t.Fatal("decoded chunks alias each other")
	}
	if q.rows.Stats().InUse != 0 {
		t.Fatal("reader connection still in use")
	}
}

// Count only loader checks: vectorScanQueryer passes the underlying context to
// database/sql. The seventh check is decoding row two, after its RawBytes Scan
// and after row one was added to the map. Canceling there exercises the read-hold
// release without a timing race or a production-only test hook.
type vectorScanCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *vectorScanCancelContext) Err() error {
	c.checks++
	if c.checks == 7 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestLoadVectorsCanceledWithBorrowedRowReleasesReadHold(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &vectorScanCancelContext{Context: base, cancel: cancel}
	q, rows := vectorScanFixture(t, base)
	type result struct {
		vecs map[string][][]float32
		err  error
	}
	done := make(chan result, 1)
	go func() {
		_, _, vecs, err := loadVectorsContext(ctx, q, nil)
		done <- result{vecs, err}
	}()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.vecs != nil {
			t.Fatalf("canceled load = %v, %v", got.vecs, got.err)
		}
		if rows.index != 2 {
			t.Fatalf("canceled after %d rows, want second borrowed row", rows.index)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled load deadlocked while holding borrowed bytes")
	}
	select {
	case <-rows.closed:
	case <-time.After(time.Second):
		t.Fatal("canceled driver rows not closed")
	}
	if q.rows.Stats().InUse != 0 {
		t.Fatal("canceled load retained its connection")
	}
}

func TestLoadVectorsCanceledAfterMetadataReleasesSnapshot(t *testing.T) {
	s := openVecStore(t)
	replaceVectorSnapshot(t, s, "old", []float32{1, 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, vecs, err := s.loadVectorsSnapshotContext(ctx, cancel)
	if !errors.Is(err, context.Canceled) || vecs != nil {
		t.Fatalf("canceled snapshot = %v, %v", vecs, err)
	}
	replaceVectorSnapshot(t, s, "new", []float32{3, 4})
	model, _, current, err := s.LoadVectors()
	if err != nil || model != "new" || !reflect.DeepEqual(current["note:vector-note"], [][]float32{{3, 4}}) {
		t.Fatalf("subsequent snapshot = %q, %v, %v", model, current, err)
	}
}

func TestLoadVectorsBorrowedScanPreservesDecoding(t *testing.T) {
	s := openVecStore(t)
	replaceVectorSnapshot(t, s, "scan-model", []float32{0, 1})
	for _, value := range []any{
		[]byte{}, []byte{1, 2, 3}, []byte{1, 2, 3, 4, 5},
		encodeVec([]float32{math.Float32frombits(0x80000000), math.Float32frombits(0x7fc00001), float32(math.Inf(1))}),
		"abcdefgh", int64(12345678), 1.25,
	} {
		if err := s.Write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE vectors SET embedding=?`, value)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// SQLite BLOB affinity can still hold text or numeric values. Preserve the
		// old owned Scan conversion and partial-float truncation, not just BLOBs.
		var owned []byte
		if err := s.readDB.QueryRow(`SELECT embedding FROM vectors`).Scan(&owned); err != nil {
			t.Fatal(err)
		}
		want := decodeVec(owned)
		_, _, byNode, err := s.LoadVectors()
		if err != nil {
			t.Fatal(err)
		}
		chunks := byNode["note:vector-note"]
		if len(chunks) != 1 || len(chunks[0]) != len(want) {
			t.Fatalf("%T: wrong decoded shape %v", value, chunks)
		}
		for i, got := range chunks[0] {
			if math.Float32bits(got) != math.Float32bits(want[i]) {
				t.Fatalf("%T: float bits changed at %d", value, i)
			}
		}
	}
}
