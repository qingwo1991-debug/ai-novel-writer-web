// Package prompttemplate carries builtin prompt templates as committed JSON
// assets. This file is the finalization-generation half: the two templates the
// finalization family renders (generate_chapter_notes and
// update_character_cards) plus the two prose fragments the reference composes
// around them in electron/services/finalization-generation-source.ts.
//
// The assets beside this file are generated from the shipped TypeScript
// sources, exactly like edit-selected-text-v1.json. Each `templates` value is
// the compact JSON.stringify of getBuiltinPromptTemplate(key, lang) - the exact
// text the desktop freezes, hashes and reads back - so a Go run freezes the
// digest of the shipped template rather than of a Go re-serialization of it.
// The package test re-reads the TypeScript sources so an asset that drifted
// from them fails instead of freezing a prompt nobody ships.
package prompttemplate

import (
	"bytes"
	_ "embed"
	"encoding/json"
)

// Finalization prompt keys the family freezes (electron/services/
// finalization-generation-source.ts:29).
const (
	FinalizationChapterNotesKey   = "generate_chapter_notes"
	FinalizationCharacterCardsKey = "update_character_cards"
)

// FinalizationPrompt is the prompt contract for one finalization step and one
// writing language: the resolved template plus the two prose fragments the
// reference composes around it. Every fragment comes from an asset, never from a
// Go literal.
type FinalizationPrompt struct {
	Key      string
	Language string
	Template Template
	// SystemContract is composePromptSystemRole's immutable contract
	// (src/services/builtin-prompt-templates.ts:132-149).
	SystemContract string
	// OutputContract is finalizationGenerationTask's bilingual output contract
	// (electron/services/finalization-generation-source.ts:50-51). It is appended
	// to the user message only for the character_cards step.
	OutputContract string
}

//go:embed generate-chapter-notes-v1.json
var generateChapterNotesAsset []byte

//go:embed update-character-cards-v1.json
var updateCharacterCardsAsset []byte

// finalizationDocument is one asset's own shape.
type finalizationDocument struct {
	FormatVersion  int                        `json:"formatVersion"`
	Kind           string                     `json:"kind"`
	Key            string                     `json:"key"`
	Templates      map[string]json.RawMessage `json:"templates"`
	SystemContract map[string]string          `json:"systemContract"`
	OutputContract map[string]string          `json:"outputContract"`
}

// FinalizationTemplate returns the finalization prompt contract for one step
// key and one writing language. Every failure is ErrAsset: the caller should
// refuse the operation rather than compose a prompt this package could not prove
// is the shipped one.
func FinalizationTemplate(key, language string) (FinalizationPrompt, error) {
	if language != LanguageChinese && language != LanguageEnglish {
		return FinalizationPrompt{}, ErrAsset
	}
	var asset []byte
	var kind string
	switch key {
	case FinalizationChapterNotesKey:
		asset, kind = generateChapterNotesAsset, "finalization-generation-chapter-notes"
	case FinalizationCharacterCardsKey:
		asset, kind = updateCharacterCardsAsset, "finalization-generation-character-cards"
	default:
		return FinalizationPrompt{}, ErrAsset
	}
	document, err := finalizationDocumentOf(asset, kind, key)
	if err != nil {
		return FinalizationPrompt{}, err
	}
	raw, ok := document.Templates[language]
	if !ok || len(raw) == 0 {
		return FinalizationPrompt{}, ErrAsset
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return FinalizationPrompt{}, ErrAsset
	}
	if !bytes.Equal(compact.Bytes(), raw) {
		return FinalizationPrompt{}, ErrAsset
	}
	var template Template
	if err := json.Unmarshal(raw, &template); err != nil {
		return FinalizationPrompt{}, ErrAsset
	}
	template.Language = language
	template.Text = string(raw)
	if template.Key != key || template.SystemRole == "" || template.Content == "" {
		return FinalizationPrompt{}, ErrAsset
	}
	contract := document.SystemContract[language]
	output := document.OutputContract[language]
	if contract == "" || output == "" {
		return FinalizationPrompt{}, ErrAsset
	}
	// Every variable the renderer supplies for this step must be in the body:
	// a body missing one would reach a model with the token still written in it.
	for _, variable := range finalizationVariables(key) {
		if !bytesContains(template.Content, variable) {
			return FinalizationPrompt{}, ErrAsset
		}
	}
	return FinalizationPrompt{
		Key:            key,
		Language:       language,
		Template:       template,
		SystemContract: contract,
		OutputContract: output,
	}, nil
}

// finalizationVariables is the renderer's variable set per step
// (electron/services/finalization-generation-source.ts:55-56).
func finalizationVariables(key string) []string {
	switch key {
	case FinalizationChapterNotesKey:
		return []string{"{{chapter_content}}", "{{chapter_number}}", "{{chapter_title}}"}
	case FinalizationCharacterCardsKey:
		return []string{"{{chapter_content}}", "{{chapter_number}}", "{{existing_cards_json}}"}
	}
	return nil
}

// finalizationDocumentOf decodes the embedded asset on every call, so no caller
// can hand another a template it mutated.
func finalizationDocumentOf(asset []byte, kind, key string) (finalizationDocument, error) {
	var document finalizationDocument
	if err := json.Unmarshal(asset, &document); err != nil {
		return finalizationDocument{}, ErrAsset
	}
	if document.FormatVersion != 1 || document.Kind != kind || document.Key != key {
		return finalizationDocument{}, ErrAsset
	}
	return document, nil
}
