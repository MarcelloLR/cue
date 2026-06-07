package parser

import (
	"testing"

	"github.com/MarcelloLR/cue/lexer"
)

// parse is a helper that parses src and fails the test on any diagnostic.
func parse(t *testing.T, src string) string {
	t.Helper()
	p := New(lexer.New(src))
	program := p.ParseProgram()
	if p.HasErrors() {
		for _, d := range p.Diagnostics() {
			t.Errorf("diagnostic: %d:%d [%s] %s", d.Span.Start.Line, d.Span.Start.Col, d.Code, d.Message)
		}
		t.Fatalf("unexpected diagnostics parsing %q", src)
	}
	return program.String()
}

func TestOperatorPrecedence(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"2 + 3 * 4", "(2 + (3 * 4))"},
		{"-a * b", "((-a) * b)"},
		{"!-a", "(!(-a))"},
		{"a + b - c", "((a + b) - c)"},
		{"a + b * c + d", "((a + (b * c)) + d)"},
		{"3 + 4 * 5 == 3 * 1 + 4 * 5", "((3 + (4 * 5)) == ((3 * 1) + (4 * 5)))"},
		{"1 < 2 && 3 > 4", "((1 < 2) && (3 > 4))"},
		{"a || b && c", "(a || (b && c))"},
		{"(2 + 3) * 4", "((2 + 3) * 4)"},
		{"a + b % c", "(a + (b % c))"},
		{"a.b.c", "((a.b).c)"},
		{"a.b(c)", "(a.b)(c)"},
		{"a[0][1]", "((a[0])[1])"},
		{"-foo()", "(-foo())"},
	}
	for _, c := range cases {
		if got := parse(t, c.input); got != c.want {
			t.Errorf("parse(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestStatementsParse(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"let x = 5", "let x = 5"},
		{"return", "return"},
		{"return 5", "return 5"},
		{"x = 10", "x = 10"},
		{"a[0] = 1", "(a[0]) = 1"},
		{"u.name = \"hi\"", "(u.name) = \"hi\""},
		{"fn(x, y) { x + y }", "fn(x, y) { (x + y) }"},
		{"foo(1, 2 * 3, 4 + 5)", "foo(1, (2 * 3), (4 + 5))"},
		{"[1, 2 + 2, 3]", "[1, (2 + 2), 3]"},
		{"{\"a\": 1, \"b\": 2}", "{\"a\": 1, \"b\": 2}"},
		{"for x in xs { x }", "for x in xs { x }"},
	}
	for _, c := range cases {
		if got := parse(t, c.input); got != c.want {
			t.Errorf("parse(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestIfElseParses(t *testing.T) {
	got := parse(t, "if x < 1 { a } else { b }")
	want := "if (x < 1) { a } else { b }"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestElseIfParses(t *testing.T) {
	// else-if chains should parse without diagnostics.
	parse(t, "if a { 1 } else if b { 2 } else { 3 }")
}

func TestDiagnosticsHaveSpans(t *testing.T) {
	p := New(lexer.New("let = 5"))
	p.ParseProgram()
	if !p.HasErrors() {
		t.Fatal("expected a diagnostic for `let = 5`")
	}
	d := p.Diagnostics()[0]
	if d.Code == "" {
		t.Error("diagnostic missing code")
	}
	if d.Span.Start.Line == 0 {
		t.Error("diagnostic missing span")
	}
}

func TestParallelParses(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"parallel (i in xs) { i }", "parallel (i in xs) { i }"},
		{"parallel (i in xs) { f(i) }", "parallel (i in xs) { f(i) }"},
		// The leading '(' opens paren-depth, so a newline inside the head is a
		// continuation (DESIGN.md §2).
		{"parallel (i in\nxs) { i }", "parallel (i in xs) { i }"},
		// Inline bounded limit.
		{"parallel (i in xs, limit = 4) { i }", "parallel (i in xs, limit = 4) { i }"},
		{"parallel (i in [1, 2, 3], limit = n) { i }", "parallel (i in [1, 2, 3], limit = n) { i }"},
	}
	for _, c := range cases {
		if got := parse(t, c.input); got != c.want {
			t.Errorf("parse(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestParallelBlockParses(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// Single and multiple branches; the String() form joins branches with "; ".
		{"parallel { a = f() }", "parallel { a = f() }"},
		{"parallel { a = f(); b = g() }", "parallel { a = f(); b = g() }"},
		// Newline-separated branches: the lexer's terminator rule inserts the ';'
		// after each value (DESIGN.md §2), so no explicit separator is needed.
		{"parallel {\n  a = f()\n  b = g()\n}", "parallel { a = f(); b = g() }"},
		// Branch values are arbitrary expressions, including tool calls and arithmetic.
		{"parallel { x = 1 + 2; y = ns.tool(z) }", "parallel { x = (1 + 2); y = (ns.tool)(z) }"},
		// A trailing terminator before '}' is tolerated.
		{"parallel { a = f();\n }", "parallel { a = f() }"},
	}
	for _, c := range cases {
		if got := parse(t, c.input); got != c.want {
			t.Errorf("parse(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestParallelBlockMalformedYieldsDiagnostic(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"branch not name=expr", "parallel { f() }"},
		{"branch missing equals", "parallel { a f() }"},
		{"branch missing value", "parallel { a = }"},
		{"non-ident branch name", "parallel { 1 = f() }"},
		{"unclosed block", "parallel { a = f()"},
		{"neither paren nor brace", "parallel 42"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(lexer.New(c.src))
			p.ParseProgram()
			if !p.HasErrors() {
				t.Fatalf("expected a diagnostic for %q", c.src)
			}
			for _, d := range p.Diagnostics() {
				if d.Span.Start.Line == 0 {
					t.Errorf("diagnostic %q missing span", d.Code)
				}
			}
		})
	}
}

// TestParallelBlockReportsEveryMalformedBranch confirms error recovery: a block with
// two bad branches reports two diagnostics, not just the first (DESIGN.md §9).
func TestParallelBlockReportsEveryMalformedBranch(t *testing.T) {
	const src = "parallel { f(); g() }"
	p := New(lexer.New(src))
	p.ParseProgram()
	diags := p.Diagnostics()
	if len(diags) < 2 {
		t.Fatalf("expected a diagnostic per malformed branch (>=2), got %d: %+v", len(diags), diags)
	}
}

func TestRetryParses(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"retry (3) { f() }", "retry (3) { f() }"},
		{"retry (n) { tool.go() }", "retry (n) { (tool.go)() }"},
		// The leading '(' opens paren-depth, so a newline in the head continues.
		{"retry (\n3\n) { f() }", "retry (3) { f() }"},
	}
	for _, c := range cases {
		if got := parse(t, c.input); got != c.want {
			t.Errorf("parse(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestRetryMalformedYieldsDiagnostic(t *testing.T) {
	for _, src := range []string{
		"retry 3 { f() }",  // missing parens
		"retry (3)",        // missing body
		"retry () { f() }", // missing attempts expr
	} {
		p := New(lexer.New(src))
		p.ParseProgram()
		if !p.HasErrors() {
			t.Errorf("expected a diagnostic for %q", src)
		}
	}
}

func TestParallelMalformedYieldsDiagnostic(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"missing open paren", "parallel i in xs { i }"},
		{"missing in", "parallel (i xs) { i }"},
		{"missing var", "parallel (in xs) { i }"},
		{"missing close paren", "parallel (i in xs { i }"},
		{"missing body", "parallel (i in xs)"},
		{"limit keyword typo", "parallel (i in xs, limt = 4) { i }"},
		{"limit missing equals", "parallel (i in xs, limit 4) { i }"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(lexer.New(c.src))
			p.ParseProgram()
			if !p.HasErrors() {
				t.Fatalf("expected a diagnostic for %q", c.src)
			}
			for _, d := range p.Diagnostics() {
				if d.Span.Start.Line == 0 {
					t.Errorf("diagnostic %q missing span", d.Code)
				}
			}
		})
	}
}
