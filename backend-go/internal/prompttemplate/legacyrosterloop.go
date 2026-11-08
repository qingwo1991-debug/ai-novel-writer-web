// This file is the legacy-roster loop half: the two prompts the six-step
// initial/repair loop composes when its first answer is not usable. They are
// authored in committed TypeScript, not in builtin-prompt-templates.ts:
//
//	buildStructuredReplacementPrompt  src/shared/structured-replacement-prompt.ts:3-41
//	CHARACTER_ROSTER_JSON_REPAIR_SYSTEM + the <invalid-json> wrapper
//	                                  src/shared/legacy-roster-generation-pure.ts:38-39,
//	                                  :50-52, composed by
//	                                  buildCharacterRosterJsonRepairTask (:67-76)
//
// The asset beside this file is generated from those literals: each member is a
// contiguous piece of the reference's composed prompt, and the two holes the
// reference interpolates (the original task, the visible partial answer, the
// frozen contract and the unparsable text) are the only things this package
// joins them with. Nothing here is prose: the loader test re-reads the
// TypeScript sources and rebuilds both prompts from them, so an asset that
// stopped matching the renderer fails instead of freezing a prompt nobody
// ships.
package prompttemplate

import (
	"bytes"
	_ "embed"
	"encoding/json"
)

// LegacyRosterReplacement is buildStructuredReplacementPrompt for one writing
// language: the fixed lead, the partial-answer lead, the fallback the reference
// uses when the previous attempt produced no visible text, and the closing
// requirements.
type LegacyRosterReplacement struct {
	// Language is the writing language this piece was shipped for.
	Language string
	// OriginalLead precedes the original task's user prompt.
	OriginalLead string
	// PartialLead precedes the visible incomplete output.
	PartialLead string
	// Fallback replaces an empty visible partial (the reference's `||` arm).
	Fallback string
	// Suffix closes the prompt after the visible partial.
	Suffix string
}

// Render is buildStructuredReplacementPrompt's string interpolation without the
// language switch, which Replacement already made.
func (r LegacyRosterReplacement) Render(originalPrompt, visiblePartial string) string {
	visible := visiblePartial
	if visible == "" {
		visible = r.Fallback
	}
	return r.OriginalLead + originalPrompt + r.PartialLead + visible + r.Suffix
}

// LegacyRosterLoopPrompt is what the loop's two non-happy branches compose.
type LegacyRosterLoopPrompt struct {
	// RepairSystem is CHARACTER_ROSTER_JSON_REPAIR_SYSTEM, byte for byte.
	RepairSystem string
	// RepairLead follows CHARACTER_ROSTER_JSON_CONTRACT and precedes the raw
	// text inside the <invalid-json> element.
	RepairLead string
	// RepairTail closes the <invalid-json> element.
	RepairTail string
	// replacements is keyed by writing language; it is unexported so a caller
	// cannot read a language the asset did not carry.
	replacements map[string]LegacyRosterReplacement
}

// Replacement answers buildStructuredReplacementPrompt's language switch. A
// language the asset does not ship is ErrAsset rather than the zh-CN arm, so a
// run can never freeze a prompt for a language the reference would not have
// written.
func (p LegacyRosterLoopPrompt) Replacement(language string) (LegacyRosterReplacement, error) {
	replacement, ok := p.replacements[language]
	if !ok {
		return LegacyRosterReplacement{}, ErrAsset
	}
	return replacement, nil
}

// Repair composes buildCharacterRosterJsonRepairPrompt: the immutable roster
// contract, the shipped lead, the unparsable text and the shipped tail. The
// contract comes from the roster asset, so the repair prompt and the initial
// task cannot disagree about the output shape.
func (p LegacyRosterLoopPrompt) Repair(rawText string) (string, error) {
	if p.RepairSystem == "" || p.RepairLead == "" || p.RepairTail == "" {
		return "", ErrAsset
	}
	roster, err := LegacyRosterTemplate()
	if err != nil {
		return "", ErrAsset
	}
	return roster.Contract + p.RepairLead + rawText + p.RepairTail, nil
}

