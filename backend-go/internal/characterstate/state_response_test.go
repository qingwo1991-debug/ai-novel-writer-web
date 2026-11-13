package characterstate

// These tests drive ParseResponse against the reference's own fixture prose
// (electron/repositories/__tests__/finalized-character-state.test.ts:23, which
// opens with an astral emoji so every offset is a UTF-16 code unit) and pin one
// exact refusal code per arm of parseFinalizedCharacterStateResponse
// (src/shared/finalized-continuity.ts:177-230). There is no database: a Context
// is a value, so "the chapter froze this" is a fixture, not a query.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// prose is 16 UTF-16 code units: the emoji takes two, so the first character name
// starts at offset 2 rather than at 1 or at byte 4.
const prose = "🌙林岚走进北塔。周研留在旧站。"

// validEvidence is one verified span: units 2..7 are 林岚走进北塔.
const validEvidence = `"evidence":{"start":2,"end":8,"text":"林岚走进北塔"}`

// cleanItem is one bound update whose fields the model wrote out of order.
const cleanItem = `{"characterId":"character-a","currentState":{"recentEvents":"进入北塔","location":"北塔"},` + validEvidence + `}`

func testContext() Context {
	return Context{
		Source:         ContextSource{ChapterNumber: 1},
		Content:        prose,
		IdentityStatus: IdentityBound,
		Characters: []ContextCharacter{
			{CharacterID: "character-a", DisplayNameSnapshot: "林岚", Aliases: []string{"阿岚"}},
			{CharacterID: "character-b", DisplayNameSnapshot: "周研", Aliases: []string{}},
		},
	}
}

func updatesBody(items ...string) string { return `{"updates":[` + strings.Join(items, ",") + `]}` }

// unresolvedBody is a response of count updates that each need an identity
// decision, so only the update cap can decide whether they are accepted.
func unresolvedBody(count int) string {
	items := make([]string, count)
	for index := range items {
		items[index] = `{"name":"林岚","currentState":{"location":"北塔"},"evidence":{"start":2,"end":4,"text":"林岚"}}`
	}
	return updatesBody(items...)
}

