package effectlog

import (
	"sync"
	"testing"
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
