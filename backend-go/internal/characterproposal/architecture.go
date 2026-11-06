// Package characterproposal decodes the model answers the architecture step
// stages as one character proposal.
//
// Everything here is untrusted input: the slot list the manifest prompt answers,
// one detail document per slot, and the current state each detail carries. This
// package performs no I/O, holds no database handle and never hands raw JSON on:
// ParseArchitecture answers typed Items, or one of the named refusals below, and
// each refusal names the exact shape it rejects so a caller can publish the
// reference's own answer for it.
//
// # The reference
//
//	src/shared/character-proposal-parser.ts:16-47   parseArchitectureCharacterProposal
//	src/shared/character-proposal-parser.ts:60-106  decodeCharacterIdentityManifest
//	src/shared/character-proposal-parser.ts:107-156 decodeCharacterDetails
//	src/shared/character-proposal-parser.ts:157-215 validateCharacterDetail
//	src/shared/character-proposal-parser.ts:286-330 extractSingleCompleteJsonObject
//	src/shared/draft-visible-text.ts:11-31          stripDraftThinkingTags
//
// # What differs, and why
//
// The reference is compiled from TypeScript and reads JSON through a JavaScript
// parse, so its numbers arrive as doubles, its objects keep insertion order and
// its text is measured in UTF-16 code units. This port decodes with UseNumber so
// a slot id written as 2 stays the integer 2 instead of becoming 2.0, keeps the
// reference's declared key order in the typed structs it answers with, and trims
// with Unicode whitespace instead of the JavaScript definition. The only
// behaviour that changes is derivation version 1: the reference truncates a
// description to 120 characters and a state field to 80 before it validates them,
// and this port serves derivation version 2 only, so a pre-version-2 answer whose
// text exceeds those caps is refused rather than silently shortened.
package characterproposal

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The two shapes the architecture step fixes: a slot list of three to eight
// slots, exactly one of which is the protagonist, and one detail document per
// slot that repeats the slot's own name and role.
const (
	// MinSlots and MaxSlots are the manifest's own bounds (parser :104-105).
	MinSlots = 3
	MaxSlots = 8
)

// Roles is CHARACTER_ROLES (src/shared/character-role.ts:1-6) in its declared
// order. A detail or a slot naming anything else is refused by name.
var Roles = []string{"protagonist", "antagonist", "supporting", "minor"}

// StaticFields is the eleven-field order every identity write uses, and the order
// this package answers in. It is the roster entry's fields without currentState and
// relationships (src/shared/character-roster.ts:79-92).
var StaticFields = []string{"name", "role", "gender", "age", "appearance",
	"personality", "background", "abilities", "motivation", "arc", "notes"}

// DescriptionFields is the seven free-text fields a detail carries (parser :8-10).
var DescriptionFields = []string{"appearance", "personality", "background",
	"abilities", "motivation", "arc", "notes"}

// StateTextFields is the six current-state text fields a detail must answer
// (parser :11-13). The seventh state field, updatedAtChapter, is a number.
var StateTextFields = []string{"location", "powerLevel", "physicalState",
	"mentalState", "keyItems", "recentEvents"}

// validatedFields is the order validateCharacterDetail checks (:222-228): every
// field of StaticFields except the role, which it checks on its own (:234).
var validatedFields = []string{"name", "gender", "age", "appearance", "personality",
	"background", "abilities", "motivation", "arc", "notes"}

// detailKeys is the closed key set of one raw detail entry (:33-35): the slot it
// belongs to, its eleven identity fields, and its current state.
var detailKeys = func() []string {
	keys := make([]string, 0, len(StaticFields)+2)
	keys = append(keys, "slotId")
	keys = append(keys, StaticFields...)
	return append(keys, "currentState")
}()

// slotKeys is the closed key set of one manifest slot (parser :76-77).
var slotKeys = []string{"slotId", "name", "role", "narrativeDuty", "relations"}

// relationKeys is the closed key set of one manifest relation (parser :88).
var relationKeys = []string{"targetSlotId", "relation"}

