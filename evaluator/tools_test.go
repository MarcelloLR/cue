package evaluator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MarcelloLR/cue/ast"
	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
	"github.com/MarcelloLR/cue/runtime/effectlog"
	"github.com/MarcelloLR/cue/runtime/policy"
)

// mockTool is a deterministic ToolImpl — it never touches the network, so tool
// invocation can be tested end-to-end with stable output.
type mockTool struct {
	name    string
	sig     object.Signature
	rev     object.Reversibility
	fail    bool
	invoked int
}

func (m *mockTool) Name() string                        { return m.name }
func (m *mockTool) Signature() object.Signature         { return m.sig }
func (m *mockTool) Reversibility() object.Reversibility { return m.rev }
func (m *mockTool) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	m.invoked++
	if m.fail {
		return nil, errors.New("boom")
	}
	s := args[0].(*object.String)
	out := object.NewHash()
	out.Set("echo", &object.String{Value: s.Value})
	return out, nil
}

// denyAll is a Policy that rejects everything, exercising the CUE_CAP path.
type denyAll struct{}

func (denyAll) Check(string, []object.Object, object.Reversibility) policy.Decision {
	return policy.Deny
}

// promptAll is a Policy that returns Prompt for everything, exercising the
// Prompter seam.
type promptAll struct{}

func (promptAll) Check(string, []object.Object, object.Reversibility) policy.Decision {
	return policy.Prompt
}

// compMock is a ToolImpl that also implements object.Compensator, so it captures
// an inverse action on a successful call (DESIGN.md §7).
type compMock struct {
	mockTool
	compTool string
}

func (c *compMock) Compensation(args []object.Object, result object.Object) (string, []object.Object, bool) {
	return c.compTool, args, true
}

// runTool parses src, injects a namespace "tool" with member "echo" backed by
// impl, and evaluates with the given options. It returns the result and effects.
func runTool(t *testing.T, src string, impl object.ToolImpl, opts ...Option) (object.Object, []effectlog.Record) {
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
	ns := &object.Namespace{
		Name:    "tool",
		Members: map[string]*object.Tool{"echo": {Impl: impl}},
	}
	env.Set("tool", ns)

	effects := effectlog.NewRecorder()
	allOpts := append([]Option{WithEffects(effects)}, opts...)
	interp := New(allOpts...)
	return interp.Eval(program, env), effects.Records()
}

func newMock() *mockTool {
	return &mockTool{
		name: "tool.echo",
		sig:  object.Signature{Params: []object.Param{{Name: "s", Type: "string"}}},
		rev:  object.Reversible,
	}
}

func TestToolInvocationSucceedsAndLogs(t *testing.T) {
	result, effects := runTool(t, `tool.echo("hi")`, newMock())

	hash, ok := result.(*object.Hash)
	if !ok {
		t.Fatalf("got %T (%s), want Hash", result, result.Inspect())
	}
	if v := hash.Pairs["echo"].(*object.String).Value; v != "hi" {
		t.Errorf("echo = %q, want hi", v)
	}

	if len(effects) != 1 {
		t.Fatalf("effects = %d, want 1", len(effects))
	}
	rec := effects[0]
	if rec.Tool != "tool.echo" {
		t.Errorf("rec.Tool = %q", rec.Tool)
	}
	if rec.Status != "ok" {
		t.Errorf("rec.Status = %q, want ok", rec.Status)
	}
	if rec.Branch != "root" {
		t.Errorf("rec.Branch = %q, want root", rec.Branch)
	}
	if !rec.Reversible {
		t.Errorf("rec.Reversible should be true")
	}
	if rec.Callsite == "" {
		t.Errorf("rec.Callsite should be a line:col key")
	}
	if len(rec.Args) != 1 || rec.Args[0] != "hi" {
		t.Errorf("rec.Args = %v, want [hi]", rec.Args)
	}
}

