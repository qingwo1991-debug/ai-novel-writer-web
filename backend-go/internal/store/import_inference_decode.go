package store

// Native port of the import-inference decode the raised `generated` arm of the
// character-proposal import proof needs.
//
// Reference chain:
//
//	src/services/workflows/commands/import-inference-contract.ts
//	  :82-96   trimEdgeWrapperResidue + normalizeImportInferenceJsonContent
//	  :98-125  findCompleteJsonObjectEnd
//	  :127-157 extractSingleCompleteJsonObject + parseImportInferenceJsonObject
//	  :159-198 record/required/text/age/currentState
//	  :200-255 decodeCards
//	  :257-289 decodeImportInferenceJson
//	  :323-332 importInferenceCards
//	  :350-370 unresolvedImportRelationshipTargets
//	  :372-432 parseImportEndpointCorrectionDelta
//	  :434-443 decodeImportInferenceWithEndpointDelta
//	src/services/workflows/structured-syntax-repair.ts:109-183
//	  lexicalEvidence + preservesStructuredJsonEvidence
//	electron/services/generation-character-proposal-proof.ts:16-41
//	  importCharacterEntriesFromArtifacts, the reader that composes them
//
// # Where it is used
//
// pendingFinalizedImportProof (character_proposal_pending.go) is
// generation-character-proposal-proof.ts:67-103. Its last arm compares the
// receipt's frozen characterEntries with the cards the run's settled
// import-inference artifacts decode to (:40, :97-98). Until this file
// existed, the decoder had no Go owner, so a receipt carrying a
// generationRunHandle was refused with CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE
// rather than guessed at. The port makes the arm answer what the reference
// answers:
//
//   - the decoded cards equal the receipt's entries, so the provenance is
//     `generated` with the run's modelRevision (:99-102);
//   - the run does not decode - no single inference, a doubled repair, a
//     repair that changed scalar evidence, a decode the contract rejects - so the
//     provenance is `null`, which is the reference's `!generated` arm (:98);
//   - a receipt whose artifact, attempt, result or text hash does not hold up
//     raises the reference's own CHARACTER_PROPOSAL_SOURCE_INVALID (:26-28).
//
// # Fidelity
//
// Every value is parsed with this package's JS-compatible decoder (glParse) and
// re-emitted with its JSON.stringify-compatible serializer (plotJSON), so the
// comparison the proof makes is the reference's isDeepStrictEqual over the same
// bytes. Two deliberate narrowings, both conservative (they can only turn a
// `generated` provenance into the reference's `null`, never invent one):
// glParse refuses lone UTF-16 surrogates and invalid UTF-8, which JSON.parse
// would accept, and plotJSON refuses a non-finite number where JSON.stringify
// would write null. Both are inputs no import writer this lane stages can
// produce.

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

// errIIContractInvalid is the one refusal every parse step here raises. The
// reference raises a StructuredContractDiagnostic per invalid path
// (import-inference-contract.ts:149-157), and its caller catches all of them
// alike (generation-character-proposal-proof.ts:38-40), so the decoded value is
// what survives and the diagnostic path is never part of an answer. It is
// symbolic and never reaches the wire: pendingFinalizedImportGeneratedEntries
// turns it into the reference's own null.
var errIIContractInvalid = errors.New("IMPORT_INFERENCE_CONTRACT_INVALID")

// iiCharacterRoles is CHARACTER_ROSTER_ROLES (character-role.ts:1-6).
var iiCharacterRoles = []string{"protagonist", "antagonist", "supporting", "minor"}

// iiEndpointCardKeys, iiEndpointCurrentStateKeys and iiEndpointRelationshipKeys
// are the exact key sets the bounded endpoint correction may carry
// (import-inference-contract.ts:293-317).
var (
	iiEndpointCardKeys = []string{
		"abilities", "age", "appearance", "arc", "background", "currentState",
		"gender", "motivation", "name", "notes", "personality", "relationships", "role",
	}
	iiEndpointCurrentStateKeys = []string{
		"keyItems", "location", "mentalState", "physicalState", "powerLevel",
		"recentEvents", "updatedAtChapter",
	}
	iiEndpointRelationshipKeys = []string{"relation", "target"}
)

