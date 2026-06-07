// Package llm is the pluggable provider seam behind the `llm()` agent-runtime
// primitive (DESIGN.md §8). The program-facing `llm(prompt, [opts])` builtin (in
// the evaluator) routes through a Provider; the provider is what actually turns a
// prompt into a completion.
//
// The default provider is an offline, deterministic MockProvider — no network, no
// API key — so tests and examples are reproducible (DESIGN.md §14). A real
// provider (OpenAI, Anthropic, …) would satisfy the same interface and be swapped
// in at construction; nothing else in the runtime changes. This is the same
// "outer agent vs. the llm() the program calls" split §1 is careful to keep
// distinct: this package is only the inner tool, never the author of the program.
package llm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// Request is one completion request. Schema, when non-nil, asks the provider for
// structured output: a JSON-object-shaped completion whose keys and value types
// match the schema (a key→type-name map, e.g. {"title":"STRING"}). A nil Schema
// means a free-form string completion is wanted. Keeping the schema in the request
// lets a real provider use it for constrained decoding / function-calling, while
// the MockProvider uses it to synthesise a satisfying value.
type Request struct {
	Prompt string
	Schema map[string]string // key → Cue type name (STRING|INTEGER|FLOAT|BOOLEAN); nil = free-form
}

// Provider turns a completion request into a result. A string-shaped request
// (Schema == nil) returns a string in Text; a schema-shaped request returns a
// map[string]any in Object whose values are Go scalars matching the requested
// types. Exactly one of Text/Object is meaningful, selected by whether the request
// carried a schema. A provider error surfaces to the program as a CUE_TOOL_* error
// at the call site, like any other tool failure (DESIGN.md §7, §8).
type Provider interface {
	Complete(ctx context.Context, req Request) (Result, error)
}

// Result is a completion. For a free-form request Text holds the string; for a
// schema request Object holds the validated key→value map. The caller (the llm
// builtin) selects the field matching the request shape.
type Result struct {
	Text   string
	Object map[string]any
}

// MockProvider is the default, deterministic, offline provider (DESIGN.md §8,
// §14). It never makes a network call and needs no API key, so every example and
// test that calls llm() is reproducible. Its output is a stable function of the
// prompt (and, for structured requests, the schema), which is exactly what makes a
// run replayable and a test assertable.
type MockProvider struct{}

// Complete returns a deterministic completion. For a free-form request it echoes a
// stable, recognisable transform of the prompt. For a schema request it
// synthesises a value for every requested key whose Go type satisfies the
// requested Cue type, so the happy path through schema validation always passes —
// the schema mismatch path is exercised by a real provider returning the wrong
// shape, modelled in tests by a stub.
func (MockProvider) Complete(_ context.Context, req Request) (Result, error) {
	if req.Schema == nil {
		return Result{Text: mockText(req.Prompt)}, nil
	}
	obj := make(map[string]any, len(req.Schema))
	// Iterate keys in sorted order so the synthesised values are deterministic
	// regardless of map iteration order.
	keys := make([]string, 0, len(req.Schema))
	for k := range req.Schema {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		obj[k] = mockValue(k, req.Schema[k], req.Prompt)
	}
	return Result{Object: obj}, nil
}

// mockText is the deterministic free-form completion: a fixed prefix plus the
// prompt, so a caller can recognise the mock and a test can assert on it exactly.
func mockText(prompt string) string {
	return "[mock] " + prompt
}

// mockValue synthesises a deterministic value of the requested Cue type for a
// schema key. Strings echo the key; numeric/boolean values are derived from a hash
// of (key, prompt) so they are stable per input yet vary across keys. An
// unrecognised type name falls back to a string (validation in the evaluator is
// the authority on type names; the mock just produces *something* typed).
func mockValue(key, typeName, prompt string) any {
	switch strings.ToUpper(typeName) {
	case "STRING":
		return "[mock] " + key
	case "INTEGER":
		return int64(hashSeed(key, prompt) % 100)
	case "FLOAT":
		return float64(hashSeed(key, prompt)%1000) / 10.0
	case "BOOLEAN":
		return hashSeed(key, prompt)%2 == 0
	default:
		return "[mock] " + key
	}
}

// hashSeed derives a small deterministic non-negative integer from its inputs.
func hashSeed(parts ...string) uint64 {
	h := sha256.Sum256([]byte(fmt.Sprint(parts)))
	return binary.BigEndian.Uint64(h[:8]) >> 1 // shift to stay non-negative as int64
}
