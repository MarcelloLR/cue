# Cue — an agent-oriented programming language

> Working title; rename freely. "Cue" = you *cue* an agent to take an action.

A learning project: build a small interpreted programming language whose
primitives and action space are designed for an **AI agent to emit**, in order
to **orchestrate tool calls and workflows** safely and observably. Built in **Go**.

This document is the context/spec for the build. It captures *what* is being
built, *why* the key decisions were made, and the *order* to build it in.

---

## 1. Goal

This is a **vehicle**, not a product. The point is to:

- Learn **Go** (especially its concurrency model).
- Learn how interpreters work end-to-end (lexer → parser → AST → evaluator).
- Learn agent-runtime concepts (tool calls, effect logging, human-in-the-loop).
- Produce a project with strong **interview narratives** — decisions and
  trade-offs to talk through, not just "I built a thing."

It is explicitly *fine* that adjacent things already exist (see §8). For a
learning project, "someone built a serious neighbor" is a gift: a reference to
study and a clear point of contrast to articulate.

## 2. Thesis — why an "agent language"

The frontier insight (from the **CodeAct** paper, Wang et al., arXiv:2402.01030)
is that **code is a better "action space" for agents than JSON**. Instead of an
agent emitting a structured JSON tool call, it writes executable code.

- Measured result: up to ~20% higher success on complex multi-tool tasks, and
  fewer steps, versus JSON/text tool-calling.
- Why: LLMs are trained on enormous amounts of real code but tool-call JSON is a
  synthetic format with sparse training representation. Models are fluent in
  code and clumsy in tool-call JSON.

Two distinct "language for agents" theses now exist in the wild:

