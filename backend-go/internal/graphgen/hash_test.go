package graphgen

import (
	"errors"
	"math"
	"testing"
)

// The expected digests were produced with Node's
// createHash('sha256').update(text, 'utf8').digest('hex'), i.e. TextHash.
func TestTextHash(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty text is the hash generation-run-repository.ts seeds an artifact with",
			input: "",
			want:  "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:  "ascii",
			input: "abc",
			want:  "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		},
		{
			name:  "non-ASCII Chinese text hashes the utf8 bytes",
			input: "剧情树",
			want:  "e058561ec0191aa81adbae944fc0760e8bfdbaefdf925cba7e38c30750ffdf23",
		},
		{
			name:  "nested JSON object produced by JSON.stringify",
			input: `{"outer":{"inner":"值","list":[1,"two",null]}}`,
			want:  "b46edcee548004d3ee3156aa80e0b8878bef907f4a301e9c991e9d7a796a9898",
		},
		{
			name:  "a prompt fact string",
			input: "A clerk investigates a ledger that vanished from a sealed safe.",
			want:  "3e7bad2813974a4476b60368456e52403172f4f78fa9fde41482ee6cae8b9907",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := TextHash(testCase.input); got != testCase.want {
				t.Fatalf("TextHash(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
		})
	}
}

// JSONBase and JSONSample pin the struct rules EncodeJSON needs: declaration
// order, json tags, omitempty for TypeScript `undefined`, and inlining of an
// untagged embedded struct.
type JSONBase struct {
	A string `json:"a"`
}

type JSONSample struct {
	JSONBase
	ID   int      `json:"id"`
	Name string   `json:"name"`
	Note *string  `json:"note,omitempty"`
	Tags []string `json:"tags"`
}

func TestEncodeJSON(t *testing.T) {
	empty := ""
	note := "kept even when empty"
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "object members keep insertion order and are never sorted",
			value: Object{{Key: "b", Value: 1}, {Key: "a", Value: "x"}},
			want:  `{"b":1,"a":"x"}`,
		},
		{
			name: "nested object and array",
			value: Object{{Key: "outer", Value: Object{
				{Key: "inner", Value: "值"},
				{Key: "list", Value: []any{1, "two", nil}},
			}}},
			want: `{"outer":{"inner":"值","list":[1,"two",null]}}`,
		},
		{
			name:  "string escapes match QuoteJSONString",
			value: "a\"b\\c\nd\te\x01f",
			want:  `"a\"b\\c\nd\te\u0001f"`,
		},
		{
			name:  "non-ASCII and line separators are emitted verbatim",
			value: "账本\u2028x",
			want:  "\"账本\u2028x\"",
		},
		{
			name:  "nil interface",
			value: nil,
			want:  "null",
		},
		{
			name:  "nil slice is an empty array, never null",
			value: []any(nil),
			want:  "[]",
		},
		{
			name:  "NaN and infinities follow JSON.stringify and become null",
			value: []any{math.NaN(), math.Inf(1), math.Inf(-1)},
			want:  "[null,null,null]",
		},
		{
			name:  "numbers use the ECMAScript Number::toString algorithm",
			value: []any{100, 1.5, 1e21, 1e-7, 1e-6, math.Copysign(0, -1), 123.456},
			want:  "[100,1.5,1e+21,1e-7,0.000001,0,123.456]",
		},
		{
			name:  "struct tags decide names and omitempty models undefined",
			value: JSONSample{ID: 7, Name: "n"},
			want:  `{"a":"","id":7,"name":"n","tags":[]}`,
		},
		{
			name:  "a present empty optional value is still emitted",
			value: JSONSample{JSONBase: JSONBase{A: "1"}, ID: 7, Name: "n", Note: &empty, Tags: []string{}},
			want:  `{"a":"1","id":7,"name":"n","note":"","tags":[]}`,
		},
		{
			name: "pointer means present",
			value: JSONSample{
				JSONBase: JSONBase{A: "1"},
				ID:       7,
				Name:     "n",
				Note:     &note,
				Tags:     []string{"alpha", "beta"},
			},
			want: `{"a":"1","id":7,"name":"n","note":"kept even when empty","tags":["alpha","beta"]}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := EncodeJSON(testCase.value)
			if err != nil {
				t.Fatalf("EncodeJSON returned %v", err)
			}
			if got != testCase.want {
				t.Fatalf("EncodeJSON mismatch\n got: %q\nwant: %q", got, testCase.want)
			}
		})
	}
}

// TestEncodeJSONSourceReferenceOrder pins the de-duplication key format that
// plot-tree-generation-pure.ts:188,240 rely on: JSON.stringify of the
// branch-specific source literal.
func TestEncodeJSONSourceReferenceOrder(t *testing.T) {
	chapter := 1
	draftID := 41
	planID := 7
	eventID := 11
	cases := []struct {
		name      string
		reference PlotTreeSourceReference
		want      string
	}{
		{
			name:      "blueprint",
			reference: PlotTreeSourceReference{Type: PlotTreeReferenceBlueprint, ChapterNumber: &chapter},
			want:      `{"type":"blueprint","chapterNumber":1}`,
		},
		{
			name: "finalized chapter",
			reference: PlotTreeSourceReference{
				Type:          PlotTreeReferenceFinalizedChapter,
				DraftID:       &draftID,
				ChapterNumber: &chapter,
			},
			want: `{"type":"finalized-chapter","draftId":41,"chapterNumber":1}`,
		},
		{
			name:      "narrative thread plan",
			reference: PlotTreeSourceReference{Type: PlotTreeReferenceNarrativeThread, PlanID: &planID},
			want:      `{"type":"narrative-thread","planId":7}`,
		},
		{
			name: "narrative thread event",
			reference: PlotTreeSourceReference{
				Type:          PlotTreeReferenceNarrativeThread,
				PlanID:        &planID,
				EventID:       &eventID,
				ChapterNumber: &chapter,
			},
			want: `{"type":"narrative-thread","planId":7,"eventId":11,"chapterNumber":1}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := EncodeJSON(testCase.reference)
			if err != nil {
				t.Fatalf("EncodeJSON returned %v", err)
			}
			if got != testCase.want {
				t.Fatalf("EncodeJSON mismatch\n got: %q\nwant: %q", got, testCase.want)
			}
		})
	}
}

func TestEncodeJSONRejectsMaps(t *testing.T) {
	if _, err := EncodeJSON(map[string]any{"a": 1}); !errors.Is(err, ErrUnsupportedJSONValue) {
		t.Fatalf("expected ErrUnsupportedJSONValue, got %v", err)
	}
}

func TestTextHashOfEncodedJSON(t *testing.T) {
	encoded, err := EncodeJSON(Object{{Key: "outer", Value: Object{
		{Key: "inner", Value: "值"},
		{Key: "list", Value: []any{1, "two", nil}},
	}}})
	if err != nil {
		t.Fatalf("EncodeJSON returned %v", err)
	}
	if encoded != `{"outer":{"inner":"值","list":[1,"two",null]}}` {
		t.Fatalf("unexpected encoding %q", encoded)
	}
	if got, want := TextHash(encoded), "b46edcee548004d3ee3156aa80e0b8878bef907f4a301e9c991e9d7a796a9898"; got != want {
		t.Fatalf("TextHash(EncodeJSON(...)) = %q, want %q", got, want)
	}
}
