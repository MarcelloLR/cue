package sqlitelog

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/MarcelloLR/cue/runtime/effectlog"
)

func TestSinkWritesQueryableRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "effects.db")
	sink, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	msg := "boom"
	records := []effectlog.Record{
		{Seq: 0, Branch: "root", Callsite: "1:1", Tool: "fs.write", Args: []any{"/tmp/x", "hi"},
			Reversible: true, Status: "ok", Result: map[string]any{"path": "/tmp/x"},
			Compensation: &effectlog.Compensation{Tool: "fs.delete", Args: []any{"/tmp/x"}}},
		{Seq: 1, Branch: "parallel:0", Callsite: "2:5", Tool: "http.get", Args: []any{"u"},
			Status: "error", Error: &msg},
	}
	for _, r := range records {
		if err := sink.WriteRecord(r); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen independently and query back via SQL — the point of the backend.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	var n int
	if err := db.QueryRow("SELECT count(*) FROM effects").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("row count = %d, want 2", n)
	}

	// The (branch, callsite, occurrence) key and JSON-encoded fields round-trip.
	var tool, status, comp string
	row := db.QueryRow(`SELECT tool, status, compensation FROM effects WHERE branch = 'root'`)
	if err := row.Scan(&tool, &status, &comp); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if tool != "fs.write" || status != "ok" {
		t.Errorf("got tool=%q status=%q, want fs.write/ok", tool, status)
	}
	if comp == "" || comp == "null" {
		t.Errorf("compensation should be stored as JSON text, got %q", comp)
	}

	// A NULL compensation for the errored row (no inverse captured).
	var compNull sql.NullString
	if err := db.QueryRow(`SELECT compensation FROM effects WHERE branch = 'parallel:0'`).Scan(&compNull); err != nil {
		t.Fatalf("scan null: %v", err)
	}
	if compNull.Valid {
		t.Errorf("errored row should store SQL NULL compensation, got %q", compNull.String)
	}
}
