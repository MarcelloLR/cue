package envelope

import (
	"encoding/json"
	"testing"

	"github.com/MarcelloLR/cue/diag"
	"github.com/MarcelloLR/cue/object"
	"github.com/MarcelloLR/cue/runtime/effectlog"
	"github.com/MarcelloLR/cue/token"
)

func TestBuildOKResult(t *testing.T) {
	result := &object.Integer{Value: 42}
	env := Build(result, nil, nil, "")

	if !env.OK {
		t.Errorf("OK should be true with no diagnostics")
	}
	if env.Result != int64(42) {
		t.Errorf("Result = %v (%T), want 42", env.Result, env.Result)
	}
	if env.Diagnostics == nil || len(env.Diagnostics) != 0 {
		t.Errorf("Diagnostics should be empty non-nil, got %v", env.Diagnostics)
	}
	if env.Effects == nil {
		t.Errorf("Effects should be empty non-nil for stable JSON shape")
	}
	if env.Trace == nil {
		t.Errorf("Trace should be empty non-nil")
	}
}

func TestBuildShapeMatchesContract(t *testing.T) {
	source := "let x = htp.get(\"u\")\nx"
	diags := []diag.Diagnostic{{
		Code:     diag.NameUnknownMember,
		Severity: diag.SeverityError,
		Message:  "unknown tool",
		Span: token.Span{
			Start: token.Position{Line: 1, Col: 9},
			End:   token.Position{Line: 1, Col: 16},
		},
		Hint: "did you mean \"http.get\"?",
		Data: map[string]any{"namespace": "htp", "did_you_mean": "http.get"},
	}}
	effects := []effectlog.Record{{
		Seq: 0, Callsite: "1:9", Branch: "root", Tool: "http.get",
		Args: []any{"u"}, Reversible: true, Status: "ok", DurationMs: 3,
	}}

	env := Build(nil, diags, effects, source)
	if env.OK {
		t.Errorf("OK should be false with an error diagnostic")
	}

	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Round-trip into a generic map and assert the §9 shape.
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"ok", "result", "diagnostics", "effects", "trace"} {
		if _, ok := m[key]; !ok {
			t.Errorf("envelope missing top-level key %q", key)
		}
	}
	if m["result"] != nil {
		t.Errorf("result should be null, got %v", m["result"])
	}

	d := m["diagnostics"].([]any)[0].(map[string]any)
	if d["code"] != diag.NameUnknownMember {
		t.Errorf("diagnostic code = %v", d["code"])
	}
	span := d["span"].(map[string]any)
	start := span["start"].(map[string]any)
	if start["line"].(float64) != 1 || start["col"].(float64) != 9 {
		t.Errorf("span.start = %v", start)
	}
	if d["snippet"] != "let x = htp.get(\"u\")" {
		t.Errorf("snippet = %q, want the source line", d["snippet"])
	}
	data := d["data"].(map[string]any)
	if data["did_you_mean"] != "http.get" {
		t.Errorf("data.did_you_mean = %v", data["did_you_mean"])
	}

	e := m["effects"].([]any)[0].(map[string]any)
	if e["tool"] != "http.get" || e["status"] != "ok" {
		t.Errorf("effect = %v", e)
	}
}

func TestSnippetOmittedWhenEmpty(t *testing.T) {
	// A diagnostic whose span has no source line yields no snippet key.
	diags := []diag.Diagnostic{{
		Code:     diag.RuntimeBuiltin,
		Severity: diag.SeverityError,
		Message:  "x",
	}}
	env := Build(nil, diags, nil, "")
	b, _ := json.Marshal(env)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	d := m["diagnostics"].([]any)[0].(map[string]any)
	if _, ok := d["snippet"]; ok {
		t.Errorf("empty snippet should be omitted")
	}
	if _, ok := d["data"]; ok {
		t.Errorf("nil data should be omitted")
	}
}
