// Package tools holds the concrete tool implementations Cue ships with
// (DESIGN.md §7, §12). Each satisfies object.ToolImpl and is wired into a
// registry by Register, the single place new tools are turned on.
//
// Phase 1 ships two: http.get (the first "it touched the real world" moment) and
// strings.upper (a deterministic, offline tool so examples and tests run without
// the network and with stable output). Later phases add llm, ask_human, fs, etc.
package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
