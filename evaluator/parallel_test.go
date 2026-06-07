package evaluator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MarcelloLR/cue/lexer"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/parser"
	"github.com/MarcelloLR/cue/runtime/effectlog"
)

// parallelTool is a deterministic ToolImpl for exercising the scheduler. It never
// touches the network. Each invocation is keyed off its single string argument so
// behaviour (sleep duration, error injection, concurrency tracking) is stable and
// reproducible across runs (and under the race detector).
type parallelTool struct {
	// onInvoke, if set, runs synchronously inside Invoke with the arg string and
	// the context. It returns the output value and a Go error to surface. The
	// runtime's invocation pipeline already holds no lock here, so onInvoke is the
	// place a test observes concurrency, sleeps, or returns an error.
	onInvoke func(ctx context.Context, arg string) (object.Object, error)
}

func (p *parallelTool) Name() string { return "tool.echo" }
func (p *parallelTool) Signature() object.Signature {
	return object.Signature{Params: []object.Param{{Name: "s", Type: "string"}}}
}
func (p *parallelTool) Reversibility() object.Reversibility { return object.Reversible }
func (p *parallelTool) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	arg := args[0].(*object.String).Value
	if p.onInvoke != nil {
		return p.onInvoke(ctx, arg)
	}
	return &object.String{Value: arg}, nil
}

// runParallel parses src, injects a "tool" namespace whose "echo" member is impl,
// and evaluates it. It returns the result value and the recorded effects.
func runParallel(t *testing.T, src string, impl object.ToolImpl, opts ...Option) (object.Object, []effectlog.Record) {
	t.Helper()
	p := parser.New(lexer.New(src))
	program := p.ParseProgram()
	if p.HasErrors() {
		for _, d := range p.Diagnostics() {
			t.Errorf("parse diagnostic: [%s] %s", d.Code, d.Message)
		}
		t.Fatalf("unexpected diagnostics for %q", src)
	}

	env := object.NewEnvironment()
	env.Set("tool", &object.Namespace{
		Name:    "tool",
		Members: map[string]*object.Tool{"echo": {Impl: impl}},
	})

	effects := effectlog.NewRecorder()
	allOpts := append([]Option{WithEffects(effects)}, opts...)
	return New(allOpts...).Eval(program, env), effects.Records()
}

// TestParallelOrderedResultsUnderOutOfOrderCompletion is the core correctness
// story (SPEC §7): earlier input indices finish LAST, yet results come back in
// input order. The tool sleeps for (n-i) units keyed off the index encoded in the
// argument, so index 0 sleeps longest and index n-1 returns first.
func TestParallelOrderedResultsUnderOutOfOrderCompletion(t *testing.T) {
	const n = 5
	const unit = 10 * time.Millisecond

	impl := &parallelTool{
		onInvoke: func(_ context.Context, arg string) (object.Object, error) {
			// arg is "item-<i>"; sleep so smaller i finishes later.
			i := arg[len("item-")] - '0'
			time.Sleep(time.Duration(n-int(i)) * unit)
			return &object.String{Value: arg}, nil
		},
	}

	src := `
let xs = ["item-0", "item-1", "item-2", "item-3", "item-4"]
parallel (x in xs) { tool.echo(x) }
`
	result, _ := runParallel(t, src, impl)

	arr, ok := result.(*object.Array)
	if !ok {
		t.Fatalf("got %T (%s), want Array", result, result.Inspect())
	}
	if len(arr.Elements) != n {
		t.Fatalf("len = %d, want %d", len(arr.Elements), n)
	}
	for i, el := range arr.Elements {
		s, ok := el.(*object.String)
		if !ok {
			t.Fatalf("element %d is %T, want String", i, el)
		}
		want := "item-" + string(rune('0'+i))
		if s.Value != want {
			t.Errorf("results[%d] = %q, want %q (results not in input order)", i, s.Value, want)
		}
	}
}

