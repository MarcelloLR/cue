package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MarcelloLR/cue/object"
)

// TestFSReversibility documents the reversibility classifications the policy gate
// and rollback substrate depend on (DESIGN.md §4, §7): reads and writes are
// reversible, deletes are not.
func TestFSReversibility(t *testing.T) {
	tests := []struct {
		impl object.ToolImpl
		want object.Reversibility
	}{
		{&fsRead{}, object.Reversible},
		{&fsWrite{}, object.Reversible},
		{&fsDelete{}, object.Irreversible},
	}
	for _, tt := range tests {
		if got := tt.impl.Reversibility(); got != tt.want {
			t.Errorf("%s reversibility = %q, want %q", tt.impl.Name(), got, tt.want)
		}
	}
}

// TestFSWriteIsCompensator confirms fs.write exposes its inverse (fs.delete) so a
// successful write captures a compensation descriptor (DESIGN.md §7).
func TestFSWriteIsCompensator(t *testing.T) {
	var w object.ToolImpl = &fsWrite{}
	comp, ok := w.(object.Compensator)
	if !ok {
		t.Fatalf("fs.write should implement object.Compensator")
	}
	path := &object.String{Value: "/tmp/x"}
	content := &object.String{Value: "data"}
	tool, args, ok := comp.Compensation([]object.Object{path, content}, nil)
	if !ok {
		t.Fatalf("fs.write should describe a compensation")
	}
	if tool != "fs.delete" {
		t.Errorf("compensation tool = %q, want fs.delete", tool)
	}
	if len(args) != 1 || args[0].(*object.String).Value != "/tmp/x" {
		t.Errorf("compensation args = %v, want [/tmp/x]", args)
	}
}

// TestFSWriteThenRead round-trips real bytes through the disk-touching tools using
// a temp dir, then deletes — covering the happy path of all three fs tools.
func TestFSWriteThenReadThenDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	ctx := context.Background()

	w := &fsWrite{}
	out, err := w.Invoke(ctx, []object.Object{
		&object.String{Value: path}, &object.String{Value: "hello"},
	})
	if err != nil {
		t.Fatalf("fs.write: %v", err)
	}
	h := out.(*object.Hash)
	if b := h.Pairs["bytes"].(*object.Integer).Value; b != 5 {
		t.Errorf("bytes = %d, want 5", b)
	}

	r := &fsRead{}
	rout, err := r.Invoke(ctx, []object.Object{&object.String{Value: path}})
	if err != nil {
		t.Fatalf("fs.read: %v", err)
	}
	if got := rout.(*object.Hash).Pairs["content"].(*object.String).Value; got != "hello" {
		t.Errorf("content = %q, want hello", got)
	}

	d := &fsDelete{}
	if _, err := d.Invoke(ctx, []object.Object{&object.String{Value: path}}); err != nil {
		t.Fatalf("fs.delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be gone after fs.delete, stat err = %v", err)
	}
}

// TestFSReadMissingFileIsError confirms an I/O failure surfaces as a Go error
// (which the runtime maps to CUE_TOOL_001).
func TestFSReadMissingFileIsError(t *testing.T) {
	r := &fsRead{}
	_, err := r.Invoke(context.Background(),
		[]object.Object{&object.String{Value: filepath.Join(t.TempDir(), "nope")}})
	if err == nil {
		t.Errorf("reading a missing file should error")
	}
}
