// Package object defines Cue's runtime value types — the things expressions
// evaluate to. It is dynamically typed: every value implements Object.
package object

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/MarcelloLR/cue/ast"
	"github.com/MarcelloLR/cue/token"
)

// ObjectType is the runtime type tag of a value.
type ObjectType string

const (
	INTEGER_OBJ      ObjectType = "INTEGER"
	FLOAT_OBJ        ObjectType = "FLOAT"
	BOOLEAN_OBJ      ObjectType = "BOOLEAN"
	NULL_OBJ         ObjectType = "NULL"
	STRING_OBJ       ObjectType = "STRING"
	RETURN_VALUE_OBJ ObjectType = "RETURN_VALUE"
	ERROR_OBJ        ObjectType = "ERROR"
	FUNCTION_OBJ     ObjectType = "FUNCTION"
	BUILTIN_OBJ      ObjectType = "BUILTIN"
	ARRAY_OBJ        ObjectType = "ARRAY"
	HASH_OBJ         ObjectType = "HASH"
	NAMESPACE_OBJ    ObjectType = "NAMESPACE"
	TOOL_OBJ         ObjectType = "TOOL"
)

// Object is the interface all Cue runtime values implement.
type Object interface {
	Type() ObjectType
	Inspect() string
}

// Integer is a 64-bit signed integer.
type Integer struct{ Value int64 }

func (i *Integer) Type() ObjectType { return INTEGER_OBJ }
func (i *Integer) Inspect() string  { return strconv.FormatInt(i.Value, 10) }

// Float is a 64-bit IEEE-754 floating-point number.
type Float struct{ Value float64 }

func (f *Float) Type() ObjectType { return FLOAT_OBJ }
func (f *Float) Inspect() string  { return strconv.FormatFloat(f.Value, 'g', -1, 64) }

// Boolean is true or false.
type Boolean struct{ Value bool }

func (b *Boolean) Type() ObjectType { return BOOLEAN_OBJ }
func (b *Boolean) Inspect() string  { return strconv.FormatBool(b.Value) }

// Null is the absence of a value.
type Null struct{}

func (n *Null) Type() ObjectType { return NULL_OBJ }
func (n *Null) Inspect() string  { return "null" }

// String is an immutable UTF-8 string.
type String struct{ Value string }

func (s *String) Type() ObjectType { return STRING_OBJ }
func (s *String) Inspect() string  { return s.Value }

// ReturnValue wraps a value being propagated out of a function body by a
// `return` statement. It never escapes into user space.
type ReturnValue struct{ Value Object }

func (rv *ReturnValue) Type() ObjectType { return RETURN_VALUE_OBJ }
func (rv *ReturnValue) Inspect() string  { return rv.Value.Inspect() }

// Error is a runtime error value. It short-circuits evaluation (it propagates
// like ReturnValue) and carries the stable diagnostic Code and source Span that
// the structured-diagnostics contract reports.
type Error struct {
	Code    string
	Message string
	Span    token.Span
}

func (e *Error) Type() ObjectType { return ERROR_OBJ }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }

// Function is a user-defined closure: parameters, body, and the environment it
// was defined in.
type Function struct {
	Parameters []*ast.Identifier
	Body       *ast.BlockStatement
	Env        *Environment
}

func (f *Function) Type() ObjectType { return FUNCTION_OBJ }
func (f *Function) Inspect() string {
	params := make([]string, 0, len(f.Parameters))
	for _, p := range f.Parameters {
		params = append(params, p.String())
	}
	return "fn(" + strings.Join(params, ", ") + ") { ... }"
}

// BuiltinFunction is the Go signature of a native builtin.
type BuiltinFunction func(args ...Object) Object

// Builtin is a native function exposed to Cue programs (e.g. len, print).
type Builtin struct {
	Name string
	Fn   BuiltinFunction
}

func (b *Builtin) Type() ObjectType { return BUILTIN_OBJ }
func (b *Builtin) Inspect() string  { return "builtin " + b.Name }

// Array is an ordered, mutable sequence of values.
type Array struct{ Elements []Object }

func (a *Array) Type() ObjectType { return ARRAY_OBJ }
func (a *Array) Inspect() string {
	els := make([]string, 0, len(a.Elements))
	for _, e := range a.Elements {
		els = append(els, inspectElem(e))
	}
	return "[" + strings.Join(els, ", ") + "]"
}

// Hash is a string-keyed map. Keys preserves insertion order so the value
// inspects deterministically.
type Hash struct {
	Pairs map[string]Object
	Keys  []string
}

// NewHash returns an empty Hash ready for Set.
func NewHash() *Hash {
	return &Hash{Pairs: map[string]Object{}}
}

