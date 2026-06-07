package evaluator

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
	"github.com/MarcelloLR/cue/runtime/effectlog"
	"github.com/MarcelloLR/cue/runtime/replay"
)

// sourceFrom builds a replay.Source from captured effect records by serialising
// them to the same JSONL shape `cue run --log` writes and reading them back. This
// exercises the real Load/Read path (JSON round-trip included), so a test asserts
// against exactly what a replay from disk would see.
func sourceFrom(t *testing.T, recs []effectlog.Record) *replay.Source {
	t.Helper()
	var buf bytes.Buffer
	sink := effectlog.NewRecorder()
	sink.SetSink(&buf)
	for _, rec := range recs {
		// AppendReserved preserves each record's branch/callsite/occurrence verbatim
		// so the serialised log matches the original run's keys exactly.
		sink.AppendReserved(rec)
	}
	src, err := replay.Read(&buf)
	if err != nil {
		t.Fatalf("replay.Read: %v", err)
	}
	return src
}

// replayProgram parses and runs src in replay mode against source, injecting the
// "tool" namespace (so tool calls resolve) and any extra options. It returns the
// result and the freshly recorded replay effects.
func replayProgram(t *testing.T, src string, impl object.ToolImpl, source *replay.Source, opts ...Option) (object.Object, []effectlog.Record) {
	t.Helper()
	p := parser.New(lexer.New(src))
	program := p.ParseProgram()
	if p.HasErrors() {
		for _, d := range p.Diagnostics() {
			t.Errorf("parse diagnostic: [%s] %s", d.Code, d.Message)
		}
		t.Fatalf("unexpected diagnostics for %q", src)
	}
	env := object.NewEnvironment()
	if impl != nil {
		env.Set("tool", &object.Namespace{
			Name:    "tool",
			Members: map[string]*object.Tool{"echo": {Impl: impl}},
		})
	}
	effects := effectlog.NewRecorder()
	allOpts := append([]Option{WithEffects(effects), WithReplay(source)}, opts...)
	return New(allOpts...).Eval(program, env), effects.Records()
}

// TestReplayRoundTripNoSideEffects is the headline §10 round-trip: run a program
// with a mock tool under a recorder, capture the records, then replay the same
// program against those records. The replayed result must match the original AND
// the tool must NOT be invoked during replay (the side effect is served from the
// log, not performed).
func TestReplayRoundTripNoSideEffects(t *testing.T) {
	const src = `
let a = tool.echo("one")
let b = tool.echo("two")
[a["echo"], b["echo"]]
`
	// Live run captures the log.
	live := newMock()
	result, recs := runTool(t, src, live)
	if live.invokedCount() != 2 {
		t.Fatalf("live run should invoke the tool twice, got %d", live.invokedCount())
	}
	wantInspect := result.Inspect()
	if len(recs) != 2 {
		t.Fatalf("live run should log 2 effects, got %d", len(recs))
	}

	// Replay against the captured log with a FRESH mock whose invoked counter must
	// stay 0 — proving no side effect ran.
	replayTool := newMock()
	got, replayRecs := replayProgram(t, src, replayTool, sourceFrom(t, recs))

	if got.Inspect() != wantInspect {
		t.Errorf("replay result = %q, want %q (must match the original run)", got.Inspect(), wantInspect)
	}
	if replayTool.invokedCount() != 0 {
		t.Errorf("replay must not invoke the tool, but invoked = %d", replayTool.invokedCount())
	}
	// Replay still records the effects it served, so the envelope shows them and a
	// fresh log could be produced.
	if len(replayRecs) != 2 {
		t.Fatalf("replay should record 2 served effects, got %d", len(replayRecs))
	}
	for i, rec := range replayRecs {
		if rec.Status != "ok" || rec.Tool != "tool.echo" {
			t.Errorf("replay rec[%d] = %+v, want ok tool.echo", i, rec)
		}
	}
}

