package agentgen

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// This file is the begin-side half of the agent round protocol: the input the
// browser declares, the frozen context the desktop assembles from it, and the
// message list the first round would send. It is the same pure layer as the rest
// of this package - no database, no lease, no dispatch - and it is a
// transliteration of two reference functions:
//
//	validateAgentGenerationInput  electron/services/agent-generation-context.ts:5-29
//	buildAgentGenerationContext   electron/services/agent-generation-context.ts:32-57
//
// The prompt itself is never written here. The caller hands in the resolved
// identity template and its prose fragments (internal/prompttemplate), and this
// file only decides where they go: the order of the system message's sections,
// what a missing optional section does, and which messages the round starts
// with. That keeps the frozen text a function of shipped prose and admitted
// project state, never of a Go literal.

// The two names this file's refusals are spelled with, verbatim from the
// reference so a renderer that matches the desktop strings matches here too.
const (
	// CodeInputInvalid is validateAgentGenerationInput's refusal
	// (agent-generation-context.ts:6, :14, :27).
	CodeInputInvalid = "GENERATION_AGENT_INPUT_INVALID"
	// CodePromptInvalid is buildAgentGenerationContext's refusal for a template
	// that is not the identity one (agent-generation-context.ts:37-38).
	CodePromptInvalid = "GENERATION_AGENT_PROMPT_INVALID"
	// CodeNonceConflict is begin's refusal for a spent nonce that froze a
	// different input or model (electron/services/agent-generation.ts:150).
	CodeNonceConflict = "GENERATION_AGENT_NONCE_CONFLICT"
	// CodeRoundAmbiguous is a session whose run holds more than one attempt
	// (electron/services/agent-generation.ts:86).
	CodeRoundAmbiguous = "GENERATION_AGENT_ROUND_AMBIGUOUS"
)

// Input bounds (agent-generation-context.ts:11, :14, :27).
const (
	// MaxHistoryMessages bounds the frozen conversation the browser may send.
	MaxHistoryMessages = 32
	// MaxTools bounds the frozen tool manifest.
	MaxTools = 256
	// MaxInputBytes is the ceiling on the whole frozen input, measured in UTF-8
	// bytes exactly like the reference measures JSON.stringify(input).
	MaxInputBytes = 2 << 20
)

