// Command cue is the Cue interpreter CLI.
//
//	cue run <file.cue>   parse and execute a program
//	cue <file.cue>       shorthand for `cue run`
//	cue repl             start an interactive session
//	cue                  start an interactive session
//
// Diagnostics are printed as `file:line:col: [CODE] message`. A later phase adds
// a --json mode that emits the structured run-result envelope (DESIGN.md §9).
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/evaluator"
	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
	"github.com/MarcelloLR/cue/repl"
)

func main() {
	args := os.Args[1:]

	switch {
	case len(args) == 0, args[0] == "repl":
		fmt.Println("Cue — interactive session. Ctrl-D to exit.")
		repl.Start(os.Stdin, os.Stdout)
	case args[0] == "run":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: cue run <file.cue>")
			os.Exit(2)
		}
		os.Exit(runFile(args[1]))
	case strings.HasSuffix(args[0], ".cue"):
		os.Exit(runFile(args[0]))
	default:
		fmt.Fprintln(os.Stderr, "usage: cue [run] <file.cue> | cue repl")
		os.Exit(2)
	}
}

// runFile parses and executes a program file, returning a process exit code.
func runFile(path string) int {
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cue: %v\n", err)
		return 1
	}

	p := parser.New(lexer.New(string(src)))
	program := p.ParseProgram()

	if p.HasErrors() {
		for _, d := range p.Diagnostics() {
			fmt.Fprintln(os.Stderr, formatDiag(path, d))
		}
		return 1
	}

	result := evaluator.Eval(program, object.NewEnvironment())
	if e, ok := result.(*object.Error); ok {
		fmt.Fprintf(os.Stderr, "%s:%d:%d: [%s] %s\n",
			path, e.Span.Start.Line, e.Span.Start.Col, e.Code, e.Message)
		return 1
	}
	return 0
}

func formatDiag(path string, d diag.Diagnostic) string {
	s := fmt.Sprintf("%s:%d:%d: [%s] %s",
		path, d.Span.Start.Line, d.Span.Start.Col, d.Code, d.Message)
	if d.Hint != "" {
		s += " (hint: " + d.Hint + ")"
	}
	return s
}
