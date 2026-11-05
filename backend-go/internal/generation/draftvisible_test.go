package generation

// Every expectation in this file is transcribed by hand from the TypeScript and
// written out literally, so a drift shows up as a failing literal instead of as
// a self-consistent reimplementation of the algorithm under test. The cases are
// chorded against src/shared/visible-continuation.ts, src/shared/draft-visible-text.ts
// and src/shared/draft-units.ts; the seven continuation rows that carry a TS
// reference are the table pinned by
// src/services/workflows/__tests__/bounded-completion.test.ts:447-455.

import (
	"errors"
	"strings"
	"testing"
)

func TestComposeVisibleContinuation(t *testing.T) {
	rep := strings.Repeat
	cases := []struct {
		name     string
		existing string
		addition string
		want     string
	}{
		// bounded-completion.test.ts:447 - 47 non-whitespace characters is below the 48 character floor, so nothing is treated as an overlap.
		{name: "no overlap below the 48 character floor", existing: rep("甲", 47), addition: rep("甲", 47) + "乙", want: rep("甲", 47) + "\n\n" + rep("甲", 47) + "乙"},
		// bounded-completion.test.ts:448 - the overlap is measured with all whitespace removed, and the removal then skips the whitespace that followed the repeated run.
		{name: "overlap ignores whitespace and strips it after the repeat", existing: rep("甲", 48), addition: rep("甲 ", 48) + "\n  乙", want: rep("甲", 48) + "\n\n乙"},
		// bounded-completion.test.ts:449 - only the last 1600 code units are searched, so a repeated tail that fills the window is still found.
		{name: "overlap window is the last 1600 characters", existing: "开头" + rep("甲", 1600), addition: rep("甲", 1600) + "尾声", want: "开头" + rep("甲", 1600) + "\n\n尾声"},
		// bounded-completion.test.ts:450.
		{name: "empty existing trims the addition", existing: "", addition: "  雨夜\r\n", want: "雨夜"},
		// bounded-completion.test.ts:451 - a blank addition contributes nothing, not even the separator.
		{name: "empty addition keeps the existing text", existing: "雨夜", addition: "", want: "雨夜"},
		// bounded-completion.test.ts:452 - the whole addition is the overlap.
		{name: "identical inputs collapse to one copy", existing: rep("甲", 48), addition: rep("甲", 48), want: rep("甲", 48)},
		// bounded-completion.test.ts:453 - the original counts UTF-16 code units, so 24 astral characters are exactly the 48 character floor and the overlap lands on a character boundary.
		{name: "astral characters count as two UTF-16 code units", existing: rep("𠮷", 24), addition: rep("𠮷", 24) + "乙", want: rep("𠮷", 24) + "\n\n乙"},
		// The requested prefix shape: a short source that prefixes another is not an overlap, and both sides survive in order.
		{name: "a short prefix is not an overlap", existing: "雨夜来信", addition: "雨夜来信之后的故事", want: "雨夜来信\n\n雨夜来信之后的故事"},
		// The overlap may itself contain whitespace in the addition.
		{name: "overlap may span a newline inside the addition", existing: rep("甲", 48), addition: rep("甲", 24) + "\n" + rep("甲", 24) + "乙", want: rep("甲", 48) + "\n\n乙"},
		{name: "overlap absorbs a space inside the addition", existing: rep("甲", 50), addition: rep("甲", 20) + " " + rep("甲", 30) + "乙", want: rep("甲", 50) + "\n\n乙"},
		// U+0085 is not in the JavaScript \s set even though unicode.IsSpace says it is, so it survives trimming and blocks nothing.
		{name: "U+0085 is not JavaScript whitespace", existing: "\u0085雨夜", addition: "来信", want: "\u0085雨夜\n\n来信"},
		// U+FEFF is in the JavaScript \s set even though unicode.IsSpace says it is not.
		{name: "U+FEFF is JavaScript whitespace", existing: "\ufeff雨夜\ufeff", addition: "\ufeff来信", want: "雨夜\n\n来信"},
		{name: "no-break space is JavaScript whitespace", existing: "\u00a0甲\u00a0", addition: "乙", want: "甲\n\n乙"},
		{name: "both sides blank produce no separator", existing: "", addition: "   ", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ComposeVisibleContinuation(c.existing, c.addition); got != c.want {
				t.Fatalf("ComposeVisibleContinuation(%q, %q) = %q, want %q", c.existing, c.addition, got, c.want)
			}
		})
	}
}

