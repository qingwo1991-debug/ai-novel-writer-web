//go:build linux

package main

import (
	"strings"
	"testing"
)

// TestSyntheticFinalizationReadChoice pins the finalization slot comparison on
// its own, with no database and no handler: given the rows whose frozen manifest
// names one slot key, which of them does that key's committed values reproduce?
//
// The three answers this rule can give are all data-safety answers, so each is
// asserted on its own: the reference raises the foreign-owner conflict before it
// proves any context, it raises the unprovable-context refusal for a row whose
// committed slot does not reproduce the key, and only then does it answer. The
// request key is derived here the way the original derives it - the hash of the
// JSON array of the slot's five primitives - rather than through the lane's own
// helper, so a wrong derivation cannot pass by agreeing with itself.
func TestSyntheticFinalizationReadChoice(t *testing.T) {
	const project = "finalization-choice-project"
	const foreign = "finalization-choice-foreign"
	contentHash := strings.Repeat("d", 64)
	slot := func(finalizationID string) map[string]any {
		return map[string]any{
			"source": map[string]any{
				"draftId":        float64(7),
				"chapterNumber":  float64(3),
				"finalizationId": finalizationID,
				"contentHash":    contentHash,
			},
			"stepKey": "chapter_notes",
		}
	}
	key := sha256Hex(`[7,"choice-owned",3,"` + contentHash + `","chapter_notes"]`)
	owned := func(runID string) syntheticFinalizationReadRow {
		return syntheticFinalizationReadRow{RunID: runID, Owner: project, Committed: slot("choice-owned")}
	}
	rows := func(entries ...syntheticFinalizationReadRow) []syntheticFinalizationReadRow { return entries }
	cases := []struct {
		name      string
		rows      []syntheticFinalizationReadRow
		wantRun   string
		wantFound bool
		wantCode  string
	}{
		{"no row names the key", nil, "", false, ""},
		{"the first matching row wins", rows(owned("run-1"), owned("run-2")), "run-1", true, ""},
		{"a foreign owner is the original's conflict",
			rows(syntheticFinalizationReadRow{RunID: "run-1", Owner: foreign, Committed: slot("choice-owned")}),
			"", false, "GENERATION_FINALIZATION_SLOT_CONFLICT"},
		{"a foreign row fails an earlier match",
			rows(owned("run-1"), syntheticFinalizationReadRow{RunID: "run-2", Owner: foreign, Committed: slot("choice-owned")}),
			"", false, "GENERATION_FINALIZATION_SLOT_CONFLICT"},
		{"an absent committed slot cannot be proved",
			rows(syntheticFinalizationReadRow{RunID: "run-1", Owner: project}),
			"", false, "GENERATION_FINALIZATION_CONTEXT_INVALID"},
		{"a committed slot that reproduces another key cannot be proved",
			rows(syntheticFinalizationReadRow{RunID: "run-1", Owner: project, Committed: slot("choice-other")}),
			"", false, "GENERATION_FINALIZATION_CONTEXT_INVALID"},
		{"an unprovable row fails an earlier match",
			rows(owned("run-1"), syntheticFinalizationReadRow{RunID: "run-2", Owner: project}),
			"", false, "GENERATION_FINALIZATION_CONTEXT_INVALID"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			matched, found, err := syntheticFinalizationReadChoice(testCase.rows, project, key)
			if testCase.wantCode != "" {
				if err == nil || err.Error() != testCase.wantCode {
					t.Fatalf("syntheticFinalizationReadChoice = %v, want %s", err, testCase.wantCode)
				}
				if found || matched.RunID != "" {
					t.Fatalf("syntheticFinalizationReadChoice answered %q while refusing", matched.RunID)
				}
				return
			}
			if err != nil {
				t.Fatalf("syntheticFinalizationReadChoice refused with %v, want an answer", err)
			}
			if found != testCase.wantFound || matched.RunID != testCase.wantRun {
				t.Fatalf("syntheticFinalizationReadChoice = %q/%v, want %q/%v",
					matched.RunID, found, testCase.wantRun, testCase.wantFound)
			}
		})
	}
}
