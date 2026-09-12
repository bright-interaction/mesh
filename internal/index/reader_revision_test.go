// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReaderRevisionBaseTableClassificationIsComplete(t *testing.T) {
	// Fail when a new base table arrives without an explicit cache-vs-live review.
	classified := map[string]bool{}
	for _, table := range readerRevisionTables {
		classified[table] = true
	}
	for _, table := range []string{"search_index", "code_search", "metrics", "note_reuse", "note_health", "dropped_notes", "pending_notes"} {
		classified[table] = true
	}
	probes := allSchemaProbes()
	if len(probes) != len(classified) {
		t.Fatal("base tables changed: review revision coverage")
	}
	for _, probe := range probes {
		if !classified[probe.table] {
			t.Fatalf("unclassified table %s", probe.table)
		}
	}
}

func TestReaderRevisionSchemaAndRevisionShareSnapshot(t *testing.T) {
	_, m, db := revisionFixture(t)
	before := readRevision(t, m)
	m.afterRevisionValidation = func() {
		// Change both the contract and counter AFTER the schema read. The current
		// sample must stay on the earlier snapshot, even on the cached-validation path.
		revisionExec(t, db, `DROP TRIGGER _mesh_rr_v1_nodes_update`)
		revisionExec(t, db, `INSERT INTO meta(key,value) VALUES('snapshot-test','changed')`)
	}
	if sampled := readRevision(t, m); sampled != before {
		t.Fatalf("mixed schema/revision snapshots: %+v -> %+v", before, sampled)
	}
	m.afterRevisionValidation = nil
	if current := readRevision(t, m); current.Tracked || current == before {
		t.Fatal("next sample missed committed invalidation")
	}
}

func TestReaderRevisionUnexpectedTriggerCannotHideChanges(t *testing.T) {
	_, m, db := revisionFixture(t)
	before := readRevision(t, m)
	// A customization outside our trigger namespace can undermine the counter.
	// Validate the complete trigger set rather than trusting canonical names alone.
	revisionExec(t, db, `CREATE TRIGGER custom_metrics AFTER INSERT ON metrics BEGIN
		UPDATE _mesh_reader_revision_v1 SET revision=0; END`)
	if v := readRevision(t, m); v.Tracked {
		t.Fatal("unexpected trigger behavior trusted")
	}
	if err := ensureSchema(db, false, true); err != nil {
		t.Fatal(err)
	}
	if v := readRevision(t, m); v.Tracked {
		t.Fatal("opener removed/ignored custom trigger")
	}
	fallback := readRevision(t, m)
	revisionExec(t, db, `INSERT INTO nodes(id,kind,label) VALUES('n','note','changed');
		INSERT INTO metrics(key,value) VALUES('custom',1)`)
	if v := readRevision(t, m); v == fallback || v.Tracked {
		t.Fatal("custom counter reset concealed data change")
	}
	revisionExec(t, db, `DROP TRIGGER custom_metrics`)
	if v := readRevision(t, m); !v.Tracked || v == before {
		t.Fatal("restored tracker reused pre-customization stamp")
	}
}

type failRevisionDDL struct{ schemaConnection }

func (q failRevisionDDL) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.HasPrefix(query, "CREATE TRIGGER "+readerRevisionTriggerPrefix+"edges_update") {
		return nil, errors.New("injected tracker install failure")
	}
	return q.schemaConnection.ExecContext(ctx, query, args...)
}

