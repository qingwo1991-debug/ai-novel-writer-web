// Package agentgen is the round protocol of the agent generation family: the
// identity a round freezes, the order rounds may be executed in, and the
// metadata one workflow registration carries.
//
// It is NOT a runner and must never become one. It holds no database, no model
// lease, no egress client and no handler. Every function below is a pure
// decision over a projection the caller has already read, and every refusal is
// a symbolic code - the same codes the reference throws
// (electron/services/agent-generation.ts), so the renderer matches one string
// on both builds. Nothing here formats author text, a path or a credential.
//
// A round IS a generation run, and the only ledger operation this protocol
// freezes is Operation: one frozen manifest, one attempt, one artifact per
// round, exactly like every other family. The ledger already admits the
// agentSession manifest key for that one operation
// (internal/store/generation_ledger.go:428-429, whose comment names
// "agent-round"), so the run identity is not what this lane is missing. What is
// missing is the receipt state the four tool operations write and the dispatch
// protocol the round request carries; production_agent_generation_linux.go
// states both, name by name, with the line that has to move.
//
// The rules are transliterated from the reference, not invented:
//
//	session identity       electron/services/agent-generation.ts:64-69, :158-159
//	round classification   electron/services/agent-generation.ts:83-97
//	next round             electron/services/agent-generation.ts:138-148
//	round protocol text    electron/services/agent-generation.ts:129-130, :200-203
//	round admission        electron/services/agent-generation.ts:180-204
//	claim                  electron/services/agent-generation.ts:230-258
//	finish                 electron/services/agent-generation.ts:259-275
//	register               electron/services/agent-generation.ts:276-293
//	admit registration     electron/services/agent-generation.ts:305-341
package agentgen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
)

// Operation is the one ledger operation an agent round freezes. The eight
// agent-generation:* names all resolve to it and to nothing else: the desktop
// owner freezes operation 'agent-round' for every round run it opens
// (electron/services/agent-generation.ts:175-178), and every identity check in
// the reference refuses a run whose manifest operation is anything else
// (:54-62).
const Operation = "agent-round"

// MaxRounds bounds one session. roundIndex must satisfy 0 <= index < MaxRounds
// (electron/services/agent-generation.ts:65), and a completed round that is not
// the last one is what promotes the session to the next index (:145-147).
const MaxRounds = 8

// MaxObservationBytes is the ceiling on one tool observation, measured in UTF-8
// bytes exactly like the reference measures it
// (electron/services/agent-generation.ts:260, 256 * 1024).
const MaxObservationBytes = 256 << 10

