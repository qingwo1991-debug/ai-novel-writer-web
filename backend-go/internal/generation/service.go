// Package generation contains an INTERNAL, trusted ordinary-generation service.
// It is not RPC, project admission, a model/URL policy owner, or author-write
// authority. Agent tool protocols, planning/blueprint/effect workflows, portable
// projects, composition, and automatic recovery authorization are unsupported.
// The caller supplies a captured ordinary-project ledger and frozen provider
// request; never expose these interfaces directly to an untrusted client.
package generation

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Ledger interface {
	CallJSON(context.Context, string, json.RawMessage) (any, error)
}
type VisibleEvent struct {
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	EventID string `json:"eventId,omitempty"`
}
type DispatchResult struct {
	Usage        map[string]any `json:"usage"`
	FinishReason string         `json:"finishReason"`
}
type Callbacks struct {
	OnVisible     func(VisibleEvent)
	OnReasoning   func(string)
	OnDiagnostics func(map[string]any)
}
type Dispatch func(context.Context, any, Callbacks) (DispatchResult, error)
type Request struct {
	RunID                     string         `json:"runId"`
	InvocationNonce           string         `json:"invocationNonce"`
	RequestHash               string         `json:"requestHash"`
	ProviderRequest           any            `json:"providerRequest"`
	ReservedTokens            int64          `json:"reservedTokens"`
	RequestedOutputTokens     int64          `json:"requestedOutputTokens"`
	InputUpperBoundTokens     int64          `json:"inputUpperBoundTokens"`
	ReasoningUpperBoundTokens int64          `json:"reasoningUpperBoundTokens"`
	UsagePolicy               map[string]any `json:"usagePolicy"`
	Purpose                   string         `json:"purpose,omitempty"`
	AgentToolNames            []string       `json:"agentToolNames,omitempty"`
	ReplayTask                any            `json:"replayTask,omitempty"`
	BudgetDecision            any            `json:"budgetDecision,omitempty"`
	ReconciliationInjected    bool           `json:"reconciliationInjected,omitempty"`
}
type Snapshot struct {
	AttemptID       string         `json:"attemptId"`
	VisibleText     string         `json:"visibleText"`
	DurableText     string         `json:"durableText"`
	DurableRevision int64          `json:"durableRevision"`
	StorageFailed   bool           `json:"storageFailed"`
	Diagnostics     map[string]any `json:"diagnostics,omitempty"`
}
type ReasoningEvent struct {
	RunID        string `json:"runId"`
	RootActionID string `json:"rootActionId"`
	AttemptID    string `json:"attemptId"`
	Text         string `json:"text"`
}
type UnsavedTail struct {
	AttemptID       string `json:"attemptId"`
	ArtifactID      string `json:"artifactId"`
	DurableRevision int64  `json:"durableRevision"`
	Text            string `json:"text"`
	FailureCode     string `json:"failureCode"`
}
type Dependencies struct {
	Ledger           Ledger
	Dispatch         Dispatch
	OnSnapshot       func(Snapshot)
	OnReasoning      func(ReasoningEvent)
	ValidateRecovery func(context.Context, map[string]any, map[string]any) (bool, error)
	Now              func() time.Time
	FlushInterval    time.Duration
}
type invocation struct {
	hash    string
	done    chan struct{}
	receipt map[string]any
	err     error
}
type execution struct {
	mu                                                                            sync.Mutex
	runID, rootID, attemptID, artifactID                                          string
	text, durable                                                                 string
	revision                                                                      int64
	receipt                                                                       map[string]any
	diagnostics                                                                   map[string]any
	events                                                                        map[string]VisibleEvent
	cancel                                                                        context.CancelFunc
	ctx                                                                           context.Context
	terminal, finished, suspended, storageFailed, streamFailed, stopped, timedOut bool
}
type Service struct {
	deps Dependencies
	// gate makes reserve/register/mark atomic with root stop and project close.
	gate        sync.Mutex
	ledgerMu    sync.Mutex
	mu          sync.Mutex
	pending     map[string]*invocation
	active      map[string]*execution
	failedRoots map[string]bool
	tails       map[string]UnsavedTail
	closed      bool
}

