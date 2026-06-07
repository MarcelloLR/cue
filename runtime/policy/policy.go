// Package policy is the capability-gating seam (DESIGN.md §7). Every tool call
// is checked against a Policy before it runs, which is what makes tool access —
// the only outside-world access in Cue — safe by construction.
//
// Phase 1 ships a single AllowAll policy but routes every invocation through the
// interface so the pipeline already has the gate in place. Phase 3 replaces
// AllowAll with config-driven rules (allow-list, per-tool Prompt/Deny) and the
// CUE_CAP_* diagnostics that teach an agent its boundaries.
package policy

import "github.com/MarcelloLR/cue/object"

// Decision is a policy's verdict on a single tool call.
type Decision string

const (
	// Allow lets the call proceed.
	Allow Decision = "allow"
	// Deny blocks the call; the runtime reports it as CUE_CAP_* so the agent
	// learns the boundary instead of silently failing.
	Deny Decision = "deny"
	// Prompt requires human confirmation, routed through ask_human in Phase 4.
	// Phase 1 treats it as Allow (the prompt machinery does not exist yet).
	Prompt Decision = "prompt"
)

// Policy maps (tool, args, reversibility) to a Decision (DESIGN.md §7).
type Policy interface {
	Check(toolName string, args []object.Object, rev object.Reversibility) Decision
}

// AllowAll permits every call. It is the Phase 1 default and the baseline a
// real policy specializes from.
type AllowAll struct{}

// Check always returns Allow.
func (AllowAll) Check(toolName string, args []object.Object, rev object.Reversibility) Decision {
	return Allow
}