func TestStripDraftThinkingTags(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		// draft-visible-text.ts:12 - the empty string is returned untouched.
		{name: "empty stays empty", text: "", want: ""},
		{name: "whitespace only is trimmed", text: "  \n ", want: ""},
		{name: "paired tag", text: "<think>隐藏</think>雨夜", want: "雨夜"},
		{name: "tags are case insensitive", text: "<THINK>隐藏</Think>雨夜", want: "雨夜"},
		// draft-visible-text.ts:14 - the losing alternative is end of text, so an unclosed tag swallows the rest of the artifact.
		{name: "unclosed tag swallows the remainder", text: "正文开头。<think>未闭合推理", want: "正文开头。"},
		// The [\s\S] body spans newlines.
		{name: "paired tag spans newlines", text: "<think>第一步\n第二步</think>正文。", want: "正文。"},
		// draft-visible-text.ts:25 - a reasoning prefix makes the orphan-close prefix hidden.
		{name: "reasoning prefix before an orphan close tag is dropped", text: "推理过程：隐藏步骤。</think>雨夜来信。", want: "雨夜来信。"},
		{name: "english reasoning prefix is dropped", text: "reasoning: hidden steps</think>雨夜来信。", want: "雨夜来信。"},
		// draft-visible-text.ts:26-29 - ordinary prose is retained; only the malformed tag is removed.
		{name: "ordinary prose before an orphan close tag is kept", text: "正文第一段。</think>雨夜来信。", want: "正文第一段。雨夜来信。"},
		{name: "the decision is text anchored, not line anchored", text: "正文第一段。\n推理第二段。</think>雨夜来信。", want: "正文第一段。\n推理第二段。雨夜来信。"},
		// draft-visible-text.ts:26 - a structured suffix makes the prefix hidden.
		{name: "structured suffix after an orphan close tag", text: "隐藏片段。</think>\n\n{\"chapters\":[]}", want: "{\"chapters\":[]}"},
		{name: "fenced json suffix after an orphan close tag", text: "隐藏片段。</think>\n```json\n{\"chapters\":[]}", want: "```json\n{\"chapters\":[]}"},
		// The \s of the reasoning prefix is the JavaScript set, which includes no-break space.
		{name: "no-break space before a reasoning prefix", text: "\u00a0推理：隐藏</think>雨夜来信", want: "雨夜来信"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripDraftThinkingTags(c.text); got != c.want {
				t.Fatalf("StripDraftThinkingTags(%q) = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

func TestSanitizeDraftText(t *testing.T) {
	rep := strings.Repeat
	cases := []struct {
		name       string
		text       string
		version    string
		useDefault bool
		want       string
	}{
		{name: "empty stays empty", text: "", version: DraftVisibleTextVersion, want: ""},
		{name: "trims outer whitespace", text: "  \n雨夜来信\n  ", version: DraftVisibleTextVersion, want: "雨夜来信"},
		// draft-visible-text.ts:36 - three or more newlines collapse to one blank line.
		{name: "collapses runs of newlines", text: "第一段。\n\n\n\n第二段。", version: DraftVisibleTextVersion, want: "第一段。\n\n第二段。"},
		// A single newline is not a paragraph break.
		{name: "keeps a single newline inside a paragraph", text: "第一行\n第二行\n\n新段。", version: DraftVisibleTextVersion, want: "第一行\n第二行\n\n新段。"},
		// draft-visible-text.ts:39 - a break is \n, any whitespace, \n, so a carriage return rides along inside the break.
		{name: "CRLF paragraph break", text: "甲\r\n\r\n乙", version: DraftVisibleTextVersion, want: "甲\n\n乙"},
		{name: "a whitespace only line is a paragraph break", text: "甲\n   \n乙", version: DraftVisibleTextVersion, want: "甲\n\n乙"},
		{name: "trims every paragraph", text: "  甲  \n\n\t乙\t", version: DraftVisibleTextVersion, want: "甲\n\n乙"},
		// draft-visible-text.ts:35 - the trailing \s* is greedy, so the matched line may take the newline after it and still satisfy the multiline end anchor.
		{name: "drops a UI continue prompt at the head", text: "点我继续生成后续内容\n\n林岚推开办公室的门。", version: DraftVisibleTextVersion, want: "林岚推开办公室的门。"},
		{name: "drops a UI continue prompt as its own line", text: "林岚推开办公室的门。\n\n点我继续生成后续内容\n\n屏幕仍在闪烁。", version: DraftVisibleTextVersion, want: "林岚推开办公室的门。\n\n屏幕仍在闪烁。"},
		{name: "drops a trailing unfinished marker line", text: "正文最后一段。\n未完待续", version: DraftVisibleTextVersion, want: "正文最后一段。"},
		{name: "drops a click prompt and keeps the paragraphs apart", text: "第一段。\n请点击继续\n第二段。", version: DraftVisibleTextVersion, want: "第一段。\n\n第二段。"},
		// Transcribed from generate-draft.command.test.ts:113-121.
		{name: "removes thinking residue and continue prompts together", text: "<think>分析过程</think>\n\n点我继续生成后续内容\n\n林岚推开办公室的门，屏幕上的航班编号仍在闪烁。", version: DraftVisibleTextVersion, want: "林岚推开办公室的门，屏幕上的航班编号仍在闪烁。"},
		// Transcribed from generate-draft.command.test.ts:124-127.
		{name: "removes a long malformed thinking prefix", text: "推理过程：" + rep("隐藏步骤。", 61) + "</think>\n\n林岚推开办公室的门。", version: DraftVisibleTextVersion, want: "林岚推开办公室的门。"},
		{name: "thinking only leaves nothing", text: "<think>只有推理</think>", version: DraftVisibleTextVersion, want: ""},
		{name: "keeps non-ASCII and emoji prose", text: "雨夜来信 😀\n\n第二段 ǅ", version: DraftVisibleTextVersion, want: "雨夜来信 😀\n\n第二段 ǅ"},
		// v1 only: draft-visible-text.ts:42-48 dedupes paragraphs whose whitespace stripped key is at least 40 UTF-16 code units.
		{name: "v1 keeps the first of two identical long paragraphs", text: rep("甲", 40) + "\n\n" + rep("甲", 40), version: DraftVisibleTextVersionV1, want: rep("甲", 40)},
		{name: "v1 keeps short repetitions", text: rep("甲", 39) + "\n\n" + rep("甲", 39), version: DraftVisibleTextVersionV1, want: rep("甲", 39) + "\n\n" + rep("甲", 39)},
		{name: "v2 keeps repeated paragraphs", text: rep("甲", 40) + "\n\n" + rep("甲", 40), version: DraftVisibleTextVersion, want: rep("甲", 40) + "\n\n" + rep("甲", 40)},
		// The duplicate key is the paragraph with all whitespace removed, so a line break inside one copy does not save it.
		{name: "v1 compares paragraphs without whitespace", text: rep("乙", 20) + "\n" + rep("乙", 20) + "\n\n" + rep("乙", 40), version: DraftVisibleTextVersionV1, want: rep("乙", 20) + "\n" + rep("乙", 20)},
		// key.length is a UTF-16 code unit count: 20 astral characters reach the 40 unit threshold.
		{name: "v1 measures the duplicate key in UTF-16 code units", text: rep("𠮷", 20) + "\n\n" + rep("𠮷", 20), version: DraftVisibleTextVersionV1, want: rep("𠮷", 20)},
		// Any version other than the exact v1 literal takes the v2 path, including the empty one (draft-visible-text.ts:40).
		{name: "an unknown version takes the v2 path", text: rep("甲", 40) + "\n\n" + rep("甲", 40), version: "", want: rep("甲", 40) + "\n\n" + rep("甲", 40)},
		// The default argument is DraftVisibleTextVersion.
		{name: "defaults to v2", text: rep("甲", 40) + "\n\n" + rep("甲", 40), useDefault: true, want: rep("甲", 40) + "\n\n" + rep("甲", 40)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizeDraftText(c.text, c.version)
			if c.useDefault {
				got = SanitizeDraftText(c.text)
			}
			if got != c.want {
				t.Fatalf("SanitizeDraftText(%q, %q) = %q, want %q", c.text, c.version, got, c.want)
			}
		})
	}
}

func TestComposeDraftVisibleContinuation(t *testing.T) {
	rep := strings.Repeat
	refrain := "林舟守在海港等待潮汐。"
	cases := []struct {
		name     string
		existing string
		addition string
		version  string
		want     string
	}{
		{name: "empty existing", existing: "", addition: "雨夜", version: DraftVisibleTextVersion, want: "雨夜"},
		{name: "empty addition", existing: "雨夜", addition: "", version: DraftVisibleTextVersion, want: "雨夜"},
		{name: "two fresh sources join in order", existing: "雨夜", addition: "来信", version: DraftVisibleTextVersion, want: "雨夜\n\n来信"},
		// Transcribed from generate-draft.command.test.ts:52-62: both sides are sanitised first, then composed, then sanitised again.
		{name: "sanitises both sides before composing", existing: "<think>内部推理</think>\n" + rep(refrain, 12) + "\n\n未完待续", addition: rep(refrain, 8) + "\n\n钟楼终于亮起灯。", version: DraftVisibleTextVersion, want: rep(refrain, 12) + "\n\n钟楼终于亮起灯。"},
		// The final sanitise pass runs over the whole projection, so v1 dedupe sees a duplicate that only exists across the join.
		{name: "v1 dedupes across the join", existing: rep("甲", 40), addition: rep("甲", 40), version: DraftVisibleTextVersionV1, want: rep("甲", 40)},
		{name: "v2 keeps the join across identical paragraphs", existing: rep("甲", 40), addition: rep("甲", 40), version: DraftVisibleTextVersion, want: rep("甲", 40) + "\n\n" + rep("甲", 40)},
		{name: "nothing visible on either side", existing: "<think>推理</think>", addition: "\n\n", version: DraftVisibleTextVersion, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ComposeDraftVisibleContinuation(c.existing, c.addition, c.version); got != c.want {
				t.Fatalf("ComposeDraftVisibleContinuation(%q, %q) = %q, want %q", c.existing, c.addition, got, c.want)
			}
		})
	}
}

func TestComposeDraftVisibleSources(t *testing.T) {
	rep := strings.Repeat
	refrain := "林舟守在海港等待潮汐。"

	text, hash := ComposeDraftVisibleSources([]string{"雨夜", "来信", "今夜"})
	if want := "雨夜\n\n来信\n\n今夜"; text != want {
		t.Fatalf("sources text = %q, want %q", text, want)
	}
	if want := TextHash("雨夜\n\n来信\n\n今夜"); hash != want {
		t.Fatalf("sources hash = %q, want %q", hash, want)
	}

	// Ordering is part of the value, so a different order is a different text and a different hash.
	reordered, reorderedHash := ComposeDraftVisibleSources([]string{"今夜", "来信", "雨夜"})
	if want := "今夜\n\n来信\n\n雨夜"; reordered != want {
		t.Fatalf("reordered text = %q, want %q", reordered, want)
	}
	if reordered == text || reorderedHash == hash {
		t.Fatalf("reordering sources must change text and hash: %q %q", reordered, reorderedHash)
	}

	// The first source is only sanitised; later ones are composed onto it.
	first, firstHash := ComposeDraftVisibleSources([]string{"<think>推理</think>雨夜\n\n未完待续", "来信"})
	if want := "雨夜\n\n来信"; first != want {
		t.Fatalf("single source text = %q, want %q", first, want)
	}
	if want := TextHash("雨夜\n\n来信"); firstHash != want {
		t.Fatalf("single source hash = %q, want %q", firstHash, want)
	}

	// A repeated continuation is trimmed by the overlap rule before hashing.
	repeated, repeatedHash := ComposeDraftVisibleSources([]string{rep(refrain, 12), rep(refrain, 8) + "\n\n钟楼终于亮起灯。"})
	if want := rep(refrain, 12) + "\n\n钟楼终于亮起灯。"; repeated != want {
		t.Fatalf("repeated sources text = %q, want %q", repeated, want)
	}
	if want := TextHash(rep(refrain, 12) + "\n\n钟楼终于亮起灯。"); repeatedHash != want {
		t.Fatalf("repeated sources hash = %q, want %q", repeatedHash, want)
	}

	// No sources project to the empty text and the empty digest.
	empty, emptyHash := ComposeDraftVisibleSources(nil)
	if empty != "" {
		t.Fatalf("empty sources text = %q, want %q", empty, "")
	}
	if want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"; emptyHash != want {
		t.Fatalf("empty sources hash = %q, want %q", emptyHash, want)
	}
}

func TestDraftVisibleConstants(t *testing.T) {
	if DraftVisibleTextVersion != "draft-visible-v2" || DraftVisibleTextVersionV1 != "draft-visible-v1" {
		t.Fatalf("versions = %q %q, want %q %q", DraftVisibleTextVersion, DraftVisibleTextVersionV1, "draft-visible-v2", "draft-visible-v1")
	}
	if VisibleContinuationVersion != "visible-append-v1" {
		t.Fatalf("VisibleContinuationVersion = %q, want %q", VisibleContinuationVersion, "visible-append-v1")
	}
	if ContinuationVisibleTailChars != 1600 {
		t.Fatalf("ContinuationVisibleTailChars = %d, want 1600", ContinuationVisibleTailChars)
	}
	if DraftCondensePurpose != "chapter-draft-condense" {
		t.Fatalf("DraftCondensePurpose = %q, want %q", DraftCondensePurpose, "chapter-draft-condense")
	}
	if DraftUnitAlgorithmVersion != 3 {
		t.Fatalf("DraftUnitAlgorithmVersion = %d, want 3", DraftUnitAlgorithmVersion)
	}
}

func TestIsDraftVisibleTextVersion(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{value: "draft-visible-v1", want: true},
		{value: "draft-visible-v2", want: true},
		{value: "visible-append-v1", want: false},
		{value: "", want: false},
	}
	for _, c := range cases {
		if got := IsDraftVisibleTextVersion(c.value); got != c.want {
			t.Fatalf("IsDraftVisibleTextVersion(%q) = %t, want %t", c.value, got, c.want)
		}
	}
}

