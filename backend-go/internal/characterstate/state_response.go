// Package characterstate is the isolated Go port of the finalized-character
// state response parser. It is the last piece the finalized-character:commit
// handler needs: it turns the model's answer into a validated list of updates.
//
// It is a leaf package. It imports only the standard library and never
// internal/store, so a model answer can be validated without depending on the
// database layer; the commit adapter converts store.FinalizedCharacterContext
// into Context (see below).
//
// Every refusal code of parseFinalizedCharacterStateResponse
// (src/shared/finalized-continuity.ts:177-230) is reproduced with the reference
// line quoted above its guard, and the guards keep the reference's order: the
// first invalid update refuses the whole response and no update is ever skipped.
package characterstate

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// IdentityBound and IdentityLegacy are context.identityStatus
// (finalized-continuity.ts:118), the same two strings store publishes
// (finalized_character_context.go:55-56). Any other value behaves like the
// reference's "not bound": no character is resolved.
const (
	IdentityBound  = "bound"
	IdentityLegacy = "legacy"
)

// The six state fields are CHARACTER_STATE_TEXT_FIELDS
// (src/shared/character-roster.ts:44-51), in the reference's order. The order is
// observable: the reference builds currentState by walking that list (:201), so
// a model that emits its fields in another order still produces this one.
const (
	FieldLocation      = "location"
	FieldPowerLevel    = "powerLevel"
	FieldPhysicalState = "physicalState"
	FieldMentalState   = "mentalState"
	FieldKeyItems      = "keyItems"
	FieldRecentEvents  = "recentEvents"
)

// updatedAtChapterField is the one accepted key outside the six value fields
// (:206). It is validated against the frozen chapter and never copied into the
// result.
const updatedAtChapterField = "updatedAtChapter"

// stateFields is CHARACTER_STATE_TEXT_FIELDS in reference order.
var stateFields = [6]string{FieldLocation, FieldPowerLevel, FieldPhysicalState, FieldMentalState, FieldKeyItems, FieldRecentEvents}

// Context is the frozen chapter context the caller already holds. It is a local,
// minimal mirror of store.FinalizedCharacterContext
// (internal/store/finalized_character_context.go:131): the commit adapter
// converts that value member by member, and only the members the parser reads are
// declared here - the source chapter number (:195-197, :206), the frozen prose
// (:190, :192, :197) and the frozen identities (:211, :215, :219-222).
type Context struct {
	Source         ContextSource
	Content        string
	IdentityStatus string
	Characters     []ContextCharacter
}

// ContextSource is the only part of FinalizedSourceIdentity (:12-17) the parser
// reads; a digest or receipt check belongs to the caller, not here.
type ContextSource struct {
	ChapterNumber int64
}

// ContextCharacter is one frozen character
// (store.FinalizedCharacterContextCharacter, finalized_character_context.go:120)
// without its field snapshots: the parser never reads a frozen field list, it
// validates the model's field names against the closed six-name list above.
type ContextCharacter struct {
	CharacterID         string
	DisplayNameSnapshot string
	Aliases             []string
}

// Evidence is FinalizedCharacterEvidence (:112). Start and End count UTF-16 code
// units in the immutable, unnormalized prose, end exclusive; the parser either
// verifies them or computes them from a unique, verbatim Text.
type Evidence struct {
	Start int
	End   int
	Text  string
}

// Values is FinalizedCharacterStateValues (:142), a partial record that keeps
// "absent" distinct from "present and empty": the reference copies a key only
// when Object.hasOwn reports it, and "" is a legitimate value. A field is nil
// exactly when the model omitted it.
type Values struct {
	Location      *string `json:"location,omitempty"`
	PowerLevel    *string `json:"powerLevel,omitempty"`
	PhysicalState *string `json:"physicalState,omitempty"`
	MentalState   *string `json:"mentalState,omitempty"`
	KeyItems      *string `json:"keyItems,omitempty"`
	RecentEvents  *string `json:"recentEvents,omitempty"`
}

