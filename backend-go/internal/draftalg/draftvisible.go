package draftalg

// Ports of src/shared/visible-continuation.ts and src/shared/draft-visible-text.ts:
// the visible continuation append and the draft projection built on top of it,
// plus the SHA-256 the composition receipt stores.
//
// Every function is pure. The original operates on JavaScript strings, so the
// continuation arithmetic runs over UTF-16 code units (jsUnits/jsText) exactly
// where the original's `.length`, `.slice`, `text[index]` and `String#trim` do;
// the sanitizer runs over Go strings with the JavaScript \s class spelled out.
//
// Order of operations, and therefore of the hashed bytes, is the reference's:
// each artifact is sanitised, then composed onto the running text in ascending
// artifact ordinal order, then sanitised again as a whole
// (electron/repositories/generation-run-repository.ts:493-495), and only that
// final text is hashed (:512).

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf16"
)

const (
	// DraftVisibleTextVersionV1 is the legacy projection that an existing receipt
	// must keep reproducing (draft-visible-text.ts:4).
	DraftVisibleTextVersionV1 = "draft-visible-v1"
	// DraftVisibleTextVersion is DRAFT_VISIBLE_TEXT_VERSION
	// (draft-visible-text.ts:3).
	DraftVisibleTextVersion = "draft-visible-v2"
	// DraftCondensePurpose is DRAFT_CONDENSE_PURPOSE (draft-visible-text.ts:9).
	DraftCondensePurpose = "chapter-draft-condense"
	// VisibleContinuationVersion is VISIBLE_CONTINUATION_VERSION
	// (visible-continuation.ts:1).
	VisibleContinuationVersion = "visible-append-v1"
	// ContinuationVisibleTailChars is CONTINUATION_VISIBLE_TAIL_CHARS
	// (visible-continuation.ts:2): the window, in code units, that the overlap
	// search looks at.
	ContinuationVisibleTailChars = 1600
	// minVisibleOverlapChars is MIN_VISIBLE_OVERLAP_CHARS
	// (visible-continuation.ts:3): overlaps shorter than this are not trimmed.
	minVisibleOverlapChars = 48
)

// jsSpaceClassBody is the body of a JavaScript \s character class: the
// WhiteSpace and LineTerminator productions. Go's \s is ASCII-only and would
// silently keep U+00A0, U+3000 and the like, so every pattern below spells the
// set out. Keep this in step with isJavaScriptSpaceRune.
const jsSpaceClassBody = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// jsSpaceClass is the same set as a bracketed class.
const jsSpaceClass = "[" + jsSpaceClassBody + "]"

var (
	// /<think>[\s\S]*?(?:<\/think>|$)/gi (draft-visible-text.ts:14). Dot-all
	// stands in for [\s\S]; the lazy body stops at the first closing tag, and $
	// without multiline is end of text, so an unclosed tag swallows the rest.
	thinkingPairPattern = regexp.MustCompile(`(?is)<think>.*?(?:</think>|$)`)
	// /<\/?think>/gi (draft-visible-text.ts:17, :30).
	thinkingTagPattern = regexp.MustCompile(`(?i)</?think>`)
	// /<\/think>/i (draft-visible-text.ts:15): the first orphan closing tag.
	thinkingClosePattern = regexp.MustCompile(`(?i)</think>`)
	// /^\s*(?:思考|推理|分析|reasoning|analysis)/iu
	// (draft-visible-text.ts:25).
	reasoningPrefixPattern = regexp.MustCompile(`(?i)^` + jsSpaceClass + `*(?:思考|推理|分析|reasoning|analysis)`)
	// /^\s*(?:```(?:json)?\s*)?[{[]/iu (draft-visible-text.ts:26). The class
	// [{[] in the original holds `{` and `[` only: the trailing `]` closes it.
	structuredSuffixPattern = regexp.MustCompile("(?i)^" + jsSpaceClass + "*(?:```(?:json)?" + jsSpaceClass + "*)?(?:\\{|\\[)")
	// /^\s*(?:点我继续生成后续内容|继续生成后续内容|请点击继续|未完待续)\s*$/gmi
	// (draft-visible-text.ts:35). Trailing \s* is greedy, so a matched line may
	// consume the newline after it and still satisfy the multiline $.
	draftUIPromptPattern = regexp.MustCompile(`(?mi)^` + jsSpaceClass + `*(?:点我继续生成后续内容|继续生成后续内容|请点击继续|未完待续)` + jsSpaceClass + `*$`)
	// /\n{3,}/g (draft-visible-text.ts:36).
	tripleNewlinePattern = regexp.MustCompile(`\n{3,}`)
	// /\n\s*\n/ (draft-visible-text.ts:39): a paragraph break may contain any
	// whitespace between the two newlines, carriage returns included.
	paragraphBreakPattern = regexp.MustCompile(`\n` + jsSpaceClass + `*\n`)
	// /\s+/g (draft-visible-text.ts:44).
	spaceRunPattern = regexp.MustCompile(jsSpaceClass + `+`)
)