func TestCountDraftUnits(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		// Transcribed from src/shared/__tests__/draft-units.test.ts:16.
		{name: "mixed Han and English", text: "林岚 walked into the room.", want: 6},
		// draft-units.test.ts:20.
		{name: "accented Latin words stay whole", text: "Café naïve résumé déjà vu", want: 5},
		// draft-units.test.ts:21 - NFC first, so a combining acute is not a stray character.
		{name: "decomposed Latin word normalises to one word", text: "Cafe\u0301", want: 1},
		// draft-units.test.ts:25 - \p{Script=Han} matches one code point at a time.
		{name: "non-BMP Han counts per code point", text: "𠀀𠮷", want: 2},
		// draft-units.test.ts:29.
		{name: "digits keep the per-code-point count", text: "2026", want: 4},
		// draft-units.test.ts:33.
		{name: "punctuation whitespace and emoji are not prose", text: "... ！😀 🚀", want: 0},
		{name: "CJK punctuation is not prose", text: "雨夜，来信。", want: 4},
		// draft-units.ts:59 - Han is replaced by a space, not deleted, so it separates the word beside it.
		{name: "Han separates an adjacent Latin word", text: "林岚walked", want: 3},
		// Transcribed from generate-draft.command.test.ts:135.
		{name: "line breaks separate words", text: "林岚\n\n 推门", want: 4},
		// draft-units.ts:61-64 spreads the remainder into code points, so an astral digit counts once here even though the legacy counter counts two.
		{name: "astral non-letter code point counts once", text: "𝟎", want: 1},
		// draft-units.ts:19 - the \s member is the JavaScript class, which includes U+00A0.
		{name: "no-break space is whitespace", text: "雨夜\u00a0来信", want: 4},
		{name: "U+FEFF is whitespace", text: "雨夜\ufeff来信", want: 4},
		// U+0085 is not in the JavaScript \s set, so it is a visible code point.
		{name: "U+0085 is not whitespace", text: "雨夜\u0085来信", want: 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CountDraftUnits(c.text); got != c.want {
				t.Fatalf("CountDraftUnits(%q) = %d, want %d", c.text, got, c.want)
			}
		})
	}

	// The single entry cache must be keyed on the text: two texts of the same length alternating still count on their own (draft-units.test.ts:36-38).
	texts := []string{"林岚推门", "....", "林岚推门", "...."}
	wants := []int{4, 0, 4, 0}
	for index, text := range texts {
		if got := CountDraftUnits(text); got != wants[index] {
			t.Fatalf("CountDraftUnits(%q) on pass %d = %d, want %d", text, index, got, wants[index])
		}
	}
}

