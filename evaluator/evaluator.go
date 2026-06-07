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
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/MarcelloLR/cue/ast"
	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/effectlog"
	"github.com/MarcelloLR/cue/runtime/llm"
	"github.com/MarcelloLR/cue/runtime/policy"
	"github.com/MarcelloLR/cue/runtime/registry"
	"github.com/MarcelloLR/cue/token"
)

// DefaultParallelLimit bounds how many `parallel` branches run at once when the
// program does not give an inline `limit =` (DESIGN.md §6). Tool/LLM calls are
// IO-bound, so the cap exists to avoid unbounded fan-out, not CPU contention.
const DefaultParallelLimit = 8

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
	// Ctx is honoured by tool Invoke calls for cancellation/timeouts. Inside a
	// `parallel` branch this is the errgroup-derived child context, so a sibling's
	// error (or a top-level cancel) cancels in-flight Invoke calls (DESIGN.md §6).
	Ctx context.Context
	// Policy gates every tool call before it runs (DESIGN.md §7).
	Policy policy.Policy
	// Prompter resolves a policy Prompt decision into allow/deny at call time
	// (DESIGN.md §7). It defaults to policy.DenyPrompter (a Prompt with no prompter
	// is conservatively denied); Phase 4's ask_human becomes the interactive one.
	Prompter policy.Prompter
	// Effects records every gated tool call for the run envelope (§7, §9). Parallel
	// branches share one Recorder (it is goroutine-safe), so the whole run stays in
	// one ledger; each record is tagged with the branch it ran on.
	Effects *effectlog.Recorder
	// Branch is the concurrency path of this Interp: "root" at the top level, and
	// "parallel:k" (nested as "parent/parallel:k") inside a parallel branch. It is
	// stamped onto every effect record so concurrent runs stay reconstructable and,
	// later, replayable (DESIGN.md §6, §10).
	Branch string
	// Out is where `print` writes. It defaults to os.Stdout, but in --json mode the
	// CLI points it at stderr so stdout carries only the machine-readable envelope:
	// the run output must stay parseable by the agent (DESIGN.md §9).
	Out io.Writer
	// In is where ask_human reads a line of human input from. It defaults to
	// os.Stdin; a harness (or a test) supplies any reader. Prompts are written to
	// Out, never to In (DESIGN.md §8).
	In io.Reader
	// LLM is the provider the `llm()` primitive calls. It defaults to an offline,
	// deterministic llm.MockProvider so tests and examples need no network or API
	// key; a real provider is swapped in at construction (DESIGN.md §8, §14).
	LLM llm.Provider
	// promptLock serializes the human-prompt critical section (ask_human and the
	// interactive Prompter) across all sibling Interps. It is shared by reference:
	// New() allocates one and every `parallel` child Interp copies the same pointer,
	// so concurrent branches that prompt block on one another and stdin never
	// interleaves (DESIGN.md §6). A nil lock means "no serialization needed" (a bare
	// Interp literal), which the prompt helpers tolerate.
	promptLock *sync.Mutex
}

// Option configures an Interp at construction.
type Option func(*Interp)

// WithContext sets the context passed to tool invocations.
func WithContext(ctx context.Context) Option { return func(i *Interp) { i.Ctx = ctx } }

// WithPolicy sets the capability policy used to gate tool calls.
func WithPolicy(p policy.Policy) Option { return func(i *Interp) { i.Policy = p } }

// WithPrompter sets the prompter that resolves a policy Prompt decision. Defaults
// to policy.DenyPrompter (Prompt without a prompter is denied).
func WithPrompter(p policy.Prompter) Option { return func(i *Interp) { i.Prompter = p } }

// WithEffects sets the effect recorder. Defaults to a fresh recorder.
func WithEffects(r *effectlog.Recorder) Option { return func(i *Interp) { i.Effects = r } }

// WithBranch sets the concurrency-path label stamped onto effect records.
func WithBranch(branch string) Option { return func(i *Interp) { i.Branch = branch } }

// WithOutput sets the writer `print` writes to. Defaults to os.Stdout.
func WithOutput(w io.Writer) Option { return func(i *Interp) { i.Out = w } }

// WithInput sets the reader ask_human reads a line from. Defaults to os.Stdin.
func WithInput(r io.Reader) Option { return func(i *Interp) { i.In = r } }

// WithLLM sets the provider the `llm()` primitive calls. Defaults to an offline,
// deterministic llm.MockProvider.
func WithLLM(p llm.Provider) Option { return func(i *Interp) { i.LLM = p } }