// Symbolic refusal codes, verbatim from the reference so a renderer that
// matches on the desktop strings matches here too.
const (
	// CodeIdentityMismatch is a handle that does not name the run, project,
	// epoch or operation it claims (agent-generation.ts:56-61).
	CodeIdentityMismatch = "GENERATION_AGENT_IDENTITY_MISMATCH"
	// CodeNavigationInvalid is a frozen session that is not 64 lowercase hex
	// plus an integer round index in range (agent-generation.ts:64-69).
	CodeNavigationInvalid = "GENERATION_AGENT_NAVIGATION_INVALID"
	// CodeContextInvalid is a run whose frozen agent context does not re-hash
	// to its own digest (agent-generation.ts:62-63).
	CodeContextInvalid = "GENERATION_AGENT_CONTEXT_INVALID"
	// CodeBeginInvalid is a malformed begin request (agent-generation.ts:142-145).
	CodeBeginInvalid = "GENERATION_AGENT_BEGIN_INVALID"
	// CodeRoundInvalid is an executeRound index outside 0..MaxRounds-1
	// (agent-generation.ts:190-191).
	CodeRoundInvalid = "GENERATION_AGENT_ROUND_INVALID"
	// CodeRoundOrder is a round requested out of order
	// (agent-generation.ts:197).
	CodeRoundOrder = "GENERATION_AGENT_ROUND_ORDER"
	// CodeRoundGap is a recovery whose round indexes are not exactly
	// 0..len-1 (agent-generation.ts:141).
	CodeRoundGap = "GENERATION_AGENT_ROUND_GAP"
	// CodeRootMismatch is a session whose runs do not share one root
	// (agent-generation.ts:139).
	CodeRootMismatch = "GENERATION_AGENT_ROOT_MISMATCH"
	// CodeActionBlocked is a round on a blocked or cancelled root
	// (agent-generation.ts:198).
	CodeActionBlocked = "GENERATION_AGENT_ACTION_BLOCKED"
	// CodeSourceChanged is a round whose frozen sources drifted
	// (agent-generation.ts:199, :206).
	CodeSourceChanged = "GENERATION_SOURCE_CHANGED"
	// CodeToolIdentityMismatch is a tool reference that is not an action of the
	// named attempt (agent-generation.ts:209-213).
	CodeToolIdentityMismatch = "GENERATION_AGENT_TOOL_IDENTITY_MISMATCH"
	// CodeToolStale is a claim or registration under a lease that is no longer
	// the current one (agent-generation.ts:240, :326-327).
	CodeToolStale = "GENERATION_AGENT_TOOL_STALE"
	// CodeEpochStale is a finish under a stale root epoch
	// (agent-generation.ts:261).
	CodeEpochStale = "GENERATION_EPOCH_STALE"
	// CodeToolResultInvalid is a finish whose status or observation is
	// unacceptable (agent-generation.ts:260).
	CodeToolResultInvalid = "GENERATION_AGENT_TOOL_RESULT_INVALID"
	// CodeToolResultConflict is a finish that contradicts the stored action
	// state (agent-generation.ts:271).
	CodeToolResultConflict = "GENERATION_AGENT_TOOL_RESULT_CONFLICT"
	// CodeDomainEffectRequired is a completed builtin domain tool with no
	// domain write behind it (agent-generation.ts:263).
	CodeDomainEffectRequired = "GENERATION_AGENT_DOMAIN_EFFECT_REQUIRED"
	// CodeWorkflowNotAuthorized is a registration attempted for an action that
	// is not a running start_workflow call under a live lease
	// (agent-generation.ts:277).
	CodeWorkflowNotAuthorized = "GENERATION_AGENT_WORKFLOW_NOT_AUTHORIZED"
	// CodeWorkflowInvalid is a start_workflow call naming an unsupported
	// workflow or unsupported arguments (agent-generation.ts:279-281).
	CodeWorkflowInvalid = "GENERATION_AGENT_WORKFLOW_INVALID"
	// CodeRegistrationInvalid is a registration id that is not
	// agent-workflow:<64 hex> (agent-generation.ts:295-296).
	CodeRegistrationInvalid = "GENERATION_AGENT_REGISTRATION_INVALID"
	// CodeRegistrationMissing is a registration id that does not resolve to
	// exactly one action of this project (agent-generation.ts:303).
	CodeRegistrationMissing = "GENERATION_AGENT_REGISTRATION_MISSING"
	// CodeRegistrationRequired is a child begin under an agent-round parent
	// that offered no registration (agent-generation.ts:296-300).
	CodeRegistrationRequired = "GENERATION_AGENT_REGISTRATION_REQUIRED"
	// CodeChildNotAuthorized is a registration that does not authorize this
	// child (agent-generation.ts:307-311, :324).
	CodeChildNotAuthorized = "GENERATION_AGENT_CHILD_NOT_AUTHORIZED"
	// CodeChildAmbiguous is more than one child run for one registration slot
	// (agent-generation.ts:314).
	CodeChildAmbiguous = "GENERATION_AGENT_CHILD_AMBIGUOUS"
)

// Round statuses the recovery projects (agent-generation.ts:84-98).
const (
	// RoundNotStarted is a run that froze its round but has no attempt row yet.
	RoundNotStarted = "not-started"
	// RoundIncomplete is a round whose attempt did not produce a usable
	// response.
	RoundIncomplete = "incomplete"
	// RoundUnknown is a round whose attempt outcome cannot be known - the
	// invocation may have reached the provider, so it is never redispatched
	// (agent-generation.ts:92-96, :194-196).
	RoundUnknown = "unknown"
	// RoundCompleted is a round whose response parsed into actions.
	RoundCompleted = "completed"
)

// Attempt and action statuses. StatusUnknown is both the attempt status that
// makes a round unknown and the finish status a caller may report
// (agent-generation.ts:92, :259).
const (
	StatusUnknown = "unknown"
	// AttemptReserved and AttemptDispatchMarked are the ledger's own attempt
	// statuses; both mean "the request may be in flight"
	// (internal/store/generation_ledger.go:940-1111).
	AttemptReserved       = "reserved"
	AttemptDispatchMarked = "dispatch-marked"
	// AttemptSettled is an attempt whose receipt was committed.
	AttemptSettled = "settled"
)