// The refusals this package raises. A caller maps each one to the reference's own
// answer: the six whose name is already a symbolic code
// (CHARACTER_PROPOSAL_KEYS_INVALID .. CHARACTER_PROPOSAL_COVERAGE_INCOMPLETE) pass
// through unchanged, and every refusal below them carries Chinese prose in the
// reference, which its controller answers with GENERATION_REQUEST_FAILED
// (electron/controllers/generation-controller.ts:101).
var (
	// ErrKeysInvalid is :12 and :19 - a blank or repeated artifact id, or a
	// repeated selection key.
	ErrKeysInvalid = errors.New("CHARACTER_PROPOSAL_KEYS_INVALID")

	// ErrEnvelopeInvalid is :24 - a detail document with a key other than entries.
	ErrEnvelopeInvalid = errors.New("CHARACTER_PROPOSAL_ENVELOPE_INVALID")

	// ErrEmptyDetails is :26 - a detail document whose entries array is empty.
	ErrEmptyDetails = errors.New("CHARACTER_PROPOSAL_EMPTY_DETAILS")

	// ErrDetailFieldInvalid is :35 - a raw detail entry that is not an object, or
	// carries a key outside detailKeys.
	ErrDetailFieldInvalid = errors.New("CHARACTER_PROPOSAL_DETAIL_FIELD_INVALID")

	// ErrSlotMismatch is :31 - a detail whose slot, name or role disagrees with the
	// manifest slot it claims.
	ErrSlotMismatch = errors.New("CHARACTER_PROPOSAL_SLOT_MISMATCH")

	// ErrCoverageIncomplete is :45 - the details do not cover the slots once each.
	ErrCoverageIncomplete = errors.New("CHARACTER_PROPOSAL_COVERAGE_INCOMPLETE")

	// ErrDocumentTruncated is :364 - an opening brace whose object never closes.
	ErrDocumentTruncated = errors.New("CHARACTER_PROPOSAL_DOCUMENT_TRUNCATED")

	// ErrDocumentMultiple is :371 - more than one complete object in one answer.
	ErrDocumentMultiple = errors.New("CHARACTER_PROPOSAL_DOCUMENT_AMBIGUOUS")

	// ErrDocumentMissing is :373 - no complete object at all.
	ErrDocumentMissing = errors.New("CHARACTER_PROPOSAL_DOCUMENT_MISSING")

	// ErrManifestSlots is :62 - the answer carries no slots array.
	ErrManifestSlots = errors.New("CHARACTER_PROPOSAL_MANIFEST_SLOTS_MISSING")

	// ErrManifestFields is :63 - the answer carries a key other than slots.
	ErrManifestFields = errors.New("CHARACTER_PROPOSAL_MANIFEST_FIELDS_UNKNOWN")

	// ErrManifestCount is :65 - fewer than MinSlots or more than MaxSlots.
	ErrManifestCount = errors.New("CHARACTER_PROPOSAL_MANIFEST_COUNT_INVALID")

	// ErrManifestSlotInvalid is :69 - one slot is not an object.
	ErrManifestSlotInvalid = errors.New("CHARACTER_PROPOSAL_MANIFEST_SLOT_INVALID")

	// ErrManifestSlotFields is :70 - one slot carries a key outside slotKeys.
	ErrManifestSlotFields = errors.New("CHARACTER_PROPOSAL_MANIFEST_SLOT_FIELDS_UNKNOWN")

	// ErrManifestRelations is :73 - one slot carries no relations array.
	ErrManifestRelations = errors.New("CHARACTER_PROPOSAL_MANIFEST_RELATIONS_INVALID")

	// ErrManifestIncomplete is :74 - one slot is missing a required field.
	ErrManifestIncomplete = errors.New("CHARACTER_PROPOSAL_MANIFEST_SLOT_INCOMPLETE")

	// ErrManifestRelation is :88-90 - one relation is not a closed object with a
	// non-empty target and relation.
	ErrManifestRelation = errors.New("CHARACTER_PROPOSAL_MANIFEST_RELATION_INVALID")

	// ErrManifestDuplicate is :96 - the same slot id twice.
	ErrManifestDuplicate = errors.New("CHARACTER_PROPOSAL_MANIFEST_DUPLICATE_SLOT")

	// ErrManifestProtagonist is :98 - no slot is the protagonist.
	ErrManifestProtagonist = errors.New("CHARACTER_PROPOSAL_MANIFEST_PROTAGONIST_MISSING")

	// ErrManifestEndpoints is :102 - a relation to an unknown slot, or to itself.
	ErrManifestEndpoints = errors.New("CHARACTER_PROPOSAL_MANIFEST_RELATION_ENDPOINT")

	// ErrDetailEntries is :113 - the detail document carries no entries array.
	ErrDetailEntries = errors.New("CHARACTER_PROPOSAL_DETAIL_ENTRIES_INVALID")

	// ErrDetailField is :158-165 - a detail field that is absent, not text, or blank
	// once trimmed.
	ErrDetailField = errors.New("CHARACTER_PROPOSAL_DETAIL_FIELD_MISSING")

	// ErrDetailRole is :166 - a role outside Roles.
	ErrDetailRole = errors.New("CHARACTER_PROPOSAL_DETAIL_ROLE_INVALID")

	// ErrDetailRelationships is :167 - a detail that answered its own relationships.
	ErrDetailRelationships = errors.New("CHARACTER_PROPOSAL_DETAIL_RELATIONSHIPS_FORBIDDEN")

	// ErrDetailState is :168-215 - a current state that is absent, not an object, or
	// carries a state field that is absent, not text, or blank.
	ErrDetailState = errors.New("CHARACTER_PROPOSAL_DETAIL_STATE_INVALID")
)

