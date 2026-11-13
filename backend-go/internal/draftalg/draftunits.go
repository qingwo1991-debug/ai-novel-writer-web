package draftalg

// Port of src/shared/draft-units.ts: the persisted prose-count contract and the
// target-unit range the commit gate compares against. Everything here is a pure
// function of its arguments; the only shared state is the single-text count
// cache the original keeps at draft-units.ts:37-54, which cannot change a result.

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// DraftUnitAlgorithmVersion is DRAFT_UNIT_ALGORITHM_VERSION
// (draft-units.ts:5): the version of the counting algorithm persisted next to
// cached counts.
const DraftUnitAlgorithmVersion = 3

// ErrDraftTargetUnitsInvalid is the error draftTargetUnitRange throws
// (draft-units.ts:8).
var ErrDraftTargetUnitsInvalid = errors.New("DRAFT_TARGET_UNITS_INVALID")

// DraftUnitRange is the frozen `Readonly<{ minimum: number; maximum: number }>`
// draftTargetUnitRange returns (draft-units.ts:7-13).
type DraftUnitRange struct {
	Minimum int64 `json:"minimum"`
	Maximum int64 `json:"maximum"`
}

// DraftTargetUnitRange is draftTargetUnitRange (draft-units.ts:7-13).
//
// The multiplication is done in float64 on purpose. The original evaluates
// `Math.floor(targetUnits * 0.7)` and `Math.ceil(targetUnits * 1.3)` on a double,
// so on some targets the product lands a representable step away from the
// rational value; reproducing the double arithmetic keeps the same integer for
// every target instead of only for the ones that happen to round cleanly.
func DraftTargetUnitRange(targetUnits int64) (DraftUnitRange, error) {
	// `!Number.isSafeInteger(targetUnits) || targetUnits < 1` throws
	// DRAFT_TARGET_UNITS_INVALID; an int64 cannot be a non-integer, and the
	// non-safe-integer half of that guard is enforced by ParseDraftTargetUnits
	// before a value ever reaches this function.
	if targetUnits < 1 {
		return DraftUnitRange{}, ErrDraftTargetUnitsInvalid
	}
	return DraftUnitRange{
		Minimum: int64(math.Floor(float64(targetUnits) * 0.7)),
		Maximum: int64(math.Ceil(float64(targetUnits) * 1.3)),
	}, nil
}

// ParseDraftTargetUnits mirrors `Number(text)` over the `draft:target-units`
// author input followed by `Number.isSafeInteger(value)`
// (electron/services/generation-draft-effects.ts:169-173). ok is false for NaN,
// for a non-integral value, and for a magnitude beyond 2^53-1, exactly where the
// original's guard fails; the reference's separate `target < 1` clause is
// DraftTargetUnitsUsable.
func ParseDraftTargetUnits(text string) (int64, bool) {
	value := jsNumber(text)
	if !isSafeInteger(value) {
		return 0, false
	}
	return int64(value), true
}

// DraftTargetUnitsUsable is the `target < 1` clause of the reference gate
// (electron/services/generation-draft-effects.ts:169-173) in its positive form.
func DraftTargetUnitsUsable(targetUnits int64) bool {
	return targetUnits >= 1
}

// isSafeInteger is Number.isSafeInteger.
func isSafeInteger(value float64) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return false
	}
	return math.Abs(value) <= 9007199254740991
}

