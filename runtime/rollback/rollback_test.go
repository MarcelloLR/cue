package rollback

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MarcelloLR/cue/evaluator"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/effectlog"
	"github.com/MarcelloLR/cue/runtime/policy"
	"github.com/MarcelloLR/cue/runtime/registry"
	"github.com/MarcelloLR/cue/runtime/tools"
)

// okWriteRecord builds an effect record shaped like a successful fs.write: status ok,
// reversible, with an fs.delete(path) compensation — exactly what the live pipeline
// captures (DESIGN.md §7). callsite is recovered into the rollback's effect key.
func okWriteRecord(seq int, path, callsite string) effectlog.Record {
	return effectlog.Record{
		Seq:        seq,
		Callsite:   callsite,
		Branch:     "root",
		Tool:       "fs.write",
		Args:       []any{path, "contents"},
		Reversible: true,
		Status:     "ok",
		Result:     map[string]any{"path": path, "bytes": 8},
		Compensation: &effectlog.Compensation{
			Tool: "fs.delete",
			Args: []any{path},
		},
	}
}

// newInterp builds a rollback-branch Interp wired to a fresh recorder and the given
// policy, mirroring how cmdRollback constructs one.
func newInterp(pol policy.Policy) (*evaluator.Interp, *effectlog.Recorder) {
	rec := effectlog.NewRecorder()
	interp := evaluator.New(
		evaluator.WithContext(context.Background()),
		evaluator.WithEffects(rec),
		evaluator.WithPolicy(pol),
		evaluator.WithBranch("rollback"),
	)
	return interp, rec
}