// TestReplayDeterministicUnderParallel is the subtle §10 point: a program with
// parallel tool calls replays deterministically even though seq order differs run
// to run, because the key is (branch, callsite, occurrence), not seq. The replay
// result must match and nothing may be invoked.
func TestReplayDeterministicUnderParallel(t *testing.T) {
	const src = `
let xs = ["a", "b", "c", "d", "e", "f"]
parallel (x in xs) { tool.echo(x)["echo"] }
`
	// Live run: the tool is invoked once per branch; effects are tagged per branch.
	live := newMock()
	result, recs := runTool(t, src, live)
	arr, ok := result.(*object.Array)
	if !ok {
		t.Fatalf("live parallel result = %T, want Array", result)
	}
	if live.invokedCount() != int64(len(arr.Elements)) {
		t.Fatalf("live run should invoke once per branch (%d), got %d", len(arr.Elements), live.invokedCount())
	}
	wantInspect := result.Inspect()

	// Each branch is its own (branch, callsite) so every record has occurrence 0 but
	// a distinct branch label — that is what makes the parallel run keyable.
	branches := map[string]bool{}
	for _, rec := range recs {
		branches[rec.Branch] = true
		if rec.Occurrence != 0 {
			t.Errorf("each parallel branch's single call should be occurrence 0, got %d on %s", rec.Occurrence, rec.Branch)
		}
	}
	if len(branches) != len(arr.Elements) {
		t.Errorf("expected one branch per element, got %d branches for %d elements", len(branches), len(arr.Elements))
	}

	// Replay many times: scheduling (and thus seq) can differ each run, but the
	// (branch, callsite, occurrence) key is stable, so results match every time.
	for iter := 0; iter < 20; iter++ {
		replayTool := newMock()
		got, _ := replayProgram(t, src, replayTool, sourceFrom(t, recs))
		if got.Inspect() != wantInspect {
			t.Fatalf("iter %d: replay parallel result = %q, want %q", iter, got.Inspect(), wantInspect)
		}
		if replayTool.invokedCount() != 0 {
			t.Fatalf("iter %d: replay must not invoke the tool, invoked = %d", iter, replayTool.invokedCount())
		}
	}
}

// TestReplayLLMAndAskHuman: an llm() and ask_human() replay from the log without
// reaching the provider or stdin. The spy provider would flip a flag if called and
// the input reader would error if read, so a passing test proves neither happened.
func TestReplayLLMAndAskHuman(t *testing.T) {
	const src = `
let s = llm("summarize this")
let name = ask_human("your name?")
[s, name]
`
	// Live run with the deterministic mock provider + a real input line captures the
	// log; the recorded outputs are what replay will serve.
	liveEffects := effectlog.NewRecorder()
	liveResult := evalSrc(t, src, object.NewEnvironment(),
		WithEffects(liveEffects),
		WithInput(strings.NewReader("Ada\n")),
		WithOutput(&bytes.Buffer{}))
	wantInspect := liveResult.Inspect()
	recs := liveEffects.Records()
	if len(recs) != 2 {
		t.Fatalf("live run should log llm + ask_human, got %d effects", len(recs))
	}

	// Replay: a spy provider that must never be called, and an input reader that
	// errors if read (a single byte then EOF would be read by promptLine; errReader
	// guarantees any read is observable as a failure if it happened).
	spy := &spyProvider{}
	er := &errReader{}
	got, replayRecs := evalSrcReplay(t, src, sourceFrom(t, recs),
		WithLLM(spy), WithInput(er))

	if got.Inspect() != wantInspect {
		t.Errorf("replay result = %q, want %q", got.Inspect(), wantInspect)
	}
	if spy.called {
		t.Errorf("replay must not reach the llm provider")
	}
	if er.reads != 0 {
		t.Errorf("replay must not read stdin for ask_human, but In was read %d times", er.reads)
	}
	if len(replayRecs) != 2 {
		t.Errorf("replay should record 2 served effects, got %d", len(replayRecs))
	}
}

// TestReplayMismatchYieldsReplay001: replaying a program edited so a call site no
// longer matches the log (an extra call) yields CUE_REPLAY_001.
func TestReplayMismatchYieldsReplay001(t *testing.T) {
	// Original program: one tool call. Capture its single record.
	const orig = `tool.echo("one")["echo"]`
	_, recs := runTool(t, orig, newMock())
	if len(recs) != 1 {
		t.Fatalf("expected one captured record, got %d", len(recs))
	}

	// Edited program: a SECOND call at a new call site that the log never recorded.
	const edited = `
let a = tool.echo("one")["echo"]
let b = tool.echo("two")["echo"]
[a, b]
`
	replayTool := newMock()
	got, _ := replayProgram(t, edited, replayTool, sourceFrom(t, recs))
	e, ok := got.(*object.Error)
	if !ok {
		t.Fatalf("edited replay should error, got %T (%s)", got, got.Inspect())
	}
	if e.Code != "CUE_REPLAY_001" {
		t.Errorf("code = %q, want CUE_REPLAY_001", e.Code)
	}
	if !contains(e.Message, "tool.echo") {
		t.Errorf("mismatch message should name the tool: %q", e.Message)
	}
	if replayTool.invokedCount() != 0 {
		t.Errorf("a mismatched replay must not invoke the tool")
	}
}

