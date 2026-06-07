package evaluator

import (
	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/token"
)

// rollback.go exposes the single-tool invocation pipeline so the runtime/rollback
// package can fire a compensation through EXACTLY the same gate → effect-log path a
// program's tool call uses (DESIGN.md §7). Rollback walks a prior effect log backward
// and re-invokes each captured inverse action; routing those inverses through the
// shared pipeline (rather than re-implementing policy + recording in the rollback
// package) is what keeps the gate, the CUE_CAP_*/CUE_TOOL_* codes, and the §9 effect
// records identical to a normal call — a compensation is a tool call like any other,
// just driven from the log instead of from source.

// syntheticNode is an ast.Node stand-in for an effect that has no source position —
// a compensation reconstructed from a log record. The effect pipeline keys records by
// callsite() (derived from a node's span), so the synthetic node carries the original
// record's callsite back through Span: callsite(n) reproduces it exactly, and the §9
// record stays consistent with the run that captured the compensation. Any error
// surfaced for a compensation carries this same (recovered) span.
type syntheticNode struct{ sp token.Span }

func (n syntheticNode) TokenLiteral() string { return "" }
func (n syntheticNode) String() string       { return "" }
func (n syntheticNode) Span() token.Span     { return n.sp }

// spanFromCallsite reconstructs a token.Span whose start line:col is the original
// record's callsite (the string callsite() produced). It lets a compensation reuse
// the captured callsite as its effect key so the rollback's records line up, by
// (branch, callsite, occurrence), with the originals they undo (DESIGN.md §7, §10).
func spanFromCallsite(line, col int) token.Span {
	pos := token.Position{Line: line, Col: col}
	return token.Span{Start: pos, End: pos}
}

// InvokeCompensation fires one captured inverse action through the standard tool
// pipeline (gate → effect-log → Invoke → record) and returns the runtime value or an
// *object.Error (DESIGN.md §7). It is the seam rollback uses so a compensation is
// gated, logged, and coded identically to a program-issued tool call:
//
//   - the policy is consulted — a Deny yields CUE_CAP_* and the inverse is NOT run,
//     recorded as a "denied" effect on this Interp's branch (rollback sets it to
//     "rollback");
//   - a successful inverse is recorded "ok"; an Invoke failure is recorded "error"
//     and surfaced as CUE_TOOL_* so the caller can keep rolling back the rest.
//
// The (line, col) recover the original record's callsite so the rollback effect is
// keyed consistently with the effect it undoes. Arity is checked first, exactly as a
// source-level call would be. Replay mode is irrelevant here: rollback always performs
// real inverse effects, so this path never consults i.Replay.
func (i *Interp) InvokeCompensation(tool *object.Tool, args []object.Object, line, col int) object.Object {
	node := syntheticNode{sp: spanFromCallsite(line, col)}
	return i.invokeToolLive(node, tool, args)
}

// RecordCompensationUnresolved logs a "denied" effect for a captured compensation
// whose tool no longer resolves in the registry (DESIGN.md §7, §9). It reuses the same
// recordDenied path a policy block uses, so an unresolvable inverse is auditable in the
// §9 effects rather than vanishing silently — and rollback can keep undoing the rest.
// args are the logged compensation arguments (plain JSON values), reconstructed into
// runtime values only for the record's rendered form.
func (i *Interp) RecordCompensationUnresolved(toolName string, args []any, line, col int) {
	node := syntheticNode{sp: spanFromCallsite(line, col)}
	objs := make([]object.Object, 0, len(args))
	for _, a := range args {
		objs = append(objs, object.FromAny(a))
	}
	rec := i.newRecord(node, toolName, objs, false)
	i.recordDenied(node, rec, diag.NameUnknownTool, "%s: compensation tool not found in registry", toolName)
}
