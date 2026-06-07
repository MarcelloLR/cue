package registry

import (
	"context"
	"testing"

	"github.com/MarcelloLR/cue/object"
)

// fakeTool is a deterministic ToolImpl for registry tests — no network, no
// side effects.
type fakeTool struct {
	name string
	sig  object.Signature
	rev  object.Reversibility
}

func (f *fakeTool) Name() string                        { return f.name }
func (f *fakeTool) Signature() object.Signature         { return f.sig }
func (f *fakeTool) Reversibility() object.Reversibility { return f.rev }
func (f *fakeTool) Invoke(ctx context.Context, args []object.Object) (object.Object, error) {
	return object.NewHash(), nil
}

func newTestRegistry() *Registry {
	r := New()
	r.Register(&fakeTool{
		name: "http.get",
		sig:  object.Signature{Params: []object.Param{{Name: "url", Type: "string"}}},
		rev:  object.Reversible,
	})
	r.Register(&fakeTool{
		name: "http.post",
		sig:  object.Signature{Params: []object.Param{{Name: "url", Type: "string"}, {Name: "body", Type: "string"}}},
		rev:  object.Irreversible,
	})
	r.Register(&fakeTool{
		name: "strings.upper",
		sig:  object.Signature{Params: []object.Param{{Name: "s", Type: "string"}}},
		rev:  object.Reversible,
	})
	return r
}

func TestResolve(t *testing.T) {
	r := newTestRegistry()
	if _, ok := r.Resolve("http.get"); !ok {
		t.Errorf("http.get should resolve")
	}
	if _, ok := r.Resolve("http.delete"); ok {
		t.Errorf("http.delete should not resolve")
	}
}

func TestNamespaces(t *testing.T) {
	r := newTestRegistry()
	ns := r.Namespaces()

	http, ok := ns["http"]
	if !ok {
		t.Fatalf("expected an http namespace")
	}
	if http.Name != "http" {
		t.Errorf("namespace name = %q, want http", http.Name)
	}
	if _, ok := http.Members["get"]; !ok {
		t.Errorf("http namespace missing member get")
	}
	if _, ok := http.Members["post"]; !ok {
		t.Errorf("http namespace missing member post")
	}
	if _, ok := ns["strings"].Members["upper"]; !ok {
		t.Errorf("strings namespace missing member upper")
	}
	if http.Type() != object.NAMESPACE_OBJ {
		t.Errorf("namespace Type() = %v", http.Type())
	}
}

func TestCatalog(t *testing.T) {
	r := newTestRegistry()
	cat := r.Catalog()

	if len(cat.Tools) != 3 {
		t.Fatalf("catalog tools = %d, want 3", len(cat.Tools))
	}
	// Sorted by name: http.get, http.post, strings.upper.
	if cat.Tools[0].Name != "http.get" {
		t.Errorf("first tool = %q, want http.get", cat.Tools[0].Name)
	}
	get := cat.Tools[0]
	if len(get.Signature.Params) != 1 || get.Signature.Params[0].Name != "url" {
		t.Errorf("http.get params = %+v", get.Signature.Params)
	}
	if get.Reversibility != object.Reversible {
		t.Errorf("http.get reversibility = %v", get.Reversibility)
	}
	if !get.Gated {
		t.Errorf("tools should be gated")
	}
	if cat.Grammar == "" {
		t.Errorf("catalog should include a grammar summary")
	}
}

func TestSuggest(t *testing.T) {
	r := newTestRegistry()
	cases := []struct {
		name string
		want string
	}{
		{"http.gt", "http.get"},
		{"strings.uppr", "strings.upper"},
		{"http.get", "http.get"}, // exact
		{"zzzzzzzzzz", ""},       // nothing close
	}
	for _, tc := range cases {
		if got := r.Suggest(tc.name); got != tc.want {
			t.Errorf("Suggest(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
