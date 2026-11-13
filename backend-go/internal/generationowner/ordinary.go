package generationowner

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
)

// OrdinaryService joins the admitted model/source owner to the ordinary
// generation ledger. The embedding server still owns the durable project
// session, immutable root binding, and the caller's whole-call lease.
type OrdinaryService struct {
	executor *Executor
	service  *generation.Service
}

type ordinaryContextKey struct{}
type ordinaryLedger struct{ inner generation.Ledger }

func (l ordinaryLedger) CallJSON(ctx context.Context, method string, args json.RawMessage) (any, error) {
	if method == "get" || method == "reserve" {
		frozen, ok := ctx.Value(ordinaryContextKey{}).(*Frozen)
		if !ok {
			return l.inner.CallJSON(ctx, method, args)
		}
		if frozen == nil || frozen.binding == nil {
			return nil, failure("GENERATION_BINDING_INVALID")
		}
		if method == "reserve" {
			var parameters []any
			if json.Unmarshal(args, &parameters) != nil || len(parameters) == 0 {
				return nil, failure("GENERATION_BINDING_INVALID")
			}
			check, _ := json.Marshal([]any{parameters[0]})
			run, err := l.inner.CallJSON(ctx, "get", check)
			if err != nil {
				return nil, err
			}
			if err := verifyOrdinaryRun(run, frozen); err != nil {
				return nil, err
			}
		}
		result, err := l.inner.CallJSON(ctx, method, args)
		if err != nil {
			return nil, err
		}
		if method == "get" {
			if err := verifyOrdinaryRun(result, frozen); err != nil {
				return nil, err
			}
		}
		return result, nil
	}
	return l.inner.CallJSON(ctx, method, args)
}

// The root owner writes this fingerprint and exact refs when it opens the run.
// A different root/run cannot borrow an otherwise valid model lease.
func verifyOrdinaryRun(result any, frozen *Frozen) error {
	run, ok := result.(map[string]any)
	if !ok {
		return errors.New("GENERATION_BINDING_CONFLICT")
	}
	binding, ok := run["binding"].(map[string]any)
	if !ok || binding["projectId"] != frozen.scope.Project || binding["epoch"] != frozen.scope.Epoch {
		return failure("GENERATION_BINDING_CONFLICT")
	}
	manifest, ok := binding["sourceManifest"].(map[string]any)
	if !ok || manifest["goOwnerSourceFingerprint"] != frozen.binding.Fingerprint() {
		return failure("GENERATION_BINDING_CONFLICT")
	}
	var refs, current any
	encoded, err := json.Marshal(frozen.binding.Refs())
	if err != nil || json.Unmarshal(encoded, &refs) != nil {
		return failure("GENERATION_BINDING_CONFLICT")
	}
	encoded, err = json.Marshal(binding["sourceRefs"])
	if err != nil || json.Unmarshal(encoded, &current) != nil || !reflect.DeepEqual(current, refs) {
		return failure("GENERATION_BINDING_CONFLICT")
	}
	return nil
}

type OrdinaryCallbacks struct {
	OnSnapshot  func(generation.Snapshot)
	OnReasoning func(generation.ReasoningEvent)
	// ValidateRecovery authorizes generation:resume. When nil the service
	// remains default-deny, so a durable lane that wants resume must supply it.
	ValidateRecovery func(context.Context, map[string]any, map[string]any) (bool, error)
}

// NewOrdinaryService always installs the owner's native dispatch. The caller
// cannot substitute a renderer-supplied provider, usage policy, or HTTP client.
func (e *Executor) NewOrdinaryService(ledger generation.Ledger, callbacks OrdinaryCallbacks) (*OrdinaryService, error) {
	if e == nil || e.Registry == nil || e.Egress == nil || e.SecretStorage == nil || ledger == nil || e.Registry.options.AuthorityLock == nil {
		return nil, failure("MODEL_EXECUTION_CONFIG_REQUIRED")
	}
	return &OrdinaryService{executor: e, service: generation.New(generation.Dependencies{
		Ledger: ordinaryLedger{inner: ledger}, Dispatch: e.Dispatch, OnSnapshot: callbacks.OnSnapshot, OnReasoning: callbacks.OnReasoning,
		ValidateRecovery: callbacks.ValidateRecovery,
	})}, nil
}

var ordinaryIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_-]{0,127}$`)

// Execute requires a source-bound Frozen produced by this executor. RunID and
// nonce must come from an admitted root/run owner; no browser field can supply
// them directly. Revalidation occurs before the ledger reservation and again
// inside Executor.Dispatch at the network boundary.
func (s *OrdinaryService) Execute(ctx context.Context, frozen *Frozen, runID, nonce string) (map[string]any, error) {
	if s == nil || s.executor == nil || s.service == nil || frozen == nil || frozen.binding == nil ||
		frozen.registry != s.executor.Registry || frozen.binding.registry != s.executor.Registry || frozen.binding.scope != frozen.scope ||
		!ordinaryIdentity.MatchString(runID) || !ordinaryIdentity.MatchString(nonce) {
		return nil, failure("GENERATION_DISPATCH_AUTHORITY_INVALID")
	}
	if ctx == nil || ctx.Err() != nil {
		return nil, failure("GENERATION_CANCELLED")
	}
	if _, err := s.executor.Registry.resolve(ctx, frozen.leaseID, frozen.scope, frozen.purpose); err != nil {
		return nil, err
	}
	if err := frozen.binding.validate(ctx); err != nil {
		return nil, err
	}
	usage, err := json.Marshal(frozen.plan.UsagePolicy)
	if err != nil {
		return nil, failure("GENERATION_RESERVATION_INVALID")
	}
	var policy map[string]any
	if err := json.Unmarshal(usage, &policy); err != nil {
		return nil, failure("GENERATION_RESERVATION_INVALID")
	}
	hash := hashJSON(struct {
		RunID   string `json:"runId"`
		Nonce   string `json:"invocationNonce"`
		LeaseID string `json:"modelLeaseId"`
		Binding string `json:"sourceFingerprint"`
		Task    Task   `json:"task"`
		Plan    Plan   `json:"plan"`
	}{runID, nonce, frozen.leaseID, frozen.binding.Fingerprint(), frozen.task, frozen.plan})
	if hash == "" {
		return nil, failure("GENERATION_REQUEST_HASH_INVALID")
	}
	// The frozen task is the reference's replayTask: recorded on the attempt's
	// usage receipt so a faithful retry can read it back and re-dispatch it
	// instead of authoring a new message. It is serialised as raw JSON so the
	// ledger's ordered writer preserves the task's exact field order.
	taskRaw, err := json.Marshal(frozen.task)
	if err != nil {
		return nil, failure("GENERATION_SEMANTIC_TASK_INVALID")
	}
	return s.service.Execute(context.WithValue(ctx, ordinaryContextKey{}, frozen), generation.Request{
		RunID: runID, InvocationNonce: nonce, RequestHash: hash,
		ProviderRequest: frozen,
		ReservedTokens:  frozen.plan.Reserved, RequestedOutputTokens: frozen.plan.Output,
		InputUpperBoundTokens: frozen.plan.Input, ReasoningUpperBoundTokens: frozen.plan.Reasoning,
		UsagePolicy: policy, Purpose: frozen.task.Purpose, BudgetDecision: frozen.plan.BudgetDecision,
		ReplayTask: json.RawMessage(taskRaw),
	})
}

// SuspendForProjectClose fences active dispatch and flushes candidate text
// before the embedding project owner closes its database and epoch. It does
// not itself close an author project or grant candidate commit authority.
func (s *OrdinaryService) SuspendForProjectClose(ctx context.Context) ([]map[string]any, error) {
	if s == nil || s.service == nil {
		return nil, failure("MODEL_EXECUTION_CONFIG_REQUIRED")
	}
	return s.service.SuspendForProjectClose(ctx)
}

// The accessors below expose the ordinary service to the durable generation
// lane (slice S1). They are deliberately thin: no caller-supplied root, run or
// attempt identity is ever accepted; the embedding owner still owns admission,
// binding verification and the whole-call lease.

// Pause parks an admitted root's active dispatch without cancelling it.
func (s *OrdinaryService) Pause(ctx context.Context, rootID string) (map[string]any, error) {
	if s == nil || s.service == nil {
		return nil, failure("MODEL_EXECUTION_CONFIG_REQUIRED")
	}
	return s.service.Pause(ctx, rootID)
}

// Cancel stops an admitted root and marks it cancelled.
func (s *OrdinaryService) Cancel(ctx context.Context, rootID string) (map[string]any, error) {
	if s == nil || s.service == nil {
		return nil, failure("MODEL_EXECUTION_CONFIG_REQUIRED")
	}
	return s.service.Cancel(ctx, rootID)
}

// Resume continues a paused admitted run with the owner-rebuilt next binding.
// Default-deny without a ValidateRecovery callback on this service.
func (s *OrdinaryService) Resume(ctx context.Context, runID string, next json.RawMessage) (map[string]any, error) {
	if s == nil || s.service == nil {
		return nil, failure("MODEL_EXECUTION_CONFIG_REQUIRED")
	}
	return s.service.Resume(ctx, runID, next)
}

// DiscardCandidate acknowledges one admitted candidate artifact, including its
// retained in-memory tail.
func (s *OrdinaryService) DiscardCandidate(ctx context.Context, artifactID string) (map[string]any, error) {
	if s == nil || s.service == nil {
		return nil, failure("MODEL_EXECUTION_CONFIG_REQUIRED")
	}
	return s.service.DiscardCandidate(ctx, artifactID)
}

// ReadUnsavedTails reports retained candidate tails that a refused project
// close could not flush; the view projection filters them by attempt identity.
func (s *OrdinaryService) ReadUnsavedTails() []generation.UnsavedTail {
	if s == nil || s.service == nil {
		return nil
	}
	return s.service.ReadUnsavedTails()
}