// Len is Object.keys(currentState).length (:206).
func (values Values) Len() int {
	present := 0
	for _, field := range [...]*string{values.Location, values.PowerLevel, values.PhysicalState,
		values.MentalState, values.KeyItems, values.RecentEvents} {
		if field != nil {
			present++
		}
	}
	return present
}

// Get reads one field the way currentState[field] does, reporting whether the
// model supplied it. An unknown name is never present.
func (values Values) Get(field string) (string, bool) {
	var pointer *string
	switch field {
	case FieldLocation:
		pointer = values.Location
	case FieldPowerLevel:
		pointer = values.PowerLevel
	case FieldPhysicalState:
		pointer = values.PhysicalState
	case FieldMentalState:
		pointer = values.MentalState
	case FieldKeyItems:
		pointer = values.KeyItems
	case FieldRecentEvents:
		pointer = values.RecentEvents
	}
	if pointer == nil {
		return "", false
	}
	return *pointer, true
}

// set is currentState[field] = value (:204). The address of the caller's own
// parameter is taken, so each copied value owns its cell.
func (values *Values) set(field, value string) {
	switch field {
	case FieldLocation:
		values.Location = &value
	case FieldPowerLevel:
		values.PowerLevel = &value
	case FieldPhysicalState:
		values.PhysicalState = &value
	case FieldMentalState:
		values.MentalState = &value
	case FieldKeyItems:
		values.KeyItems = &value
	case FieldRecentEvents:
		values.RecentEvents = &value
	}
}

// Update is FinalizedCharacterStateUpdate (:143-147).
type Update struct {
	CharacterID  string
	CurrentState Values
	Evidence     Evidence
}

// Occurrence is FinalizedCharacterStateOccurrence (:149-158): a model occurrence
// that needs an explicit identity decision and is never merged automatically.
// RawValue is structuredClone(input) (:221): the update exactly as the model wrote
// it, unknown keys included. It is a private decode of this body, so it is already
// a deep copy and shares nothing with the result.
type Occurrence struct {
	SelectionKey string
	DisplayName  string
	OriginalText string
	CandidateIDs []string
	CurrentState Values
	Evidence     Evidence
	RawValue     any
	Reason       string
}

// Response is FinalizedCharacterStateResponse (:159-162). Both lists are non-nil
// on success, mirroring the reference's [] initializer (:181), so a re-encode
// emits [] rather than null.
type Response struct {
	Updates    []Update
	Unresolved []Occurrence
}

// Evidence reasons are the reason union of FinalizedCharacterEvidenceError
// (:170-172).
const (
	ReasonMissing              = "missing"
	ReasonNoExactMatch         = "no-exact-match"
	ReasonMultipleExactMatches = "multiple-exact-matches"
	ReasonOffsetMismatch       = "offset-mismatch"
)

// Occurrence reasons are the reason union of FinalizedCharacterStateOccurrence
// (:157).
const (
	ReasonNameOnly  = "name-only"
	ReasonUnknownID = "unknown-id"
	ReasonAmbiguous = "ambiguous"
	ReasonLegacy    = "legacy"
)

// MaxUpdates is the cap of :180 (parsed.updates.length > 1_000). 1,000 updates
// are accepted, 1,001 are refused.
const MaxUpdates = 1000

// The bounds of Number.isSafeInteger.
const (
	minSafeInteger = -(1<<53 - 1)
	maxSafeInteger = 1<<53 - 1
)

// ErrUpdatesInvalid is :180
//
//	if (!record(parsed) || !Array.isArray(parsed.updates) || parsed.updates.length > 1_000)
//	  throw new Error('FINALIZED_CHARACTER_UPDATES_INVALID')
var ErrUpdatesInvalid = errors.New("FINALIZED_CHARACTER_UPDATES_INVALID")

