// Package tools holds the concrete tool implementations Cue ships with
// (DESIGN.md §7, §12). Each satisfies object.ToolImpl and is wired into a
// registry by Register, the single place new tools are turned on.
//
// Phase 1 shipped http.get (the first "it touched the real world" moment) and
// strings.upper (a deterministic, offline tool so examples and tests run without
// the network and with stable output). Phase 3 adds the fs.* family so policy
// gating, reversibility, and compensation are demonstrable end-to-end: fs.read
// (read-only, reversible), fs.write (reversible, with an fs.delete compensation),
// and fs.delete (irreversible). Later phases add llm, ask_human, etc.
package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/registry"
)

// Register turns on every built-in tool by registering it. It is the single
// switchboard for the shipped tool surface; the catalog and namespaces are then
// derived from the registry (DESIGN.md §7).
func Register(reg *registry.Registry) {
	reg.Register(&httpGet{})
	reg.Register(&stringsUpper{})
	reg.Register(&fsRead{})
	reg.Register(&fsWrite{})
	reg.Register(&fsDelete{})
}

// argErr is returned (as a Go error) when a tool is called with the wrong kind
// of argument. The runtime turns it into a CUE_TOOL_001 diagnostic at the call
// site. Arity itself is checked by the evaluator from the Signature.
func argErr(tool string, format string, a ...any) error {
	return fmt.Errorf("%s: %s", tool, fmt.Sprintf(format, a...))
}

// --- http.get ---

// httpGet performs an HTTP GET. A GET is read-only, hence Reversible. It returns
// a Hash {status, body, headers}; transport failures surface as a Go error,
// which the runtime reports as CUE_TOOL_001.
type httpGet struct{}

func (h *httpGet) Name() string { return "http.get" }

func (h *httpGet) Signature() object.Signature {
	return object.Signature{Params: []object.Param{{Name: "url", Type: "string"}}}
}

func (h *httpGet) Reversibility() object.Reversibility { return object.Reversible }

func (h *httpGet) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	url, ok := args[0].(*object.String)
	if !ok {
		return nil, argErr("http.get", "url must be a string, got %s", args[0].Type())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url.Value, nil)
	if err != nil {
		return nil, argErr("http.get", "invalid request: %v", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http.get: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("http.get: reading body: %w", err)
	}

	headers := object.NewHash()
	for k := range resp.Header {
		headers.Set(k, &object.String{Value: resp.Header.Get(k)})
	}

	out := object.NewHash()
	out.Set("status", &object.Integer{Value: int64(resp.StatusCode)})
	out.Set("body", &object.String{Value: string(body)})
	out.Set("headers", headers)
	return out, nil
}

// --- strings.upper ---

// stringsUpper upper-cases a string. It is deterministic and offline — the tool
// examples and tests exercise so verification never depends on the network. As a
// pure transform it has nothing to undo, so it is Reversible.
type stringsUpper struct{}

func (s *stringsUpper) Name() string { return "strings.upper" }

func (s *stringsUpper) Signature() object.Signature {
	return object.Signature{Params: []object.Param{{Name: "s", Type: "string"}}}
}

func (s *stringsUpper) Reversibility() object.Reversibility { return object.Reversible }

func (s *stringsUpper) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	str, ok := args[0].(*object.String)
	if !ok {
		return nil, argErr("strings.upper", "argument must be a string, got %s", args[0].Type())
	}
	return &object.String{Value: strings.ToUpper(str.Value)}, nil
}

// --- fs.read ---

// fsRead reads a file's contents. A read leaves the filesystem untouched, so it
// is Reversible. It returns a Hash {path, content}; an I/O failure surfaces as a
// Go error → CUE_TOOL_001.
type fsRead struct{}

func (f *fsRead) Name() string { return "fs.read" }

func (f *fsRead) Signature() object.Signature {
	return object.Signature{Params: []object.Param{{Name: "path", Type: "string"}}}
}

func (f *fsRead) Reversibility() object.Reversibility { return object.Reversible }

func (f *fsRead) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	path, ok := args[0].(*object.String)
	if !ok {
		return nil, argErr("fs.read", "path must be a string, got %s", args[0].Type())
	}
	data, err := os.ReadFile(path.Value)
	if err != nil {
		return nil, fmt.Errorf("fs.read: %w", err)
	}
	out := object.NewHash()
	out.Set("path", &object.String{Value: path.Value})
	out.Set("content", &object.String{Value: string(data)})
	return out, nil
}

// --- fs.write ---

// fsWrite writes content to a file, creating or truncating it. It is Reversible
// and implements object.Compensator: the inverse of creating a file is deleting
// it, so a successful write captures an fs.delete(path) compensation into the
// effect log (DESIGN.md §7). (A truncating overwrite of an existing file is only
// approximately inverted by delete; capturing the prior contents for a precise
// restore is a Phase 5 refinement.) It returns a Hash {path, bytes}.
type fsWrite struct{}

func (f *fsWrite) Name() string { return "fs.write" }

func (f *fsWrite) Signature() object.Signature {
	return object.Signature{Params: []object.Param{
		{Name: "path", Type: "string"},
		{Name: "content", Type: "string"},
	}}
}

func (f *fsWrite) Reversibility() object.Reversibility { return object.Reversible }

func (f *fsWrite) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	path, ok := args[0].(*object.String)
	if !ok {
		return nil, argErr("fs.write", "path must be a string, got %s", args[0].Type())
	}
	content, ok := args[1].(*object.String)
	if !ok {
		return nil, argErr("fs.write", "content must be a string, got %s", args[1].Type())
	}
	if err := os.WriteFile(path.Value, []byte(content.Value), 0o644); err != nil {
		return nil, fmt.Errorf("fs.write: %w", err)
	}
	out := object.NewHash()
	out.Set("path", &object.String{Value: path.Value})
	out.Set("bytes", &object.Integer{Value: int64(len(content.Value))})
	return out, nil
}

// Compensation returns the inverse action that undoes a successful write: delete
// the file that was created (DESIGN.md §4, §7). Phase 3 captures this descriptor;
// firing it is Phase 5.
func (f *fsWrite) Compensation(args []object.Object, result object.Object) (string, []object.Object, bool) {
	path, ok := args[0].(*object.String)
	if !ok {
		return "", nil, false
	}
	return "fs.delete", []object.Object{path}, true
}

// --- fs.delete ---

// fsDelete removes a file. Deletion cannot be undone (the bytes are gone), so it
// is Irreversible — exactly the kind of effect a policy denies or gates behind a
// prompt. It returns a Hash {path, deleted}.
type fsDelete struct{}

func (f *fsDelete) Name() string { return "fs.delete" }

func (f *fsDelete) Signature() object.Signature {
	return object.Signature{Params: []object.Param{{Name: "path", Type: "string"}}}
}

func (f *fsDelete) Reversibility() object.Reversibility { return object.Irreversible }

func (f *fsDelete) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	path, ok := args[0].(*object.String)
	if !ok {
		return nil, argErr("fs.delete", "path must be a string, got %s", args[0].Type())
	}
	if err := os.Remove(path.Value); err != nil {
		return nil, fmt.Errorf("fs.delete: %w", err)
	}
	out := object.NewHash()
	out.Set("path", &object.String{Value: path.Value})
	out.Set("deleted", &object.Boolean{Value: true})
	return out, nil
}
