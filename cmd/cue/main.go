// Command cue is the Cue interpreter CLI (DESIGN.md §11).
//
//	cue run <file.cue> [--json]     parse and execute a program
//	cue check <file.cue> [--json]   static checks only, no execution
//	cue catalog [--json]            available tools + grammar (the agent prompt)
//	cue <file.cue>                  shorthand for `cue run`
//	cue repl                        start an interactive session
//	cue                             start an interactive session
//
// In default mode, diagnostics print as `file:line:col: [CODE] message (hint: …)`
// for humans. With --json the command emits the structured run-result envelope
// (DESIGN.md §9), which is the machine-readable contract an agent reads.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/MarcelloLR/cue/check"
	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/evaluator"
	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
	"github.com/MarcelloLR/cue/repl"
	"github.com/MarcelloLR/cue/runtime/effectlog"
	"github.com/MarcelloLR/cue/runtime/envelope"
	"github.com/MarcelloLR/cue/runtime/registry"
	"github.com/MarcelloLR/cue/runtime/tools"
)

func main() {
	args := os.Args[1:]

	switch {
	case len(args) == 0, args[0] == "repl":
		fmt.Println("Cue — interactive session. Ctrl-D to exit.")
		repl.Start(os.Stdin, os.Stdout)
	case args[0] == "run":
		os.Exit(cmdRun(args[1:]))
	case args[0] == "check":
		os.Exit(cmdCheck(args[1:]))
	case args[0] == "catalog":
		os.Exit(cmdCatalog(args[1:]))
	case strings.HasSuffix(args[0], ".cue"):
		os.Exit(cmdRun(args))
	default:
		fmt.Fprintln(os.Stderr, "usage: cue [run|check] <file.cue> [--json] | cue catalog [--json] | cue repl")
		os.Exit(2)
	}
}

// newRegistry builds the registry with every shipped tool registered. It is the
// single place the CLI turns on the tool surface (DESIGN.md §7).
func newRegistry() *registry.Registry {
	reg := registry.New()
	tools.Register(reg)
	return reg
}

// flag pulls --json (and similar boolean flags) out of args, returning the
// remaining positional args and whether the flag was present.
func popFlag(args []string, name string) (rest []string, present bool) {
	for _, a := range args {
		if a == name {
			present = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, present
}

// cmdRun parses and executes a program file, optionally emitting the JSON
// envelope.
func cmdRun(args []string) int {
	pos, asJSON := popFlag(args, "--json")
	if len(pos) < 1 {
		fmt.Fprintln(os.Stderr, "usage: cue run <file.cue> [--json]")
		return 2
	}
	path := pos[0]
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cue: %v\n", err)
		return 1
	}
	source := string(src)

	p := parser.New(lexer.New(source))
	program := p.ParseProgram()

	// Parse errors short-circuit execution; report them in the chosen mode.
	if p.HasErrors() {
		if asJSON {
			return emitEnvelope(nil, p.Diagnostics(), nil, source)
		}
		for _, d := range p.Diagnostics() {
			fmt.Fprintln(os.Stderr, formatDiag(path, d))
		}
		return 1
	}

	reg := newRegistry()
	env := object.NewEnvironment()
	injectNamespaces(env, reg)

	effects := effectlog.NewRecorder()
	interp := evaluator.New(
		evaluator.WithContext(context.Background()),
		evaluator.WithEffects(effects),
	)
	result := interp.Eval(program, env)

	if asJSON {
		var diags []diag.Diagnostic
		var value object.Object = result
		if e, ok := result.(*object.Error); ok {
			diags = append(diags, errorToDiag(e))
			value = nil
		}
		return emitEnvelope(value, diags, effects.Records(), source)
	}

	// Human mode: print nothing for null, the value otherwise; errors to stderr.
	if e, ok := result.(*object.Error); ok {
		fmt.Fprintln(os.Stderr, formatDiag(path, errorToDiag(e)))
		return 1
	}
	if _, isNull := result.(*object.Null); !isNull && result != nil {
		fmt.Println(result.Inspect())
	}
	return 0
}

// cmdCheck runs lex + parse + static checks without executing (DESIGN.md §9).
func cmdCheck(args []string) int {
	pos, asJSON := popFlag(args, "--json")
	if len(pos) < 1 {
		fmt.Fprintln(os.Stderr, "usage: cue check <file.cue> [--json]")
		return 2
	}
	path := pos[0]
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cue: %v\n", err)
		return 1
	}
	source := string(src)

	p := parser.New(lexer.New(source))
	program := p.ParseProgram()

	// Parser diagnostics (with recovery) come first, then static checks. The
	// parser may have built a partial tree; static checks still run on it.
	diags := append([]diag.Diagnostic{}, p.Diagnostics()...)
	reg := newRegistry()
	diags = append(diags, check.Program(program, reg, evaluator.BuiltinArities())...)

	if asJSON {
		// `cue check` never executes: result is null, effects empty.
		return emitEnvelope(nil, diags, nil, source)
	}

	hasError := false
	for _, d := range diags {
		if d.Severity == diag.SeverityError {
			hasError = true
		}
		fmt.Fprintln(os.Stderr, formatDiag(path, d))
	}
	if hasError {
		return 1
	}
	fmt.Printf("%s: ok (no diagnostics)\n", path)
	return 0
}

// cmdCatalog prints the tool catalog + grammar summary (DESIGN.md §9), generated
// from the registry.
func cmdCatalog(args []string) int {
	_, asJSON := popFlag(args, "--json")
	reg := newRegistry()
	cat := reg.Catalog()

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cat); err != nil {
			fmt.Fprintf(os.Stderr, "cue: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Println("Tools:")
	for _, t := range cat.Tools {
		params := make([]string, 0, len(t.Signature.Params))
		for _, p := range t.Signature.Params {
			params = append(params, p.Name+": "+p.Type)
		}
		sig := strings.Join(params, ", ")
		if t.Signature.Variadic {
			sig += ", ..."
		}
		fmt.Printf("  %-16s (%s)  [%s, gated=%v]\n", t.Name, sig, t.Reversibility, t.Gated)
	}
	fmt.Println("\nGrammar:")
	fmt.Printf("  %s\n", cat.Grammar)
	return 0
}

// injectNamespaces binds each registered namespace as a top-level value so member
// access (e.g. `http.get`) resolves at runtime (DESIGN.md §4).
func injectNamespaces(env *object.Environment, reg *registry.Registry) {
	for name, ns := range reg.Namespaces() {
		env.Set(name, ns)
	}
}

// emitEnvelope writes the §9 run-result envelope to stdout and returns the
// process exit code (0 when ok, 1 otherwise).
func emitEnvelope(result object.Object, diags []diag.Diagnostic, effects []effectlog.Record, source string) int {
	env := envelope.Build(result, diags, effects, source)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		fmt.Fprintf(os.Stderr, "cue: %v\n", err)
		return 1
	}
	if env.OK {
		return 0
	}
	return 1
}

// errorToDiag lifts a runtime *object.Error into a diag.Diagnostic for the
// envelope/human formatter.
func errorToDiag(e *object.Error) diag.Diagnostic {
	return diag.Diagnostic{
		Code:     e.Code,
		Severity: diag.SeverityError,
		Message:  e.Message,
		Span:     e.Span,
	}
}

func formatDiag(path string, d diag.Diagnostic) string {
	s := fmt.Sprintf("%s:%d:%d: [%s] %s",
		path, d.Span.Start.Line, d.Span.Start.Col, d.Code, d.Message)
	if d.Hint != "" {
		s += " (hint: " + d.Hint + ")"
	}
	return s
}