// ErrUpdateInvalid is :184
//
//	if (!record(input) || !record(input.currentState)) throw new Error('FINALIZED_CHARACTER_UPDATE_INVALID')
var ErrUpdateInvalid = errors.New("FINALIZED_CHARACTER_UPDATE_INVALID")

// ErrFieldInvalid is :203 and :206-208
//
//	if (typeof input.currentState[field] !== 'string') throw new Error('FINALIZED_CHARACTER_FIELD_INVALID')
//	if (!Object.keys(currentState).length || Object.hasOwn(input.currentState, 'updatedAtChapter')
//	  && input.currentState.updatedAtChapter !== context.source.chapterNumber)
//	  throw new Error('FINALIZED_CHARACTER_FIELD_INVALID')
var ErrFieldInvalid = errors.New("FINALIZED_CHARACTER_FIELD_INVALID")

// ErrIdentityRequired is :217
//
//	if (!originalText.trim() && !characterId) throw new Error('FINALIZED_CHARACTER_IDENTITY_REQUIRED')
var ErrIdentityRequired = errors.New("FINALIZED_CHARACTER_IDENTITY_REQUIRED")

// ErrDuplicateID is :225
//
//	if (seen.has(characterId)) throw new Error('FINALIZED_CHARACTER_DUPLICATE_ID')
var ErrDuplicateID = errors.New("FINALIZED_CHARACTER_DUPLICATE_ID")

// ErrBodyInvalid is :179
//
//	const parsed: unknown = JSON.parse(text)
//
// Port-only code: JSON.parse throws a SyntaxError, which carries no code at all,
// and the caller keeps that arm distinct from the eight coded refusals
// (main-generation-owner.ts:1281 treats a SyntaxError as a repairable body).
// Naming it here is what preserves that distinction.
var ErrBodyInvalid = errors.New("FINALIZED_CHARACTER_RESPONSE_BODY_INVALID")

// EvidenceError is FinalizedCharacterEvidenceError (:169-173). Like the
// reference, its message is the bare code; the index and reason ride beside it so
// a repair turn can name updates[index].evidence.text without parsing prose.
type EvidenceError struct {
	UpdateIndex int
	Reason      string
	code        error
}

// Error reports the bare code, exactly as super(code) does.
func (e *EvidenceError) Error() string { return e.code.Error() }

// Unwrap is what makes errors.Is(err, ErrEvidenceMissing) and its siblings work.
func (e *EvidenceError) Unwrap() error { return e.code }

// Code returns the refusal code carried in the reference's message.
func (e *EvidenceError) Code() error { return e.code }

// ErrEvidenceMissing is :186
//
//	if (!record(input.evidence)) throw new FinalizedCharacterEvidenceError('FINALIZED_CHARACTER_EVIDENCE_MISSING', index, 'missing')
var ErrEvidenceMissing = errors.New("FINALIZED_CHARACTER_EVIDENCE_MISSING")

// ErrEvidenceNotUnique is :191 and :192
//
//	if (located < 0) throw new FinalizedCharacterEvidenceError('FINALIZED_CHARACTER_EVIDENCE_NOT_UNIQUE', index, 'no-exact-match')
//	if (context.content.indexOf(evidence.text, located + 1) !== -1) throw new FinalizedCharacterEvidenceError('FINALIZED_CHARACTER_EVIDENCE_NOT_UNIQUE', index, 'multiple-exact-matches')
var ErrEvidenceNotUnique = errors.New("FINALIZED_CHARACTER_EVIDENCE_NOT_UNIQUE")

// ErrEvidenceInvalid is :195-199
//
//	if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || (start as number) < 0
//	  || (end as number) <= (start as number) || (end as number) > context.content.length
//	  || typeof evidence.text !== 'string' || context.content.slice(start as number, end as number) !== evidence.text)
//	  throw new FinalizedCharacterEvidenceError('FINALIZED_CHARACTER_EVIDENCE_INVALID', index, 'offset-mismatch')
var ErrEvidenceInvalid = errors.New("FINALIZED_CHARACTER_EVIDENCE_INVALID")

