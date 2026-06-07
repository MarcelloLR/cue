// Package policy is the capability-gating seam (DESIGN.md §7). Every tool call is
// checked against a Policy before it runs, which is what makes tool access — the
// only outside-world access in Cue — safe by construction.
//
// Phase 1 shipped a single AllowAll policy but routed every invocation through the
// interface so the gate was already on the path. Phase 3 fills it in: a real,
// config-driven policy (loaded from cue.policy.json) maps (tool, args,
// reversibility) → Allow | Deny | Prompt via an ordered, first-match-wins rule
// list with a default fallback. Deny becomes the learnable CUE_CAP_* boundary;
// Prompt routes through a Prompter seam that Phase 4's ask_human will satisfy.
// AllowAll stays the default when no --policy is given, so behaviour is backward
// compatible.
package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/MarcelloLR/cue/object"
)

// Decision is a policy's verdict on a single tool call.
type Decision string

const (
	// Allow lets the call proceed.
	Allow Decision = "allow"
	// Deny blocks the call; the runtime reports it as CUE_CAP_001 so the agent
	// learns the boundary instead of silently failing, and records a denied effect
	// so the attempt is auditable.
	Deny Decision = "deny"
	// Prompt requires human confirmation, routed through a Prompter (Phase 4's
	// ask_human). With no prompter configured the runtime conservatively denies.
	Prompt Decision = "prompt"
)

// Policy maps (tool, args, reversibility) to a Decision (DESIGN.md §7).
type Policy interface {
	Check(toolName string, args []object.Object, rev object.Reversibility) Decision
}

// AllowAll permits every call. It is the default when no --policy is given and the
// baseline a real policy specializes from.
type AllowAll struct{}

// Check always returns Allow.
func (AllowAll) Check(toolName string, args []object.Object, rev object.Reversibility) Decision {
	return Allow
}

// --- Config-driven policy (DESIGN.md §7: cue.policy.json) ---
//
// The config is a JSON document with a `default` decision and an ordered list of
// `rules`. Each rule matches on a tool-name pattern (exact, or a trailing-`*`
// prefix glob, or the catch-all `"*"`) and optionally on reversibility; the first
// rule whose conditions all match decides, else `default` applies. Example:
//
//	{
//	  "default": "allow",
//	  "rules": [
//	    { "tool": "fs.delete", "decision": "deny" },
//	    { "tool": "fs.write",  "decision": "prompt" },
//	    { "reversibility": "irreversible", "decision": "deny" },
//	    { "tool": "fs.*",      "decision": "allow" }
//	  ]
//	}
//
// First-match-wins makes the ordering meaningful: put specific rules before broad
// ones. Reversibility-only rules (no tool, or tool "*") express blanket policies
// like "deny everything irreversible".

// Rule is one ordered entry in a Config. A rule matches a call when its Tool
// pattern matches the tool name AND (if set) its Reversibility equals the tool's.
type Rule struct {
	// Tool is the name pattern: an exact name ("fs.delete"), a trailing-`*` prefix
	// glob ("fs.*" matches "fs.read", "fs.write", …), or "*" / "" (match any).
	Tool string `json:"tool,omitempty"`
	// Reversibility, when set, additionally requires the call's reversibility to
	// match ("reversible" | "irreversible" | "unknown"). Empty means "any".
	Reversibility object.Reversibility `json:"reversibility,omitempty"`
	// Decision is the verdict when this rule matches.
	Decision Decision `json:"decision"`
}

// Config is the loadable policy document (DESIGN.md §7). Default is the fallback
// decision when no rule matches; Rules are evaluated top-to-bottom, first match
// wins. A Config is itself a Policy.
type Config struct {
	Default Decision `json:"default"`
	Rules   []Rule   `json:"rules"`
}

// Load reads and validates a cue.policy.json file. Validation errors are prefixed
// with the path and use CUE_*-style language so a malformed policy fails loudly
// rather than silently mis-gating tools (DESIGN.md §7, §9).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cue: policy: %w", err)
	}
	return Parse(data, path)
}

