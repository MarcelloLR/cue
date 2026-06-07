// Package evaluator tree-walks the AST, producing runtime values (object.Object).
//
// Control flow uses two propagating wrapper values: ReturnValue (unwound at
// function boundaries) and Error (short-circuits everything). Errors carry a
// stable code and the source span of the offending node, feeding the
// structured-diagnostics contract.
package evaluator

import (
	"fmt"

	"github.com/MarcelloLR/cue/ast"
	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/token"
)

// Shared singletons for the immutable values.
var (
	NULL  = &object.Null{}
	TRUE  = &object.Boolean{Value: true}
	FALSE = &object.Boolean{Value: false}
)

// Eval evaluates a node in env.
func Eval(node ast.Node, env *object.Environment) object.Object {
	switch node := node.(type) {

	// Statements.
	case *ast.Program:
		return evalProgram(node, env)
	case *ast.ExpressionStatement:
		return Eval(node.Expr, env)
	case *ast.BlockStatement:
		return evalBlockStatement(node, env)
	case *ast.LetStatement:
		val := Eval(node.Value, env)
		if isError(val) {
			return val
		}
		env.Set(node.Name.Value, val)
		return NULL
	case *ast.AssignStatement:
		return evalAssignStatement(node, env)
	case *ast.ReturnStatement:
		if node.Value == nil {
			return &object.ReturnValue{Value: NULL}
		}
		val := Eval(node.Value, env)
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
		elems := evalExpressions(node.Elements, env)
		if len(elems) == 1 && isError(elems[0]) {
			return elems[0]
		}
		return &object.Array{Elements: elems}
	case *ast.HashLiteral:
		return evalHashLiteral(node, env)
	case *ast.FunctionLiteral:
		return &object.Function{Parameters: node.Parameters, Body: node.Body, Env: env}

	// Expressions.
	case *ast.Identifier:
		return evalIdentifier(node, env)
	case *ast.PrefixExpression:
		right := Eval(node.Right, env)
		if isError(right) {
			return right
		}
		return evalPrefixExpression(node, right)
	case *ast.InfixExpression:
		return evalInfixExpression(node, env)
	case *ast.IfExpression:
		return evalIfExpression(node, env)
	case *ast.ForExpression:
		return evalForExpression(node, env)
	case *ast.CallExpression:
		return evalCallExpression(node, env)
	case *ast.IndexExpression:
		left := Eval(node.Left, env)
		if isError(left) {
			return left
		}
		index := Eval(node.Index, env)
		if isError(index) {
			return index
		}
		return evalIndexExpression(node, left, index)
	case *ast.MemberExpression:
		return evalMemberExpression(node, env)
	}

	return newError(node, diag.RuntimeBuiltin, "evaluation not implemented for %T", node)
}

func evalProgram(program *ast.Program, env *object.Environment) object.Object {
	var result object.Object = NULL
	for _, stmt := range program.Statements {
		result = Eval(stmt, env)
		switch result := result.(type) {
		case *object.ReturnValue:
			return result.Value
		case *object.Error:
			return result
		}
	}
	return result
}

func evalBlockStatement(block *ast.BlockStatement, env *object.Environment) object.Object {
	var result object.Object = NULL
	for _, stmt := range block.Statements {
		result = Eval(stmt, env)
		if result != nil {
			rt := result.Type()
			if rt == object.RETURN_VALUE_OBJ || rt == object.ERROR_OBJ {
				return result
			}
		}
	}
	return result
}