// expectCleanUpdate is the resolved arm: one bound character, two of the six
// fields, and offsets the parser verified rather than computed.
func expectCleanUpdate(t *testing.T, response Response) {
	t.Helper()
	if len(response.Updates) != 1 || len(response.Unresolved) != 0 {
		t.Fatalf("updates %d unresolved %d, want 1/0", len(response.Updates), len(response.Unresolved))
	}
	update := response.Updates[0]
	if update.CharacterID != "character-a" {
		t.Fatalf("character %v, want character-a", update.CharacterID)
	}
	if update.Evidence.Start != 2 || update.Evidence.End != 8 || update.Evidence.Text != "林岚走进北塔" {
		t.Fatalf("evidence %d/%d/%s, want 2/8/林岚走进北塔", update.Evidence.Start, update.Evidence.End, update.Evidence.Text)
	}
	location, present := update.CurrentState.Get(FieldLocation)
	if !present || location != "北塔" {
		t.Fatalf("location %v/%v, want 北塔", location, present)
	}
	recent, present := update.CurrentState.Get(FieldRecentEvents)
	if !present || recent != "进入北塔" {
		t.Fatalf("recentEvents %v/%v, want 进入北塔", recent, present)
	}
	// Absent is not empty: the model omitted the other four fields.
	if _, present := update.CurrentState.Get(FieldPhysicalState); present {
		t.Fatal("physicalState is present, want absent")
	}
	if update.CurrentState.Len() != 2 {
		t.Fatalf("fields %d, want 2", update.CurrentState.Len())
	}
	// The model wrote recentEvents first; CHARACTER_STATE_TEXT_FIELDS order (:201)
	// is what the result carries.
	encoded, err := json.Marshal(update.CurrentState)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"location":"北塔","recentEvents":"进入北塔"}` {
		t.Fatalf("field order %s, want location then recentEvents", encoded)
	}
}

func TestParseResponseAccepted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		body  string
		check func(*testing.T, Response)
	}{
		{
			name:  "clean response",
			body:  cleanBody(),
			check: expectCleanUpdate,
		},
		{
			name:  "fenced with a json tag",
			body:  "```json\n" + cleanBody() + "\n```",
			check: expectCleanUpdate,
		},
		{
			name:  "fenced without a tag and with CRLF",
			body:  "```\r\n" + cleanBody() + "\r\n```",
			check: expectCleanUpdate,
		},
		{
			name:  "padded fence with an uppercase tag",
			body:  "  ```JSON\n" + cleanBody() + "\n```  ",
			check: expectCleanUpdate,
		},
		{
			name: "no updates at all",
			body: `{"updates":[]}`,
			check: func(t *testing.T, response Response) {
				// The reference returns [] rather than null (:181).
				if response.Updates == nil || response.Unresolved == nil {
					t.Fatal("an accepted response must carry two empty lists, not nil")
				}
				if len(response.Updates) != 0 || len(response.Unresolved) != 0 {
					t.Fatalf("updates %d unresolved %d, want 0/0", len(response.Updates), len(response.Unresolved))
				}
			},
		},
		{
			name: "evidence offsets computed from a unique text",
			body: updatesBody(`{"characterId":"character-a","currentState":{"location":"旧站"},` +
				`"evidence":{"text":"周研留在旧站"}}`),
			check: func(t *testing.T, response Response) {
				if len(response.Updates) != 1 {
					t.Fatalf("updates %d, want 1", len(response.Updates))
				}
				// 周研留在旧站 is units 9..14, so the parser computes 9/15.
				if response.Updates[0].Evidence.Start != 9 || response.Updates[0].Evidence.End != 15 {
					t.Fatalf("evidence %d/%d, want 9/15", response.Updates[0].Evidence.Start, response.Updates[0].Evidence.End)
				}
			},
		},
		{
			name: "a present empty field is kept",
			body: updatesBody(`{"characterId":"character-a","currentState":{"keyItems":""},` + validEvidence + `}`),
			check: func(t *testing.T, response Response) {
				if len(response.Updates) != 1 {
					t.Fatalf("updates %d, want 1", len(response.Updates))
				}
				items, present := response.Updates[0].CurrentState.Get(FieldKeyItems)
				if !present || items != "" {
					t.Fatalf("keyItems %v/%v, want present and empty", items, present)
				}
				if response.Updates[0].CurrentState.Len() != 1 {
					t.Fatalf("fields %d, want 1", response.Updates[0].CurrentState.Len())
				}
			},
		},
		{
			name: "updatedAtChapter of this chapter",
			body: updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔","updatedAtChapter":1},` + validEvidence + `}`),
			check: func(t *testing.T, response Response) {
				if len(response.Updates) != 1 {
					t.Fatalf("updates %d, want 1", len(response.Updates))
				}
				// updatedAtChapter is checked against the context, never copied.
				if _, present := response.Updates[0].CurrentState.Get(updatedAtChapterField); present {
					t.Fatal("updatedAtChapter leaked into the values")
				}
				if response.Updates[0].CurrentState.Len() != 1 {
					t.Fatalf("fields %d, want 1", response.Updates[0].CurrentState.Len())
				}
			},
		},
		{
			name: "updatedAtChapter compared as a number",
			body: updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔","updatedAtChapter":1.0},` + validEvidence + `}`),
			check: func(t *testing.T, response Response) {
				// === compares numbers, not literals: 1.0 is the chapter number.
				if len(response.Updates) != 1 {
					t.Fatalf("updates %d, want 1", len(response.Updates))
				}
			},
		},
		{
			name: "a thousand updates are accepted",
			body: unresolvedBody(MaxUpdates),
			check: func(t *testing.T, response Response) {
				if len(response.Updates) != 0 || len(response.Unresolved) != MaxUpdates {
					t.Fatalf("updates %d unresolved %d, want 0/%d", len(response.Updates), len(response.Unresolved), MaxUpdates)
				}
			},
		},
		{
			name: "one resolved and one unresolved update",
			body: updatesBody(cleanItem, `{"name":"周研","currentState":{"location":"旧站"},`+validEvidence+`}`),
			check: func(t *testing.T, response Response) {
				if len(response.Updates) != 1 || len(response.Unresolved) != 1 {
					t.Fatalf("updates %d unresolved %d, want 1/1", len(response.Updates), len(response.Unresolved))
				}
				occurrence := response.Unresolved[0]
				// The key carries the position in the model's own list.
				if occurrence.SelectionKey != "update:1" || occurrence.Reason != ReasonNameOnly {
					t.Fatalf("occurrence %v/%v, want update:1/name-only", occurrence.SelectionKey, occurrence.Reason)
				}
				if len(occurrence.CandidateIDs) != 1 || occurrence.CandidateIDs[0] != "character-b" {
					t.Fatalf("candidates %v, want [character-b]", occurrence.CandidateIDs)
				}
			},
		},
		{
			name: "an unresolved name only occurrence",
			body: updatesBody(`{"name":"林岚","currentState":{"location":"北塔"},` + validEvidence + `}`),
			check: func(t *testing.T, response Response) {
				if len(response.Updates) != 0 || len(response.Unresolved) != 1 {
					t.Fatalf("updates %d unresolved %d, want 0/1", len(response.Updates), len(response.Unresolved))
				}
				occurrence := response.Unresolved[0]
				if occurrence.SelectionKey != "update:0" || occurrence.Reason != ReasonNameOnly {
					t.Fatalf("occurrence %v/%v, want update:0/name-only", occurrence.SelectionKey, occurrence.Reason)
				}
				if occurrence.DisplayName != "林岚" || occurrence.OriginalText != "林岚" {
					t.Fatalf("names %v/%v, want 林岚 twice", occurrence.DisplayName, occurrence.OriginalText)
				}
				if occurrence.Evidence.Start != 2 || occurrence.Evidence.End != 8 {
					t.Fatalf("evidence %d/%d, want 2/8", occurrence.Evidence.Start, occurrence.Evidence.End)
				}
				// rawValue is structuredClone(input): the update as the model wrote it.
				raw, isObject := occurrence.RawValue.(map[string]any)
				if !isObject {
					t.Fatalf("rawValue %T, want the decoded object", occurrence.RawValue)
				}
				if raw["name"] != "林岚" {
					t.Fatalf("rawValue name %v, want 林岚", raw["name"])
				}
				if _, present := raw["characterId"]; present {
					t.Fatal("rawValue gained a key the model never wrote")
				}
			},
		},
		{
			name: "an unresolved unknown id",
			body: updatesBody(`{"characterId":"character-z","currentState":{"location":"北塔"},` + validEvidence + `}`),
			check: func(t *testing.T, response Response) {
				if len(response.Updates) != 0 || len(response.Unresolved) != 1 {
					t.Fatalf("updates %d unresolved %d, want 0/1", len(response.Updates), len(response.Unresolved))
				}
				occurrence := response.Unresolved[0]
				if occurrence.Reason != ReasonUnknownID {
					t.Fatalf("reason %v, want unknown-id", occurrence.Reason)
				}
				if len(occurrence.CandidateIDs) != 0 {
					t.Fatalf("candidates %v, want none", occurrence.CandidateIDs)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response, err := ParseResponse(test.body, testContext())
			if err != nil {
				t.Fatalf("body %s refused with %v, want acceptance", test.body, err)
			}
			test.check(t, response)
		})
	}
}

