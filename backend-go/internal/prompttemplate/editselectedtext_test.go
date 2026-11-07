package prompttemplate

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The asset beside this file is what a Go lane freezes into a run binding, so
// these tests read it the way the lane does and then check it against the
// TypeScript sources it was generated from. The point is not that the JSON parses;
// it is that the template a Go run writes down is the template the renderer ships,
// byte for byte, and that no prose of it lives in Go source.

const (
	// shippedBuiltin is the renderer's builtin prompt corpus: the zh-CN template,
	// the immutable system contract, and the prompt key itself.
	shippedBuiltin = "../../../src/services/builtin-prompt-templates.ts"
	// shippedOverlay is the en-US overlay the corpus is merged with.
	shippedOverlay = "../../../src/services/prompt-language.ts"
	// shippedEditorInline is the desktop's editor-inline task authoring: the fixed
	// action instructions and the author-note rule.
	shippedEditorInline = "../../../electron/services/editor-inline-generation.ts"
)

// shippedCorpus reports whether this checkout carries the TypeScript tree the
// asset was generated from. A module-only checkout carries none, and then the
// drift half is skipped rather than silently passing: the asset half still runs.
func shippedCorpus(t *testing.T) bool {
	t.Helper()
	raw, err := os.ReadFile(shippedBuiltin)
	if err != nil || len(raw) == 0 {
		t.Skipf("the shipped TypeScript corpus is not in this checkout: %v", err)
		return false
	}
	return true
}

// TestEditSelectedTextTemplateServesBothLanguages proves both written languages
// resolve, that the frozen text is compact (a pretty-printed asset would freeze
// whitespace the desktop never writes), and that the body would reach a model with
// no unfilled token other than the two the renderer always supplies.
func TestEditSelectedTextTemplateServesBothLanguages(t *testing.T) {
	var texts []string
	for _, language := range []string{LanguageChinese, LanguageEnglish} {
		template, err := EditSelectedTextTemplate(language)
		if err != nil {
			t.Fatalf("%s: %v", language, err)
		}
		if template.Language != language {
			t.Fatalf("%s resolved the %s template", language, template.Language)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(template.Text)); err != nil {
			t.Fatalf("%s: the frozen text is not JSON: %v", language, err)
		}
		if compact.String() != template.Text {
			t.Fatalf("%s: the frozen text is not compact, so the digest of it would be a digest of this file's formatting", language)
		}
		var decoded struct {
			Key    string `json:"key"`
			Role   string `json:"systemRole"`
			Body   string `json:"content"`
			Suffix string `json:"systemSuffix"`
		}
		if err := json.Unmarshal([]byte(template.Text), &decoded); err != nil {
			t.Fatalf("%s: %v", language, err)
		}
		if decoded.Key != "edit_selected_text" || decoded.Role == "" || decoded.Body != template.Content || decoded.Suffix != template.SystemSuffix {
			t.Fatalf("%s: the frozen text is not the template this package answers: %+v", language, decoded)
		}
		for _, token := range []string{"{{edit_instruction}}", "{{selected_text}}"} {
			if !strings.Contains(template.Content, token) {
				t.Fatalf("%s: the body no longer carries %s", language, token)
			}
		}
		contract, err := EditSelectedTextEdit(language)
		if err != nil {
			t.Fatalf("%s: %v", language, err)
		}
		if contract.SystemContract == "" || contract.InstructRule == "" || contract.InstructPrefix == "" {
			t.Fatalf("%s: the contract is incomplete: %+v", language, contract)
		}
		if len(contract.Instructions) != len(editorInlineActions) {
			t.Fatalf("%s: %d fixed instructions, want %d", language, len(contract.Instructions), len(editorInlineActions))
		}
		for _, action := range editorInlineActions {
			if strings.TrimSpace(contract.Instructions[action]) == "" {
				t.Fatalf("%s: no instruction for %s", language, action)
			}
		}
		texts = append(texts, template.Text)
	}
	if texts[0] == texts[1] {
		t.Fatal("both written languages froze the same template, so at least one of them is not its language")
	}
	if _, err := EditSelectedTextEdit("fr-FR"); err == nil {
		t.Fatal("a language the corpus does not ship resolved anyway")
	}
	if _, err := EditSelectedTextTemplate(""); err == nil {
		t.Fatal("an empty language resolved to a template")
	}
}

// TestEditSelectedTextAssetTracksTheShippedSources is the drift check: every prose
// fragment in the asset must appear verbatim in the file the asset cites, so an
// asset that stopped matching the renderer fails here instead of freezing a prompt
// nobody ships.
func TestEditSelectedTextAssetTracksTheShippedSources(t *testing.T) {
	if !shippedCorpus(t) {
		return
	}
	files := map[string]string{}
	for _, path := range []string{shippedBuiltin, shippedOverlay, shippedEditorInline} {
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
	for _, pair := range []struct {
		language string
		body     string
		edit     EditorInline
	}{
		{LanguageChinese, shippedBuiltin, mustEdit(t, LanguageChinese)},
		{LanguageEnglish, shippedOverlay, mustEdit(t, LanguageEnglish)},
	} {
		claims = append(claims,
			claim{pair.body, pair.language + " system role", pair.edit.Template.SystemRole},
			claim{pair.body, pair.language + " body", pair.edit.Template.Content},
			claim{pair.body, pair.language + " output contract", pair.edit.Template.SystemSuffix},
			// The immutable contract is one literal in the builtin corpus for both
			// languages, never in the overlay.
			claim{shippedBuiltin, pair.language + " system contract", pair.edit.SystemContract},
			claim{shippedEditorInline, pair.language + " author-note rule", pair.edit.InstructRule},
			claim{shippedEditorInline, pair.language + " author-note prefix", pair.edit.InstructPrefix},
		)
		for _, action := range editorInlineActions {
			claims = append(claims, claim{shippedEditorInline, pair.language + " " + action + " instruction", pair.edit.Instructions[action]})
		}
	}
	for _, claim := range claims {
		if claim.value == "" {
			// A fragment this package did not resolve is the first test's failure,
			// never a silent pass here.
			continue
		}
		if !strings.Contains(files[claim.source], claim.value) {
			t.Fatalf("the asset carries a %s that %s no longer ships: the committed corpus drifted from the renderer",
				claim.label, claim.source)
		}
	}
	// The zh-CN and en-US bodies pin both literals; this last claim pins the key the
	// run freezes.
	if !strings.Contains(files[shippedBuiltin], "key: 'edit_selected_text'") {
		t.Fatalf("%s no longer ships the prompt key this family freezes", shippedBuiltin)
	}
}

func mustEdit(t *testing.T, language string) EditorInline {
	t.Helper()
	edit, err := EditSelectedTextEdit(language)
	if err != nil {
		t.Fatalf("%s: %v", language, err)
	}
	return edit
}