// jsDecimalNumberPattern is the StrDecimalLiteral grammar ToNumber accepts for a
// signed decimal string.
var jsDecimalNumberPattern = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// jsNumber is ToNumber applied to a string (StringNumericLiteral). It is not
// strconv.ParseFloat: the original also accepts hexadecimal, binary and octal
// literals, reads an empty or blank string as 0, and reads the signed
// "Infinity" literals, so that `Number(”)` and `Number('0x10')` behave as they
// do in the reference.
func jsNumber(text string) float64 {
	trimmed := jsTrimText(text)
	if trimmed == "" {
		return 0
	}
	// NonDecimalIntegerLiteral: no sign is allowed before the radix prefix.
	if len(trimmed) >= 2 && trimmed[0] == '0' {
		switch trimmed[1] {
		case 'x', 'X':
			return jsRadixNumber(trimmed[2:], 16)
		case 'o', 'O':
			return jsRadixNumber(trimmed[2:], 8)
		case 'b', 'B':
			return jsRadixNumber(trimmed[2:], 2)
		}
	}
	switch trimmed {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if !jsDecimalNumberPattern.MatchString(trimmed) {
		return math.NaN()
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return math.NaN()
	}
	// An out-of-range literal becomes +/-Inf under ParseFloat as it does under
	// Number(); isSafeInteger rejects either.
	return value
}

// jsRadixNumber accumulates a 0x/0o/0b digit run the way the original widens it
// to a double; a digit run that is empty or out of range for the radix is NaN.
func jsRadixNumber(digits string, radix int) float64 {
	if digits == "" {
		return math.NaN()
	}
	value := 0.0
	for _, r := range digits {
		var digit int
		switch {
		case r >= '0' && r <= '9':
			digit = int(r - '0')
		case r >= 'a' && r <= 'f':
			digit = int(r-'a') + 10
		case r >= 'A' && r <= 'F':
			digit = int(r-'A') + 10
		default:
			return math.NaN()
		}
		if digit >= radix {
			return math.NaN()
		}
		value = value*float64(radix) + float64(digit)
	}
	return value
}

var (
	// LEGACY_CHINESE_CHARACTER_PATTERN (draft-units.ts:15) is BMP-only on
	// purpose: the v0.9.0 counter never recognised astral Han.
	legacyChineseCharacterPattern = regexp.MustCompile(`[\x{3400}-\x{4DBF}\x{4E00}-\x{9FFF}\x{F900}-\x{FAFF}]`)
	// LEGACY_ENGLISH_WORD_PATTERN (draft-units.ts:16).
	legacyEnglishWordPattern = regexp.MustCompile(`[A-Za-z]+(?:['’][A-Za-z]+)*`)
	// HAN_CHARACTER_PATTERN (draft-units.ts:17) is \p{Script=Han}.
	hanCharacterPattern = regexp.MustCompile(`\p{Han}`)
	// UNICODE_WORD_PATTERN (draft-units.ts:18): a letter, then letters and
	// combining marks, then any number of apostrophe-joined segments. Both
	// apostrophes are literal members: U+0027 and U+2019.
	unicodeWordPattern = regexp.MustCompile(`\p{L}[\p{L}\p{M}]*(?:['’]\p{L}[\p{L}\p{M}]*)*`)
	// WHITESPACE_OR_PUNCTUATION_OR_SYMBOL_PATTERN (draft-units.ts:19). The
	// whitespace member is the JavaScript \s set, not Go's ASCII-only \s.
	whitespacePunctuationSymbolPattern = regexp.MustCompile("[" + jsSpaceClassBody + `\p{P}\p{S}]`)
)

// CountLegacyDraftUnitsV1 is countLegacyDraftUnitsV1 (draft-units.ts:25-34): the
// v0.9.0 counter, retained only so an existing receipt replays byte for byte. It
// is not the metric new writes use.
func CountLegacyDraftUnitsV1(text string) int {
	englishWords := len(legacyEnglishWordPattern.FindAllStringIndex(text, -1))
	withoutEnglishWords := legacyEnglishWordPattern.ReplaceAllString(text, "")
	chineseCharacters := len(legacyChineseCharacterPattern.FindAllStringIndex(withoutEnglishWords, -1))
	otherCharacters := legacyChineseCharacterPattern.ReplaceAllString(withoutEnglishWords, "")
	otherCharacters = whitespacePunctuationSymbolPattern.ReplaceAllString(otherCharacters, "")
	// `.length` on a string is a UTF-16 code-unit count, so this arm counts two
	// for one astral character even though the current counter counts one.
	return chineseCharacters + englishWords + utf16Length(otherCharacters)
}

// CountDraftUnits is countDraftUnits (draft-units.ts:49-54): the visible prose
// unit shared by chapter targets, storage and UI.
//
//   - Every Han code point counts as one, including non-BMP extensions.
//   - Words in any Unicode script count as one.
//   - Digits and other non-word visible code points keep the historical
//     per-code-point behaviour.
//   - Whitespace, punctuation and symbols (including emoji) do not count.
func CountDraftUnits(text string) int {
	draftUnitCache.mu.Lock()
	if draftUnitCache.valid && draftUnitCache.text == text {
		units := draftUnitCache.units
		draftUnitCache.mu.Unlock()
		return units
	}
	draftUnitCache.mu.Unlock()

	units := countUncachedDraftUnits(text)

	draftUnitCache.mu.Lock()
	draftUnitCache.text, draftUnitCache.units, draftUnitCache.valid = text, units, true
	draftUnitCache.mu.Unlock()
	return units
}

// draftUnitCache is the module-level `lastCounted` pair (draft-units.ts:37). It
// is keyed on the exact text, so it can only ever return a count the slow path
// would also return; the mutex only makes that cache safe to share.
var draftUnitCache struct {
	mu    sync.Mutex
	text  string
	units int
	valid bool
}

// countUncachedDraftUnits is countUncached (draft-units.ts:56-66).
func countUncachedDraftUnits(text string) int {
	normalized := norm.NFC.String(text)
	hanCharacters := len(hanCharacterPattern.FindAllStringIndex(normalized, -1))
	// Han code points are replaced by a space rather than deleted, so a Han
	// character next to a Latin word still separates two words.
	withoutHan := hanCharacterPattern.ReplaceAllString(normalized, " ")
	words := len(unicodeWordPattern.FindAllStringIndex(withoutHan, -1))
	remainder := unicodeWordPattern.ReplaceAllString(withoutHan, "")
	remainder = whitespacePunctuationSymbolPattern.ReplaceAllString(remainder, "")
	// `[...text].length` iterates code points, so this arm counts one per astral
	// character - the opposite choice from the legacy arm above.
	return hanCharacters + words + utf8.RuneCountInString(remainder)
}

// jsTrimText is String.prototype.trim over a Go string.
func jsTrimText(text string) string {
	return strings.TrimFunc(text, isJavaScriptSpaceRune)
}

// isJavaScriptSpaceRune is membership in the JavaScript WhiteSpace and
// LineTerminator set, which is also what \s and String.prototype.trim use. Go's
// unicode.IsSpace disagrees at both ends of the range: it includes U+0085, which
// the original does not treat as whitespace, and it excludes U+FEFF, which the
// original does treat as whitespace. Neither library helper may stand in here.
func isJavaScriptSpaceRune(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}
