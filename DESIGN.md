# Cue — Detailed Design Reference

Companion to `SPEC.md`. `SPEC.md` owns the *vision & milestones* (the *why* and
the build *order*); this document owns the *mechanics* (the *what* and the
*how*). Where the two disagree, `SPEC.md` owns intent and this doc owns detail.

> Locked decisions behind this doc: Go-style brace syntax; dynamically typed
> tree-walking interpreter scaffolded off Monkey (*Writing an Interpreter in
> Go*); the structured-diagnostics contract is a **core pillar**, not a
> stretch; full coverage of language core, concurrency, the tool/effect/
> capability layer, and the agent runtime.

## 0. Status

Design reference for a first version ("v1"). Items marked *phase-later* or
*stretch* are specified here for coherence but are not part of the v1 bar.

## 1. Intended use & design rationale (not a component)

> **This section is rationale, not a build target.** The *agent*, the *harness*,
> and the *observation loop* described below live **outside** the interpreter —
> they are the intended *usage context* that explains why the language is shaped
> the way it is. None of them are things you build. What you build is the
> interpreter + runtime (§2 onward); every Cue program runs by hand, with no
> agent or harness present. The agent-author only (a) motivates *which*
> primitives are first-class (`parallel`, gated tools, `llm`/`ask_human`/`retry`,
> the effect log) and (b) is why the interpreter has a machine-readable `--json`
> output mode (§9). That's the entire cash value of "designed for an agent."

The agent **does not host the interpreter** and never touches Go. The split:

- **Agent (an LLM):** emits **Cue source text**. Its entire interface is
  knowledge of the language: the grammar + the live tool catalog + the
  diagnostics contract. It emits a string; nothing more.
- **Cue runtime (the Go binary this project builds):** owns the tool registry,
  capability gating, effect log, and scheduler. A **harness** feeds it the
  agent's source and returns observations.

CodeAct loop, concrete:

```
agent --emits Cue source--> harness --> Cue runtime (Go)
                                          executes: tools, parallel, gating, log
  ^                                          |
  +---- observations: result + effects + structured diagnostics <----+
```

Consequences (all normative for this doc):

- **Interpreter, not compiler — on purpose.** Actions are ephemeral; you want
  emit→run→observe with no build step. This *is* the runtime-orchestration vs
  compile-time-codegen contrast with zerolang.
- **Two distinct LLM roles, never conflated:** the *outer agent* that **emits**
  a program vs. the **`llm()` primitive** the program **calls** as a tool.
- **Submission modes:** batch whole-program (`cue run prog.cue`, Phase 1) is the
  baseline; a stateful session/REPL where the environment persists across turns
  is a stretch. The book's REPL covers dev use.
- **Go is invisible** to the agent — implementation detail only.

## 2. Lexical structure

- **Comments:** `//` to end of line; `/* ... */` block (optional, phase-later).
- **Statement termination:** newline ends a statement, *except* it is treated as
  line continuation when the previous token is a binary operator, `,`, or an open
  `(`/`[`/`{`, or when nesting depth inside `()`/`[]` > 0. (Go-style implicit
  terminator; explicit `;` also accepted.) This rule is a deliberate, small
  interview story.
- **Identifiers:** `[A-Za-z_][A-Za-z0-9_]*`.
- **Keywords:** `let fn if else for in return true false null parallel retry`.
  (`llm`, `ask_human`, `print`, `len`, … are builtins, not keywords.)