// IsDraftVisibleTextVersion is isDraftVisibleTextVersion
// (draft-visible-text.ts:5-7).
func IsDraftVisibleTextVersion(value string) bool {
	return value == DraftVisibleTextVersionV1 || value == DraftVisibleTextVersion
}

// StripDraftThinkingTags is stripDraftThinkingTags (draft-visible-text.ts:11-31).
func StripDraftThinkingTags(text string) string {
	if text == "" {
		return text
	}
	withoutPairedThinking := thinkingPairPattern.ReplaceAllString(text, "")
	orphanClosingTag := thinkingClosePattern.FindStringIndex(withoutPairedThinking)
	if orphanClosingTag == nil {
		return jsTrimText(thinkingTagPattern.ReplaceAllString(withoutPairedThinking, ""))
	}

	hiddenPrefix := withoutPairedThinking[:orphanClosingTag[0]]
	visibleSuffix := withoutPairedThinking[orphanClosingTag[1]:]
	// A missing opening tag is only safe to treat as hidden reasoning when its
	// prefix identifies itself as reasoning, or when it precedes structured
	// output. Otherwise retain ordinary prose and remove only the malformed tag.
	looksLikeReasoning := reasoningPrefixPattern.MatchString(hiddenPrefix)
	hasStructuredVisibleSuffix := structuredSuffixPattern.MatchString(visibleSuffix)
	cleaned := hiddenPrefix + visibleSuffix
	if looksLikeReasoning || hasStructuredVisibleSuffix {
		cleaned = visibleSuffix
	}
	return jsTrimText(thinkingTagPattern.ReplaceAllString(cleaned, ""))
}

// SanitizeDraftText is sanitizeDraftText (draft-visible-text.ts:33-49). version
// defaults to DraftVisibleTextVersion, like the original's default parameter; as
// in the original, only the exact v1 string selects the legacy projection, so
// any other value - including "" - takes the v2 path.
func SanitizeDraftText(text string, version ...string) string {
	algorithm := draftVisibleVersion(version)
	cleaned := StripDraftThinkingTags(text)
	cleaned = draftUIPromptPattern.ReplaceAllString(cleaned, "")
	cleaned = tripleNewlinePattern.ReplaceAllString(cleaned, "\n\n")
	cleaned = jsTrimText(cleaned)

	parts := paragraphBreakPattern.Split(cleaned, -1)
	paragraphs := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := jsTrimText(part); trimmed != "" {
			paragraphs = append(paragraphs, trimmed)
		}
	}
	if algorithm != DraftVisibleTextVersionV1 {
		return strings.Join(paragraphs, "\n\n")
	}
	// Existing v1 receipts must retain their exact projection and hash on
	// recovery, so the duplicate-paragraph filter stays exactly as it was.
	seen := make(map[string]struct{}, len(paragraphs))
	kept := make([]string, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		key := spaceRunPattern.ReplaceAllString(paragraph, "")
		// `key.length` is a UTF-16 code-unit count: 20 astral characters reach
		// the 40-unit threshold and 39 astral characters do not.
		if utf16Length(key) >= 40 {
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
		}
		kept = append(kept, paragraph)
	}
	return strings.Join(kept, "\n\n")
}

