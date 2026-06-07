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

## A worked example

A small program an agent might emit to triage a batch of bug reports —
summarizing them concurrently, then escalating to a human if there are too many.
`parallel`, `llm`, and `ask_human` are first-class ([`examples/triage.cue`](examples/triage.cue)):

```cue
// Summarize a batch of bug reports concurrently, then escalate to a human
// if there are too many to auto-file.
let issues = ["login button does nothing", "dashboard loads slowly", "typo in footer"]

let summaries = parallel (issue in issues) {
    llm("one-line summary: " + issue)
}

let decision = if len(summaries) > 2 {
    ask_human("3+ issues found — auto-file tickets? (yes/no)")
} else {
    "auto"
}

decision
```

Run it — `llm` uses a built-in deterministic mock by default, so this needs no
network or API key:

```sh
go build -o cue ./cmd/cue
echo "yes" | ./cue run examples/triage.cue
# 3+ issues found — auto-file tickets? (yes/no) yes
```

### The machine-readable contract

The point of Cue is that the runtime answers the agent in **data, not prose**.
Add `--json` for the structured run-result envelope (DESIGN.md §9): the result
plus an effect-log record for every tool / `llm` / `ask_human` call, each tagged
with the concurrency branch it ran on.

```sh
echo "yes" | ./cue run examples/triage.cue --json
```

```jsonc
{
  "ok": true,
  "result": "yes",
  "effects": [
    // each record also has seq, ts, callsite, occurrence, args, reversible, duration_ms
    { "tool": "llm",       "branch": "parallel:0", "status": "ok", "result": "[mock] one-line summary: login button does nothing" },
    { "tool": "llm",       "branch": "parallel:1", "status": "ok", "result": "[mock] one-line summary: dashboard loads slowly" },
    { "tool": "llm",       "branch": "parallel:2", "status": "ok", "result": "[mock] one-line summary: typo in footer" },
    { "tool": "ask_human", "branch": "root",       "status": "ok", "result": "yes" }
  ],
  "diagnostics": []
}
```

### Pre-flight and self-repair

Before running, an agent can statically check what it emitted with `cue check`
(no execution). Misspell a tool and you get a stable code, a source span, and a
did-you-mean hint it can act on:

```sh
$ echo 'let body = http.gt("https://example.com")' > broken.cue
$ ./cue check broken.cue
broken.cue:1:12: [CUE_NAME_003] unknown tool "http.gt" in namespace "http" (hint: did you mean "http.get"? run `cue catalog` for tools)
```

With `--json` that same diagnostic carries `code`, `span`, `snippet`, `hint`, and
`data.did_you_mean` — everything needed to fix the program and re-check. The loop
is: emit → `cue check --json` → repair → `cue run --json` → read effects.
`cue catalog --json` dumps the available tools and grammar to seed the agent's
prompt in the first place.

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
