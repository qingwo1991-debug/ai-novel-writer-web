package generationowner

import (
	"context"
	"errors"
	"log"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
)

// CredentialStorage is the credential seam Executor resolves through at the
// network boundary. *secrets.Storage satisfies it for the AES-GCM lane; the
// plaintext models.json apiKey field is served by PlaintextCredential. The key
// is never serialized: Profile.EncryptedCredential is json:"-" everywhere.
type CredentialStorage interface {
	DecryptString([]byte) (string, error)
}

// PlaintextCredential adapts the legacy plaintext models.json apiKey field to
// the credential seam. Documented shim for the durable generation lane; it
// never logs, stores, or serializes the key beyond the transient llm.Model.
type PlaintextCredential struct{}

func (PlaintextCredential) DecryptString(value []byte) (string, error) {
	if !utf8.Valid(value) {
		return "", failure("MODEL_CREDENTIAL_UNAVAILABLE")
	}
	return string(value), nil
}

// Executor has no ambient network, secret key, settings, or project authority.
// SecretStorage and Egress come from explicit administrator configuration.
// Registry.AuthorityLock is REQUIRED for actual dispatch. Current, LoadModel
// and SourceReader must not reacquire this non-reentrant shared owner gate.
// The gate serializes settings/project invalidation with request start through
// response headers; body streaming runs unlocked. Active-call cancellation on
// project close remains the embedding durable generation service's job.
// DispatchFunc is a synthetic test seam only: when set it replaces the whole
// native dispatch (authority checks, credential resolution and egress HTTP);
// production lanes leave it nil so egress remains the sole network client.
type Executor struct {
	Registry      *Registry
	SecretStorage CredentialStorage
	Egress        *egress.Client
	DispatchFunc  generation.Dispatch
}

// Frozen cannot be created or edited by RPC. Projections are detached copies.
type Frozen struct {
	registry *Registry
	leaseID  string
	scope    Scope
	purpose  string
	task     Task
	plan     Plan
	binding  *Binding
}

func (f *Frozen) Plan() Plan {
	if f == nil {
		return Plan{}
	}
	return clone(f.plan)
}
func (e *Executor) Prepare(ctx context.Context, scope Scope, leaseID string, task Task, rootOutput int64, binding *Binding) (*Frozen, error) {
	if e == nil || e.Registry == nil {
		return nil, failure("MODEL_EXECUTION_CONFIG_REQUIRED")
	}
	record, err := e.Registry.resolve(ctx, leaseID, scope, "generation")
	if err != nil {
		return nil, err
	}
	task = clone(task)
	plan, err := BuildPlan(record.profile, record.receipt, task, rootOutput)
	if err != nil {
		return nil, err
	}
	if binding != nil && (binding.scope != scope || binding.registry != e.Registry || binding.modelRevision != record.receipt.ModelRevision) {
		return nil, failure("GENERATION_BINDING_INVALID")
	}
	return &Frozen{registry: e.Registry, leaseID: leaseID, scope: scope, purpose: "generation", task: task, plan: clone(plan), binding: binding}, nil
}

// Dispatch fits generation.Dependencies.Dispatch, but is deliberately NOT
// installed into public handlers. Actual HTTP dispatch repeats lease identity,
// expiry, scope, revision/credential and optional bound-source checks under the
// REQUIRED shared revision/project gate. Exact-origin egress is the sole client.
// reserve/mark/settle/write belong to the existing durable generation service.
// refused logs the symbolic refusal code only. Paths, endpoints and credentials
// never enter it, exactly like the lane's egress refusal. Without it a failed
// dispatch reached the author as one opaque provider code.
func refused(code string) error {
	err := failure(code)
	log.Printf("novel-server: generation dispatch refused (%s)", err.Error())
	return err
}

// egressRefusal names the reason without echoing the endpoint a transport error
// would carry: the policy's own sentinels are symbolic, anything else is the
// transport.
func egressRefusal(err error) string {
	for _, known := range []error{egress.ErrNotAllowed, egress.ErrBaseURLInvalid, egress.ErrPolicyInvalid, egress.ErrRedirectForbidden} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "EGRESS_TRANSPORT"
}

func (e *Executor) Dispatch(ctx context.Context, request any, callbacks generation.Callbacks) (generation.DispatchResult, error) {
	if e == nil {
		return generation.DispatchResult{}, refused("MODEL_EXECUTION_CONFIG_REQUIRED")
	}
	if e.DispatchFunc != nil {
		return e.DispatchFunc(ctx, request, callbacks)
	}
	frozen, ok := request.(*Frozen)
	if !ok || frozen == nil || e.Registry == nil || frozen.registry != e.Registry {
		return generation.DispatchResult{}, refused("GENERATION_DISPATCH_AUTHORITY_INVALID")
	}
	if e.Registry.options.AuthorityLock == nil || e.Egress == nil || e.SecretStorage == nil {
		return generation.DispatchResult{}, refused("MODEL_EXECUTION_CONFIG_REQUIRED")
	}
	dispatch := func(req *http.Request) (*http.Response, error) {
		unlock, err := e.Registry.options.AuthorityLock(ctx, frozen.scope)
		if err != nil || unlock == nil {
			return nil, refused("GENERATION_EPOCH_STALE")
		}
		defer unlock()
		checked, err := e.Registry.resolve(ctx, frozen.leaseID, frozen.scope, frozen.purpose)
		if err != nil {
			return nil, err
		}
		if frozen.binding != nil {
			if err := frozen.binding.validate(ctx); err != nil {
				log.Printf("novel-server: generation dispatch refused (%s)", err.Error())
				return nil, err
			}
		}
		// Serialize Close/RevokeScope with actual request start as well. No external
		// injected callback runs under this registry mutex; only the owned egress.
		e.Registry.mu.Lock()
		defer e.Registry.mu.Unlock()
		if _, live := e.Registry.records[frozen.leaseID]; !live {
			return nil, refused("MODEL_EXECUTION_LEASE_INVALID")
		}
		if e.Registry.options.Now().UnixMilli() >= checked.receipt.ExpiresAt {
			return nil, refused("MODEL_EXECUTION_LEASE_EXPIRED")
		}
		response, err := e.Egress.Do(req)
		if err != nil {
			return nil, refused(egressRefusal(err))
		}
		return response, nil
	}
	record, err := e.Registry.resolve(ctx, frozen.leaseID, frozen.scope, frozen.purpose)
	if err != nil {
		log.Printf("novel-server: generation dispatch refused (%s)", err.Error())
		return generation.DispatchResult{}, err
	}
	key, err := e.SecretStorage.DecryptString(record.profile.EncryptedCredential)
	if err != nil {
		return generation.DispatchResult{}, refused("MODEL_CREDENTIAL_UNAVAILABLE")
	}
	model := llm.Model{Provider: record.profile.Provider, ModelName: record.profile.ModelName, BaseURL: record.profile.BaseURL, APIKey: key, MaxTokens: record.profile.MaxTokens}
	options := clone(frozen.plan.Options)
	options.VisibleOnly = true
	// Reuse the round25 native provider adapter: preserve raw Gemini candidates
	// versus thoughts and existing terminal/error/usage normalization semantics.
	return generation.NativeProvider(dispatch)(ctx, generation.ProviderRequest{Adapter: record.profile.Protocol, Model: model, Messages: frozen.task.Messages, Options: options}, callbacks)
}
