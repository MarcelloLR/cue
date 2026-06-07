package evaluator

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
	"github.com/MarcelloLR/cue/runtime/effectlog"
	"github.com/MarcelloLR/cue/runtime/llm"
)

// --- helpers ---

// evalSrc parses and evaluates src with the given options, failing on parse
// errors. It is the bare-primitive companion to runTool (which injects a tool ns).
func evalSrc(t *testing.T, src string, env *object.Environment, opts ...Option) object.Object {
	t.Helper()
	p := parser.New(lexer.New(src))
	prog := p.ParseProgram()
	if p.HasErrors() {
		for _, d := range p.Diagnostics() {
			t.Errorf("parse diagnostic: [%s] %s", d.Code, d.Message)
		}
		t.Fatalf("unexpected parse errors for %q", src)
	}
	return New(opts...).Eval(prog, env)
}

// flakyTool fails its first failFirst calls, then succeeds — the canonical thing
// you wrap in retry. It is deterministic and offline.
type flakyTool struct {
	failFirst int
	calls     int
}

func (f *flakyTool) Name() string                        { return "flaky.go" }
func (f *flakyTool) Signature() object.Signature         { return object.Signature{Variadic: true} }
func (f *flakyTool) Reversibility() object.Reversibility { return object.Reversible }
func (f *flakyTool) Invoke(_ context.Context, _ []object.Object) (object.Object, error) {
	f.calls++
	if f.calls <= f.failFirst {
		return nil, fmt.Errorf("attempt %d failed", f.calls)
	}
	return &object.String{Value: "ok"}, nil
}

func envWithFlaky(f *flakyTool) *object.Environment {
	env := object.NewEnvironment()
	env.Set("flaky", &object.Namespace{
		Name:    "flaky",
		Members: map[string]*object.Tool{"go": {Impl: f}},
	})
	return env
}

// spyProvider records whether it was called and returns a fixed result.
type spyProvider struct{ called bool }

func (s *spyProvider) Complete(context.Context, llm.Request) (llm.Result, error) {
	s.called = true
	return llm.Result{Text: "should not be seen"}, nil
}

// badProvider returns a structured result whose type violates any STRING schema.
type badProvider struct{}

func (badProvider) Complete(_ context.Context, req llm.Request) (llm.Result, error) {
	obj := make(map[string]any, len(req.Schema))
	for k := range req.Schema {
		obj[k] = int64(0) // always INTEGER, mismatching a STRING schema
	}
	return llm.Result{Object: obj}, nil
}

// --- retry ---

func TestRetrySucceedsAfterFailures(t *testing.T) {
	f := &flakyTool{failFirst: 2}
	effects := effectlog.NewRecorder()
	res := evalSrc(t, `retry(5) { flaky.go() }`, envWithFlaky(f), WithEffects(effects))

	s, ok := res.(*object.String)
	if !ok || s.Value != "ok" {
		t.Fatalf("retry result = %T %v, want String ok", res, res.Inspect())
	}
	if f.calls != 3 {
		t.Errorf("flaky called %d times, want 3 (2 failures + 1 success)", f.calls)
	}
	// Each attempt re-runs (and re-logs) the body — the documented idempotency
	// hazard: 2 error effects + 1 ok effect.
	if len(effects.Records()) != 3 {
		t.Errorf("effects = %d, want 3 (one per attempt)", len(effects.Records()))
	}
}

func TestRetryReturnsLastErrorWhenAllFail(t *testing.T) {
	f := &flakyTool{failFirst: 100}
	res := evalSrc(t, `retry(3) { flaky.go() }`, envWithFlaky(f))
	e, ok := res.(*object.Error)
	if !ok {
		t.Fatalf("all-fail retry result = %T, want Error", res)
	}
	if e.Code != "CUE_TOOL_001" {
		t.Errorf("code = %q, want CUE_TOOL_001 (the last attempt's error)", e.Code)
	}
	if f.calls != 3 {
		t.Errorf("flaky called %d times, want exactly 3 attempts", f.calls)
	}
}

func TestRetryRejectsNonPositiveAndNonInteger(t *testing.T) {
	for _, src := range []string{`retry(0) { 1 }`, `retry(-2) { 1 }`, `retry("x") { 1 }`} {
		res := evalSrc(t, src, object.NewEnvironment())
		e, ok := res.(*object.Error)
		if !ok {
			t.Fatalf("%s: got %T, want Error", src, res)
		}
		if e.Code != "CUE_TYPE_001" {
			t.Errorf("%s: code = %q, want CUE_TYPE_001", src, e.Code)
		}
	}
}

// --- ask_human ---

func TestAskHumanReadsLineAndLogs(t *testing.T) {
	var out bytes.Buffer
	effects := effectlog.NewRecorder()
	res := evalSrc(t, `ask_human("your name?")`, object.NewEnvironment(),
		WithEffects(effects), WithInput(strings.NewReader("Ada\n")), WithOutput(&out))

	s, ok := res.(*object.String)
	if !ok || s.Value != "Ada" {
		t.Fatalf("ask_human result = %T %v, want String Ada", res, res.Inspect())
	}
	if !strings.Contains(out.String(), "your name?") {
		t.Errorf("prompt %q should have been written to Out", out.String())
	}
	recs := effects.Records()
	if len(recs) != 1 || recs[0].Tool != "ask_human" || recs[0].Status != "ok" {
		t.Fatalf("ask_human should log one ok effect, got %+v", recs)
	}
	// The human input is recorded — that is what makes a run replayable (§8).
	if recs[0].Result != "Ada" {
		t.Errorf("recorded human input = %v, want Ada", recs[0].Result)
	}
}

