package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MarcelloLR/cue/object"
)

// testConfig is the canonical Phase 3 policy used across the decision tests: an
// allow default with an ordered rule list that exercises exact match, a prefix
// glob, the catch-all, and a reversibility-only rule.
const testConfig = `{
  "default": "allow",
  "rules": [
    { "tool": "fs.delete", "decision": "deny" },
    { "tool": "fs.write",  "decision": "prompt" },
    { "reversibility": "irreversible", "decision": "deny" },
    { "tool": "fs.*",      "decision": "allow" },
    { "tool": "secret.*",  "decision": "deny" }
  ]
}`

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(src), "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

// TestCheckDecisions covers exact, prefix-glob, catch-all/default, and
// reversibility-based matching with first-match-wins ordering (DESIGN.md §7).
func TestCheckDecisions(t *testing.T) {
	cfg := mustParse(t, testConfig)

	tests := []struct {
		name string
		tool string
		rev  object.Reversibility
		want Decision
	}{
		// Exact rule wins.
		{"exact deny", "fs.delete", object.Irreversible, Deny},
		{"exact prompt", "fs.write", object.Reversible, Prompt},
		// First match wins: fs.read is reversible so the irreversible rule does not
		// fire; the fs.* glob then allows it.
		{"prefix glob allow", "fs.read", object.Reversible, Allow},
		// A different irreversible tool falls to the reversibility-only rule.
		{"reversibility rule deny", "db.drop", object.Irreversible, Deny},
		// Prefix glob on a different namespace.
		{"glob deny", "secret.read", object.Reversible, Deny},
		// No rule matches → default.
		{"default allow", "http.get", object.Reversible, Allow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cfg.Check(tt.tool, nil, tt.rev)
			if got != tt.want {
				t.Errorf("Check(%q, %s) = %q, want %q", tt.tool, tt.rev, got, tt.want)
			}
		})
	}
}

// TestFirstMatchWins proves ordering is meaningful: a broad allow placed before a
// specific deny shadows it.
func TestFirstMatchWins(t *testing.T) {
	cfg := mustParse(t, `{
      "default": "deny",
      "rules": [
        { "tool": "fs.*",      "decision": "allow" },
        { "tool": "fs.delete", "decision": "deny" }
      ]
    }`)
	if got := cfg.Check("fs.delete", nil, object.Irreversible); got != Allow {
		t.Errorf("fs.delete = %q, want Allow (earlier fs.* rule shadows the deny)", got)
	}
}

// TestStarMatchesAny confirms "*" and "" tool patterns match any tool.
func TestStarMatchesAny(t *testing.T) {
	cfg := mustParse(t, `{"default":"allow","rules":[{"tool":"*","decision":"deny"}]}`)
	if got := cfg.Check("anything.at.all", nil, object.Reversible); got != Deny {
		t.Errorf(`"*" rule = %q, want Deny`, got)
	}
}

// TestMalformedConfig covers every validation path Load/Parse guards.
func TestMalformedConfig(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"invalid JSON", `{`},
		{"unknown field", `{"default":"allow","bogus":1}`},
		{"missing default", `{"rules":[]}`},
		{"invalid default", `{"default":"maybe"}`},
		{"rule missing decision", `{"default":"allow","rules":[{"tool":"fs.*"}]}`},
		{"rule invalid decision", `{"default":"allow","rules":[{"tool":"fs.*","decision":"nope"}]}`},
		{"rule invalid reversibility", `{"default":"allow","rules":[{"reversibility":"sometimes","decision":"deny"}]}`},
		{"rule matches nothing", `{"default":"allow","rules":[{"decision":"deny"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.src), "p.json"); err == nil {
				t.Errorf("Parse(%q) = nil error, want a validation error", tt.src)
			}
		})
	}
}

// TestLoadFromFile round-trips a config through the filesystem and through a
// missing-file error.
func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cue.policy.json")
	if err := os.WriteFile(path, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Check("fs.delete", nil, object.Irreversible); got != Deny {
		t.Errorf("loaded config fs.delete = %q, want Deny", got)
	}

	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Errorf("Load(missing) = nil error, want an error")
	}
}

// TestPrompters exercises the Prompter seam variants.
func TestPrompters(t *testing.T) {
	if ok, _ := (DenyPrompter{}).Confirm("fs.write", nil); ok {
		t.Errorf("DenyPrompter should decline")
	}
	allow := FuncPrompter(func(string, []object.Object) (bool, error) { return true, nil })
	if ok, _ := allow.Confirm("fs.write", nil); !ok {
		t.Errorf("FuncPrompter(true) should allow")
	}
}

// TestAllowAll keeps the backward-compatible default honest.
func TestAllowAll(t *testing.T) {
	if got := (AllowAll{}).Check("fs.delete", nil, object.Irreversible); got != Allow {
		t.Errorf("AllowAll = %q, want Allow", got)
	}
}
