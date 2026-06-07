package evaluator

import (
	"testing"

	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/effectlog"
	"github.com/MarcelloLR/cue/runtime/policy"
)

// TestInvokeCompensationRecordsOK confirms the exported compensation seam fires a tool
// through the standard pipeline: the tool runs, the result comes back, and a §9 effect
// is recorded with the recovered callsite and this Interp's branch (DESIGN.md §7).
func TestInvokeCompensationRecordsOK(t *testing.T) {
	rec := effectlog.NewRecorder()
	interp := New(WithEffects(rec), WithBranch("rollback"))

	m := &mockTool{
		name: "fs.delete",
		sig:  object.Signature{Params: []object.Param{{Name: "path", Type: "string"}}},
		rev:  object.Irreversible,
	}
	out := interp.InvokeCompensation(&object.Tool{Impl: m}, []object.Object{&object.String{Value: "x"}}, 7, 3)

	if isError(out) {
		t.Fatalf("InvokeCompensation errored: %s", out.Inspect())
	}
	if m.invokedCount() != 1 {
		t.Errorf("tool invoked %d times, want 1", m.invokedCount())
	}
	effs := rec.Records()
	if len(effs) != 1 {
		t.Fatalf("recorded %d effects, want 1", len(effs))
	}
	if effs[0].Tool != "fs.delete" || effs[0].Status != "ok" {
		t.Errorf("effect = %+v, want ok fs.delete", effs[0])
	}
	if effs[0].Branch != "rollback" {
		t.Errorf("effect branch = %q, want rollback", effs[0].Branch)
	}
	if effs[0].Callsite != "7:3" {
		t.Errorf("effect callsite = %q, want 7:3 (recovered from line/col)", effs[0].Callsite)
	}
}

// TestInvokeCompensationGated confirms a policy Deny blocks the compensation: the tool
// is NOT invoked, a CUE_CAP error comes back, and a "denied" effect is recorded — the
// same gate a source-level call hits (DESIGN.md §7).
func TestInvokeCompensationGated(t *testing.T) {
	rec := effectlog.NewRecorder()
	interp := New(WithEffects(rec), WithBranch("rollback"), WithPolicy(denyAll{}))

	m := &mockTool{
		name: "fs.delete",
		sig:  object.Signature{Params: []object.Param{{Name: "path", Type: "string"}}},
		rev:  object.Irreversible,
	}
	out := interp.InvokeCompensation(&object.Tool{Impl: m}, []object.Object{&object.String{Value: "x"}}, 1, 1)

	e, ok := out.(*object.Error)
	if !ok {
		t.Fatalf("got %T, want Error", out)
	}
	if e.Code != "CUE_CAP_001" {
		t.Errorf("code = %q, want CUE_CAP_001", e.Code)
	}
	if m.invokedCount() != 0 {
		t.Errorf("denied compensation must not invoke the tool, got %d", m.invokedCount())
	}
	effs := rec.Records()
	if len(effs) != 1 || effs[0].Status != "denied" {
		t.Fatalf("want one denied effect, got %+v", effs)
	}
}

// TestRecordCompensationUnresolved confirms an unresolvable compensation tool is logged
// as a denied effect (CUE_NAME_*) for audit, without running anything.
func TestRecordCompensationUnresolved(t *testing.T) {
	rec := effectlog.NewRecorder()
	interp := New(WithEffects(rec), WithBranch("rollback"), WithPolicy(policy.AllowAll{}))

	interp.RecordCompensationUnresolved("fs.unmake", []any{"x"}, 2, 4)
	effs := rec.Records()
	if len(effs) != 1 {
		t.Fatalf("recorded %d effects, want 1", len(effs))
	}
	if effs[0].Tool != "fs.unmake" || effs[0].Status != "denied" {
		t.Errorf("effect = %+v, want denied fs.unmake", effs[0])
	}
	if effs[0].Callsite != "2:4" {
		t.Errorf("callsite = %q, want 2:4", effs[0].Callsite)
	}
}
