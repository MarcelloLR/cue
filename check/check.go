// Package check is Cue's static checker: it walks the AST without executing and
// reports the name-resolution and arity problems an agent can fix before running
// (DESIGN.md §9). It is the "pre-flight" half of the self-repair loop — emit →
// `cue check --json` → read codes+hints → edit → re-check → `cue run`.
//
// The checker is deliberately conservative: it flags only what it is confident
// about (unbound identifiers, unknown members of a known namespace, arity of
// calls to known tools/builtins) and stays silent on anything dynamic (e.g.
// member access on a value whose type it cannot know). False negatives are
// preferred over false positives so the agent is never sent chasing a phantom.
package check

import (
	"fmt"

	"github.com/MarcelloLR/cue/ast"
	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/registry"
)

// checker carries the immutable inputs (registry, builtin arities, namespace
// names) and the diagnostic collector across the walk. Scopes are passed down the
// recursion explicitly rather than mutated globally.
type checker struct {
	reg      *registry.Registry
	builtins map[string]int // name → arity (-1 = variadic)
	nsNames  map[string]bool
	diags    *diag.Collector
}

// scope is a chained set of known names (let bindings, params). Lookups walk
// outward; the builtins/namespace names are checked separately as a final
// fallback, mirroring evalIdentifier's resolution order.
type scope struct {
	names map[string]bool
	outer *scope
}

func newScope(outer *scope) *scope { return &scope{names: map[string]bool{}, outer: outer} }

func (s *scope) define(name string) { s.names[name] = true }

func (s *scope) has(name string) bool {
	for cur := s; cur != nil; cur = cur.outer {
		if cur.names[name] {
			return true
		}
	}
	return false
}

// Program statically checks a parsed program against the tool registry and
// builtin set, returning the collected diagnostics. builtinArities is the
// evaluator's BuiltinArities() (name → fixed arity, -1 for variadic).
func Program(prog *ast.Program, reg *registry.Registry, builtinArities map[string]int) []diag.Diagnostic {
	c := &checker{
		reg:      reg,
		builtins: builtinArities,
		nsNames:  map[string]bool{},
		diags:    &diag.Collector{},
	}
	for name := range reg.Namespaces() {
		c.nsNames[name] = true
	}

	top := newScope(nil)
	for _, stmt := range prog.Statements {
		c.checkStatement(stmt, top)
	}
	return c.diags.Items()
}

func (c *checker) checkStatement(stmt ast.Statement, sc *scope) {
	switch s := stmt.(type) {
	case *ast.LetStatement:
		// Evaluate the value first, then bind — so `let f = fn() {...}` cannot
		// see f, matching runtime scoping (recursion goes through a prior let).
		if s.Value != nil {
			c.checkExpr(s.Value, sc)
		}
		sc.define(s.Name.Value)
	case *ast.AssignStatement:
		// Reassignment targets must already exist, but `=` to an unknown name is a
		// runtime error we leave to execution; we only check the value + target
		// subexpressions here to stay conservative.
		c.checkExpr(s.Value, sc)
		c.checkAssignTarget(s.Target, sc)
	case *ast.ReturnStatement:
		if s.Value != nil {
			c.checkExpr(s.Value, sc)
		}
	case *ast.ExpressionStatement:
		if s.Expr != nil {
			c.checkExpr(s.Expr, sc)
		}
	case *ast.BlockStatement:
		inner := newScope(sc)
		for _, st := range s.Statements {
			c.checkStatement(st, inner)
		}
	}
}

// checkAssignTarget checks the subexpressions of an lvalue (the container being
// indexed / the object whose member is set) without flagging the bare target
// identifier — an unbound assignment target is a runtime concern.
func (c *checker) checkAssignTarget(target ast.Expression, sc *scope) {
	switch t := target.(type) {
	case *ast.IndexExpression:
		c.checkExpr(t.Left, sc)
		c.checkExpr(t.Index, sc)
	case *ast.MemberExpression:
		c.checkExpr(t.Object, sc)
	}
}

func (c *checker) checkExpr(expr ast.Expression, sc *scope) {
	switch e := expr.(type) {
	case *ast.Identifier:
		c.checkIdentifier(e, sc)
	case *ast.PrefixExpression:
		c.checkExpr(e.Right, sc)
	case *ast.InfixExpression:
		c.checkExpr(e.Left, sc)
		c.checkExpr(e.Right, sc)
	case *ast.IfExpression:
		c.checkExpr(e.Condition, sc)
		c.checkStatement(e.Consequence, sc)
		if e.Alternative != nil {
			c.checkStatement(e.Alternative, sc)
		}
	case *ast.ForExpression:
		c.checkExpr(e.Iterable, sc)
		inner := newScope(sc)
		inner.define(e.Var.Value)
		c.checkStatement(e.Body, inner)
	case *ast.FunctionLiteral:
		inner := newScope(sc)
		for _, p := range e.Parameters {
			inner.define(p.Value)
		}
		c.checkStatement(e.Body, inner)
	case *ast.CallExpression:
		c.checkCall(e, sc)
	case *ast.IndexExpression:
		c.checkExpr(e.Left, sc)
		c.checkExpr(e.Index, sc)
	case *ast.MemberExpression:
		c.checkMember(e, sc)
	case *ast.ArrayLiteral:
		for _, el := range e.Elements {
			c.checkExpr(el, sc)
		}
	case *ast.HashLiteral:
		for _, pair := range e.Pairs {
			c.checkExpr(pair.Key, sc)
			c.checkExpr(pair.Value, sc)
		}
	}
}

