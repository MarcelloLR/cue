package object

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFromAnyRoundTripsToAny confirms FromAny is the inverse of ToAny for the data
// values deterministic replay reconstructs (DESIGN.md §10): a value serialised by
// ToAny and lifted back by FromAny inspects identically.
func TestFromAnyRoundTrips(t *testing.T) {
	cases := []Object{
		&Null{},
		&Boolean{Value: true},
		&Boolean{Value: false},
		&String{Value: "hello"},
		&Integer{Value: 42},
		&Integer{Value: -7},
		&Float{Value: 3.5},
		&Array{Elements: []Object{&Integer{Value: 1}, &String{Value: "x"}, &Boolean{Value: true}}},
	}
	for _, want := range cases {
		got := FromAny(ToAny(want))
		if got.Inspect() != want.Inspect() || got.Type() != want.Type() {
			t.Errorf("FromAny(ToAny(%s)) = %s (%s), want %s (%s)",
				want.Inspect(), got.Inspect(), got.Type(), want.Inspect(), want.Type())
		}
	}
}

// TestFromAnyHashRoundTrip checks a hash round-trips structurally (keys sorted for
// determinism on the way back).
func TestFromAnyHashRoundTrip(t *testing.T) {
	h := NewHash()
	h.Set("b", &Integer{Value: 2})
	h.Set("a", &String{Value: "x"})

	got, ok := FromAny(ToAny(h)).(*Hash)
	if !ok {
		t.Fatalf("FromAny of a hash should be a Hash")
	}
	if len(got.Keys) != 2 {
		t.Fatalf("hash keys = %v, want 2", got.Keys)
	}
	if got.Pairs["a"].(*String).Value != "x" || got.Pairs["b"].(*Integer).Value != 2 {
		t.Errorf("hash values lost in round-trip: %s", got.Inspect())
	}
	// Keys come back sorted so the reconstruction is deterministic.
	if got.Keys[0] != "a" || got.Keys[1] != "b" {
		t.Errorf("hash keys = %v, want sorted [a b]", got.Keys)
	}
}

// TestFromAnyThroughJSON mirrors the real replay path: a value serialised to JSON
// (as a JSONL log line stores it) and decoded with UseNumber must reconstruct to
// the original type — crucially, a logged int does not widen to a float.
func TestFromAnyThroughJSON(t *testing.T) {
	orig := NewHash()
	orig.Set("count", &Integer{Value: 5})
	orig.Set("ratio", &Float{Value: 1.5})
	orig.Set("name", &String{Value: "ada"})

	blob, err := json.Marshal(ToAny(orig))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(blob)))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		t.Fatal(err)
	}

	got, ok := FromAny(decoded).(*Hash)
	if !ok {
		t.Fatalf("decoded value should reconstruct to a Hash")
	}
	if _, ok := got.Pairs["count"].(*Integer); !ok {
		t.Errorf("count should reconstruct as Integer, got %T", got.Pairs["count"])
	}
	if _, ok := got.Pairs["ratio"].(*Float); !ok {
		t.Errorf("ratio should reconstruct as Float, got %T", got.Pairs["ratio"])
	}
	if got.Pairs["name"].(*String).Value != "ada" {
		t.Errorf("name lost in round-trip")
	}
}

// TestFromAnyWholeNumberFloatIsInteger confirms a JSON-decoded whole-number float64
// (the shape a logged int takes when decoded WITHOUT UseNumber) reconstructs as an
// Integer, so replay stays faithful regardless of how the log was decoded.
func TestFromAnyWholeNumberFloatIsInteger(t *testing.T) {
	if got := FromAny(float64(7)); got.Type() != INTEGER_OBJ {
		t.Errorf("FromAny(7.0) type = %s, want INTEGER", got.Type())
	}
	if got := FromAny(float64(7.25)); got.Type() != FLOAT_OBJ {
		t.Errorf("FromAny(7.25) type = %s, want FLOAT", got.Type())
	}
}
