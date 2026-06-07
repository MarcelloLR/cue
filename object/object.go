// Package object defines Cue's runtime value types — the things expressions
// evaluate to. It is dynamically typed: every value implements Object.
package object

import (
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
