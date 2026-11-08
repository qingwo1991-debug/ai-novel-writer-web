package prompttemplate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The loop asset is what a Go lane freezes into the repair and replacement
// tasks, so this file reads it the way the lane does and then rebuilds both
// prompts from the TypeScript they were generated from. The point is not that
// the JSON parses: it is that the pieces this package joins are the pieces the
// renderer ships, byte for byte, including the two interpolation points and the
// empty-partial fallback.

const (
	// shippedLegacyRosterReplacementSource authors the length-stop prompt.
	shippedLegacyRosterReplacementSource = "../../../src/shared/structured-replacement-prompt.ts"
	// shippedLegacyRosterLoopSource authors the repair system prompt and its
	// <invalid-json> wrapper.
	shippedLegacyRosterLoopSource = "../../../src/shared/legacy-roster-generation-pure.ts"
)

// shippedTemplate is one shipped template-literal arm: the static text split
// around the ${...} holes the reference interpolates. parts always carries one
// more entry than holes.
type shippedTemplate struct {
	parts []string
	holes []string
}

// compose fills every hole in order and joins the static pieces back together.
func (t shippedTemplate) compose(values []string) (string, error) {
	if len(values) != len(t.holes) {
		return "", errors.New("the shipped prompt carries a different number of holes than the asset")
	}
	var composed strings.Builder
	for index := range t.holes {
		composed.WriteString(t.parts[index])
		composed.WriteString(values[index])
	}
	composed.WriteString(t.parts[len(t.parts)-1])
	return composed.String(), nil
}

// plain is compose for a template without holes.
func (t shippedTemplate) plain() string {
	if len(t.holes) != 0 {
		return ""
	}
	return t.parts[0]
}

// shippedTSTemplate reads the first template-literal arm at or after start,
// following `+` concatenations and decoding the escapes the renderer writes. It
// returns the arm and the index just past it.
func shippedTSTemplate(source string, start int) (shippedTemplate, int, error) {
	if start < 0 || start > len(source) {
		return shippedTemplate{}, 0, errors.New("the anchor is not in the shipped source")
	}
	result := shippedTemplate{}
	var text strings.Builder
	index := start
	for index < len(source) && source[index] != '`' {
		index++
	}
	for {
		if index >= len(source) || source[index] != '`' {
			return shippedTemplate{}, 0, errors.New("the shipped source carries no template literal at the anchor")
		}
		index++
		for {
			if index >= len(source) {
				return shippedTemplate{}, 0, errors.New("the shipped source ends inside a template literal")
			}
			character := source[index]
			if character == '`' {
				index++
				break
			}
			if character == '\\' {
				if index+1 >= len(source) {
					return shippedTemplate{}, 0, errors.New("the shipped source ends inside an escape")
				}
				decoded, ok := shippedTSEscape(source[index+1])
				if !ok {
					return shippedTemplate{}, 0, errors.New("the shipped source uses an escape this reader does not decode")
				}
				text.WriteByte(decoded)
				index += 2
				continue
			}
			if character == '$' && index+1 < len(source) && source[index+1] == '{' {
				end, ok := shippedTSHoleEnd(source, index+2)
				if !ok {
					return shippedTemplate{}, 0, errors.New("the shipped source has an unterminated interpolation")
				}
				result.parts = append(result.parts, text.String())
				text.Reset()
				result.holes = append(result.holes, strings.TrimSpace(source[index+2:end]))
				index = end + 1
				continue
			}
			text.WriteByte(character)
			index++
		}
		// A `+` after the closing backtick is the same arm; anything else is the
		// next argument and ends it.
		lookahead := shippedTSSkipSpace(source, index)
		if lookahead < len(source) && source[lookahead] == '+' {
			lookahead = shippedTSSkipSpace(source, lookahead+1)
			if lookahead >= len(source) || source[lookahead] != '`' {
				return shippedTemplate{}, 0, errors.New("a shipped concatenation does not continue with a template literal")
			}
			index = lookahead
			continue
		}
		result.parts = append(result.parts, text.String())
		return result, index, nil
	}
}

