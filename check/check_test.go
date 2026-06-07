package check

import (
	"context"
	"testing"

	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
	"github.com/MarcelloLR/cue/runtime/registry"
)

// fakeTool is a deterministic ToolImpl for checker tests.
type fakeTool struct {
	name string
	sig  object.Signature
}

func (f *fakeTool) Name() string                        { return f.name }
func (f *fakeTool) Signature() object.Signature         { return f.sig }
func (f *fakeTool) Reversibility() object.Reversibility { return object.Reversible }
func (f *fakeTool) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	return object.NewHash(), nil
}

func testRegistry() *registry.Registry {
	r := registry.New()
	r.Register(&fakeTool{name: "http.get", sig: object.Signature{Params: []object.Param{{Name: "url", Type: "string"}}}})
	r.Register(&fakeTool{name: "strings.upper", sig: object.Signature{Params: []object.Param{{Name: "s", Type: "string"}}}})
	return r
}

// fixed builtin arities used in tests (mirrors evaluator.BuiltinArities for the
// ones we exercise; the checker only reads the table it is given).
func testBuiltins() map[string]int {
	return map[string]int{"len": 1, "print": -1, "push": 2}
}

func checkSrc(t *testing.T, src string) []diag.Diagnostic {
	t.Helper()
	p := parser.New(lexer.New(src))
	prog := p.ParseProgram()
	if p.HasErrors() {
		t.Fatalf("unexpected parse errors for %q", src)
	}
	return Program(prog, testRegistry(), testBuiltins())
}

// findCode returns the first diagnostic with the given code, or nil.
func findCode(diags []diag.Diagnostic, code string) *diag.Diagnostic {
	for i := range diags {
		if diags[i].Code == code {
			return &diags[i]
		}
	}
	return nil
}

func TestCheckCleanProgram(t *testing.T) {
	src := `
		let url = "http://x"
		let r = http.get(url)
		let up = strings.upper("hi")
		print(up, len([1, 2]))
	`
	diags := checkSrc(t, src)
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %+v", diags)
	}
}

func TestCheckUnknownIdentifier(t *testing.T) {
	diags := checkSrc(t, `let x = nope`)
	d := findCode(diags, diag.NameUnknownIdent)
	if d == nil {
		t.Fatalf("expected CUE_NAME_001, got %+v", diags)
	}
	if d.Span.Start.Line == 0 {
		t.Errorf("diagnostic missing span")
	}
}

func TestCheckUnknownToolWithSuggestion(t *testing.T) {
	// http.gt is a typo for http.get on a known namespace → CUE_NAME_003.
	diags := checkSrc(t, `let r = http.gt("u")`)
	d := findCode(diags, diag.NameUnknownMember)
	if d == nil {
		t.Fatalf("expected CUE_NAME_003, got %+v", diags)
	}
	if d.Data["namespace"] != "http" {
		t.Errorf("data.namespace = %v, want http", d.Data["namespace"])
	}
	if d.Data["did_you_mean"] != "http.get" {
		t.Errorf("data.did_you_mean = %v, want http.get", d.Data["did_you_mean"])
	}
	if d.Hint == "" {
		t.Errorf("expected a did-you-mean hint")
	}
}

func TestCheckUnknownNamespace(t *testing.T) {
	// htp is not a namespace and not bound → unknown identifier on the object.
	diags := checkSrc(t, `let r = htp.get("u")`)
	if findCode(diags, diag.NameUnknownIdent) == nil {
		t.Fatalf("expected CUE_NAME_001 for htp, got %+v", diags)
	}
}

func TestCheckToolArity(t *testing.T) {
	diags := checkSrc(t, `http.get("a", "b")`)
	d := findCode(diags, diag.TypeArgCount)
	if d == nil {
		t.Fatalf("expected CUE_TYPE_004, got %+v", diags)
	}
}

func TestCheckBuiltinArity(t *testing.T) {
	diags := checkSrc(t, `len(1, 2)`)
	if findCode(diags, diag.TypeArgCount) == nil {
		t.Fatalf("expected CUE_TYPE_004 for len, got %+v", diags)
	}
	// print is variadic — no arity diagnostic.
	if d := findCode(checkSrc(t, `print(1, 2, 3)`), diag.TypeArgCount); d != nil {
		t.Errorf("print is variadic; should not flag arity: %+v", d)
	}
}

func TestCheckParallelCleanAndScoped(t *testing.T) {
	// The loop var is in scope inside the body, and the body/iterable/limit are
	// all walked so a clean parallel form yields no diagnostics.
	src := `
		let xs = ["a", "b"]
		parallel (x in xs, limit = 2) { strings.upper(x) }
	`
	if diags := checkSrc(t, src); len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %+v", diags)
	}
}

func TestCheckParallelWalksBody(t *testing.T) {
	// An unknown identifier inside the body is still reported (the checker
	// descends into the parallel body).
	diags := checkSrc(t, `parallel (x in [1]) { strings.upper(nope) }`)
	if findCode(diags, diag.NameUnknownIdent) == nil {
		t.Fatalf("expected CUE_NAME_001 for nope in body, got %+v", diags)
	}
	// And it validates tool arity inside the body.
	diags = checkSrc(t, `parallel (x in [1]) { strings.upper(x, x) }`)
	if findCode(diags, diag.TypeArgCount) == nil {
		t.Fatalf("expected CUE_TYPE_004 in body, got %+v", diags)
	}
}

func TestCheckShadowingSilencesNamespace(t *testing.T) {
	// A local binding shadows the http namespace name; member access on it is
	// dynamic, so the checker stays silent (conservative, no false positive).
	src := `
		let http = {"get": 1}
		let x = http.get
	`
	diags := checkSrc(t, src)
	if len(diags) != 0 {
		t.Fatalf("shadowed namespace should not be flagged: %+v", diags)
	}
}

func TestCheckParamsAndLetAreKnown(t *testing.T) {
	src := `
		let f = fn(a, b) { a + b }
		let g = fn(x) { f(x, x) }
		g(2)
	`
	if diags := checkSrc(t, src); len(diags) != 0 {
		t.Fatalf("params/let bindings should be known: %+v", diags)
	}
}