func TestCountLegacyDraftUnitsV1(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		// draft-units.test.ts:41 - the v0.9.0 counter splits "Café" into "Caf" plus one stray character.
		{name: "splits accented Latin", text: "Café", want: 2},
		// Every accented word splits: "Caf", "na", "ve" plus the two stray characters, where CountDraftUnits reads two words.
		{name: "splits every accented word", text: "Café naïve", want: 5},
		{name: "agrees on plain English", text: "walked into the room.", want: 4},
		// The legacy other-character arm counts UTF-16 code units, so an astral digit counts twice where the current counter counts once.
		{name: "counts astral code points twice", text: "𝟎", want: 2},
		// The legacy counter drops punctuation after Han the same way the current one does.
		{name: "drops punctuation after Han", text: "雨夜，", want: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CountLegacyDraftUnitsV1(c.text); got != c.want {
				t.Fatalf("CountLegacyDraftUnitsV1(%q) = %d, want %d", c.text, got, c.want)
			}
		})
	}
}

func TestDraftTargetUnitRange(t *testing.T) {
	cases := []struct {
		target  int64
		minimum int64
		maximum int64
	}{
		// draft-units.test.ts:46.
		{target: 2000, minimum: 1400, maximum: 2600},
		{target: 100, minimum: 70, maximum: 130},
		// The floor of a small product can be zero; only the input is validated.
		{target: 1, minimum: 0, maximum: 2},
	}
	for _, c := range cases {
		got, err := DraftTargetUnitRange(c.target)
		if err != nil {
			t.Fatalf("DraftTargetUnitRange(%d) error: %v", c.target, err)
		}
		if want := (DraftUnitRange{Minimum: c.minimum, Maximum: c.maximum}); got != want {
			t.Fatalf("DraftTargetUnitRange(%d) = %+v, want %+v", c.target, got, want)
		}
	}
	for _, target := range []int64{0, -1} {
		if _, err := DraftTargetUnitRange(target); !errors.Is(err, ErrDraftTargetUnitsInvalid) {
			t.Fatalf("DraftTargetUnitRange(%d) error = %v, want %v", target, err, ErrDraftTargetUnitsInvalid)
		}
	}
}

