package prompttemplate

import (
	"os"
	"strings"
	"testing"
)

// The legacy roster asset is what a Go lane freezes into a run binding, so these
// tests read it the way the lane does and then check it against the TypeScript
// source it was generated from. The point is not that the JSON parses; it is
// that the two prose literals a Go run writes down are the ones the renderer
// ships, byte for byte, and that none of it lives in Go source.

const shippedLegacyRosterSource = "../../../src/shared/legacy-roster-generation-pure.ts"

// TestLegacyRosterTemplateServesTheShippedProse proves the two literals resolve,
// that each carries the template literal's leading newline, and that the two are
// distinct.
func TestLegacyRosterTemplateServesTheShippedProse(t *testing.T) {
	prompt, err := LegacyRosterTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if prompt.SystemPrompt == "" || prompt.Contract == "" {
		t.Fatal("the legacy roster prompt is incomplete")
	}
	if !strings.HasPrefix(prompt.SystemPrompt, "\n") || !strings.HasPrefix(prompt.Contract, "\n") {
		t.Fatal("the legacy roster prose lost its leading newline")
	}
	if strings.HasSuffix(prompt.SystemPrompt, "\n") || strings.HasSuffix(prompt.Contract, "\n") {
		t.Fatal("the legacy roster prose gained a trailing newline the template literal does not carry")
	}
	if prompt.SystemPrompt == prompt.Contract {
		t.Fatal("the system prompt and the contract froze the same text")
	}
	if !strings.Contains(prompt.SystemPrompt, "结构化迁移器") || !strings.Contains(prompt.Contract, "schemaVersion") {
		t.Fatal("the legacy roster prose no longer carries the reference's markers")
	}
}

// TestLegacyRosterAssetTracksTheShippedSource is the drift check: every prose
// literal in the asset must appear verbatim in the file the asset cites, so an
// asset that stopped matching the renderer fails here instead of freezing a
// prompt nobody ships.
func TestLegacyRosterAssetTracksTheShippedSource(t *testing.T) {
	raw, err := os.ReadFile(shippedLegacyRosterSource)
	if err != nil {
		t.Fatalf("%v", err)
	}
	source := string(raw)
	prompt, err := LegacyRosterTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	for label, value := range map[string]string{
		"system prompt": prompt.SystemPrompt,
		"contract":      prompt.Contract,
	} {
		if !legacyRosterSourceContains(source, value) {
			t.Fatalf("the asset carries a %s that %s no longer ships: the committed corpus drifted from the renderer",
				label, shippedLegacyRosterSource)
		}
	}
	// The two functions the run freezes must still be spelled in the source.
	for _, key := range []string{"export function buildLegacyRosterTask(", "export function parseLegacyRosterJson("} {
		if !strings.Contains(source, key) {
			t.Fatalf("%s no longer ships %s", shippedLegacyRosterSource, key)
		}
	}
}

// legacyRosterSourceContains checks that a decoded prose literal still appears
// in its shipped source. The literals are template literals, so a backtick in
// the content would be stored escaped (\`) in the source while the asset's
// decoded value carries the literal character; both spellings are checked so a
// source that switched escaping is not a false drift.
func legacyRosterSourceContains(source, value string) bool {
	if strings.Contains(source, value) {
		return true
	}
	return strings.Contains(source, strings.ReplaceAll(value, "`", "\\`"))
}