// cleanBody is cleanItem inside an updates envelope.
func cleanBody() string { return updatesBody(cleanItem) }

// TestParseResponseLegacyIdentity is the identityStatus 'legacy' arm (:211,
// :222): nothing binds, so even an explicit id is an occurrence.
func TestParseResponseLegacyIdentity(t *testing.T) {
	t.Parallel()
	context := testContext()
	context.IdentityStatus = IdentityLegacy
	response, err := ParseResponse(updatesBody(`{"characterId":"character-a","name":"林岚","currentState":{"location":"北塔"},`+validEvidence+`}`), context)
	if err != nil {
		t.Fatalf("legacy body refused with %v", err)
	}
	if len(response.Updates) != 0 || len(response.Unresolved) != 1 {
		t.Fatalf("updates %d unresolved %d, want 0/1", len(response.Updates), len(response.Unresolved))
	}
	occurrence := response.Unresolved[0]
	if occurrence.Reason != ReasonLegacy {
		t.Fatalf("reason %v, want legacy", occurrence.Reason)
	}
	if len(occurrence.CandidateIDs) != 1 || occurrence.CandidateIDs[0] != "character-a" {
		t.Fatalf("candidates %v, want [character-a]", occurrence.CandidateIDs)
	}
	// A legacy context still refuses an occurrence that names nothing at all.
	refused, err := ParseResponse(updatesBody(`{"currentState":{"location":"北塔"},`+validEvidence+`}`), context)
	if err == nil {
		t.Fatalf("nameless legacy update was accepted with %d entries", len(refused.Unresolved))
	}
	if err.Error() != "FINALIZED_CHARACTER_IDENTITY_REQUIRED" {
		t.Fatalf("code %s, want FINALIZED_CHARACTER_IDENTITY_REQUIRED", err.Error())
	}
}

