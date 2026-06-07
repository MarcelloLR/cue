// Package evaluator tree-walks the AST, producing runtime values (object.Object).
//
// Control flow uses two propagating wrapper values: ReturnValue (unwound at
// function boundaries) and Error (short-circuits everything). Errors carry a
// stable code and the source span of the offending node, feeding the
// structured-diagnostics contract.
//
// Evaluation is driven by an Interp, which carries the run-scoped state every
// agent-runtime concern needs: a context.Context for cancellation/timeouts, the
// capability Policy that gates tool calls, and the effect Recorder that logs them
// (DESIGN.md §7). Recursion goes through methods on the same *Interp so that
// state is threaded through the whole run; a package-level Eval wrapper preserves
// the simple Phase 0 entry point for the REPL and tests.
package evaluator

import (
	"context"
	"fmt"
	"time"

	"github.com/MarcelloLR/cue/ast"
	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/effectlog"
	"github.com/MarcelloLR/cue/runtime/policy"
	"github.com/MarcelloLR/cue/runtime/registry"
	"github.com/MarcelloLR/cue/token"
)

// Shared singletons for the immutable values.
var (
	NULL  = &object.Null{}
	TRUE  = &object.Boolean{Value: true}
	FALSE = &object.Boolean{Value: false}
)

// Interp holds the run-scoped state for one evaluation. Splitting this out of the
// free Eval is foundational: every later phase (parallel cancellation, gating,
// logging, prompts) needs a place to hang state, and threading it through methods
// now avoids a disruptive refactor later (DESIGN.md §13).
type Interp struct {
	// Ctx is honoured by tool Invoke calls for cancellation/timeouts.
	Ctx context.Context
	// Policy gates every tool call before it runs (DESIGN.md §7).
	Policy policy.Policy
	// Effects records every gated tool call for the run envelope (§7, §9).
	Effects *effectlog.Recorder
}

// Option configures an Interp at construction.
type Option func(*Interp)

// WithContext sets the context passed to tool invocations.
func WithContext(ctx context.Context) Option { return func(i *Interp) { i.Ctx = ctx } }

// WithPolicy sets the capability policy used to gate tool calls.
func WithPolicy(p policy.Policy) Option { return func(i *Interp) { i.Policy = p } }

// WithEffects sets the effect recorder. Defaults to a fresh recorder.
func WithEffects(r *effectlog.Recorder) Option { return func(i *Interp) { i.Effects = r } }

