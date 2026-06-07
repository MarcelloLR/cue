// Package diag holds the structured-diagnostic types and the stable error
// codes that the Cue toolchain emits. It is the seed of the "runtime is the
// agent's API" pillar (DESIGN.md §9): every problem is reported as data — a
// stable code, a source span, a message, and an optional repair hint — rather
// than as ad-hoc prose.
package diag

import (
	"fmt"

	"github.com/MarcelloLR/cue/token"
)

// Severity classifies a diagnostic.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Stable diagnostic codes. The namespace prefix groups the failure kind so an
// agent (or a human) can branch on it without parsing the message.
const (
	// Lexing.
	LexIllegal            = "CUE_LEX_001"
	LexUnterminatedString = "CUE_LEX_002"

	// Parsing.
	ParseUnexpectedToken = "CUE_PARSE_001"
	ParseNoPrefix        = "CUE_PARSE_002"
	ParseInvalidAssign   = "CUE_PARSE_003"
	ParseInvalidNumber   = "CUE_PARSE_004"

	// Name resolution (runtime + static check).
	NameUnknownIdent  = "CUE_NAME_001" // unbound identifier
	NameUnknownTool   = "CUE_NAME_002" // member access on a non-namespace value
	NameUnknownMember = "CUE_NAME_003" // unknown member on a known namespace

	// Type / value errors (runtime).
	TypeMismatch     = "CUE_TYPE_001"
	TypeNotCallable  = "CUE_TYPE_002"
	TypeNotIndexable = "CUE_TYPE_003"
	TypeArgCount     = "CUE_TYPE_004"
	TypeNotIterable  = "CUE_TYPE_005"
	TypeBadKey       = "CUE_TYPE_006"

	// Capability / policy gating.
	CapDenied         = "CUE_CAP_001" // policy denied the call outright
	CapPromptRequired = "CUE_CAP_002" // policy requires confirmation; prompter declined or absent

	// Tool invocation failures (the tool's Invoke returned a Go error).
	ToolFailure = "CUE_TOOL_001"

	// Generic runtime errors.
	RuntimeDivByZero = "CUE_RUNTIME_001"
	RuntimeBuiltin   = "CUE_RUNTIME_002"

	// Deterministic replay (DESIGN.md §10). Raised when a program is replayed
	// against an effect log that has no recorded outcome for an effect's
	// (branch, callsite, occurrence) key — typically because the program was edited
	// so a call site moved, was added, or now runs a different number of times.
	ReplayMismatch = "CUE_REPLAY_001"
)

// Diagnostic is a single structured report about the program. It is the unit of
// the agent contract (DESIGN.md §9): a stable Code, the source Span, a Message,
// and optional self-repair affordances (Snippet of the offending source, a Hint,
// and machine-readable Data such as a did-you-mean suggestion).
type Diagnostic struct {
	Code     string
	Severity Severity
	Message  string
	Span     token.Span
	Snippet  string
	Hint     string
	Data     map[string]any
}

// Collector accumulates diagnostics so a single pass can report many problems
// (with error recovery) instead of aborting on the first one.
type Collector struct {
	items []Diagnostic
}

// Add records a diagnostic.
func (c *Collector) Add(d Diagnostic) {
	c.items = append(c.items, d)
}

// Error is a convenience for recording an error-severity diagnostic.
func (c *Collector) Error(code string, span token.Span, format string, args ...any) {
	c.items = append(c.items, Diagnostic{
		Code:     code,
		Severity: SeverityError,
		Message:  fmt.Sprintf(format, args...),
		Span:     span,
	})
}

// Items returns the collected diagnostics in the order they were reported.
func (c *Collector) Items() []Diagnostic { return c.items }

// HasErrors reports whether any error-severity diagnostic was collected.
func (c *Collector) HasErrors() bool {
	for _, d := range c.items {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}
