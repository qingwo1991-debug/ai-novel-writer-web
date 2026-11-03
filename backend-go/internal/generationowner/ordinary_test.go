package generationowner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/secrets"
)

type ordinaryTestLedger struct {
	mu    sync.Mutex
	calls []string
	run   map[string]any
}

func (l *ordinaryTestLedger) CallJSON(_ context.Context, method string, _ json.RawMessage) (any, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, method)
	switch method {
	case "findInvocation":
		return nil, nil
	case "get":
		return l.run, nil
	case "reserve":
		return nil, errors.New("ROOT_BUDGET_EXHAUSTED")
	default:
		return nil, errors.New("unexpected ledger method")
	}
}
func (l *ordinaryTestLedger) count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.calls) }

func ordinaryFixture(t *testing.T) (*OrdinaryService, *Frozen, *ordinaryTestLedger, *SourceValue) {
	t.Helper()
	_, registry := newSettings(t)
	storage, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	client, err := egress.New([]string{"https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	executor := &Executor{Registry: registry, SecretStorage: storage, Egress: client}
	receipt := begin(t, registry)
	source := &SourceValue{Revision: 1, Text: "PUBLIC admitted source"}
	binding, err := registry.BindSources(context.Background(), publicScope, receipt.LeaseID, Sources{
		ProjectCore: func(context.Context, Scope, SourceKey) (SourceValue, error) { return *source, nil },
	}, []SourceKey{{Kind: "project-core", ID: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := executor.Prepare(context.Background(), publicScope, receipt.LeaseID, publicTask(), 1024, binding)
	if err != nil {
		t.Fatal(err)
	}
	var refs any
	raw, _ := json.Marshal(binding.Refs())
	if json.Unmarshal(raw, &refs) != nil {
		t.Fatal("refs")
	}
	ledger := &ordinaryTestLedger{run: map[string]any{"rootActionId": "root", "binding": map[string]any{
		"projectId": publicScope.Project, "epoch": publicScope.Epoch,
		"sourceManifest": map[string]any{"goOwnerSourceFingerprint": binding.Fingerprint()}, "sourceRefs": refs,
	}}}
	service, err := executor.NewOrdinaryService(ledger, OrdinaryCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	return service, frozen, ledger, source
}

func TestOrdinaryServiceChecksRunBindingBeforeReservation(t *testing.T) {
	service, frozen, ledger, source := ordinaryFixture(t)
	_, err := service.Execute(context.Background(), frozen, "run-1", "nonce-1")
	if err == nil || err.Error() != "ROOT_BUDGET_EXHAUSTED" {
		t.Fatalf("valid bound run: %v", err)
	}
	if ledger.count() != 4 {
		t.Fatalf("expected lookup, two bound reads, reserve; calls=%d", ledger.count())
	}
	ledger.run["binding"].(map[string]any)["sourceManifest"].(map[string]any)["goOwnerSourceFingerprint"] = "wrong-root"
	prior := ledger.count()
	_, err = service.Execute(context.Background(), frozen, "run-2", "nonce-2")
	if err == nil || err.Error() != "GENERATION_BINDING_CONFLICT" || ledger.count() != prior+2 {
		t.Fatalf("wrong root reached reserve: err=%v calls=%d", err, ledger.count())
	}
	source.Text = "PUBLIC source changed"
	prior = ledger.count()
	_, err = service.Execute(context.Background(), frozen, "run-3", "nonce-3")
	if err == nil || err.Error() != "GENERATION_SOURCE_CHANGED" || ledger.count() != prior {
		t.Fatalf("stale source reached ledger: err=%v calls=%d", err, ledger.count())
	}
}

func TestOrdinaryServiceRejectsUnboundAndCancelledAdmission(t *testing.T) {
	service, frozen, ledger, _ := ordinaryFixture(t)
	prior := ledger.count()
	_, err := service.Execute(context.Background(), nil, "run", "nonce")
	if err == nil || err.Error() != "GENERATION_DISPATCH_AUTHORITY_INVALID" {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.Execute(ctx, frozen, "run", "nonce")
	if err == nil || err.Error() != "GENERATION_CANCELLED" || ledger.count() != prior {
		t.Fatalf("cancel reached ledger: %v", err)
	}
}
