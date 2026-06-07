package evaluator

import (
	"fmt"
	"strings"

	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/object"
)

// builtins are the native, un-gated functions always in scope. Outside-world
// tools (http, llm, ask_human, …) are a separate, gated registry added in a
// later phase; these are pure-ish language utilities.
var builtins = map[string]*object.Builtin{
	"len":   {Name: "len", Fn: builtinLen},
	"print": {Name: "print", Fn: builtinPrint},
	"type":  {Name: "type", Fn: builtinType},
	"str":   {Name: "str", Fn: builtinStr},
	"first": {Name: "first", Fn: builtinFirst},
	"last":  {Name: "last", Fn: builtinLast},
	"rest":  {Name: "rest", Fn: builtinRest},
	"push":  {Name: "push", Fn: builtinPush},
	"keys":  {Name: "keys", Fn: builtinKeys},
	"range": {Name: "range", Fn: builtinRange},
}

func berr(format string, args ...any) *object.Error {
	return &object.Error{Code: diag.RuntimeBuiltin, Message: fmt.Sprintf(format, args...)}
}

func wantArgs(name string, args []object.Object, n int) *object.Error {
	if len(args) != n {
		return &object.Error{Code: diag.TypeArgCount,
			Message: fmt.Sprintf("%s: wrong number of arguments: want %d, got %d", name, n, len(args))}
	}
	return nil
}

func builtinLen(args ...object.Object) object.Object {
	if e := wantArgs("len", args, 1); e != nil {
		return e
	}
	switch arg := args[0].(type) {
	case *object.String:
		return &object.Integer{Value: int64(len([]rune(arg.Value)))}
	case *object.Array:
		return &object.Integer{Value: int64(len(arg.Elements))}
	case *object.Hash:
		return &object.Integer{Value: int64(len(arg.Keys))}
	}
	return berr("len: unsupported type %s", args[0].Type())
}

func builtinPrint(args ...object.Object) object.Object {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, a.Inspect())
	}
	fmt.Println(strings.Join(parts, " "))
	return NULL
}

func builtinType(args ...object.Object) object.Object {
	if e := wantArgs("type", args, 1); e != nil {
		return e
	}
	return &object.String{Value: string(args[0].Type())}
}

func builtinStr(args ...object.Object) object.Object {
	if e := wantArgs("str", args, 1); e != nil {
		return e
	}
	return &object.String{Value: args[0].Inspect()}
}

func builtinFirst(args ...object.Object) object.Object {
	if e := wantArgs("first", args, 1); e != nil {
		return e
	}
	arr, ok := args[0].(*object.Array)
	if !ok {
		return berr("first: argument must be ARRAY, got %s", args[0].Type())
	}
	if len(arr.Elements) == 0 {
		return NULL
	}
	return arr.Elements[0]
}

func builtinLast(args ...object.Object) object.Object {
	if e := wantArgs("last", args, 1); e != nil {
		return e
	}
	arr, ok := args[0].(*object.Array)
	if !ok {
		return berr("last: argument must be ARRAY, got %s", args[0].Type())
	}
	if len(arr.Elements) == 0 {
		return NULL
	}
	return arr.Elements[len(arr.Elements)-1]
}

func builtinRest(args ...object.Object) object.Object {
	if e := wantArgs("rest", args, 1); e != nil {
		return e
	}
	arr, ok := args[0].(*object.Array)
	if !ok {
		return berr("rest: argument must be ARRAY, got %s", args[0].Type())
	}
	if len(arr.Elements) == 0 {
		return NULL
	}
	out := make([]object.Object, len(arr.Elements)-1)
	copy(out, arr.Elements[1:])
	return &object.Array{Elements: out}
}

// builtinPush returns a new array with val appended (the input is unchanged).
func builtinPush(args ...object.Object) object.Object {
	if e := wantArgs("push", args, 2); e != nil {
		return e
	}
	arr, ok := args[0].(*object.Array)
	if !ok {
		return berr("push: first argument must be ARRAY, got %s", args[0].Type())
	}
	out := make([]object.Object, len(arr.Elements)+1)
	copy(out, arr.Elements)
	out[len(arr.Elements)] = args[1]
	return &object.Array{Elements: out}
}

func builtinKeys(args ...object.Object) object.Object {
	if e := wantArgs("keys", args, 1); e != nil {
		return e
	}
	hash, ok := args[0].(*object.Hash)
	if !ok {
		return berr("keys: argument must be HASH, got %s", args[0].Type())
	}
	out := make([]object.Object, 0, len(hash.Keys))
	for _, k := range hash.Keys {
		out = append(out, &object.String{Value: k})
	}
	return &object.Array{Elements: out}
}

// builtinRange returns [0, 1, ..., n-1], handy for counted for-loops.
func builtinRange(args ...object.Object) object.Object {
	if e := wantArgs("range", args, 1); e != nil {
		return e
	}
	n, ok := args[0].(*object.Integer)
	if !ok {
		return berr("range: argument must be INTEGER, got %s", args[0].Type())
	}
	if n.Value < 0 {
		return berr("range: argument must be non-negative, got %d", n.Value)
	}
	out := make([]object.Object, 0, n.Value)
	for i := int64(0); i < n.Value; i++ {
		out = append(out, &object.Integer{Value: i})
	}
	return &object.Array{Elements: out}
}