func New(deps Dependencies) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.FlushInterval <= 0 {
		deps.FlushInterval = time.Second
	}
	return &Service{deps: deps, pending: map[string]*invocation{}, active: map[string]*execution{}, failedRoots: map[string]bool{}, tails: map[string]UnsavedTail{}}
}
func smap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func stext(v any) string        { s, _ := v.(string); return s }
func sint(v any) (int64, bool) {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	case json.Number:
		var err error
		f, err = n.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f || math.Abs(f) > 9007199254740991 {
		return 0, false
	}
	return int64(f), true
}
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, v := range x {
			out[i] = cloneMap(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = cloneValue(v)
		}
		return out
	default:
		return x
	}
}
func serviceError(code string) error { return errors.New(code) }

// An exact source-code allowlist, not a prefix/regexp pass-through: arbitrary
// sqlite/provider messages (including apparent code prefixes) never escape.
func safeLedgerError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return serviceError("GENERATION_CANCELLED")
	}
	switch err.Error() {
	case "GENERATION_REQUEST_HASH_INVALID", "GENERATION_RESERVATION_INVALID", "GENERATION_RUN_MISSING", "GENERATION_ROOT_MISSING", "GENERATION_ATTEMPT_MISSING", "GENERATION_INVOCATION_CONFLICT", "GENERATION_INVOCATION_INVALID", "GENERATION_DISPATCH_IN_FLIGHT", "GENERATION_DISPATCH_BLOCKED", "GENERATION_EPOCH_STALE", "GENERATION_OPERATION_UNSUPPORTED", "GENERATION_EFFECT_UNSUPPORTED", "GENERATION_BINDING_INVALID", "GENERATION_BINDING_CONFLICT", "GENERATION_JSON_INVALID", "GENERATION_JSON_ORDER_REQUIRED", "GENERATION_INPUT_INVALID", "GENERATION_RECOVERY_UNAUTHORIZED", "GENERATION_MATERIAL_DECISION_TOO_LATE", "PORTABLE_RUNTIME_FROZEN", "ROOT_BUDGET_EXHAUSTED", "INVALID_ROOT_ACTION", "INVALID_BUDGET", "INVALID_RESERVATION", "INVALID_OUTPUT_RESERVATION", "RESERVATION_CONFLICT", "INVALID_LEDGER", "INVALID_ATTEMPT_TRANSITION", "ARTIFACT_INTEGRITY_FAILED", "ARTIFACT_SNAPSHOT_CONFLICT", "ARTIFACT_ATTEMPT_TERMINAL":
		return serviceError(err.Error())
	default:
		return serviceError("GENERATION_STORAGE_FAILED")
	}
}
func expectedReservationError(err error) bool {
	if err == nil {
		return false
	}
	switch err.Error() {
	case "GENERATION_CANCELLED", "ROOT_BUDGET_EXHAUSTED", "INVALID_BUDGET", "INVALID_RESERVATION", "INVALID_OUTPUT_RESERVATION", "RESERVATION_CONFLICT", "GENERATION_INVOCATION_CONFLICT", "GENERATION_INVOCATION_INVALID", "GENERATION_DISPATCH_IN_FLIGHT", "GENERATION_DISPATCH_BLOCKED", "GENERATION_EPOCH_STALE", "GENERATION_OPERATION_UNSUPPORTED", "GENERATION_EFFECT_UNSUPPORTED", "PORTABLE_RUNTIME_FROZEN":
		return true
	default:
		return false
	}
}
func (s *Service) call(ctx context.Context, method string, args ...any) (any, error) {
	if s.deps.Ledger == nil {
		return nil, serviceError("GENERATION_STORAGE_FAILED")
	}
	if args == nil {
		args = []any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, serviceError("GENERATION_INPUT_INVALID")
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	out, err := s.deps.Ledger.CallJSON(ctx, method, raw)
	return out, safeLedgerError(err)
}
func (s *Service) cleanupCall(method string, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.call(ctx, method, args...)
}

var requestHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var visibleThinkPattern = regexp.MustCompile(`(?i)</?think\b`)

func serviceTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0 && !math.IsNaN(x)
	case json.Number:
		n, err := x.Float64()
		return err == nil && n != 0 && !math.IsNaN(n)
	default:
		return true
	}
}
func validateRequest(r Request) error {
	if !requestHashPattern.MatchString(r.RequestHash) {
		return serviceError("GENERATION_REQUEST_HASH_INVALID")
	}
	// A present empty Agent tool manifest is still a protocol request, not ordinary text.
	if r.AgentToolNames != nil {
		return serviceError("GENERATION_OPERATION_UNSUPPORTED")
	}
	p := r.UsagePolicy
	margin, ok := sint(p["safetyMarginTokens"])
	_, bounded := p["canBoundTotalLiability"].(bool)
	if !ok || margin < 0 || !bounded || !serviceTruthy(p["estimatorVersion"]) {
		return serviceError("GENERATION_RESERVATION_INVALID")
	}
	sum := int64(0)
	for _, n := range []int64{r.InputUpperBoundTokens, r.RequestedOutputTokens, r.ReasoningUpperBoundTokens, margin} {
		if n < 0 || n > 9007199254740991 || sum > 9007199254740991-n {
			return serviceError("GENERATION_RESERVATION_INVALID")
		}
		sum += n
	}
	if r.ReservedTokens < sum || r.ReservedTokens > 9007199254740991 {
		return serviceError("GENERATION_RESERVATION_INVALID")
	}
	return nil
}
func (s *Service) Execute(ctx context.Context, request Request) (map[string]any, error) {
	if ctx.Err() != nil {
		return nil, serviceError("GENERATION_CANCELLED")
	}
	if !requestHashPattern.MatchString(request.RequestHash) {
		return nil, serviceError("GENERATION_REQUEST_HASH_INVALID")
	}
	if request.AgentToolNames != nil {
		return nil, serviceError("GENERATION_OPERATION_UNSUPPORTED")
	}
	keyBytes, _ := json.Marshal([]string{request.RunID, request.InvocationNonce})
	key := string(keyBytes)
	s.mu.Lock()
	if p := s.pending[key]; p != nil {
		if p.hash != request.RequestHash {
			s.mu.Unlock()
			return nil, serviceError("GENERATION_INVOCATION_CONFLICT")
		}
		s.mu.Unlock()
		select {
		case <-p.done:
			return cloneMap(p.receipt), p.err
		case <-ctx.Done():
			return nil, serviceError("GENERATION_CANCELLED")
		}
	}
	p := &invocation{hash: request.RequestHash, done: make(chan struct{})}
	s.pending[key] = p
	s.mu.Unlock()
	result, err := s.execute(ctx, request)
	s.mu.Lock()
	p.receipt, p.err = cloneMap(result), err
	delete(s.pending, key)
	close(p.done)
	s.mu.Unlock()
	return cloneMap(result), err
}
func (s *Service) execute(ctx context.Context, r Request) (map[string]any, error) {
	s.gate.Lock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		s.gate.Unlock()
		return nil, serviceError("GENERATION_PROJECT_CLOSED")
	}
	found, err := s.call(ctx, "findInvocation", r.RunID, r.InvocationNonce, r.RequestHash)
	if err != nil {
		s.gate.Unlock()
		return nil, err
	}
	if found != nil {
		s.gate.Unlock()
		return smap(found), nil
	}
	runValue, err := s.call(ctx, "get", r.RunID)
	if err != nil {
		s.gate.Unlock()
		return nil, err
	}
	rootID := stext(smap(runValue)["rootActionId"])
	s.mu.Lock()
	failed := s.failedRoots[rootID]
	s.mu.Unlock()
	if failed {
		s.gate.Unlock()
		return nil, serviceError("GENERATION_STORAGE_FAILED")
	}
	if err := validateRequest(r); err != nil {
		s.gate.Unlock()
		return nil, err
	}
	if s.deps.Dispatch == nil {
		s.gate.Unlock()
		return nil, serviceError("GENERATION_DISPATCH_REQUIRED")
	}
	reserved, err := s.call(ctx, "reserve", r.RunID, r.InvocationNonce, r.RequestHash, r.ReservedTokens, r.RequestedOutputTokens, r.UsagePolicy, r.Purpose, r.ReplayTask, r.BudgetDecision, r.ReconciliationInjected)
	if err != nil {
		if !expectedReservationError(err) {
			s.failRoot(rootID)
		}
		s.gate.Unlock()
		return nil, err
	}
	receipt := smap(reserved)
	if smap(receipt["attempt"])["status"] != "reserved" {
		s.gate.Unlock()
		return receipt, nil
	}
	providerCtx, cancel := context.WithCancel(ctx)
	artifact := smap(receipt["artifact"])
	revision, _ := sint(artifact["revision"])
	x := &execution{runID: r.RunID, rootID: rootID, attemptID: stext(smap(receipt["attempt"])["attemptId"]), artifactID: stext(artifact["artifactId"]), text: stext(artifact["text"]), durable: stext(artifact["text"]), revision: revision, receipt: receipt, events: map[string]VisibleEvent{}, cancel: cancel, ctx: providerCtx}
	s.mu.Lock()
	s.active[x.attemptID] = x
	s.mu.Unlock()
	_, err = s.cleanupCall("markDispatched", x.attemptID)
	if err != nil {
		x.mu.Lock()
		x.storageFailed = true
		x.terminal = true
		cancel()
		s.failRoot(rootID)
		x.mu.Unlock()
		s.mu.Lock()
		delete(s.active, x.attemptID)
		s.mu.Unlock()
		s.gate.Unlock()
		return s.failedReceipt(x), nil
	}
	budgetValue, err := s.cleanupCall("budget", rootID)
	if err != nil {
		x.mu.Lock()
		s.storageFailureLocked(x)
		x.mu.Unlock()
	}
	budget := smap(budgetValue)
	maximum, _ := sint(smap(budget["policy"])["maxActiveElapsedMs"])
	elapsed, _ := sint(budget["activeElapsedMs"])
	remaining := maximum - elapsed
	if remaining < 0 {
		remaining = 0
	}
	s.gate.Unlock()
	defer cancel()
	defer func() { s.mu.Lock(); delete(s.active, x.attemptID); s.mu.Unlock() }()
	type outcome struct {
		result DispatchResult
		err    error
	}
	finished := make(chan outcome, 1)
	// Cancellation never waits for an uncooperative provider; retained callbacks
	// share the terminal fence and cannot touch a closed DB or mutate artifacts.
	go func() {
		var out outcome
		func() {
			defer func() {
				if recover() != nil {
					out.err = serviceError("GENERATION_PROVIDER_FAILED")
				}
			}()
			if providerCtx.Err() != nil {
				out.err = serviceError("GENERATION_CANCELLED")
				return
			}
			out.result, out.err = s.deps.Dispatch(providerCtx, r.ProviderRequest, Callbacks{
				OnVisible:     func(e VisibleEvent) { s.visible(x, e) },
				OnReasoning:   func(text string) { s.reasoning(x, text) },
				OnDiagnostics: func(d map[string]any) { s.diagnostics(x, d) },
			})
		}()
		x.mu.Lock()
		x.finished = true
		x.mu.Unlock()
		finished <- out
	}()
	ticker := time.NewTicker(s.deps.FlushInterval)
	defer ticker.Stop()
	// JS safe-integer milliseconds can exceed Go time.Duration's range.
	if remaining > int64(math.MaxInt64)/int64(time.Millisecond) {
		remaining = int64(math.MaxInt64) / int64(time.Millisecond)
	}
	deadline := time.NewTimer(time.Duration(remaining) * time.Millisecond)
	defer deadline.Stop()
	var out outcome
	waiting := true
	for waiting {
		select {
		case out = <-finished:
			waiting = false
		case <-providerCtx.Done():
			out.err = serviceError("GENERATION_CANCELLED")
			waiting = false
		case <-ticker.C:
			x.mu.Lock()
			dirty := !x.terminal && !x.storageFailed && x.text != x.durable
			if dirty {
				s.flushLocked(x)
			}
			snap := s.snapshotLocked(x)
			x.mu.Unlock()
			if dirty {
				s.notify(snap)
			}
		case <-deadline.C:
			x.mu.Lock()
			if !x.terminal {
				x.timedOut = true
				if _, e := s.cleanupCall("block", rootID, "ROOT_BUDGET_EXHAUSTED"); e != nil {
					s.storageFailureLocked(x)
				}
				x.cancel()
			}
			x.mu.Unlock()
		}
	}
	s.gate.Lock()
	x.mu.Lock()
	if x.suspended {
		result := cloneMap(x.receipt)
		result["failureCode"] = "GENERATION_PROJECT_CLOSED"
		if x.storageFailed {
			result = s.failedReceipt(x)
		}
		x.mu.Unlock()
		s.gate.Unlock()
		return result, nil
	}
	// Fence before final flush/settlement, including callbacks after dispatch returns.
	x.terminal = true
	if providerCtx.Err() != nil && out.err == nil {
		out.err = serviceError("GENERATION_CANCELLED")
	}
	finalDirty := !x.storageFailed && x.text != x.durable
	s.flushLocked(x)
	failure := ""
	if x.storageFailed {
		failure = "GENERATION_STORAGE_FAILED"
	} else if x.timedOut {
		failure = "ROOT_BUDGET_EXHAUSTED"
	} else if x.streamFailed {
		failure = "GENERATION_STREAM_INVALID"
	} else if x.stopped || providerCtx.Err() != nil {
		failure = "GENERATION_CANCELLED"
	} else if out.err != nil {
		failure = providerFailure(out.err, x.diagnostics)
	}
	var usage any = SettleProviderUsage(out.result.Usage, r.UsagePolicy)
	var finish any = safeFinish(out.result.FinishReason)
	// Catch-path receipts use null usage, never a fabricated trusted receipt.
	if out.err != nil || x.streamFailed || x.stopped || x.timedOut {
		usage = nil
		finish = nil
	}
	// A resolved provider whose final snapshot failed still has usage evidence;
	// the original settles that evidence with error finish and a storage fence.
	if x.storageFailed && out.err == nil {
		finish = "error"
	}
	if x.diagnostics != nil && out.err != nil && x.diagnostics["endReason"] == nil {
		if providerCtx.Err() != nil {
			x.diagnostics["endReason"] = "cancelled"
		} else {
			x.diagnostics["endReason"] = "failed"
		}
	}
	var failureArg any
	if failure != "" {
		failureArg = failure
	}
	settled, settleErr := s.cleanupCall("settle", x.attemptID, usage, finish, failureArg, x.diagnostics)
	if settleErr != nil {
		s.storageFailureLocked(x)
	} else {
		x.receipt = smap(settled)
	}
	result := cloneMap(x.receipt)
	if x.storageFailed {
		result = s.failedReceipt(x)
	} else if failure != "" && result["failureCode"] == nil {
		result["failureCode"] = failure
	}
	snap := s.snapshotLocked(x)
	x.mu.Unlock()
	s.gate.Unlock()
	if finalDirty {
		s.notify(snap)
	}
	return result, nil
}
func safeFinish(reason string) any {
	switch reason {
	case "stop", "length", "content_filter", "error", "unknown":
		return reason
	default:
		return nil
	}
}
func providerFailure(err error, diagnostics map[string]any) string {
	if err != nil {
		switch err.Error() {
		case "GENERATION_CONTEXT_LENGTH_EXCEEDED":
			return "GENERATION_CONTEXT_LENGTH_EXCEEDED"
		case "NETWORK_ERROR", "ECONNRESET", "ETIMEDOUT", "FETCH_FAILED":
			return "NETWORK_ERROR"
		}
	}
	for _, key := range []string{"causeCode", "errorCode"} {
		if code := stext(diagnostics[key]); safeNetworkCode(code) && code != "ABORT_ERR" {
			return "NETWORK_ERROR"
		}
	}
	return "GENERATION_PROVIDER_FAILED"
}
func safeNetworkCode(code string) bool {
	switch code {
	case "UND_ERR_BODY_TIMEOUT", "UND_ERR_HEADERS_TIMEOUT", "UND_ERR_CONNECT_TIMEOUT", "UND_ERR_SOCKET", "ECONNRESET", "ECONNREFUSED", "ETIMEDOUT", "ENOTFOUND", "EAI_AGAIN", "ABORT_ERR", "ERR_STREAM_PREMATURE_CLOSE":
		return true
	default:
		return false
	}
}
func safeDiagnostics(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"startedAt", "elapsedMs", "firstResponseMs", "lastResponseMs", "lastOutputMs", "visibleEvents", "reasoningEvents"} {
		v, exists := input[key]
		if !exists {
			continue
		}
		if v == nil && (key == "firstResponseMs" || key == "lastResponseMs" || key == "lastOutputMs") {
			out[key] = nil
			continue
		}
		if n, ok := sint(v); ok && n >= 0 {
			out[key] = n
		}
	}
	if n, ok := sint(input["httpStatus"]); ok && n >= 100 && n <= 599 {
		out["httpStatus"] = n
	}
	for _, key := range []string{"errorCode", "causeCode"} {
		if code := stext(input[key]); safeNetworkCode(code) {
			out[key] = code
		}
	}
	switch name := stext(input["errorName"]); name {
	case "Error", "TypeError", "AbortError", "TimeoutError", "SocketError", "BodyTimeoutError", "HeadersTimeoutError", "ConnectTimeoutError":
		out["errorName"] = name
	}
	switch phase := stext(input["phase"]); phase {
	case "request", "response", "stream", "complete":
		out["phase"] = phase
	}
	switch reason := stext(input["endReason"]); reason {
	case "completed", "cancelled", "failed":
		out["endReason"] = reason
	}
	return out
}
func (s *Service) failRoot(rootID string) {
	s.mu.Lock()
	s.failedRoots[rootID] = true
	s.mu.Unlock()
	_, _ = s.cleanupCall("block", rootID, "GENERATION_STORAGE_FAILED")
}