// TestParallelFirstErrorCancels asserts that one branch's error becomes the
// expression's value and cancels its in-flight siblings via the shared context.
// The failing branch errors immediately; the others block on <-ctx.Done() and
// record whether they observed cancellation, proving the shared gctx propagated.
func TestParallelFirstErrorCancels(t *testing.T) {
	const n = 6
	var completed int32 // branches that finished WITHOUT seeing cancellation
	var cancelled int32 // branches that observed ctx cancellation

	impl := &parallelTool{
		onInvoke: func(ctx context.Context, arg string) (object.Object, error) {
			if arg == "item-2" {
				// The designated failer returns an error right away.
				return nil, errors.New("kaboom")
			}
			// Siblings wait for either cancellation or a generous timeout. With the
			// failer cancelling first, they should hit ctx.Done().
			select {
			case <-ctx.Done():
				atomic.AddInt32(&cancelled, 1)
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				atomic.AddInt32(&completed, 1)
				return &object.String{Value: arg}, nil
			}
		},
	}

	src := `
let xs = ["item-0", "item-1", "item-2", "item-3", "item-4", "item-5"]
parallel (x in xs) { tool.echo(x) }
`
	result, _ := runParallel(t, src, impl)

	e, ok := result.(*object.Error)
	if !ok {
		t.Fatalf("got %T (%s), want the first Error", result, result.Inspect())
	}
	if e.Code != "CUE_TOOL_001" {
		t.Errorf("code = %q, want CUE_TOOL_001", e.Code)
	}
	if atomic.LoadInt32(&completed) != 0 {
		t.Errorf("%d sibling(s) ran to completion; expected all in-flight siblings to be cancelled", completed)
	}
	if atomic.LoadInt32(&cancelled) == 0 {
		t.Errorf("no sibling observed ctx cancellation; the shared context did not propagate the first error")
	}
}

// TestParallelBoundedConcurrency asserts the observed maximum in-flight branch
// count never exceeds the limit — both for the default limit and an inline
// `limit =` override. A small atomic gauge tracks in-flight invocations.
func TestParallelBoundedConcurrency(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantMax int // observed concurrency must be <= this
		nItems  int
	}{
		{
			name:    "inline limit = 2",
			src:     buildItemsSrc(8) + "\nparallel (x in xs, limit = 2) { tool.echo(x) }",
			wantMax: 2,
			nItems:  8,
		},
		{
			name:    "default limit",
			src:     buildItemsSrc(20) + "\nparallel (x in xs) { tool.echo(x) }",
			wantMax: DefaultParallelLimit,
			nItems:  20,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var inFlight int32
			var maxSeen int32

			impl := &parallelTool{
				onInvoke: func(_ context.Context, arg string) (object.Object, error) {
					cur := atomic.AddInt32(&inFlight, 1)
					// Track the high-water mark with a CAS loop.
					for {
						m := atomic.LoadInt32(&maxSeen)
						if cur <= m || atomic.CompareAndSwapInt32(&maxSeen, m, cur) {
							break
						}
					}
					// Hold the slot briefly so concurrent branches overlap.
					time.Sleep(15 * time.Millisecond)
					atomic.AddInt32(&inFlight, -1)
					return &object.String{Value: arg}, nil
				},
			}

			result, _ := runParallel(t, c.src, impl)
			arr, ok := result.(*object.Array)
			if !ok {
				t.Fatalf("got %T (%s), want Array", result, result.Inspect())
			}
			if len(arr.Elements) != c.nItems {
				t.Fatalf("len = %d, want %d", len(arr.Elements), c.nItems)
			}
			if got := atomic.LoadInt32(&maxSeen); int(got) > c.wantMax {
				t.Errorf("observed max concurrency = %d, want <= %d", got, c.wantMax)
			}
		})
	}
}

// TestParallelEffectsBranchTagged confirms each branch's effect record carries
// its "parallel:k" branch label, and that the per-(branch, callsite) occurrence
// counter starts at 0 for every distinct branch.
func TestParallelEffectsBranchTagged(t *testing.T) {
	const n = 4
	impl := &parallelTool{} // default: echoes its argument, no sleep

	src := buildItemsSrc(n) + "\nparallel (x in xs) { tool.echo(x) }"
	result, effects := runParallel(t, src, impl)

	if _, ok := result.(*object.Array); !ok {
		t.Fatalf("got %T, want Array", result)
	}
	if len(effects) != n {
		t.Fatalf("effects = %d, want %d", len(effects), n)
	}

	// Every record should be tagged "parallel:k" for some k in [0, n), each k
	// appearing exactly once, and each with occurrence 0 (one call per branch).
	seen := map[string]bool{}
	for _, rec := range effects {
		if rec.Tool != "tool.echo" {
			t.Errorf("rec.Tool = %q", rec.Tool)
		}
		wantPrefix := "parallel:"
		if len(rec.Branch) <= len(wantPrefix) || rec.Branch[:len(wantPrefix)] != wantPrefix {
			t.Errorf("rec.Branch = %q, want a %q label", rec.Branch, wantPrefix)
		}
		if rec.Occurrence != 0 {
			t.Errorf("branch %q occurrence = %d, want 0 (one call per branch)", rec.Branch, rec.Occurrence)
		}
		if seen[rec.Branch] {
			t.Errorf("branch %q recorded more than once", rec.Branch)
		}
		seen[rec.Branch] = true
	}
	for k := 0; k < n; k++ {
		label := "parallel:" + string(rune('0'+k))
		if !seen[label] {
			t.Errorf("missing effect for branch %q", label)
		}
	}
}

