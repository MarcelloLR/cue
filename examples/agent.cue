// Phase 4: the agent-runtime primitives — llm(), retry(), ask_human().
//
// Run with:        echo "yes" | cue run examples/agent.cue
// Machine output:  echo "yes" | cue run examples/agent.cue --json   (the §9 envelope)
// Pre-flight:      cue check examples/agent.cue --json
//
// This is the SPEC §5 flavor end-to-end: summarize a batch of items concurrently
// with the llm() primitive, retry a flaky-by-nature call, and escalate to a human
// when a threshold is crossed. Everything offline & deterministic: the default
// llm provider is a mock, so the result never depends on a network or an API key.

let issues = ["login is broken", "slow dashboard", "typo in footer"]

// Concurrency + the llm primitive: one summary per issue, gathered in input order.
// Each llm() call is gated and logged, so all three show up in the effect log.
let summaries = parallel (issue in issues) {
    llm("summarize: " + issue)
}
print("summaries:", summaries)

// Structured output: ask the model for a typed shape, validated against a schema.
// A missing key or wrong type would be a CUE_TYPE_* error (the agent can self-repair).
let triage = llm("triage the backlog", {"schema": {"severity": "STRING", "open": "INTEGER"}})
print("triage:", triage)

// retry: re-run a fallible block up to N times, returning the first success. Here
// the body is trivially successful; wrap genuinely flaky/idempotent tool calls.
let count = retry(3) { len(summaries) }
print("count:", count)

// Human-in-the-loop: when there are too many, escalate. The answer is recorded in
// the effect log (which is what makes the run replayable).
let decision = if count > 2 {
    ask_human("More than 2 issues — proceed with auto-triage? (yes/no)")
} else {
    "auto"
}
print("decision:", decision)

// The program's value is the human's (or default) decision.
decision
