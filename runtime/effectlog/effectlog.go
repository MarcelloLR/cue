// Package effectlog is the append-only ledger of side-effecting tool calls
// (DESIGN.md §7). Every gated invocation produces one Record; the run-result
// envelope (§9) surfaces them so an agent can observe exactly what touched the
// outside world.
//
// Phase 3 fills out the §7 schema: Records gain a timestamp and a captured
// compensation descriptor, the Recorder gains a testable clock seam, and a
// durable JSONL sink streams each record to disk as it is recorded (DESIGN.md
// §7: "JSONL append-only by default — durable, greppable, trivially
// replayable"). The mutex established in Phase 1 still guards every mutation, so
// Phase 2's parallel branches and the new sink stay race-free with no rewrite.
package effectlog

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Record is one append-only effect-log entry. The field set and JSON tags match
// the §7 schema exactly: seq, ts, callsite, branch, tool, args, reversible,
// status, result, error, duration_ms, compensation.
type Record struct {
	// Seq is the global append order. It is scheduling-dependent under
	// `parallel`; deterministic replay keys on (Branch, Callsite, Occurrence)
	// instead (§10).
	Seq int `json:"seq"`
	// Ts is the wall-clock time the record was appended, in RFC3339. It comes
	// from the Recorder's clock seam so tests can pin it deterministically.
	Ts string `json:"ts"`
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
	// Status is "ok", "error", or "denied" (the call was blocked by policy and
	// never invoked, but the attempt is auditable).
	Status string `json:"status"`
	// Result is the tool's return value as a plain JSON value (nil on error/denied).
	Result any `json:"result,omitempty"`
	// Error is the failure message, present only when Status == "error" or
	// "denied" (where it carries the policy reason).
	Error *string `json:"error,omitempty"`
	// DurationMs is the wall-clock time spent inside Invoke (0 for denied calls,
	// which never run Invoke).
	DurationMs int64 `json:"duration_ms"`
	// Compensation is the captured inverse action for a successful reversible call
	// (DESIGN.md §7): the tool name + args that would undo it. Phase 5 walks the
	// log backward firing these; Phase 3 only captures the data. Absent when the
	// tool declares no compensation.
	Compensation *Compensation `json:"compensation,omitempty"`
}

// Compensation is the inverse-action descriptor captured for a reversible call
// (DESIGN.md §7). It records *what would undo* a successful effect — the tool to
// call and the arguments to call it with — so a later phase can roll back by
// replaying compensations in reverse order. Capturing it is Phase 3; firing it
// is Phase 5.
type Compensation struct {
	// Tool is the full namespaced name of the inverse tool, e.g. "fs.delete".
	Tool string `json:"tool"`
	// Args are the inverse call's arguments, as plain JSON values (same encoding
	// as Record.Args).
	Args []any `json:"args"`
}

// Recorder accumulates effect Records in invocation order. It is safe for
// concurrent use so Phase 2's parallel branches can share one Recorder: every
// mutation happens under mu, which is the single piece of shared mutable runtime
// state the scheduler touches (DESIGN.md §6). Phase 3 streams each record to an
// optional durable sink under the same lock, so the JSONL file and the in-memory
// slice never diverge and never race.
type Recorder struct {
	mu      sync.Mutex
	records []Record
	// occ counts prior appends per (branch, callsite) so each record's Occurrence
	// is assigned deterministically regardless of goroutine scheduling (§10).
	occ map[string]int
	// Clock supplies the timestamp stamped onto each record. It defaults to
	// time.Now but is a seam so tests can pin time for deterministic ts fields.
	Clock func() time.Time
	// sink, when non-nil, receives each record as one JSON line as it is appended
	// (DESIGN.md §7 durable JSONL). Writes happen under mu so the stream stays in
	// append order and goroutine-safe.
	sink io.Writer
	// sinks receive each appended record via WriteRecord, for storage backends
	// richer than line-oriented JSONL (e.g. the SQLite "queryable upgrade",
	// DESIGN.md §7). Like the JSONL sink they are driven under mu, in append order.
	sinks []Sink
}

// Sink receives each record as it is appended, for durable or queryable storage
// beyond the in-memory ledger (DESIGN.md §7). The SQLite backend is a Sink. Because
// the Recorder calls WriteRecord under its lock and in append order, implementations
// need no locking of their own. A returned error is swallowed by the Recorder (the
// audit log is a side-channel and must never break a user program), so a Sink that
// wants to surface failures should do so out-of-band.
type Sink interface {
	WriteRecord(Record) error
}

// NewRecorder returns an empty Recorder with the default wall-clock.
func NewRecorder() *Recorder {
	return &Recorder{occ: map[string]int{}, Clock: time.Now}
}