// Tool action statuses (electron/services/agent-generation.ts:230-275).
const (
	ActionPending   = "pending"
	ActionRunning   = "running"
	ActionCompleted = "completed"
	ActionFailed    = "failed"
	ActionDeclined  = "declined"
)

// Decision kinds. A replay is the whole idempotency rule: the caller answers
// with the stored recovery and writes nothing.
const (
	// KindExecute means the round may dispatch.
	KindExecute = "execute"
	// KindReplay means the round already ran and must not be redispatched.
	KindReplay = "replay"
	// KindApply means the tool state transition may be written.
	KindApply = "apply"
)

// Claim kinds (agent-generation.ts:230-258).
const (
	// ClaimExecute means the confirmed tool may run.
	ClaimExecute = "execute"
	// ClaimDeclined means the author declined it.
	ClaimDeclined = "declined"
	// ClaimNoop means the action is no longer pending, so nothing is written
	// and the lease is not re-read (agent-generation.ts:250).
	ClaimNoop = "noop"
	// ClaimUnknownTool means the frozen context never registered that tool; the
	// caller records a failed action, not a refusal
	// (agent-generation.ts:242-243). The observation text is the caller's.
	ClaimUnknownTool = "unknown-tool"
)

// Registration admission kinds (agent-generation.ts:305-341).
const (
	// AdmitOrdinary is a begin that claims no registration and is not a child
	// of an agent round.
	AdmitOrdinary = "ordinary"
	// AdmitExisting is a child begin that replays a child run it already has.
	AdmitExisting = "existing"
	// AdmitNew is a child begin the registration authorizes for the first time.
	AdmitNew = "new"
)

// Workflow names one registration may carry (agent-generation.ts:279).
const (
	WorkflowDraft        = "generate_draft"
	WorkflowBlueprint    = "generate_blueprint"
	WorkflowArchitecture = "generate_architecture"
)

// Registration states (agent-generation.ts:284, :371-374).
const (
	RegistrationRegistered = "registered"
	RegistrationStarted    = "started"
)

var (
	sessionKeyPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	registrationPattern  = regexp.MustCompile(`^agent-workflow:[a-f0-9]{64}$`)
	maxSafeInteger       = float64(1 << 53)
	maxNonceLength       = 256
	registrationIDPrefix = "agent-workflow:"
)

func failure(code string) error { return errors.New(code) }

// integer reads an exact integer out of a decoded JSON value. The reference
// spells every one of these bounds Number.isSafeInteger, so a fractional value,
// an infinity or a value outside the safe range is not an integer here either.
func integer(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) {
			return 0, false
		}
		if typed > maxSafeInteger || typed < -maxSafeInteger {
			return 0, false
		}
		return int64(typed), true
	}
	return 0, false
}

// hashOf is the reference's textHash: lowercase hex SHA-256 of the UTF-8 text.
func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Session is the identity a round run freezes in sourceManifest.agentSession
// (electron/services/agent-generation.ts:12). The key is the session, the index
// is the position of the round inside it; together they are why a replayed
// round can be recognised instead of run twice.
type Session struct {
	Key        string
	RoundIndex int64
}

// ParseSession reads a frozen agentSession and refuses anything the reference
// refuses: the key must be 64 lowercase hex and the index an integer in
// 0..MaxRounds-1 (electron/services/agent-generation.ts:64-69). A nil record is
// a run of another family, so it refuses the same way.
func ParseSession(value map[string]any) (Session, error) {
	if value == nil {
		return Session{}, failure(CodeNavigationInvalid)
	}
	key, ok := value["key"].(string)
	if !ok || !sessionKeyPattern.MatchString(key) {
		return Session{}, failure(CodeNavigationInvalid)
	}
	index, ok := integer(value["roundIndex"])
	if !ok || index < 0 || index >= MaxRounds {
		return Session{}, failure(CodeNavigationInvalid)
	}
	return Session{Key: key, RoundIndex: index}, nil
}

// SessionKey derives the key a begin freezes: the SHA-256 of the canonical JSON
// pair [projectId, uiActionNonce] (electron/services/agent-generation.ts:158-159,
// where the pair is hashed with textHash over JSON.stringify). The nonce alone
// is not the key, so two projects that spend the same nonce are two sessions
// and neither can replay the other's run.
func SessionKey(projectID, nonce string) (string, error) {
	if projectID == "" || nonce == "" || len(nonce) > maxNonceLength {
		return "", failure(CodeBeginInvalid)
	}
	canonical, err := json.Marshal([]string{projectID, nonce})
	if err != nil {
		return "", failure(CodeBeginInvalid)
	}
	return hashOf(string(canonical)), nil
}