- **Literals:** int (`int64`), **float** (`float64`, new vs Monkey), string
  (double-quoted, `\n`/`\"` escapes; interpolation deferred), `[a, b]` arrays,
  `{k: v}` hashes. `{}` is a hash literal in expression position and a block in
  statement position (Go's resolution rule) — documented to avoid ambiguity.
- **Operators:** `+ - * / %`, `== != < > <= >=`, `&& || !`, `.` (member),
  `[]` (index), `()` (call), `=` (reassign existing binding).
- **Spans:** every token carries `{line, col, offset}` for start/end. Non-
  negotiable from day one — the diagnostics pillar depends on it.

## 3. Grammar (EBNF, informal)

```
program     = { statement } ;
statement   = letStmt | assignStmt | returnStmt | exprStmt | block ;
letStmt     = "let" ident "=" expr ;
assignStmt  = lvalue "=" expr ;                 lvalue = ident { ("." ident) | ("[" expr "]") } ;
returnStmt  = "return" [ expr ] ;
block       = "{" { statement } "}" ;
exprStmt    = expr ;

expr        = ifExpr | forExpr | parallelExpr | retryExpr | fnLit | binary ;
ifExpr      = "if" expr block [ "else" (ifExpr | block) ] ;      // expression-valued
forExpr     = "for" ident "in" expr block ;                      // iteration, value = null
parallelExpr= "parallel" "(" ident "in" expr ")" block           // map form
            | "parallel" block ;                                 // block form (phase-later)
retryExpr   = "retry" "(" expr ")" block ;                       // expr = max attempts
fnLit       = "fn" "(" [ params ] ")" block ;
binary      = unary { binop unary } ;                            // Pratt precedence, see §5
unary       = [ "!" | "-" ] postfix ;
postfix     = primary { call | index | member } ;
call        = "(" [ args ] ")" ;   index = "[" expr "]" ;   member = "." ident ;
primary     = ident | int | float | string | "true" | "false" | "null"
            | "(" expr ")" | arrayLit | hashLit ;
```

`if`/`fn`/`parallel`/`retry` are **expressions** (yield values), as in Monkey.

## 4. Value / type system

Dynamically typed. The `object.Object` set extends Monkey:

| Type | Notes |
|---|---|
| `Integer` (int64) | |
| `Float` (float64) | **new**; int↔float coercion in mixed arithmetic → float |
| `Boolean`, `String`, `Null` | |
| `Array`, `Hash` | `a.b` is sugar for `a["b"]` on a Hash |
| `Function` | closure: params + body + captured env |
| `Builtin` | native fn (`len`, `print`, …); ungated |
| `Error` | runtime error value; **propagates** (short-circuits), carries `{code, message, span}` |
| `Namespace` | **new**; e.g. `github`; `.member` resolves a registered `Tool` |
| `Tool` | **new**; callable; invocation routes through gating + logging |

- **Truthiness:** only `false` and `null` are falsy (Monkey rule retained).
- **Equality:** value equality for scalars/strings; arrays/hashes structural.
- **Member access resolution (`a.b`):** if `a` is a `Hash` → string-key lookup;
  if `a` is a `Namespace` → registry lookup `"a.b"` → `Tool`; else
  `CUE_TYPE_*` diagnostic.
- No first-class futures/promises: `parallel` materializes results into arrays;
  structured concurrency only.

## 5. Evaluation semantics

- **Tree-walking** `Eval(node, env)`; `Environment` is Monkey's enclosed-scope
  map; closures capture the defining env.
- **Precedence** (low→high): `|| < && < == != < < > <= >= < + - < * / % <
  prefix(!,-) < call/index/member`.
- **`return`** uses Monkey's `ReturnValue` unwrap at function boundaries.
- **Errors** propagate as `Error` values (no panics in user programs); at top
  level the runtime serializes them into the run envelope (§9).
- **`for x in xs`**: iterates arrays/hashes/strings; body is a block; the
  expression evaluates to `null` (use `parallel`/builtins to collect). Adds
  `break`/`continue`? Deferred — recursion + `for` suffice for v1.
- **Reassignment (`=`)**: rebinds an existing binding in the nearest enclosing
  scope; assigning an unbound name is `CUE_NAME_*`. `let` always introduces a
  new binding in the current scope.

## 6. Concurrency — `parallel`

Backed by goroutines + `golang.org/x/sync/errgroup` + a shared `context.Context`.

**Form 1 — parallel map (v1):** `parallel (i in xs) { body }`

- Runs `body` for each element concurrently.
- **Returns an array of results in *input order***, regardless of completion
  order. (The "collect results in order" correctness story from SPEC §7.)
- **Error model:** first error cancels the rest via the shared context;
  the first error propagates as the expression's value (`errgroup` semantics).
  A settle-all variant (`parallel.settled`, collect `{ok|err}` per item) is an
  option, not the default.
- **Bounded concurrency:** default limit (config, e.g. 8) via
  `errgroup.SetLimit`; optional inline `parallel(i in xs, limit=4) { ... }`.

**Form 2 — parallel block (phase-later):** `parallel { a = toolA(); b = toolB() }`
runs named branches concurrently, returns a hash of bindings.

**Cross-cutting hazards (specified):**

- **Effect log is goroutine-safe** (single-writer goroutine fed by a channel, or
  a mutex). Each record carries a `branch` path (`root`, `parallel:2`, …) and a
  deterministic call-site key so concurrent runs stay reconstructable/replayable.
- **`ask_human` inside `parallel`** serializes on a global prompt lock (branches
  block while one prompt is outstanding) to avoid interleaved stdin. Documented
  hazard.

## 7. Tool & effect model

**Tool interface (Go):**

```go
type Tool interface {
    Name() string                 // namespaced, e.g. "github.search"
    Signature() Signature         // param names/types — feeds catalog + arity checks
    Reversibility() Reversibility // Reversible | Irreversible | Unknown
    Invoke(ctx context.Context, args []object.Object) (object.Object, error)
}
```

- **Registry:** namespaced names; `github` resolves to a `Namespace`,
  `github.search` to a `Tool`. Registration is the single source of truth that
  also generates the catalog (§9) — never hand-maintained.
- **Invocation pipeline:** resolve → **policy check** → log "start" → `Invoke`
  (with ctx for cancellation/timeouts) → log "result/error" → return.

**Capability / policy gating** (the zerolang capability borrow, at runtime):

- A `Policy` maps (tool, args, reversibility) → `Allow | Deny | Prompt`.
- Declared in a config file (`cue.policy.json`/YAML): allow-list + per-tool
  rules (e.g. allow `http.get`, `Prompt` on `http.post`, deny `fs.delete`).
- `Deny` → `CUE_CAP_*` diagnostic (so the agent *learns* the boundary);
  `Prompt` → routes through `ask_human`.

**Effect log** (audit + rollback + replay substrate):

- One append-only record per gated call:

```json
{ "seq": 42, "ts": "...", "callsite": "f3#0", "branch": "parallel:2",
  "tool": "github.search", "args": { }, "reversible": true,
  "status": "ok|error", "result": { }, "error": { }, "duration_ms": 123,
  "compensation": { } }
```

- **Storage:** **JSONL append-only** by default (durable, greppable, trivially
  replayable). SQLite is the "queryable" upgrade if SQL access is wanted later.
- **Reversibility** declared per-tool; reversible calls may register a
  `compensation` (inverse action) → **rollback** = walk the log backward firing
  compensations. Rollback execution is a stretch but its data is captured now.

## 8. Agent runtime primitives

- **`llm(prompt, [opts])`** — calls a configured provider; returns a string, or,
  with a schema arg, validated structured output (a `Hash` checked against a
  shape) → `CUE_TYPE_*` on mismatch. It is itself a gated/logged tool, and is
  **pluggable/mockable** so tests stay deterministic. Distinct from the outer
  agent (§1).
- **`ask_human(prompt)`** — pauses, reads a line from stdin (or a harness
  channel), returns the string; **logged** (the human input is recorded, which
  is what makes replay possible); serialized under the global prompt lock.
- **`retry(n) { body }`** — re-evaluates `body` up to `n` times while it yields
  an `Error`; optional backoff; returns first success or the last error. Retries
  parse-clean, runtime-fallible blocks only. **Idempotency caveat documented:**
  each attempt logs effects; retrying non-idempotent/irreversible tools is a
  hazard — pairs with reversibility/compensation.

## 9. The agent contract (core pillar) — structured diagnostics & self-repair

The runtime never returns bare prose to the agent. Every run yields a
machine-readable **envelope**:

```json
{ "ok": false, "result": null,
  "diagnostics": [
    { "code": "CUE_NAME_001", "severity": "error",
      "message": "unknown tool 'githab.search'",
      "span": { "start": {"line":1,"col":13}, "end": {"line":1,"col":26} },
      "snippet": "let x = githab.search(...)",
      "hint": "did you mean 'github.search'? run `cue catalog` for tools",
      "data": { "namespace": "githab", "did_you_mean": "github" } } ],
  "effects": [ /* §7 records for this run */ ],
  "trace":   [ /* optional step trace */ ] }
```

**Stable code namespaces:** `CUE_LEX_*`, `CUE_PARSE_*`, `CUE_NAME_*` (unbound
id / unknown tool or namespace member), `CUE_TYPE_*` (arity/shape at a
boundary), `CUE_CAP_*` (policy denied), `CUE_RUNTIME_*`, `CUE_TOOL_*` (tool
failure), `CUE_REPLAY_*`.

- **`cue check prog.cue --json`** runs lex+parse+static checks (unknown
  tools/identifiers, arity) **without executing**, emitting diagnostics only —
  the agent's pre-flight, mirroring `zero check --json`. The parser does **error
  recovery** so one pass reports many diagnostics, not just the first.
- **Self-repair loop (the narrative):** emit → `cue check --json` → read
  codes+hints, edit, re-check → when clean, `cue run --json` → read
  effects/result. Document explicitly.
- **`cue catalog --json`** dumps available tools (names, signatures,
  reversibility, gated?) + a grammar summary — this is exactly what goes in the
  agent's system prompt. Generated from the registry (single source of truth).

## 10. Deterministic replay (designed; stretch to build)

- The only nondeterminism is tool results, `llm()`, and `ask_human()` — all
  logged. Replay re-executes the program but reads recorded outputs from the log
  instead of invoking tools.
- **Stable keying:** key effects by `(branch-path, callsite-id, occurrence)`,
  **not** global `seq` (which is scheduling-dependent under `parallel`). This is
  the subtle correctness point that makes parallel runs replayable.
- `cue replay <log> prog.cue`; a program edit that breaks a call-site match →
  `CUE_REPLAY_*` diagnostic.

## 11. CLI surface

- `cue run prog.cue [--json] [--log path] [--policy path]`
- `cue check prog.cue [--json]`  — static, no execution
- `cue catalog [--json]`        — tool catalog + grammar for the agent prompt
- `cue repl`                    — interactive (from the book)
- `cue replay <log> prog.cue`   — deterministic replay (stretch)

## 12. Go package architecture

```
token      tokens + positions
lexer      text -> tokens (newline-terminator rule)
ast        nodes; every node carries a span
parser     Pratt + recursive descent; diagnostic-collecting, error-recovering
object     value types (§4)
eval       tree-walking evaluator
runtime/   the agent layer:
  registry   Tool interface + namespaced registry  (also emits the catalog)
  policy     capability gating
  effectlog  goroutine-safe JSONL ledger
  scheduler  parallel via errgroup + ctx + limit, ordered results
  diag       diagnostic types, stable codes, JSON envelope
  tools      concrete tools: http, llm, ask_human, fs
cmd/cue    CLI (run / check / catalog / repl / replay)
```

**Foundational decision:** spans on every AST node + a diagnostic collector from
Phase 0 — cheap now, expensive to retrofit, and the whole §9 pillar needs them.

## 13. Build-plan mapping (to SPEC §6 phases)

- **Phase 0 (book + small deltas):** Go-style core; add spans + diagnostic
  collector now; add floats, `for`, member access, `=` reassignment as the first
  peel-off extensions.
- **Phase 1:** registry + member-access resolution + one real tool (`http.get`)
  + run-result envelope + `cue check`/`--json`. Brings the agent contract online
  early (it's a pillar).
- **Phase 2:** `parallel` map form on errgroup with ordered results +
  goroutine-safe effect log.
- **Phase 3:** full effect-log schema + reversibility + policy gating +
  `CUE_CAP_*`.
- **Phase 4:** `ask_human` + `retry` + `llm()`; serialized prompts; structured
  `llm` output.
- **Stretch:** deterministic replay; parallel block form; rollback via
  compensations; SQLite log; stateful agent session.

## 14. Open questions / deferred

- String interpolation; `break`/`continue`; user-defined module/`import` system.
- Static/gradual type annotations (dynamic like Monkey for v1).
- Real LLM provider vs mock for `llm()` during the learning phase.
- parallel block (Form 2) result shape; settle-all default vs opt-in.
- Rollback/compensation execution details.

## Appendix — sanity-checking this design

This is a document, so "verification" means confirming the design is coherent
and buildable before code starts:

1. **Trace the SPEC §5 example end-to-end** through §1→§9: source → lex/parse
   (spans) → resolve `github.search` (registry) → policy check → invoke + log →
   `parallel` map (ordered, errgroup) → `len`/`if` → `ask_human` (serialized,
   logged) → run envelope. Every subsystem must touch it with no gaps.
2. **Check each diagnostic code** has a triggering construct and a `cue check`
   path that emits it without execution.
3. **Real proof comes at Phase 0–1:** implement the Go-style core with spans,
   register `http.get`, and round-trip a program through `cue check --json` then
   `cue run --json`, confirming the envelope shape in §9. That is the first
   executable validation of this document.
