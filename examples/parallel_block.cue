// The `parallel` block form / Form 2 (DESIGN.md §3, §6): `parallel { name = expr }`.
//
// Run with:        cue run examples/parallel_block.cue
// Machine output:  cue run examples/parallel_block.cue --json   (the §9 envelope)
//
// Unlike the map form (`parallel (i in xs) { ... }`, which fans one body over a
// collection into an ordered array), the block form runs a fixed set of *named*
// branches concurrently and collects them into a Hash of `{name: result}`. Use it
// when you have a handful of independent calls to make at once and want to name
// each result. Branches run on goroutines + errgroup + a shared context; the first
// error cancels the rest and becomes the value. Each branch's effects are tagged
// `branch: "parallel:<name>"`.

let report = parallel {
    title   = strings.upper("quarterly report")
    summary = llm("summarize the quarter")
    flag    = strings.upper("draft")
}

print("title:", report.title)
print("summary:", report.summary)
print("flag:", report.flag)

// The expression's value is the Hash of named branch results, in source order.
report
