package store

import "testing"

// The manifest is closed by default. This pins the only widening: a family key
// is readable only for the operation that owns it, and the effect keys and the
// remaining family keys stay refused for every operation.
func TestGlOrdinaryBindingScopesFamilyKeysToTheirOperation(t *testing.T) {
	admitted := []struct{ operation, key string }{
		{"agent-round", "agentSession"},
		{"plot-tree-snapshot", "graphGenerationContext"},
		{"narrative-thread-plan-candidate", "graphGenerationContext"},
		{"narrative-thread-event-candidate", "graphGenerationContext"},
		{"finalized-chapter-notes", "graphGenerationContext"},
		{"finalized-character-state", "finalizationGenerationSlotKey"},
	}
	for _, c := range admitted {
		binding := map[string]any{"sourceManifest": map[string]any{
			"operation": c.operation,
			c.key:       map[string]any{"synthetic": true},
		}}
		if err := glOrdinaryBinding(binding); err != nil {
			t.Fatalf("%s with %s must be admitted, got %v", c.operation, c.key, err)
		}
	}
	refused := []struct{ operation, key string }{
		{"chapter-draft", "agentSession"},
		{"agent-round", "graphGenerationContext"},
		{"plot-tree-snapshot", "finalizationGenerationSlotKey"},
		{"finalized-character-state", "graphGenerationContext"},
		{"finalized-character-state", "agentSession"},
		{"chapter-draft", "importSlot"},
		{"agent-round", "legacyRosterContext"},
	}
	for _, c := range refused {
		binding := map[string]any{"sourceManifest": map[string]any{
			"operation": c.operation,
			c.key:       map[string]any{"synthetic": true},
		}}
		err := glOrdinaryBinding(binding)
		if err == nil || err.Error() != "GENERATION_OPERATION_UNSUPPORTED" {
			t.Fatalf("%s with %s must stay refused, got %v", c.operation, c.key, err)
		}
	}
	for _, key := range []string{"$runEffects", "$blueprintRetirement"} {
		binding := map[string]any{"sourceManifest": map[string]any{"operation": "chapter-draft"}, key: true}
		err := glOrdinaryBinding(binding)
		if err == nil || err.Error() != "GENERATION_EFFECT_UNSUPPORTED" {
			t.Fatalf("%s must stay refused, got %v", key, err)
		}
	}
}