// TestReplayReconstructsErrorAndDenied: a recorded error and a recorded denial
// replay as the same CUE_TOOL_* / CUE_CAP_* errors, without re-invoking or
// re-gating.
func TestReplayReconstructsErrorAndDenied(t *testing.T) {
	// Recorded error: live run with a failing tool.
	failed := newMock()
	failed.fail = true
	_, errRecs := runTool(t, `tool.echo("x")`, failed)
	if len(errRecs) != 1 || errRecs[0].Status != "error" {
		t.Fatalf("expected one error record, got %+v", errRecs)
	}

	replayTool := newMock() // would SUCCEED if invoked
	got, _ := replayProgram(t, `tool.echo("x")`, replayTool, sourceFrom(t, errRecs))
	e, ok := got.(*object.Error)
	if !ok || e.Code != "CUE_TOOL_001" {
		t.Fatalf("recorded error should replay as CUE_TOOL_001, got %T %v", got, got.Inspect())
	}
	if replayTool.invokedCount() != 0 {
		t.Errorf("replaying a recorded error must not invoke the tool")
	}

	// Recorded denial: live run under a deny-all policy.
	denied := newMock()
	_, denRecs := runTool(t, `tool.echo("x")`, denied, WithPolicy(denyAll{}))
	if len(denRecs) != 1 || denRecs[0].Status != "denied" {
		t.Fatalf("expected one denied record, got %+v", denRecs)
	}

	replayTool2 := newMock()
	// Replay with allow-all policy: the recorded denial must still replay (the gate
	// is skipped in replay; the log is authoritative on the original decision).
	got2, _ := replayProgram(t, `tool.echo("x")`, replayTool2, sourceFrom(t, denRecs))
	e2, ok := got2.(*object.Error)
	if !ok || e2.Code != "CUE_CAP_001" {
		t.Fatalf("recorded denial should replay as CUE_CAP_001, got %T %v", got2, got2.Inspect())
	}
	if replayTool2.invokedCount() != 0 {
		t.Errorf("replaying a recorded denial must not invoke the tool")
	}
}

// TestReplayLoopOccurrences: a tool called repeatedly in a `for` loop replays each
// occurrence to its own recorded result, proving occurrence counting is in lockstep
// with the original run.
func TestReplayLoopOccurrences(t *testing.T) {
	const src = `
let out = []
for x in ["a", "b", "c"] {
  out = push(out, tool.echo(x)["echo"])
}
out
`
	live := newMock()
	result, recs := runTool(t, src, live)
	wantInspect := result.Inspect()
	if len(recs) != 3 {
		t.Fatalf("loop should log 3 effects, got %d", len(recs))
	}
	// All three records share (branch, callsite) but increment occurrence 0,1,2.
	occs := make([]int, 0, 3)
	for _, rec := range recs {
		occs = append(occs, rec.Occurrence)
	}
	sort.Ints(occs)
	for i, occ := range occs {
		if occ != i {
			t.Fatalf("loop occurrences = %v, want 0,1,2", occs)
		}
	}

	replayTool := newMock()
	got, _ := replayProgram(t, src, replayTool, sourceFrom(t, recs))
	if got.Inspect() != wantInspect {
		t.Errorf("loop replay result = %q, want %q", got.Inspect(), wantInspect)
	}
	if replayTool.invokedCount() != 0 {
		t.Errorf("loop replay must not invoke the tool, invoked = %d", replayTool.invokedCount())
	}
}

// evalSrcReplay parses and runs src in replay mode against source with a fresh
// recorder, returning the result and the served effects. It is the no-tool-namespace
// companion to replayProgram for primitive-only programs (llm/ask_human).
func evalSrcReplay(t *testing.T, src string, source *replay.Source, opts ...Option) (object.Object, []effectlog.Record) {
	t.Helper()
	p := parser.New(lexer.New(src))
	prog := p.ParseProgram()
	if p.HasErrors() {
		for _, d := range p.Diagnostics() {
			t.Errorf("parse diagnostic: [%s] %s", d.Code, d.Message)
		}
		t.Fatalf("unexpected parse errors for %q", src)
	}
	effects := effectlog.NewRecorder()
	allOpts := append([]Option{WithEffects(effects), WithReplay(source)}, opts...)
	return New(allOpts...).Eval(prog, object.NewEnvironment()), effects.Records()
}

// errReader fails every read, counting attempts. Replay must never read it: a
// non-zero count means ask_human reached stdin instead of the log.
type errReader struct{ reads int }

func (e *errReader) Read(p []byte) (int, error) {
	e.reads++
	return 0, errReadAttempted
}

var errReadAttempted = errReadErr("replay read stdin")

type errReadErr string

func (e errReadErr) Error() string { return string(e) }