// Tool name grammar (agent-generation-context.ts:18) and the one source
// vocabulary the reference admits (:22).
var (
	toolNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_:-]{0,127}$`)
	// toolSources are the three provenances a frozen tool may carry.
	toolSources = []string{"builtin", "mcp", "skill"}
	// inputKeys and toolKeys are the closed member sets the reference validates.
	inputKeys = []string{"mode", "uiLocale", "historyMessages", "userMessage", "editorContext", "tools"}
	toolKeys  = []string{"name", "description", "inputSchema", "requiresConfirmation", "isReadOnly", "source"}
	// historyRoles are the two roles a frozen history message may carry.
	historyRoles = []string{"user", "assistant"}
	// languages are the two writing languages the reference validates.
	languages = []string{"zh-CN", "en-US"}
	// modes are the two assistant modes.
	modes = []string{"fast", "planning"}
)

// InputTool is one frozen tool manifest entry (src/shared/agent-generation.ts:19).
type InputTool struct {
	Name                 string         `json:"name"`
	Description          string         `json:"description"`
	InputSchema          map[string]any `json:"inputSchema"`
	RequiresConfirmation bool           `json:"requiresConfirmation"`
	IsReadOnly           bool           `json:"isReadOnly"`
	Source               string         `json:"source"`
}

// Input is the validated begin input (src/shared/agent-generation.ts:25). It is
// the value a session freezes in its context, and it is what a replayed begin is
// compared against, so its projection is canonical: two requests that differ only
// in member order are the same input.
type Input struct {
	Mode             string
	UILocale         string
	HistoryMessages  []Message
	UserMessage      string
	EditorContext    string
	HasEditorContext bool
	Tools            []InputTool
}

// inputWire is the input's canonical JSON projection, in the order the renderer
// builds it (agent-generation-context.ts:12).
type inputWire struct {
	Mode            string      `json:"mode"`
	UILocale        string      `json:"uiLocale"`
	HistoryMessages []Message   `json:"historyMessages"`
	UserMessage     string      `json:"userMessage"`
	EditorContext   *string     `json:"editorContext,omitempty"`
	Tools           []InputTool `json:"tools"`
}

// Text renders the validated input as the canonical JSON its digest and the
// session's replay comparison are taken over.
func (i Input) Text() (string, error) {
	wire := inputWire{Mode: i.Mode, UILocale: i.UILocale, UserMessage: i.UserMessage,
		HistoryMessages: i.HistoryMessages, Tools: i.Tools}
	if i.HasEditorContext {
		editor := i.EditorContext
		wire.EditorContext = &editor
	}
	if wire.HistoryMessages == nil {
		wire.HistoryMessages = []Message{}
	}
	if wire.Tools == nil {
		wire.Tools = []InputTool{}
	}
	return encodeJSON(wire)
}

// ParseInput validates a begin input exactly like validateAgentGenerationInput
// and returns its canonical projection. Every refusal is CodeInputInvalid: the
// reference throws one code for all of them, so a renderer cannot tell which
// field it got wrong - and neither may this layer.
func ParseInput(value map[string]any) (Input, error) {
	if value == nil || !closedKeys(value, inputKeys) {
		return Input{}, failure(CodeInputInvalid)
	}
	input := Input{
		Mode:          memberText(value["mode"]),
		UILocale:      memberText(value["uiLocale"]),
		UserMessage:   memberText(value["userMessage"]),
		EditorContext: memberText(value["editorContext"]),
	}
	if !oneOf(input.Mode, modes) || !oneOf(input.UILocale, languages) ||
		strings.TrimSpace(input.UserMessage) == "" || !utf8.ValidString(input.UserMessage) {
		return Input{}, failure(CodeInputInvalid)
	}
	if raw, present := value["editorContext"]; present {
		editor, ok := raw.(string)
		if !ok || !utf8.ValidString(editor) {
			return Input{}, failure(CodeInputInvalid)
		}
		input.EditorContext = editor
		input.HasEditorContext = true
	}
	history, ok := value["historyMessages"].([]any)
	if !ok || len(history) > MaxHistoryMessages {
		return Input{}, failure(CodeInputInvalid)
	}
	input.HistoryMessages = make([]Message, 0, len(history))
	for _, item := range history {
		message, ok := item.(map[string]any)
		if !ok || !closedKeys(message, []string{"role", "content"}) {
			return Input{}, failure(CodeInputInvalid)
		}
		role, content := memberText(message["role"]), memberText(message["content"])
		if !oneOf(role, historyRoles) {
			return Input{}, failure(CodeInputInvalid)
		}
		if _, ok := message["content"].(string); !ok || !utf8.ValidString(content) {
			return Input{}, failure(CodeInputInvalid)
		}
		input.HistoryMessages = append(input.HistoryMessages, Message{Role: role, Content: content})
	}
	tools, ok := value["tools"].([]any)
	if !ok || len(tools) > MaxTools {
		return Input{}, failure(CodeInputInvalid)
	}
	input.Tools = make([]InputTool, 0, len(tools))
	names := make(map[string]bool, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok || !closedKeys(tool, toolKeys) {
			return Input{}, failure(CodeInputInvalid)
		}
		entry := InputTool{
			Name:        memberText(tool["name"]),
			Description: memberText(tool["description"]),
			Source:      memberText(tool["source"]),
		}
		if !toolNamePattern.MatchString(entry.Name) || names[entry.Name] {
			return Input{}, failure(CodeInputInvalid)
		}
		// A missing or non-string name is already refused by the pattern above: it
		// answers the empty string, which no name may be.
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema == nil {
			return Input{}, failure(CodeInputInvalid)
		}
		entry.InputSchema = schema
		confirmed, confirmedOK := tool["requiresConfirmation"].(bool)
		readOnly, readOnlyOK := tool["isReadOnly"].(bool)
		if _, ok := tool["description"].(string); !ok || !confirmedOK || !readOnlyOK || !oneOf(entry.Source, toolSources) {
			return Input{}, failure(CodeInputInvalid)
		}
		entry.RequiresConfirmation = confirmed
		entry.IsReadOnly = readOnly
		names[entry.Name] = true
		// The generated action cannot turn workflow registration into an
		// unconfirmed read (agent-generation-context.ts:25).
		if entry.Name == "start_workflow" && (!entry.RequiresConfirmation || entry.IsReadOnly || entry.Source != "builtin") {
			return Input{}, failure(CodeInputInvalid)
		}
		input.Tools = append(input.Tools, entry)
	}
	encoded, err := input.Text()
	if err != nil || len(encoded) > MaxInputBytes {
		return Input{}, failure(CodeInputInvalid)
	}
	return input, nil
}

// IdentityPrompt is the resolved agent-generation prompt the caller read out of
// the shipped asset: the identity template's four fields, the immutable system
// contract, the two mode instructions, the tool manifest texts and the three
// section headings. This package never supplies any of them.
type IdentityPrompt struct {
	SystemRole     string
	Content        string
	TaskGuidance   string
	SystemSuffix   string
	SystemContract string
	ModePlanning   string
	ModeFast       string
	ToolHeading    string
	// ToolInstruction is the fixed sentence the manifest block carries.
	ToolInstruction string
	// The three headings the system message puts in front of the author
	// guidance, the project core and the explicit editor context.
	AuthorGuidanceHeading  string
	ProjectSettingsHeading string
	EditorContextHeading   string
}

// Context is the frozen AgentGenerationContext (src/shared/agent-generation.ts:37):
// the validated input, the project's writing language and the messages the first
// round sends. Version is always 1; the reference refuses any other.
type Context struct {
	Version         int64
	Input           Input
	WritingLanguage string
	Messages        []Message
}

// contextWire is the context's canonical JSON projection
// (agent-generation-context.ts:55-56).
type contextWire struct {
	Version         int64           `json:"version"`
	Input           json.RawMessage `json:"input"`
	WritingLanguage string          `json:"writingLanguage"`
	InitialMessages []Message       `json:"initialMessages"`
}

// Text renders the frozen context as the canonical JSON its digest is taken over.
func (c Context) Text() (string, error) {
	input, err := c.Input.Text()
	if err != nil {
		return "", failure(CodeContextInvalid)
	}
	return encodeJSON(contextWire{Version: c.Version, Input: json.RawMessage(input),
		WritingLanguage: c.WritingLanguage, InitialMessages: c.Messages})
}

// modeInstruction is the interpolate() of agent-generation-context.ts:43: every
// occurrence of the body's one variable becomes the mode's instruction.
func modeInstruction(body, instruction string) string {
	return strings.ReplaceAll(body, "{{mode_instruction}}", instruction)
}

// BuildContext composes the frozen context for one round session
// (agent-generation-context.ts:32-57). The sections appear in the reference's
// order and a section that is absent is dropped whole, never left as an empty
// heading:
//
//  1. the identity role plus the immutable system contract;
//  2. the template body, with its one variable filled by the mode instruction;
//  3. the author creative guidance, when the template ships any;
//  4. the template's immutable suffix;
//  5. the project core the application read, as JSON;
//  6. the explicit editor context, when the author supplied one;
//  7. the tool manifest, when the input declares any tool.
func BuildContext(input Input, coreText, language string, prompt IdentityPrompt) (Context, error) {
	if !oneOf(language, languages) || !validPrompt(prompt) {
		return Context{}, failure(CodePromptInvalid)
	}
	if strings.TrimSpace(coreText) == "" || !json.Valid([]byte(coreText)) || !strings.HasPrefix(strings.TrimSpace(coreText), "{") {
		return Context{}, failure(CodeContextInvalid)
	}
	instruction := prompt.ModeFast
	if input.Mode == "planning" {
		instruction = prompt.ModePlanning
	}
	parts := make([]string, 0, 7)
	parts = append(parts, composeRole(prompt.SystemRole, prompt.SystemContract))
	parts = append(parts, modeInstruction(prompt.Content, instruction))
	if prompt.TaskGuidance != "" {
		parts = append(parts, prompt.AuthorGuidanceHeading+"\n"+modeInstruction(prompt.TaskGuidance, instruction))
	}
	parts = append(parts, modeInstruction(prompt.SystemSuffix, instruction))
	parts = append(parts, prompt.ProjectSettingsHeading+"\n"+coreText)
	if input.HasEditorContext && input.EditorContext != "" {
		parts = append(parts, prompt.EditorContextHeading+"\n"+input.EditorContext)
	}
	if len(input.Tools) > 0 {
		tools, err := encodeJSON(input.Tools)
		if err != nil {
			return Context{}, failure(CodeInputInvalid)
		}
		parts = append(parts, prompt.ToolHeading+"\n\n"+prompt.ToolInstruction+"\n\n"+tools)
	}
	messages := make([]Message, 0, len(input.HistoryMessages)+2)
	messages = append(messages, Message{Role: "system", Content: strings.Join(parts, "\n\n")})
	messages = append(messages, input.HistoryMessages...)
	messages = append(messages, Message{Role: "user", Content: input.UserMessage})
	return Context{Version: 1, Input: input, WritingLanguage: language, Messages: messages}, nil
}

// composeRole is composePromptSystemRole (src/services/builtin-prompt-templates.ts:
// 130-149): the template's trimmed role, then the immutable contract, with the
// contract alone when the template ships no role.
func composeRole(systemRole, contract string) string {
	role := strings.TrimSpace(systemRole)
	if role == "" {
		return contract
	}
	return role + "\n\n" + contract
}

// validPrompt refuses a prompt that could not have come out of the shipped asset:
// every section this layer places must be present, and the body must still carry
// the variable the interpolate step fills.
func validPrompt(prompt IdentityPrompt) bool {
	for _, fragment := range []string{prompt.SystemRole, prompt.Content, prompt.TaskGuidance, prompt.SystemSuffix,
		prompt.SystemContract, prompt.ModePlanning, prompt.ModeFast, prompt.ToolHeading,
		prompt.ToolInstruction, prompt.AuthorGuidanceHeading, prompt.ProjectSettingsHeading,
		prompt.EditorContextHeading} {
		if strings.TrimSpace(fragment) == "" {
			return false
		}
	}
	return strings.Contains(prompt.Content, "{{mode_instruction}}")
}

// ParseContext reads a frozen context back. The caller has already proved that
// the stored text re-hashes to the manifest's digest; this function proves the
// value itself is the reference's: version 1, an input its own validator
// accepts, a writing language the corpus ships, and at least the system and user
// messages a round starts from. Anything else is CodeContextInvalid, which is
// the code requireRun throws for a context it cannot prove
// (electron/services/agent-generation.ts:60-62).
func ParseContext(raw []byte) (Context, error) {
	var wire contextWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Context{}, failure(CodeContextInvalid)
	}
	if wire.Version != 1 || !oneOf(wire.WritingLanguage, languages) ||
		len(wire.InitialMessages) == 0 || len(wire.Input) == 0 {
		return Context{}, failure(CodeContextInvalid)
	}
	var value map[string]any
	if err := json.Unmarshal(wire.Input, &value); err != nil {
		return Context{}, failure(CodeContextInvalid)
	}
	input, err := ParseInput(value)
	if err != nil {
		return Context{}, failure(CodeContextInvalid)
	}
	for _, message := range wire.InitialMessages {
		if !utf8.ValidString(message.Content) || !utf8.ValidString(message.Role) {
			return Context{}, failure(CodeContextInvalid)
		}
	}
	return Context{Version: 1, Input: input, WritingLanguage: wire.WritingLanguage,
		Messages: wire.InitialMessages}, nil
}

// encodeJSON is the compact JSON every canonical text here is rendered with: no
// HTML escaping and no trailing newline, so a value carrying <, > or & keeps the
// bytes the desktop would write for it.
func encodeJSON(value any) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// closedKeys reports whether every member of value is one the reference admits:
// Object.keys(value).some(key => !allowed.includes(key)) refuses unknown members
// and nothing else. A missing member is caught by that member's own type check,
// which is why editorContext may be absent and every other member may not.
func closedKeys(value map[string]any, allowed []string) bool {
	for key := range value {
		if !oneOf(key, allowed) {
			return false
		}
	}
	return true
}

// memberText reads a string member, answering the empty string for anything
// else: the reference's typeof checks are made separately wherever the empty
// string is not itself a refusal.
func memberText(value any) string {
	member, _ := value.(string)
	return member
}

// oneOf reports whether value is one of the finite vocabulary.
func oneOf(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