// ParseResponse is parseFinalizedCharacterStateResponse (:177-230).
//
// The answer is re-parsed against the caller's frozen context instead of being
// trusted, so an update can only name a field of the closed six-name list and an
// identity the frozen chapter actually holds.
//
// A refusal returns the zero Response with an error whose message is the bare
// code, exactly as the reference throws: updates accepted earlier in the same
// body are dropped, never returned alongside the error.
func ParseResponse(content string, context Context) (Response, error) {
	// :178
	//   const text = content.trim().replace(/^```(?:json)?\s*\n?/i, '').replace(/\n?```\s*$/, '')
	// The trimming and the two fence rewrites are reproduced exactly, so a model
	// that emits a fence parses the same way in both implementations.
	text := stripTrailingFence(stripLeadingFence(trimJS(content)))

	// :179
	//   const parsed: unknown = JSON.parse(text)
	var parsed any
	decoder := json.NewDecoder(strings.NewReader(text))
	// UseNumber keeps an integer an integer: a float64 round trip would accept
	// offsets the reference rejects.
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil {
		return Response{}, ErrBodyInvalid
	}
	// JSON.parse refuses trailing content; a bare Decode would stop at the end of
	// the first value and silently ignore the rest.
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Response{}, ErrBodyInvalid
	}

	// :180
	//   if (!record(parsed) || !Array.isArray(parsed.updates) || parsed.updates.length > 1_000)
	//     throw new Error('FINALIZED_CHARACTER_UPDATES_INVALID')
	body, isObject := parsed.(map[string]any)
	if !isObject {
		return Response{}, ErrUpdatesInvalid
	}
	inputs, isArray := body["updates"].([]any)
	if !isArray || len(inputs) > MaxUpdates {
		return Response{}, ErrUpdatesInvalid
	}

	// :181
	//   const result: FinalizedCharacterStateResponse = { updates: [], unresolved: [] }
	response := Response{Updates: make([]Update, 0, len(inputs)), Unresolved: make([]Occurrence, 0)}
	// :182
	//   const seen = new Set<string>()
	seen := make(map[string]struct{}, len(inputs))
	// Offsets are UTF-16 code units in the unnormalized prose (:111), so the
	// frozen content is compared in code units and never in bytes or runes.
	contentUnits := unitsOf(context.Content)

	// :183
	//   for (const [index, input] of parsed.updates.entries()) {
	for index, rawUpdate := range inputs {
		// :184
		//   if (!record(input) || !record(input.currentState)) throw new Error('FINALIZED_CHARACTER_UPDATE_INVALID')
		input, isRecord := rawUpdate.(map[string]any)
		if !isRecord {
			return Response{}, ErrUpdateInvalid
		}
		rawState, hasState := input["currentState"].(map[string]any)
		if !hasState {
			return Response{}, ErrUpdateInvalid
		}

		// :186-199
		evidence, err := parseEvidence(input["evidence"], contentUnits, index)
		if err != nil {
			return Response{}, err
		}

		// :200-208
		values, err := parseValues(rawState, context.Source.ChapterNumber)
		if err != nil {
			return Response{}, err
		}

		// :209
		//   const characterId = typeof input.characterId === 'string' ? input.characterId : ''
		characterID, _ := input["characterId"].(string)
		// :210
		//   const originalText = typeof input.name === 'string' ? input.name : ''
		originalText, _ := input["name"].(string)
		// :211
		//   const character = context.identityStatus === 'bound' ? context.characters.find(item => item.characterId === characterId) : undefined
		var character *ContextCharacter
		if context.IdentityStatus == IdentityBound {
			for position := range context.Characters {
				if context.Characters[position].CharacterID == characterID {
					character = &context.Characters[position]
					break
				}
			}
		}

		// :215
		//   const sharedLabel = character && context.characters.filter(item => item.displayNameSnapshot === character.displayNameSnapshot).length > 1
		sharedLabel := false
		if character != nil {
			sharing := 0
			for _, item := range context.Characters {
				if item.DisplayNameSnapshot == character.DisplayNameSnapshot {
					sharing++
				}
			}
			sharedLabel = sharing > 1
		}

		// :216
		//   if (!character || sharedLabel) {
		if character == nil || sharedLabel {
			// :217
			//   if (!originalText.trim() && !characterId) throw new Error('FINALIZED_CHARACTER_IDENTITY_REQUIRED')
			trimmed := trimJS(originalText)
			if trimmed == "" && characterID == "" {
				return Response{}, ErrIdentityRequired
			}
			// :218
			//   displayName: originalText.trim() ? originalText : character?.displayNameSnapshot ?? originalText
			displayName := originalText
			if trimmed == "" && character != nil {
				displayName = character.DisplayNameSnapshot
			}
			// :219-220
			//   candidateIds: context.characters.filter(item => item.characterId === characterId
			//     || sharedLabel && item.displayNameSnapshot === character?.displayNameSnapshot
			//     || item.displayNameSnapshot === originalText || item.aliases.includes(originalText)).map(item => item.characterId)
			candidates := make([]string, 0, len(context.Characters))
			for _, item := range context.Characters {
				matched := item.CharacterID == characterID ||
					(sharedLabel && character != nil && item.DisplayNameSnapshot == character.DisplayNameSnapshot) ||
					item.DisplayNameSnapshot == originalText ||
					slices.Contains(item.Aliases, originalText)
				if matched {
					candidates = append(candidates, item.CharacterID)
				}
			}
			// :222
			//   reason: context.identityStatus === 'legacy' ? 'legacy' : sharedLabel ? 'ambiguous' : characterId ? 'unknown-id' : 'name-only'
			reason := ReasonNameOnly
			switch {
			case context.IdentityStatus == IdentityLegacy:
				reason = ReasonLegacy
			case sharedLabel:
				reason = ReasonAmbiguous
			case characterID != "":
				reason = ReasonUnknownID
			}
			response.Unresolved = append(response.Unresolved, Occurrence{
				// :218
				//   selectionKey: `update:${index}`
				SelectionKey: "update:" + strconv.Itoa(index),
				DisplayName:  displayName,
				OriginalText: originalText,
				CandidateIDs: candidates,
				CurrentState: values,
				Evidence:     evidence,
				RawValue:     input,
				Reason:       reason,
			})
			// :223
			//   continue
			continue
		}

		// :225
		//   if (seen.has(characterId)) throw new Error('FINALIZED_CHARACTER_DUPLICATE_ID')
		if _, duplicate := seen[characterID]; duplicate {
			return Response{}, ErrDuplicateID
		}
		// :226
		//   seen.add(characterId)
		seen[characterID] = struct{}{}
		// :227
		//   result.updates.push({ characterId, currentState, evidence: exactEvidence })
		response.Updates = append(response.Updates, Update{CharacterID: characterID, CurrentState: values, Evidence: evidence})
	}
	// :229
	//   return result
	return response, nil
}