// Handle is the browser's run handle (agent-generation.ts:50-52).
type Handle struct {
	ProjectID    string
	Epoch        string
	RootActionID string
	RunID        string
}

// Frozen is what a stored run says about itself; the caller reads it out of the
// ledger binding and hands it here unchanged.
type Frozen struct {
	ProjectID    string
	Epoch        string
	RootActionID string
	RunID        string
	Operation    string
	Session      Session
}

// MatchIdentity refuses a handle that does not name this run, project, epoch
// and operation, and returns the frozen session when it does
// (electron/services/agent-generation.ts:54-62). The caller builds Handle from
// its authenticated scope (ProjectID, Epoch) plus the two ids the browser sent
// (RootActionID, RunID), so the comparison below is both of the reference's
// checks at once: the handle must equal the run, and the run must belong to this
// project and lease. The run id alone is never identity - a run of another
// project, another lease or another operation refuses, which is what stops a
// handle from borrowing a session it does not own.
func MatchIdentity(handle Handle, run Frozen) (Session, error) {
	if handle.ProjectID != run.ProjectID || handle.Epoch != run.Epoch ||
		handle.RootActionID != run.RootActionID || handle.RunID != run.RunID ||
		run.Operation != Operation {
		return Session{}, failure(CodeIdentityMismatch)
	}
	return run.Session, nil
}

// ToolCall is one parsed agent action (src/shared/agent-response-protocol.ts).
type ToolCall struct {
	Name      string
	Arguments map[string]any
}

// Attempt is the receipt projection one round is classified from: the attempt
// status, the settled finish reason, whether result.usage.agentResponse exists,
// the artifact text the attempt wrote, and the tool calls that response
// carried.
type Attempt struct {
	Status          string
	FinishReason    string
	HasResponse     bool
	ResponseVersion int64
	ResponseText    string
	ArtifactText    string
	ToolCalls       []ToolCall
}

// Action is one tool call as the round reports it, plus the state the attempt
// receipt keeps for it (agent-generation.ts:98-130, :215-227). Observation is
// author-facing text the caller already has; this package never builds it.
type Action struct {
	Name            string
	Arguments       map[string]any
	Status          string
	Observation     string
	HasDomainEffect bool
	Registration    *Registration
}

// Round is one round of one session. VisibleText is the part of a completed
// response the author may read; the tool protocol stays out of it.
type Round struct {
	Index       int64
	Status      string
	VisibleText string
	Actions     []Action
}

// ClassifyRound projects one attempt into the round the recovery reports
// (electron/services/agent-generation.ts:83-98). A run with no attempt is
// not-started; an attempt whose response is missing, unstamped, not a stop, not
// settled-or-unknown, or whose artifact text is not the response's visible text
// is incomplete (or unknown when the invocation may be in flight); only a
// complete response becomes the actions of a completed round.
func ClassifyRound(index int64, attempt *Attempt) Round {
	if attempt == nil {
		return Round{Index: index, Status: RoundNotStarted}
	}
	if !complete(attempt) {
		status := RoundIncomplete
		switch attempt.Status {
		case StatusUnknown, AttemptDispatchMarked, AttemptReserved:
			status = RoundUnknown
		}
		return Round{Index: index, Status: status}
	}
	actions := make([]Action, 0, len(attempt.ToolCalls))
	for _, call := range attempt.ToolCalls {
		actions = append(actions, Action{Name: call.Name, Arguments: call.Arguments, Status: ActionPending})
	}
	return Round{Index: index, Status: RoundCompleted, VisibleText: attempt.ResponseText, Actions: actions}
}

// complete is the reference's `complete` predicate
// (electron/services/agent-generation.ts:93-95).
func complete(attempt *Attempt) bool {
	if !attempt.HasResponse || attempt.ResponseVersion != 1 || attempt.FinishReason != "stop" {
		return false
	}
	if attempt.Status != AttemptSettled && attempt.Status != StatusUnknown {
		return false
	}
	return attempt.ArtifactText == attempt.ResponseText
}

