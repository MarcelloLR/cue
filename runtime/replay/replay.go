// Package replay is the recorded-outcome source behind deterministic replay
// (DESIGN.md §10). The only nondeterminism in a Cue run lives at three boundaries
// — a tool's Invoke, the llm() provider, and ask_human()'s stdin — and every one
// of those is captured in the effect log (§7). Replay re-executes a program but,
// at each of those boundaries, returns the recorded outcome from a prior log
// instead of performing the side effect, so the run reproduces exactly.
//
// The subtle correctness point (DESIGN.md §6, §10) is the KEY. Records are matched
// by (branch-path, callsite-id, occurrence), NEVER by the global seq, because seq
// is scheduling-dependent under `parallel`: two runs of the same program can
// interleave their parallel branches differently and so assign different seqs to
// the same logical call. Branch labels ("parallel:k"), call sites ("line:col"),
// and the per-(branch, callsite) occurrence counter are all deterministic, so this
// key is stable across runs — which is exactly what makes a parallel run replayable.
//
// A Source is read-only after loading and safe for concurrent lookup, so parallel
// branches replay concurrently against one shared Source by reference, just as they
// share one effect Recorder.
package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/MarcelloLR/cue/runtime/effectlog"
)

// Source serves recorded effect outcomes by their stable (branch, callsite,
// occurrence) key. It is built once from a JSONL effect log and is immutable
// afterward, so every lookup is a pure map read — no locking is needed and any
// number of parallel branches can replay against the same Source concurrently
// (DESIGN.md §10).
//
// Design choice (DESIGN.md §10, option (a)): the Source indexes all records into a
// map keyed by (branch, callsite, occurrence) and the evaluator computes the
// occurrence in lockstep with the live recorder (effectlog.Recorder.Reserve), so
// the lookup key is produced by the SAME counter the original run used. That keeps
// occurrence counting single-source — the alternative, having the Source keep its
// own per-key cursor, would duplicate the counting logic and is precisely how
// replay drift creeps in.
type Source struct {
	byKey map[string]effectlog.Record
}

// key builds the lookup string for a (branch, callsite, occurrence) triple. Branch
// is normalised to "root" when empty to match how the recorder defaults it, so a
// record written with an implicit root branch is still found (DESIGN.md §10).
func key(branch, callsite string, occurrence int) string {
	if branch == "" {
		branch = "root"
	}
	return branch + "\x00" + callsite + "\x00" + strconv.Itoa(occurrence)
}

// Load reads a JSONL effect log from path and indexes it into a Source. Each line
// is one effectlog.Record (the exact shape `cue run --log` writes). Numbers are
// decoded with UseNumber so a logged int64 does not silently widen to a float when
// it is later reconstructed into a Cue value (object.FromAny handles json.Number).
func Load(path string) (*Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open log: %w", err)
	}
	defer f.Close()
	return Read(f)
}

// Read indexes a JSONL effect log streamed from r into a Source. It is the
// io.Reader-based core of Load, exposed so tests (and a harness) can replay from an
// in-memory log without touching disk. It decodes the records via ReadRecords (the
// shared parsing core) and then indexes them by key; blank lines are skipped and a
// malformed line is a hard error so a corrupt log fails loudly rather than silently
// dropping effects.
func Read(r io.Reader) (*Source, error) {
	recs, err := ReadRecords(r)
	if err != nil {
		return nil, err
	}
	src := &Source{byKey: make(map[string]effectlog.Record, len(recs))}
	for _, rec := range recs {
		// Last write wins if a (branch, callsite, occurrence) repeats — a single
		// run never produces duplicates, but appending two runs to one log file
		// would, and replaying the most recent is the least surprising behaviour.
		src.byKey[key(rec.Branch, rec.Callsite, rec.Occurrence)] = rec
	}
	return src, nil
}

// LoadRecords reads a JSONL effect log from path into the ordered slice of records it
// contains, preserving append (seq) order. It shares the exact line-parsing core that
// Load uses (Read → ReadRecords), so a log that loads for replay also loads for
// rollback. Rollback needs the records in order — to walk them backward — rather than
// indexed by key, which is why this returns the slice and Load returns the Source.
func LoadRecords(path string) ([]effectlog.Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open log: %w", err)
	}
	defer f.Close()
	return ReadRecords(f)
}

// ReadRecords decodes a JSONL effect log streamed from r into the ordered slice of
// records it contains, in append order. It is the io.Reader-based core LoadRecords and
// Read share (Read indexes the same records by key), so the JSONL parsing — UseNumber,
// the raised line cap, blank-line skipping, and the hard error on a malformed line —
// lives in exactly one place. Exposed so tests (and a harness) can drive rollback from
// an in-memory log without disk.
func ReadRecords(r io.Reader) ([]effectlog.Record, error) {
	var out []effectlog.Record
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var rec effectlog.Record
		if err := dec.Decode(&rec); err != nil {
			return nil, fmt.Errorf("replay: log line %d is not a valid effect record: %w", line, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("replay: read log: %w", err)
	}
	return out, nil
}

// Lookup returns the recorded outcome for a (branch, callsite, occurrence) key and
// whether one was found. A miss means the program no longer matches the log (a call
// site moved, was added, or now runs more times than the log captured); the caller
// surfaces that as a CUE_REPLAY_001 diagnostic (DESIGN.md §10).
func (s *Source) Lookup(branch, callsite string, occurrence int) (effectlog.Record, bool) {
	rec, ok := s.byKey[key(branch, callsite, occurrence)]
	return rec, ok
}

// Len reports how many records the Source indexed. It is a convenience for tests
// and diagnostics.
func (s *Source) Len() int { return len(s.byKey) }