// TestRollbackDeletesWrittenFile is the headline §7 round-trip: a log with a
// successful fs.write (compensation fs.delete) rolls back by deleting the file using
// the REAL fs.delete tool.
func TestRollbackDeletesWrittenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "created.txt")
	if err := os.WriteFile(path, []byte("contents"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	reg := registry.New()
	tools.Register(reg)
	interp, rec := newInterp(policy.AllowAll{})

	records := []effectlog.Record{okWriteRecord(0, path, "1:9")}
	res := Rollback(records, reg, interp)

	if !res.OK {
		t.Errorf("Result.OK = false, want true; %+v", res)
	}
	if res.Considered != 1 || res.Fired != 1 || res.Succeeded != 1 {
		t.Errorf("counts = %+v, want considered/fired/succeeded = 1", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still exists after rollback (stat err = %v); compensation did not delete it", err)
	}

	// The fired inverse is recorded as an effect on the "rollback" branch (DESIGN.md §7).
	effs := rec.Records()
	if len(effs) != 1 {
		t.Fatalf("recorded effects = %d, want 1", len(effs))
	}
	if effs[0].Tool != "fs.delete" {
		t.Errorf("recorded tool = %q, want fs.delete", effs[0].Tool)
	}
	if effs[0].Branch != "rollback" {
		t.Errorf("recorded branch = %q, want rollback", effs[0].Branch)
	}
	if effs[0].Status != "ok" {
		t.Errorf("recorded status = %q, want ok", effs[0].Status)
	}
}

// TestRollbackReverseOrder confirms the log is walked backward: the LAST successful
// effect is undone first. Two files are written; rollback must delete them in reverse
// creation order (b then a).
func TestRollbackReverseOrder(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("contents"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	reg := registry.New()
	tools.Register(reg)

	// A spy ToolImpl that records the order it was asked to delete, then forwards to
	// the real removal so the files actually go away.
	var order []string
	spy := &orderSpy{onDelete: func(p string) { order = append(order, p) }}
	reg.Register(spy)

	interp, _ := newInterp(policy.AllowAll{})
	records := []effectlog.Record{
		okWriteRecord(0, a, "1:9"),
		okWriteRecord(1, b, "2:9"),
	}
	res := Rollback(records, reg, interp)
	if !res.OK || res.Succeeded != 2 {
		t.Fatalf("Result = %+v, want OK with 2 succeeded", res)
	}
	if len(order) != 2 || order[0] != b || order[1] != a {
		t.Errorf("delete order = %v, want [%s %s] (last-created undone first)", order, b, a)
	}
}

// TestRollbackSkipsRecordsWithoutCompensation confirms a record with no compensation
// (an irreversible or read-only call) is skipped: nothing is fired for it.
func TestRollbackSkipsRecordsWithoutCompensation(t *testing.T) {
	reg := registry.New()
	tools.Register(reg)
	interp, rec := newInterp(policy.AllowAll{})

	// An ok record with NO compensation (e.g. an fs.read or an fs.delete) — nothing to
	// undo, so rollback must skip it entirely.
	noComp := effectlog.Record{
		Seq: 0, Callsite: "1:1", Branch: "root",
		Tool: "fs.read", Args: []any{"x"}, Reversible: true, Status: "ok",
	}
	res := Rollback([]effectlog.Record{noComp}, reg, interp)

	if res.Considered != 0 || res.Fired != 0 {
		t.Errorf("counts = %+v, want considered/fired = 0 (record has no compensation)", res)
	}
	if !res.OK {
		t.Errorf("Result.OK = false, want true (a skip is not a failure)")
	}
	if len(rec.Records()) != 0 {
		t.Errorf("recorded %d effects, want 0 (nothing fired)", len(rec.Records()))
	}
}

// TestRollbackSkipsNonOK confirms a non-"ok" record (a failed or denied original call)
// is skipped even if it somehow carries a compensation: only successful effects are
// undone (DESIGN.md §7).
func TestRollbackSkipsNonOK(t *testing.T) {
	reg := registry.New()
	tools.Register(reg)
	interp, _ := newInterp(policy.AllowAll{})

	r := okWriteRecord(0, "/tmp/whatever", "1:9")
	r.Status = "error"
	res := Rollback([]effectlog.Record{r}, reg, interp)
	if res.Considered != 0 || res.Fired != 0 {
		t.Errorf("counts = %+v, want 0 (non-ok record is skipped)", res)
	}
}

// TestRollbackPolicyDenyDoesNotRunInverse confirms a policy Deny on the compensation
// tool blocks the inverse: it is recorded "denied" (CUE_CAP_*) and NOT run, and the
// file the inverse would have deleted survives.
func TestRollbackPolicyDenyDoesNotRunInverse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kept.txt")
	if err := os.WriteFile(path, []byte("contents"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	reg := registry.New()
	tools.Register(reg)

	// Deny fs.delete — the compensation tool — so the inverse must not run.
	pol := &policy.Config{
		Default: policy.Allow,
		Rules:   []policy.Rule{{Tool: "fs.delete", Decision: policy.Deny}},
	}
	interp, rec := newInterp(pol)

	records := []effectlog.Record{okWriteRecord(0, path, "1:9")}
	res := Rollback(records, reg, interp)

	if res.OK {
		t.Errorf("Result.OK = true, want false (a denial is a failure to fully roll back)")
	}
	if res.Denied != 1 || res.Fired != 0 {
		t.Errorf("counts = %+v, want denied=1, fired=0", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file was removed despite policy Deny (stat err = %v); inverse should NOT have run", err)
	}

	effs := rec.Records()
	if len(effs) != 1 || effs[0].Status != "denied" {
		t.Fatalf("want one denied effect, got %+v", effs)
	}
	if effs[0].Tool != "fs.delete" || effs[0].Branch != "rollback" {
		t.Errorf("denied effect = %+v, want fs.delete on rollback branch", effs[0])
	}
}

// TestRollbackContinuesAfterFailure confirms one failing inverse does not strand the
// rest: a compensation whose Invoke errors (deleting an already-gone file) is recorded
// as a failure, but a later (earlier-in-log) inverse still fires. Result.OK is false.
func TestRollbackContinuesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	missing := filepath.Join(dir, "missing.txt") // never created → fs.delete will fail
	if err := os.WriteFile(good, []byte("contents"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	reg := registry.New()
	tools.Register(reg)
	interp, rec := newInterp(policy.AllowAll{})

	// Log order: good first, missing last. Reverse walk fires missing's delete first
	// (which fails), then good's delete (which must still run).
	records := []effectlog.Record{
		okWriteRecord(0, good, "1:9"),
		okWriteRecord(1, missing, "2:9"),
	}
	res := Rollback(records, reg, interp)

	if res.OK {
		t.Errorf("Result.OK = true, want false (one inverse failed)")
	}
	if res.Failed != 1 {
		t.Errorf("Failed = %d, want 1", res.Failed)
	}
	if res.Succeeded != 1 {
		t.Errorf("Succeeded = %d, want 1 (the good inverse must still run after the failure)", res.Succeeded)
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Errorf("good file not removed (stat err = %v); a prior failure stranded the remaining inverse", err)
	}

	effs := rec.Records()
	if len(effs) != 2 {
		t.Fatalf("recorded %d effects, want 2 (both inverses attempted)", len(effs))
	}
}

// TestRollbackUnresolvedCompensationTool confirms a compensation naming a tool the
// registry does not know is recorded as a denied effect and counted as a failure,
// without stranding the rest.
func TestRollbackUnresolvedCompensationTool(t *testing.T) {
	reg := registry.New()
	tools.Register(reg)
	interp, rec := newInterp(policy.AllowAll{})

	r := okWriteRecord(0, "/tmp/x", "1:9")
	r.Compensation.Tool = "fs.unmake" // not a registered tool
	res := Rollback([]effectlog.Record{r}, reg, interp)

	if res.OK {
		t.Errorf("Result.OK = true, want false (unresolved inverse cannot run)")
	}
	if res.Denied != 1 {
		t.Errorf("Denied = %d, want 1", res.Denied)
	}
	effs := rec.Records()
	if len(effs) != 1 || effs[0].Status != "denied" || effs[0].Tool != "fs.unmake" {
		t.Fatalf("want one denied fs.unmake effect, got %+v", effs)
	}
}

// orderSpy is an fs.delete stand-in that observes the deletion order, then performs the
// real removal so the filesystem state matches a genuine rollback.
type orderSpy struct {
	onDelete func(path string)
}

func (o *orderSpy) Name() string { return "fs.delete" }
func (o *orderSpy) Signature() object.Signature {
	return object.Signature{Params: []object.Param{{Name: "path", Type: "string"}}}
}
func (o *orderSpy) Reversibility() object.Reversibility { return object.Irreversible }
func (o *orderSpy) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	p := args[0].(*object.String).Value
	if o.onDelete != nil {
		o.onDelete(p)
	}
	if err := os.Remove(p); err != nil {
		return nil, err
	}
	out := object.NewHash()
	out.Set("path", &object.String{Value: p})
	out.Set("deleted", &object.Boolean{Value: true})
	return out, nil
}
