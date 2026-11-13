package graphgen

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// ErrNarrativeThreadCandidatesInvalid mirrors the symbolic error thrown by
// invalidCandidates (narrative-thread-generation-pure.ts:54-57).
var ErrNarrativeThreadCandidatesInvalid = errors.New("NARRATIVE_THREAD_CANDIDATES_INVALID")

// Narrative thread statuses and event types (src/shared/narrative-thread.ts:1-2).
const (
	NarrativeThreadStatusPlanned     = "planned"
	NarrativeThreadStatusPlanted     = "planted"
	NarrativeThreadStatusProgressing = "progressing"
	NarrativeThreadStatusResolved    = "resolved"
	NarrativeThreadStatusAbandoned   = "abandoned"
)

// narrativeThreadCandidateLimits mirrors MAX_PLAN_CANDIDATES and
// MAX_EVENT_CANDIDATES (narrative-thread-generation-pure.ts:51-52).
const (
	maxNarrativeThreadPlanCandidates  = 8
	maxNarrativeThreadEventCandidates = 5
)

// Candidate field ceilings from parseNarrativeThreadPlanCandidates /
// parseNarrativeThreadEventCandidates (narrative-thread-generation-pure.ts:81-83, :114-115).
const (
	narrativeThreadTitleCharacters        = 120
	narrativeThreadTypeCharacters         = 60
	narrativeThreadAuthorIntentCharacters = 1000
	narrativeThreadEvidenceCharacters     = 240
	narrativeThreadReasonCharacters       = 500
)

// NarrativeThreadPlan mirrors NarrativeThreadPlanInput (src/shared/narrative-thread.ts:14-20).
type NarrativeThreadPlan struct {
	Title              string `json:"title"`
	Type               string `json:"type"`
	TargetStartChapter int    `json:"targetStartChapter"`
	TargetEndChapter   int    `json:"targetEndChapter"`
	AuthorIntent       string `json:"authorIntent"`
}

// NarrativeThreadPlanCandidate mirrors NarrativeThreadPlanCandidate
// (narrative-thread-generation-pure.ts:10).
type NarrativeThreadPlanCandidate = NarrativeThreadPlan

// NarrativeThreadEvent mirrors NarrativeThreadEventInput
// (src/shared/narrative-thread.ts:22-28).
type NarrativeThreadEvent struct {
	PlanID   int    `json:"planId"`
	DraftID  int    `json:"draftId"`
	Type     string `json:"type"`
	Evidence string `json:"evidence"`
	Reason   string `json:"reason"`
}

// NarrativeThreadStoredEvent mirrors NarrativeThreadEvent (src/shared/narrative-thread.ts:30-37).
type NarrativeThreadStoredEvent struct {
	NarrativeThreadEvent
	ID            int    `json:"id"`
	ChapterNumber int    `json:"chapterNumber"`
	ChapterTitle  string `json:"chapterTitle"`
	CreatedAt     string `json:"createdAt"`
}

// NarrativeThreadView mirrors NarrativeThreadView (src/shared/narrative-thread.ts:39-48).
type NarrativeThreadView struct {
	NarrativeThreadPlan
	ID              int                          `json:"id"`
	Status          string                       `json:"status"`
	DormantChapters int                          `json:"dormantChapters"`
	Overdue         bool                         `json:"overdue"`
	Events          []NarrativeThreadStoredEvent `json:"events"`
	CreatedAt       string                       `json:"createdAt"`
	UpdatedAt       string                       `json:"updatedAt"`
}

// NarrativeThreadEventCandidate mirrors NarrativeThreadEventCandidate
// (narrative-thread-generation-pure.ts:12-16).
type NarrativeThreadEventCandidate struct {
	Type     string `json:"type"`
	Evidence string `json:"evidence"`
	Reason   string `json:"reason"`
}