func TestParseDraftTargetUnits(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int64
		ok   bool
	}{
		{name: "plain integer", text: "2000", want: 2000, ok: true},
		{name: "surrounding whitespace", text: "  80  ", want: 80, ok: true},
		{name: "no-break space padding", text: "\u00a080\u00a0", want: 80, ok: true},
		{name: "exponent notation", text: "2e3", want: 2000, ok: true},
		{name: "hexadecimal notation", text: "0x10", want: 16, ok: true},
		{name: "binary notation", text: "0b101", want: 5, ok: true},
		{name: "octal notation", text: "0o17", want: 15, ok: true},
		{name: "trailing decimal point", text: "80.", want: 80, ok: true},
		{name: "largest safe integer", text: "9007199254740991", want: 9007199254740991, ok: true},
		// Number('') is 0 and 0 is a safe integer, so the coercion SUCCEEDS with
		// zero; usability is a separate question answered by
		// DraftTargetUnitsUsable, exactly as draft-units.ts:8 keeps the range check
		// apart from the coercion.
		{name: "empty text coerces to zero", text: "", want: 0, ok: true},
		{name: "blank text coerces to zero", text: "   ", want: 0, ok: true},
		{name: "fraction is not an integer", text: "80.5", want: 0, ok: false},
		{name: "non-numeric text", text: "八十", want: 0, ok: false},
		{name: "infinity", text: "Infinity", want: 0, ok: false},
		{name: "beyond the safe integer range", text: "9007199254740993", want: 0, ok: false},
		// A radix prefix cannot carry a sign.
		{name: "signed hexadecimal", text: "-0x10", want: 0, ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseDraftTargetUnits(c.text)
			if ok != c.ok || got != c.want {
				t.Fatalf("ParseDraftTargetUnits(%q) = (%d, %t), want (%d, %t)", c.text, got, ok, c.want, c.ok)
			}
		})
	}

	// The reference gate is `!Number.isSafeInteger(target) || target < 1`.
	for _, target := range []int64{0, -3} {
		if DraftTargetUnitsUsable(target) {
			t.Fatalf("DraftTargetUnitsUsable(%d) = true, want false", target)
		}
	}
	for _, target := range []int64{1, 2000, 9007199254740991} {
		if !DraftTargetUnitsUsable(target) {
			t.Fatalf("DraftTargetUnitsUsable(%d) = false, want true", target)
		}
	}
}

