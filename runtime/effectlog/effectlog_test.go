package effectlog

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOccurrenceCountsPerBranchCallsite verifies the per-(branch, callsite)
// occurrence counter: repeats within one (branch, callsite) increment, while a
// different branch or callsite restarts at 0. This is the scheduling-independent
// half of the replay key (DESIGN.md §10).
func TestOccurrenceCountsPerBranchCallsite(t *testing.T) {
	r := NewRecorder()

	// Same (branch, callsite) twice → occurrences 0, 1.
	if got := recOcc(r, "root", "1:1"); got != 0 {
		t.Errorf("first root/1:1 occurrence = %d, want 0", got)
	}
	if got := recOcc(r, "root", "1:1"); got != 1 {
		t.Errorf("second root/1:1 occurrence = %d, want 1", got)
	}
	// Different callsite, same branch → restarts at 0.
	if got := recOcc(r, "root", "2:1"); got != 0 {
		t.Errorf("root/2:1 occurrence = %d, want 0", got)
	}
	// Different branch, same callsite → restarts at 0.
	if got := recOcc(r, "parallel:0", "1:1"); got != 0 {
		t.Errorf("parallel:0/1:1 occurrence = %d, want 0", got)
	}

	// Seq remains global insertion order regardless of branch/callsite.
	recs := r.Records()
	for i, rec := range recs {
		if rec.Seq != i {
			t.Errorf("recs[%d].Seq = %d, want %d", i, rec.Seq, i)
		}
	}
}

// recOcc appends a record on (branch, callsite) and returns the assigned
// Occurrence, by reading it back from the stored slice.
func recOcc(r *Recorder, branch, callsite string) int {
	seq := r.Append(Record{Branch: branch, Callsite: callsite, Tool: "t", Status: "ok"})
	return r.Records()[seq].Occurrence
}

// TestDefaultBranchIsRoot confirms an unset Branch defaults to "root".
func TestDefaultBranchIsRoot(t *testing.T) {
	r := NewRecorder()
	r.Append(Record{Callsite: "1:1"})
	if b := r.Records()[0].Branch; b != "root" {
		t.Errorf("branch = %q, want root", b)
	}
}

// TestConcurrentAppendIsSafe stresses the Recorder from many goroutines; run
// under -race this guards the shared-state seam (DESIGN.md §6).
func TestConcurrentAppendIsSafe(t *testing.T) {
	r := NewRecorder()
	const goroutines = 16
	const each = 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			branch := "parallel:" + string(rune('0'+g%10))
			for n := 0; n < each; n++ {
				r.Append(Record{Branch: branch, Callsite: "1:1", Tool: "t", Status: "ok"})
			}
		}(g)
	}
	wg.Wait()

	if n := len(r.Records()); n != goroutines*each {
		t.Errorf("records = %d, want %d", n, goroutines*each)
	}
}

// TestClockSeamStampsTs confirms the Clock seam supplies a deterministic RFC3339
// timestamp (DESIGN.md §7).
func TestClockSeamStampsTs(t *testing.T) {
	fixed := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	r := NewRecorder()
	r.Clock = func() time.Time { return fixed }

	r.Append(Record{Callsite: "1:1", Tool: "fs.read", Status: "ok"})
	got := r.Records()[0].Ts
	if want := fixed.Format(time.RFC3339); got != want {
		t.Errorf("ts = %q, want %q", got, want)
	}
}

// TestJSONLSinkWritesOneObjectPerLine verifies the durable sink streams each
// record as exactly one valid JSON object per line, parseable back to a Record,
// and that a captured compensation survives the round-trip (DESIGN.md §7).
func TestJSONLSinkWritesOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	fixed := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	r := NewRecorder()
	r.Clock = func() time.Time { return fixed }
	r.SetSink(&buf)

	r.Append(Record{Callsite: "1:1", Tool: "fs.read", Args: []any{"/a"}, Reversible: true, Status: "ok"})
	r.Append(Record{
		Callsite:     "2:1",
		Tool:         "fs.write",
		Args:         []any{"/b", "hi"},
		Reversible:   true,
		Status:       "ok",
		Compensation: &Compensation{Tool: "fs.delete", Args: []any{"/b"}},
	})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("sink wrote %d lines, want 2:\n%s", len(lines), buf.String())
	}
	for i, line := range lines {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i, err, line)
		}
		if rec.Ts != fixed.Format(time.RFC3339) {
			t.Errorf("line %d ts = %q, want fixed clock time", i, rec.Ts)
		}
		if rec.Seq != i {
			t.Errorf("line %d seq = %d, want %d", i, rec.Seq, i)
		}
	}

	// The second record's compensation must survive the JSONL round-trip.
	var second Record
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if second.Compensation == nil {
		t.Fatalf("compensation lost in round-trip: %s", lines[1])
	}
	if second.Compensation.Tool != "fs.delete" {
		t.Errorf("compensation tool = %q, want fs.delete", second.Compensation.Tool)
	}

	// The first record declares no compensation, so the field must be omitted from
	// its JSON (omitempty), keeping the line terse.
	if strings.Contains(lines[0], "compensation") {
		t.Errorf("first line should omit absent compensation: %s", lines[0])
	}
}

// TestConcurrentSinkIsSafe streams from many goroutines through the sink; under
// -race this guards the sink write happening under the same lock (DESIGN.md §6).
func TestConcurrentSinkIsSafe(t *testing.T) {
	var buf bytes.Buffer
	r := NewRecorder()
	r.SetSink(&buf)

	const goroutines = 16
	const each = 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for n := 0; n < each; n++ {
				r.Append(Record{Branch: "parallel:0", Callsite: "1:1", Tool: "t", Status: "ok"})
			}
		}(g)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != goroutines*each {
		t.Fatalf("sink lines = %d, want %d", len(lines), goroutines*each)
	}
	for i, line := range lines {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not valid JSON under concurrency (interleaved write?): %v", i, err)
		}
	}
}