// ParseNarrativeThreadPlanCandidates mirrors parseNarrativeThreadPlanCandidates
// (narrative-thread-generation-pure.ts:72-101).
//
// `legacyStoredEffect` has no Go default parameter, so callers pass false for a
// fresh model response. With legacyStoredEffect the parser drops invalid
// candidates instead of rejecting the whole envelope, which is how already
// stored graph effects are re-proved with their original filtered indices; it
// also skips the eight-candidate ceiling (line 62).
//
// Malformed JSON is not NARRATIVE_THREAD_CANDIDATES_INVALID in the TypeScript
// either: JSON.parse's SyntaxError escapes at line 60, and the Go port returns
// the encoding/json error unchanged.
func ParseNarrativeThreadPlanCandidates(
	content string,
	totalChapters int,
	legacyStoredEffect bool,
) ([]NarrativeThreadPlan, error) {
	candidates, err := candidatesFromJSON(content, maxNarrativeThreadPlanCandidates, legacyStoredEffect)
	if err != nil {
		return nil, err
	}
	plans := make([]NarrativeThreadPlan, 0, len(candidates))
	for _, candidate := range candidates {
		value, ok := jsonRecord(candidate)
		if !ok {
			if !legacyStoredEffect {
				return nil, ErrNarrativeThreadCandidatesInvalid
			}
			continue
		}
		title, titleOK := boundedCandidateText(value["title"], narrativeThreadTitleCharacters)
		threadType, typeOK := boundedCandidateText(value["type"], narrativeThreadTypeCharacters)
		authorIntent, authorIntentOK := boundedCandidateText(value["authorIntent"], narrativeThreadAuthorIntentCharacters)
		targetStartChapter, startOK := jsonSafeInteger(value["targetStartChapter"])
		targetEndChapter, endOK := jsonSafeInteger(value["targetEndChapter"])
		if !titleOK || !typeOK || !authorIntentOK ||
			!startOK || targetStartChapter < 1 || targetStartChapter > totalChapters ||
			!endOK || targetEndChapter < targetStartChapter || targetEndChapter > totalChapters {
			if !legacyStoredEffect {
				return nil, ErrNarrativeThreadCandidatesInvalid
			}
			continue
		}
		plans = append(plans, NarrativeThreadPlan{
			Title:              title,
			Type:               threadType,
			TargetStartChapter: targetStartChapter,
			TargetEndChapter:   targetEndChapter,
			AuthorIntent:       authorIntent,
		})
	}
	return plans, nil
}

// ParseNarrativeThreadEventCandidates mirrors parseNarrativeThreadEventCandidates
// (narrative-thread-generation-pure.ts:103-119).
//
// Evidence must appear in the finalized chapter after both sides have every
// `\s` code point removed (line 109 and line 116), so whitespace differences
// never reject a genuine verbatim excerpt.
func ParseNarrativeThreadEventCandidates(
	content string,
	finalizedContent string,
	legacyStoredEffect bool,
) ([]NarrativeThreadEvent, error) {
	normalizedSource := removeJSWhitespace(finalizedContent)
	candidates, err := candidatesFromJSON(content, maxNarrativeThreadEventCandidates, legacyStoredEffect)
	if err != nil {
		return nil, err
	}
	events := make([]NarrativeThreadEvent, 0, len(candidates))
	for _, candidate := range candidates {
		value, ok := jsonRecord(candidate)
		if !ok {
			if !legacyStoredEffect {
				return nil, ErrNarrativeThreadCandidatesInvalid
			}
			continue
		}
		// Line 112-113 with JavaScript operator precedence:
		//   (!value) || ((!legacyStoredEffect) && typeof value.type !== 'string') || !allowed.includes(String(value.type))
		rawType := value["type"]
		if !legacyStoredEffect {
			if _, isString := rawType.(string); !isString {
				return nil, ErrNarrativeThreadCandidatesInvalid
			}
		}
		// String(value.type) decides the allowed-type check even in legacy mode,
		// so a JSON array whose String() is "planted" passes in TypeScript. The
		// Go port coerces it to that string instead of preserving the array.
		eventType := jsString(rawType)
		if !isNarrativeThreadEventType(eventType) {
			if !legacyStoredEffect {
				return nil, ErrNarrativeThreadCandidatesInvalid
			}
			continue
		}
		evidence, evidenceOK := boundedCandidateText(value["evidence"], narrativeThreadEvidenceCharacters)
		reason, reasonOK := boundedCandidateText(value["reason"], narrativeThreadReasonCharacters)
		if !evidenceOK || !reasonOK ||
			!strings.Contains(normalizedSource, removeJSWhitespace(evidence)) {
			if !legacyStoredEffect {
				return nil, ErrNarrativeThreadCandidatesInvalid
			}
			continue
		}
		events = append(events, NarrativeThreadEvent{
			Type:     eventType,
			Evidence: evidence,
			Reason:   reason,
		})
	}
	return events, nil
}

