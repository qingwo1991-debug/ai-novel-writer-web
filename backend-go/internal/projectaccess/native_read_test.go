//go:build lance_native && cgo

package projectaccess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNativeKnowledgeImmutableReadCaptureSurvivesCleanup(t *testing.T) {
	c := nativeAccessFixture(t)
	before := nativeSourceManifest(t, c)
	read, report, err := InspectKnowledge(context.Background(), c)
	if err != nil || read == nil || read.View() == nil || read.Search() == nil || report.Knowledge == nil {
		t.Fatal(report, err)
	}
	if !reflect.DeepEqual(before, nativeSourceManifest(t, c)) {
		t.Fatal("original source changed")
	}
	accessClean(t, c)
	search := read.Search()
	beforeSearch, err := search.SearchFTS(context.Background(), "", 5, nil, nil)
	if err != nil || len(beforeSearch) == 0 {
		t.Fatal("missing admitted search projection", err)
	}
	view := read.View()
	documents, err := view.Documents(context.Background())
	if err != nil || len(documents) != 3 {
		t.Fatal(documents, err)
	}
	stats, err := view.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 3 || stats["documentCount"] != 3 || stats["totalChunks"] != 4 || stats["vectorDimension"] != 2 {
		t.Fatal("unexpected public stats", stats)
	}
	missing, err := view.VectorlessCount(context.Background())
	if err != nil || missing["count"] != 0 {
		t.Fatal(missing, err)
	}
	for _, document := range documents {
		copy, err := view.ReadCopy(context.Background(), document["id"].(string))
		if err != nil || copy["available"] != true || copy["indexStatus"] != "current" {
			t.Fatal(copy, err)
		}
	}
	chapters := read.FinalizedChapters()
	chapters["injected"] = 999
	if _, exists := read.FinalizedChapters()["injected"]; exists {
		t.Fatal("borrowed mutable finalization map")
	} // Changing the source AFTER proof/unlock must not mutate already captured state.
	if err := os.RemoveAll(filepath.Join(c.ProjectPath, ".ai-novel/knowledge-copies")); err != nil {
		t.Fatal(err)
	}
	first, err := view.ReadCopy(context.Background(), documents[0]["id"].(string))
	if err != nil || first["available"] != true {
		t.Fatal("view retained deleted scratch/source dependency", err)
	}
	afterSearch, err := search.SearchFTS(context.Background(), "", 5, nil, nil)
	if err != nil || !reflect.DeepEqual(beforeSearch, afterSearch) {
		t.Fatal("search retained deleted source dependency", err)
	}
}
func TestNativeKnowledgeReadCaptureZeroOnFailureAndOrdinaryOptInRequired(t *testing.T) {
	c := nativeAccessFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	read, report, err := InspectKnowledge(ctx, c)
	if !errors.Is(err, context.Canceled) || read != nil || !reflect.DeepEqual(report, Report{}) {
		t.Fatal(read, report, err)
	}
	accessClean(t, c)
	c.Knowledge.Native = false
	read, report, err = InspectKnowledge(context.Background(), c)
	if !errors.Is(err, ErrConfiguration) || read != nil || !reflect.DeepEqual(report, Report{}) {
		t.Fatal(read, report, err)
	}
	accessClean(t, c)
}
