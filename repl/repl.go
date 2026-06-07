// Package repl provides an interactive read-eval-print loop for Cue. It keeps a
// single environment across inputs and buffers lines until brackets balance, so
// multi-line functions and blocks can be entered naturally.
package repl

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/MarcelloLR/cue/evaluator"
	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
)

const prompt = ">> "
const continuation = ".. "

// Start runs the REPL, reading from in and writing to out.
func Start(in io.Reader, out io.Writer) {
	scanner := bufio.NewScanner(in)
	env := object.NewEnvironment()

	var buf strings.Builder
	fmt.Fprint(out, prompt)

	for scanner.Scan() {
		line := scanner.Text()
		buf.WriteString(line)
		buf.WriteString("\n")

		// Keep reading until brackets balance, so blocks can span lines.
		if bracketBalance(buf.String()) > 0 {
			fmt.Fprint(out, continuation)
			continue
		}

		src := buf.String()
		buf.Reset()

		if strings.TrimSpace(src) != "" {
			evalLine(src, env, out)
		}
		fmt.Fprint(out, prompt)
	}
	fmt.Fprintln(out)
}

func evalLine(src string, env *object.Environment, out io.Writer) {
	p := parser.New(lexer.New(src))
	program := p.ParseProgram()

	if p.HasErrors() {
		for _, d := range p.Diagnostics() {
			fmt.Fprintf(out, "  %d:%d: [%s] %s\n", d.Span.Start.Line, d.Span.Start.Col, d.Code, d.Message)
		}
		return
	}

	result := evaluator.Eval(program, env)
	if result == nil {
		return
	}
	if e, ok := result.(*object.Error); ok {
		fmt.Fprintf(out, "  %d:%d: [%s] %s\n", e.Span.Start.Line, e.Span.Start.Col, e.Code, e.Message)
		return
	}
	if _, isNull := result.(*object.Null); !isNull {
		fmt.Fprintln(out, result.Inspect())
	}
}

// bracketBalance returns the net open-bracket count, ignoring bracket
// characters inside string literals. A positive result means more input is
// expected.
func bracketBalance(s string) int {
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		}
	}
	if depth < 0 {
		return 0
	}
	return depth
}
