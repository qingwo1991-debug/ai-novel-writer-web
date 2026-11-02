package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestParseDependenciesValidationParity(t *testing.T) {
	hash := strings.Repeat("a", 64)
	base := fmt.Sprintf(`{"draftId":1,"contentHash":%q}`, hash)
	cases := []struct {
		name, value string
		valid       bool
	}{
		{"empty", `[]`, true},
		{"legacy-candidate", `[` + base + `]`, true},
		{"candidate", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":"candidate"}]`, hash), true},
		{"number-js-semantics", fmt.Sprintf(`[{"draftId":1e0,"contentHash":%q}]`, hash), true},
		{"sealed", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":"finalized","chapterNumber":2,"finalizationId":" receipt "}]`, hash), true},
		{"legacy-sealed", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":"legacy-finalized","chapterNumber":2}]`, hash), true},
		{"ignored-extra-key", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"extra":true}]`, hash), true},
		{"invalid-json", `not-json`, false},
		{"null-array", `null`, false},
		{"object", `{}`, false},
		{"null-element", `[null]`, false},
		{"array-element", `[[]]`, false},
		{"numeric-element", `[1]`, false},
		{"string-id", fmt.Sprintf(`[{"draftId":"1","contentHash":%q}]`, hash), false},
		{"zero-id", fmt.Sprintf(`[{"draftId":0,"contentHash":%q}]`, hash), false},
		{"fraction-id", fmt.Sprintf(`[{"draftId":1.5,"contentHash":%q}]`, hash), false},
		{"unsafe-id", fmt.Sprintf(`[{"draftId":9007199254740992,"contentHash":%q}]`, hash), false},
		{"null-id", fmt.Sprintf(`[{"draftId":null,"contentHash":%q}]`, hash), false},
		{"null-hash", `[{"draftId":1,"contentHash":null}]`, false},
		{"uppercase-hash", fmt.Sprintf(`[{"draftId":1,"contentHash":%q}]`, strings.Repeat("A", 64)), false},
		{"short-hash", `[{"draftId":1,"contentHash":"a"}]`, false},
		{"duplicate-id", `[` + base + `,` + base + `]`, false},
		{"null-kind", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":null}]`, hash), false},
		{"unknown-kind", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":"other"}]`, hash), false},
		{"candidate-chapter", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"chapterNumber":1}]`, hash), false},
		{"candidate-null-chapter", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"chapterNumber":null}]`, hash), false},
		{"candidate-receipt", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"finalizationId":null}]`, hash), false},
		{"legacy-receipt", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":"legacy-finalized","chapterNumber":1,"finalizationId":"id"}]`, hash), false},
		{"sealed-missing-chapter", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":"finalized","finalizationId":"id"}]`, hash), false},
		{"sealed-missing-receipt", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":"finalized","chapterNumber":1}]`, hash), false},
		{"sealed-blank-receipt", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":"finalized","chapterNumber":1,"finalizationId":"\ufeff "}]`, hash), false},
		{"sealed-long-utf16-receipt", fmt.Sprintf(`[{"draftId":1,"contentHash":%q,"kind":"finalized","chapterNumber":1,"finalizationId":%q}]`, hash, strings.Repeat("😀", 251)), false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			dependencies, valid := parseDependencies(sql.NullString{String: test.value, Valid: true})
			if valid != test.valid || dependencies == nil || !valid && len(dependencies) != 0 {
				t.Fatalf("parseDependencies(%s) = %+v, %v; want valid=%v", test.value, dependencies, valid, test.valid)
			}
		})
	}
	if dependencies, valid := parseDependencies(sql.NullString{}); valid || dependencies == nil || len(dependencies) != 0 {
		t.Fatalf("NULL metadata must fail closed: %+v, %v", dependencies, valid)
	}
}

func TestParseDependenciesOrderedAndSizeBounded(t *testing.T) {
	items := make([]string, 501)
	for i := range items {
		items[i] = fmt.Sprintf(`{"draftId":%d,"contentHash":%q}`, 501-i, strings.Repeat("b", 64))
	}
	dependencies, valid := parseDependencies(sql.NullString{String: "[" + strings.Join(items[:500], ",") + "]", Valid: true})
	if !valid || len(dependencies) != 500 || dependencies[0].DraftID != 501 || dependencies[499].DraftID != 2 {
		t.Fatalf("500 ordered dependencies: len=%d valid=%v", len(dependencies), valid)
	}
	dependencies, valid = parseDependencies(sql.NullString{String: "[" + strings.Join(items, ",") + "]", Valid: true})
	if valid || len(dependencies) != 0 {
		t.Fatalf("501 dependencies must fail closed: len=%d valid=%v", len(dependencies), valid)
	}
}