// ComposeDraftVisibleContinuation is composeDraftVisibleContinuation
// (draft-visible-text.ts:52-55): a pure projection of independent raw-visible
// artifacts, each sanitised before it is composed.
func ComposeDraftVisibleContinuation(existing, addition string, version ...string) string {
	algorithm := draftVisibleVersion(version)
	return SanitizeDraftText(
		ComposeVisibleContinuation(SanitizeDraftText(existing, algorithm), SanitizeDraftText(addition, algorithm)),
		algorithm)
}

// ComposeDraftVisibleSources folds already-settled artifact texts in the order
// the composition receipt lists them (ascending artifact ordinal,
// electron/repositories/generation-run-repository.ts:468-512): the first source
// is only sanitised, every later one is composed onto the running text, and the
// hash is taken over the final text. Ordering is part of the value: the same
// sources in another order compose and hash differently.
func ComposeDraftVisibleSources(texts []string, version ...string) (string, string) {
	algorithm := draftVisibleVersion(version)
	text := ""
	for index, raw := range texts {
		if index == 0 {
			text = SanitizeDraftText(raw, algorithm)
			continue
		}
		text = ComposeDraftVisibleContinuation(text, raw, algorithm)
	}
	return text, TextHash(text)
}

// TextHash is textHash (electron/repositories/generation-run-repository.ts:92,
// src/shared/source-ref.ts:23): the SHA-256 of the UTF-8 bytes of the text, as
// lower-case hex. It is always taken over the canonicalised projection, never
// over the raw artifact bytes.
func TextHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// draftVisibleVersion resolves the optional version argument.
func draftVisibleVersion(version []string) string {
	if len(version) == 0 {
		return DraftVisibleTextVersion
	}
	return version[0]
}

// ComposeVisibleContinuation is composeVisibleContinuation
// (visible-continuation.ts:28-33): append the visible addition to the visible
// existing text, dropping the leading part of the addition that repeats the
// existing tail. Inputs are already visible text; no source bytes are modified.
func ComposeVisibleContinuation(existing, addition string) string {
	visibleExisting := jsTrimUnits(jsUnits(existing))
	visibleAddition := jsTrimUnits(jsUnits(addition))
	overlap := overlappingVisiblePrefixLength(visibleExisting, visibleAddition)
	nextVisibleText := removeLeadingNonWhitespace(visibleAddition, overlap)

	// [visibleExisting, newVisibleText].filter(Boolean).join('\n\n').trim()
	parts := make([][]uint16, 0, 2)
	if len(visibleExisting) > 0 {
		parts = append(parts, visibleExisting)
	}
	if len(nextVisibleText) > 0 {
		parts = append(parts, nextVisibleText)
	}
	return jsText(jsTrimUnits(joinVisibleUnits(parts)))
}

// overlappingVisiblePrefixLength is overlappingVisiblePrefixLength
// (visible-continuation.ts:15-24): the longest run of characters, ignoring all
// whitespace, that ends the existing tail and starts the addition, searched from
// the full window down to the 48-character floor. Both sides are capped at
// ContinuationVisibleTailChars code units, so a repeated tail longer than the
// window is still found.
func overlappingVisiblePrefixLength(existing, addition []uint16) int {
	tail := stripWhitespaceUnits(tailUnits(existing, ContinuationVisibleTailChars))
	head := stripWhitespaceUnits(headUnits(addition, ContinuationVisibleTailChars))
	maximum := len(tail)
	if len(head) < maximum {
		maximum = len(head)
	}
	for length := maximum; length >= minVisibleOverlapChars; length-- {
		if equalUnits(tail[len(tail)-length:], head[:length]) {
			return length
		}
	}
	return 0
}

