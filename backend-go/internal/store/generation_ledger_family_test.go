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

// The receipt keys are only spellable for the operation that owns them, and the
// eight original keys plus every unknown key keep the original refusal.
func TestGlEffectKeyAllowedStaysOperationScoped(t *testing.T) {
	admitted := []struct{ key, operation string }{
		{"graphEffects", "plot-tree-snapshot"},
		{"graphEffects", "narrative-thread-plan-candidate"},
		{"graphEffects", "narrative-thread-event-candidate"},
		{"graphEffects", "finalized-chapter-notes"},
		{"finalizationEffect", "finalized-character-state"},
	}
	for _, c := range admitted {
		if !glEffectKeyAllowed(c.key, c.operation) {
			t.Fatalf("%s must be allowed for %s", c.key, c.operation)
		}
	}
	refused := []struct{ key, operation string }{
		{"graphEffects", "chapter-draft"},
		{"graphEffects", "finalized-character-state"},
		{"finalizationEffect", "plot-tree-snapshot"},
		{"finalizationEffect", "chapter-draft"},
		{"result", "plot-tree-snapshot"},
		{"graphEffect", "plot-tree-snapshot"},
	}
	for _, c := range refused {
		if glEffectKeyAllowed(c.key, c.operation) {
			t.Fatalf("%s must stay refused for %s", c.key, c.operation)
		}
	}
}
