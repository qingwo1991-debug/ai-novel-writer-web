// Package prompttemplate carries builtin prompt templates as committed JSON
// assets, so a Go lane can freeze and render the prompt the renderer ships
// without restating any of its prose in Go source.
//
// The asset beside this file is generated from the original TypeScript corpus and
// cites the line it came from. EditSelectedTextEdit is the only reader: it returns
// the template as the exact JSON text the desktop freezes into a run binding, so
// the digest a Go run stores is the digest of the shipped template rather than of
// a Go re-serialization of it.
//
// This package composes nothing. Rendering a template into a task is the caller's
// job, and the prose fragments returned beside it (the system contract, the fixed
// action instructions, the author-note rule and prefix) exist so that job needs no
// literal of its own.
package prompttemplate

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
)

// ErrAsset is the one refusal: the asset is absent, is not the format this
// package ships, or no longer carries what the contract names. It is a fixed
// symbol, never a path or a decoder message.
var ErrAsset = errors.New("PROMPT_TEMPLATE_ASSET_INVALID")

// editorInlineOptionalLabels are the tokens the renderer's two optional-section
// deletions match before a prompt reaches a model (src/services/
// builtin-prompt-templates.ts:151-174). This lane ports only the renderer's blank
// -line normalization, so the asset may not start carrying any of these: the
// loader refuses instead of quietly rendering a prompt the desktop would have
// pruned.
var editorInlineOptionalLabels = []string{
	"★【",
	"】★",
	"（如有",
	"作者补充指导",
	"作者节奏/风格指导",
	"作者要求重点检查的维度",
	"【剧情记忆库与前置断点上下文】",
	"【后续章节大纲预告】",
	"[Story memory and previous stopping point]",
	"[Upcoming chapter blueprints",
	"[Knowledge-base context]",
}

// editorInlineActions are the four fixed editor-inline actions the reference
// carries an instruction for (src/shared/editor-inline-generation.ts:8). An asset
// missing one of them could not compose a task for it.
var editorInlineActions = []string{"refine", "expand", "continue", "dialogue"}

// Templates are the written languages a builtin template is shipped in. The
// renderer resolves anything else to zh-CN (src/shared/writing-language.ts), and
// so does the caller before asking here.
const (
	LanguageChinese = "zh-CN"
	LanguageEnglish = "en-US"
)

// Template is one shipped builtin prompt template.
type Template struct {
	// Key is the prompt key, as the source binding freezes it.
	Key string `json:"key"`
	// Language is the writing language this text is the template for.
	Language string `json:"-"`
	// Text is the template object as compact JSON: the exact text the desktop
	// freezes, hashes and reads back (electron/services/main-generation-owner.ts:
	// 1432 freezes promptKeys, generation-source-binding.ts:560 the object).
	Text string `json:"-"`
	// SystemRole is the template's own editor role.
	SystemRole string `json:"systemRole"`
	// Content is the body with its {{variable}} placeholders still unfilled.
	Content string `json:"content"`
	// SystemSuffix is the immutable output contract appended to the body.
	SystemSuffix string `json:"systemSuffix"`
	// RequiredVariables are the placeholders a rendered body must supply.
	RequiredVariables []string `json:"requiredContextVariables"`
}

// EditorInline is the editor-inline prompt contract for one writing language: the
// shipped template plus every prose fragment the reference composes around it
// (electron/services/editor-inline-generation.ts:47-69).
type EditorInline struct {
	Key      string
	Language string
	Template Template
	// SystemContract is the immutable contract appended to every system message
	// (src/services/builtin-prompt-templates.ts:132-149).
	SystemContract string
	// Instructions is the fixed instruction per action.
	Instructions map[string]string
	// InstructRule is the author-note precedence rule added to the system role for
	// the 'instruct' action.
	InstructRule string
	// InstructPrefix is the language's lead-in for an author-written instruction;
	// the reference appends a newline and the note itself.
	InstructPrefix string
}

// editSelectedTextAsset is the committed corpus. It is generated from the shipped
// TypeScript sources, and the package test re-reads those sources so an asset that
// drifted from them fails instead of freezing a prompt nobody ships.
//
//go:embed edit-selected-text-v1.json
var editSelectedTextAsset []byte

