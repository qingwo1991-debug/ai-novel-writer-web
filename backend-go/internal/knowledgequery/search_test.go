package knowledgequery

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestSearchFTSRankScopeDedupeAndCapture(t *testing.T) {
	snapshot, root, _ := queryFixture(t)
	snapshot.Tables[0].Records[0]["filePath"] = "/unmanaged/source.txt"
	snapshot.Tables[1].Schema = append(snapshot.Tables[1].Schema, queryField("chapterNumber", "int32"))
	rows := snapshot.Tables[1].Records
	rows[0]["text"] = "alpha beta"
	rows[0]["chapterNumber"] = 1
	rows[1]["text"] = "beta alpha"
	rows[1]["chapterNumber"] = 2
	// Repeated text with a distinct chunk id is admitted, but only one search
	// result may survive the original (fileName,text) deduplication.
	rows[1]["text"] = "alpha beta"
	rows[1]["chapterNumber"] = 1
	capture, err := NewSearchCapture(context.Background(), snapshot, root, queryBudget())
	if err != nil {
		t.Fatal(err)
	}
	rows[0]["text"] = "mutated after capture"
	result, err := capture.SearchFTS(context.Background(), "alpha beta", 2, nil, nil)
	if err != nil || !reflect.DeepEqual(result, []SearchResult{{Text: "alpha beta", Score: 0.5, FileName: "source.txt"}}) {
		t.Fatal(result, err)
	}
	result[0].Text = "caller mutation"
	result, err = capture.SearchFTS(context.Background(), "alpha", 2, &ChapterScope{From: 2, To: 2}, nil)
	if err != nil || len(result) != 0 {
		t.Fatal(result, err)
	}
	result, err = capture.SearchFTS(context.Background(), "alpha", 2, nil, []string{"reference"})
	if err != nil || len(result) != 0 {
		t.Fatal(result, err)
	}
}

func TestSearchFTSWeightedRankAndBounds(t *testing.T) {
	snapshot, root, _ := queryFixture(t)
	snapshot.Tables[0].Records[0]["filePath"] = "/unmanaged/source.txt"
	snapshot.Tables[1].Records[0]["text"] = "beta"
	snapshot.Tables[1].Records[1]["text"] = "alpha beta"
	capture, err := NewSearchCapture(context.Background(), snapshot, root, queryBudget())
	if err != nil {
		t.Fatal(err)
	}
	got, err := capture.SearchFTS(context.Background(), "alpha beta", 1, nil, nil)
	if err != nil || len(got) != 1 || got[0].Text != "alpha beta" {
		t.Fatal(got, err)
	}
	for _, request := range []struct {
		query string
		topK  int
		scope *ChapterScope
	}{{query: "x", topK: -1}, {query: "x", topK: 101}, {query: "x", topK: 1, scope: &ChapterScope{From: 3, To: 2}}} {
		if _, err := capture.SearchFTS(context.Background(), request.query, request.topK, request.scope, nil); !errors.Is(err, ErrInput) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := capture.SearchFTS(ctx, "alpha", 1, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