// removeLeadingNonWhitespace is removeLeadingNonWhitespaceCharacters
// (visible-continuation.ts:5-13). count is a code-unit count because the
// original increments it per text[index], so one astral character consumes two.
func removeLeadingNonWhitespace(text []uint16, count int) []uint16 {
	if count <= 0 {
		return text
	}
	consumed := 0
	for index := 0; index < len(text); index++ {
		if !isJavaScriptSpaceUnit(text[index]) {
			consumed++
		}
		if consumed >= count {
			return trimStartUnits(text[index+1:])
		}
	}
	return nil
}

// jsUnits re-encodes a Go string as the UTF-16 code units the original indexes.
func jsUnits(text string) []uint16 {
	return utf16.Encode([]rune(text))
}

// jsText is the inverse. A window boundary can cut a surrogate pair; the unpaired
// half then decodes to U+FFFD, which is the same replacement the original's UTF-8
// encoding performs on a lone surrogate, so the hashed bytes still agree.
func jsText(units []uint16) string {
	return string(utf16.Decode(units))
}

// jsTrimUnits is String.prototype.trim over code units. Every whitespace code
// point is a single BMP code unit, so trimming units and trimming code points
// are the same operation.
func jsTrimUnits(units []uint16) []uint16 {
	end := len(units)
	for end > 0 && isJavaScriptSpaceUnit(units[end-1]) {
		end--
	}
	start := 0
	for start < end && isJavaScriptSpaceUnit(units[start]) {
		start++
	}
	return units[start:end]
}

// trimStartUnits is String.prototype.trimStart over code units.
func trimStartUnits(units []uint16) []uint16 {
	start := 0
	for start < len(units) && isJavaScriptSpaceUnit(units[start]) {
		start++
	}
	return units[start:]
}

// stripWhitespaceUnits is String.prototype.replace with /\s+/gu and "".
func stripWhitespaceUnits(units []uint16) []uint16 {
	stripped := make([]uint16, 0, len(units))
	for _, unit := range units {
		if !isJavaScriptSpaceUnit(unit) {
			stripped = append(stripped, unit)
		}
	}
	return stripped
}

// tailUnits is String.prototype.slice(-count).
func tailUnits(units []uint16, count int) []uint16 {
	if count <= 0 || len(units) == 0 {
		return nil
	}
	if count >= len(units) {
		return units
	}
	return units[len(units)-count:]
}

// headUnits is String.prototype.slice(0, count).
func headUnits(units []uint16, count int) []uint16 {
	if count <= 0 || len(units) == 0 {
		return nil
	}
	if count >= len(units) {
		return units
	}
	return units[:count]
}

// joinVisibleUnits is Array.prototype.join with the "\n\n" separator the original
// uses; empty parts are dropped by the caller.
func joinVisibleUnits(parts [][]uint16) []uint16 {
	separator := jsUnits("\n\n")
	total := 0
	for index, part := range parts {
		if index > 0 {
			total += len(separator)
		}
		total += len(part)
	}
	joined := make([]uint16, 0, total)
	for index, part := range parts {
		if index > 0 {
			joined = append(joined, separator...)
		}
		joined = append(joined, part...)
	}
	return joined
}

// equalUnits is string equality over code units.
func equalUnits(left, right []uint16) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// isJavaScriptSpaceUnit is the JavaScript \s class, code unit by code unit.
func isJavaScriptSpaceUnit(unit uint16) bool {
	switch unit {
	case 0x0009, 0x000a, 0x000b, 0x000c, 0x000d, 0x0020,
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return unit >= 0x2000 && unit <= 0x200a
}

// utf16Length is the value of `.length` on a JavaScript string: the number of
// UTF-16 code units, so an astral character counts two.
func utf16Length(text string) int {
	length := 0
	for _, r := range text {
		if r > 0xffff {
			length += 2
			continue
		}
		length++
	}
	return length
}
