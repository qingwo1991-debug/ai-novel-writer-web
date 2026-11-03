package importpreview

import (
	"errors"
	"strings"
	"testing"
)

var smallLimits = Limits{MaxBytes: 4096, MaxChapters: 3}

func TestTextPreviewMatchesLegacyHeadingAndPrefaceRules(t *testing.T) {
	chapters, err := ParseText("book.md", "序言\n丢弃标题而保留正文\n第十章：雨夜\nCafé 中 🙂 12。\n## Chapter 11 - Dawn\nsecond line", smallLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 3 {
		t.Fatalf("chapters=%#v", chapters)
	}
	if chapters[0].Number != 1 || chapters[0].Title != "序言" || chapters[0].Content != "丢弃标题而保留正文" {
		t.Fatalf("preface=%#v", chapters[0])
	}
	if chapters[1].Number != 10 || chapters[1].Title != "雨夜" || chapters[1].Content != "Café 中 🙂 12。" || chapters[1].WordCount != 4 {
		t.Fatalf("Chinese heading=%#v", chapters[1])
	}
	if chapters[2].Number != 11 || chapters[2].Title != "Dawn" || chapters[2].Content != "second line" || chapters[2].WordCount != 2 {
		t.Fatalf("Markdown heading=%#v", chapters[2])
	}
}

func TestTextPreviewNoHeadingUsesDisplayBasename(t *testing.T) {
	chapters, err := ParseText("draft 第三章.txt", "  雨落长安。\nCafé e\u0301 🙂 42  ", smallLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 1 || chapters[0].Number != 3 || chapters[0].Title != "draft 第三章" || chapters[0].Content != "雨落长安。\nCafé e\u0301 🙂 42" || chapters[0].WordCount != 8 {
		t.Fatalf("preview=%#v", chapters)
	}
}

func TestTextPreviewTitleOnlyAndAutoNumber(t *testing.T) {
	chapters, err := ParseText("book.txt", "第1章\n第2章\n正文\n第零章\n后文", smallLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 2 || chapters[0].Number != 2 || chapters[0].Title != "第2章" || chapters[1].Number != 3 || chapters[1].Title != "第零章" {
		t.Fatalf("chapters=%#v", chapters)
	}
}

func TestTextPreviewBoundariesAndUnsupportedFormat(t *testing.T) {
	for _, tc := range []struct {
		name, file, content string
		limits              Limits
		want                error
	}{
		{"epub", "book.epub", "abc", smallLimits, ErrUnsupported},
		{"path", "../book.txt", "abc", smallLimits, ErrInput},
		{"backslash", `C:\\book.txt`, "abc", smallLimits, ErrInput},
		{"invalid utf8", "book.txt", string([]byte{0xff}), smallLimits, ErrInput},
		{"bytes", "book.txt", "12345", Limits{MaxBytes: 4, MaxChapters: 3}, ErrSourceBytes},
		{"chapters", "book.txt", "第1章\na\n第2章\nb", Limits{MaxBytes: 4096, MaxChapters: 1}, ErrChapterCount},
		{"limit", "book.txt", "a", Limits{MaxBytes: MaxSourceBytes + 1, MaxChapters: 1}, ErrInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseText(tc.file, tc.content, tc.limits)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	chapters, err := ParseText("empty.text", " \n\t ", smallLimits)
	if err != nil || len(chapters) != 0 {
		t.Fatalf("empty got %#v, %v", chapters, err)
	}
	if CountDraftUnits(strings.Repeat("雨", 10)+"🙂 4") != 11 {
		t.Fatal("visible unit count")
	}
}