| Thesis | Question it answers | Focus |
|---|---|---|
| **Code-generation** (e.g. Vercel's `zerolang`) | What language should an agent write *general software* in, and how should the compiler give it legible feedback to self-repair? | compile-time, building software |
| **Orchestration** (this project) | What small language should an agent *emit as its action space* to safely run *tool calls and workflows*? | runtime, taking actions |

**This project lives in the orchestration box.** That is the deliberate point of
contrast with `zerolang`.

## 3. Locked decisions

**Host language: Go.**

- Primary reason: it's the language worth *learning* here, and learning a
  language through a project you care about beats any tutorial.
- The `parallel` primitive maps directly onto Go's signature feature
  (goroutines + `errgroup`/`WaitGroup`). Tool/LLM calls are **IO-bound**, so
  real concurrency is the right model — and implementing it is the best possible
  Go exercise.
- Compiles to a single self-contained binary; aligns with infra/distributed
  job targets where Go is commonly listed.
- **Acknowledged trade-off:** Go is the weakest *classical*-OOD showcase (no
  inheritance; composition + interfaces + type switches). If "prove textbook
  OOD" were the single most important signal, Java would win. It isn't here.

**Scaffold: _Writing an Interpreter in Go_ by Thorsten Ball** (the "Monkey"
language). Follow it to a working tree-walking interpreter, then **peel off**
(see §6) and add the agent-specific layer as the original contribution.

## 4. Architecture

Standard interpreter pipeline, plus an agent runtime layer:

```
source text
  → lexer        (text → tokens)
  → parser       (tokens → AST; recursive descent + Pratt for precedence)
  → AST          (typed node structs)
  → evaluator    (tree-walk the AST)
        │
        ├── environment        (variable scopes)
        ├── tool registry      (the only way to touch the outside world)
        ├── effect log         (records every side-effecting call)
        └── scheduler          (runs `parallel` children as goroutines)
```

Keep these as clean, separate Go packages so the boundaries are explicit and
easy to talk about: `lexer`, `parser`, `ast`, `eval`, `runtime`.

## 5. The language (sketch)

A program is a sequence of statements. Tool calls, `parallel`, and
human-in-the-loop are **first-class**, which is what makes it agent-native
rather than a generic scripting language.

```
let issues = github.search("label:bug")      # tool call — gated + logged
let summaries = parallel for i in issues:     # concurrency as a keyword
    llm("summarize: " + i.body)               # LLM call as a primitive
if len(summaries) > 10:
    ask_human("too many — proceed?")          # human-in-the-loop primitive
```

What the **runtime** adds beyond a vanilla tree-walker:

- **Capability gating** — tool calls are the only outside-world access, each
  checked against a policy before running. Safe-by-construction; far easier than
  sandboxing a full language.
- **Effect ledger** — every side-effecting call recorded (request, result,
  reversible/irreversible tag). Gives audit + the basis for rollback.
- **Deterministic replay** (stretch) — since the only nondeterminism is LLM
  calls and tool results, record them and replay a run deterministically.

> See **`DESIGN.md`** for the detailed language + runtime reference: execution
> model, full grammar, value/type system, concurrency semantics, the
> tool/effect/capability layer, and the structured-diagnostics contract.

## 6. Build plan / milestones

**Phase 0 — Foundations (follow the book).**
Lexer, parser, evaluator for a Monkey-like core: integers, booleans, strings,
`let`, arithmetic with correct precedence, `if/else`, functions. End state: you
can run small programs and get correct results.

**↳ PEEL-OFF POINT:** once the tree-walking evaluator handles functions and a
basic environment, stop following the book. Everything below is yours.

**Phase 1 — Tool calls as first-class.**
Add a tool registry and a builtin-call mechanism; execute *one* real tool (an
HTTP GET) through the interpreter. First "it touched the real world" moment.

**Phase 2 — The `parallel` primitive.**
Evaluate parallel children as goroutines; gather results with `errgroup`.
This is the core Go-concurrency exercise and a key interview story.

**Phase 3 — Effect log.**
Record every tool call (request, response, timestamp, reversible flag) to a
durable, queryable log. This is the audit/rollback substrate.

**Phase 4 — Human-in-the-loop + retry.**
`ask_human(...)` that pauses for input; `retry` semantics around fallible calls.

**Stretch.**
Deterministic replay from the effect log; an `llm(...)` primitive with typed
structured output; a capability/policy layer for gating.

## 7. The "walls" (i.e. the interview stories)

Build *toward* these deliberately — they're where the good narration lives:

- **Operator precedence** (Phase 0): making `2 + 3 * 4` yield 14, not 20.
  Solved with Pratt parsing / precedence climbing.
- **Concurrency correctness** (Phase 2): coordinating goroutines for `parallel`,
  collecting results in order, propagating the first error. "I implemented
  concurrent tool execution on goroutines" >> "I called Promise.all".
- **Effect-log design** (Phase 3): what to record, how to classify
  reversibility, how an action gets undone.
- **Replay determinism** (stretch): isolating *all* nondeterminism to recorded
  boundaries so a run reproduces exactly.

## 8. Relationship to `zerolang` (study reference)

`github.com/vercel-labs/zerolang` is a real, serious adjacent project (Vercel
Labs; compiler written in C). It is a **different point in the design space** —
a compiled language an agent writes *software* in, whose distinctive idea is
that **the compiler is the agent's API** (stable JSON diagnostics, typed repair
plans, an effect/capability graph for a self-repair loop).

- **Borrow:** the idea of a **structured (JSON) contract** for diagnostics, and
  the **effect/capability graph** concept.
- **Skip:** its scope. It's a general-purpose compiler for code generation; this
  project is a runtime DSL for orchestration. Don't copy its breadth.
- **Narrate:** "I studied zerolang, identified its thesis (code generation,
  compile-time), and built my own take aimed at runtime tool-orchestration
  instead." That shows you can read a real codebase and choose a deliberate
  contrast — a senior-engineer signal.

## 9. References

- **_Writing an Interpreter in Go_** — Thorsten Ball. Primary scaffold (Phase 0).
- **_Crafting Interpreters_** — Robert Nystrom (craftinginterpreters.com). The
  canonical interpreter book; free online; great for concepts even though its
  code is Java/C.
- **CodeAct** — "Executable Code Actions Elicit Better LLM Agents," arXiv:2402.01030.
  The thesis behind §2.
- **zerolang** — github.com/vercel-labs/zerolang. Study reference / point of contrast.

## 10. Scope estimate

- Phase 0: a weekend (faster if following the book closely).
- Phases 1–3: roughly one to two weeks of evenings.
- A genuinely demoable agent-orchestration language: ~2–3 weeks total for a
  first version; stretch goals open-ended.

The bar for "done enough to talk about": an agent can emit a Cue program that
runs at least one real tool call and one `parallel` block, with every effect
recorded in the log.
