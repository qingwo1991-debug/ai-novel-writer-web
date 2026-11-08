package prompttemplate

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The two finalization assets are what a Go lane freezes into a run binding, so
// these tests read them the way the lane does and then check them against the
// TypeScript sources they were generated from. The point is not that the JSON
// parses; it is that the template and prose a Go run writes down are the ones
// the renderer ships, byte for byte, and that none of it lives in Go source.

const (
	// shippedFinalizationSource is the desktop's finalization task authoring: the
	// bilingual output contract the character_cards step appends.
	shippedFinalizationSource = "../../../electron/services/finalization-generation-source.ts"
)

// TestFinalizationTemplatesServeBothLanguages proves both steps and both written
// languages resolve, that the frozen text is compact, and that every variable
// the renderer always supplies is still in the body.
func TestFinalizationTemplatesServeBothLanguages(t *testing.T) {
	for _, key := range []string{FinalizationChapterNotesKey, FinalizationCharacterCardsKey} {
		var texts []string
		for _, language := range []string{LanguageChinese, LanguageEnglish} {
			prompt, err := FinalizationTemplate(key, language)
			if err != nil {
				t.Fatalf("%s/%s: %v", key, language, err)
			}
			if prompt.Key != key || prompt.Language != language {
				t.Fatalf("%s/%s resolved key %s language %s", key, language, prompt.Key, prompt.Language)
			}
			if prompt.Template.Language != language || prompt.Template.Key != key {
				t.Fatalf("%s/%s resolved the wrong template: %+v", key, language, prompt.Template)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(prompt.Template.Text)); err != nil {
				t.Fatalf("%s/%s: the frozen text is not JSON: %v", key, language, err)
			}
			if compact.String() != prompt.Template.Text {
				t.Fatalf("%s/%s: the frozen text is not compact, so the digest of it would be a digest of this file's formatting", key, language)
			}
			var decoded struct {
				Key    string `json:"key"`
				Role   string `json:"systemRole"`
				Body   string `json:"content"`
				Suffix string `json:"systemSuffix"`
			}
			if err := json.Unmarshal([]byte(prompt.Template.Text), &decoded); err != nil {
				t.Fatalf("%s/%s: %v", key, language, err)
			}
			if decoded.Key != key || decoded.Role == "" || decoded.Body != prompt.Template.Content || decoded.Suffix != "" {
				t.Fatalf("%s/%s: the frozen text is not the template this package answers: %+v", key, language, decoded)
			}
			if prompt.SystemContract == "" || prompt.OutputContract == "" {
				t.Fatalf("%s/%s: the contract is incomplete: %+v", key, language, prompt)
			}
			for _, variable := range finalizationVariables(key) {
				if !strings.Contains(prompt.Template.Content, variable) {
					t.Fatalf("%s/%s: the body no longer carries %s", key, language, variable)
				}
			}
			texts = append(texts, prompt.Template.Text)
		}
		if texts[0] == texts[1] {
			t.Fatalf("%s: both written languages froze the same template, so at least one of them is not its language", key)
		}
	}
	if _, err := FinalizationTemplate(FinalizationChapterNotesKey, "fr-FR"); err == nil {
		t.Fatal("a language the corpus does not ship resolved anyway")
	}
	if _, err := FinalizationTemplate(FinalizationChapterNotesKey, ""); err == nil {
		t.Fatal("an empty language resolved to a template")
	}
	if _, err := FinalizationTemplate("edit_selected_text", LanguageChinese); err == nil {
		t.Fatal("a non-finalization key resolved to a finalization template")
	}
}

// TestFinalizationAssetsTrackTheShippedSources is the drift check: every prose
// fragment in the assets must appear verbatim in the file the asset cites, so an
// asset that stopped matching the renderer fails here instead of freezing a
// prompt nobody ships.
func TestFinalizationAssetsTrackTheShippedSources(t *testing.T) {
	if !shippedCorpus(t) {
		return
	}
	files := map[string]string{}
	for _, path := range []string{shippedBuiltin, shippedOverlay, shippedFinalizationSource} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v", err)
		}
		files[path] = string(raw)
	}
	type claim struct {
		source string
		label  string
		value  string
	}
	var claims []claim
	for _, key := range []string{FinalizationChapterNotesKey, FinalizationCharacterCardsKey} {
		zh, err := FinalizationTemplate(key, LanguageChinese)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		en, err := FinalizationTemplate(key, LanguageEnglish)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		claims = append(claims,
			claim{shippedBuiltin, key + " zh-CN system role", zh.Template.SystemRole},
			claim{shippedBuiltin, key + " zh-CN body", zh.Template.Content},
			claim{shippedOverlay, key + " en-US system role", en.Template.SystemRole},
			claim{shippedOverlay, key + " en-US body", en.Template.Content},
			// The immutable contract is one literal in the builtin corpus for both
			// languages, never in the overlay or the finalization source.
			claim{shippedBuiltin, key + " zh-CN system contract", zh.SystemContract},
			claim{shippedBuiltin, key + " en-US system contract", en.SystemContract},
			// The output contract is one bilingual literal in the finalization source.
			claim{shippedFinalizationSource, key + " zh-CN output contract", zh.OutputContract},
			claim{shippedFinalizationSource, key + " en-US output contract", en.OutputContract},
		)
	}
	for _, claim := range claims {
		if claim.value == "" {
			continue
		}
		if !finalizationSourceContains(files[claim.source], claim.value) {
			t.Fatalf("the asset carries a %s that %s no longer ships: the committed corpus drifted from the renderer",
				claim.label, claim.source)
		}
	}
	// The two keys the run freezes must still be spelled in the builtin corpus.
	for _, key := range []string{"key: 'generate_chapter_notes'", "key: 'update_character_cards'"} {
		if !strings.Contains(files[shippedBuiltin], key) {
			t.Fatalf("%s no longer ships the prompt key this family freezes", shippedBuiltin)
		}
	}
}

// finalizationSourceContains checks that a decoded prose fragment still appears
// in its shipped source. The output contract is a single-quoted JavaScript
// string, so its apostrophes are stored escaped (\') in the source, and a
// template-literal body stores its backticks escaped (\`); the asset's decoded
// value carries the literal characters. Both spellings are checked so a source
// that switched quote style is not a false drift.
func finalizationSourceContains(source, value string) bool {
	if strings.Contains(source, value) {
		return true
	}
	if strings.Contains(source, strings.ReplaceAll(value, "'", "\\'")) {
		return true
	}
	return strings.Contains(source, strings.ReplaceAll(value, "`", "\\`"))
}
