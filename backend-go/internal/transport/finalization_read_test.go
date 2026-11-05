package transport

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// finalization-generation:read takes `{ slot }`, not `{ handle }`, and it is a
// read: its refusals leave through the HTTP error envelope, never the mutating
// business DTO. Both halves are pinned here, because a slot read that only got
// the binding right would refuse every real call while a "not 501" assertion
// still passed (electron/controllers/generation-controller.ts:223 registers the
// whole finalization family through the same adapter, and
// electron/services/main-generation-owner.ts:1236 rejects any key but `slot`
// before it looks at the run).
func TestFinalizationReadUnwrapsTheSlotEnvelope(t *testing.T) {
	expected := map[string]any{"projectId": "project", "leaseId": "lease", "projectPath": "/project"}
	slot := map[string]any{"stepKey": "chapter_notes", "source": map[string]any{"draftId": float64(1)}}
	var received any
	server := fixture(t)
	if err := server.BindGraphGeneration(&GraphGenerationProvider{
		Read: func(context.Context, Caller, map[string]any, any) (any, error) {
			return nil, errors.New("GENERATION_RUN_NOT_FOUND")
		},
		FinalizationRead: func(_ context.Context, _ Caller, _ map[string]any, arg any) (any, error) {
			received = arg
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	handler := server.handlers["finalization-generation:read"]
	if handler == nil {
		t.Fatal("finalization-generation:read is not bound")
	}
	value, err := handler(context.Background(), Caller{}, []any{map[string]any{"slot": slot}, expected})
	if err != nil {
		t.Fatalf("finalization-generation:read: %v", err)
	}
	if value != nil {
		t.Fatalf("absent slot projection = %#v, want null", value)
	}
	if !reflect.DeepEqual(received, slot) {
		t.Fatalf("finalization-generation:read passed %#v to the provider, want the unwrapped slot", received)
	}

	for _, rejection := range []struct {
		name     string
		envelope any
	}{
		{"an extra key", map[string]any{"slot": slot, "handle": slot}},
		{"no slot", map[string]any{"handle": slot}},
		{"a bare slot", slot},
		{"a null request", nil},
	} {
		value, err := handler(context.Background(), Caller{}, []any{rejection.envelope, expected})
		if err == nil {
			t.Fatalf("finalization-generation:read with %s was accepted: %#v", rejection.name, value)
		}
		if !strings.Contains(err.Error(), "GENERATION_FINALIZATION_SLOT_INVALID") {
			t.Fatalf("finalization-generation:read with %s = %v, want GENERATION_FINALIZATION_SLOT_INVALID", rejection.name, err)
		}
	}
}

// A provider that does not implement the slot read must leave the channel
// unbound rather than bound-but-unsupported, so the lane keeps answering 501
// GO_OPERATION_NOT_MIGRATED for every finalization name it cannot serve.
func TestFinalizationReadStaysUnboundWithoutAnImplementation(t *testing.T) {
	server := fixture(t)
	if err := server.BindGraphGeneration(&GraphGenerationProvider{
		Read: func(context.Context, Caller, map[string]any, any) (any, error) {
			return nil, errors.New("GENERATION_RUN_NOT_FOUND")
		},
	}); err != nil {
		t.Fatal(err)
	}
	if handler := server.handlers["finalization-generation:read"]; handler != nil {
		t.Fatal("finalization-generation:read is bound without an implementation")
	}
	if handler := server.handlers["graph-generation:read"]; handler == nil {
		t.Fatal("graph-generation:read is not bound")
	}
}
