# cue

Cue, an agent-oriented programming language — a small tree-walking interpreter
in Go, designed as the action space an AI agent emits to orchestrate tool calls
and workflows.

See [SPEC.md](SPEC.md) for the vision and milestones, and
[DESIGN.md](DESIGN.md) for the detailed language + runtime reference.

## Status: Phase 0 (language core)

The interpreter currently runs the Go-style language core: `let` bindings and
`=` reassignment, integers/floats/strings/booleans/`null`, arrays and hashes,
operator precedence, `if`/`else` expressions, `for x in ...` loops, first-class
`fn` closures and recursion, member access (`a.b`), and indexing. Every error is
reported as a structured diagnostic with a stable code and a source span.

Still to come (see DESIGN.md §13): tool registry + `parallel`, the effect log,
capability gating, the agent runtime primitives, and the `--json` envelope.

## Build & run

```sh
go build -o cue ./cmd/cue

./cue run examples/tour.cue   # run a program
./cue repl                    # interactive session
./cue                         # also starts the REPL
```

## Develop

```sh
go test ./...                 # run the test suite
go vet ./... && gofmt -l .    # lint + format check
```

## Package layout

| Package      | Responsibility                                        |
|--------------|-------------------------------------------------------|
| `token`      | token types + source `Position`/`Span`                |
| `lexer`      | text → tokens, incl. the newline-terminator rule      |
| `ast`        | typed nodes; every node carries a span                |
| `parser`     | recursive descent + Pratt; collects diagnostics       |
| `object`     | runtime value types + lexical `Environment`           |
| `evaluator`  | tree-walking `Eval`, builtins                         |
| `diag`       | structured diagnostics + stable codes                 |
| `repl`       | interactive read-eval-print loop                      |
| `cmd/cue`    | the `cue` CLI                                          |