func TestTextHash(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		// SHA-256 of the empty byte string.
		{name: "empty", text: "", want: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{name: "ascii", text: "abc", want: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		// SHA-256 over the UTF-8 bytes of four Han characters, 12 bytes.
		{name: "non-ASCII UTF-8 bytes", text: "雨夜来信", want: "43273bff66890c4cd32847b444a31b727962f1e5e2074f96bee1f50f84e3840a"},
		// The composed projection "雨夜" plus the separator plus "来信".
		{name: "composed projection", text: "雨夜\n\n来信", want: "b89cd0da22f146f5cb18a9a053102415e152f6ca6a3133c16777d60649219f58"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TextHash(c.text); got != c.want {
				t.Fatalf("TextHash(%q) = %q, want %q", c.text, got, c.want)
			}
		})
	}

	// What is hashed is the canonicalised projection, not the raw artifact bytes.
	composed := ComposeDraftVisibleContinuation("  雨夜  ", "<think>推理</think>来信")
	if composed != "雨夜\n\n来信" {
		t.Fatalf("composed = %q, want %q", composed, "雨夜\n\n来信")
	}
	raw := "  雨夜  <think>推理</think>来信"
	if got, want := TextHash(composed), TextHash("雨夜\n\n来信"); got != want {
		t.Fatalf("hash of composed = %q, want %q", got, want)
	}
	if got := TextHash(composed); got != "b89cd0da22f146f5cb18a9a053102415e152f6ca6a3133c16777d60649219f58" {
		t.Fatalf("hash of composed = %q, want the literal digest", got)
	}
	if TextHash(composed) == TextHash(raw) {
		t.Fatal("the hash must be taken after canonicalisation")
	}
}
