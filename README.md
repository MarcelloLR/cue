# cue

Cue, an agent-oriented programming language — a small tree-walking interpreter
in Go, designed as the action space an AI agent emits to orchestrate tool calls
and workflows.

See [SPEC.md](SPEC.md) for the vision and milestones, and
[DESIGN.md](DESIGN.md) for the detailed language + runtime reference.

## Status: v1 complete (Phases 0–4 + stretch)

The full DESIGN.md v1 bar is implemented, plus the stretch goals:

- **Language core** — `let` bindings and `=` reassignment, integers/floats/
  strings/booleans/`null`, arrays and hashes, operator precedence, `if`/`else`,
  `for x in ...`, first-class `fn` closures and recursion, member access (`a.b`),
  indexing. Every error is a structured diagnostic with a stable code + source span.
- **Tools as first-class** (Phase 1) — a namespaced tool registry, `ns.tool(args)`
  member resolution, the real `http.get` tool, the machine-readable run-result
  envelope, and `cue check` / `cue catalog`.
- **`parallel`** (Phase 2) — the map form `parallel (i in xs) { ... }` (goroutines
  + `errgroup`, ordered results, first-error cancellation, bounded concurrency) and
  the block form `parallel { a = ..; b = .. }`.
- **Effect log + gating** (Phase 3) — an append-only JSONL ledger of every tool
  call, per-tool reversibility with compensation capture, and config-driven
  capability/policy gating (`Allow|Deny|Prompt`, `CUE_CAP_*`).
- **Agent primitives** (Phase 4) — `ask_human`, `retry(n) { ... }`, and a gated,
  mockable `llm(prompt, [opts])` with schema-validated structured output.
- **Stretch** — deterministic replay (`cue replay`), rollback via compensations
  (`cue rollback`), the `parallel` block form, and a queryable SQLite log backend.

## Build & run

```sh
go build -o cue ./cmd/cue

./cue run examples/tour.cue                  # run a program (human output)
./cue run examples/agent.cue --json          # the §9 machine-readable envelope
./cue run prog.cue --log run.jsonl           # stream the effect log to JSONL
./cue run prog.cue --sqlite run.db           # ...or to a queryable SQLite db
./cue run prog.cue --policy cue.policy.json  # gate tool calls against a policy
./cue check prog.cue --json                  # static pre-flight, no execution
./cue catalog --json                         # tool catalog + grammar (agent prompt)
./cue replay run.jsonl prog.cue --json       # deterministic replay from a log
./cue rollback run.jsonl --json              # fire compensations in reverse
./cue repl                                   # interactive session
```

See [`examples/`](examples/) for runnable programs covering each feature.

## Develop

```sh
go test ./...                 # run the test suite
go test -race ./...           # the concurrency-correctness gate
go vet ./... && gofmt -l .    # lint + format check
```

## Package layout

| Package             | Responsibility                                          |
|---------------------|---------------------------------------------------------|
| `token`             | token types + source `Position`/`Span`                  |
| `lexer`             | text → tokens, incl. the newline-terminator rule        |
| `ast`               | typed nodes; every node carries a span                  |
| `parser`            | recursive descent + Pratt; collects diagnostics         |
| `object`            | runtime value types + lexical `Environment`             |
| `evaluator`         | tree-walking `Interp`, builtins, the agent primitives   |
| `diag`              | structured diagnostics + stable codes                   |
| `check`             | static checker (`cue check`): name/arity, no execution  |
| `runtime/registry`  | `Tool` interface + namespaced registry; the catalog     |
| `runtime/tools`     | concrete tools: `http.get`, `strings.upper`, `fs.*`     |
| `runtime/policy`    | capability gating (`Allow`/`Deny`/`Prompt`)             |
| `runtime/effectlog` | the append-only effect ledger + durable sinks           |
| `runtime/envelope`  | the §9 run-result JSON envelope                         |
| `runtime/llm`       | the pluggable `llm()` provider (default: a mock)        |
| `runtime/replay`    | deterministic replay source (`cue replay`)              |
| `runtime/rollback`  | compensation-firing rollback (`cue rollback`)           |
| `runtime/sqlitelog` | the queryable SQLite effect-log backend                 |
| `repl`              | interactive read-eval-print loop                        |
| `cmd/cue`           | the `cue` CLI                                            |