// Caller holds x.mu. No user callback is invoked while a service lock is held.
func (s *Service) storageFailureLocked(x *execution) {
	x.storageFailed = true
	x.cancel()
	s.mu.Lock()
	s.failedRoots[x.rootID] = true
	if len(x.text) > len(x.durable) {
		s.tails[x.attemptID] = UnsavedTail{x.attemptID, x.artifactID, x.revision, x.text[len(x.durable):], "GENERATION_STORAGE_FAILED"}
	}
	s.mu.Unlock()
	_, _ = s.cleanupCall("block", x.rootID, "GENERATION_STORAGE_FAILED")
}
func (s *Service) flushLocked(x *execution) {
	if x.storageFailed || x.text == x.durable {
		return
	}
	out, err := s.cleanupCall("snapshot", x.attemptID, x.revision, x.text)
	if err != nil {
		s.storageFailureLocked(x)
		return
	}
	artifact := smap(out)
	revision, ok := sint(artifact["revision"])
	if !ok || revision != x.revision+1 || artifact["text"] != x.text {
		s.storageFailureLocked(x)
		return
	}
	x.revision, x.durable = revision, x.text
}
func (s *Service) snapshotLocked(x *execution) Snapshot {
	return Snapshot{x.attemptID, x.text, x.durable, x.revision, x.storageFailed, cloneMap(x.diagnostics)}
}
func (s *Service) notify(snapshot Snapshot) {
	if s.deps.OnSnapshot != nil {
		func() { defer func() { _ = recover() }(); s.deps.OnSnapshot(snapshot) }()
	}
}
func (s *Service) visible(x *execution, event VisibleEvent) {
	x.mu.Lock()
	if x.terminal || x.finished || x.storageFailed || x.stopped || x.suspended || x.streamFailed || x.ctx.Err() != nil {
		x.mu.Unlock()
		return
	}
	if event.EventID != "" {
		if prior, exists := x.events[event.EventID]; exists {
			if prior != event {
				x.streamFailed = true
				x.cancel()
			}
			x.mu.Unlock()
			return
		}
		x.events[event.EventID] = event
	}
	next := event.Text
	if event.Kind == "delta" {
		next = x.text + event.Text
	}
	if (event.Kind != "delta" && event.Kind != "cumulative") || !utf8.ValidString(next) || !strings.HasPrefix(next, x.text) || visibleThinkPattern.MatchString(event.Text) || visibleThinkPattern.MatchString(next) {
		x.streamFailed = true
		x.cancel()
		x.mu.Unlock()
		return
	}
	x.text = next
	snap := s.snapshotLocked(x)
	x.mu.Unlock()
	s.notify(snap)
	// Match the original pre-flush projection, without holding a lock across
	// notifications (which may synchronously pause or close the project).
	x.mu.Lock()
	dirty := !x.terminal && !x.storageFailed && !x.suspended && len(x.text)-len(x.durable) >= 4096
	if dirty {
		s.flushLocked(x)
	}
	snap = s.snapshotLocked(x)
	x.mu.Unlock()
	if dirty {
		s.notify(snap)
	}
}
func (s *Service) reasoning(x *execution, text string) {
	x.mu.Lock()
	allowed := !x.terminal && !x.finished && !x.storageFailed && !x.stopped && !x.suspended && !x.streamFailed && x.ctx.Err() == nil && text != ""
	event := ReasoningEvent{x.runID, x.rootID, x.attemptID, text}
	x.mu.Unlock()
	if allowed && s.deps.OnReasoning != nil {
		func() { defer func() { _ = recover() }(); s.deps.OnReasoning(event) }()
	}
}
func (s *Service) diagnostics(x *execution, input map[string]any) {
	x.mu.Lock()
	if x.terminal || x.finished || x.storageFailed || x.stopped || x.suspended || x.streamFailed || x.ctx.Err() != nil {
		x.mu.Unlock()
		return
	}
	x.diagnostics = safeDiagnostics(input)
	snap := s.snapshotLocked(x)
	x.mu.Unlock()
	s.notify(snap)
}
func (s *Service) failedReceipt(x *execution) map[string]any {
	out := cloneMap(x.receipt)
	if out == nil {
		out = map[string]any{}
	}
	out["failureCode"] = "GENERATION_STORAGE_FAILED"
	out["unsavedTail"] = x.text[len(x.durable):]
	return out
}
func (s *Service) states() []*execution {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.active))
	for id := range s.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*execution, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.active[id])
	}
	return out
}