// SetSink directs the Recorder to stream every subsequently appended record to w
// as one JSON object per line (JSONL). It is the durable-storage seam the
// `cue run --log <path>` flag wires to an append-only file (DESIGN.md §7). Passing
// nil disables streaming. Records already appended are not back-filled; call this
// before recording begins.
func (r *Recorder) SetSink(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sink = w
}

// AddSink registers a Sink to receive every subsequently appended record. It is
// the seam the SQLite backend (`cue run --sqlite <path>`) wires in, alongside or
// instead of the JSONL sink. Records already appended are not back-filled; call it
// before recording begins.
func (r *Recorder) AddSink(s Sink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sinks = append(r.sinks, s)
}

// occKey builds the per-(branch, callsite) counter key. Branch is normalised to
// "root" when empty, matching the default applied on append, so the live recorder
// and the replay lookup that share this counter agree on the key (DESIGN.md §10).
func occKey(branch, callsite string) string {
	if branch == "" {
		branch = "root"
	}
	return branch + "\x00" + callsite
}

// nextOcc returns the next Occurrence for a (branch, callsite) and advances the
// counter. It is the single source of truth for occurrence assignment: every
// effect — whether appended by a live run or reserved by deterministic replay —
// pulls its Occurrence from here exactly once, so the replay key cannot drift from
// what a live run would have produced (DESIGN.md §10). It must be called under mu.
func (r *Recorder) nextOcc(key string) int {
	if r.occ == nil {
		r.occ = map[string]int{}
	}
	occ := r.occ[key]
	r.occ[key]++
	return occ
}

// Reserve advances and returns the next Occurrence for (branch, callsite) without
// storing a record. It is the seam deterministic replay uses to compute, in
// lockstep with the live recorder, the (Branch, Callsite, Occurrence) key it looks
// the recorded outcome up by — the occurrence MUST come from the same counter a
// live Append would use, which is why both go through nextOcc (DESIGN.md §10). The
// replay path follows a Reserve with AppendReserved so the counter advances exactly
// once per effect. Branch defaults to "root" when unset.
func (r *Recorder) Reserve(branch, callsite string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nextOcc(occKey(branch, callsite))
}

// Append assigns the timestamp (from Clock), the scheduling-dependent Seq (global
// append order) and the scheduling-independent Occurrence (per-(Branch, Callsite)
// repeat index), stores the record, streams it to the sink if one is configured,
// and returns its Seq. Branch defaults to "root" when unset.
func (r *Recorder) Append(rec Record) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Branch == "" {
		rec.Branch = "root"
	}
	rec.Occurrence = r.nextOcc(occKey(rec.Branch, rec.Callsite))
	return r.store(rec)
}

// AppendReserved stores a record whose Occurrence was already assigned by a prior
// Reserve, leaving the occurrence counter untouched. Deterministic replay uses it
// so a replayed effect still lands in the live ledger (and a fresh JSONL log) with
// the same key it was looked up under, without double-counting the occurrence
// (DESIGN.md §10). Branch defaults to "root" when unset.
func (r *Recorder) AppendReserved(rec Record) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Branch == "" {
		rec.Branch = "root"
	}
	return r.store(rec)
}

// store stamps the timestamp and global Seq, appends the record to the in-memory
// slice, and streams it to the durable sink. It is the shared tail of Append and
// AppendReserved and must be called under mu; it never touches the occurrence
// counter — its callers own that decision (DESIGN.md §10).
func (r *Recorder) store(rec Record) int {
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	if rec.Ts == "" {
		rec.Ts = clock().Format(time.RFC3339)
	}
	rec.Seq = len(r.records)
	r.records = append(r.records, rec)
	r.writeSink(rec)
	return rec.Seq
}

// writeSink streams one record to the durable sink as a single JSON line. It is
// called under mu. A marshal/write failure is intentionally swallowed: the effect
// log is an audit side-channel, and a failed durable write must not break (or
// panic) a user program — the in-memory Records() still carry the truth for the
// envelope (DESIGN.md §7, §9).
func (r *Recorder) writeSink(rec Record) {
	if r.sink != nil {
		if line, err := json.Marshal(rec); err == nil {
			line = append(line, '\n')
			_, _ = r.sink.Write(line)
		}
	}
	// Richer sinks (e.g. SQLite) receive the record as-is. Errors are swallowed for
	// the same reason JSONL write errors are: the audit log must not break the run.
	for _, s := range r.sinks {
		_ = s.WriteRecord(rec)
	}
}

// Records returns a copy of the recorded entries in append order.
func (r *Recorder) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, len(r.records))
	copy(out, r.records)
	return out
}