// New returns an Interp with sensible defaults: a background context, the
// allow-all policy, a fresh effect recorder, and the "root" branch. Options
// override these.
func New(opts ...Option) *Interp {
	i := &Interp{
		Ctx:        context.Background(),
		Policy:     policy.AllowAll{},
		Prompter:   policy.DenyPrompter{},
		Effects:    effectlog.NewRecorder(),
		Branch:     "root",
		Out:        os.Stdout,
		In:         os.Stdin,
		LLM:        llm.MockProvider{},
		promptLock: &sync.Mutex{},
	}
	for _, opt := range opts {
		opt(i)
	}
	// Serialize writes to Out: `parallel` branches share one Interp output, and a
	// caller-supplied writer (e.g. a bytes.Buffer in a harness) is not necessarily
	// goroutine-safe. Sibling Interps copy this same *syncWriter by reference, so
	// concurrent prints never interleave mid-line or race (DESIGN.md §6).
	if _, already := i.Out.(*syncWriter); !already {
		i.Out = &syncWriter{w: i.Out}
	}
	return i
}

// syncWriter serializes concurrent writes to an underlying writer with a mutex.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// promptLine is the single human-prompt critical section shared by ask_human and
// the interactive Prompter (DESIGN.md §6, §8). It holds the global prompt lock so
// concurrent `parallel` branches serialize, writes the question to Out, and reads
// one line from In. Sibling Interps share the same lock/In/Out by reference, so the
// whole run prompts on one channel without interleaving.
func (i *Interp) promptLine(question string) (string, error) {
	if i.promptLock != nil {
		i.promptLock.Lock()
		defer i.promptLock.Unlock()
	}
	if question != "" {
		fmt.Fprint(i.Out, question)
	}
	return readLine(i.In)
}

// readLine reads a single newline-terminated line from r, one byte at a time so a
// shared reader (os.Stdin across branches) is never over-read past the line. A
// final line without a trailing newline is returned on EOF; a CRLF is normalized.
func readLine(r io.Reader) (string, error) {
	if r == nil {
		return "", io.EOF
	}
	var b []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return strings.TrimSuffix(string(b), "\r"), nil
			}
			b = append(b, buf[0])
		}
		if err != nil {
			if err == io.EOF && len(b) > 0 {
				return strings.TrimSuffix(string(b), "\r"), nil
			}
			return "", err
		}
	}
}

// interactivePrompter resolves a policy Prompt by asking a yes/no question on the
// Interp's prompt channel — under the same lock ask_human uses — so a confirmation
// and a concurrent ask_human never interleave (DESIGN.md §6, §7). This is what
// makes ask_human "the interactive Prompter" the policy layer routes Prompt to.
type interactivePrompter struct{ interp *Interp }

// NewInteractivePrompter returns a policy.Prompter that confirms via i's prompt
// channel (stdin/Out). The CLI installs it for `cue run`; tests inject a
// policy.FuncPrompter instead so they never block on real input.
func NewInteractivePrompter(i *Interp) policy.Prompter { return &interactivePrompter{interp: i} }