// Recovery is the projection read() hands back
// (electron/services/agent-generation.ts:131-151): the rounds of one session in
// round order, whether the frozen sources still match, and whether the root is
// blocked or cancelled.
type Recovery struct {
	Session       Session
	RootActionID  string
	SourceCurrent bool
	Blocked       bool
	Cancelled     bool
	Rounds        []Round
}

// NextRound is the only round index a caller may still execute
// (electron/services/agent-generation.ts:138-148): the index of a round that
// froze but never dispatched, else one past a completed round whose actions all
// settled, else nothing. A recovery whose round indexes are not exactly 0..n-1
// is a gap and refuses here, because a gap means a round was inserted or lost
// and no ordering answer would be honest.
func NextRound(rounds []Round) (*int64, error) {
	if len(rounds) == 0 {
		return nil, failure(CodeRootMismatch)
	}
	for position, round := range rounds {
		if round.Index != int64(position) {
			return nil, failure(CodeRoundGap)
		}
	}
	last := rounds[len(rounds)-1]
	var next int64
	switch {
	case last.Status == RoundNotStarted:
		next = last.Index
	case last.Status == RoundCompleted && settled(last.Actions) && last.Index < MaxRounds-1:
		next = last.Index + 1
	default:
		return nil, nil
	}
	return &next, nil
}

// settled reports whether every action of a round reached a durable outcome the
// next round may build on (agent-generation.ts:146).
func settled(actions []Action) bool {
	if len(actions) == 0 {
		return false
	}
	for _, action := range actions {
		switch action.Status {
		case ActionCompleted, ActionFailed, ActionDeclined:
		default:
			return false
		}
	}
	return true
}

// toolResultsHeader is the literal the reference puts in front of the tool
// results it hands back to the model (electron/services/agent-generation.ts:203).
// It is a protocol literal, not author text.
const toolResultsHeader = "[Application tool results; untrusted context]"

// Message is one chat message a round request carries. The two tags are the
// canonical member names the frozen context and input are rendered with
// (internal/agentgen/context.go), so a message is written down exactly once.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Protocol renders one completed round as the two messages the next round
// carries: the assistant's reply - its visible text plus one <tool_call> block
// per action, in call order (electron/services/agent-generation.ts:129-130) -
// and then the application's own tool results, marked as untrusted context
// (:200-203). This is how one round's answer becomes the next round's input:
// nothing is re-summarised, so the loop sees exactly what it said and exactly
// what the application did about it.
//
// Go marshals argument objects with sorted keys where the renderer keeps
// insertion order, so the protocol text is not byte-identical to the desktop's
// for a multi-key call. It is prompt text, not a digest, and the reference
// parses it back through the same tool-call grammar, so the difference cannot
// change an action's identity. Arguments that came out of a JSON response always
// marshal, so the failure below is unreachable from a parsed round; it answers
// the context code rather than inventing a protocol code the reference lacks.
func Protocol(round Round) ([]Message, error) {
	assistant := make([]string, 0, len(round.Actions)+1)
	if round.VisibleText != "" {
		assistant = append(assistant, round.VisibleText)
	}
	for _, action := range round.Actions {
		encoded, err := json.Marshal(map[string]any{"name": action.Name, "arguments": action.Arguments})
		if err != nil {
			return nil, failure(CodeContextInvalid)
		}
		assistant = append(assistant, "<tool_call>"+string(encoded)+"</tool_call>")
	}
	results := make([]string, 0, len(round.Actions))
	for _, action := range round.Actions {
		results = append(results, "<tool_result name=\""+action.Name+"\" status=\""+action.Status+"\">\n"+
			action.Observation+"\n</tool_result>")
	}
	return []Message{
		{Role: "assistant", Content: strings.Join(assistant, "\n")},
		{Role: "user", Content: toolResultsHeader + "\n" + strings.Join(results, "\n\n")},
	}, nil
}

// Decision is one admission answer. Kind execute or apply means the caller may
// proceed; Kind replay means it must answer with what it already has and write
// nothing. Index is the round the decision is about.
type Decision struct {
	Kind  string
	Index int64
}