func TestToolInvocationGoErrorBecomesCueTool001(t *testing.T) {
	m := newMock()
	m.fail = true
	result, effects := runTool(t, `tool.echo("hi")`, m)

	e, ok := result.(*object.Error)
	if !ok {
		t.Fatalf("got %T, want Error", result)
	}
	if e.Code != "CUE_TOOL_001" {
		t.Errorf("code = %q, want CUE_TOOL_001", e.Code)
	}
	if e.Span.Start.Line == 0 {
		t.Errorf("tool error missing span")
	}
	if len(effects) != 1 || effects[0].Status != "error" {
		t.Fatalf("expected one error-status effect, got %+v", effects)
	}
	if effects[0].Error == nil || *effects[0].Error == "" {
		t.Errorf("error effect should carry a message")
	}
}

func TestToolArityMismatch(t *testing.T) {
	result, effects := runTool(t, `tool.echo("a", "b")`, newMock())
	e, ok := result.(*object.Error)
	if !ok {
		t.Fatalf("got %T, want Error", result)
	}
	if e.Code != "CUE_TYPE_004" {
		t.Errorf("code = %q, want CUE_TYPE_004", e.Code)
	}
	if len(effects) != 0 {
		t.Errorf("arity failure should not invoke the tool or log an effect")
	}
}

func TestToolDeniedByPolicy(t *testing.T) {
	m := newMock()
	result, effects := runTool(t, `tool.echo("hi")`, m, WithPolicy(denyAll{}))
	e, ok := result.(*object.Error)
	if !ok {
		t.Fatalf("got %T, want Error", result)
	}
	if e.Code != "CUE_CAP_001" {
		t.Errorf("code = %q, want CUE_CAP_001", e.Code)
	}
	if m.invoked != 0 {
		t.Errorf("denied tool should not be invoked")
	}
	// Phase 3: the blocked attempt is still recorded (status "denied") so the
	// boundary is auditable, but Invoke never ran (DESIGN.md §7).
	if len(effects) != 1 {
		t.Fatalf("denied tool should log exactly one audit effect, got %d", len(effects))
	}
	if effects[0].Status != "denied" {
		t.Errorf("denied effect status = %q, want denied", effects[0].Status)
	}
	if effects[0].Result != nil {
		t.Errorf("denied effect should carry no result")
	}
	if effects[0].Error == nil || *effects[0].Error == "" {
		t.Errorf("denied effect should carry the policy reason")
	}
}

func TestEffectRecordFullySpecified(t *testing.T) {
	// Drive a successful tool call through a recorder with a pinned clock and
	// assert every §7 field lands on the record.
	fixed := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	effects := effectlog.NewRecorder()
	effects.Clock = func() time.Time { return fixed }

	p := parser.New(lexer.New(`tool.echo("hi")`))
	program := p.ParseProgram()
	if p.HasErrors() {
		t.Fatalf("parse error")
	}
	env := object.NewEnvironment()
	env.Set("tool", &object.Namespace{
		Name:    "tool",
		Members: map[string]*object.Tool{"echo": {Impl: newMock()}},
	})
	New(WithEffects(effects)).Eval(program, env)

	recs := effects.Records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Ts != fixed.Format(time.RFC3339) {
		t.Errorf("ts = %q, want pinned clock time", rec.Ts)
	}
	if rec.Tool != "tool.echo" || rec.Status != "ok" || rec.Branch != "root" {
		t.Errorf("unexpected record header: %+v", rec)
	}
	if rec.Callsite == "" || rec.Result == nil {
		t.Errorf("record missing callsite/result: %+v", rec)
	}
	if !rec.Reversible {
		t.Errorf("echo is reversible; record should reflect it")
	}
}

func TestPromptAllowedByPrompter(t *testing.T) {
	m := newMock()
	allow := policy.FuncPrompter(func(string, []object.Object) (bool, error) { return true, nil })
	result, effects := runTool(t, `tool.echo("hi")`, m,
		WithPolicy(promptAll{}), WithPrompter(allow))

	if _, ok := result.(*object.Hash); !ok {
		t.Fatalf("an allow-prompter should let the call proceed, got %T (%s)", result, result.Inspect())
	}
	if m.invoked != 1 {
		t.Errorf("confirmed tool should be invoked once, got %d", m.invoked)
	}
	if len(effects) != 1 || effects[0].Status != "ok" {
		t.Fatalf("confirmed call should log one ok effect, got %+v", effects)
	}
}

