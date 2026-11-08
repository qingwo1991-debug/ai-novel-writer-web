// Package prompttemplate carries builtin prompt templates as committed JSON
// assets. This file is the legacy-roster half: the two prose literals the
// reference composes into buildLegacyRosterTask
// (src/shared/legacy-roster-generation-pure.ts:55-66).
//
// legacy_roster does NOT ship in src/services/builtin-prompt-templates.ts. The
// roster prompt is authored in src/shared/legacy-roster-generation-pure.ts as
// two template-literal constants - LEGACY_ROSTER_SYSTEM_PROMPT and
// CHARACTER_ROSTER_JSON_CONTRACT - so the asset beside this file is generated
// from that source, exactly like the finalization assets. Each value is the
// exact template-literal text, including its leading newline and nothing else,
// so a Go run freezes the digest of the shipped prose rather than of a Go
// re-serialization of it. The package test re-reads the TypeScript source so an
// asset that drifted from it fails instead of freezing a prompt nobody ships.
package prompttemplate

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"strings"
)

// LegacyRosterPrompt is the prompt contract for the legacy roster repair: the
// system prompt and the immutable output contract the reference composes around
// the project's preserved markdown and genre
// (src/shared/legacy-roster-generation-pure.ts:55-66). Every fragment comes from
// the asset, never from a Go literal.
type LegacyRosterPrompt struct {
	// SystemPrompt is LEGACY_ROSTER_SYSTEM_PROMPT, byte for byte.
	SystemPrompt string
	// Contract is CHARACTER_ROSTER_JSON_CONTRACT, byte for byte.
	Contract string
}

//go:embed legacy-roster-v1.json
var legacyRosterAsset []byte

// legacyRosterDocument is the asset's own shape.
type legacyRosterDocument struct {
	FormatVersion int    `json:"formatVersion"`
	Kind          string `json:"kind"`
	Key           string `json:"key"`
	SystemPrompt  string `json:"systemPrompt"`
	Contract      string `json:"contract"`
}

// LegacyRosterTemplate returns the shipped legacy roster prompt. Every failure
// is ErrAsset: the caller should refuse the operation rather than compose a
// prompt this package could not prove is the shipped one.
func LegacyRosterTemplate() (LegacyRosterPrompt, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, legacyRosterAsset); err != nil {
		return LegacyRosterPrompt{}, ErrAsset
	}
	// The asset must already be compact: a pretty-printed asset would freeze
	// whitespace the renderer never writes, and the digest over the prose would
	// then be a digest of this file's formatting.
	if !bytes.Equal(compact.Bytes(), legacyRosterAsset) {
		return LegacyRosterPrompt{}, ErrAsset
	}
	var document legacyRosterDocument
	if err := json.Unmarshal(legacyRosterAsset, &document); err != nil {
		return LegacyRosterPrompt{}, ErrAsset
	}
	if document.FormatVersion != 1 || document.Kind != "legacy-roster-generation" || document.Key != "legacy-roster" {
		return LegacyRosterPrompt{}, ErrAsset
	}
	if document.SystemPrompt == "" || document.Contract == "" {
		return LegacyRosterPrompt{}, ErrAsset
	}
	// Both literals are template literals whose value begins with the newline
	// after the opening backtick and ends with the character before the closing
	// backtick; a loader that normalized either would change the task text the
	// desktop hashes.
	if !strings.HasPrefix(document.SystemPrompt, "\n") || !strings.HasPrefix(document.Contract, "\n") {
		return LegacyRosterPrompt{}, ErrAsset
	}
	return LegacyRosterPrompt{SystemPrompt: document.SystemPrompt, Contract: document.Contract}, nil
}