// DecideRound decides whether one round may dispatch, in the reference's exact
// check order (electron/services/agent-generation.ts:180-204):
//
//  1. the index must be in 0..MaxRounds-1, else ROUND_INVALID;
//  2. a round that exists and is no longer not-started is a REPLAY - it is
//     answered from the stored recovery and never redispatched, even after a
//     restart (:194-196);
//  3. the index must be the next round, else ROUND_ORDER;
//  4. a blocked root or a cancelled run refuses ACTION_BLOCKED;
//  5. drifting sources refuse SOURCE_CHANGED.
//
// Step 2 precedes step 3 on purpose: replaying the latest round is a replay,
// not an ordering error, so a retried request is idempotent instead of a
// refusal. An implementation that checked order first would answer
// ROUND_ORDER for the same request and fail this rule.
func DecideRound(recovery Recovery, index int64) (Decision, error) {
	if index < 0 || index >= MaxRounds {
		return Decision{}, failure(CodeRoundInvalid)
	}
	for _, round := range recovery.Rounds {
		if round.Index != index {
			continue
		}
		if round.Status != RoundNotStarted {
			return Decision{Kind: KindReplay, Index: index}, nil
		}
		break
	}
	next, err := NextRound(recovery.Rounds)
	if err != nil {
		return Decision{}, err
	}
	if next == nil || *next != index {
		return Decision{}, failure(CodeRoundOrder)
	}
	if recovery.Blocked || recovery.Cancelled {
		return Decision{}, failure(CodeActionBlocked)
	}
	if !recovery.SourceCurrent {
		return Decision{}, failure(CodeSourceChanged)
	}
	return Decision{Kind: KindExecute, Index: index}, nil
}

// ClaimRequest carries the gates claimTool reads. Confirmed is the author's
// answer, which the IPC envelope already proved to be a boolean
// (agent-generation.ts:231); EpochCurrent, SourcesCurrent and RootActive are
// what the caller observed on the live lease and root; ToolKnown says the
// frozen context input registered that tool name.
type ClaimRequest struct {
	Confirmed      bool
	EpochCurrent   bool
	SourcesCurrent bool
	RootActive     bool
	ToolKnown      bool
}

// DecideClaim answers claimTool (electron/services/agent-generation.ts:230-258)
// in the reference's order: an action that is not pending is a noop and the
// lease is not re-read at all (:250); a stale epoch, drifting sources or an
// inactive root refuse TOOL_STALE (:240); a tool the frozen input never
// registered is reported as a failed action, not a refusal (:242-243); and only
// then does the author's answer decide between execute and declined. The
// observation text for an unknown tool belongs to the caller, which is why this
// returns a kind rather than prose.
func DecideClaim(action Action, request ClaimRequest) (string, error) {
	if action.Status != ActionPending {
		return ClaimNoop, nil
	}
	if !request.EpochCurrent || !request.SourcesCurrent || !request.RootActive {
		return "", failure(CodeToolStale)
	}
	if !request.ToolKnown {
		return ClaimUnknownTool, nil
	}
	if request.Confirmed {
		return ClaimExecute, nil
	}
	return ClaimDeclined, nil
}

// FinishRequest is the finishTool argument (agent-generation.ts:259).
type FinishRequest struct {
	Status       string
	Observation  string
	EpochCurrent bool
}

// DecideFinish answers finishTool (electron/services/agent-generation.ts:259-275):
// a status outside completed/failed/unknown or an observation over the byte
// ceiling is invalid; a stale epoch refuses EPOCH_STALE; a completed builtin
// domain tool with no domain write behind it refuses DOMAIN_EFFECT_REQUIRED
// (:263-264); and an action that is no longer running either replays the same
// outcome unchanged or conflicts (:266-271). Nothing here writes: the caller
// applies the answer inside its one transaction.
func DecideFinish(action Action, request FinishRequest) (Decision, error) {
	switch request.Status {
	case ActionCompleted, ActionFailed, StatusUnknown:
	default:
		return Decision{}, failure(CodeToolResultInvalid)
	}
	if len(request.Observation) > MaxObservationBytes {
		return Decision{}, failure(CodeToolResultInvalid)
	}
	if !request.EpochCurrent {
		return Decision{}, failure(CodeEpochStale)
	}
	if request.Status == ActionCompleted && domainTool(action.Name) && !action.HasDomainEffect {
		return Decision{}, failure(CodeDomainEffectRequired)
	}
	if action.Status != ActionRunning {
		if action.HasDomainEffect && action.Status == ActionCompleted && request.Status == ActionCompleted {
			return Decision{Kind: KindReplay}, nil
		}
		if action.Status == request.Status && action.Observation == request.Observation {
			return Decision{Kind: KindReplay}, nil
		}
		return Decision{}, failure(CodeToolResultConflict)
	}
	return Decision{Kind: KindApply}, nil
}

