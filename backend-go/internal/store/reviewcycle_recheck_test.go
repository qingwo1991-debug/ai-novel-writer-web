package store

import (
	"errors"
	"testing"
)

func TestReviewCycleEvidenceChangeMatchesTypeScriptCases(t *testing.T) {
	anchored := rcRecheckFinding{Kind: "literary", HasSpan: true, SpanStart: 2, SpanEnd: 5, Occurrence: 1}
	if got := rcClassifyEvidenceChange("😀abc", "😀xyz", anchored); got != "changed" {
		t.Fatal(got)
	}
	if got := rcClassifyEvidenceChange("😀abc", "😀abc ", anchored); got != "unchanged" {
		t.Fatal(got)
	}
	if got := rcClassifyEvidenceChange("😀abc", "😀abc\u200b", anchored); got != "unchanged" {
		t.Fatal(got)
	}
	anchored.SpanStart, anchored.SpanEnd = 1, 2
	if got := rcClassifyEvidenceChange("😀abc", "xyz", anchored); got != "ambiguous" {
		t.Fatal(got)
	}
	anchored.SpanStart, anchored.SpanEnd, anchored.Occurrence = 2, 5, 2
	if got := rcClassifyEvidenceChange("😀abc", "xyz", anchored); got != "ambiguous" {
		t.Fatal(got)
	}
	objective := rcRecheckFinding{Kind: "objective", HasSpan: true, SpanStart: 0, SpanEnd: 3, Occurrence: 1}
	if got := rcClassifyEvidenceChange("A!B", "A B and more", objective); got != "unchanged" {
		t.Fatal(got)
	}
	if got := rcClassifyEvidenceChange("A!B", "C D", objective); got != "changed" {
		t.Fatal(got)
	}
	mustShow := rcRecheckFinding{MustShow: true}
	if got := rcClassifyEvidenceChange("A, B", "A B", mustShow); got != "unchanged" {
		t.Fatal(got)
	}
	if got := rcClassifyEvidenceChange("A B", "A B C", mustShow); got != "changed" {
		t.Fatal(got)
	}
}

func TestReviewCycleMustShowDispositionNeedsExplicitAppliedChoice(t *testing.T) {
	const report = `{"summary":"review","items":[{"category":"goal","severity":"unknown","description":"needs the event","goalId":"ch1:mustShow:1"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"complete","items":[{"id":"ch1:mustShow:1","text":"event","status":"unknown","description":"show it","evidence":[]}]}}`
	expected := "event\nshow it"
	row := rcRecheckRow{FindingID: "finding:one", Category: "goal", Kind: "objective", Status: "unverified", Problem: "needs the event", Expected: &expected}
	choice := rcConfirmationChoice{GoalID: "ch1:mustShow:1", FindingID: "finding:one", Origin: "ai", Decision: "apply"}
	got, err := rcPlanRecheckDisposition("old", "new", report, 0, nil, []rcRecheckRow{row}, []rcConfirmationChoice{choice})
	if err != nil || got != "required" {
		t.Fatalf("applied mustShow: %q %v", got, err)
	}
	got, err = rcPlanRecheckDisposition("old", "new", report, 0, nil, []rcRecheckRow{row}, nil)
	if err != nil || got != "not-required" {
		t.Fatalf("unconfirmed mustShow: %q %v", got, err)
	}
	got, err = rcPlanRecheckDisposition("old", "old!", report, 0, nil, []rcRecheckRow{row}, []rcConfirmationChoice{choice})
	if err != nil || got != "not-required" {
		t.Fatalf("punctuation-only mustShow: %q %v", got, err)
	}
	completedAttempt := "attempt-1"
	got, err = rcPlanRecheckDisposition("old", "new", report, 1, &completedAttempt, []rcRecheckRow{row}, []rcConfirmationChoice{choice})
	if err != nil || got != "completed" {
		t.Fatalf("completed: %q %v", got, err)
	}
	if _, err := rcPlanRecheckDisposition("old", "new", report, 1, nil, []rcRecheckRow{row}, []rcConfirmationChoice{choice}); !errors.Is(err, errReviewCycleRecheckEvidence) {
		t.Fatalf("completed without attempt accepted: %v", err)
	}
	attempt := "unexpected"
	if _, err := rcPlanRecheckDisposition("old", "new", report, 0, &attempt, []rcRecheckRow{row}, nil); !errors.Is(err, errReviewCycleRecheckEvidence) {
		t.Fatalf("accepted stray attempt: %v", err)
	}
}

func TestReviewCycleUnshownMustShowPreservesReportGoalOrder(t *testing.T) {
	const report = `{"summary":"review","items":[{"category":"goal","severity":"unknown","description":"last","goalId":"ch1:mustShow:2"},{"category":"goal","severity":"unknown","description":"first","goalId":"ch1:mustShow:1"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"complete","items":[{"id":"ch1:mustShow:2","text":"two","status":"unknown","description":"d2","evidence":[]},{"id":"ch1:mustShow:1","text":"one","status":"unknown","description":"d1","evidence":[]}]}}`
	wantTwo, wantOne := "two\nd2", "one\nd1"
	rows := []rcRecheckRow{{FindingID: "finding:2", Kind: "objective", Status: "unverified", Problem: "last", Expected: &wantTwo}, {FindingID: "finding:1", Kind: "objective", Status: "unverified", Problem: "first", Expected: &wantOne}}
	choices := []rcConfirmationChoice{{GoalID: "ch1:mustShow:1", FindingID: "finding:1", Origin: "ai", Decision: "apply"}, {GoalID: "ch1:mustShow:2", FindingID: "finding:2", Origin: "ai", Decision: "apply"}}
	got, err := rcUnshownMustShow(report, choices, rows)
	if err != nil || len(got) != 2 || got[0].FindingID != "finding:2" || got[1].FindingID != "finding:1" {
		t.Fatalf("goal order differs from report: %#v %v", got, err)
	}
}
