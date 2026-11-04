package graphgen

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCanonicalJSONSortsKeysAtEveryLevel(t *testing.T) {
	value := map[string]any{
		"zulu": float64(1),
		"alpha": map[string]any{
			"second": "中",
			"first":  []any{map[string]any{"b": true, "a": nil}},
		},
	}
	want := `{"alpha":{"first":[{"a":null,"b":true}],"second":"中"},"zulu":1}`
	got, err := CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("CanonicalJSON = %s, want %s", got, want)
	}
}

func TestCanonicalJSONIsStableAcrossMapIteration(t *testing.T) {
	// Go randomises map iteration order. With 64 keys an accidentally stable
	// order is not a realistic explanation for 200 identical results.
	value := make(map[string]any, 64)
	for index := 0; index < 64; index++ {
		value[fmt.Sprintf("k%02d", index)] = float64(index)
	}
	first, err := CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, `{"k00":0,"k01":1,"k02":2,`) {
		t.Fatalf("keys are not in lexical order: %s", first)
	}
	for attempt := 0; attempt < 200; attempt++ {
		again, err := CanonicalJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("encoding is not stable on attempt %d", attempt)
		}
	}
}

func TestCanonicalJSONKeepsArrayOrder(t *testing.T) {
	// Only object keys are canonicalised. Reordering an array would change the
	// value, so order must survive untouched.
	got, err := CanonicalJSON(map[string]any{"list": []any{float64(3), float64(1), float64(2)}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"list":[3,1,2]}`; got != want {
		t.Fatalf("CanonicalJSON = %s, want %s", got, want)
	}
}

func TestCanonicalHashIsIndependentOfHowTheValueWasBuilt(t *testing.T) {
	// The same context assembled as an ordered Object and as a decoded map must
	// hash identically: that is the whole point of canonicalising.
	object := Object{
		{Key: "projectId", Value: "p1"},
		{Key: "kind", Value: "plot"},
		{Key: "input", Value: Object{{Key: "chapterNumber", Value: float64(3)}}},
	}
	decoded := map[string]any{
		"input":     map[string]any{"chapterNumber": float64(3)},
		"kind":      "plot",
		"projectId": "p1",
	}
	objectHash, err := CanonicalHash(object)
	if err != nil {
		t.Fatal(err)
	}
	decodedHash, err := CanonicalHash(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if objectHash != decodedHash {
		t.Fatalf("hash depends on construction shape: %s != %s", objectHash, decodedHash)
	}
	// And it is a real sha256 hex digest, not an empty string.
	if len(objectHash) != 64 {
		t.Fatalf("hash = %q", objectHash)
	}
}

func TestCanonicalJSONSortsObjectsToo(t *testing.T) {
	// An ordered Object is still sorted, otherwise the same value would hash
	// differently depending on how the caller happened to build it.
	got, err := CanonicalJSON(Object{
		{Key: "zulu", Value: float64(1)},
		{Key: "alpha", Value: "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"alpha":"a","zulu":1}`; got != want {
		t.Fatalf("CanonicalJSON(Object) = %s, want %s", got, want)
	}
}

func TestCanonicalJSONReportsUnencodableLeaves(t *testing.T) {
	// A value EncodeJSON cannot express must surface as ErrUnsupportedJSONValue
	// rather than a panic or a silently dropped key.
	_, err := CanonicalJSON(map[string]any{"bad": make(chan int)})
	if !errors.Is(err, ErrUnsupportedJSONValue) {
		t.Fatalf("err = %v, want ErrUnsupportedJSONValue", err)
	}
}