func evalAssignStatement(node *ast.AssignStatement, env *object.Environment) object.Object {
	val := Eval(node.Value, env)
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
		left := Eval(target.Left, env)
		if isError(left) {
			return left
		}
		index := Eval(target.Index, env)
		if isError(index) {
			return index
		}
		return evalIndexAssign(node, left, index, val)

	case *ast.MemberExpression:
		obj := Eval(target.Object, env)
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

func evalInfixExpression(node *ast.InfixExpression, env *object.Environment) object.Object {
	// Logical operators short-circuit, so evaluate the left side first.
	if node.Operator == "&&" || node.Operator == "||" {
		left := Eval(node.Left, env)
		if isError(left) {
			return left
		}
		if node.Operator == "&&" && !isTruthy(left) {
			return FALSE
		}
		if node.Operator == "||" && isTruthy(left) {
			return TRUE
		}
		right := Eval(node.Right, env)
		if isError(right) {
			return right
		}
		return nativeBool(isTruthy(right))
	}

	left := Eval(node.Left, env)
	if isError(left) {
		return left
	}
	right := Eval(node.Right, env)
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

func evalIfExpression(node *ast.IfExpression, env *object.Environment) object.Object {
	cond := Eval(node.Condition, env)
	if isError(cond) {
		return cond
	}
	if isTruthy(cond) {
		return Eval(node.Consequence, env)
	}
	if node.Alternative != nil {
		return Eval(node.Alternative, env)
	}
	return NULL
}

func evalForExpression(node *ast.ForExpression, env *object.Environment) object.Object {
	iterable := Eval(node.Iterable, env)
	if isError(iterable) {
		return iterable
	}

	loopEnv := object.NewEnclosedEnvironment(env)
	run := func(item object.Object) object.Object {
		loopEnv.Set(node.Var.Value, item)
		result := Eval(node.Body, loopEnv)
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

func evalCallExpression(node *ast.CallExpression, env *object.Environment) object.Object {
	fn := Eval(node.Function, env)
	if isError(fn) {
		return fn
	}
	args := evalExpressions(node.Arguments, env)
	if len(args) == 1 && isError(args[0]) {
		return args[0]
	}
	return applyFunction(node, fn, args)
}

func applyFunction(node *ast.CallExpression, fn object.Object, args []object.Object) object.Object {
	switch fn := fn.(type) {
	case *object.Function:
		if len(args) != len(fn.Parameters) {
			return newError(node, diag.TypeArgCount,
				"wrong number of arguments: want %d, got %d", len(fn.Parameters), len(args))
		}
		extended := object.NewEnclosedEnvironment(fn.Env)
		for i, p := range fn.Parameters {
			extended.Set(p.Value, args[i])
		}
		result := Eval(fn.Body, extended)
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
	}
	return newError(node, diag.TypeNotCallable, "not callable: %s", fn.Type())
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

func evalMemberExpression(node *ast.MemberExpression, env *object.Environment) object.Object {
	obj := Eval(node.Object, env)
	if isError(obj) {
		return obj
	}
	hash, ok := obj.(*object.Hash)
	if !ok {
		return newError(node, diag.TypeMismatch, "cannot access member .%s on %s", node.Property, obj.Type())
	}
	if v, ok := hash.Pairs[node.Property]; ok {
		return v
	}
	return NULL
}

func evalHashLiteral(node *ast.HashLiteral, env *object.Environment) object.Object {
	hash := object.NewHash()
	for _, pair := range node.Pairs {
		key := Eval(pair.Key, env)
		if isError(key) {
			return key
		}
		ks, ok := key.(*object.String)
		if !ok {
			return newError(node, diag.TypeBadKey, "hash key must be STRING, got %s", key.Type())
		}
		val := Eval(pair.Value, env)
		if isError(val) {
			return val
		}
		hash.Set(ks.Value, val)
	}
	return hash
}

func evalExpressions(exps []ast.Expression, env *object.Environment) []object.Object {
	var result []object.Object
	for _, e := range exps {
		evaluated := Eval(e, env)
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

func newError(node ast.Node, code, format string, args ...any) *object.Error {
	var sp token.Span
	if node != nil {
		sp = node.Span()
	}
	return &object.Error{Code: code, Message: fmt.Sprintf(format, args...), Span: sp}
}
