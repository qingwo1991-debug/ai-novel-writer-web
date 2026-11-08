// Package prompttemplate carries builtin prompt templates as committed JSON
// assets. This file is the agent-generation half: the assistant_writing_identity
// template the desktop resolves for an agent round, plus the prose fragments
// buildAgentGenerationContext composes around it.
//
// The identity template ships in src/services/builtin-prompt-templates.ts:194
// with its en-US overlay at :1472, exactly like edit_selected_text, so the asset
// beside this file is generated from that source. The fragments - the mode
// instruction, the tool manifest heading and instruction, and the five section
// headings - ship in electron/services/agent-generation-context.ts:39-54, which
// is the one place the desktop authors them. Every value is the exact literal
// text of its TypeScript string (a template literal's contents without the
// backticks), so a Go run composes the prompt the desktop ships rather than a Go
// paraphrase of it. The package test re-reads both sources so an asset that
// drifted from them fails instead of freezing a prompt nobody ships.
package prompttemplate

import (
	"bytes"
	_ "embed"
	"encoding/json"
)

// AgentIdentityKey is the one prompt key an agent round freezes
// (electron/services/agent-generation.ts:176).
const AgentIdentityKey = "assistant_writing_identity"

// AgentIdentityTemplate is the shipped template's own fields. Text is the whole
// template object as compact JSON - the exact text the desktop freezes, hashes
// and reads back - and the other fields are its members, so a caller can compose
// the system message without decoding it again.
type AgentIdentityTemplate struct {
	Key          string `json:"key"`
	SystemRole   string `json:"systemRole"`
	TaskGuidance string `json:"taskGuidance"`
	Content      string `json:"content"`
	SystemSuffix string `json:"systemSuffix"`
	// Text is the compact JSON of the shipped template object.
	Text string `json:"-"`
}

// AgentIdentity is the agent-generation prompt contract for one writing
// language: the resolved identity template plus every prose fragment
// buildAgentGenerationContext interpolates.
type AgentIdentity struct {
	Key      string
	Language string
	Template AgentIdentityTemplate
	// SystemContract is composePromptSystemRole's immutable contract
	// (src/services/builtin-prompt-templates.ts:132-149).
	SystemContract string
	// ModePlanning and ModeFast are the two mode instructions the body's
	// template variable is replaced with (agent-generation-context.ts:40-42).
	ModePlanning string
	ModeFast     string
	// ToolHeading and ToolInstruction open the tool manifest block that follows
	// the project settings when the input declares any tool
	// (agent-generation-context.ts:44-48).
	ToolHeading     string
	ToolInstruction string
	// The three section headings the composed system message puts in front of
	// the author guidance, the project core and the editor context
	// (agent-generation-context.ts:49-54).
	AuthorGuidanceHeading  string
	ProjectSettingsHeading string
	EditorContextHeading   string
}

//go:embed agent-identity-v1.json
var agentIdentityAsset []byte

// agentIdentityDocument is the asset's own shape.
type agentIdentityDocument struct {
	FormatVersion  int                          `json:"formatVersion"`
	Kind           string                       `json:"kind"`
	Key            string                       `json:"key"`
	Templates      map[string]json.RawMessage   `json:"templates"`
	SystemContract map[string]string            `json:"systemContract"`
	Mode           map[string]map[string]string `json:"mode"`
	ToolManifest   struct {
		Heading     map[string]string `json:"heading"`
		Instruction map[string]string `json:"instruction"`
	} `json:"toolManifest"`
	Headings map[string]map[string]string `json:"headings"`
}

// AgentIdentityPrompt returns the shipped agent-generation prompt for one
// writing language. Every failure is ErrAsset: the caller should refuse the
// begin rather than compose a prompt this package could not prove is the
// shipped one.
func AgentIdentityPrompt(language string) (AgentIdentity, error) {
	if language != LanguageChinese && language != LanguageEnglish {
		return AgentIdentity{}, ErrAsset
	}
	document, err := agentIdentityDocumentOf()
	if err != nil {
		return AgentIdentity{}, err
	}
	raw, ok := document.Templates[language]
	if !ok || len(raw) == 0 {
		return AgentIdentity{}, ErrAsset
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return AgentIdentity{}, ErrAsset
	}
	// The frozen text must already be compact: a pretty-printed asset would freeze
	// whitespace the desktop never writes, and the digest over the prompt would
	// then be a digest of this file's formatting.
	if !bytes.Equal(compact.Bytes(), raw) {
		return AgentIdentity{}, ErrAsset
	}
	var template AgentIdentityTemplate
	if err := json.Unmarshal(raw, &template); err != nil {
		return AgentIdentity{}, ErrAsset
	}
	template.Text = string(raw)
	if template.Key != AgentIdentityKey || template.SystemRole == "" || template.Content == "" ||
		template.SystemSuffix == "" || template.TaskGuidance == "" {
		return AgentIdentity{}, ErrAsset
	}
	// The one template variable the renderer always supplies must be in the body:
	// a body missing it would reach a model with a bare token in it.
	if !bytesContains(template.Content, "{{mode_instruction}}") {
		return AgentIdentity{}, ErrAsset
	}
	identity := AgentIdentity{
		Key:                    document.Key,
		Language:               language,
		Template:               template,
		SystemContract:         document.SystemContract[language],
		ModePlanning:           document.Mode[language]["planning"],
		ModeFast:               document.Mode[language]["fast"],
		ToolHeading:            document.ToolManifest.Heading[language],
		ToolInstruction:        document.ToolManifest.Instruction[language],
		AuthorGuidanceHeading:  document.Headings["authorGuidance"][language],
		ProjectSettingsHeading: document.Headings["projectSettings"][language],
		EditorContextHeading:   document.Headings["editorContext"][language],
	}
	for _, fragment := range []string{identity.SystemContract, identity.ModePlanning, identity.ModeFast,
		identity.ToolHeading, identity.ToolInstruction, identity.AuthorGuidanceHeading,
		identity.ProjectSettingsHeading, identity.EditorContextHeading} {
		if fragment == "" {
			return AgentIdentity{}, ErrAsset
		}
	}
	// The two modes are two instructions: an asset that shipped one text twice
	// could not tell a planning round from a fast one.
	if identity.ModePlanning == identity.ModeFast {
		return AgentIdentity{}, ErrAsset
	}
	return identity, nil
}

// agentIdentityDocumentOf decodes the embedded asset on every call, so no caller
// can hand another a prompt it mutated.
func agentIdentityDocumentOf() (agentIdentityDocument, error) {
	var document agentIdentityDocument
	if err := json.Unmarshal(agentIdentityAsset, &document); err != nil {
		return agentIdentityDocument{}, ErrAsset
	}
	if document.FormatVersion != 1 || document.Kind != "agent-generation-identity" || document.Key != AgentIdentityKey {
		return agentIdentityDocument{}, ErrAsset
	}
	return document, nil
}
