// Package sqlitelog is the SQLite "queryable upgrade" for the effect log
// (DESIGN.md §7: "SQLite is the queryable upgrade if SQL access is wanted later").
// It implements effectlog.Sink by inserting each record into an `effects` table, so
// a run's side effects can be queried with SQL instead of grepped as JSONL.
//
// It uses the pure-Go modernc.org/sqlite driver (no cgo), so Cue stays a single
// self-contained binary (SPEC §3). The SQLite dependency is deliberately isolated in
// this one package: effectlog and the rest of the runtime depend only on the small
// effectlog.Sink interface, never on the driver.
package sqlitelog

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/MarcelloLR/cue/runtime/effectlog"
)

// schema creates the append-only effects table if absent. JSON-shaped fields (args,
// result, compensation) are stored as TEXT holding their JSON encoding, so they are
// both human-readable and decodable, while scalar fields get native column types for
// easy WHERE/ORDER BY. The (branch, callsite, occurrence) triple — the §10 replay
// key — is indexed for fast lookups.
const schema = `
CREATE TABLE IF NOT EXISTS effects (
    seq          INTEGER,
    ts           TEXT,
    callsite     TEXT,
    branch       TEXT,
    occurrence   INTEGER,
    tool         TEXT,
    args         TEXT,
    reversible   INTEGER,
    status       TEXT,
    result       TEXT,
    error        TEXT,
    duration_ms  INTEGER,
    compensation TEXT
);
CREATE INDEX IF NOT EXISTS effects_key ON effects (branch, callsite, occurrence);`

const insertSQL = `INSERT INTO effects
    (seq, ts, callsite, branch, occurrence, tool, args, reversible, status, result, error, duration_ms, compensation)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Sink is an effectlog.Sink backed by a SQLite database. Open it with Open and pass
// it to (*effectlog.Recorder).AddSink; the Recorder drives WriteRecord under its
// lock and in append order, so the Sink needs no locking of its own.
type Sink struct {
	db   *sql.DB
	stmt *sql.Stmt
}

// Open opens (creating if needed) a SQLite database at path, ensures the effects
// table exists, and prepares the insert statement. Close it when the run finishes.
func Open(path string) (*Sink, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlitelog: open %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlitelog: create schema: %w", err)
	}
	stmt, err := db.Prepare(insertSQL)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlitelog: prepare insert: %w", err)
	}
	return &Sink{db: db, stmt: stmt}, nil
}

// WriteRecord inserts one effect record. JSON-shaped fields are encoded to TEXT; a
// nil result/compensation is stored as SQL NULL. It satisfies effectlog.Sink.
func (s *Sink) WriteRecord(rec effectlog.Record) error {
	args, err := jsonText(rec.Args)
	if err != nil {
		return err
	}
	result, err := jsonText(rec.Result)
	if err != nil {
		return err
	}
	var comp any
	if rec.Compensation != nil {
		comp, err = jsonText(rec.Compensation)
		if err != nil {
			return err
		}
	}
	var errMsg any
	if rec.Error != nil {
		errMsg = *rec.Error
	}
	_, err = s.stmt.Exec(rec.Seq, rec.Ts, rec.Callsite, rec.Branch, rec.Occurrence,
		rec.Tool, args, rec.Reversible, rec.Status, result, errMsg, rec.DurationMs, comp)
	return err
}

// Close releases the prepared statement and database handle.
func (s *Sink) Close() error {
	if s.stmt != nil {
		s.stmt.Close()
	}
	return s.db.Close()
}

// jsonText encodes v as a JSON string for a TEXT column, returning a nil any (SQL
// NULL) for a nil value so absent results/args don't store the literal "null".
func jsonText(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("sqlitelog: encode field: %w", err)
	}
	return string(b), nil
}