// candidatesFromJSON mirrors candidatesFromJSON
// (narrative-thread-generation-pure.ts:59-64).
func candidatesFromJSON(content string, limit int, legacyStoredEffect bool) ([]any, error) {
	var parsed any
	if err := json.Unmarshal([]byte(jsTrim(content)), &parsed); err != nil {
		// JSON.parse throws here too (line 60); the syntax error is not
		// translated into NARRATIVE_THREAD_CANDIDATES_INVALID.
		return nil, err
	}
	envelope, ok := jsonRecord(parsed)
	if !ok {
		return invalidCandidates(legacyStoredEffect)
	}
	candidates, ok := jsonArray(envelope["candidates"])
	if !ok {
		return invalidCandidates(legacyStoredEffect)
	}
	if !legacyStoredEffect && len(candidates) > limit {
		return invalidCandidates(false)
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates, nil
}

// invalidCandidates mirrors invalidCandidates
// (narrative-thread-generation-pure.ts:54-57): legacy callers get an empty,
// valid result while everyone else gets the symbolic error.
func invalidCandidates(legacyStoredEffect bool) ([]any, error) {
	if !legacyStoredEffect {
		return nil, ErrNarrativeThreadCandidatesInvalid
	}
	return []any{}, nil
}

// boundedCandidateText mirrors boundedText (narrative-thread-generation-pure.ts:66-70):
// only strings survive, the value is trimmed, and the ceiling is measured in
// UTF-16 code units like String.prototype.length.
func boundedCandidateText(value any, maximum int) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := jsTrim(text)
	if trimmed == "" || utf16Length(trimmed) > maximum {
		return "", false
	}
	return trimmed, true
}

func isNarrativeThreadEventType(value string) bool {
	switch value {
	case NarrativeThreadStatusPlanted, NarrativeThreadStatusProgressing,
		NarrativeThreadStatusResolved, NarrativeThreadStatusAbandoned:
		return true
	}
	return false
}

// jsonRecord mirrors `record` (narrative-thread-generation-pure.ts:45-49,
// plot-tree.ts:90-95): a JSON object, never an array, never null.
func jsonRecord(value any) (map[string]any, bool) {
	record, ok := value.(map[string]any)
	return record, ok
}

// jsonArray mirrors `Array.isArray` for JSON-decoded values.
func jsonArray(value any) ([]any, bool) {
	array, ok := value.([]any)
	return array, ok
}

// jsonSafeInteger mirrors Number.isSafeInteger for JSON-decoded numbers, which
// decode to float64 exactly like a JavaScript number. 1.0 and 1 are the same
// value, so an integral float is accepted.
func jsonSafeInteger(value any) (int, bool) {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		return typed, true
	case int64:
		if typed > 1<<53-1 || typed < -(1<<53-1) {
			return 0, false
		}
		return int(typed), true
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) {
		return 0, false
	}
	if number > 1<<53-1 || number < -(1<<53-1) {
		return 0, false
	}
	return int(number), true
}

// jsString mirrors String(value) for JSON-decoded values, which
// parseNarrativeThreadEventCandidates uses to validate `value.type`
// (narrative-thread-generation-pure.ts:113). Objects become "[object Object]"
// exactly like JavaScript.
func jsString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return formatJSONNumber(typed)
	case int:
		return formatJSONNumber(float64(typed))
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, jsString(item))
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return "[object Object]"
	}
}

// isJSWhitespace reports the exact ECMAScript `\s` set: WhiteSpace (TAB, VT,
// FF, SP, NBSP, ZWNBSP and the Zs category) plus LineTerminator (LF, CR, LS,
// PS). Go's unicode.IsSpace differs (it also accepts U+0085, which
// JavaScript does not, and both accept the Zs category), so the set is spelled
// out for /(?:)/u parity at narrative-thread-generation-pure.ts:109 and :116.
func isJSWhitespace(character rune) bool {
	switch character {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return character >= 0x2000 && character <= 0x200A
}

// removeJSWhitespace mirrors String.prototype.replace(/\s+/gu, ”)
// (narrative-thread-generation-pure.ts:109,116).
func removeJSWhitespace(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, character := range value {
		if isJSWhitespace(character) {
			continue
		}
		builder.WriteRune(character)
	}
	return builder.String()
}

// jsTrim mirrors String.prototype.trim, which strips the same ECMAScript
// whitespace set on both ends.
func jsTrim(value string) string {
	return strings.TrimFunc(value, isJSWhitespace)
}