// shippedTSSkipSpace walks past the whitespace a formatter may add around a
// concatenation.
func shippedTSSkipSpace(source string, index int) int {
	for index < len(source) {
		switch source[index] {
		case ' ', '\t', '\r', '\n':
			index++
		default:
			return index
		}
	}
	return index
}

// shippedTSHoleEnd finds the `}` closing one interpolation, counting the braces
// an object literal inside it would add.
func shippedTSHoleEnd(source string, start int) (int, bool) {
	depth := 1
	for index := start; index < len(source); index++ {
		switch source[index] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return index, true
			}
		}
	}
	return 0, false
}

// shippedTSEscape decodes the escapes the renderer uses in these literals.
func shippedTSEscape(character byte) (byte, bool) {
	switch character {
	case 'n':
		return '\n', true
	case 't':
		return '\t', true
	case 'r':
		return '\r', true
	case '`', '\\', '"', '\'', '$':
		return character, true
	default:
		return 0, false
	}
}

// shippedTSFallback reads the literal the reference's `visiblePartial || ...`
// arm answers when the previous attempt produced no visible text.
func shippedTSFallback(hole string) (string, error) {
	marker := strings.Index(hole, "||")
	if marker < 0 {
		return "", errors.New("the partial-answer hole no longer carries a fallback arm")
	}
	rest := strings.TrimSpace(hole[marker+2:])
	if len(rest) < 2 || rest[0] != '\'' || rest[len(rest)-1] != '\'' {
		return "", errors.New("the fallback arm is no longer a single-quoted literal")
	}
	return rest[1 : len(rest)-1], nil
}

// TestLegacyRosterLoopTemplateServesBothLanguages proves the loader answers the
// two shipped languages, that they are different prompts, and that an unknown
// language is refused instead of silently answered in zh-CN.
func TestLegacyRosterLoopTemplateServesBothLanguages(t *testing.T) {
	loop, err := LegacyRosterLoopTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.HasPrefix(loop.RepairSystem, "\n") {
		t.Fatal("the repair system prompt lost the template literal's leading newline")
	}
	zh, err := loop.Replacement(LanguageChinese)
	if err != nil {
		t.Fatalf("%v", err)
	}
	en, err := loop.Replacement(LanguageEnglish)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if zh.Language != LanguageChinese || en.Language != LanguageEnglish {
		t.Fatalf("the loader answered %s/%s", zh.Language, en.Language)
	}
	if zh.Suffix == en.Suffix {
		t.Fatal("both writing languages froze the same requirements, so one of them is not its language")
	}
	// The fallback is what the reference substitutes for an empty partial; an
	// empty prompt would be a rewritten requirement, not the shipped one.
	if zh.Render("task", "") == zh.Render("task", "partial") {
		t.Fatal("an empty partial answer did not take the shipped fallback arm")
	}
	if !strings.Contains(zh.Render("task", ""), zh.Fallback) {
		t.Fatal("the rendered prompt dropped the shipped fallback text")
	}
	for _, language := range []string{"fr-FR", "", "zh"} {
		if _, err := loop.Replacement(language); err == nil {
			t.Fatalf("%v resolved a replacement prompt the asset does not ship", language)
		}
	}
	raw := "不是 JSON"
	repair, err := loop.Repair(raw)
	if err != nil {
		t.Fatalf("%v", err)
	}
	roster, err := LegacyRosterTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.HasPrefix(repair, roster.Contract) {
		t.Fatal("the repair prompt does not start with the shipped roster contract")
	}
	if !strings.HasPrefix(repair, roster.Contract+loop.RepairLead) || !strings.HasSuffix(repair, loop.RepairTail) {
		t.Fatal("the repair prompt does not splice the raw text between the shipped wrapper pieces")
	}
	if !strings.Contains(repair, raw) {
		t.Fatal("the repair prompt dropped the unparsable text")
	}
}

