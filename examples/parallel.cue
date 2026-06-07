// Phase 2: the `parallel` map form — concurrency as a keyword.
// Run with:        cue run examples/parallel.cue
// Machine output:  cue run examples/parallel.cue --json   (the §9 envelope)
// Pre-flight:      cue check examples/parallel.cue --json  (no execution)
//
// `parallel (i in xs) { body }` evaluates `body` for each element concurrently
// (goroutines + errgroup + a shared context) and collects the results into an
// array in INPUT ORDER, regardless of completion order (DESIGN.md §6, SPEC §7).
// `strings.upper` is offline and deterministic, so this runs without the network
// and produces stable output.

let xs = ["alpha", "beta", "gamma", "delta"]

// Each branch is its own goroutine; the tool call inside it is gated and logged,
// so every element produces an effect record tagged `branch: "parallel:k"`. The
// expression's value is the ordered array of upper-cased strings.
let shouted = parallel (i in xs) { strings.upper(i) }
print("shouted:", shouted)

// Concurrency is bounded by a default limit (8); an inline `limit =` caps it
// further. Here at most two branches run at once.
let capped = parallel (i in xs, limit = 2) { strings.upper(i) }
print("capped:", capped)

// The program's value is the result of its final expression — the ordered array.
shouted
