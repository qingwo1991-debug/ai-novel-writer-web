package store

import (
	"errors"
	"testing"
)

// Expected IDs were produced by the TypeScript finding() ordered JSON identity
// and node:crypto SHA-256. The emoji forces the original UTF-16 offset 2.
func TestReviewCycleSavedReportItemIndexMatchesTypeScript(t *testing.T) {
	const report = `{"summary":"审稿","items":[{"category":"目标","severity":"error","description":"目标未达成","quote":"目标","goalId":"ch1:mustShow:1"},{"category":"风格","severity":"warning","description":"重写段落","quote":"不存在"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"complete","items":[{"id":"ch1:mustShow:1","text":"展示目标","status":"unmet","description":"必须出现","evidence":[{"quote":"目标","start":2,"end":4}]}]}}`
	source := reviewCycleSource{"project-A", "epoch-A", "draft:7", 2, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	indices, err := rcItemIndex(report, "😀目标！", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(indices) != 2 {
		t.Fatalf("finding count = %d", len(indices))
	}
	if got := indices["finding:d486f3f8e124b0bfe1c8cf9ef5a8f560e82d0cff96b96d273d6e2b52a9752cc3"]; got != 0 {
		t.Fatalf("goal item index = %d", got)
	}
	if got := indices["finding:0e7a89fa3621add6d9efdbffde4fca2b3cfbd48f90ba500958e23890c098c477"]; got != 1 {
		t.Fatalf("free item index = %d", got)
	}
}

func TestReviewCycleSavedReportRejectsInvalidGoalsAndFields(t *testing.T) {
	for _, body := range []string{
		`{"summary":"x","items":[{"category":"a","severity":"error","description":"d","goalId":"g"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"complete","items":[]}}`,
		`{"summary":"x","items":[{"category":"a","severity":"pass","description":"d","goalId":"g"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"complete","items":[{"id":"g","text":"t","status":"unmet","description":"d","evidence":[{"quote":"😀","start":0,"end":1}]}]}}`,
		`{"summary":"x","items":[{"category":"a","severity":"warning","description":"d","stableFactKey":"fact"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"not_configured","items":[]}}`,
		`{"summary":"x","items":[{"category":"a","severity":"warning","description":"d","unknown":"field"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"not_configured","items":[]}}`,
	} {
		if _, _, err := rcParseSavedReport(body); !errors.Is(err, errReviewCycleSavedReport) {
			t.Fatalf("accepted invalid report: %s", body)
		}
	}
}

func TestReviewCycleOverlappingQuoteIsUnanchored(t *testing.T) {
	source := reviewCycleSource{"p", "e", "s", 1, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	item := reviewCycleSavedItem{Index: 0, Category: "style", Severity: "warning", Description: "d"}
	quote := "aa"
	item.Quote = &quote
	actual, err := rcFindingID(source, "aaa", item, nil)
	if err != nil {
		t.Fatal(err)
	}
	item.Quote = nil
	expected, err := rcFindingID(source, "aaa", item, nil)
	if err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("overlapping quote was treated as a unique anchor: %s != %s", actual, expected)
	}
}

func TestReviewCycleProjectionOrderingAndOptionalFields(t *testing.T) {
	target := "goal-1"
	rows := []reviewCycleProjectionRow{{FindingID: "finding:z", Category: "style", Kind: "literary", Status: "unverified"}, {FindingID: "finding:a", Category: "goal", Kind: "objective", Status: "unresolved", TargetID: &target}}
	got := rcProjectionDTO("cycle:1", 7, "not-generated", nil, 0, nil, rows, map[string]int{"finding:a": 3})
	if _, exists := got["mergedHash"]; exists {
		t.Fatal("unexpected merged hash")
	}
	if _, exists := got["recheckDisposition"]; exists {
		t.Fatal("unexpected recheck disposition")
	}
	findings := got["findings"].([]map[string]any)
	if findings[0]["findingId"] != "finding:a" || findings[0]["reviewItemIndex"] != 3 {
		t.Fatalf("ordered mapped finding: %#v", findings[0])
	}
	if findings[1]["findingId"] != "finding:z" || findings[1]["reviewItemIndex"] != nil {
		t.Fatalf("ordered unmapped finding: %#v", findings[1])
	}
	if _, exists := findings[1]["targetId"]; exists {
		t.Fatal("null targetId must be omitted")
	}
}

func TestReviewCycleRejectsEscapedLoneSurrogateBeforeJSONDecode(t *testing.T) {
	const report = `{"summary":"x","items":[{"category":"style","severity":"warning","description":"\ude00"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"not_configured","items":[]}}`
	if !rcLoneSurrogateEscape(report) {
		t.Fatal("escaped low surrogate was not detected")
	}
	if _, _, err := rcParseSavedReport(report); !errors.Is(err, errReviewCycleSavedReport) {
		t.Fatalf("surrogate was accepted: %v", err)
	}
	const paired = `{"summary":"x","items":[{"category":"style","severity":"warning","description":"\ud83d\ude00"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"not_configured","items":[]}}`
	if rcLoneSurrogateEscape(paired) {
		t.Fatal("valid surrogate pair rejected")
	}
	if _, _, err := rcParseSavedReport(paired); err != nil {
		t.Fatalf("valid pair failed: %v", err)
	}
	const literal = `{"summary":"x","items":[{"category":"style","severity":"warning","description":"\\ude00"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"not_configured","items":[]}}`
	if rcLoneSurrogateEscape(literal) {
		t.Fatal("literal backslash was interpreted as a JSON surrogate escape")
	}
}

func TestReviewCycleRejectsInvalidUTF8BeforeFindingHash(t *testing.T) {
	source := reviewCycleSource{"p", "e", "s", 1, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	item := reviewCycleSavedItem{Category: "style", Description: "d"}
	if _, err := rcFindingID(source, string([]byte{0xff}), item, nil); !errors.Is(err, errReviewCycleSavedReport) {
		t.Fatalf("invalid UTF-8 source accepted: %v", err)
	}
	if _, _, err := rcParseSavedReport(string([]byte{0xff})); !errors.Is(err, errReviewCycleSavedReport) {
		t.Fatalf("invalid UTF-8 JSON accepted: %v", err)
	}
}