// parseEvidence is :186-199 for one update.
func parseEvidence(raw any, contentUnits []uint16, index int) (Evidence, error) {
	evidence, isObject := raw.(map[string]any)
	if !isObject {
		// :186
		return Evidence{}, &EvidenceError{UpdateIndex: index, Reason: ReasonMissing, code: ErrEvidenceMissing}
	}
	// :197 requires evidence.text to be a string; it is converted once here so the
	// offset comparison can work in code units.
	text, textIsString := evidence["text"].(string)
	var textUnits []uint16
	if textIsString {
		textUnits = unitsOf(text)
	}
	startValue, hasStart := evidence["start"]
	endValue, hasEnd := evidence["end"]

	// :189-194
	//   if (!Object.hasOwn(evidence, 'start') && !Object.hasOwn(evidence, 'end') && typeof evidence.text === 'string' && evidence.text.length > 0) {
	start, end := int64(0), int64(0)
	located := false
	if !hasStart && !hasEnd && len(textUnits) > 0 {
		// :190
		//   const located = context.content.indexOf(evidence.text)
		first := indexUnits(contentUnits, textUnits, 0)
		// :191
		//   if (located < 0) throw ... 'no-exact-match'
		if first < 0 {
			return Evidence{}, &EvidenceError{UpdateIndex: index, Reason: ReasonNoExactMatch, code: ErrEvidenceNotUnique}
		}
		// :192
		//   if (context.content.indexOf(evidence.text, located + 1) !== -1) throw ... 'multiple-exact-matches'
		if indexUnits(contentUnits, textUnits, first+1) >= 0 {
			return Evidence{}, &EvidenceError{UpdateIndex: index, Reason: ReasonMultipleExactMatches, code: ErrEvidenceNotUnique}
		}
		// :193
		//   start = located; end = located + evidence.text.length
		start, end, located = int64(first), int64(first+len(textUnits)), true
	}
	if !located {
		// :188
		//   let start = evidence.start, end = evidence.end
		// An absent, non-numeric, fractional or unsafe value is rejected by the
		// :195 guard below, exactly where the reference rejects it.
		var startOK, endOK bool
		start, startOK = safeInteger(hasStart, startValue)
		end, endOK = safeInteger(hasEnd, endValue)
		if !startOK || !endOK {
			return Evidence{}, &EvidenceError{UpdateIndex: index, Reason: ReasonOffsetMismatch, code: ErrEvidenceInvalid}
		}
	}

	// :195-199
	// The || chain is evaluated left to right like the reference's, so the slice is
	// only formed after the bounds and the text type have been proved.
	if start < 0 || end <= start || end > int64(len(contentUnits)) ||
		!textIsString || !slices.Equal(contentUnits[int(start):int(end)], textUnits) {
		return Evidence{}, &EvidenceError{UpdateIndex: index, Reason: ReasonOffsetMismatch, code: ErrEvidenceInvalid}
	}
	// :212
	//   const exactEvidence = { start: start as number, end: end as number, text: evidence.text }
	return Evidence{Start: int(start), End: int(end), Text: text}, nil
}