// TestParseResponseSharedLabel is the sharedLabel arm (:215-216): an explicit id
// cannot disambiguate two frozen characters with the same display label.
func TestParseResponseSharedLabel(t *testing.T) {
	t.Parallel()
	context := testContext()
	context.Characters = append(context.Characters, ContextCharacter{CharacterID: "character-c", DisplayNameSnapshot: "林岚"})
	response, err := ParseResponse(updatesBody(cleanItem), context)
	if err != nil {
		t.Fatalf("shared label body refused with %v", err)
	}
	if len(response.Updates) != 0 || len(response.Unresolved) != 1 {
		t.Fatalf("updates %d unresolved %d, want 0/1", len(response.Updates), len(response.Unresolved))
	}
	occurrence := response.Unresolved[0]
	if occurrence.Reason != ReasonAmbiguous {
		t.Fatalf("reason %v, want ambiguous", occurrence.Reason)
	}
	if len(occurrence.CandidateIDs) != 2 || occurrence.CandidateIDs[0] != "character-a" || occurrence.CandidateIDs[1] != "character-c" {
		t.Fatalf("candidates %v, want [character-a character-c]", occurrence.CandidateIDs)
	}
}

func TestParseResponseRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		code string
		is   error
	}{
		{
			name: "root is an array",
			body: `[]`,
			code: "FINALIZED_CHARACTER_UPDATES_INVALID",
			is:   ErrUpdatesInvalid,
		},
		{
			name: "root is a number",
			body: `42`,
			code: "FINALIZED_CHARACTER_UPDATES_INVALID",
			is:   ErrUpdatesInvalid,
		},
		{
			name: "updates missing",
			body: `{}`,
			code: "FINALIZED_CHARACTER_UPDATES_INVALID",
			is:   ErrUpdatesInvalid,
		},
		{
			name: "updates is not an array",
			body: `{"updates":{}}`,
			code: "FINALIZED_CHARACTER_UPDATES_INVALID",
			is:   ErrUpdatesInvalid,
		},
		{
			name: "a thousand and one updates",
			body: unresolvedBody(MaxUpdates + 1),
			code: "FINALIZED_CHARACTER_UPDATES_INVALID",
			is:   ErrUpdatesInvalid,
		},
		{
			name: "update is null",
			body: `{"updates":[null]}`,
			code: "FINALIZED_CHARACTER_UPDATE_INVALID",
			is:   ErrUpdateInvalid,
		},
		{
			name: "update is not an object",
			body: `{"updates":["北塔"]}`,
			code: "FINALIZED_CHARACTER_UPDATE_INVALID",
			is:   ErrUpdateInvalid,
		},
		{
			name: "state missing",
			body: updatesBody(`{"characterId":"character-a",` + validEvidence + `}`),
			code: "FINALIZED_CHARACTER_UPDATE_INVALID",
			is:   ErrUpdateInvalid,
		},
		{
			name: "state is not an object",
			body: updatesBody(`{"characterId":"character-a","currentState":"北塔",` + validEvidence + `}`),
			code: "FINALIZED_CHARACTER_UPDATE_INVALID",
			is:   ErrUpdateInvalid,
		},
		{
			name: "state is an array",
			body: updatesBody(`{"characterId":"character-a","currentState":[],` + validEvidence + `}`),
			code: "FINALIZED_CHARACTER_UPDATE_INVALID",
			is:   ErrUpdateInvalid,
		},
		{
			name: "a field the context does not freeze",
			body: updatesBody(`{"characterId":"character-a","currentState":{"mood":"肃杀"},` + validEvidence + `}`),
			code: "FINALIZED_CHARACTER_FIELD_INVALID",
			is:   ErrFieldInvalid,
		},
		{
			name: "a field value that is not a string",
			body: updatesBody(`{"characterId":"character-a","currentState":{"location":3},` + validEvidence + `}`),
			code: "FINALIZED_CHARACTER_FIELD_INVALID",
			is:   ErrFieldInvalid,
		},
		{
			name: "no value field at all",
			body: updatesBody(`{"characterId":"character-a","currentState":{},` + validEvidence + `}`),
			code: "FINALIZED_CHARACTER_FIELD_INVALID",
			is:   ErrFieldInvalid,
		},
		{
			name: "updatedAtChapter of another chapter",
			body: updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔","updatedAtChapter":2},` + validEvidence + `}`),
			code: "FINALIZED_CHARACTER_FIELD_INVALID",
			is:   ErrFieldInvalid,
		},
		{
			name: "updatedAtChapter that is not a number",
			body: updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔","updatedAtChapter":"1"},` + validEvidence + `}`),
			code: "FINALIZED_CHARACTER_FIELD_INVALID",
			is:   ErrFieldInvalid,
		},
		{
			name: "duplicate derived key",
			body: updatesBody(cleanItem, `{"characterId":"character-a","currentState":{"location":"旧站"},`+validEvidence+`}`),
			code: "FINALIZED_CHARACTER_DUPLICATE_ID",
			is:   ErrDuplicateID,
		},
		{
			name: "identity required",
			body: updatesBody(`{"name":"   ","currentState":{"location":"北塔"},` + validEvidence + `}`),
			code: "FINALIZED_CHARACTER_IDENTITY_REQUIRED",
			is:   ErrIdentityRequired,
		},
		{
			name: "malformed json body",
			body: `{"updates": [`,
			code: "FINALIZED_CHARACTER_RESPONSE_BODY_INVALID",
			is:   ErrBodyInvalid,
		},
		{
			name: "empty body",
			body: "",
			code: "FINALIZED_CHARACTER_RESPONSE_BODY_INVALID",
			is:   ErrBodyInvalid,
		},
		{
			name: "whitespace only body",
			body: " \n\t\r ",
			code: "FINALIZED_CHARACTER_RESPONSE_BODY_INVALID",
			is:   ErrBodyInvalid,
		},
		{
			name: "fence without a body",
			body: "```json\n",
			code: "FINALIZED_CHARACTER_RESPONSE_BODY_INVALID",
			is:   ErrBodyInvalid,
		},
		{
			name: "a second json value after the first",
			body: `{"updates":[]}` + "\n" + `{"updates":[]}`,
			code: "FINALIZED_CHARACTER_RESPONSE_BODY_INVALID",
			is:   ErrBodyInvalid,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response, err := ParseResponse(test.body, testContext())
			if err == nil {
				t.Fatalf("body %s was accepted, want %s", test.body, test.code)
			}
			// The reference's message is the code itself, so the Go message must be
			// that same string: no prefix, no detail.
			if err.Error() != test.code {
				t.Fatalf("code %s, want %s", err.Error(), test.code)
			}
			if !errors.Is(err, test.is) {
				t.Fatalf("errors.Is is false for %s", test.code)
			}
			// A refusal is total: nothing of the body comes back.
			if len(response.Updates) != 0 || len(response.Unresolved) != 0 {
				t.Fatalf("refusal returned %d/%d entries", len(response.Updates), len(response.Unresolved))
			}
		})
	}
}

