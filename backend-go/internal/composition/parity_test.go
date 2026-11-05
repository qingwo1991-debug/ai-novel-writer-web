package composition

import (
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
)

// The lane now carries two ports of the same reference algorithm: this package
// composes the visible continuation when a receipt is stored, and
// internal/generation composes it when a draft is committed. A receipt stored by
// one and re-derived by the other must hash identically, or the browser would be
// told a composition is intact by the writer and invalid by the reader. Rather
// than trust that two hand-written ports agree, these tests pin the agreement:
// if either side drifts, the build fails here instead of at an author's expense.
func TestComposeVisibleMatchesTheDraftPort(t *testing.T) {
	texts := [][2]string{
		{"", ""},
		{"", "第一章"},
		{"第一章", ""},
		{"第一章的内容。", "第二章的内容。"},
		{"  尾部空白  \n\n", "\n  新段落  "},
		{"甲", "乙"},
		{"a\r\nb", "c\rd"},
		{"前文\u0085后文", "\uFEFF新内容"},
		{"重复重复重复重复重复", "重复重复重复重复重复"},
		{"😀表情😀", "😀更多😀"},
		{"\t制表符\t", "　全角空格　"},
	}
	for _, pair := range texts {
		existing, addition := pair[0], pair[1]
		got := ComposeVisible(existing, addition)
		want := generation.ComposeVisibleContinuation(existing, addition)
		if got != want {
			t.Fatalf("ComposeVisible(%q, %q) = %q, but the draft port answers %q",
				existing, addition, got, want)
		}
	}
}

func TestTextHashMatchesTheDraftPort(t *testing.T) {
	for _, text := range []string{
		"", " ", "\n", "第一章", "混合 mixed 文本。", "😀", "\u0085", "\uFEFF",
		"尾随空格   ", "a\r\nb", "第一章的内容。第二章的内容。",
	} {
		if got, want := TextHash(text), generation.TextHash(text); got != want {
			t.Fatalf("TextHash(%q) = %q, but the draft port answers %q", text, got, want)
		}
	}
}
