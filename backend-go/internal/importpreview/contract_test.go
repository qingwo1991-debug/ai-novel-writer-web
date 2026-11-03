package importpreview

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestOriginalTypeScriptTextImportContract(t *testing.T) {
	file := os.Getenv("GO_IMPORTPREVIEW_CONTRACT")
	if file == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original import preview oracle mandatory")
		}
		t.Skip("GitHub original import preview oracle required")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Format int `json:"format"`
		Cases  []struct {
			Name     string    `json:"name"`
			Chapters []Chapter `json:"chapters"`
			Error    string    `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &contract); err != nil || contract.Format != 1 || len(contract.Cases) != 8 {
		t.Fatalf("invalid import contract: %v", err)
	}
	inputs := []struct {
		name, content string
		limit         int
	}{
		{"standard", "第1章：雨夜\n雨落长安。\nChapter 2: Dawn\nCafé e\u0301 🙂 42", 5000},
		{"markdown", "## Chapter 11 - Dawn\nsecond line\n### 第十二章·归途\n第二段", 5000},
		{"preface", "序言\n丢弃标题而保留正文\n第十章：雨夜\nCafé 中 🙂 12。", 5000},
		{"title-only", "第1章\n第2章\n正文\n第零章\n后文", 5000},
		{"no-heading", "雨落长安。\nCafé e\u0301 🙂 42", 5000},
		{"unicode-space", "#\u00a0第3章—春夜\n风从海上来。", 5000},
		{"empty-heading", "Chapter 1\n正文", 5000},
		{"count-limit", "第1章\na\n第2章\nb", 1},
	}
	for i, input := range inputs {
		item := contract.Cases[i]
		if item.Name != input.name {
			t.Fatalf("oracle order mismatch: %q != %q", item.Name, input.name)
		}
		t.Run(input.name, func(t *testing.T) {
			actual, err := ParseText("book.txt", input.content, Limits{MaxBytes: 4096, MaxChapters: input.limit})
			if item.Error != "" {
				if item.Error == ErrChapterCount.Error() && errors.Is(err, ErrChapterCount) {
					return
				}
				t.Fatalf("got %v, want %q", err, item.Error)
			}
			if err != nil || !reflect.DeepEqual(actual, item.Chapters) {
				t.Fatalf("got %#v, %v; want %#v", actual, err, item.Chapters)
			}
		})
	}
}
