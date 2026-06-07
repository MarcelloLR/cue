// Package registry is the single source of truth for the tools a Cue program can
// call (DESIGN.md §7). Registration drives everything downstream: the namespace
// values injected into the environment, the catalog dumped for an agent's system
// prompt (§9), and the did-you-mean suggestions in name-resolution diagnostics.
// Nothing about the tool surface is hand-maintained — it is all derived here.
package registry

import (
	"sort"
	"strings"

	"github.com/MarcelloLR/cue/object"
)

// Registry holds registered tool implementations keyed by full namespaced name
// (e.g. "http.get").
type Registry struct {
	tools map[string]object.ToolImpl
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{tools: map[string]object.ToolImpl{}}
}

// Register adds an implementation under its Name(). A duplicate name overwrites
// the prior registration; registration is the single source of truth, so the
// last writer wins by design.
func (r *Registry) Register(impl object.ToolImpl) {
	r.tools[impl.Name()] = impl
}

// Resolve looks up a tool by its full namespaced name.
func (r *Registry) Resolve(full string) (object.ToolImpl, bool) {
	impl, ok := r.tools[full]
	return impl, ok
}

// Names returns every registered tool name, sorted, for stable iteration.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Namespaces groups the registered tools by their namespace prefix and builds
// the *object.Namespace values ready to inject as environment bindings. A tool
// named "http.get" becomes a member "get" of the "http" namespace; a name with
// no dot is treated as its own single-tool namespace.
func (r *Registry) Namespaces() map[string]*object.Namespace {
	out := map[string]*object.Namespace{}
	for full, impl := range r.tools {
		ns, member := splitName(full)
		n, ok := out[ns]
		if !ok {
			n = &object.Namespace{Name: ns, Members: map[string]*object.Tool{}}
			out[ns] = n
		}
		n.Members[member] = &object.Tool{Impl: impl}
	}
	return out
}

// splitName splits "ns.member" into its parts. Names without a dot use the whole
// name as both namespace and member.
func splitName(full string) (ns, member string) {
	if i := strings.IndexByte(full, '.'); i >= 0 {
		return full[:i], full[i+1:]
	}
	return full, full
}

// --- Catalog (DESIGN.md §9) ---

// CatalogParam is one parameter in a catalog entry's signature.
type CatalogParam struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// CatalogSignature is the serializable form of a tool signature.
type CatalogSignature struct {
	Params   []CatalogParam `json:"params"`
	Variadic bool           `json:"variadic"`
}

// CatalogTool is one tool's catalog entry: its name, signature, reversibility,
// and whether calls are gated by a policy (always true in v1 — tools are the
// gated surface; builtins are not).
type CatalogTool struct {
	Name          string               `json:"name"`
	Signature     CatalogSignature     `json:"signature"`
	Reversibility object.Reversibility `json:"reversibility"`
	Gated         bool                 `json:"gated"`
}

// Catalog is the machine-readable tool surface plus a short grammar summary —
// exactly what goes into an agent's system prompt (DESIGN.md §9). It is
// generated from the registry, never hand-written.
type Catalog struct {
	Tools   []CatalogTool `json:"tools"`
	Grammar string        `json:"grammar"`
}

// grammarSummary is a compact reminder of Cue's surface for the agent prompt.
const grammarSummary = "let x = expr | x = expr | fn(a, b) { ... } | " +
	"if cond { ... } else { ... } | for x in xs { ... } | " +
	"parallel (x in xs[, limit = n]) { ... } | " +
	"retry (n) { ... } | ns.tool(args) | llm(prompt[, opts]) | " +
	"ask_human(prompt) | [a, b] | {\"k\": v} | a.b | a[i] | // comment"

// Catalog produces the serializable catalog for `cue catalog`, sorted by tool
// name for deterministic output.
func (r *Registry) Catalog() Catalog {
	cat := Catalog{Grammar: grammarSummary}
	for _, name := range r.Names() {
		impl := r.tools[name]
		sig := impl.Signature()
		params := make([]CatalogParam, 0, len(sig.Params))
		for _, p := range sig.Params {
			params = append(params, CatalogParam{Name: p.Name, Type: p.Type})
		}
		cat.Tools = append(cat.Tools, CatalogTool{
			Name:          name,
			Signature:     CatalogSignature{Params: params, Variadic: sig.Variadic},
			Reversibility: impl.Reversibility(),
			Gated:         true,
		})
	}
	return cat
}

// Suggest returns the registered tool name closest to name by Levenshtein
// distance, for did-you-mean hints (used by the static checker and the runtime).
// It returns "" when nothing is reasonably close (distance grows with the name's
// length, so a wild miss yields no suggestion).
func Suggest(name string, candidates []string) string {
	best := ""
	bestDist := -1
	for _, c := range candidates {
		d := levenshtein(name, c)
		if bestDist == -1 || d < bestDist {
			bestDist, best = d, c
		}
	}
	if best == "" {
		return ""
	}
	// Only suggest when the edit distance is small relative to the longer name,
	// so "githab.search" → "github.search" but "xyz" → nothing.
	limit := len(name)/2 + 1
	if bestDist > limit {
		return ""
	}
	return best
}

// Suggest is the registry-scoped convenience over the package Suggest helper: it
// searches the registered tool names.
func (r *Registry) Suggest(name string) string {
	return Suggest(name, r.Names())
}

// levenshtein computes the edit distance between a and b.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