func (p *interactivePrompter) Confirm(name string, args []object.Object) (bool, error) {
	line, err := p.interp.promptLine(fmt.Sprintf("Policy requires confirmation to run %q. Allow? [y/N] ", name))
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
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
		return i.evalIdentifier(node, env)
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
	case *ast.ParallelExpression:
		return i.evalParallelExpression(node, env)
	case *ast.RetryExpression:
		return i.evalRetryExpression(node, env)
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

func (i *Interp) evalIdentifier(node *ast.Identifier, env *object.Environment) object.Object {
	if val, ok := env.Get(node.Value); ok {
		return val
	}
	// A handful of builtins depend on run state (where output goes, where input
	// comes from, the configured llm provider, the policy/effect path), so they are
	// built per-Interp rather than living in the shared, stateless map.
	switch node.Value {
	case "print":
		return i.printBuiltin()
	case "ask_human":
		return i.askHumanBuiltin(node)
	case "llm":
		return i.llmBuiltin(node)
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

// evalParallelExpression runs the parallel map form (DESIGN.md §6, normative).
// It materializes the iterable into an ordered item list, then evaluates the body
// for each item concurrently on a golang.org/x/sync/errgroup bounded by limit and
// sharing a derived context. Results land in a pre-sized slice at each item's
// input index, so the returned Array is always in input order regardless of
// completion order. The first body to yield an *object.Error returns it as a Go
// error, which cancels the shared context (errgroup semantics); that first error
// becomes the expression's value. Every branch gets its own enclosed environment
// and its own child Interp (sharing only the goroutine-safe Effects recorder),
// so no writable state is shared across goroutines.
func (i *Interp) evalParallelExpression(node *ast.ParallelExpression, env *object.Environment) object.Object {
	iterable := i.Eval(node.Iterable, env)
	if isError(iterable) {
		return iterable
	}

	items, errObj := i.parallelItems(node, iterable)
	if errObj != nil {
		return errObj
	}

	limit, errObj := i.parallelLimit(node, env)
	if errObj != nil {
		return errObj
	}

	n := len(items)
	results := make([]object.Object, n)

	g, gctx := errgroup.WithContext(i.Ctx)
	g.SetLimit(limit)

	for k := 0; k < n; k++ {
		k, item := k, items[k]
		g.Go(func() error {
			// Each branch gets its own enclosed env and child Interp. The env is
			// never shared between goroutines (only ancestor scopes are read, and
			// those are not written during the parallel region). The child Interp
			// shares the goroutine-safe Effects recorder and the Policy, runs under
			// the errgroup's context so a sibling error cancels it, and carries the
			// branch label for effect tagging (DESIGN.md §6).
			branchEnv := object.NewEnclosedEnvironment(env)
			branchEnv.Set(node.Var.Value, item)

			child := &Interp{
				Ctx:      gctx,
				Policy:   i.Policy,
				Prompter: i.Prompter,
				Effects:  i.Effects,
				Branch:   i.branchLabel(k),
				Out:      i.Out,
				// In, LLM, and promptLock are shared by reference across siblings:
				// the same *sync.Mutex serializes every branch's human-prompt
				// critical section so concurrent ask_human calls (and interactive
				// policy prompts) never interleave on stdin (DESIGN.md §6).
				In:         i.In,
				LLM:        i.LLM,
				promptLock: i.promptLock,
			}

			result := child.Eval(node.Body, branchEnv)
			if e, ok := result.(*object.Error); ok {
				// Surface the runtime error as a Go error so errgroup cancels the
				// rest via gctx; the *object.Error is recovered after Wait.
				return &branchError{err: e}
			}
			// A `return` inside the body unwinds to its value, mirroring how a
			// function body collapses a ReturnValue (DESIGN.md §5).
			if rv, ok := result.(*object.ReturnValue); ok {
				result = rv.Value
			}
			results[k] = result
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		if be, ok := err.(*branchError); ok {
			return be.err
		}
		// errgroup only ever sees branchError values from our g.Go closures, so
		// this is unreachable; report defensively rather than panic.
		return newError(node, diag.RuntimeBuiltin, "parallel: %s", err.Error())
	}
	return &object.Array{Elements: results}
}

// branchError carries an *object.Error out of a parallel branch as a Go error so
// errgroup can use it to cancel siblings; evalParallelExpression unwraps the
// first one back into the expression's value.
type branchError struct{ err *object.Error }

func (b *branchError) Error() string { return b.err.Message }

// branchLabel builds the branch path for the k-th parallel branch. At the top
// level it is "parallel:k"; nested inside another branch it is
// "<parent>/parallel:k" so the full concurrency path stays reconstructable
// (DESIGN.md §6, §10).
func (i *Interp) branchLabel(k int) string {
	label := fmt.Sprintf("parallel:%d", k)
	if i.Branch != "" && i.Branch != "root" {
		return i.Branch + "/" + label
	}
	return label
}

// parallelItems materializes an iterable into the ordered list of items the
// parallel map binds its loop variable over, matching `for`'s iteration sources
// (arrays, hash keys, string runes) for consistency (DESIGN.md §5, §6).
func (i *Interp) parallelItems(node *ast.ParallelExpression, iterable object.Object) ([]object.Object, *object.Error) {
	switch it := iterable.(type) {
	case *object.Array:
		items := make([]object.Object, len(it.Elements))
		copy(items, it.Elements)
		return items, nil
	case *object.String:
		runes := []rune(it.Value)
		items := make([]object.Object, len(runes))
		for idx, ch := range runes {
			items[idx] = &object.String{Value: string(ch)}
		}
		return items, nil
	case *object.Hash:
		items := make([]object.Object, len(it.Keys))
		for idx, k := range it.Keys {
			items[idx] = &object.String{Value: k}
		}
		return items, nil
	default:
		return nil, newError(node, diag.TypeNotIterable, "%s is not iterable", iterable.Type())
	}
}

// parallelLimit resolves the bound on concurrency: the inline `limit =` value
// when present (which must be a positive integer), else DefaultParallelLimit
// (DESIGN.md §6).
func (i *Interp) parallelLimit(node *ast.ParallelExpression, env *object.Environment) (int, *object.Error) {
	if node.Limit == nil {
		return DefaultParallelLimit, nil
	}
	val := i.Eval(node.Limit, env)
	if e, ok := val.(*object.Error); ok {
		return 0, e
	}
	n, ok := val.(*object.Integer)
	if !ok {
		return 0, newError(node.Limit, diag.TypeMismatch,
			"parallel limit must be INTEGER, got %s", val.Type())
	}
	if n.Value <= 0 {
		return 0, newError(node.Limit, diag.TypeMismatch,
			"parallel limit must be a positive integer, got %d", n.Value)
	}
	return int(n.Value), nil
}

// evalRetryExpression runs the retry form `retry (n) { body }` (DESIGN.md §3, §8).
// It evaluates the attempt count (which must be a positive *object.Integer, else
// CUE_TYPE_*), then re-evaluates body up to n times while it yields an
// *object.Error, returning the first non-error result or — if every attempt errors
// — the LAST error. A `return` inside the body unwinds to its value, like any other
// expression body. Backoff is intentionally none by default so tests are
// deterministic and fast and never depend on wall-clock time; a real deployment
// that wants spacing between attempts would add it behind an explicit, off-by-
// default knob.
//
// IDEMPOTENCY HAZARD (DESIGN.md §8): each attempt re-evaluates the whole body, so
// every effectful call inside it is re-logged and re-run on every retry. Retrying a
// body that performs a non-idempotent or irreversible tool call (e.g. fs.delete, a
// POST that charges money) can repeat that effect — retry pairs with Phase 3
// reversibility/compensation precisely because of this. Prefer retrying bodies
// whose tool calls are idempotent or reversible.
func (i *Interp) evalRetryExpression(node *ast.RetryExpression, env *object.Environment) object.Object {
	attempts := i.Eval(node.Attempts, env)
	if isError(attempts) {
		return attempts
	}
	n, ok := attempts.(*object.Integer)
	if !ok {
		return newError(node.Attempts, diag.TypeMismatch,
			"retry attempts must be INTEGER, got %s", attempts.Type())
	}
	if n.Value <= 0 {
		return newError(node.Attempts, diag.TypeMismatch,
			"retry attempts must be a positive integer, got %d", n.Value)
	}

	var last object.Object = NULL
	for attempt := int64(0); attempt < n.Value; attempt++ {
		// A fresh enclosed env per attempt so a `let` in the body does not leak a
		// rebinding across retries.
		result := i.Eval(node.Body, object.NewEnclosedEnvironment(env))
		if rv, ok := result.(*object.ReturnValue); ok {
			result = rv.Value
		}
		if !isError(result) {
			return result
		}
		last = result
	}
	// Every attempt errored: surface the last error (DESIGN.md §8).
	return last
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
// result/error/compensation → return the value. Effects are recorded into
// i.Effects keyed by a deterministic call-site so the run stays reconstructable.
//
// The safety guarantee is structural: a Deny (and a Prompt the prompter declines)
// returns a CUE_CAP_* error *before* Invoke is ever called — the tool does not
// run — yet the blocked attempt is still recorded with status "denied" so the
// boundary is auditable as well as learnable.
func (i *Interp) invokeTool(node *ast.CallExpression, tool *object.Tool, args []object.Object) object.Object {
	impl := tool.Impl
	sig := impl.Signature()

	// Arity: variadic tools accept any count; otherwise the call must match.
	if !sig.Variadic && len(args) != len(sig.Params) {
		return newError(node, diag.TypeArgCount,
			"%s: wrong number of arguments: want %d, got %d", impl.Name(), len(sig.Params), len(args))
	}

	rev := impl.Reversibility()

	// The base record shared by every outcome (allow/deny/error/ok). Filled in
	// per outcome below. The callsite is the deterministic key the effect log
	// (and, later, replay) uses instead of the scheduling-dependent Seq.
	rec := i.newRecord(node, impl.Name(), args, rev == object.Reversible)

	// Policy gate (the shared pipeline llm() also uses): Deny is a learnable
	// boundary (CUE_CAP_001); Prompt routes through the Prompter seam (CUE_CAP_002
	// when it declines or is absent). A blocked call is recorded with status
	// "denied" and NOT invoked.
	if denied := i.gate(node, rec, impl.Name(), args, rev); denied != nil {
		return denied
	}

	start := time.Now()
	result, err := impl.Invoke(i.Ctx, args)
	rec.DurationMs = time.Since(start).Milliseconds()

	if err != nil {
		return i.recordError(node, rec, err.Error())
	}

	if result == nil {
		result = NULL
	}
	// Capture the inverse action for a successful reversible call so a later phase
	// can roll it back (DESIGN.md §7). Capture only; firing is Phase 5.
	if comp, ok := impl.(object.Compensator); ok {
		if cTool, cArgs, ok := comp.Compensation(args, result); ok {
			rec.Compensation = &effectlog.Compensation{Tool: cTool, Args: renderArgs(cArgs)}
		}
	}
	i.recordOK(rec, result)
	return result
}

// --- shared effect-logging pipeline (DESIGN.md §7) ---
//
// Tools, llm(), and ask_human() all touch the outside world and so all flow
// through one place rather than re-implementing policy/record logic: invokeTool
// builds the full pipeline (gate → Invoke → record); llm() reuses gate +
// recordError/recordOK; ask_human() reuses newRecord + recordOK (it is logged but
// not gated, being itself the escalation target of a Prompt). Keeping this single
// keeps the §10 replay key — (Branch, Callsite, Occurrence) — consistent across
// every effect kind.

// newRecord builds the base effect record common to every outcome: the
// deterministic callsite key, the branch label, the tool name, the rendered args,
// and the reversibility flag (DESIGN.md §7).
func (i *Interp) newRecord(node ast.Node, name string, args []object.Object, reversible bool) effectlog.Record {
	return effectlog.Record{
		Callsite:   callsite(node),
		Branch:     i.Branch,
		Tool:       name,
		Args:       renderArgs(args),
		Reversible: reversible,
	}
}

// gate runs the policy check for a call. It returns nil to proceed, or a
// CUE_CAP_* error (after recording the blocked attempt as a "denied" effect) when
// the policy denies or a required confirmation is not granted. The tool's Invoke
// is never reached on a non-nil return — the safe-by-construction guarantee
// (DESIGN.md §7, §9).
func (i *Interp) gate(node ast.Node, rec effectlog.Record, name string, args []object.Object, rev object.Reversibility) object.Object {
	switch i.Policy.Check(name, args, rev) {
	case policy.Deny:
		return i.recordDenied(node, rec, diag.CapDenied, "%s: denied by policy", name)
	case policy.Prompt:
		prompter := i.Prompter
		if prompter == nil {
			prompter = policy.DenyPrompter{}
		}
		ok, perr := prompter.Confirm(name, args)
		if perr != nil {
			return i.recordDenied(node, rec, diag.CapPromptRequired,
				"%s: confirmation failed: %s", name, perr.Error())
		}
		if !ok {
			if _, deny := prompter.(policy.DenyPrompter); deny {
				return i.recordDenied(node, rec, diag.CapPromptRequired,
					"%s: policy requires confirmation; no prompter configured", name)
			}
			return i.recordDenied(node, rec, diag.CapPromptRequired,
				"%s: confirmation declined", name)
		}
	}
	return nil
}

// recordError logs a failed call (status "error") and returns the matching
// CUE_TOOL_* error at the call site.
func (i *Interp) recordError(node ast.Node, rec effectlog.Record, msg string) object.Object {
	rec.Status = "error"
	rec.Error = &msg
	i.Effects.Append(rec)
	return newError(node, diag.ToolFailure, "%s", msg)
}

// recordOK logs a successful call (status "ok") with its result rendered to a
// plain JSON value for the envelope and durable log.
func (i *Interp) recordOK(rec effectlog.Record, result object.Object) {
	rec.Status = "ok"
	rec.Result = object.ToAny(result)
	i.Effects.Append(rec)
}

// recordDenied logs a blocked call as a "denied" effect (so the attempt is
// auditable) and returns the matching CUE_CAP_* error. The tool's Invoke is never
// reached, which is the safe-by-construction guarantee (DESIGN.md §7, §9).
func (i *Interp) recordDenied(node ast.Node, rec effectlog.Record, code, format string, args ...any) object.Object {
	msg := fmt.Sprintf(format, args...)
	rec.Status = "denied"
	rec.Error = &msg
	i.Effects.Append(rec)
	return &object.Error{Code: code, Message: msg, Span: node.Span()}
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