func TestReaderRevisionInstallRollsBackAsOneTransaction(t *testing.T) {
	_, m, db := revisionFixture(t)
	removeRevisionTracking(t, db)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ensureReaderRevision(context.Background(), failRevisionDDL{tx}); err == nil {
		t.Fatal("failure injection did not fire")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	objects, err := readRevisionObjects(context.Background(), db)
	if err != nil || len(objects) != 0 {
		t.Fatalf("partial tracker escaped rollback: %d %v", len(objects), err)
	}
	if v := readRevision(t, m); v.Tracked {
		t.Fatal("failed installation trusted")
	}
}

func TestReaderRevisionRejectsMalformedSingletonAndCancellation(t *testing.T) {
	_, m, db := revisionFixture(t)
	for _, query := range []string{
		`UPDATE _mesh_reader_revision_v1 SET epoch='invalid'`,
		`UPDATE _mesh_reader_revision_v1 SET epoch=lower(hex(randomblob(16))),revision=-1`,
	} {
		revisionExec(t, db, query)
		if v := readRevision(t, m); v.Tracked {
			t.Fatal("malformed singleton trusted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.ReaderVersion(ctx); err == nil {
		t.Fatal("canceled sample succeeded")
	}
}

func revisionFixture(t testing.TB) (*Store, *ChangeMonitor, *sql.DB) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	m, err := s.NewChangeMonitor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	// A separate, non-cooperating connection models an older writer: it knows
	// nothing about revisions, and never calls Store.Write or a new Mesh method.
	legacy, err := sql.Open("sqlite", dsn(s.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { legacy.Close() })
	return s, m, legacy
}

func readRevision(t testing.TB, m *ChangeMonitor) ReaderVersion {
	t.Helper()
	v, err := m.ReaderVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func revisionExec(t testing.TB, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func removeRevisionTracking(t testing.TB, db *sql.DB) {
	t.Helper()
	for _, o := range readerRevisionObjects[1:] {
		revisionExec(t, db, "DROP TRIGGER "+o.name)
	}
	revisionExec(t, db, "DROP TABLE "+readerRevisionTable)
}

func TestReaderRevisionCoversAllTrackedWritesFromLegacyConnection(t *testing.T) {
	_, m, db := revisionFixture(t)
	fixtures := map[string][2]string{
		"notes":           {`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime) VALUES('n','n.md','note','n','h','{}',0)`, `title='updated'`},
		"nodes":           {`INSERT INTO nodes(id,kind,label) VALUES('n','note','n')`, `label='updated'`},
		"edges":           {`INSERT INTO edges(source,target,relation,confidence) VALUES('n','m','references','EXTRACTED')`, `weight=0.5`},
		"vectors":         {`INSERT INTO vectors(node_id,chunk_ix,model,dim,embedding) VALUES('n',0,'test',1,X'00000000')`, `content_hash='updated'`},
		"corpus_stats":    {`INSERT INTO corpus_stats(key,value) VALUES('test',1)`, `value=2`},
		"code_files":      {`INSERT INTO code_files(path,lang,mtime,retrieval_hash) VALUES('x.go','go',0,'h')`, `retrieval_hash='updated'`},
		"code_symbols":    {`INSERT INTO code_symbols(id,path,lang,name,kind,start_line,end_line) VALUES('s','x.go','go','S','func',1,2)`, `name='Updated'`},
		"code_edges":      {`INSERT INTO code_edges(src_id,dst_id,relation) VALUES('s','t','calls')`, `dst_id='u'`},
		"note_code_links": {`INSERT INTO note_code_links(note_id,symbol_id,name) VALUES('n','s','S')`, `name='Updated'`},
		"meta":            {`INSERT INTO meta(key,value) VALUES('revision-test','first')`, `value='updated' WHERE key='revision-test'`},
	}
	if len(fixtures) != len(readerRevisionTables) {
		t.Fatal("add write coverage for every tracked table")
	}
	for _, table := range readerRevisionTables {
		t.Run(table, func(t *testing.T) {
			fixture, ok := fixtures[table]
			if !ok {
				t.Fatal("missing table fixture")
			}
			deletion := "DELETE FROM " + table
			if table == "meta" {
				deletion += " WHERE key='revision-test'"
			}
			for _, query := range []string{fixture[0], "UPDATE " + table + " SET " + fixture[1], deletion} {
				before := readRevision(t, m)
				if !before.Tracked {
					t.Fatal("new store did not enable tracking")
				}
				revisionExec(t, db, query)
				after := readRevision(t, m)
				if !after.Tracked || before == after {
					t.Fatalf("missed %s: %+v -> %+v", query, before, after)
				}
			}
		})
	}
}

func TestReaderRevisionIgnoresBookkeepingButNotMetaBoundaryCrossings(t *testing.T) {
	_, m, db := revisionFixture(t)
	before := readRevision(t, m)
	dataBefore, _ := m.Version(context.Background())
	for _, query := range []string{
		`INSERT INTO metrics(key,value) VALUES('fetch:n',1)`,
		`INSERT INTO note_reuse(note_id,authored_at,reuse_count) VALUES('n',1,1)`,
		`INSERT INTO note_health(note_id,path,issue,detected_at) VALUES('n','n.md','overdue',1)`,
		`INSERT INTO dropped_notes(path,err,detected_at) VALUES('bad.md','bad',1)`,
		`INSERT INTO pending_notes(id,type,title,created_at) VALUES('p','note','pending',1)`,
		`INSERT INTO meta(key,value) VALUES('health_completed_at','first')`,
		`UPDATE meta SET value='second' WHERE key='health_completed_at'`,
		`DELETE FROM meta WHERE key='health_completed_at'`,
	} {
		revisionExec(t, db, query)
		if after := readRevision(t, m); after != before {
			t.Fatalf("bookkeeping invalidated: %s", query)
		}
	}
	dataAfter, _ := m.Version(context.Background())
	if dataAfter == dataBefore {
		t.Fatal("test did not make real commits")
	}
	revisionExec(t, db, `INSERT INTO meta(key,value) VALUES('health_completed_at','value')`)
	for _, query := range []string{
		`UPDATE meta SET key='vector_model' WHERE key='health_completed_at'`,
		`UPDATE meta SET key='health_completed_at' WHERE key='vector_model'`,
		`INSERT OR REPLACE INTO meta(key,value) VALUES('vector_model','replacement')`,
		`INSERT OR REPLACE INTO meta(key,value) VALUES('vector_model','second replacement')`,
	} {
		before = readRevision(t, m)
		revisionExec(t, db, query)
		if after := readRevision(t, m); after == before {
			t.Fatalf("missed key boundary/replace: %s", query)
		}
	}
}

func TestReaderRevisionRollbackAndReadPoolLifetime(t *testing.T) {
	s, m, db := revisionFixture(t)
	s.readDB.SetMaxOpenConns(1)
	before := readRevision(t, m)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO nodes(id,kind,label) VALUES('rollback','note','rollback')`); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if after := readRevision(t, m); after != before {
		t.Fatal("rolled back update advanced revision")
	}
	if _, err := s.LoadGraph(); err != nil {
		t.Fatal(err)
	}
	revisionExec(t, db, `INSERT INTO nodes(id,kind,label) VALUES('committed','note','committed')`)
	if after := readRevision(t, m); after == before {
		t.Fatal("monitor retained a read snapshot")
	}
}

func TestReaderRevisionLegacyFallbackAndNonDestructiveInstallation(t *testing.T) {
	_, m, db := revisionFixture(t)
	removeRevisionTracking(t, db)
	revisionExec(t, db, `INSERT INTO metrics(key,value) VALUES('preserved',42)`)
	before := readRevision(t, m)
	if before.Tracked {
		t.Fatal("legacy schema claimed tracking")
	}
	revisionExec(t, db, `UPDATE metrics SET value=43 WHERE key='preserved'`)
	if after := readRevision(t, m); before == after {
		t.Fatal("legacy fallback ignored telemetry commit")
	}
	if err := ensureSchema(db, false, true); err != nil {
		t.Fatal(err)
	}
	installed := readRevision(t, m)
	if !installed.Tracked || installed == before {
		t.Fatal("installation did not establish a new tracked baseline")
	}
	var value int
	if err := db.QueryRow(`SELECT value FROM metrics WHERE key='preserved'`).Scan(&value); err != nil || value != 43 {
		t.Fatalf("installation lost data: %d %v", value, err)
	}
	data, _ := m.Version(context.Background())
	if err := ensureSchema(db, false, true); err != nil {
		t.Fatal(err)
	}
	if after := readRevision(t, m); after != installed {
		t.Fatal("complete writable open changed schema/revision")
	}
	if after, _ := m.Version(context.Background()); after != data {
		t.Fatal("complete writable open wrote rows")
	}
}

func TestReaderRevisionMissingAlteredAndRecreatedTriggersFailClosed(t *testing.T) {
	_, m, db := revisionFixture(t)
	initial := readRevision(t, m)
	trigger := readerRevisionTriggerPrefix + "nodes_update"
	revisionExec(t, db, "DROP TRIGGER "+trigger)
	missing := readRevision(t, m)
	if missing.Tracked {
		t.Fatal("missing trigger trusted")
	}
	revisionExec(t, db, "CREATE TRIGGER "+trigger+" AFTER UPDATE ON nodes BEGIN SELECT 1; END")
	if v := readRevision(t, m); v.Tracked {
		t.Fatal("same-name altered trigger trusted")
	}
	if err := ensureSchema(db, false, true); err != nil {
		t.Fatal(err)
	}
	if v := readRevision(t, m); v.Tracked {
		t.Fatal("opener overwrote unrecognized trigger")
	}
	revisionExec(t, db, "DROP TRIGGER "+trigger)
	if err := ensureSchema(db, false, true); err != nil {
		t.Fatal(err)
	}
	repaired := readRevision(t, m)
	if !repaired.Tracked || repaired.Epoch == initial.Epoch {
		t.Fatal("repair reused an epoch that missed writes")
	}
	revisionExec(t, db, `DELETE FROM _mesh_reader_revision_v1`)
	if v := readRevision(t, m); v.Tracked {
		t.Fatal("missing singleton trusted")
	}
}

func BenchmarkReaderRevisionWrites(b *testing.B) {
	for _, rows := range []int{1, 1000} {
		for _, tracked := range []bool{false, true} {
			b.Run(fmt.Sprintf("rows=%d/tracked=%t", rows, tracked), func(b *testing.B) {
				_, _, db := revisionFixture(b)
				if !tracked {
					removeRevisionTracking(b, db)
				}
				for i := 0; i < rows; i++ {
					revisionExec(b, db, `INSERT INTO nodes(id,kind,label) VALUES(?,'note','initial')`, fmt.Sprint(i))
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					revisionExec(b, db, `UPDATE nodes SET label=?`, fmt.Sprint(i))
				}
			})
		}
	}
}