// Flush is also the deterministic testing seam for the default one-second timer.
func (s *Service) Flush(ctx context.Context) error {
	_ = ctx
	var failure error
	for _, x := range s.states() {
		x.mu.Lock()
		dirty := !x.terminal && !x.storageFailed && x.text != x.durable
		if dirty {
			s.flushLocked(x)
		}
		if x.storageFailed {
			failure = serviceError("GENERATION_STORAGE_FAILED")
		}
		snap := s.snapshotLocked(x)
		x.mu.Unlock()
		if dirty {
			s.notify(snap)
		}
	}
	return failure
}
func (s *Service) ReadUnsavedTails() []UnsavedTail {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.tails))
	for id := range s.tails {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]UnsavedTail, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.tails[id])
	}
	return out
}

// DiscardCandidate is the explicit author/caller acknowledgment of a candidate,
// including its retained in-memory tail. A failed durable discard never loses a
// tail. It remains available after a refused close so the caller can resolve it.
func (s *Service) DiscardCandidate(ctx context.Context, artifactID string) (map[string]any, error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if _, err := s.call(ctx, "discardCandidate", artifactID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	for attemptID, tail := range s.tails {
		if tail.ArtifactID == artifactID {
			delete(s.tails, attemptID)
		}
	}
	s.mu.Unlock()
	return map[string]any{}, nil
}
func (s *Service) stop(ctx context.Context, rootID string, cancelRoot bool) (map[string]any, error) {
	if ctx.Err() != nil {
		return nil, serviceError("GENERATION_CANCELLED")
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	if ctx.Err() != nil {
		return nil, serviceError("GENERATION_CANCELLED")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, serviceError("GENERATION_PROJECT_CLOSED")
	}
	// Once control admission commits, finish durable cancellation independently
	// of the caller's display/request lifetime; cancellation is not DB failure.
	_, err := s.cleanupCall("pause", rootID, cancelRoot)
	for _, x := range s.states() {
		if x.rootID != rootID {
			continue
		}
		x.mu.Lock()
		if !x.terminal {
			x.stopped = true
			x.cancel()
			if err != nil {
				s.storageFailureLocked(x)
			}
		}
		x.mu.Unlock()
	}
	if err != nil {
		s.failRoot(rootID)
		return nil, err
	}
	out, err := s.cleanupCall("budget", rootID)
	return smap(out), err
}
func (s *Service) Pause(ctx context.Context, rootID string) (map[string]any, error) {
	return s.stop(ctx, rootID, false)
}
func (s *Service) Cancel(ctx context.Context, rootID string) (map[string]any, error) {
	return s.stop(ctx, rootID, true)
}

// SuspendForProjectClose must finish while the captured database is open. It
// fences admission, flushes full bodies, recovers accounting, captures receipts,
// then aborts providers. Returned unsaved tails prevent the caller closing DB.
func (s *Service) SuspendForProjectClose(ctx context.Context) ([]map[string]any, error) {
	if ctx.Err() != nil {
		return nil, serviceError("GENERATION_CANCELLED")
	}
	var snapshots []Snapshot
	// Registered before the lock-release defers: observers run last, outside
	// every lock, but see the exact dirty-flush projections captured in order.
	defer func() {
		for _, snapshot := range snapshots {
			s.notify(snapshot)
		}
	}()
	s.gate.Lock()
	defer s.gate.Unlock()
	if ctx.Err() != nil {
		return nil, serviceError("GENERATION_CANCELLED")
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	states := s.states()
	for _, x := range states {
		x.mu.Lock()
	}
	defer func() {
		for i := len(states) - 1; i >= 0; i-- {
			states[i].mu.Unlock()
		}
	}()
	for _, x := range states {
		if !x.terminal && !x.storageFailed && x.text != x.durable {
			s.flushLocked(x)
			snapshots = append(snapshots, s.snapshotLocked(x))
		}
	}
	_, failure := s.cleanupCall("recoverInterrupted")
	out := make([]map[string]any, 0, len(states))
	for _, x := range states {
		if !x.terminal {
			receipt, err := s.cleanupCall("receipt", x.attemptID)
			if err != nil {
				s.storageFailureLocked(x)
				failure = err
			} else {
				x.receipt = smap(receipt)
			}
			if failure != nil {
				s.storageFailureLocked(x)
			}
			x.suspended, x.terminal = true, true
			x.cancel()
		}
		out = append(out, cloneMap(x.receipt))
	}
	if len(s.ReadUnsavedTails()) != 0 {
		return out, serviceError("GENERATION_UNSAVED_TAIL_PRESENT")
	}
	return out, failure
}

// Resume is default-deny and preserves the actual frozen fingerprint, context
// and manifest comparison. next stays raw so ledger order-sensitive JSON is not
// silently rewritten by this adapter. Authorization never comes from next.
func (s *Service) Resume(ctx context.Context, runID string, next json.RawMessage) (map[string]any, error) {
	// Own the raw bytes across the external validator callback.
	next = append(json.RawMessage(nil), next...)
	s.gate.Lock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		s.gate.Unlock()
		return nil, serviceError("GENERATION_PROJECT_CLOSED")
	}
	oldValue, err := s.call(ctx, "get", runID)
	s.gate.Unlock()
	if err != nil {
		return nil, err
	}
	old := smap(smap(oldValue)["binding"])
	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(next)))
	if decoder.Decode(&decoded) != nil || decoded == nil || s.deps.ValidateRecovery == nil {
		return nil, serviceError("GENERATION_RECOVERY_UNAUTHORIZED")
	}
	canonical := func(v any) string {
		b, _ := json.Marshal(v)
		var plain any
		if json.Unmarshal(b, &plain) != nil {
			return ""
		}
		b, _ = json.Marshal(plain)
		return string(b)
	}
	for _, key := range []string{"fingerprint", "contextSnapshotId", "sourceManifest"} {
		if canonical(old[key]) != canonical(decoded[key]) {
			return nil, serviceError("GENERATION_RECOVERY_UNAUTHORIZED")
		}
	}
	// Never hold a service/state/ledger mutex across application code.
	allowed, authErr := s.deps.ValidateRecovery(ctx, cloneMap(old), cloneMap(decoded))
	if authErr != nil || !allowed {
		return nil, serviceError("GENERATION_RECOVERY_UNAUTHORIZED")
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	s.mu.Lock()
	closed = s.closed
	failed := s.failedRoots[stext(smap(oldValue)["rootActionId"])]
	s.mu.Unlock()
	if closed {
		return nil, serviceError("GENERATION_PROJECT_CLOSED")
	}
	if failed {
		return nil, serviceError("GENERATION_STORAGE_FAILED")
	}
	current, err := s.call(ctx, "get", runID)
	if err != nil {
		return nil, err
	}
	if canonical(current) != canonical(oldValue) {
		return nil, serviceError("GENERATION_RECOVERY_UNAUTHORIZED")
	}
	if _, err = s.call(ctx, "resume", runID, next); err != nil {
		return nil, err
	}
	out, err := s.call(ctx, "get", runID)
	return smap(out), err
}
