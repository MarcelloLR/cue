package evaluator

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MarcelloLR/cue/ast"
	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/llm"
)

// builtins are the native, un-gated functions always in scope. Outside-world
// tools (http, llm, ask_human, …) are a separate, gated registry added in a
// later phase; these are pure-ish language utilities.
// builtins are stateless: their behaviour does not depend on the run. `print` is
// the exception (its output target is run state), so it is constructed per-Interp
// by printBuiltin and is deliberately absent here.
var builtins = map[string]*object.Builtin{
	"len":   {Name: "len", Fn: builtinLen},
	"type":  {Name: "type", Fn: builtinType},
	"str":   {Name: "str", Fn: builtinStr},
	"first": {Name: "first", Fn: builtinFirst},
	"last":  {Name: "last", Fn: builtinLast},
	"rest":  {Name: "rest", Fn: builtinRest},
	"push":  {Name: "push", Fn: builtinPush},
	"keys":  {Name: "keys", Fn: builtinKeys},
	"range": {Name: "range", Fn: builtinRange},
}

// builtinArities maps each builtin to its fixed argument count, or -1 when it is
// variadic (no static arity check). The static checker (package check) reads this
// so the builtin surface is described in exactly one place.
var builtinArities = map[string]int{
	"len":       1,
	"print":     -1, // variadic
	"type":      1,
	"str":       1,
	"first":     1,
	"last":      1,
	"rest":      1,
	"push":      2,
	"keys":      1,
	"range":     1,
	"ask_human": 1,
	"llm":       -1, // llm(prompt) or llm(prompt, opts)
}

// BuiltinArities returns a copy of the builtin name→arity table (arity -1 means
// variadic). It exists so the static checker can validate builtin calls without
// duplicating the list that lives next to the implementations.
func BuiltinArities() map[string]int {
	out := make(map[string]int, len(builtinArities))
	for k, v := range builtinArities {
		out[k] = v
	}
	return out
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

// printBuiltin builds the `print` builtin bound to this run's output writer.
// In --json mode the CLI sets i.Out to stderr so program output never pollutes
// the machine-readable envelope on stdout (DESIGN.md §9).
func (i *Interp) printBuiltin() *object.Builtin {
	return &object.Builtin{Name: "print", Fn: func(args ...object.Object) object.Object {
		parts := make([]string, 0, len(args))
		for _, a := range args {
			parts = append(parts, a.Inspect())
		}
		fmt.Fprintln(i.Out, strings.Join(parts, " "))
		return NULL
	}}
}

// askHumanBuiltin builds the per-Interp `ask_human(prompt)` primitive (DESIGN.md
// §8). It writes the prompt to Out, reads ONE line from In under the global prompt
// lock (so concurrent `parallel` branches never interleave on stdin, §6), records
// the human's answer as an effect — recording the input is what makes a run
// replayable — and returns the line as a String. It is logged but NOT policy-gated:
// ask_human is itself the escalation target of a policy Prompt, not a tool to gate.
func (i *Interp) askHumanBuiltin(node ast.Node) *object.Builtin {
	return &object.Builtin{Name: "ask_human", Fn: func(args ...object.Object) object.Object {
		if len(args) != 1 {
			return newError(node, diag.TypeArgCount,
				"ask_human: wrong number of arguments: want 1, got %d", len(args))
		}
		prompt, ok := args[0].(*object.String)
		if !ok {
			return newError(node, diag.TypeMismatch,
				"ask_human: prompt must be STRING, got %s", args[0].Type())
		}
		rec := i.newRecord(node, "ask_human", args, false)

		// Deterministic replay (DESIGN.md §10): return the recorded human answer
		// instead of reading stdin. Recording the human's input is precisely what
		// makes ask_human replayable (§8), so no prompt is shown and In is never read.
		if i.replaying() {
			return i.replayEffect(node, rec)
		}

		line, err := i.promptLine(prompt.Value + " ")
		if err != nil {
			return i.recordError(node, rec, "ask_human: "+err.Error())
		}
		answer := &object.String{Value: line}
		i.recordOK(rec, answer)
		return answer
	}}
}

// llmBuiltin builds the per-Interp `llm(prompt, [opts])` primitive (DESIGN.md §8).
// It is a gated, logged tool like any other (a Deny on "llm" yields CUE_CAP_* and
// the provider is never called), routed through the same policy/effect pipeline as
// tools. Called as `llm(prompt)` it returns a String; called as `llm(prompt, opts)`
// where opts carries a `schema` (a key→type-name Hash) it returns a Hash validated
// against that shape, with a CUE_TYPE_* error on a missing key or type mismatch (§9).
// The provider is pluggable; the default is an offline deterministic mock.
func (i *Interp) llmBuiltin(node ast.Node) *object.Builtin {
	return &object.Builtin{Name: "llm", Fn: func(args ...object.Object) object.Object {
		if len(args) < 1 || len(args) > 2 {
			return newError(node, diag.TypeArgCount,
				"llm: wrong number of arguments: want 1 or 2, got %d", len(args))
		}
		prompt, ok := args[0].(*object.String)
		if !ok {
			return newError(node, diag.TypeMismatch,
				"llm: prompt must be STRING, got %s", args[0].Type())
		}
		schema, serr := llmSchema(node, args)
		if serr != nil {
			return serr
		}

		rec := i.newRecord(node, "llm", args, true)

		// Deterministic replay (DESIGN.md §10): return the recorded completion
		// instead of calling the provider, skipping the gate. A schema request's
		// recorded result was stored as the validated Hash, so FromAny reconstructs
		// the same shape; a free-form request reconstructs the recorded String.
		if i.replaying() {
			return i.replayEffect(node, rec)
		}

		if denied := i.gate(node, rec, "llm", args, object.Reversible); denied != nil {
			return denied
		}

		start := time.Now()
		res, err := i.LLM.Complete(i.Ctx, llm.Request{Prompt: prompt.Value, Schema: schema})
		rec.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			return i.recordError(node, rec, "llm: "+err.Error())
		}

		out, verr := llmResult(node, schema, res)
		if verr != nil {
			// A schema mismatch is a type error, not a tool failure; record the
			// blocked outcome for audit and surface CUE_TYPE_* to the program.
			msg := verr.Message
			rec.Status = "error"
			rec.Error = &msg
			i.Effects.Append(rec)
			return verr
		}
		i.recordOK(rec, out)
		return out
	}}
}

