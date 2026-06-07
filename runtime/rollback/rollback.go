// Package rollback walks a prior effect log backward and fires the captured
// compensation for each successful, reversible call — the "rollback = walk the log
// backward firing compensations" model (DESIGN.md §7). It is the execution half of
// reversibility: Phase 3 captured each reversible call's inverse action into the
// effect log's compensation field; this package replays those inverses, last-created
// first, to undo a run's side effects.
//
// Design (DESIGN.md §7, §9): rollback does NOT re-implement policy gating or effect
// recording. It reconstructs each compensation's tool + args from the log, resolves
// the tool in the registry, and fires it through the SAME invocation pipeline a normal
// tool call uses (evaluator.Interp.InvokeCompensation → invokeToolLive). That single
// reuse buys, for free and consistently: the policy gate (a Deny yields CUE_CAP_* and
// the inverse is never run), the §9 effect record for each inverse (tagged on the
// caller's branch — the CLI uses "rollback"), and the stable CUE_TOOL_*/CUE_CAP_*
// codes. The compensation calls are thus themselves gated + logged, exactly as §7
// requires.
//
// Robustness: one failed or denied inverse must not strand the rest, so rollback
// records the failure and CONTINUES walking the log. The Result reports whether every
// inverse succeeded, so the CLI can surface ok:false when any failed or was denied.
package rollback

import (
	"strconv"
	"strings"

	"github.com/MarcelloLR/cue/evaluator"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/effectlog"
)

// Resolver is the subset of *registry.Registry rollback needs: turn a full
// namespaced tool name into its implementation. Taking the interface (rather than the
// concrete registry) keeps the package testable with a stub registry.
type Resolver interface {
	Resolve(full string) (object.ToolImpl, bool)
}

// Result summarizes a rollback pass for the caller (and, via the CLI, the §9
// envelope). OK is true only when every fired compensation succeeded — a single
// failure or policy denial flips it false so the run envelope reports ok:false.
type Result struct {
	// Considered is the number of "ok" records that carried a compensation and were
	// therefore candidates to undo (records without one are skipped, not counted).
	Considered int
	// Fired is the number of compensations that ran (whether they then succeeded or
	// the tool's Invoke failed); a policy Deny is NOT counted here, as the inverse
	// never ran.
	Fired int
	// Succeeded counts compensations that completed without error.
	Succeeded int
	// Failed counts compensations whose Invoke returned an error (CUE_TOOL_*).
	Failed int
	// Denied counts compensations the policy blocked (CUE_CAP_*); their inverse was
	// not run.
	Denied int
	// OK is true when no compensation failed or was denied.
	OK bool
}

// Rollback walks records in reverse seq order and fires the compensation for each
// successful, reversible record that captured one, returning a Result summary
// (DESIGN.md §7). The interp supplies the gate + effect pipeline and the recorder the
// inverses are logged to; the caller sets its Branch (the CLI uses "rollback") and any
// policy/prompter before calling. The reg resolves each compensation's tool name.
//
// Selection (per §7): only records with status "ok" AND a non-nil Compensation are
// undone. Records without a compensation — irreversible effects (e.g. fs.delete) and
// read-only ones — are skipped, since they registered no inverse. Reverse order is by
// the log's append order (Seq): the last successful effect is undone first, so a chain
// of dependent effects unwinds in the inverse of how it was applied.
//
// Each inverse goes through interp.InvokeCompensation, which gates and records it; a
// Deny is reported (CUE_CAP_*) without running the inverse, and an Invoke error is
// reported (CUE_TOOL_*) — both leave the remaining inverses to be processed, so one
// failure never strands the rest. Resolving the compensation tool itself is also
// logged when it fails (an unknown tool is a "denied" record), keeping every attempt
// auditable in the §9 effects.
func Rollback(records []effectlog.Record, reg Resolver, interp *evaluator.Interp) Result {
	res := Result{OK: true}

	// Walk backward over the log's append order. Records carry their Seq, but the
	// slice is already in append order, so iterating the slice in reverse is the
	// reverse-seq order §7 calls for.
	for k := len(records) - 1; k >= 0; k-- {
		rec := records[k]

		// Skip anything that did not succeed or registered no inverse: irreversible
		// effects and read-only calls capture no compensation and so have nothing to
		// undo (DESIGN.md §7).
		if rec.Status != "ok" || rec.Compensation == nil {
			continue
		}
		res.Considered++

		comp := rec.Compensation
		line, col := parseCallsite(rec.Callsite)

		// Resolve the inverse tool. An unknown compensation tool cannot be fired; log
		// it as a denied effect (so the failed attempt is auditable in the envelope)
		// and keep going — a missing tool must not strand the remaining inverses.
		impl, ok := reg.Resolve(comp.Tool)
		if !ok {
			interp.RecordCompensationUnresolved(comp.Tool, comp.Args, line, col)
			res.Denied++
			res.OK = false
			continue
		}

		// Reconstruct the inverse's arguments from the logged plain JSON values, the
		// same way deterministic replay reconstructs a recorded result (object.FromAny).
		args := fromArgs(comp.Args)

		out := interp.InvokeCompensation(&object.Tool{Impl: impl}, args, line, col)
		if e, ok := out.(*object.Error); ok {
			res.OK = false
			if e.Code == "CUE_CAP_001" || e.Code == "CUE_CAP_002" {
				res.Denied++ // policy blocked it; the inverse never ran
			} else {
				res.Fired++ // it ran but failed (e.g. CUE_TOOL_*)
				res.Failed++
			}
			continue
		}
		res.Fired++
		res.Succeeded++
	}

	return res
}

// fromArgs reconstructs a compensation's argument values from the plain JSON values
// stored in the log, mirroring how deterministic replay rebuilds a recorded result
// (object.FromAny round-trips object.ToAny). It is what lets a logged fs.delete(path)
// inverse become a live call with the original string path (DESIGN.md §7, §10).
func fromArgs(raw []any) []object.Object {
	out := make([]object.Object, 0, len(raw))
	for _, a := range raw {
		out = append(out, object.FromAny(a))
	}
	return out
}

// parseCallsite splits a "line:col" callsite (the string evaluator.callsite produced)
// back into its parts so the fired inverse can reuse the original effect's callsite as
// its own effect key (DESIGN.md §7, §10). A malformed or empty callsite yields (0, 0),
// which still produces a usable record — rollback never fails on an odd log.
func parseCallsite(cs string) (line, col int) {
	i := strings.IndexByte(cs, ':')
	if i < 0 {
		return 0, 0
	}
	line, _ = strconv.Atoi(cs[:i])
	col, _ = strconv.Atoi(cs[i+1:])
	return line, col
}
