package evaluator

import (
	"testing"

	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
)

func run(t *testing.T, src string) object.Object {
	t.Helper()
	p := parser.New(lexer.New(src))
	program := p.ParseProgram()
	if p.HasErrors() {
		for _, d := range p.Diagnostics() {
			t.Errorf("parse diagnostic: [%s] %s", d.Code, d.Message)
		}
		t.Fatalf("unexpected diagnostics for %q", src)
	}
	return Eval(program, object.NewEnvironment())
}

func wantInt(t *testing.T, src string, want int64) {
	t.Helper()
	obj := run(t, src)
	i, ok := obj.(*object.Integer)
	if !ok {
		t.Fatalf("%q: got %T (%s), want Integer", src, obj, obj.Inspect())
	}
	if i.Value != want {
		t.Errorf("%q = %d, want %d", src, i.Value, want)
	}
}

func wantFloat(t *testing.T, src string, want float64) {
	t.Helper()
	obj := run(t, src)
	f, ok := obj.(*object.Float)
	if !ok {
		t.Fatalf("%q: got %T, want Float", src, obj)
	}
	if f.Value != want {
		t.Errorf("%q = %v, want %v", src, f.Value, want)
	}
}

func wantBool(t *testing.T, src string, want bool) {
	t.Helper()
	obj := run(t, src)
	b, ok := obj.(*object.Boolean)
	if !ok {
		t.Fatalf("%q: got %T, want Boolean", src, obj)
	}
	if b.Value != want {
		t.Errorf("%q = %v, want %v", src, b.Value, want)
	}
}

func TestArithmeticAndPrecedence(t *testing.T) {
	wantInt(t, "2 + 3 * 4", 14)
	wantInt(t, "(2 + 3) * 4", 20)
	wantInt(t, "50 / 2 * 2 + 10", 60)
	wantInt(t, "-5 + 10", 5)
	wantInt(t, "17 % 5", 2)
	wantFloat(t, "10 / 4.0", 2.5)
	wantFloat(t, "1 + 2.5", 3.5)
}

func TestComparisonsAndLogic(t *testing.T) {
	wantBool(t, "1 < 2", true)
	wantBool(t, "2 <= 2", true)
	wantBool(t, "1 == 1.0", true)
	wantBool(t, "true && false", false)
	wantBool(t, "true || false", true)
	wantBool(t, "!false", true)
	wantBool(t, `"a" < "b"`, true)
}

func TestStringConcat(t *testing.T) {
	obj := run(t, `"foo" + "bar"`)
	s, ok := obj.(*object.String)
	if !ok || s.Value != "foobar" {
		t.Fatalf("got %v, want foobar", obj.Inspect())
	}
}

func TestIfExpression(t *testing.T) {
	wantInt(t, "if 1 < 2 { 10 } else { 20 }", 10)
	wantInt(t, "if 1 > 2 { 10 } else { 20 }", 20)
	wantInt(t, "if false { 1 } else if true { 2 } else { 3 }", 2)
}

func TestLetAndReassign(t *testing.T) {
	wantInt(t, "let a = 5; a", 5)
	wantInt(t, "let a = 5; a = a + 1; a", 6)
}

func TestReassignUndefinedErrors(t *testing.T) {
	obj := run(t, "x = 5")
	e, ok := obj.(*object.Error)
	if !ok {
		t.Fatalf("got %T, want Error", obj)
	}
	if e.Code == "" || e.Span.Start.Line == 0 {
		t.Errorf("error missing code/span: %+v", e)
	}
}

func TestClosuresAndRecursion(t *testing.T) {
	wantInt(t, "let add = fn(a, b) { a + b }; add(2, 3)", 5)
	wantInt(t, `
		let makeAdder = fn(n) { fn(x) { x + n } }
		let add5 = makeAdder(5)
		add5(10)`, 15)
	wantInt(t, `
		let fib = fn(n) {
			if n < 2 { return n }
			return fib(n - 1) + fib(n - 2)
		}
		fib(10)`, 55)
}

func TestArraysAndBuiltins(t *testing.T) {
	wantInt(t, "len([1, 2, 3])", 3)
	wantInt(t, "[1, 2, 3][1]", 2)
	wantInt(t, "first([9, 8, 7])", 9)
	wantInt(t, "last([9, 8, 7])", 7)
	wantInt(t, "len(push([1], 2))", 2)
	wantInt(t, `len("héllo")`, 5)
}

func TestForLoopAccumulate(t *testing.T) {
	wantInt(t, `
		let total = 0
		for x in [1, 2, 3, 4] { total = total + x }
		total`, 10)
	wantInt(t, `
		let n = 0
		for i in range(5) { n = n + i }
		n`, 10)
}

func TestHashAndMemberAccess(t *testing.T) {
	wantInt(t, `let u = {"age": 36}; u["age"]`, 36)
	wantInt(t, `let u = {"age": 36}; u.age`, 36)
	wantInt(t, `let u = {"age": 36}; u.age = 37; u.age`, 37)
	obj := run(t, `let u = {"a": 1}; u.missing`)
	if _, ok := obj.(*object.Null); !ok {
		t.Errorf("missing member should be null, got %T", obj)
	}
}

func TestRuntimeErrorsCarryCodeAndSpan(t *testing.T) {
	cases := []string{
		"5 + true",
		"foobar",
		"1 / 0",
		`"x"()`,
	}
	for _, src := range cases {
		obj := run(t, src)
		e, ok := obj.(*object.Error)
		if !ok {
			t.Errorf("%q: got %T, want Error", src, obj)
			continue
		}
		if e.Code == "" {
			t.Errorf("%q: error missing code", src)
		}
		if e.Span.Start.Line == 0 {
			t.Errorf("%q: error missing span", src)
		}
	}
}