// Artifact is one settled model answer: the id the ledger knows it by and the text
// it carries.
type Artifact struct {
	// ArtifactID is generation_artifacts.artifact_id.
	ArtifactID string

	// Text is the artifact body.
	Text string
}

// Fields is CharacterStaticFields (src/shared/character-proposal.ts:9-21) in its
// declared order. Every field is filled by the parser: an architecture detail is
// refused unless all eleven are non-empty.
type Fields struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Gender      string `json:"gender"`
	Age         string `json:"age"`
	Appearance  string `json:"appearance"`
	Personality string `json:"personality"`
	Background  string `json:"background"`
	Abilities   string `json:"abilities"`
	Motivation  string `json:"motivation"`
	Arc         string `json:"arc"`
	Notes       string `json:"notes"`
}

// Relation is one manifest relation: the slot it points at and how it reads.
type Relation struct {
	TargetSlotID string `json:"targetSlotId"`
	Relation     string `json:"relation"`
}

// Slot is one decoded manifest entry. It is what rawValue.manifest keeps, so an
// author reading the proposal can see the duty the model wrote for the slot.
type Slot struct {
	SlotID        string     `json:"slotId"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	NarrativeDuty string     `json:"narrativeDuty"`
	Relations     []Relation `json:"relations"`
}

// Relationship is one CharacterProposalItem relation (character-proposal.ts:24).
// The architecture parser fills only the two slot-addressed keys; TargetName and
// Raw belong to the import and planning-material answers this package refuses.
type Relationship struct {
	TargetSelectionKey string          `json:"targetSelectionKey,omitempty"`
	TargetName         string          `json:"targetName,omitempty"`
	Relation           string          `json:"relation"`
	Raw                json.RawMessage `json:"raw,omitempty"`
}

// RawValue is the evidence each item keeps: the manifest slot and the exact detail
// entry it was decoded from. Detail is kept as the raw bytes the answer carried
// (compacted, never re-ordered), so a stored envelope round-trips byte for byte.
type RawValue struct {
	Manifest Slot            `json:"manifest"`
	Detail   json.RawMessage `json:"detail"`
}

// Item is one CharacterProposalItem without its resolution
// (src/shared/character-proposal.ts:19-27, Omit<..., 'resolution'>): the resolution
// is recomputed against the live roster every time a batch is read, so it never
// belongs to the parsed proof.
type Item struct {
	SelectionKey  string         `json:"selectionKey"`
	SourceID      string         `json:"sourceId"`
	Fields        Fields         `json:"fields"`
	Relationships []Relationship `json:"relationships"`
	RawValue      RawValue       `json:"rawValue"`
}

// ParseArchitecture decodes one manifest artifact and the detail artifacts staged
// beside it into the items a proposal carries, in manifest slot order.
//
// It refuses, by name, every shape it does not match: a document that is not one
// complete JSON object, a slot list that is not three to eight closed slots with
// one protagonist, a detail that disagrees with its slot, and a detail whose
// current state is not the complete object the roster contract requires.
func ParseArchitecture(manifest Artifact, details []Artifact) ([]Item, error) {
	keys := make([]string, 0, len(details)+1)
	keys = append(keys, manifest.ArtifactID)
	for _, detail := range details {
		keys = append(keys, detail.ArtifactID)
	}
	if err := uniqueKeys(keys); err != nil {
		return nil, err
	}
	slots, err := decodeManifest(manifest.Text)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Slot, len(slots))
	for _, slot := range slots {
		byID[slot.SlotID] = slot
	}
	records := make([]Item, 0, len(slots))
	for _, detail := range details {
		entries, err := detailEntries(detail.Text)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, ErrEmptyDetails
		}
		for _, raw := range entries {
			entry, err := decodeDetail(raw)
			if err != nil {
				return nil, err
			}
			slot, ok := byID[entry.slotID]
			if !ok || slot.Name != entry.name || slot.Role != entry.role {
				return nil, ErrSlotMismatch
			}
			if err := detailKeyAllowed(entry.object); err != nil {
				return nil, err
			}
			item := Item{
				SelectionKey:  slot.SlotID,
				SourceID:      detail.ArtifactID + ":" + slot.SlotID,
				Fields:        entry.fields,
				Relationships: make([]Relationship, 0, len(slot.Relations)),
				RawValue:      RawValue{Manifest: slot, Detail: raw},
			}
			for _, relation := range slot.Relations {
				item.Relationships = append(item.Relationships, Relationship{
					TargetSelectionKey: relation.TargetSlotID,
					Relation:           relation.Relation,
				})
			}
			records = append(records, item)
		}
	}
	selection := make([]string, 0, len(records))
	for _, record := range records {
		selection = append(selection, record.SelectionKey)
	}
	if err := uniqueKeys(selection); err != nil {
		return nil, err
	}
	if len(records) != len(slots) {
		return nil, ErrCoverageIncomplete
	}
	items := make([]Item, 0, len(slots))
	for _, slot := range slots {
		matched := false
		for _, record := range records {
			if record.SelectionKey == slot.SlotID {
				items = append(items, record)
				matched = true
				break
			}
		}
		if !matched {
			return nil, ErrCoverageIncomplete
		}
	}
	return items, nil
}

// uniqueKeys is :11-13: every key non-blank after trimming, and no key twice.
func uniqueKeys(keys []string) error {
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || seen[key] {
			return ErrKeysInvalid
		}
		seen[key] = true
	}
	return nil
}

// decodeManifest is :60-106.
func decodeManifest(content string) ([]Slot, error) {
	text, err := extractSingleCompleteObject(content)
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return nil, ErrManifestSlots
	}
	raw, present := root["slots"]
	if !present {
		return nil, ErrManifestSlots
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, ErrManifestSlots
	}
	for key := range root {
		if key != "slots" {
			return nil, ErrManifestFields
		}
	}
	if len(entries) < MinSlots || len(entries) > MaxSlots {
		return nil, ErrManifestCount
	}
	slots := make([]Slot, 0, len(entries))
	for _, entry := range entries {
		object, err := jsonObject(entry)
		if err != nil {
			return nil, ErrManifestSlotInvalid
		}
		for key := range object {
			if !contains(slotKeys, key) {
				return nil, ErrManifestSlotFields
			}
		}
		relations, ok := object["relations"].([]any)
		if !ok {
			return nil, ErrManifestRelations
		}
		slotID := slotIdentifier(object["slotId"])
		name := textValue(object["name"])
		role := textValue(object["role"])
		duty := textValue(object["narrativeDuty"])
		if slotID == "" || name == "" || role == "" || !contains(Roles, role) || duty == "" {
			return nil, ErrManifestIncomplete
		}
		slot := Slot{SlotID: slotID, Name: name, Role: role, NarrativeDuty: duty,
			Relations: make([]Relation, 0, len(relations))}
		for _, value := range relations {
			relation, ok := value.(map[string]any)
			if !ok {
				return nil, ErrManifestRelation
			}
			for key := range relation {
				if !contains(relationKeys, key) {
					return nil, ErrManifestRelation
				}
			}
			target := slotIdentifier(relation["targetSlotId"])
			read := textValue(relation["relation"])
			if target == "" || read == "" {
				return nil, ErrManifestRelation
			}
			slot.Relations = append(slot.Relations, Relation{TargetSlotID: target, Relation: read})
		}
		slots = append(slots, slot)
	}
	seen := make(map[string]bool, len(slots))
	protagonist := false
	for _, slot := range slots {
		if seen[slot.SlotID] {
			return nil, ErrManifestDuplicate
		}
		seen[slot.SlotID] = true
		if slot.Role == "protagonist" {
			protagonist = true
		}
	}
	if !protagonist {
		return nil, ErrManifestProtagonist
	}
	for _, slot := range slots {
		for _, relation := range slot.Relations {
			if relation.TargetSlotID == slot.SlotID || !seen[relation.TargetSlotID] {
				return nil, ErrManifestEndpoints
			}
		}
	}
	return slots, nil
}

// detailEntries is :22-25: one single complete object whose only key is entries,
// answered as the raw bytes of each entry in document order.
func detailEntries(content string) ([]json.RawMessage, error) {
	text, err := extractSingleCompleteObject(content)
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return nil, ErrEnvelopeInvalid
	}
	raw, present := root["entries"]
	if !present {
		return nil, ErrDetailEntries
	}
	for key := range root {
		if key != "entries" {
			return nil, ErrEnvelopeInvalid
		}
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, ErrDetailEntries
	}
	return entries, nil
}

// detail is one decoded detail entry: the three fields the manifest has to agree
// with, the eleven static fields, and the raw object the key allowlist runs over.
type detail struct {
	slotID string
	name   string
	role   string
	fields Fields
	object map[string]any
}

// decodeDetail is :107-156 followed by :157-215: normalize the entry the way the
// reference rebuilds it, validate it, and answer the typed fields.
func decodeDetail(raw json.RawMessage) (detail, error) {
	object, err := jsonObject(raw)
	if err != nil {
		return detail{}, ErrDetailField
	}
	entry := normalizeDetail(object)
	slotID := slotIdentifier(entry["slotId"])
	if slotID == "" {
		return detail{}, ErrDetailField
	}
	for _, key := range validatedFields {
		if textValue(entry[key]) == "" {
			return detail{}, ErrDetailField
		}
	}
	role := textValue(entry["role"])
	if !contains(Roles, role) {
		return detail{}, ErrDetailRole
	}
	if _, present := entry["relationships"]; present {
		return detail{}, ErrDetailRelationships
	}
	state, ok := entry["currentState"].(map[string]any)
	if !ok {
		return detail{}, ErrDetailState
	}
	for _, key := range StateTextFields {
		if textValue(state[key]) == "" {
			return detail{}, ErrDetailState
		}
	}
	return detail{slotID: slotID, name: textValue(entry["name"]), role: role,
		fields: Fields{
			Name:        textValue(entry["name"]),
			Role:        role,
			Gender:      textValue(entry["gender"]),
			Age:         textValue(entry["age"]),
			Appearance:  textValue(entry["appearance"]),
			Personality: textValue(entry["personality"]),
			Background:  textValue(entry["background"]),
			Abilities:   textValue(entry["abilities"]),
			Motivation:  textValue(entry["motivation"]),
			Arc:         textValue(entry["arc"]),
			Notes:       textValue(entry["notes"]),
		}, object: object}, nil
}

// normalizeDetail is :107-156: trim every description field, fold the two list
// state fields into the separators the roster uses, pin updatedAtChapter to zero
// (the reference rebuilds the state rather than trusting the model's chapter), and
// turn a numeric age into the text the roster stores.
func normalizeDetail(object map[string]any) map[string]any {
	out := make(map[string]any, len(object)+len(DescriptionFields))
	for key, value := range object {
		out[key] = value
	}
	for _, key := range DescriptionFields {
		out[key] = boundedText(out[key])
	}
	if state, ok := out["currentState"].(map[string]any); ok {
		normalized := make(map[string]any, len(state)+len(StateTextFields))
		for key, value := range state {
			normalized[key] = value
		}
		normalized["keyItems"] = textList(state["keyItems"], "、")
		normalized["recentEvents"] = textList(state["recentEvents"], "；")
		normalized["updatedAtChapter"] = json.Number("0")
		for _, key := range StateTextFields {
			normalized[key] = boundedText(normalized[key])
		}
		out["currentState"] = normalized
	}
	if number, ok := out["age"].(json.Number); ok {
		if value, err := number.Float64(); err == nil && !math.IsInf(value, 0) && !math.IsNaN(value) {
			out["age"] = strconv.FormatFloat(value, 'g', -1, 64)
		}
	}
	return out
}

// detailKeyAllowed is :33-35: the raw entry may carry only detailKeys.
func detailKeyAllowed(object map[string]any) error {
	for key := range object {
		if !contains(detailKeys, key) {
			return ErrDetailFieldInvalid
		}
	}
	return nil
}

// extractSingleCompleteObject is :355-375: the one complete top-level object the
// answer carries. Thinking tags are stripped first, a brace that never closes is a
// truncated document, and two complete objects are ambiguous.
func extractSingleCompleteObject(content string) (string, error) {
	source := strings.TrimSpace(stripThinkingTags(content))
	candidates := make([]string, 0, 2)
	for offset := 0; offset < len(source); {
		open := strings.IndexByte(source[offset:], '{')
		if open < 0 {
			break
		}
		start := offset + open
		end := closingBrace(source, start)
		if end < 0 {
			return "", ErrDocumentTruncated
		}
		candidate := source[start : end+1]
		var parsed any
		if err := json.Unmarshal([]byte(candidate), &parsed); err == nil {
			if _, ok := parsed.(map[string]any); ok {
				candidates = append(candidates, candidate)
			}
		}
		offset = end + 1
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", ErrDocumentMissing
	default:
		return "", ErrDocumentMultiple
	}
}

// closingBrace is :334-353: the index of the brace that closes the object opening
// at start, or -1 when the text ends first. String contents and backslash escapes
// are skipped, because a brace inside a model's prose is not structure.
func closingBrace(text string, start int) int {
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(text); index++ {
		character := text[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == '"' {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

var (
	// pairedThinking is /<think>[\s\S]*?(?:<\/think>|$)/gi.
	pairedThinking = regexp.MustCompile(`(?is)<think>.*?(?:</think>|$)`)

	// orphanThinking is /<\/?think>/gi.
	orphanThinking = regexp.MustCompile(`(?i)</?think>`)

	// closingThinking is /<\/think>/i.
	closingThinking = regexp.MustCompile(`(?i)</think>`)

	// reasoningPrefix is /^\s*(?:思考|推理|分析|reasoning|analysis)/iu.
	reasoningPrefix = regexp.MustCompile(`(?i)^\s*(?:思考|推理|分析|reasoning|analysis)`)

	// structuredPrefix is /^\s*(?:```(?:json)?\s*)?[{[]/iu.
	structuredPrefix = regexp.MustCompile("(?i)^\\s*(?:```(?:json)?\\s*)?[\\{\\[]")
)

// stripThinkingTags is src/shared/draft-visible-text.ts:11-31: drop a paired
// reasoning block, and treat a lone closing tag as the end of hidden reasoning only
// when its prefix identifies itself as reasoning or the text after it is structured
// output - otherwise the prefix is the author-visible answer and stays.
func stripThinkingTags(text string) string {
	if text == "" {
		return text
	}
	withoutPaired := pairedThinking.ReplaceAllString(text, "")
	closing := closingThinking.FindStringIndex(withoutPaired)
	if closing == nil {
		return strings.TrimSpace(orphanThinking.ReplaceAllString(withoutPaired, ""))
	}
	hiddenPrefix := withoutPaired[:closing[0]]
	visibleSuffix := withoutPaired[closing[1]:]
	cleaned := hiddenPrefix + visibleSuffix
	if reasoningPrefix.MatchString(hiddenPrefix) || structuredPrefix.MatchString(visibleSuffix) {
		cleaned = visibleSuffix
	}
	return strings.TrimSpace(orphanThinking.ReplaceAllString(cleaned, ""))
}

// boundedText is normalizeBoundedDetailText: a string is trimmed, and anything
// else is left exactly as the model wrote it so validation can refuse it.
func boundedText(value any) any {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return value
}

// textList is normalizeDetailStringList: one trimmed string stays, a list of
// non-blank strings is joined with separator, and any other shape is left intact so
// validation refuses it.
func textList(value any, separator string) any {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return value
	}
	joined := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return value
		}
		joined = append(joined, strings.TrimSpace(text))
	}
	return strings.Join(joined, separator)
}