// TestLegacyRosterLoopAssetTracksTheShippedSources is the drift check: both
// prompts this package composes are rebuilt from the TypeScript literals, so an
// asset that stopped matching the renderer fails instead of freezing a prompt
// nobody ships.
func TestLegacyRosterLoopAssetTracksTheShippedSources(t *testing.T) {
	loop, err := LegacyRosterLoopTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}

	// # The length-stop replacement prompt, both arms
	replacementSource, err := os.ReadFile(shippedLegacyRosterReplacementSource)
	if err != nil {
		t.Fatalf("%v", err)
	}
	replacementText := string(replacementSource)
	anchor := strings.Index(replacementText, "return promptLanguageText(")
	if anchor < 0 {
		t.Fatalf("%v no longer composes the bilingual prompt", shippedLegacyRosterReplacementSource)
	}
	chinese, afterChinese, err := shippedTSTemplate(replacementText, anchor)
	if err != nil {
		t.Fatalf("zh-CN: %v", err)
	}
	english, _, err := shippedTSTemplate(replacementText, afterChinese)
	if err != nil {
		t.Fatalf("en-US: %v", err)
	}
	for _, arm := range []struct {
		language string
		template shippedTemplate
		original string
		partial  string
	}{
		{LanguageChinese, chinese, "原始任务正文", "上一轮可见输出"},
		{LanguageEnglish, english, "original task body", "visible prior output"},
	} {
		if len(arm.template.holes) != 2 {
			t.Fatalf("%v: the shipped prompt no longer has the original-task and partial-answer holes", arm.language)
		}
		if !strings.HasPrefix(arm.template.holes[0], "originalPrompt") {
			t.Fatalf("%v: the first hole is not the original task: %v", arm.language, arm.template.holes[0])
		}
		if !strings.HasPrefix(arm.template.holes[1], "visiblePartial") {
			t.Fatalf("%v: the second hole is not the visible partial: %v", arm.language, arm.template.holes[1])
		}
		shipped, err := loop.Replacement(arm.language)
		if err != nil {
			t.Fatalf("%v: %v", arm.language, err)
		}
		expected, err := arm.template.compose([]string{arm.original, arm.partial})
		if err != nil {
			t.Fatalf("%v: %v", arm.language, err)
		}
		if actual := shipped.Render(arm.original, arm.partial); actual != expected {
			t.Fatalf("%v: the asset composes a different prompt than %v ships:\nasset: %v\nsource: %v",
				arm.language, shippedLegacyRosterReplacementSource, actual, expected)
		}
		fallback, err := shippedTSFallback(arm.template.holes[1])
		if err != nil {
			t.Fatalf("%v: %v", arm.language, err)
		}
		if shipped.Fallback != fallback {
			t.Fatalf("%v: the asset's fallback is %v, but the reference substitutes %v", arm.language, shipped.Fallback, fallback)
		}
		expectedEmpty, err := arm.template.compose([]string{arm.original, fallback})
		if err != nil {
			t.Fatalf("%v: %v", arm.language, err)
		}
		if actual := shipped.Render(arm.original, ""); actual != expectedEmpty {
			t.Fatalf("%v: an empty partial answer does not compose the shipped fallback prompt:\nasset: %v\nsource: %v",
				arm.language, actual, expectedEmpty)
		}
	}

	// # The JSON-repair prompt
	pureSource, err := os.ReadFile(shippedLegacyRosterLoopSource)
	if err != nil {
		t.Fatalf("%v", err)
	}
	pureText := string(pureSource)
	systemAnchor := strings.Index(pureText, "export const CHARACTER_ROSTER_JSON_REPAIR_SYSTEM = ")
	if systemAnchor < 0 {
		t.Fatalf("%v no longer authors the repair system prompt", shippedLegacyRosterLoopSource)
	}
	system, _, err := shippedTSTemplate(pureText, systemAnchor)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if system.plain() != loop.RepairSystem {
		t.Fatalf("the asset's repair system prompt is not the one %v ships: %v", shippedLegacyRosterLoopSource, system.plain())
	}
	wrapperAnchor := strings.Index(pureText, "export function buildCharacterRosterJsonRepairPrompt(")
	if wrapperAnchor < 0 {
		t.Fatalf("%v no longer composes the repair wrapper", shippedLegacyRosterLoopSource)
	}
	wrapper, _, err := shippedTSTemplate(pureText, wrapperAnchor)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(wrapper.holes) != 2 {
		t.Fatalf("the repair wrapper no longer has the contract and raw-text holes: %v", wrapper.holes)
	}
	if !strings.HasPrefix(wrapper.holes[0], "CHARACTER_ROSTER_JSON_CONTRACT") {
		t.Fatalf("the wrapper's first hole is not the roster contract: %v", wrapper.holes[0])
	}
	if !strings.HasPrefix(wrapper.holes[1], "rawText") {
		t.Fatalf("the wrapper's second hole is not the unparsable text: %v", wrapper.holes[1])
	}
	roster, err := LegacyRosterTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	raw := "{\"schemaVersion\":1,\"entries\":["
	expected, err := wrapper.compose([]string{roster.Contract, raw})
	if err != nil {
		t.Fatalf("%v", err)
	}
	actual, err := loop.Repair(raw)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if actual != expected {
		t.Fatalf("the asset composes a different repair prompt than %v ships:\nasset: %v\nsource: %v",
			shippedLegacyRosterLoopSource, actual, expected)
	}
}