// llmSchema extracts the optional structured-output schema from an llm call's
// second argument (an opts Hash with a `schema` Hash mapping key → Cue type name).
// It returns (nil, nil) when no schema is requested, or a CUE_TYPE_* error if the
// options/schema are the wrong shape.
func llmSchema(node ast.Node, args []object.Object) (map[string]string, *object.Error) {
	if len(args) < 2 {
		return nil, nil
	}
	opts, ok := args[1].(*object.Hash)
	if !ok {
		return nil, newError(node, diag.TypeMismatch, "llm: options must be HASH, got %s", args[1].Type())
	}
	sv, ok := opts.Pairs["schema"]
	if !ok {
		return nil, nil
	}
	sh, ok := sv.(*object.Hash)
	if !ok {
		return nil, newError(node, diag.TypeMismatch, "llm: schema must be HASH, got %s", sv.Type())
	}
	schema := make(map[string]string, len(sh.Keys))
	for _, k := range sh.Keys {
		tn, ok := sh.Pairs[k].(*object.String)
		if !ok {
			return nil, newError(node, diag.TypeMismatch,
				"llm: schema type for %q must be a STRING type-name, got %s", k, sh.Pairs[k].Type())
		}
		schema[k] = tn.Value
	}
	return schema, nil
}

// llmResult turns a provider Result into a Cue value. Without a schema it is the
// free-form string; with one it is a Hash whose every schema key is present and
// type-matches, else a CUE_TYPE_* error. Keys are emitted in sorted order so the
// resulting Hash is deterministic regardless of provider map iteration.
func llmResult(node ast.Node, schema map[string]string, res llm.Result) (object.Object, *object.Error) {
	if schema == nil {
		return &object.String{Value: res.Text}, nil
	}
	keys := make([]string, 0, len(schema))
	for k := range schema {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := object.NewHash()
	for _, key := range keys {
		raw, ok := res.Object[key]
		if !ok {
			return nil, newError(node, diag.TypeMismatch, "llm: structured output missing key %q", key)
		}
		val := goToObject(raw)
		want := object.ObjectType(strings.ToUpper(schema[key]))
		if val.Type() != want {
			return nil, newError(node, diag.TypeMismatch,
				"llm: structured output key %q: want %s, got %s", key, want, val.Type())
		}
		h.Set(key, val)
	}
	return h, nil
}

// goToObject lifts a Go scalar from a provider Result into a Cue value. The llm
// provider returns plain Go scalars (string/int64/float64/bool); anything else
// falls back to its string form.
func goToObject(v any) object.Object {
	switch x := v.(type) {
	case nil:
		return NULL
	case string:
		return &object.String{Value: x}
	case bool:
		return nativeBool(x)
	case int:
		return &object.Integer{Value: int64(x)}
	case int64:
		return &object.Integer{Value: x}
	case float64:
		return &object.Float{Value: x}
	default:
		return &object.String{Value: fmt.Sprint(x)}
	}
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
