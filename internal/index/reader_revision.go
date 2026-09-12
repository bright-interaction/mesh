// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
)

// This is an OPTIONAL, independently versioned derived extension, not a new
// minimum base schema. Old readers ignore it; new readers fall back to whole-DB
// data_version unless every definition and the singleton revision are valid.
// Only the elected writable opener installs it, in the base schema transaction.
// No notes, embeddings, telemetry or review candidates are dropped or rewritten.
const readerRevisionTable = "_mesh_reader_revision_v1"
const readerRevisionTriggerPrefix = "_mesh_rr_v1_"

// Everything cached by the graph/retriever, plus conservative code/bridge and
// corpus coverage. FTS virtual tables are queried live, not cached in the reader.
// Bookkeeping (metrics, reuse, health, dropped/pending notes) is also read live.
// Adding cached database inputs requires updating this protocol and its tests.
var readerRevisionTables = []string{
	"notes", "nodes", "edges", "vectors", "corpus_stats",
	"code_files", "code_symbols", "code_edges", "note_code_links", "meta",
}

type revisionObject struct{ name, kind, ddl string }

var readerRevisionObjects = revisionDDL()

func revisionDDL() []revisionObject {
	objects := []revisionObject{{readerRevisionTable, "table", `CREATE TABLE _mesh_reader_revision_v1 (id INTEGER PRIMARY KEY CHECK(id=1), epoch TEXT NOT NULL, revision INTEGER NOT NULL)`}}
	for _, table := range readerRevisionTables {
		for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := readerRevisionTriggerPrefix + table + "_" + strings.ToLower(operation)
			when := ""
			if table == "meta" {
				// A key rename crossing the boundary must invalidate in BOTH directions.
				switch operation {
				case "INSERT":
					when = " WHEN NEW.key != 'health_completed_at'"
				case "DELETE":
					when = " WHEN OLD.key != 'health_completed_at'"
				case "UPDATE":
					when = " WHEN OLD.key != 'health_completed_at' OR NEW.key != 'health_completed_at'"
				}
			}
			ddl := fmt.Sprintf("CREATE TRIGGER %s AFTER %s ON %s%s BEGIN UPDATE %s SET revision=revision+1 WHERE id=1; END", name, operation, table, when, readerRevisionTable)
			objects = append(objects, revisionObject{name, "trigger", ddl})
		}
	}
	return objects
}

func readRevisionObjects(ctx context.Context, q schemaConnection) (map[string]revisionObject, error) {
	// Include ALL persistent triggers, not only our names. An additional trigger
	// could reset/suppress the counter (even from a bookkeeping table). Unknown
	// trigger behavior must keep the conservative whole-database fallback.
	rows, err := q.QueryContext(ctx, `SELECT name,type,COALESCE(sql,'') FROM sqlite_schema WHERE type='trigger' OR name=? OR name GLOB ?`, readerRevisionTable, readerRevisionTriggerPrefix+"*")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := make(map[string]revisionObject)
	for rows.Next() {
		var o revisionObject
		if err := rows.Scan(&o.name, &o.kind, &o.ddl); err != nil {
			return nil, err
		}
		objects[o.name] = o
	}
	return objects, rows.Err()
}

func revisionObjectsMatch(objects map[string]revisionObject) bool {
	if len(objects) != len(readerRevisionObjects) {
		return false
	}
	for _, want := range readerRevisionObjects {
		if objects[want.name] != want {
			return false
		}
	}
	return true
}

// Called ONLY inside the owning opener's IMMEDIATE schema transaction. A complete
// install performs no DDL or writes. Partial canonical installs (e.g. after a base
// schema rebuild dropped table-bound triggers) are repaired atomically with a new
// epoch. Unrecognized definitions are left untouched and readers fall back safely.
func ensureReaderRevision(ctx context.Context, q schemaConnection) error {
	objects, err := readRevisionObjects(ctx, q)
	if err != nil {
		return err
	}
	if revisionObjectsMatch(objects) {
		return nil
	}
	wanted := make(map[string]revisionObject)
	for _, o := range readerRevisionObjects {
		wanted[o.name] = o
	}
	for name, o := range objects {
		if wanted[name] != o {
			return nil
		}
	}
	for _, o := range readerRevisionObjects {
		if _, exists := objects[o.name]; !exists {
			if _, err := q.ExecContext(ctx, o.ddl); err != nil {
				return err
			}
		}
	}
	_, err = q.ExecContext(ctx, `INSERT OR REPLACE INTO _mesh_reader_revision_v1(id,epoch,revision) VALUES(1,lower(hex(randomblob(16))),0)`)
	return err
}

// ReaderVersion is comparable only on one ChangeMonitor. Tracked revisions ignore
// bookkeeping commits. Schema changes, epoch changes and mode transitions always
// invalidate. The fallback uses SQLite's same-connection data_version as before.
type ReaderVersion struct {
	Data     int64
	Schema   int64
	Epoch    string
	Revision int64
	Tracked  bool
}

// Like Version, callers serialize samples and Close (MCP uses reloadMu).
func (m *ChangeMonitor) ReaderVersion(ctx context.Context) (version ReaderVersion, err error) {
	defer func() {
		if err == nil && (!m.revisionModeKnown || m.revisionModeTracked != version.Tracked) {
			mode := "data_version"
			if version.Tracked {
				mode = "retrieval_revision_v1"
			}
			slog.Info("mesh: reader freshness tracking", "mode", mode)
			m.revisionModeKnown, m.revisionModeTracked = true, version.Tracked
		}
	}()
	data, err := m.Version(ctx) // file identity + connection-local fallback stamp
	if err != nil {
		return ReaderVersion{}, err
	}
	tx, err := m.conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReaderVersion{}, err
	}
	defer tx.Rollback() // never pin this snapshot between samples
	var schema int64
	if err := tx.QueryRowContext(ctx, `PRAGMA main.schema_version`).Scan(&schema); err != nil {
		return ReaderVersion{}, err
	}
	if !m.revisionChecked || m.revisionSchema != schema {
		objects, err := readRevisionObjects(ctx, tx)
		if err != nil {
			return ReaderVersion{}, err
		}
		m.revisionChecked, m.revisionSchema, m.revisionValid = true, schema, revisionObjectsMatch(objects)
	}
	if m.afterRevisionValidation != nil {
		m.afterRevisionValidation()
	}
	fallback := ReaderVersion{Data: data, Schema: schema}
	if !m.revisionValid {
		return fallback, nil
	}
	var epoch, kind string
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT epoch,revision,typeof(revision) FROM _mesh_reader_revision_v1 WHERE id=1`).Scan(&epoch, &revision, &kind)
	if err == sql.ErrNoRows {
		return fallback, nil
	}
	if err != nil {
		return ReaderVersion{}, err
	}
	decoded, err := hex.DecodeString(epoch)
	if err != nil || len(decoded) != 16 || kind != "integer" || revision < 0 {
		return fallback, nil
	}
	return ReaderVersion{Schema: schema, Epoch: epoch, Revision: revision, Tracked: true}, ctx.Err()
}
