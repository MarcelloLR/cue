// Package effectlog is the append-only ledger of side-effecting tool calls
// (DESIGN.md §7). Every gated invocation produces one Record; the run-result
// envelope (§9) surfaces them so an agent can observe exactly what touched the
// outside world.
//
// Phase 1 captures the forward-compatible subset of the §7 schema. Later phases
// fill the rest: Phase 2 relies on the Recorder already being goroutine-safe so
// concurrent `parallel` branches can append without a rewrite, and Phase 3 adds
// durable JSONL persistence plus reversibility/compensation. The mutex is in
// place now precisely so that seam never has to be retrofitted.
package effectlog

import "sync"

// Record is one append-only effect-log entry. The field set is a forward slice
// of the §7 schema: enough to audit and (later) replay a run, with room to grow.
type Record struct {
	// Seq is the global append order. It is scheduling-dependent under
	// `parallel`; deterministic replay keys on (Branch, Callsite, Occurrence)
	// instead (§10).
	Seq int `json:"seq"`
	// Callsite is a deterministic key derived from the call node's span
	// ("line:col"), stable across runs for the same source.
	Callsite string `json:"callsite"`
	// Branch is the concurrency path; "root" at the top level, "parallel:k" (and
	// nested "parallel:k/parallel:j") inside parallel branches (§6).
	Branch string `json:"branch"`
	// Occurrence is the per-(Branch, Callsite) repeat index, starting at 0. Unlike
	// Seq it is scheduling-independent, so together with Branch and Callsite it
	// forms the stable replay key (§10) — e.g. a tool called inside a `for` loop
	// gets occurrences 0, 1, 2, … within its branch.
	Occurrence int `json:"occurrence"`
	// Tool is the full namespaced tool name, e.g. "http.get".
	Tool string `json:"tool"`
	// Args are the rendered call arguments (Inspect strings), kept JSON-friendly.
	Args []any `json:"args"`
	// Reversible mirrors the tool's declared reversibility, so a later phase can
	// walk the log backward firing compensations.
	Reversible bool `json:"reversible"`
	// Status is "ok" or "error".
	Status string `json:"status"`
	// Result is the tool's return value as a plain JSON value (nil on error).
	Result any `json:"result,omitempty"`
	// Error is the failure message, present only when Status == "error".
	Error *string `json:"error,omitempty"`
	// DurationMs is the wall-clock time spent inside Invoke.
	DurationMs int64 `json:"duration_ms"`
}

// Recorder accumulates effect Records in invocation order. It is safe for
// concurrent use so Phase 2's parallel branches can share one Recorder: every
// mutation happens under mu, which is the single piece of shared mutable runtime
// state the scheduler touches (DESIGN.md §6).
type Recorder struct {
	mu      sync.Mutex
	records []Record
	// occ counts prior appends per (branch, callsite) so each record's Occurrence
	// is assigned deterministically regardless of goroutine scheduling (§10).
	occ map[string]int
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder { return &Recorder{occ: map[string]int{}} }

// Append assigns the scheduling-dependent Seq (global append order) and the
// scheduling-independent Occurrence (per-(Branch, Callsite) repeat index), stores
// the record, and returns its Seq. Branch defaults to "root" when unset.
func (r *Recorder) Append(rec Record) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Branch == "" {
		rec.Branch = "root"
	}
	if r.occ == nil {
		r.occ = map[string]int{}
	}
	key := rec.Branch + "\x00" + rec.Callsite
	rec.Occurrence = r.occ[key]
	r.occ[key]++
	rec.Seq = len(r.records)
	r.records = append(r.records, rec)
	return rec.Seq
}

// Records returns a copy of the recorded entries in append order.
func (r *Recorder) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, len(r.records))
	copy(out, r.records)
	return out
}
