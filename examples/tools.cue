// Phase 1: tools as first-class, gated, logged values.
// Run with:        cue run examples/tools.cue
// Machine output:  cue run examples/tools.cue --json   (the §9 envelope)
// Pre-flight:      cue check examples/tools.cue --json  (no execution)
// Tool surface:    cue catalog --json

// `strings` is a namespace injected by the registry; `.upper` resolves to a
// gated tool. Calling it routes through the policy check and the effect log, so
// this call shows up in the `effects` array of `cue run --json`. It is offline
// and deterministic, so verification never depends on the network.
let shout = strings.upper("hello, tools")
print("upper:", shout)

// Tool results flow through the language like any other value.
let parts = ["one", "two", "three"]
let loud = []
for p in parts {
    loud = push(loud, strings.upper(p))
}
print("loud:", loud)

// `http.get` is the first "it touched the real world" tool. It is left commented
// so this example runs offline; uncomment to fetch a URL. The result is a hash
// { "status": <int>, "body": <string>, "headers": <hash> }.
//
//   let resp = http.get("https://example.com")
//   print("status:", resp.status)

// The program's value is the result of its final expression.
shout