//go:embed legacy-roster-loop-v1.json
var legacyRosterLoopAsset []byte

// legacyRosterLoopReplacementDocument is one language's four shipped pieces.
type legacyRosterLoopReplacementDocument struct {
	OriginalLead string `json:"originalLead"`
	PartialLead  string `json:"partialLead"`
	Fallback     string `json:"fallback"`
	Suffix       string `json:"suffix"`
}

// legacyRosterLoopDocument is the asset's own shape.
type legacyRosterLoopDocument struct {
	FormatVersion int                                            `json:"formatVersion"`
	Kind          string                                         `json:"kind"`
	Key           string                                         `json:"key"`
	RepairSystem  string                                         `json:"repairSystem"`
	RepairLead    string                                         `json:"repairLead"`
	RepairTail    string                                         `json:"repairTail"`
	Replacement   map[string]legacyRosterLoopReplacementDocument `json:"replacement"`
}

// LegacyRosterLoopTemplate returns the shipped loop prompts. Every failure is
// ErrAsset: the caller refuses the operation rather than compose a prompt this
// package could not prove is the shipped one.
func LegacyRosterLoopTemplate() (LegacyRosterLoopPrompt, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, legacyRosterLoopAsset); err != nil {
		return LegacyRosterLoopPrompt{}, ErrAsset
	}
	// The asset must already be compact, for the same reason the roster asset
	// must be: whitespace the renderer never writes would be frozen with it.
	if !bytes.Equal(compact.Bytes(), legacyRosterLoopAsset) {
		return LegacyRosterLoopPrompt{}, ErrAsset
	}
	var document legacyRosterLoopDocument
	if err := json.Unmarshal(legacyRosterLoopAsset, &document); err != nil {
		return LegacyRosterLoopPrompt{}, ErrAsset
	}
	if document.FormatVersion != 1 || document.Kind != "legacy-roster-loop" || document.Key != "legacy-roster" {
		return LegacyRosterLoopPrompt{}, ErrAsset
	}
	if document.RepairSystem == "" || document.RepairLead == "" || document.RepairTail == "" {
		return LegacyRosterLoopPrompt{}, ErrAsset
	}
	// Both prose literals keep the template literal's leading newline and lose
	// the one before the closing backtick; a loader that normalized either would
	// change the task text the desktop hashes.
	if document.RepairSystem[0] != '\n' || document.RepairLead[0] != '\n' {
		return LegacyRosterLoopPrompt{}, ErrAsset
	}
	if document.RepairTail[0] != '\n' {
		return LegacyRosterLoopPrompt{}, ErrAsset
	}
	// Exactly the two languages the reference's WritingLanguage type admits. An
	// asset carrying a third would be prose this loader silently dropped, which
	// is the drift the shipped-source test could not see.
	if len(document.Replacement) != 2 {
		return LegacyRosterLoopPrompt{}, ErrAsset
	}
	replacements := make(map[string]LegacyRosterReplacement, 2)
	for _, language := range []string{LanguageChinese, LanguageEnglish} {
		entry, ok := document.Replacement[language]
		if !ok {
			return LegacyRosterLoopPrompt{}, ErrAsset
		}
		if entry.OriginalLead == "" || entry.PartialLead == "" || entry.Fallback == "" || entry.Suffix == "" {
			return LegacyRosterLoopPrompt{}, ErrAsset
		}
		replacements[language] = LegacyRosterReplacement{
			Language:     language,
			OriginalLead: entry.OriginalLead,
			PartialLead:  entry.PartialLead,
			Fallback:     entry.Fallback,
			Suffix:       entry.Suffix,
		}
	}
	return LegacyRosterLoopPrompt{
		RepairSystem: document.RepairSystem,
		RepairLead:   document.RepairLead,
		RepairTail:   document.RepairTail,
		replacements: replacements,
	}, nil
}
