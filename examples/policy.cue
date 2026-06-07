// policy.cue — demonstrates capability gating (DESIGN.md §7).
//
// Run it against examples/cue.policy.json, which DENIES fs.delete:
//
//   cue run examples/policy.cue --policy examples/cue.policy.json --json
//
// The call below is blocked BEFORE fs.delete's Invoke runs, so the file is never
// touched. The run envelope reports a CUE_CAP_001 diagnostic and a `status:
// "denied"` effect — the agent learns the boundary and the attempt is audited.
fs.delete("/tmp/cue-phase3-should-not-be-deleted")