func TestPromptDeniedByDefaultPrompter(t *testing.T) {
	m := newMock()
	// No WithPrompter → the New() default policy.DenyPrompter conservatively denies.
	result, effects := runTool(t, `tool.echo("hi")`, m, WithPolicy(promptAll{}))

	e, ok := result.(*object.Error)
	if !ok {
		t.Fatalf("default prompter should deny, got %T (%s)", result, result.Inspect())
	}
	if e.Code != "CUE_CAP_002" {
		t.Errorf("code = %q, want CUE_CAP_002", e.Code)
	}
	if m.invoked != 0 {
		t.Errorf("an unconfirmed tool must not be invoked")
	}
	if len(effects) != 1 || effects[0].Status != "denied" {
		t.Fatalf("unconfirmed call should log one denied effect, got %+v", effects)
	}
}

func TestPromptDeclinedByPrompter(t *testing.T) {
	m := newMock()
	deny := policy.FuncPrompter(func(string, []object.Object) (bool, error) { return false, nil })
	result, _ := runTool(t, `tool.echo("hi")`, m, WithPolicy(promptAll{}), WithPrompter(deny))
	e, ok := result.(*object.Error)
	if !ok {
		t.Fatalf("a declining prompter should deny, got %T", result)
	}
	if e.Code != "CUE_CAP_002" {
		t.Errorf("code = %q, want CUE_CAP_002", e.Code)
	}
	if m.invoked != 0 {
		t.Errorf("a declined tool must not be invoked")
	}
}

func TestCompensationCaptured(t *testing.T) {
	c := &compMock{
		mockTool: mockTool{
			name: "tool.echo",
			sig:  object.Signature{Params: []object.Param{{Name: "s", Type: "string"}}},
			rev:  object.Reversible,
		},
		compTool: "tool.undo",
	}
	_, effects := runTool(t, `tool.echo("hi")`, c)
	if len(effects) != 1 {
		t.Fatalf("effects = %d, want 1", len(effects))
	}
	comp := effects[0].Compensation
	if comp == nil {
		t.Fatalf("a Compensator's successful call should capture a compensation descriptor")
	}
	if comp.Tool != "tool.undo" {
		t.Errorf("compensation tool = %q, want tool.undo", comp.Tool)
	}
	if len(comp.Args) != 1 || comp.Args[0] != "hi" {
		t.Errorf("compensation args = %v, want [hi]", comp.Args)
	}
}

func TestNoCompensationForPlainTool(t *testing.T) {
	// A tool that does not implement Compensator records no compensation.
	_, effects := runTool(t, `tool.echo("hi")`, newMock())
	if len(effects) != 1 {
		t.Fatalf("effects = %d, want 1", len(effects))
	}
	if effects[0].Compensation != nil {
		t.Errorf("plain tool should capture no compensation, got %+v", effects[0].Compensation)
	}
}

func TestNamespaceMemberResolution(t *testing.T) {
	// Hit: a known member resolves to a Tool value.
	env := object.NewEnvironment()
	ns := &object.Namespace{
		Name:    "tool",
		Members: map[string]*object.Tool{"echo": {Impl: newMock()}},
	}
	env.Set("tool", ns)

	hit := New().Eval(mustParse(t, `tool.echo`), env)
	if _, ok := hit.(*object.Tool); !ok {
		t.Errorf("tool.echo should resolve to a Tool, got %T", hit)
	}

	// Miss: an unknown member is CUE_NAME_003 with a did-you-mean hint.
	miss := New().Eval(mustParse(t, `tool.eco`), env)
	e, ok := miss.(*object.Error)
	if !ok {
		t.Fatalf("tool.eco should be an Error, got %T", miss)
	}
	if e.Code != "CUE_NAME_003" {
		t.Errorf("code = %q, want CUE_NAME_003", e.Code)
	}
	if !contains(e.Message, "echo") {
		t.Errorf("miss message should suggest echo: %q", e.Message)
	}
}

func mustParse(t *testing.T, src string) *ast.Program {
	t.Helper()
	p := parser.New(lexer.New(src))
	prog := p.ParseProgram()
	if p.HasErrors() {
		t.Fatalf("parse error for %q", src)
	}
	return prog
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
