// Summarize a batch of bug reports concurrently, then escalate to a human
// if there are too many to auto-file. `parallel`, `llm`, and `ask_human` are
// first-class — this is the kind of program an agent emits as its action space.
//
//   echo "yes" | cue run examples/triage.cue
//   echo "yes" | cue run examples/triage.cue --json    # the §9 effect-log envelope
//
// `llm` uses a built-in deterministic mock by default, so this runs offline with
// no API key and produces stable output.
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