// TestParseResponseEvidenceRefusal is the evidence arm (:186-199). The code is
// FINALIZED_CHARACTER_EVIDENCE_*, and the reference error also carries the update
// index and a reason the repair turn quotes back to the model.
func TestParseResponseEvidenceRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		code   string
		index  int
		reason string
	}{
		{
			name:   "evidence missing",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_MISSING",
			index:  0,
			reason: ReasonMissing,
		},
		{
			name:   "evidence is not an object",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":"林岚"}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_MISSING",
			index:  0,
			reason: ReasonMissing,
		},
		{
			name:   "second update without evidence",
			body:   updatesBody(cleanItem, `{"characterId":"character-b","currentState":{"location":"旧站"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_MISSING",
			index:  1,
			reason: ReasonMissing,
		},
		{
			name:   "text missing from the span",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"start":2,"end":8}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_INVALID",
			index:  0,
			reason: ReasonOffsetMismatch,
		},
		{
			name:   "text is not a string",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"start":2,"end":8,"text":7}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_INVALID",
			index:  0,
			reason: ReasonOffsetMismatch,
		},
		{
			name:   "a non-integer offset",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"start":2.5,"end":8,"text":"林岚走进北塔"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_INVALID",
			index:  0,
			reason: ReasonOffsetMismatch,
		},
		{
			name:   "an offset outside the safe integer range",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"start":9007199254740993,"end":9007199254740994,"text":"林岚走进北塔"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_INVALID",
			index:  0,
			reason: ReasonOffsetMismatch,
		},
		{
			name:   "an offset written as a string",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"start":"2","end":8,"text":"林岚走进北塔"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_INVALID",
			index:  0,
			reason: ReasonOffsetMismatch,
		},
		{
			name:   "an empty span",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"start":4,"end":4,"text":"林岚"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_INVALID",
			index:  0,
			reason: ReasonOffsetMismatch,
		},
		{
			name:   "an end beyond the prose",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"start":2,"end":99,"text":"林岚"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_INVALID",
			index:  0,
			reason: ReasonOffsetMismatch,
		},
		{
			name:   "offsets that do not hold the text",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"start":2,"end":4,"text":"周研"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_INVALID",
			index:  0,
			reason: ReasonOffsetMismatch,
		},
		{
			name:   "text that occurs twice",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"text":"。"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_NOT_UNIQUE",
			index:  0,
			reason: ReasonMultipleExactMatches,
		},
		{
			name:   "text that occurs nowhere",
			body:   updatesBody(`{"characterId":"character-a","currentState":{"location":"北塔"},"evidence":{"text":"不存在的片段"}}`),
			code:   "FINALIZED_CHARACTER_EVIDENCE_NOT_UNIQUE",
			index:  0,
			reason: ReasonNoExactMatch,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response, err := ParseResponse(test.body, testContext())
			if err == nil {
				t.Fatalf("body %s was accepted, want %s", test.body, test.code)
			}
			if err.Error() != test.code {
				t.Fatalf("code %s, want %s", err.Error(), test.code)
			}
			var evidence *EvidenceError
			if !errors.As(err, &evidence) {
				t.Fatalf("%s is not an EvidenceError", test.code)
			}
			if evidence.UpdateIndex != test.index || evidence.Reason != test.reason {
				t.Fatalf("evidence %d/%s, want %d/%s", evidence.UpdateIndex, evidence.Reason, test.index, test.reason)
			}
			if evidence.Code().Error() != test.code {
				t.Fatalf("Code %s, want %s", evidence.Code(), test.code)
			}
			if len(response.Updates) != 0 || len(response.Unresolved) != 0 {
				t.Fatalf("refusal returned %d/%d entries", len(response.Updates), len(response.Unresolved))
			}
		})
	}
}

// TestTrimAndFence pins the two replaces and the trim of :178, which must behave
// identically in both implementations:
//
//	const text = content.trim().replace(/^```(?:json)?\s*\n?/i, '').replace(/\n?```\s*$/, '')
func TestTrimAndFence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain object",
			input: "{}",
			want:  "{}",
		},
		{
			name:  "surrounding spaces",
			input: "  {}  ",
			want:  "{}",
		},
		{
			name:  "byte order mark is white space",
			input: "\ufeff{}\ufeff",
			want:  "{}",
		},
		{
			name:  "next line is not white space",
			input: "\u0085{}",
			want:  "\u0085{}",
		},
		{
			name:  "json fence",
			input: "```json\n{}",
			want:  "{}",
		},
		{
			name:  "uppercase tag and a blank line",
			input: "```JSON\n\n{}",
			want:  "{}",
		},
		{
			name:  "bare fence",
			input: "```\n{}",
			want:  "{}",
		},
		{
			name:  "tag without a line break",
			input: "```json{}",
			want:  "{}",
		},
		{
			name:  "trailing fence",
			input: "{}\n```",
			want:  "{}",
		},
		{
			name:  "only one newline goes with the fence",
			input: "{}\n\n```",
			want:  "{}\n",
		},
		{
			name:  "fence right after the object",
			input: "{}```",
			want:  "{}",
		},
		{
			name:  "both fences",
			input: "```json\n{}\n```",
			want:  "{}",
		},
		{
			name:  "trailing blank lines after the fence",
			input: "```json\n{}\n```\n\n",
			want:  "{}",
		},
		{
			name:  "fence only",
			input: "```",
			want:  "",
		},
		{
			name:  "fence that is not at the end",
			input: "{}\n``` x",
			want:  "{}\n``` x",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := stripTrailingFence(stripLeadingFence(trimJS(test.input)))
			if got != test.want {
				t.Fatalf("text %s, want %s", got, test.want)
			}
		})
	}
}
