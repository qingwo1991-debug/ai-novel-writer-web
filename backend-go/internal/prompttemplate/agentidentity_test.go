package prompttemplate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent-generation asset is what a Go lane composes the round's system
// message from, so these tests read it the way the lane does and then check it
// against the TypeScript sources it was generated from. The point is not that the
// JSON parses; it is that the template and the seven prose fragments a Go run
// writes down are the ones the renderer ships, byte for byte, and that none of it
// lives in Go source.

const (
	// shippedAgentIdentitySource is the identity template's authoring file: the
	// zh-CN object, the en-US overlay and the system contract.
	shippedAgentIdentitySource = "../../../src/services/builtin-prompt-templates.ts"
	// shippedAgentContextSource is the file that composes the system message.
	shippedAgentContextSource = "../../../electron/services/agent-generation-context.ts"
)

// TestAgentIdentityPromptServesBothLanguages proves both written languages
// resolve, that the frozen template text is compact, and that every section the
// composition places is present and distinct.
func TestAgentIdentityPromptServesBothLanguages(t *testing.T) {
	var texts []string
	for _, language := range []string{LanguageChinese, LanguageEnglish} {
		prompt, err := AgentIdentityPrompt(language)
		if err != nil {
			t.Fatalf("%v: %v", language, err)
		}
		if prompt.Key != AgentIdentityKey || prompt.Language != language {
			t.Fatalf("%v resolved key %v language %v", language, prompt.Key, prompt.Language)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(prompt.Template.Text)); err != nil {
			t.Fatalf("%v: the frozen text is not JSON: %v", language, err)
		}
		if compact.String() != prompt.Template.Text {
			t.Fatalf("%v: the frozen text is not compact, so its digest would be this file's formatting", language)
		}
		var decoded struct {
			Key    string `json:"key"`
			Role   string `json:"systemRole"`
			Body   string `json:"content"`
			Guide  string `json:"taskGuidance"`
			Suffix string `json:"systemSuffix"`
		}
		if err := json.Unmarshal([]byte(prompt.Template.Text), &decoded); err != nil {
			t.Fatalf("%v: %v", language, err)
		}
		if decoded.Key != AgentIdentityKey || decoded.Role != prompt.Template.SystemRole ||
			decoded.Body != prompt.Template.Content || decoded.Guide != prompt.Template.TaskGuidance ||
			decoded.Suffix != prompt.Template.SystemSuffix {
			t.Fatalf("%v: the frozen text is not the template this package answers: %+v", language, decoded)
		}
		if !strings.Contains(prompt.Template.Content, "{{mode_instruction}}") {
			t.Fatalf("%v: the body lost the variable the composition fills", language)
		}
		for label, fragment := range map[string]string{
			"systemRole":             prompt.Template.SystemRole,
			"taskGuidance":           prompt.Template.TaskGuidance,
			"systemSuffix":           prompt.Template.SystemSuffix,
			"systemContract":         prompt.SystemContract,
			"planning mode":          prompt.ModePlanning,
			"fast mode":              prompt.ModeFast,
			"tool heading":           prompt.ToolHeading,
			"tool instruction":       prompt.ToolInstruction,
			"author guidance":        prompt.AuthorGuidanceHeading,
			"project settings":       prompt.ProjectSettingsHeading,
			"editor context heading": prompt.EditorContextHeading,
		} {
			if strings.TrimSpace(fragment) == "" {
				t.Fatalf("%v: %v is empty", language, label)
			}
		}
		if prompt.ModePlanning == prompt.ModeFast {
			t.Fatalf("%v: both modes froze the same instruction", language)
		}
		texts = append(texts, prompt.Template.Text)
	}
	if texts[0] == texts[1] {
		t.Fatal("both written languages froze the same template, so one of them is not its language")
	}
	for _, language := range []string{"fr-FR", "", "zh"} {
		if _, err := AgentIdentityPrompt(language); err == nil {
			t.Fatalf("%v resolved a prompt the corpus does not ship", language)
		}
	}
}

// TestAgentIdentityAssetTracksTheShippedSources is the drift check: every literal
// in the asset must appear verbatim in the file the asset cites, so an asset that
// stopped matching the renderer fails here instead of freezing a prompt nobody
// ships.
func TestAgentIdentityAssetTracksTheShippedSources(t *testing.T) {
	identity, err := os.ReadFile(shippedAgentIdentitySource)
	if err != nil {
		t.Fatalf("%v", err)
	}
	contextSource, err := os.ReadFile(shippedAgentContextSource)
	if err != nil {
		t.Fatalf("%v", err)
	}
	for _, language := range []string{LanguageChinese, LanguageEnglish} {
		prompt, err := AgentIdentityPrompt(language)
		if err != nil {
			t.Fatalf("%v: %v", language, err)
		}
		for label, fragment := range map[string]string{
			"systemRole":     prompt.Template.SystemRole,
			"taskGuidance":   prompt.Template.TaskGuidance,
			"content":        prompt.Template.Content,
			"systemSuffix":   prompt.Template.SystemSuffix,
			"systemContract": prompt.SystemContract,
		} {
			// An empty fragment would make `strings.Contains` trivially true, so it is
			// refused here before the containment check can hide it.
			if strings.TrimSpace(fragment) == "" {
				t.Fatalf("%v %v is empty, so the asset lost a section the loader needs", language, label)
			}
			// A single-quoted TypeScript string collapses a doubled quote; the shipped
			// literals carry none, so the value is a verbatim substring.
			if !strings.Contains(string(identity), fragment) {
				t.Fatalf("%v %v is not a verbatim literal of %v, so the asset no longer tracks the source",
					language, label, shippedAgentIdentitySource)
			}
		}
		for label, fragment := range map[string]string{
			"planning mode":          prompt.ModePlanning,
			"fast mode":              prompt.ModeFast,
			"tool heading":           prompt.ToolHeading,
			"tool instruction":       prompt.ToolInstruction,
			"author guidance":        prompt.AuthorGuidanceHeading,
			"project settings":       prompt.ProjectSettingsHeading,
			"editor context heading": prompt.EditorContextHeading,
		} {
			if strings.TrimSpace(fragment) == "" {
				t.Fatalf("%v %v is empty, so the asset lost a section the loader needs", language, label)
			}
			if !strings.Contains(string(contextSource), fragment) {
				t.Fatalf("%v %v is not a verbatim literal of %v", language, label, shippedAgentContextSource)
			}
		}
	}
}

// TestAgentIdentityProseIsNotInGoSource proves the composed prose reaches this
// package only through the asset: no Go file beside it may carry a whole section
// literal.
func TestAgentIdentityProseIsNotInGoSource(t *testing.T) {
	prompt, err := AgentIdentityPrompt(LanguageChinese)
	if err != nil {
		t.Fatalf("%v", err)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("%v", err)
	}
	// The markers are lines of the shipped prose, long enough that a Go comment or
	// literal restating them would be the only way to match.
	var markers []string
	for _, fragment := range []string{prompt.Template.SystemSuffix, prompt.SystemContract, prompt.ToolInstruction} {
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
	for _, name := range files {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%v", err)
		}
		for _, marker := range markers {
			if strings.Contains(string(body), marker) {
				t.Fatalf("%v restates shipped prompt prose in Go; it must come from the committed asset", name)
			}
		}
	}
}