// domainTool reports whether a tool may only be finished as completed when its
// domain write already happened (agent-generation.ts:263).
func domainTool(name string) bool {
	return name == "propose_novel_config" || name == "propose_chapter_blueprint"
}

// Planning is the range a blueprint or architecture registration freezes
// (agent-generation.ts:281-283).
type Planning struct {
	From        int64
	To          int64
	TargetUnits int64
}

// Registration is the metadata registerWorkflow stamps onto one start_workflow
// action (electron/services/agent-generation.ts:281-284). It is metadata and
// never authority: AdmitRegistration re-proves the model, the parent root, the
// operation, the chapter and the planning range against the child begin before
// it authorizes anything, which is why a registration cannot be spent on a
// workflow it was not created for.
type Registration struct {
	ID                 string
	Workflow           string
	ChapterNumber      int64
	HasChapterNumber   bool
	Planning           *Planning
	ParentRootActionID string
	ModelID            string
	State              string
	ChildRunIDs        []string
}

// Register answers registerWorkflow for one action
// (electron/services/agent-generation.ts:276-293). The second return value says
// whether this call created the registration: when the action already carries
// one, the same metadata comes back and nothing is stamped again, which is the
// idempotency the reference gets from `if (action.workflow) return action`.
//
// The planning range is computed exactly as the reference computes it
// (`from = start_chapter`, `to = start_chapter + chapter_count - 1`,
// `targetUnits`). The reference lets a non-numeric planning argument become
// NaN; Go cannot carry NaN through the ledger's JSON, so a planning argument
// that is not a safe integer refuses WORKFLOW_INVALID here. That is the one
// place this layer is stricter, and it is stricter only over values the
// reference could not have stored either.
func Register(action Action, toolCallID, modelID, rootActionID string, epochCurrent, sourcesCurrent, rootActive bool) (Registration, bool, error) {
	if action.Registration != nil {
		return *action.Registration, false, nil
	}
	if action.Status != ActionRunning || action.Name != "start_workflow" ||
		!epochCurrent || !rootActive || !sourcesCurrent {
		return Registration{}, false, failure(CodeWorkflowNotAuthorized)
	}
	for key := range action.Arguments {
		switch key {
		case "workflow", "chapter_number", "start_chapter", "chapter_count", "target_units":
		default:
			return Registration{}, false, failure(CodeWorkflowInvalid)
		}
	}
	workflow, _ := action.Arguments["workflow"].(string)
	switch workflow {
	case WorkflowDraft, WorkflowBlueprint, WorkflowArchitecture:
	default:
		return Registration{}, false, failure(CodeWorkflowInvalid)
	}
	registration := Registration{
		ID:                 registrationIDPrefix + hashOf(toolCallID),
		Workflow:           workflow,
		ParentRootActionID: rootActionID,
		ModelID:            modelID,
		State:              RegistrationRegistered,
		ChildRunIDs:        []string{},
	}
	if workflow == WorkflowDraft {
		chapter, ok := integer(action.Arguments["chapter_number"])
		if !ok || chapter < 1 {
			return Registration{}, false, failure(CodeWorkflowInvalid)
		}
		registration.ChapterNumber = chapter
		registration.HasChapterNumber = true
		return registration, true, nil
	}
	from, ok := integer(action.Arguments["start_chapter"])
	if !ok {
		return Registration{}, false, failure(CodeWorkflowInvalid)
	}
	count, ok := integer(action.Arguments["chapter_count"])
	if !ok {
		return Registration{}, false, failure(CodeWorkflowInvalid)
	}
	targets, ok := integer(action.Arguments["target_units"])
	if !ok {
		return Registration{}, false, failure(CodeWorkflowInvalid)
	}
	registration.Planning = &Planning{From: from, To: from + count - 1, TargetUnits: targets}
	return registration, true, nil
}

// ChildRequest is what a child begin offers when it claims a registration
// (electron/services/main-generation-owner.ts admits children through
// admitWorkflow). ParentOperation is the operation of the parent root the child
// names; OwnerStatus is the status of the action that carries the registration;
// ExistingChildren is how many child runs of this registration already froze
// the same operation and chapter; Planning is the range the child's author
// inputs declare.
type ChildRequest struct {
	RegistrationID     string
	Operation          string
	ModelID            string
	ParentRootActionID string
	ParentOperation    string
	OwnerStatus        string
	ChapterNumber      int64
	HasChapterNumber   bool
	Batch              bool
	ExistingChildren   int
	Planning           *Planning
	RootCurrent        bool
	SourcesCurrent     bool
}