// iiEdgeResidue is EDGE_INVISIBLE_WRAPPER_RESIDUE (:22), the invisible
// wrapper bytes trimEdgeWrapperResidue strips beside the WhiteSpace set.
func iiEdgeResidue(r rune) bool {
	switch r {
	case '\uFEFF', '\u200B', '\u200C', '\u200D', '\u2060':
		return true
	}
	return false
}

// iiTrimEdgeWrapperResidue is trimEdgeWrapperResidue (:82-92): leading and
// trailing JavaScript WhiteSpace and invisible wrapper residue.
func iiTrimEdgeWrapperResidue(content string) string {
	start, end := 0, len(content)
	for start < end {
		r, size := utf8.DecodeRuneInString(content[start:])
		if !pfSpace(r) && !iiEdgeResidue(r) {
			break
		}
		start += size
	}
	for end > start {
		r, size := utf8.DecodeLastRuneInString(content[:end])
		if !pfSpace(r) && !iiEdgeResidue(r) {
			break
		}
		end -= size
	}
	return content[start:end]
}

// iiFindCompleteJSONObjectEnd is findCompleteJsonObjectEnd (:98-125): the index
// of the '}' that closes the object opening at start, counting braces outside
// strings, or false when the object never closes.
func iiFindCompleteJSONObjectEnd(source string, start int) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(source); index++ {
		character := source[index]
		if inString {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		if character == '"' {
			inString = true
		} else if character == '{' {
			depth++
		} else if character == '}' {
			depth--
			if depth == 0 {
				return index, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}

// iiExtractSingleCompleteJSONObject is extractSingleCompleteJsonObject
// (:127-147): the one complete JSON object the wrapper residue surrounds. No
// object, an unclosed object or more than one object is a refusal, and so is a
// candidate that parses to something other than an object.
func iiExtractSingleCompleteJSONObject(source string) (string, error) {
	candidates := []string{}
	searchFrom := 0
	for searchFrom < len(source) {
		offset := strings.IndexByte(source[searchFrom:], '{')
		if offset < 0 {
			break
		}
		start := searchFrom + offset
		end, ok := iiFindCompleteJSONObjectEnd(source, start)
		if !ok {
			return "", errIIContractInvalid
		}
		candidate := source[start : end+1]
		value, err := glParse(candidate)
		if err != nil || pfObject(value) == nil {
			return "", errIIContractInvalid
		}
		candidates = append(candidates, candidate)
		searchFrom = end + 1
	}
	if len(candidates) != 1 {
		return "", errIIContractInvalid
	}
	return candidates[0], nil
}

// iiParseImportInferenceJSONObject is parseImportInferenceJsonObject
// (:149-157): normalize the wrapper and parse the one object it surrounds.
func iiParseImportInferenceJSONObject(content string) (plotObject, error) {
	normalized, err := iiExtractSingleCompleteJSONObject(iiTrimEdgeWrapperResidue(content))
	if err != nil {
		return nil, errIIContractInvalid
	}
	value, err := glParse(normalized)
	if err != nil {
		return nil, errIIContractInvalid
	}
	root := pfObject(value)
	if root == nil {
		return nil, errIIContractInvalid
	}
	return root, nil
}

// iiRecord is record(value, path) (:159-164): an object, never an array, never
// null.
func iiRecord(value any) (plotObject, error) {
	object := pfObject(value)
	if object == nil {
		return nil, errIIContractInvalid
	}
	return object, nil
}

// iiRequired is required(value, field, path) (:166-169): the field must be an
// own key of the object, even when its value is null.
func iiRequired(object plotObject, field string) (any, error) {
	value, present := pfField(object, field)
	if !present {
		return nil, errIIContractInvalid
	}
	return value, nil
}

// iiText is text(value, path) (:171-176): a string, trimmed, non-empty.
func iiText(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", errIIContractInvalid
	}
	trimmed := pfTrim(text)
	if trimmed == "" {
		return "", errIIContractInvalid
	}
	return trimmed, nil
}

// iiAge is age(value, path) (:178-181): a finite number is stringified the way
// JavaScript String(number) does; anything else goes through text().
func iiAge(value any) (string, error) {
	if number, ok := phNumber(value); ok {
		return phNumberString(number), nil
	}
	return iiText(value)
}

// iiCurrentState is currentState(value, path) (:183-198).
func iiCurrentState(value any) (plotObject, error) {
	state, err := iiRecord(value)
	if err != nil {
		return nil, err
	}
	updatedValue, err := iiRequired(state, "updatedAtChapter")
	if err != nil {
		return nil, err
	}
	updated, ok := rrInteger(updatedValue, 0)
	if !ok {
		return nil, errIIContractInvalid
	}
	state_ := plotObject{}
	for _, field := range characterStateFields {
		raw, err := iiRequired(state, field)
		if err != nil {
			return nil, err
		}
		text, err := iiText(raw)
		if err != nil {
			return nil, err
		}
		state_ = append(state_, plotField{field, text})
	}
	return append(state_, plotField{"updatedAtChapter", updated}), nil
}

// iiDecodeCards is decodeCards (:200-255): the card array, its 3-8 bound, the
// closed role vocabulary, the required fields in the reference's own key order,
// and the two relationship rules - no self-reference for a unique name, and
// every target inside the name set.
func iiDecodeCards(value any) ([]any, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, errIIContractInvalid
	}
	if len(list) < 3 || len(list) > 8 {
		return nil, errIIContractInvalid
	}
	cards := make([]any, 0, len(list))
	for _, raw := range list {
		card, err := iiRecord(raw)
		if err != nil {
			return nil, err
		}
		nameValue, err := iiRequired(card, "name")
		if err != nil {
			return nil, err
		}
		name, err := iiText(nameValue)
		if err != nil {
			return nil, err
		}
		roleValue, err := iiRequired(card, "role")
		if err != nil {
			return nil, err
		}
		role, ok := roleValue.(string)
		if !ok || !iiRoleAllowed(role) {
			return nil, errIIContractInvalid
		}
		relationshipsValue, err := iiRequired(card, "relationships")
		if err != nil {
			return nil, err
		}
		rawRelationships, ok := relationshipsValue.([]any)
		if !ok {
			return nil, errIIContractInvalid
		}
		relationships := make([]any, 0, len(rawRelationships))
		for _, rawRelationship := range rawRelationships {
			relationship, err := iiRecord(rawRelationship)
			if err != nil {
				return nil, err
			}
			targetValue, err := iiRequired(relationship, "target")
			if err != nil {
				return nil, err
			}
			target, err := iiText(targetValue)
			if err != nil {
				return nil, err
			}
			relationValue, err := iiRequired(relationship, "relation")
			if err != nil {
				return nil, err
			}
			relation, err := iiText(relationValue)
			if err != nil {
				return nil, err
			}
			relationships = append(relationships, plotObject{{"target", target}, {"relation", relation}})
		}
		// The reference's own literal key order: name, role, gender, age,
		// appearance, personality, background, abilities, motivation,
		// relationships, arc, notes, currentState.
		built := plotObject{{"name", name}, {"role", role}}
		for _, field := range []string{"gender", "age", "appearance", "personality", "background", "abilities", "motivation"} {
			raw, err := iiRequired(card, field)
			if err != nil {
				return nil, err
			}
			text := ""
			if field == "age" {
				text, err = iiAge(raw)
			} else {
				text, err = iiText(raw)
			}
			if err != nil {
				return nil, err
			}
			built = append(built, plotField{field, text})
		}
		built = append(built, plotField{"relationships", relationships})
		for _, field := range []string{"arc", "notes"} {
			raw, err := iiRequired(card, field)
			if err != nil {
				return nil, err
			}
			text, err := iiText(raw)
			if err != nil {
				return nil, err
			}
			built = append(built, plotField{field, text})
		}
		stateValue, err := iiRequired(card, "currentState")
		if err != nil {
			return nil, err
		}
		state, err := iiCurrentState(stateValue)
		if err != nil {
			return nil, err
		}
		built = append(built, plotField{"currentState", state})
		cards = append(cards, built)
	}
	// :236-240 at least one protagonist.
	protagonist := false
	counts := map[string]int{}
	for _, raw := range cards {
		card := pfObject(raw)
		if pfText(card, "role") == "protagonist" {
			protagonist = true
		}
		counts[pfText(card, "name")]++
	}
	if !protagonist {
		return nil, errIIContractInvalid
	}
	// :241-252 a unique name may not relate to itself, and every target is a
	// name in this same set. The name set is the label set: two occurrences may
	// share a label, which is why the count decides the self-reference rule.
	names := map[string]bool{}
	for name := range counts {
		names[name] = true
	}
	for _, raw := range cards {
		card := pfObject(raw)
		name := pfText(card, "name")
		relationships, _ := pfArray(card, "relationships")
		for _, rawRelationship := range relationships {
			relationship := pfObject(rawRelationship)
			target := pfText(relationship, "target")
			if target == name && counts[name] == 1 {
				return nil, errIIContractInvalid
			}
			if !names[target] {
				return nil, errIIContractInvalid
			}
		}
	}
	return cards, nil
}

// iiRoleAllowed is CHARACTER_ROSTER_ROLES.includes(role).
func iiRoleAllowed(role string) bool {
	for _, allowed := range iiCharacterRoles {
		if role == allowed {
			return true
		}
	}
	return false
}

// iiDecodeImportInferenceJSON is decodeImportInferenceJson (:257-289). It
// answers the characterCards the contract froze; the novelConfig and
// architectureFiles half is validated because the reference validates it, and a
// malformed config is one of the ways a decode is refused.
func iiDecodeImportInferenceJSON(content string) ([]any, error) {
	root, err := iiParseImportInferenceJSONObject(content)
	if err != nil {
		return nil, err
	}
	configValue, err := iiRequired(root, "novelConfig")
	if err != nil {
		return nil, err
	}
	config, err := iiRecord(configValue)
	if err != nil {
		return nil, err
	}
	architectureValue, err := iiRequired(root, "architectureFiles")
	if err != nil {
		return nil, err
	}
	architecture, err := iiRecord(architectureValue)
	if err != nil {
		return nil, err
	}
	plotValue, err := iiRequired(config, "plotStructure")
	if err != nil {
		return nil, err
	}
	plotStructure, ok := plotValue.(string)
	if !ok || !iiOneOf(plotStructure, iiPlotStructures) {
		return nil, errIIContractInvalid
	}
	povValue, err := iiRequired(config, "narrativePOV")
	if err != nil {
		return nil, err
	}
	narrativePOV, ok := povValue.(string)
	if !ok || !iiOneOf(narrativePOV, iiNarrativePOVs) {
		return nil, errIIContractInvalid
	}
	// :261-287 every novelConfig field and every architectureFiles field is a
	// non-empty trimmed string, in the reference's own order.
	for _, field := range []string{
		"genre", "subGenre", "targetAudience", "coreOutline", "worldSetting",
		"goldenFinger", "protagonistProfile", "globalGuidance",
	} {
		value, err := iiRequired(config, field)
		if err != nil {
			return nil, err
		}
		if _, err := iiText(value); err != nil {
			return nil, err
		}
	}
	for _, field := range []string{"premise", "worldbuilding", "synopsis"} {
		value, err := iiRequired(architecture, field)
		if err != nil {
			return nil, err
		}
		if _, err := iiText(value); err != nil {
			return nil, err
		}
	}
	cardsValue, err := iiRequired(root, "characterCards")
	if err != nil {
		return nil, err
	}
	return iiDecodeCards(cardsValue)
}

// iiPlotStructures and iiNarrativePOVs are the two closed enumerations
// (:20-21).
var (
	iiPlotStructures = []string{"three_act", "heros_journey", "save_the_cat", "kishotenketsu", "multi_thread", "freeform"}
	iiNarrativePOVs  = []string{"third_limited", "first_person", "third_omniscient", "multi_pov"}
)

func iiOneOf(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// iiImportInferenceCards is importInferenceCards (:323-332): the raw cards the
// bounded corrections compare, which are only required to be an array of
// objects - the contract's own field rules belong to decodeCards.
func iiImportInferenceCards(root plotObject) ([]any, error) {
	value, present := pfField(root, "characterCards")
	if !present {
		return nil, errIIContractInvalid
	}
	cards, ok := value.([]any)
	if !ok {
		return nil, errIIContractInvalid
	}
	for _, raw := range cards {
		if pfObject(raw) == nil {
			return nil, errIIContractInvalid
		}
	}
	return cards, nil
}

// iiUnresolvedImportRelationshipTargets is
// unresolvedImportRelationshipTargets (:350-370): the targets no card's name
// closes, in first-seen order. No unresolved target is a refusal, because a
// correction exists only to close one.
func iiUnresolvedImportRelationshipTargets(root plotObject) ([]string, error) {
	cards, err := iiImportInferenceCards(root)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, raw := range cards {
		card := pfObject(raw)
		if name, ok := pfValue(card, "name").(string); ok {
			names[name] = true
		}
	}
	unresolved := []string{}
	seen := map[string]bool{}
	for _, raw := range cards {
		card := pfObject(raw)
		cardName, hasCardName := pfValue(card, "name").(string)
		relationships, ok := pfValue(card, "relationships").([]any)
		if !ok {
			continue
		}
		for _, rawRelationship := range relationships {
			relationship := pfObject(rawRelationship)
			if relationship == nil {
				continue
			}
			target, ok := pfValue(relationship, "target").(string)
			if !ok {
				continue
			}
			if hasCardName && target == cardName {
				continue
			}
			if names[target] || seen[target] {
				continue
			}
			seen[target] = true
			unresolved = append(unresolved, target)
		}
	}
	if len(unresolved) == 0 {
		return nil, errIIContractInvalid
	}
	return unresolved, nil
}

// iiAssertExactKeys is assertExactImportEndpointDeltaKeys (:334-347): the
// object's own keys, sorted, are exactly the expected set.
func iiAssertExactKeys(value plotObject, expected []string) error {
	actual := make([]string, 0, len(value))
	for _, field := range value {
		actual = append(actual, field.key)
	}
	sort.Strings(actual)
	wanted := append([]string{}, expected...)
	sort.Strings(wanted)
	if len(actual) != len(wanted) {
		return errIIContractInvalid
	}
	for index, key := range actual {
		if key != wanted[index] {
			return errIIContractInvalid
		}
	}
	return nil
}

// iiParseEndpointCorrectionDelta is parseImportEndpointCorrectionDelta
// (:372-432): the delta may carry characterCards and nothing else, one card per
// unresolved target, each card keyed exactly as the delta vocabulary says, and
// the added names exactly close the unresolved set.
func iiParseEndpointCorrectionDelta(content string, unresolved []string) ([]any, error) {
	root, err := iiParseImportInferenceJSONObject(content)
	if err != nil {
		return nil, err
	}
	if err := iiAssertExactKeys(root, []string{"characterCards"}); err != nil {
		return nil, err
	}
	cards, err := iiImportInferenceCards(root)
	if err != nil {
		return nil, err
	}
	if len(cards) != len(unresolved) {
		return nil, errIIContractInvalid
	}
	addedNames := make([]string, 0, len(cards))
	for _, raw := range cards {
		card := pfObject(raw)
		if err := iiAssertExactKeys(card, iiEndpointCardKeys); err != nil {
			return nil, err
		}
		if state := pfObject(pfValue(card, "currentState")); state != nil {
			if err := iiAssertExactKeys(state, iiEndpointCurrentStateKeys); err != nil {
				return nil, err
			}
		}
		if relationships, ok := pfValue(card, "relationships").([]any); ok {
			for _, rawRelationship := range relationships {
				relationship := pfObject(rawRelationship)
				if relationship == nil {
					continue
				}
				if err := iiAssertExactKeys(relationship, iiEndpointRelationshipKeys); err != nil {
					return nil, err
				}
			}
		}
		name, ok := pfValue(card, "name").(string)
		if !ok {
			return nil, errIIContractInvalid
		}
		addedNames = append(addedNames, name)
	}
	expected := map[string]bool{}
	for _, target := range unresolved {
		expected[target] = true
	}
	seen := map[string]bool{}
	for _, name := range addedNames {
		if !expected[name] || seen[name] {
			return nil, errIIContractInvalid
		}
		seen[name] = true
	}
	if len(addedNames) != len(expected) {
		return nil, errIIContractInvalid
	}
	return cards, nil
}

// iiDecodeImportInferenceWithEndpointDelta is
// decodeImportInferenceWithEndpointDelta (:434-443): the original cards plus the
// correction's cards, decoded by the same contract. The English branch of the
// reference's UiText is what this port would raise, and every error it raises is
// caught by the caller alike, so no locale text is carried.
func iiDecodeImportInferenceWithEndpointDelta(original, delta string) ([]any, error) {
	root, err := iiParseImportInferenceJSONObject(original)
	if err != nil {
		return nil, err
	}
	originalCards, err := iiImportInferenceCards(root)
	if err != nil {
		return nil, err
	}
	targets, err := iiUnresolvedImportRelationshipTargets(root)
	if err != nil {
		return nil, err
	}
	deltaCards, err := iiParseEndpointCorrectionDelta(delta, targets)
	if err != nil {
		return nil, err
	}
	merged := make([]any, 0, len(originalCards)+len(deltaCards))
	merged = append(merged, originalCards...)
	merged = append(merged, deltaCards...)
	encoded, err := plotJSON(pfReplace(root, "characterCards", merged))
	if err != nil {
		return nil, errIIContractInvalid
	}
	return iiDecodeImportInferenceJSON(encoded)
}

// iiScalarEvidence is lexicalEvidence's return (structured-syntax-repair.ts:104-107).
type iiScalarEvidence struct {
	scalars    []string
	containers string
}

// iiNumberPrefix is the JSON number production of lexicalEvidence
// (structured-syntax-repair.ts:155), leftmost-longest as the regular expression
// is anchored at the current index.
func iiNumberPrefix(rest string) string {
	index := 0
	if index < len(rest) && rest[index] == '-' {
		index++
	}
	if index >= len(rest) {
		return ""
	}
	switch {
	case rest[index] == '0':
		index++
	case rest[index] >= '1' && rest[index] <= '9':
		for index < len(rest) && rest[index] >= '0' && rest[index] <= '9' {
			index++
		}
	default:
		return ""
	}
	if index+1 < len(rest) && rest[index] == '.' && rest[index+1] >= '0' && rest[index+1] <= '9' {
		index += 2
		for index < len(rest) && rest[index] >= '0' && rest[index] <= '9' {
			index++
		}
	}
	if index < len(rest) && (rest[index] == 'e' || rest[index] == 'E') {
		exponent := index + 1
		if exponent < len(rest) && (rest[exponent] == '+' || rest[exponent] == '-') {
			exponent++
		}
		if exponent < len(rest) && rest[exponent] >= '0' && rest[exponent] <= '9' {
			for exponent < len(rest) && rest[exponent] >= '0' && rest[exponent] <= '9' {
				exponent++
			}
			index = exponent
		}
	}
	return rest[:index]
}

// iiLexicalEvidence is lexicalEvidence (structured-syntax-repair.ts:109-164):
// every scalar token and the whole container shape, punctuation and whitespace
// removed. A byte the JSON grammar cannot start is a refusal, which is how the
// repair is held to punctuation.
func iiLexicalEvidence(source string) (iiScalarEvidence, bool) {
	evidence := iiScalarEvidence{scalars: []string{}}
	index := 0
	for index < len(source) {
		// Whitespace is the JavaScript one, so a multi-byte space is decoded
		// rather than compared as a byte.
		if r, size := utf8.DecodeRuneInString(source[index:]); pfSpace(r) {
			index += size
			continue
		}
		character := source[index]
		if character == ',' || character == ':' {
			index++
			continue
		}
		if character == '{' || character == '}' || character == '[' || character == ']' {
			evidence.containers += string(character)
			index++
			continue
		}
		if character == '"' {
			start := index
			index++
			escaped := false
			for index < len(source) {
				current := source[index]
				if !escaped && current == '"' {
					index++
					raw := source[start:index]
					value, err := glParse(raw)
					if err != nil {
						return iiScalarEvidence{}, false
					}
					if _, ok := value.(string); !ok {
						return iiScalarEvidence{}, false
					}
					encoded, err := plotJSON(value)
					if err != nil {
						return iiScalarEvidence{}, false
					}
					evidence.scalars = append(evidence.scalars, "s:"+encoded)
					break
				}
				if !escaped && current < 0x20 {
					return iiScalarEvidence{}, false
				}
				if escaped {
					escaped = false
				} else if current == '\\' {
					escaped = true
				}
				index++
			}
			if index > len(source) || index == 0 || source[index-1] != '"' {
				return iiScalarEvidence{}, false
			}
			continue
		}
		rest := source[index:]
		literal := ""
		for _, candidate := range []string{"true", "false", "null"} {
			if strings.HasPrefix(rest, candidate) {
				literal = candidate
				break
			}
		}
		if literal != "" {
			evidence.scalars = append(evidence.scalars, "l:"+literal)
			index += len(literal)
			continue
		}
		if number := iiNumberPrefix(rest); number != "" {
			evidence.scalars = append(evidence.scalars, "n:"+number)
			index += len(number)
			continue
		}
		return iiScalarEvidence{}, false
	}
	return evidence, true
}

// iiPreservesStructuredJSONEvidence is preservesStructuredJsonEvidence
// (structured-syntax-repair.ts:170-183): the repaired text parses, its scalars
// are the candidate's scalars token for token, and its containers are the
// candidate's with nothing but closers appended.
func iiPreservesStructuredJSONEvidence(candidate, repaired string) bool {
	repaired = pfTrim(repaired)
	if _, err := glParse(repaired); err != nil {
		return false
	}
	before, ok := iiLexicalEvidence(pfTrim(candidate))
	if !ok {
		return false
	}
	after, ok := iiLexicalEvidence(repaired)
	if !ok || len(before.scalars) != len(after.scalars) {
		return false
	}
	for index, token := range before.scalars {
		if token != after.scalars[index] {
			return false
		}
	}
	if before.containers == after.containers {
		return true
	}
	if !strings.HasPrefix(after.containers, before.containers) {
		return false
	}
	for _, character := range after.containers[len(before.containers):] {
		if character != ']' && character != '}' {
			return false
		}
	}
	return true
}

// iiImportInferencePurposes are the three attempt purposes
// importCharacterEntriesFromArtifacts partitions on
// (generation-character-proposal-proof.ts:20-22).
const (
	iiInferencePurpose = "import-inference"
	iiSyntaxPurpose    = "import-inference:structured-syntax-repair"
	iiEndpointPurpose  = "import-inference:endpoint-card-recovery"
)

// pendingFinalizedImportGeneratedEntries is importCharacterEntriesFromArtifacts
// (generation-character-proposal-proof.ts:16-41): the characterCards the run's
// settled import-inference artifacts decode to.
//
// A nil result with a nil error is the reference's `return null`: no single
// inference attempt, a doubled repair, a repair that changed scalar evidence, or
// a decode the contract refuses. A non-nil error is the reference's throw -
// CHARACTER_PROPOSAL_SOURCE_INVALID, because an artifact the ledger cannot
// confirm is not a source at all - and it is answered as the refusal it is
// rather than folded into the null arm.
func pendingFinalizedImportGeneratedEntries(ctx context.Context, conn *sql.Conn, runID string) ([]any, error) {
	// :17-19 the attempts joined to their artifacts, discarded artifacts
	// excluded, in attempt order.
	rows, err := rrMaps(ctx, conn, `SELECT t.attempt_id,json_extract(t.usage_receipt_json,'$.purpose') AS purpose
 FROM generation_attempts t JOIN generation_artifacts a ON a.attempt_id=t.attempt_id
 WHERE t.run_id=? AND a.status<>'discarded' ORDER BY t.rowid`, runID)
	if err != nil {
		return nil, err
	}
	inference, syntax, endpoint := []string{}, []string{}, []string{}
	for _, row := range rows {
		attemptID := rosterText(row["attempt_id"])
		switch rosterText(row["purpose"]) {
		case iiInferencePurpose:
			inference = append(inference, attemptID)
		case iiSyntaxPurpose:
			syntax = append(syntax, attemptID)
		case iiEndpointPurpose:
			endpoint = append(endpoint, attemptID)
		}
	}
	// :23 exactly one inference; at most one repair and one correction.
	if len(inference) != 1 || len(syntax) > 1 || len(endpoint) > 1 {
		return nil, nil
	}
	// :24-30 the receipt has to confirm the artifact the attempt settled: it
	// exists, the attempt did not fail, it finished with stop, it settled (or is
	// unknown), and its text still hashes to the hash recorded beside it.
	output := func(attemptID string) (string, error) {
		receipt, err := NewGenerationLedger(nil, nil, nil).receipt(ctx, conn, attemptID, nil)
		if err != nil {
			return "", ErrCharacterProposalPendingSourceInvalid
		}
		artifact := pfObject(pfValue(receipt, "artifact"))
		if artifact == nil ||
			phTruthy(pfValue(receipt, "failureCode")) ||
			pfText(pfValue(receipt, "result"), "finishReason") != "stop" {
			return "", ErrCharacterProposalPendingSourceInvalid
		}
		status := pfText(pfValue(receipt, "attempt"), "status")
		if status != "settled" && status != "unknown" {
			return "", ErrCharacterProposalPendingSourceInvalid
		}
		text := pfText(artifact, "text")
		if summaryHash(text) != pfText(artifact, "textHash") {
			return "", ErrCharacterProposalPendingSourceInvalid
		}
		return text, nil
	}
	original, err := output(inference[0])
	if err != nil {
		return nil, err
	}
	// :32-36 a repair replaces the candidate only when it preserved the evidence.
	if len(syntax) == 1 {
		repaired, err := output(syntax[0])
		if err != nil {
			return nil, err
		}
		if !iiPreservesStructuredJSONEvidence(original, repaired) {
			return nil, nil
		}
		original = repaired
	}
	delta := ""
	if len(endpoint) == 1 {
		delta, err = output(endpoint[0])
		if err != nil {
			return nil, err
		}
	}
	// :38-40 the decode, with every contract refusal caught into the null arm.
	var cards []any
	if delta != "" {
		cards, err = iiDecodeImportInferenceWithEndpointDelta(original, delta)
	} else {
		cards, err = iiDecodeImportInferenceJSON(original)
	}
	if err != nil {
		return nil, nil
	}
	return cards, nil
}