// checkIdentifier flags a bare name that is neither bound in scope, a builtin,
// nor an injected namespace, suggesting the nearest tool/builtin name.
func (c *checker) checkIdentifier(id *ast.Identifier, sc *scope) {
	if c.known(id.Value, sc) {
		return
	}
	c.diags.Add(diag.Diagnostic{
		Code:     diag.NameUnknownIdent,
		Severity: diag.SeverityError,
		Message:  "unknown identifier " + quote(id.Value),
		Span:     id.Span(),
		Hint:     c.identHint(id.Value),
	})
}

// known reports whether name resolves to a scope binding (let/param), a builtin,
// or an injected namespace — the same three sources evalIdentifier consults.
func (c *checker) known(name string, sc *scope) bool {
	if sc.has(name) {
		return true
	}
	if _, ok := c.builtins[name]; ok {
		return true
	}
	return c.nsNames[name]
}

// checkMember validates `ns.member` when ns is a known namespace identifier: the
// member must be a registered tool, else CUE_NAME_003 with a did-you-mean over
// that namespace's tools. Member access on anything else is left to runtime.
func (c *checker) checkMember(m *ast.MemberExpression, sc *scope) {
	id, ok := m.Object.(*ast.Identifier)
	if !ok {
		c.checkExpr(m.Object, sc)
		return
	}
	// A let/param binding shadows a namespace name; in that case we cannot know
	// the value's type statically, so we stay silent.
	if sc.has(id.Value) {
		return
	}
	ns, ok := c.reg.Namespaces()[id.Value]
	if !ok {
		// Object identifier is unknown and not a namespace — let checkIdentifier
		// report the unbound name; nothing more to say about the member.
		c.checkIdentifier(id, sc)
		return
	}
	if _, ok := ns.Members[m.Property]; ok {
		return
	}
	d := diag.Diagnostic{
		Code:     diag.NameUnknownMember,
		Severity: diag.SeverityError,
		Message:  "unknown tool " + quote(ns.Name+"."+m.Property) + " in namespace " + quote(ns.Name),
		Span:     m.Span(),
		Data:     map[string]any{"namespace": ns.Name},
	}
	if s := suggestMember(ns, m.Property); s != "" {
		full := ns.Name + "." + s
		d.Hint = "did you mean " + quote(full) + "? run `cue catalog` for tools"
		d.Data["did_you_mean"] = full
	}
	c.diags.Add(d)
}

// checkCall checks the callee and arguments, then arity for calls to a known
// tool (`ns.tool(...)`) or a known builtin with fixed arity.
func (c *checker) checkCall(call *ast.CallExpression, sc *scope) {
	for _, a := range call.Arguments {
		c.checkExpr(a, sc)
	}

	switch fn := call.Function.(type) {
	case *ast.MemberExpression:
		c.checkMember(fn, sc)
		c.checkToolArity(call, fn, sc)
	case *ast.Identifier:
		if c.checkBuiltinArity(call, fn, sc) {
			return
		}
		c.checkExpr(fn, sc)
	default:
		c.checkExpr(call.Function, sc)
	}
}

// checkToolArity validates argument count for a resolved tool call. Variadic
// tools and unknown members are skipped (the latter already reported).
func (c *checker) checkToolArity(call *ast.CallExpression, fn *ast.MemberExpression, sc *scope) {
	id, ok := fn.Object.(*ast.Identifier)
	if !ok || sc.has(id.Value) {
		return
	}
	ns, ok := c.reg.Namespaces()[id.Value]
	if !ok {
		return
	}
	tool, ok := ns.Members[fn.Property]
	if !ok {
		return
	}
	sig := tool.Impl.Signature()
	if sig.Variadic {
		return
	}
	if len(call.Arguments) != len(sig.Params) {
		c.diags.Add(diag.Diagnostic{
			Code:     diag.TypeArgCount,
			Severity: diag.SeverityError,
			Message:  argCountMsg(tool.Impl.Name(), len(sig.Params), len(call.Arguments)),
			Span:     call.Span(),
		})
	}
}

// checkBuiltinArity validates argument count for a call to a known builtin. It
// returns true when the call was recognized as a builtin (so the caller need not
// re-check the identifier), false otherwise.
func (c *checker) checkBuiltinArity(call *ast.CallExpression, fn *ast.Identifier, sc *scope) bool {
	// A local binding shadows a builtin name; defer to that binding.
	if sc.has(fn.Value) {
		return false
	}
	arity, ok := c.builtins[fn.Value]
	if !ok {
		return false
	}
	if arity >= 0 && len(call.Arguments) != arity {
		c.diags.Add(diag.Diagnostic{
			Code:     diag.TypeArgCount,
			Severity: diag.SeverityError,
			Message:  argCountMsg(fn.Value, arity, len(call.Arguments)),
			Span:     call.Span(),
		})
	}
	return true
}

// identHint suggests the nearest tool or builtin name for an unknown identifier.
func (c *checker) identHint(name string) string {
	candidates := c.reg.Names()
	for b := range c.builtins {
		candidates = append(candidates, b)
	}
	for ns := range c.nsNames {
		candidates = append(candidates, ns)
	}
	if s := registry.Suggest(name, candidates); s != "" {
		return "did you mean " + quote(s) + "?"
	}
	return ""
}

func suggestMember(ns *object.Namespace, want string) string {
	names := make([]string, 0, len(ns.Members))
	for name := range ns.Members {
		names = append(names, name)
	}
	return registry.Suggest(want, names)
}

func quote(s string) string { return "\"" + s + "\"" }

func argCountMsg(name string, want, got int) string {
	return fmt.Sprintf("%s: wrong number of arguments: want %d, got %d", name, want, got)
}