// textValue is the reference's `typeof value === 'string' ? value.trim() : ''`.
func textValue(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// slotIdentifier is :81-84: a trimmed non-blank string, or a safe non-negative
// integer as its decimal text. Anything else is the empty string, which every
// caller refuses.
func slotIdentifier(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	number, ok := value.(json.Number)
	if !ok {
		return ""
	}
	if integer, err := number.Int64(); err == nil {
		if integer < 0 || integer > 9007199254740991 {
			return ""
		}
		return strconv.FormatInt(integer, 10)
	}
	value64, err := number.Float64()
	if err != nil || math.IsInf(value64, 0) || math.IsNaN(value64) || value64 < 0 ||
		value64 > 9007199254740991 || value64 != math.Trunc(value64) {
		return ""
	}
	return strconv.FormatFloat(value64, 'f', -1, 64)
}

// jsonObject decodes one JSON value with number fidelity kept, refusing anything
// that is not an object.
func jsonObject(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrDocumentMissing
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrDocumentMissing
	}
	return object, nil
}

// contains reports whether values holds value.
func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// SortedRefusals returns this package's refusals sorted by message, so a caller can
// prove its mapping table covers every one of them.
func SortedRefusals() []error {
	refusals := []error{ErrKeysInvalid, ErrEnvelopeInvalid, ErrEmptyDetails,
		ErrDetailFieldInvalid, ErrSlotMismatch, ErrCoverageIncomplete,
		ErrDocumentTruncated, ErrDocumentMultiple, ErrDocumentMissing,
		ErrManifestSlots, ErrManifestFields, ErrManifestCount, ErrManifestSlotInvalid,
		ErrManifestSlotFields, ErrManifestRelations, ErrManifestIncomplete,
		ErrManifestRelation, ErrManifestDuplicate, ErrManifestProtagonist,
		ErrManifestEndpoints, ErrDetailEntries, ErrDetailField, ErrDetailRole,
		ErrDetailRelationships, ErrDetailState}
	sorted := make([]error, len(refusals))
	copy(sorted, refusals)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left].Error() < sorted[right].Error() })
	return sorted
}