// Admission is the answer to a child begin: ordinary (no registration, not a
// child of an agent round), existing (a child run already exists and must be
// handed back instead of opened twice) or new.
type Admission struct {
	Kind         string
	Registration Registration
}

// childOperations is the finite set of child operations one workflow may launch
// (electron/services/agent-generation.ts:301-304).
func childOperations(workflow string) []string {
	switch workflow {
	case WorkflowDraft:
		return []string{"chapter-draft"}
	case WorkflowBlueprint:
		return []string{"chapter-blueprint-directory"}
	case WorkflowArchitecture:
		return []string{"generate-global-config", "generate-core-seed", "character-architecture",
			"generate-world-building", "generate-plot-outline"}
	}
	return nil
}

// AdmitRegistration decides whether a child begin may proceed, in the
// reference's order (electron/services/agent-generation.ts:305-341):
//
//  1. no registration id: a child of an agent-round root refuses
//     REGISTRATION_REQUIRED, anything else is an ordinary begin;
//  2. the id must be agent-workflow:<64 hex>, else REGISTRATION_INVALID;
//  3. it must resolve to exactly one registration, else REGISTRATION_MISSING;
//  4. the owning action must be running or completed, the model, parent root,
//     operation, chapter and absence of batch/continue-directory intent must
//     match, else CHILD_NOT_AUTHORIZED;
//  5. more than one existing child of the same slot is CHILD_AMBIGUOUS, one is
//     an EXISTING replay;
//  6. a registration with a planning range must see that exact range in the
//     child's author inputs, else CHILD_NOT_AUTHORIZED;
//  7. and, only for a child that does not exist yet, the root must still be
//     current (TOOL_STALE) and the sources must still match (SOURCE_CHANGED).
//
// This is the whole "metadata is not authority" rule: the registration carries
// no capability, and every one of these fields is re-proved here against what
// the child begin actually asked for.
func AdmitRegistration(registrations []Registration, request ChildRequest) (Admission, error) {
	if request.RegistrationID == "" {
		if request.ParentOperation == Operation {
			return Admission{}, failure(CodeRegistrationRequired)
		}
		return Admission{Kind: AdmitOrdinary}, nil
	}
	if !registrationPattern.MatchString(request.RegistrationID) {
		return Admission{}, failure(CodeRegistrationInvalid)
	}
	var found []Registration
	for _, registration := range registrations {
		if registration.ID == request.RegistrationID {
			found = append(found, registration)
		}
	}
	if len(found) != 1 {
		return Admission{}, failure(CodeRegistrationMissing)
	}
	registration := found[0]
	if request.OwnerStatus != ActionRunning && request.OwnerStatus != ActionCompleted {
		return Admission{}, failure(CodeChildNotAuthorized)
	}
	if request.ModelID != registration.ModelID || request.ParentRootActionID != registration.ParentRootActionID ||
		request.Batch || !containsChildOperation(childOperations(registration.Workflow), request.Operation) {
		return Admission{}, failure(CodeChildNotAuthorized)
	}
	if registration.Workflow == WorkflowDraft &&
		(!request.HasChapterNumber || request.ChapterNumber != registration.ChapterNumber) {
		return Admission{}, failure(CodeChildNotAuthorized)
	}
	if request.ExistingChildren > 1 {
		return Admission{}, failure(CodeChildAmbiguous)
	}
	if request.ExistingChildren == 1 {
		return Admission{Kind: AdmitExisting, Registration: registration}, nil
	}
	if registration.Planning != nil {
		if request.Planning == nil || *request.Planning != *registration.Planning {
			return Admission{}, failure(CodeChildNotAuthorized)
		}
	}
	if !request.RootCurrent {
		return Admission{}, failure(CodeToolStale)
	}
	if !request.SourcesCurrent {
		return Admission{}, failure(CodeSourceChanged)
	}
	return Admission{Kind: AdmitNew, Registration: registration}, nil
}

// containsChildOperation reports whether an operation may be launched by a
// workflow.
func containsChildOperation(operations []string, operation string) bool {
	for _, candidate := range operations {
		if candidate == operation {
			return true
		}
	}
	return false
}