// Parse validates a policy document from raw JSON bytes. path is used only for
// error messages (pass "" for in-memory configs).
func Parse(data []byte, path string) (*Config, error) {
	where := "policy"
	if path != "" {
		where = "policy " + path
	}

	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("cue: %s: invalid JSON: %w", where, err)
	}

	if cfg.Default == "" {
		return nil, fmt.Errorf("cue: %s: missing required \"default\" decision (allow|deny|prompt)", where)
	}
	if !validDecision(cfg.Default) {
		return nil, fmt.Errorf("cue: %s: invalid default decision %q (want allow|deny|prompt)", where, cfg.Default)
	}
	for i, r := range cfg.Rules {
		if r.Decision == "" {
			return nil, fmt.Errorf("cue: %s: rule %d missing \"decision\"", where, i)
		}
		if !validDecision(r.Decision) {
			return nil, fmt.Errorf("cue: %s: rule %d has invalid decision %q (want allow|deny|prompt)", where, i, r.Decision)
		}
		if r.Reversibility != "" && !validReversibility(r.Reversibility) {
			return nil, fmt.Errorf("cue: %s: rule %d has invalid reversibility %q (want reversible|irreversible|unknown)", where, i, r.Reversibility)
		}
		if r.Tool == "" && r.Reversibility == "" {
			return nil, fmt.Errorf("cue: %s: rule %d matches nothing: set \"tool\" and/or \"reversibility\" (use \"*\" to match any tool)", where, i)
		}
	}
	return &cfg, nil
}

// Check returns the decision for a call: the first matching rule's, else Default.
// The args are part of the interface (a future rule kind may match on them) but
// the v1 rule schema gates on tool name and reversibility only (DESIGN.md §7).
func (c *Config) Check(toolName string, args []object.Object, rev object.Reversibility) Decision {
	for _, r := range c.Rules {
		if matchTool(r.Tool, toolName) && (r.Reversibility == "" || r.Reversibility == rev) {
			return r.Decision
		}
	}
	return c.Default
}

// matchTool reports whether pattern matches name. "" and "*" match anything; a
// trailing "*" is a prefix glob ("fs.*" matches "fs.read"); otherwise it is an
// exact match.
func matchTool(pattern, name string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == name
	}
}

func validDecision(d Decision) bool {
	return d == Allow || d == Deny || d == Prompt
}

func validReversibility(r object.Reversibility) bool {
	return r == object.Reversible || r == object.Irreversible || r == object.ReversibilityUnknown
}

// --- Prompt seam (DESIGN.md §7) ---

// Prompter resolves a Prompt decision into Allow/Deny at call time. Phase 3 has
// no interactive prompter, so the runtime uses DenyPrompter by default (a Prompt
// without a prompter is conservatively denied). Phase 4's ask_human becomes the
// interactive Prompter, plugging into this same seam under the global prompt lock.
type Prompter interface {
	// Confirm asks whether the gated call may proceed. Returning (true, nil)
	// allows it; (false, nil) denies it; a non-nil error denies and surfaces.
	Confirm(toolName string, args []object.Object) (bool, error)
}

// DenyPrompter is the conservative Phase 3 default: every Prompt is declined,
// because allowing an unconfirmed sensitive call would defeat the gate. The
// runtime reports this as CUE_CAP_002 with a "no prompter configured" message.
type DenyPrompter struct{}

// Confirm always declines.
func (DenyPrompter) Confirm(toolName string, args []object.Object) (bool, error) {
	return false, nil
}

// FuncPrompter adapts a function to the Prompter interface, mainly for tests and
// harness injection.
type FuncPrompter func(toolName string, args []object.Object) (bool, error)

// Confirm calls the wrapped function.
func (f FuncPrompter) Confirm(toolName string, args []object.Object) (bool, error) {
	return f(toolName, args)
}
