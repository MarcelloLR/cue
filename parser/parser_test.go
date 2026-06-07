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
