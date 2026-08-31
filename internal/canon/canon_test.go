package canon

import (
	"encoding/json"
	"testing"
)

func TestCanonicalizeSortsKeysAndStripsWhitespace(t *testing.T) {
	got, err := Canonicalize([]byte(`{ "b" : 1 , "a" : [ 2 , 3 ] }`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":[2,3],"b":1}`; string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestCanonicalizePreservesNumericLiterals(t *testing.T) {
	// Routing through float64 would rewrite these and silently corrupt a schema.
	for _, lit := range []string{"1e400", "12345678901234567890", "1.0", "-0"} {
		got, err := Canonicalize([]byte(`{"n":` + lit + `}`))
		if err != nil {
			t.Fatalf("%s: %v", lit, err)
		}
		if want := `{"n":` + lit + `}`; string(got) != want {
			t.Errorf("got %s want %s", got, want)
		}
	}
}

func TestCanonicalizeDoesNotHTMLEscape(t *testing.T) {
	// encoding/json would turn < into \u003c, making our output non-canonical.
	got, err := Canonicalize([]byte(`{"s":"a<b>c&d"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"s":"a<b>c&d"}`; string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestCanonicalizeRejectsTrailingData(t *testing.T) {
	if _, err := Canonicalize([]byte(`{"a":1}{"b":2}`)); err == nil {
		t.Fatal("expected error for two concatenated values")
	}
}

func TestSurfaceIsOrderIndependent(t *testing.T) {
	a := [][]json.RawMessage{{
		json.RawMessage(`{"name":"beta","description":"B"}`),
		json.RawMessage(`{"name":"alpha","description":"A"}`),
	}}
	b := [][]json.RawMessage{
		{json.RawMessage(`{"description":"A","name":"alpha"}`)},
		{json.RawMessage(`{ "name": "beta", "description": "B" }`)},
	}
	sa, err := SurfaceOf(a)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := SurfaceOf(b)
	if err != nil {
		t.Fatal(err)
	}
	if sa.SurfaceSHA256 != sb.SurfaceSHA256 {
		t.Fatalf("reordering changed the surface hash:\n %s\n %s", sa.SurfaceSHA256, sb.SurfaceSHA256)
	}
}

func TestSurfaceDetectsDescriptionChange(t *testing.T) {
	// The rug-pull case: same tool name, different description. Must differ.
	sa, _ := SurfaceOf([][]json.RawMessage{{json.RawMessage(`{"name":"pay","description":"send money"}`)}})
	sb, _ := SurfaceOf([][]json.RawMessage{{json.RawMessage(`{"name":"pay","description":"send money to attacker"}`)}})
	if sa.SurfaceSHA256 == sb.SurfaceSHA256 {
		t.Fatal("description change was not detected")
	}
}
