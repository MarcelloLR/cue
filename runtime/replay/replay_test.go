package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarcelloLR/cue/runtime/effectlog"
)

// recordsToJSONL renders records as a recorder would stream them, so a replay
// Source can be built from the same JSONL shape `cue run --log` writes.
func recordsToJSONL(t *testing.T, recs []effectlog.Record) string {
	t.Helper()
	var b strings.Builder
	r := effectlog.NewRecorder()
	r.SetSink(&b)
	for _, rec := range recs {
		// Preserve the caller's branch/callsite/occurrence verbatim by writing the
		// record through a recorder whose counter we do not rely on: AppendReserved
		// stores the occurrence as-is.
		r.AppendReserved(rec)
	}
	return b.String()
}

func TestReadIndexesByBranchCallsiteOccurrence(t *testing.T) {
	msg := "boom"
	jsonl := recordsToJSONL(t, []effectlog.Record{
		{Branch: "root", Callsite: "1:1", Occurrence: 0, Tool: "t.a", Status: "ok", Result: "first"},
		{Branch: "root", Callsite: "1:1", Occurrence: 1, Tool: "t.a", Status: "ok", Result: "second"},
		{Branch: "parallel:0", Callsite: "2:1", Occurrence: 0, Tool: "t.b", Status: "error", Error: &msg},
	})

	src, err := Read(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if src.Len() != 3 {
		t.Fatalf("Len = %d, want 3", src.Len())
	}

	// Same (branch, callsite) different occurrence resolves to different records.
	r0, ok := src.Lookup("root", "1:1", 0)
	if !ok || r0.Result != "first" {
		t.Errorf("occurrence 0 = %+v, ok=%v; want result 'first'", r0, ok)
	}
	r1, ok := src.Lookup("root", "1:1", 1)
	if !ok || r1.Result != "second" {
		t.Errorf("occurrence 1 = %+v, ok=%v; want result 'second'", r1, ok)
	}

	// The parallel branch keys independently of root.
	rb, ok := src.Lookup("parallel:0", "2:1", 0)
	if !ok || rb.Status != "error" {
		t.Errorf("parallel:0 lookup = %+v, ok=%v; want error status", rb, ok)
	}

	// A missing occurrence is a clean miss (caller turns it into CUE_REPLAY_001).
	if _, ok := src.Lookup("root", "1:1", 2); ok {
		t.Errorf("occurrence 2 should miss")
	}
}

func TestReadEmptyBranchNormalisesToRoot(t *testing.T) {
	// A record written without an explicit branch must be findable under "root",
	// matching how the recorder defaults the branch.
	jsonl := recordsToJSONL(t, []effectlog.Record{
		{Callsite: "1:1", Occurrence: 0, Tool: "t.a", Status: "ok", Result: "x"},
	})
	src, err := Read(strings.NewReader(jsonl))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.Lookup("root", "1:1", 0); !ok {
		t.Errorf("empty-branch record should be findable under root")
	}
}

func TestReadSkipsBlankLinesAndRejectsGarbage(t *testing.T) {
	if _, err := Read(strings.NewReader("\n\n")); err != nil {
		t.Errorf("blank lines should be skipped, got %v", err)
	}
	if _, err := Read(strings.NewReader("not json")); err == nil {
		t.Errorf("garbage line should be a hard error")
	}
}

func TestLoadFromFile(t *testing.T) {
	jsonl := recordsToJSONL(t, []effectlog.Record{
		{Branch: "root", Callsite: "1:1", Occurrence: 0, Tool: "t.a", Status: "ok", Result: int64(42)},
	})
	dir := t.TempDir()
	p := filepath.Join(dir, "log.jsonl")
	if err := os.WriteFile(p, []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec, ok := src.Lookup("root", "1:1", 0)
	if !ok {
		t.Fatalf("record not found after Load")
	}
	// UseNumber keeps the int from widening; the stored value round-trips as a
	// json.Number that object.FromAny later lifts back to an Integer.
	if rec.Result == nil {
		t.Errorf("result lost in round-trip")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Errorf("loading a missing log should error")
	}
}
