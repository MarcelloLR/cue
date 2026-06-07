// Package envelope builds the machine-readable run-result envelope that is the
// heart of Cue's agent contract (DESIGN.md §9). Every run — whether it executed
// (`cue run`) or only checked (`cue check`) — yields one Envelope: an ok flag,
// the result value, the structured diagnostics, and the effect-log records.
//
// It lives in runtime/ rather than in diag because it must depend on both object
// (to serialize a result value) and effectlog (to embed the records); putting the
// builder here keeps diag a leaf package with no import cycle.
package envelope

import (
	"strings"

	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/effectlog"
)

// Position is a 1-based line/column point in the JSON span.
type Position struct {
	Line int `json:"line"`
	Col  int `json:"col"`
}

// Span is the JSON form of a source range.
type Span struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Diagnostic is the JSON form of a diag.Diagnostic, shaped exactly as §9
// specifies. Optional fields are omitted when empty so the envelope stays terse.
type Diagnostic struct {
	Code     string         `json:"code"`
	Severity string         `json:"severity"`
	Message  string         `json:"message"`
	Span     Span           `json:"span"`
	Snippet  string         `json:"snippet,omitempty"`
	Hint     string         `json:"hint,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}

// Envelope is the full §9 run result. Result is the program's value as a plain
// JSON value (or null); Trace is reserved for an optional step trace (empty in
// Phase 1 but present so the shape is stable for consumers).
type Envelope struct {
	OK          bool               `json:"ok"`
	Result      any                `json:"result"`
	Diagnostics []Diagnostic       `json:"diagnostics"`
	Effects     []effectlog.Record `json:"effects"`
	Trace       []any              `json:"trace"`
}

// Build assembles an Envelope from a run's outputs. ok is true when no
// error-severity diagnostic is present. result is serialized via object.ToAny;
// each diagnostic's snippet is the source line(s) its span covers, extracted from
// source. A nil result (e.g. from `cue check`) serializes to JSON null.
func Build(result object.Object, diags []diag.Diagnostic, effects []effectlog.Record, source string) Envelope {
	env := Envelope{
		OK:          true,
		Result:      object.ToAny(result),
		Diagnostics: make([]Diagnostic, 0, len(diags)),
		Effects:     effects,
		Trace:       []any{},
	}
	if env.Effects == nil {
		env.Effects = []effectlog.Record{}
	}

	lines := strings.Split(source, "\n")
	for _, d := range diags {
		if d.Severity == diag.SeverityError {
			env.OK = false
		}
		jd := Diagnostic{
			Code:     d.Code,
			Severity: string(d.Severity),
			Message:  d.Message,
			Span: Span{
				Start: Position{Line: d.Span.Start.Line, Col: d.Span.Start.Col},
				End:   Position{Line: d.Span.End.Line, Col: d.Span.End.Col},
			},
			Hint: d.Hint,
			Data: d.Data,
		}
		jd.Snippet = d.Snippet
		if jd.Snippet == "" {
			jd.Snippet = snippet(lines, d.Span.Start.Line, d.Span.End.Line)
		}
		env.Diagnostics = append(env.Diagnostics, jd)
	}
	return env
}

// snippet returns the source line(s) [startLine, endLine] (1-based, inclusive),
// trimmed of trailing whitespace, or "" when the range is out of bounds.
func snippet(lines []string, startLine, endLine int) string {
	if startLine < 1 || startLine > len(lines) {
		return ""
	}
	if endLine < startLine {
		endLine = startLine
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	return strings.TrimRight(strings.Join(lines[startLine-1:endLine], "\n"), " \t\r")
}