func TestAskHumanInParallelSerializes(t *testing.T) {
	// Three branches each read one line; under the global prompt lock no line is
	// lost or duplicated. (Run under -race to prove no interleaving.)
	effects := effectlog.NewRecorder()
	res := evalSrc(t, `parallel (x in ["a", "b", "c"]) { ask_human("q") }`,
		object.NewEnvironment(),
		WithEffects(effects), WithInput(strings.NewReader("1\n2\n3\n")), WithOutput(&bytes.Buffer{}))

	arr, ok := res.(*object.Array)
	if !ok || len(arr.Elements) != 3 {
		t.Fatalf("parallel ask_human result = %T, want 3-element Array", res)
	}
	got := make([]string, 0, 3)
	for _, e := range arr.Elements {
		got = append(got, e.(*object.String).Value)
	}
	sort.Strings(got)
	if want := []string{"1", "2", "3"}; !equalStrs(got, want) {
		t.Errorf("branches read %v, want each of %v exactly once (no interleaving)", got, want)
	}
}

// --- interactive prompter (ask_human as the policy Prompt resolver) ---

func TestInteractivePrompterAllowsAndDenies(t *testing.T) {
	cases := []struct {
		input     string
		wantAllow bool
	}{{"y\n", true}, {"yes\n", true}, {"n\n", false}, {"\n", false}}
	for _, tc := range cases {
		env := object.NewEnvironment()
		env.Set("tool", &object.Namespace{
			Name:    "tool",
			Members: map[string]*object.Tool{"echo": {Impl: newMock()}},
		})
		var out bytes.Buffer
		interp := New(WithPolicy(promptAll{}), WithInput(strings.NewReader(tc.input)), WithOutput(&out))
		interp.Prompter = NewInteractivePrompter(interp)
		res := interp.Eval(mustParse(t, `tool.echo("hi")`), env)

		if tc.wantAllow {
			if _, ok := res.(*object.Hash); !ok {
				t.Errorf("input %q: want allowed (Hash), got %T %v", tc.input, res, res.Inspect())
			}
		} else {
			e, ok := res.(*object.Error)
			if !ok || e.Code != "CUE_CAP_002" {
				t.Errorf("input %q: want CUE_CAP_002 denial, got %T %v", tc.input, res, res.Inspect())
			}
		}
		if !strings.Contains(out.String(), "Allow?") {
			t.Errorf("input %q: a confirmation prompt should be shown, got %q", tc.input, out.String())
		}
	}
}

// --- llm ---

func TestLLMFreeFormDeterministicAndLogged(t *testing.T) {
	effects := effectlog.NewRecorder()
	res := evalSrc(t, `llm("summarize")`, object.NewEnvironment(), WithEffects(effects))
	s, ok := res.(*object.String)
	if !ok || s.Value != "[mock] summarize" {
		t.Fatalf("llm result = %T %v, want deterministic mock string", res, res.Inspect())
	}
	recs := effects.Records()
	if len(recs) != 1 || recs[0].Tool != "llm" || recs[0].Status != "ok" {
		t.Fatalf("llm should log one ok effect, got %+v", recs)
	}
}

func TestLLMStructuredOutputValidates(t *testing.T) {
	res := evalSrc(t, `llm("extract", {"schema": {"title": "STRING", "count": "INTEGER"}})`,
		object.NewEnvironment())
	h, ok := res.(*object.Hash)
	if !ok {
		t.Fatalf("schema llm result = %T, want Hash", res)
	}
	if _, ok := h.Pairs["title"].(*object.String); !ok {
		t.Errorf("title should be a STRING, got %T", h.Pairs["title"])
	}
	if _, ok := h.Pairs["count"].(*object.Integer); !ok {
		t.Errorf("count should be an INTEGER, got %T", h.Pairs["count"])
	}
}

func TestLLMSchemaMismatchIsTypeError(t *testing.T) {
	res := evalSrc(t, `llm("x", {"schema": {"title": "STRING"}})`,
		object.NewEnvironment(), WithLLM(badProvider{}))
	e, ok := res.(*object.Error)
	if !ok {
		t.Fatalf("schema mismatch should be an Error, got %T", res)
	}
	if e.Code != "CUE_TYPE_001" {
		t.Errorf("code = %q, want CUE_TYPE_001", e.Code)
	}
}

func TestLLMDeniedByPolicyNotCalled(t *testing.T) {
	spy := &spyProvider{}
	res := evalSrc(t, `llm("hi")`, object.NewEnvironment(),
		WithPolicy(denyAll{}), WithLLM(spy))
	e, ok := res.(*object.Error)
	if !ok || e.Code != "CUE_CAP_001" {
		t.Fatalf("denied llm should be CUE_CAP_001, got %T %v", res, res.Inspect())
	}
	if spy.called {
		t.Errorf("a denied llm must not reach the provider")
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