// TestParallelOccurrenceCountsPerBranchCallsite confirms the occurrence counter
// increments per (branch, callsite): a tool called repeatedly inside one branch
// gets occurrences 0, 1, 2, …, while a separate branch restarts at 0. Here a
// `for` loop inside each parallel branch calls the tool twice.
func TestParallelOccurrenceCountsPerBranchCallsite(t *testing.T) {
	const n = 3
	impl := &parallelTool{}

	src := buildItemsSrc(n) + `
parallel (x in xs) {
  for y in ["a", "b"] {
    tool.echo(y)
  }
}`
	_, effects := runParallel(t, src, impl)

	if len(effects) != n*2 {
		t.Fatalf("effects = %d, want %d", len(effects), n*2)
	}

	// Group occurrences by (branch, callsite). Each group should be exactly {0, 1}.
	type key struct{ branch, callsite string }
	groups := map[key][]int{}
	for _, rec := range effects {
		k := key{rec.Branch, rec.Callsite}
		groups[k] = append(groups[k], rec.Occurrence)
	}
	if len(groups) != n {
		t.Fatalf("distinct (branch, callsite) groups = %d, want %d", len(groups), n)
	}
	for k, occs := range groups {
		got := map[int]bool{}
		for _, o := range occs {
			got[o] = true
		}
		if len(occs) != 2 || !got[0] || !got[1] {
			t.Errorf("group %+v occurrences = %v, want {0, 1}", k, occs)
		}
	}
}

// TestParallelReturnInBranchUnwraps confirms a `return` inside a parallel branch
// collapses to its value (like a function body) rather than leaking a
// ReturnValue wrapper into the result array.
func TestParallelReturnInBranchUnwraps(t *testing.T) {
	impl := &parallelTool{}
	src := buildItemsSrc(3) + `
parallel (x in xs) { return tool.echo(x) }`
	result, _ := runParallel(t, src, impl)
	arr, ok := result.(*object.Array)
	if !ok {
		t.Fatalf("got %T (%s), want Array", result, result.Inspect())
	}
	for i, el := range arr.Elements {
		if _, ok := el.(*object.String); !ok {
			t.Errorf("results[%d] = %T, want unwrapped String", i, el)
		}
	}
}

// TestParallelLimitMustBePositiveInteger covers the limit type/value guards.
func TestParallelLimitMustBePositiveInteger(t *testing.T) {
	impl := &parallelTool{}
	cases := []struct {
		name string
		src  string
	}{
		{"non-integer", buildItemsSrc(2) + "\nparallel (x in xs, limit = \"two\") { tool.echo(x) }"},
		{"zero", buildItemsSrc(2) + "\nparallel (x in xs, limit = 0) { tool.echo(x) }"},
		{"negative", buildItemsSrc(2) + "\nparallel (x in xs, limit = -1) { tool.echo(x) }"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, _ := runParallel(t, c.src, impl)
			e, ok := result.(*object.Error)
			if !ok {
				t.Fatalf("got %T, want Error", result)
			}
			if e.Code != "CUE_TYPE_001" {
				t.Errorf("code = %q, want CUE_TYPE_001", e.Code)
			}
		})
	}
}

// TestParallelNonIterable confirms a non-iterable yields a type error.
func TestParallelNonIterable(t *testing.T) {
	impl := &parallelTool{}
	result, _ := runParallel(t, "parallel (x in 42) { tool.echo(x) }", impl)
	e, ok := result.(*object.Error)
	if !ok {
		t.Fatalf("got %T, want Error", result)
	}
	if e.Code != "CUE_TYPE_005" {
		t.Errorf("code = %q, want CUE_TYPE_005", e.Code)
	}
}

// buildItemsSrc returns `let xs = ["item-0", ..., "item-<n-1>"]` so tests can
// build deterministic input arrays of any size.
func buildItemsSrc(n int) string {
	out := "let xs = ["
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ", "
		}
		out += "\"item-" + string(rune('0'+i)) + "\""
	}
	return out + "]"
}