// parseValues is :200-208 for one update.
func parseValues(raw map[string]any, chapterNumber int64) (Values, error) {
	var values Values
	// :201
	//   for (const field of CHARACTER_STATE_TEXT_FIELDS) {
	// The list is closed, so a field the frozen context does not freeze can never
	// be copied: an unknown key is simply never visited, and a body whose only keys
	// are unknown is refused by the emptiness check below.
	for _, field := range stateFields {
		// :202
		//   if (!Object.hasOwn(input.currentState, field)) continue
		value, present := raw[field]
		if !present {
			continue
		}
		// :203
		text, isString := value.(string)
		if !isString {
			return Values{}, ErrFieldInvalid
		}
		// :204
		values.set(field, text)
	}
	// :206
	//   if (!Object.keys(currentState).length || ...) throw new Error('FINALIZED_CHARACTER_FIELD_INVALID')
	if values.Len() == 0 {
		return Values{}, ErrFieldInvalid
	}
	// :206-207
	//   Object.hasOwn(input.currentState, 'updatedAtChapter') && input.currentState.updatedAtChapter !== context.source.chapterNumber
	if updated, present := raw[updatedAtChapterField]; present && !chapterMatches(updated, chapterNumber) {
		return Values{}, ErrFieldInvalid
	}
	return values, nil
}