// TestLegacyRosterLoopAssetCarriesExactlyTheShippedSections pins the key set the
// loader answers, so a later prose member cannot land in the asset without
// teaching this package (and the lane) to use it.
func TestLegacyRosterLoopAssetCarriesExactlyTheShippedSections(t *testing.T) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(legacyRosterLoopAsset, &document); err != nil {
		t.Fatalf("%v", err)
	}
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if joined := strings.Join(keys, ","); joined != "formatVersion,key,kind,repairLead,repairSystem,repairTail,replacement" {
		t.Fatalf("the loop asset carries %s; the loader does not answer that shape", joined)
	}
	var replacement map[string]map[string]json.RawMessage
	if err := json.Unmarshal(document["replacement"], &replacement); err != nil {
		t.Fatalf("%v", err)
	}
	languages := make([]string, 0, len(replacement))
	for language := range replacement {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	if joined := strings.Join(languages, ","); joined != LanguageEnglish+","+LanguageChinese {
		t.Fatalf("the loop asset ships replacement prompts for %s", joined)
	}
	for language, entry := range replacement {
		fields := make([]string, 0, len(entry))
		for field := range entry {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		if joined := strings.Join(fields, ","); joined != "fallback,originalLead,partialLead,suffix" {
			t.Fatalf("%v replacement prompt carries %s", language, joined)
		}
	}
}

// TestLegacyRosterLoopProseIsNotInGoSource proves the composed prose reaches
// this package only through the asset: no Go file beside it may restate a
// shipped section.
func TestLegacyRosterLoopProseIsNotInGoSource(t *testing.T) {
	loop, err := LegacyRosterLoopTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	chinese, err := loop.Replacement(LanguageChinese)
	if err != nil {
		t.Fatalf("%v", err)
	}
	templates, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("%v", err)
	}
	var markers []string
	for _, fragment := range []string{loop.RepairSystem, chinese.OriginalLead, chinese.Suffix} {
		for _, line := range strings.Split(fragment, "\n") {
			line = strings.TrimSpace(line)
			if len([]rune(line)) > 24 {
				markers = append(markers, line)
				break
			}
		}
	}
	if len(markers) != 3 {
		t.Fatalf("the asset no longer carries three long section lines to scan for: %v", markers)
	}
	for _, name := range templates {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%v", err)
		}
		for _, marker := range markers {
			if strings.Contains(string(body), marker) {
				t.Fatalf("%v restates shipped loop prompt prose in Go; it must come from the committed asset", name)
			}
		}
	}
}
