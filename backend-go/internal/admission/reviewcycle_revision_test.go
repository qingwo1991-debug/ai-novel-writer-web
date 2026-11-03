package admission

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestM03ConfirmationSnapshotCanonicalAndFailClosed(t *testing.T) {
	valid := `{"kind":"human-confirmed-review","schemaVersion":2,"cycleId":" c ","sourceReviewId":7,"sourceDraft":{"id":3,"chapterNumber":1,"version":2,"status":"draft","content":"正文","ignored":"x"},"summary":"x","authorGuidance":"","items":[{"category":" 类 ","severity":" error ","description":" 描述 ","decision":"waive","origin":"ai","findingId":" f ","ignored":true}],"ignored":true}`
	snapshot, err := m03ParseConfirmationSnapshot(valid)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot["cycleId"] != "c" || snapshot["sourceReviewId"] != float64(7) {
		t.Fatalf("canonical snapshot binding: %#v", snapshot)
	}
	item := snapshot["items"].([]any)[0].(map[string]any)
	if item["category"] != "类" || item["findingId"] != "f" {
		t.Fatalf("canonical item: %#v", item)
	}
	if _, ok := item["ignored"]; ok {
		t.Fatal("unknown item field was retained")
	}
	for _, raw := range []string{
		strings.Replace(valid, `"schemaVersion":2`, `"schemaVersion":1`, 1),
		strings.Replace(valid, `"decision":"waive"`, `"decision":"bad"`, 1),
		strings.Replace(valid, `"origin":"ai"`, `"origin":"author"`, 1),
		strings.Replace(valid, `"content":"正文"`, `"content":"   "`, 1),
		strings.Replace(valid, `"sourceReviewId":7`, `"sourceReviewId":9007199254740992`, 1),
		strings.Replace(valid, `"description":" 描述 "`, `"description":"\ud800"`, 1),
	} {
		if _, err := m03ParseConfirmationSnapshot(raw); err == nil {
			t.Fatalf("invalid snapshot accepted: %s", raw)
		}
	}
}

func TestM03GoalReviewEvidenceUsesUTF16Offsets(t *testing.T) {
	goal := map[string]any{"version": float64(1), "chapterNumber": float64(1), "coverage": "complete",
		"items": []any{map[string]any{"id": "g", "text": "目标", "status": "completed", "description": "完成",
			"evidence": []any{map[string]any{"quote": "🙂", "start": float64(3), "end": float64(5)}}}}}
	if err := m03ValidateChapterGoalReview(goal); err != nil {
		t.Fatal(err)
	}
	goal["items"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)["end"] = float64(4)
	if err := m03ValidateChapterGoalReview(goal); err == nil {
		t.Fatal("rune offset passed as UTF-16 offset")
	}
}

func TestM03VisibleCompositionOverlapAndJSONOrder(t *testing.T) {
	repeated := strings.Repeat("a", 48)
	got, err := m03ComposeVisibleContinuation("start\n\n"+repeated, repeated+" next")
	if err != nil {
		t.Fatal(err)
	}
	want := "start\n\n" + repeated + "\n\nnext"
	if got != want {
		t.Fatalf("composition = %q, want %q", got, want)
	}
	if !m03SameRawJSON(json.RawMessage(`{"artifactId":"a","revision":1,"textHash":"h"}`),
		json.RawMessage(`{ "artifactId":"a", "revision":1,"textHash":"h" }`)) {
		t.Fatal("JSON whitespace changed property order")
	}
	if m03SameRawJSON(json.RawMessage(`{"artifactId":"a","revision":1}`),
		json.RawMessage(`{"revision":1,"artifactId":"a"}`)) {
		t.Fatal("JSON property order was lost")
	}
}

func TestM03VisibleCompositionConservativelyRejectsSplitEmojiBoundary(t *testing.T) {
	// The original TypeScript slice(0, 1600) keeps an isolated high
	// surrogate here and continues. Go cannot carry that intermediate
	// string through overlap matching without a UTF-16-specific port, so
	// this helper must reject the case instead of accepting a wrong hash.
	if _, err := m03UTF16Window(strings.Repeat("a", 1599)+"🙂", false, 1600); err == nil {
		t.Fatal("UTF-16 boundary bisected surrogate was admitted")
	}
	// A full emoji exactly before the boundary remains representable.
	if got, err := m03UTF16Window(strings.Repeat("a", 1598)+"🙂x", false, 1600); err != nil ||
		got != strings.Repeat("a", 1598)+"🙂" {
		t.Fatalf("complete UTF-16 pair rejected: %q, %v", got, err)
	}
}