// New returns an Interp with sensible defaults: a background context, the
// allow-all policy, and a fresh effect recorder. Options override these.
func New(opts ...Option) *Interp {
	i := &Interp{
		Ctx:     context.Background(),
		Policy:  policy.AllowAll{},
		Effects: effectlog.NewRecorder(),
	}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// Eval evaluates a node in env using a default Interp. It preserves the Phase 0
// entry point so the REPL and existing tests keep working unchanged; callers that
// need access to effects or a custom policy build an Interp and call its Eval.
func Eval(node ast.Node, env *object.Environment) object.Object {
	return New().Eval(node, env)
}

// Eval evaluates a node in env. Recursion routes through this method so run state
// is threaded through the whole walk.
func (i *Interp) Eval(node ast.Node, env *object.Environment) object.Object {
	switch node := node.(type) {

	// Statements.
	case *ast.Program:
		return i.evalProgram(node, env)
	case *ast.ExpressionStatement:
		return i.Eval(node.Expr, env)
	case *ast.BlockStatement:
		return i.evalBlockStatement(node, env)
	case *ast.LetStatement:
		val := i.Eval(node.Value, env)
		if isError(val) {
			return val
		}
		env.Set(node.Name.Value, val)
		return NULL
	case *ast.AssignStatement:
		return i.evalAssignStatement(node, env)
	case *ast.ReturnStatement:
		if node.Value == nil {
			return &object.ReturnValue{Value: NULL}
		}
		val := i.Eval(node.Value, env)
		if isError(val) {
			return val
		}
		return &object.ReturnValue{Value: val}

	// Literals.
	case *ast.IntegerLiteral:
		return &object.Integer{Value: node.Value}
	case *ast.FloatLiteral:
		return &object.Float{Value: node.Value}
	case *ast.BooleanLiteral:
		return nativeBool(node.Value)
	case *ast.NullLiteral:
		return NULL
	case *ast.StringLiteral:
		return &object.String{Value: node.Value}
	case *ast.ArrayLiteral:
		elems := i.evalExpressions(node.Elements, env)
		if len(elems) == 1 && isError(elems[0]) {
			return elems[0]
		}
		return &object.Array{Elements: elems}
	case *ast.HashLiteral:
		return i.evalHashLiteral(node, env)
	case *ast.FunctionLiteral:
		return &object.Function{Parameters: node.Parameters, Body: node.Body, Env: env}

	// Expressions.
	case *ast.Identifier:
		return evalIdentifier(node, env)
	case *ast.PrefixExpression:
		right := i.Eval(node.Right, env)
		if isError(right) {
			return right
		}
		return evalPrefixExpression(node, right)
	case *ast.InfixExpression:
		return i.evalInfixExpression(node, env)
	case *ast.IfExpression:
		return i.evalIfExpression(node, env)
	case *ast.ForExpression:
		return i.evalForExpression(node, env)
	case *ast.CallExpression:
		return i.evalCallExpression(node, env)
	case *ast.IndexExpression:
		left := i.Eval(node.Left, env)
		if isError(left) {
			return left
		}
		index := i.Eval(node.Index, env)
		if isError(index) {
			return index
		}
		return evalIndexExpression(node, left, index)
	case *ast.MemberExpression:
		return i.evalMemberExpression(node, env)
	}

	return newError(node, diag.RuntimeBuiltin, "evaluation not implemented for %T", node)
}

func (i *Interp) evalProgram(program *ast.Program, env *object.Environment) object.Object {
	var result object.Object = NULL
	for _, stmt := range program.Statements {
		result = i.Eval(stmt, env)
		switch result := result.(type) {
		case *object.ReturnValue:
			return result.Value
		case *object.Error:
			return result
		}
	}
	return result
}

func (i *Interp) evalBlockStatement(block *ast.BlockStatement, env *object.Environment) object.Object {
	var result object.Object = NULL
	for _, stmt := range block.Statements {
		result = i.Eval(stmt, env)
		if result != nil {
			rt := result.Type()
			if rt == object.RETURN_VALUE_OBJ || rt == object.ERROR_OBJ {
				return result
			}
		}
	}
	return result
}

func (i *Interp) evalAssignStatement(node *ast.AssignStatement, env *object.Environment) object.Object {
	val := i.Eval(node.Value, env)
	if isError(val) {
		return val
	}

	switch target := node.Target.(type) {
	case *ast.Identifier:
		if !env.Assign(target.Value, val) {
			return newError(target, diag.NameUnknownIdent,
				"cannot assign to undefined variable %q (use `let` to declare it)", target.Value)
		}
		return NULL

	case *ast.IndexExpression:
		left := i.Eval(target.Left, env)
		if isError(left) {
			return left
		}
		index := i.Eval(target.Index, env)
		if isError(index) {
			return index
		}
		return evalIndexAssign(node, left, index, val)

	case *ast.MemberExpression:
		obj := i.Eval(target.Object, env)
		if isError(obj) {
			return obj
		}
		hash, ok := obj.(*object.Hash)
		if !ok {
			return newError(target, diag.TypeMismatch,
				"cannot set member .%s on %s", target.Property, obj.Type())
		}
		hash.Set(target.Property, val)
		return NULL
	}

	return newError(node, diag.ParseInvalidAssign, "invalid assignment target")
}

func evalIndexAssign(node ast.Node, left, index, val object.Object) object.Object {
	switch container := left.(type) {
	case *object.Array:
		i, ok := index.(*object.Integer)
		if !ok {
			return newError(node, diag.TypeNotIndexable, "array index must be INTEGER, got %s", index.Type())
		}
		if i.Value < 0 || i.Value >= int64(len(container.Elements)) {
			return newError(node, diag.TypeNotIndexable, "array index %d out of range (len %d)", i.Value, len(container.Elements))
		}
		container.Elements[i.Value] = val
		return NULL
	case *object.Hash:
		key, ok := index.(*object.String)
		if !ok {
			return newError(node, diag.TypeBadKey, "hash key must be STRING, got %s", index.Type())
		}
		container.Set(key.Value, val)
		return NULL
	}
	return newError(node, diag.TypeNotIndexable, "cannot index-assign into %s", left.Type())
}

func evalIdentifier(node *ast.Identifier, env *object.Environment) object.Object {
	if val, ok := env.Get(node.Value); ok {
		return val
	}
	if b, ok := builtins[node.Value]; ok {
		return b
	}
	return newError(node, diag.NameUnknownIdent, "unknown identifier %q", node.Value)
}

func evalPrefixExpression(node *ast.PrefixExpression, right object.Object) object.Object {
	switch node.Operator {
	case "!":
		return nativeBool(!isTruthy(right))
	case "-":
		switch r := right.(type) {
		case *object.Integer:
			return &object.Integer{Value: -r.Value}
		case *object.Float:
			return &object.Float{Value: -r.Value}
		}
		return newError(node, diag.TypeMismatch, "unknown operator: -%s", right.Type())
	}
	return newError(node, diag.TypeMismatch, "unknown operator: %s%s", node.Operator, right.Type())
}

func (i *Interp) evalInfixExpression(node *ast.InfixExpression, env *object.Environment) object.Object {
	// Logical operators short-circuit, so evaluate the left side first.
	if node.Operator == "&&" || node.Operator == "||" {
		left := i.Eval(node.Left, env)
		if isError(left) {
			return left
		}
		if node.Operator == "&&" && !isTruthy(left) {
			return FALSE
		}
		if node.Operator == "||" && isTruthy(left) {
			return TRUE
		}
		right := i.Eval(node.Right, env)
		if isError(right) {
			return right
		}
		return nativeBool(isTruthy(right))
	}

	left := i.Eval(node.Left, env)
	if isError(left) {
		return left
	}
	right := i.Eval(node.Right, env)
	if isError(right) {
		return right
	}

	switch {
	case isNumeric(left) && isNumeric(right):
		return evalNumericInfix(node, left, right)
	case left.Type() == object.STRING_OBJ && right.Type() == object.STRING_OBJ:
		return evalStringInfix(node, left.(*object.String), right.(*object.String))
	case node.Operator == "==":
		return nativeBool(objectsEqual(left, right))
	case node.Operator == "!=":
		return nativeBool(!objectsEqual(left, right))
	case left.Type() != right.Type():
		return newError(node, diag.TypeMismatch, "type mismatch: %s %s %s", left.Type(), node.Operator, right.Type())
	default:
		return newError(node, diag.TypeMismatch, "unknown operator: %s %s %s", left.Type(), node.Operator, right.Type())
	}
}

func evalNumericInfix(node *ast.InfixExpression, left, right object.Object) object.Object {
	// If either operand is a float, compute in float; otherwise stay integer.
	_, lf := left.(*object.Float)
	_, rf := right.(*object.Float)
	if lf || rf {
		return evalFloatInfix(node, toFloat(left), toFloat(right))
	}
	return evalIntegerInfix(node, left.(*object.Integer).Value, right.(*object.Integer).Value)
}

func evalIntegerInfix(node *ast.InfixExpression, l, r int64) object.Object {
	switch node.Operator {
	case "+":
		return &object.Integer{Value: l + r}
	case "-":
		return &object.Integer{Value: l - r}
	case "*":
		return &object.Integer{Value: l * r}
	case "/":
		if r == 0 {
			return newError(node, diag.RuntimeDivByZero, "division by zero")
		}
		return &object.Integer{Value: l / r}
	case "%":
		if r == 0 {
			return newError(node, diag.RuntimeDivByZero, "modulo by zero")
		}
		return &object.Integer{Value: l % r}
	case "<":
		return nativeBool(l < r)
	case ">":
		return nativeBool(l > r)
	case "<=":
		return nativeBool(l <= r)
	case ">=":
		return nativeBool(l >= r)
	case "==":
		return nativeBool(l == r)
	case "!=":
		return nativeBool(l != r)
	}
	return newError(node, diag.TypeMismatch, "unknown operator: INTEGER %s INTEGER", node.Operator)
}

func evalFloatInfix(node *ast.InfixExpression, l, r float64) object.Object {
	switch node.Operator {
	case "+":
		return &object.Float{Value: l + r}
	case "-":
		return &object.Float{Value: l - r}
	case "*":
		return &object.Float{Value: l * r}
	case "/":
		if r == 0 {
			return newError(node, diag.RuntimeDivByZero, "division by zero")
		}
		return &object.Float{Value: l / r}
	case "<":
		return nativeBool(l < r)
	case ">":
		return nativeBool(l > r)
	case "<=":
		return nativeBool(l <= r)
	case ">=":
		return nativeBool(l >= r)
	case "==":
		return nativeBool(l == r)
	case "!=":
		return nativeBool(l != r)
	}
	return newError(node, diag.TypeMismatch, "unknown operator: FLOAT %s FLOAT", node.Operator)
}

func evalStringInfix(node *ast.InfixExpression, l, r *object.String) object.Object {
	switch node.Operator {
	case "+":
		return &object.String{Value: l.Value + r.Value}
	case "==":
		return nativeBool(l.Value == r.Value)
	case "!=":
		return nativeBool(l.Value != r.Value)
	case "<":
		return nativeBool(l.Value < r.Value)
	case ">":
		return nativeBool(l.Value > r.Value)
	case "<=":
		return nativeBool(l.Value <= r.Value)
	case ">=":
		return nativeBool(l.Value >= r.Value)
	}
	return newError(node, diag.TypeMismatch, "unknown operator: STRING %s STRING", node.Operator)
}

func (i *Interp) evalIfExpression(node *ast.IfExpression, env *object.Environment) object.Object {
	cond := i.Eval(node.Condition, env)
	if isError(cond) {
		return cond
	}
	if isTruthy(cond) {
		return i.Eval(node.Consequence, env)
	}
	if node.Alternative != nil {
		return i.Eval(node.Alternative, env)
	}
	return NULL
}

func (i *Interp) evalForExpression(node *ast.ForExpression, env *object.Environment) object.Object {
	iterable := i.Eval(node.Iterable, env)
	if isError(iterable) {
		return iterable
	}

	loopEnv := object.NewEnclosedEnvironment(env)
	run := func(item object.Object) object.Object {
		loopEnv.Set(node.Var.Value, item)
		result := i.Eval(node.Body, loopEnv)
		if result != nil {
			if result.Type() == object.RETURN_VALUE_OBJ || result.Type() == object.ERROR_OBJ {
				return result
			}
		}
		return nil
	}

	switch it := iterable.(type) {
	case *object.Array:
		for _, el := range it.Elements {
			if r := run(el); r != nil {
				return r
			}
		}
	case *object.String:
		for _, ch := range it.Value {
			if r := run(&object.String{Value: string(ch)}); r != nil {
				return r
			}
		}
	case *object.Hash:
		for _, k := range it.Keys {
			if r := run(&object.String{Value: k}); r != nil {
				return r
			}
		}
	default:
		return newError(node, diag.TypeNotIterable, "%s is not iterable", iterable.Type())
	}
	return NULL
}

func (i *Interp) evalCallExpression(node *ast.CallExpression, env *object.Environment) object.Object {
	fn := i.Eval(node.Function, env)
	if isError(fn) {
		return fn
	}
	args := i.evalExpressions(node.Arguments, env)
	if len(args) == 1 && isError(args[0]) {
		return args[0]
	}
	return i.applyFunction(node, fn, args)
}

func (i *Interp) applyFunction(node *ast.CallExpression, fn object.Object, args []object.Object) object.Object {
	switch fn := fn.(type) {
	case *object.Function:
		if len(args) != len(fn.Parameters) {
			return newError(node, diag.TypeArgCount,
				"wrong number of arguments: want %d, got %d", len(fn.Parameters), len(args))
		}
		extended := object.NewEnclosedEnvironment(fn.Env)
		for idx, p := range fn.Parameters {
			extended.Set(p.Value, args[idx])
		}
		result := i.Eval(fn.Body, extended)
		if rv, ok := result.(*object.ReturnValue); ok {
			return rv.Value
		}
		return result
	case *object.Builtin:
		result := fn.Fn(args...)
		if result == nil {
			return NULL
		}
		// Attach the call site's span to builtin errors that lack one.
		if e, ok := result.(*object.Error); ok && e.Span == (token.Span{}) {
			e.Span = node.Span()
		}
		return result
	case *object.Tool:
		return i.invokeTool(node, fn, args)
	}
	return newError(node, diag.TypeNotCallable, "not callable: %s", fn.Type())
}

// invokeTool runs the tool invocation pipeline (DESIGN.md §7): arity check →
// policy gate → effect-log the call → Invoke (with the run context) → log the
// result/error → return the value. Effects are recorded into i.Effects keyed by a
// deterministic call-site so the run stays reconstructable.
func (i *Interp) invokeTool(node *ast.CallExpression, tool *object.Tool, args []object.Object) object.Object {
	impl := tool.Impl
	sig := impl.Signature()

	// Arity: variadic tools accept any count; otherwise the call must match.
	if !sig.Variadic && len(args) != len(sig.Params) {
		return newError(node, diag.TypeArgCount,
			"%s: wrong number of arguments: want %d, got %d", impl.Name(), len(sig.Params), len(args))
	}

	// Policy gate. Deny is a learnable boundary (CUE_CAP_*). Prompt is treated as
	// Allow in Phase 1 — the ask_human machinery arrives in Phase 4.
	switch i.Policy.Check(impl.Name(), args, impl.Reversibility()) {
	case policy.Deny:
		return newError(node, diag.CapDenied,
			"%s: denied by policy", impl.Name())
	case policy.Prompt:
		// TODO(phase 4): route Prompt through ask_human under the prompt lock.
	}

	// Record the call and measure it. The callsite is the deterministic key the
	// effect log (and, later, replay) uses instead of the scheduling-dependent
	// global sequence number.
	rec := effectlog.Record{
		Callsite:   callsite(node),
		Tool:       impl.Name(),
		Args:       renderArgs(args),
		Reversible: impl.Reversibility() == object.Reversible,
	}

	start := time.Now()
	result, err := impl.Invoke(i.Ctx, args)
	rec.DurationMs = time.Since(start).Milliseconds()

	if err != nil {
		msg := err.Error()
		rec.Status = "error"
		rec.Error = &msg
		i.Effects.Append(rec)
		return newError(node, diag.ToolFailure, "%s", msg)
	}

	if result == nil {
		result = NULL
	}
	rec.Status = "ok"
	rec.Result = object.ToAny(result)
	i.Effects.Append(rec)
	return result
}

// callsite derives a deterministic key from a call node's start position. It is
// stable across runs for the same source, which is what the effect log and
// replay need (DESIGN.md §7, §10).
func callsite(node ast.Node) string {
	sp := node.Span()
	return fmt.Sprintf("%d:%d", sp.Start.Line, sp.Start.Col)
}

// renderArgs converts call arguments to JSON-friendly values for the effect log.
func renderArgs(args []object.Object) []any {
	out := make([]any, 0, len(args))
	for _, a := range args {
		out = append(out, object.ToAny(a))
	}
	return out
}

func evalIndexExpression(node *ast.IndexExpression, left, index object.Object) object.Object {
	switch container := left.(type) {
	case *object.Array:
		i, ok := index.(*object.Integer)
		if !ok {
			return newError(node, diag.TypeNotIndexable, "array index must be INTEGER, got %s", index.Type())
		}
		if i.Value < 0 || i.Value >= int64(len(container.Elements)) {
			return NULL
		}
		return container.Elements[i.Value]
	case *object.String:
		i, ok := index.(*object.Integer)
		if !ok {
			return newError(node, diag.TypeNotIndexable, "string index must be INTEGER, got %s", index.Type())
		}
		runes := []rune(container.Value)
		if i.Value < 0 || i.Value >= int64(len(runes)) {
			return NULL
		}
		return &object.String{Value: string(runes[i.Value])}
	case *object.Hash:
		key, ok := index.(*object.String)
		if !ok {
			return newError(node, diag.TypeBadKey, "hash key must be STRING, got %s", index.Type())
		}
		if v, ok := container.Pairs[key.Value]; ok {
			return v
		}
		return NULL
	}
	return newError(node, diag.TypeNotIndexable, "%s is not indexable", left.Type())
}

// evalMemberExpression resolves `a.b`. A Hash does a string-key lookup (Phase 0
// behavior); a Namespace resolves the named Tool, reporting CUE_NAME_003 with a
// did-you-mean hint when the member is unknown (DESIGN.md §4, §9).
func (i *Interp) evalMemberExpression(node *ast.MemberExpression, env *object.Environment) object.Object {
	obj := i.Eval(node.Object, env)
	if isError(obj) {
		return obj
	}
	switch container := obj.(type) {
	case *object.Hash:
		if v, ok := container.Pairs[node.Property]; ok {
			return v
		}
		return NULL
	case *object.Namespace:
		if tool, ok := container.Members[node.Property]; ok {
			return tool
		}
		e := newError(node, diag.NameUnknownMember,
			"unknown tool %q in namespace %q", container.Name+"."+node.Property, container.Name)
		if s := suggestMember(container, node.Property); s != "" {
			e.Message += fmt.Sprintf(" (did you mean %q?)", container.Name+"."+s)
		}
		return e
	}
	return newError(node, diag.NameUnknownTool, "cannot access member .%s on %s", node.Property, obj.Type())
}

func (i *Interp) evalHashLiteral(node *ast.HashLiteral, env *object.Environment) object.Object {
	hash := object.NewHash()
	for _, pair := range node.Pairs {
		key := i.Eval(pair.Key, env)
		if isError(key) {
			return key
		}
		ks, ok := key.(*object.String)
		if !ok {
			return newError(node, diag.TypeBadKey, "hash key must be STRING, got %s", key.Type())
		}
		val := i.Eval(pair.Value, env)
		if isError(val) {
			return val
		}
		hash.Set(ks.Value, val)
	}
	return hash
}

func (i *Interp) evalExpressions(exps []ast.Expression, env *object.Environment) []object.Object {
	var result []object.Object
	for _, e := range exps {
		evaluated := i.Eval(e, env)
		if isError(evaluated) {
			return []object.Object{evaluated}
		}
		result = append(result, evaluated)
	}
	return result
}

// --- helpers ---

func nativeBool(b bool) *object.Boolean {
	if b {
		return TRUE
	}
	return FALSE
}

func isTruthy(obj object.Object) bool {
	switch obj {
	case NULL, FALSE:
		return false
	case TRUE:
		return true
	}
	if b, ok := obj.(*object.Boolean); ok {
		return b.Value
	}
	if _, ok := obj.(*object.Null); ok {
		return false
	}
	return true
}

func isError(obj object.Object) bool {
	return obj != nil && obj.Type() == object.ERROR_OBJ
}

func isNumeric(obj object.Object) bool {
	t := obj.Type()
	return t == object.INTEGER_OBJ || t == object.FLOAT_OBJ
}

func toFloat(obj object.Object) float64 {
	switch o := obj.(type) {
	case *object.Float:
		return o.Value
	case *object.Integer:
		return float64(o.Value)
	}
	return 0
}

func objectsEqual(a, b object.Object) bool {
	if isNumeric(a) && isNumeric(b) {
		return toFloat(a) == toFloat(b)
	}
	if a.Type() != b.Type() {
		return false
	}
	switch av := a.(type) {
	case *object.Boolean:
		return av.Value == b.(*object.Boolean).Value
	case *object.String:
		return av.Value == b.(*object.String).Value
	case *object.Null:
		return true
	}
	return a == b // identity for reference types
}

// suggestMember returns the namespace member closest to want by Levenshtein
// distance, mirroring registry.Suggest but over a namespace's tools.
func suggestMember(ns *object.Namespace, want string) string {
	names := make([]string, 0, len(ns.Members))
	for name := range ns.Members {
		names = append(names, name)
	}
	return registry.Suggest(want, names)
}

func newError(node ast.Node, code, format string, args ...any) *object.Error {
	var sp token.Span
	if node != nil {
		sp = node.Span()
	}
	return &object.Error{Code: code, Message: fmt.Sprintf(format, args...), Span: sp}
}
