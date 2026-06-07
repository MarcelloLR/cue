package evaluator

import (
	"bytes"
	"testing"

	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
)

// TestPrintHonorsInterpOutput pins the agent-contract guarantee that `print`
// writes to the Interp's configured writer, not hard-wired stdout. In --json
// mode the CLI points Out at stderr so the §9 envelope on stdout stays
// machine-parseable; this test is the unit-level backstop for that wiring.
func TestPrintHonorsInterpOutput(t *testing.T) {
	src := `print("hello")` + "\n" + `print("world")`
	p := parser.New(lexer.New(src))
	program := p.ParseProgram()
	if p.HasErrors() {
		t.Fatalf("unexpected parse errors: %v", p.Diagnostics())
	}

	var buf bytes.Buffer
	interp := New(WithOutput(&buf))
	if res := interp.Eval(program, object.NewEnvironment()); isError(res) {
		t.Fatalf("eval error: %s", res.Inspect())
	}

	got := buf.String()
	want := "hello\nworld\n"
	if got != want {
		t.Errorf("print output = %q, want %q", got, want)
	}
}

// TestPrintInParallelHonorsOutput ensures `print` inside a parallel branch
// inherits the parent Interp's writer — otherwise branch output would leak to
// stdout and corrupt the envelope (DESIGN.md §6, §9).
func TestPrintInParallelHonorsOutput(t *testing.T) {
	src := `parallel (x in ["a", "b", "c"]) { print(x) }`
	p := parser.New(lexer.New(src))
	program := p.ParseProgram()
	if p.HasErrors() {
		t.Fatalf("unexpected parse errors: %v", p.Diagnostics())
	}

	var buf bytes.Buffer
	interp := New(WithOutput(&buf))
	if res := interp.Eval(program, object.NewEnvironment()); isError(res) {
		t.Fatalf("eval error: %s", res.Inspect())
	}

	// Output ordering across branches is nondeterministic; assert every branch's
	// line landed in the configured writer (three lines, one per element).
	if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 3 {
		t.Errorf("got %d printed lines, want 3; output=%q", n, buf.String())
	}
}