// editSelectedTextDocument is the asset's own shape.
type editSelectedTextDocument struct {
	FormatVersion  int                          `json:"formatVersion"`
	Kind           string                       `json:"kind"`
	Key            string                       `json:"key"`
	Templates      map[string]json.RawMessage   `json:"templates"`
	SystemContract map[string]string            `json:"systemContract"`
	Instructions   map[string]map[string]string `json:"instructions"`
	Instruct       struct {
		Rule   map[string]string `json:"rule"`
		Prefix map[string]string `json:"prefix"`
	} `json:"instruct"`
}

// EditSelectedTextEdit returns the editor-inline contract for one writing
// language. Every failure is ErrAsset: the caller should refuse the operation
// rather than compose a prompt this package could not prove is the shipped one.
func EditSelectedTextEdit(language string) (EditorInline, error) {
	if language != LanguageChinese && language != LanguageEnglish {
		return EditorInline{}, ErrAsset
	}
	template, err := EditSelectedTextTemplate(language)
	if err != nil {
		return EditorInline{}, err
	}
	document, err := editSelectedTextDocumentOf()
	if err != nil {
		return EditorInline{}, err
	}
	contract := document.SystemContract[language]
	rule := document.Instruct.Rule[language]
	prefix := document.Instruct.Prefix[language]
	if contract == "" || rule == "" || prefix == "" {
		return EditorInline{}, ErrAsset
	}
	instructions := make(map[string]string, len(editorInlineActions))
	for _, action := range editorInlineActions {
		instruction := document.Instructions[action][language]
		if instruction == "" {
			return EditorInline{}, ErrAsset
		}
		instructions[action] = instruction
	}
	return EditorInline{
		Key:            document.Key,
		Language:       language,
		Template:       template,
		SystemContract: contract,
		Instructions:   instructions,
		InstructRule:   rule,
		InstructPrefix: prefix,
	}, nil
}

// EditSelectedTextTemplate returns the edit_selected_text template for one
// writing language exactly as the desktop ships it.
func EditSelectedTextTemplate(language string) (Template, error) {
	if language != LanguageChinese && language != LanguageEnglish {
		return Template{}, ErrAsset
	}
	document, err := editSelectedTextDocumentOf()
	if err != nil {
		return Template{}, err
	}
	raw, ok := document.Templates[language]
	if !ok || len(raw) == 0 {
		return Template{}, ErrAsset
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return Template{}, ErrAsset
	}
	// The frozen text must already be compact: a pretty-printed asset would freeze
	// whitespace the desktop never writes, and the digest over it would then be a
	// digest of this file's formatting.
	if !bytes.Equal(compact.Bytes(), raw) {
		return Template{}, ErrAsset
	}
	var template Template
	if err := json.Unmarshal(raw, &template); err != nil {
		return Template{}, ErrAsset
	}
	template.Language = language
	template.Text = string(raw)
	if template.Key != document.Key || template.SystemRole == "" || template.Content == "" || template.SystemSuffix == "" {
		return Template{}, ErrAsset
	}
	// Both placeholders the renderer always supplies must be in the body: a body
	// missing one would reach a model with the token still written in it.
	for _, variable := range []string{"{{edit_instruction}}", "{{selected_text}}"} {
		if !bytesContains(template.Content, variable) {
			return Template{}, ErrAsset
		}
	}
	for _, label := range editorInlineOptionalLabels {
		if bytesContains(template.Content, label) || bytesContains(template.SystemSuffix, label) {
			return Template{}, ErrAsset
		}
	}
	return template, nil
}

// editSelectedTextDocumentOf decodes the embedded asset on every call, so no
// caller can hand another a template it mutated.
func editSelectedTextDocumentOf() (editSelectedTextDocument, error) {
	var document editSelectedTextDocument
	if err := json.Unmarshal(editSelectedTextAsset, &document); err != nil {
		return editSelectedTextDocument{}, ErrAsset
	}
	if document.FormatVersion != 1 || document.Kind != "editor-inline-edit-selected-text" || document.Key != "edit_selected_text" {
		return editSelectedTextDocument{}, ErrAsset
	}
	return document, nil
}

func bytesContains(text, fragment string) bool {
	return len(fragment) != 0 && bytes.Contains([]byte(text), []byte(fragment))
}