// Set inserts or updates a key, maintaining insertion order.
func (h *Hash) Set(key string, val Object) {
	if _, ok := h.Pairs[key]; !ok {
		h.Keys = append(h.Keys, key)
	}
	h.Pairs[key] = val
}

func (h *Hash) Type() ObjectType { return HASH_OBJ }
func (h *Hash) Inspect() string {
	pairs := make([]string, 0, len(h.Keys))
	for _, k := range h.Keys {
		pairs = append(pairs, fmt.Sprintf("%q: %s", k, inspectElem(h.Pairs[k])))
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

// inspectElem renders a value as it appears nested inside a collection, quoting
// strings so the structure is unambiguous.
func inspectElem(o Object) string {
	if s, ok := o.(*String); ok {
		return strconv.Quote(s.Value)
	}
	return o.Inspect()
}

// --- Tool & namespace values (DESIGN.md §4, §7) ---
//
// Tools are the only way a Cue program touches the outside world; member access
// on a Namespace (e.g. `http.get`) resolves to a Tool, and calling a Tool routes
// through the runtime's gating + logging pipeline. The concrete tool registry
// and implementations live in higher packages (runtime/*); to keep object free
// of an import cycle, object only declares the ToolImpl *interface* those
// packages satisfy. object is allowed to depend on the stdlib context so an
// Invoke can honour cancellation and timeouts.

// Reversibility classifies whether a tool's effect can be undone, which the
// effect log records so a later phase can compensate/roll back (DESIGN.md §7).
type Reversibility string

const (
	Reversible           Reversibility = "reversible"
	Irreversible         Reversibility = "irreversible"
	ReversibilityUnknown Reversibility = "unknown"
)

// Param describes one positional parameter of a tool. Type is a human/catalog
// hint only — Cue is dynamically typed, so arguments are not statically checked
// against it (DESIGN.md §4).
type Param struct {
	Name string
	Type string
}

// Signature is a tool's parameter list. Variadic tools accept any arg count and
// are therefore exempt from arity checking.
type Signature struct {
	Params   []Param
	Variadic bool
}

// ToolImpl is the Go-side contract a concrete tool satisfies (DESIGN.md §7). It
// lives in object (not runtime/registry) so that object.Tool can wrap it without
// object importing runtime/* — which would be an import cycle.
type ToolImpl interface {
	Name() string // namespaced, e.g. "http.get"
	Signature() Signature
	Reversibility() Reversibility
	Invoke(ctx context.Context, args []Object) (Object, error)
}

// Compensator is an optional interface a ToolImpl may implement to describe the
// inverse action that undoes a successful call (DESIGN.md §4, §7). The invocation
// pipeline captures the returned descriptor into the effect log's compensation
// field after a successful call; rollback (firing it) is a later phase. Keeping
// it optional means most tools — pure transforms, reads — declare nothing, and
// only effectful tools that can be undone opt in.
type Compensator interface {
	// Compensation returns the inverse action (tool name + args) to undo a
	// successful call given its args and result, or ok=false if none applies.
	Compensation(args []Object, result Object) (tool string, compArgs []Object, ok bool)
}

// Tool is a callable runtime value wrapping a registered ToolImpl. Applying it
// runs the invocation pipeline (policy check → effect log → Invoke).
type Tool struct{ Impl ToolImpl }

func (t *Tool) Type() ObjectType { return TOOL_OBJ }
func (t *Tool) Inspect() string  { return "tool " + t.Impl.Name() }

// Namespace groups the tools sharing a prefix (e.g. `http`). Member access on a
// Namespace value resolves the named Tool (DESIGN.md §4).
type Namespace struct {
	Name    string
	Members map[string]*Tool
}

func (n *Namespace) Type() ObjectType { return NAMESPACE_OBJ }
func (n *Namespace) Inspect() string  { return "namespace " + n.Name }

// ToAny converts a runtime value into a plain Go value that marshals to the
// natural JSON shape (int/float/bool/string/null/array/object). It is the bridge
// the run-result envelope and the effect log use to serialize results (DESIGN.md
// §9). Non-data values (functions, builtins, tools, namespaces, errors) render to
// their Inspect string, since they have no JSON-native form.
func ToAny(o Object) any {
	switch v := o.(type) {
	case nil:
		return nil
	case *Null:
		return nil
	case *Integer:
		return v.Value
	case *Float:
		return v.Value
	case *Boolean:
		return v.Value
	case *String:
		return v.Value
	case *Array:
		out := make([]any, 0, len(v.Elements))
		for _, e := range v.Elements {
			out = append(out, ToAny(e))
		}
		return out
	case *Hash:
		out := make(map[string]any, len(v.Keys))
		for _, k := range v.Keys {
			out[k] = ToAny(v.Pairs[k])
		}
		return out
	default:
		return o.Inspect()
	}
}