// safeInteger is Number.isSafeInteger (:195) for one value read straight out of
// the decoded JSON: an absent key, null, string, boolean, array or object is not
// a safe integer, and neither is a fraction or a value outside ±(2^53-1). An
// integer literal is read exactly, so nothing is rounded into range.
func safeInteger(present bool, value any) (int64, bool) {
	if !present {
		return 0, false
	}
	number, isNumber := value.(json.Number)
	if !isNumber {
		return 0, false
	}
	literal := number.String()
	if parsed, err := strconv.ParseInt(literal, 10, 64); err == nil {
		if parsed >= minSafeInteger && parsed <= maxSafeInteger {
			return parsed, true
		}
		return 0, false
	}
	// A literal with a fraction or an exponent takes the route JSON.parse itself
	// takes, then gets the same range and integrality test.
	value64, err := strconv.ParseFloat(literal, 64)
	if err != nil || math.IsInf(value64, 0) || math.IsNaN(value64) {
		return 0, false
	}
	if value64 != math.Trunc(value64) || value64 < minSafeInteger || value64 > maxSafeInteger {
		return 0, false
	}
	return int64(value64), true
}

// chapterMatches is input.currentState.updatedAtChapter !== context.source.chapterNumber
// (:206). The reference compares with ===, so only a JSON number of the same value
// passes: "1", null, true and 2 all fail against chapter 1.
func chapterMatches(value any, chapterNumber int64) bool {
	number, isNumber := value.(json.Number)
	if !isNumber {
		return false
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return false
	}
	return parsed == float64(chapterNumber)
}

// unitsOf returns the UTF-16 code units of text, the unit every offset in this
// contract counts in (:111). It normalizes nothing: the prose is immutable and
// unnormalized, so only the encoding changes.
func unitsOf(text string) []uint16 { return utf16.Encode([]rune(text)) }

// indexUnits is String.prototype.indexOf(text, from) over code units (:190, :192):
// the first index at or after from where needle occurs, or -1.
func indexUnits(haystack, needle []uint16, from int) int {
	if from < 0 {
		from = 0
	}
	last := len(haystack) - len(needle)
	for start := from; start <= last; start++ {
		if slices.Equal(haystack[start:start+len(needle)], needle) {
			return start
		}
	}
	return -1
}

// jsSpace is the JavaScript WhiteSpace + LineTerminator set, which is what trim,
// \s and the two fence regexes of :178 use. It differs from unicode.IsSpace: NEL
// is not trimmed by JavaScript and U+FEFF is.
func jsSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' ||
		r == ' ' || r == '\u00a0' || r == '\u1680' ||
		(r >= '\u2000' && r <= '\u200a') || r == '\u2028' || r == '\u2029' ||
		r == '\u202f' || r == '\u205f' || r == '\u3000' || r == '\ufeff'
}

// trimJS is String.prototype.trim for :178.
func trimJS(value string) string { return strings.TrimFunc(value, jsSpace) }

// stripLeadingFence is replace(/^```(?:json)?\s*\n?/i, '') (:178). The regex's \s*
// is greedy and nothing but an optional newline follows it, so it always consumes
// the whole whitespace run - the same run TrimLeftFunc eats here.
func stripLeadingFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	rest := text[3:]
	// (?i) folding of an ASCII tag: a four-byte prefix, then the whitespace run.
	if len(rest) >= 4 && strings.EqualFold(rest[:4], "json") {
		rest = rest[4:]
	}
	return strings.TrimLeftFunc(rest, jsSpace)
}

// stripTrailingFence is replace(/\n?```\s*$/, '') (:178). Without the m flag $ is
// the end of the input, so the only fence that can match is the last one, and only
// when nothing but whitespace follows it; the single newline before it goes with it.
func stripTrailingFence(text string) string {
	fence := strings.LastIndex(text, "```")
	if fence < 0 || strings.TrimFunc(text[fence+3:], jsSpace) != "" {
		return text
	}
	if fence > 0 && text[fence-1] == '\n' {
		fence--
	}
	return text[:fence]
}
