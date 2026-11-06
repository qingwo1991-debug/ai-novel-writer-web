//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/characterstate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves `finalized-character:commit` (web/operations.json:200), the
// last unbound name of the finalized-character family and the only write in it
// that turns a model's state answer into author-visible roster changes. It binds
// that one name and installs nothing else; the four names its siblings own keep
// their binders:
//
//	finalized-character:read-context           production_finalized_character_linux.go:201
//	finalized-character:list-state-candidates  production_character_state_candidates_linux.go:213
//	finalized-character:read-state-candidate   production_character_state_candidates_linux.go:214
//	finalized-character:decide-state-candidate production_character_state_candidates_linux.go:215
//
// # The reference
//
//	electron/controllers/generation-controller.ts:219
//	  register('finalized-character:commit', 1, (owner, request) => owner.commitFinalizedCharacterStates(request))
//	electron/services/main-generation-owner.ts:1513
//	  commitFinalizedCharacterStates: (request: FinalizedCharacterGenerationCommit) =>
//	    finalizedCharacters.commit(request, characters, assertSourcesCurrent)
//	electron/services/finalized-character-generation.ts:63-83
//	  commit(request, characters, assertSources) {
//	    this.assertCurrent()                                                              (:65)
//	    if (!request || Object.keys(request).some(key => !['contextId','handle','artifact'].includes(key)))
//	      throw new Error('GENERATION_CHARACTER_COMMIT_INVALID')                           (:66-67)
//	    const context = this.contexts.get(request.contextId)                               (:69)
//	    ... proveFinalizedCharacterGeneration(...)                                        (:74)
//	    if (!isDeepStrictEqual(proof.context, context)) throw new Error('GENERATION_CHARACTER_CONTEXT_MISMATCH') (:75)
//	    const receipt = SummaryRepository.commitFinalizedCharacterStates(
//	      context, proof.response, this.db, this.transferOrigin())                        (:77)
//	SummaryRepository.commitFinalizedCharacterStates is electron/repositories/summary-repository.ts:423-475.
//	The parser is src/shared/finalized-continuity.ts:177 and is called with the
//	settled artifact's text and the frozen context
//	(electron/services/finalized-character-generation-proof.ts:58):
//	  const response = parseFinalizedCharacterStateResponse(artifact.text, context)
//
// The wire declaration is src/shared/finalized-character-generation.ts:22:
//
//	'finalized-character:commit': { args: [FinalizedCharacterGenerationCommit];
//	  return: FinalizedCharacterGenerationReceipt }
//
// and the store's own answer is FinalizedCharacterStateCommitReceipt
// (src/shared/finalized-continuity.ts:163-167):
//
//	{ applied: number; unchanged: number; candidates: FinalizedCharacterStateCandidate[] }
//
// # The two halves it joins, both already served
//
//	internal/characterstate/state_response.go:307
//	  ParseResponse(content string, context characterstate.Context) (characterstate.Response, error)
//	internal/store/finalized_character_commit.go:182-183
//	  (*FinalizedCharacters).CommitFinalizedCharacterStates(ctx context.Context,
//	    frozen store.FinalizedCharacterContext, response characterstate.Response,
//	    originProjectID string) (*store.FinalizedCharacterStateCommitReceipt, error)
//
// Nothing in either package needs a change for this binder: the parser is called
// here with the frozen context the adapter just proved, and the commit is handed
// that same context, the parser's own answer and the reference's optional transfer
// origin. The store re-reads the context inside its one BEGIN IMMEDIATE
// (finalized_character_commit.go:203-219) and refuses FINALIZED_CHARACTER_CONTEXT_CHANGED
// when the two disagree, which is the reference's :429-434 guard; the parser is
// also invoked a second time inside that transaction (:435, via
// finalizedCharacterCommitVerify) so a state answer is never trusted once.
//
// # The body this lane can honestly serve
//
// The reference request names three references into main-process state
// (src/shared/finalized-character-generation.ts:11-15):
//
//	contextId  a key into the owner's 64-entry context Map (finalized-character-generation.ts:23, :69)
//	handle     a generation run handle, proved against the run's frozen manifest (proof.ts:13-57)
//	artifact   { artifactId, revision, textHash } of a settled artifact (proof.ts:54-58)
//
// This lane holds none of the three, and it must not fake any of them:
//
//   - There is no context cache. `finalized-character:read-context` mints a fresh
//     id per call and stores nothing, and
//     production_finalized_character_linux.go:77-103 records why: a commit that
//     trusted a browser-chosen key in a process-local map would accept an id the
//     client picked instead of one the server proved. The id is therefore not a
//     body key here.
//   - There is no artifact proof. proveFinalizedCharacterGeneration requires a
//     manifest with operation 'finalized-character-state', an author input named
//     'finalized-character-context' whose text hashes to
//     sourceManifest.finalizedCharacterContextHash, selectedFinalizedDraftIds and a
//     model receipt (proof.ts:25-28, :42-46, :59-60), and no begin in this lane writes those keys
//     (synthetic_generation_linux.go:465-479 freezes only the author inputs the
//     browser sent). An artifact reference accepted without that proof would let a
//     caller name any run's text.
//
// The served body is therefore exactly the two facts the adapter cannot obtain
// from the admission, and nothing that could name a scope:
//
//	{ draftId: number, text: string }
//
//	draftId  which draft's frozen context to prove. It is the same value question
//	         `finalized-character:read-context` asks, refused with the same
//	         GENERATION_CHARACTER_DRAFT_INVALID through the very same guard
//	         (production_finalized_character_linux.go:219, productionFinalizedCharacterDraft at :256),
//	         so one question never has two codes.
//	text     the model's answer, i.e. the `artifact.text` the reference parses
//	         (proof.ts:58) and the parser's own `content` argument
//	         (state_response.go:307). It is deliberately not named `content`: in
//	         this family `content` is the frozen prose (finalized-continuity.ts:113-124),
//	         and this key must never be confused with it.
//
// The adapter then does what the reference's commit does with its cached context
// and its proof: it reads the frozen context from the admitted database under the
// admitted scope, parses the answer against that context, and commits. The context
// is never taken from the body, so a body cannot forge a project, an epoch, a
// character list, a content hash or a source order.
//
// # The request guard
//
// The reference's commit is the one body of this family that refuses an unknown
// key (finalized-character-generation.ts:66-67) - the four candidate bodies
// deliberately do not (production_character_state_candidates_linux.go:161-165) - so
// this port reproduces that allowlist over its own two keys: a body that is not an
// object, that carries a key other than draftId/text, or whose text is not text is
// GENERATION_CHARACTER_COMMIT_INVALID as an HTTP 400, which is this lane's status
// for a request-shaped refusal (production_finalized_character_linux.go:282-290).
// An absent, fractional, textual or out-of-range draftId is the read's own
// GENERATION_CHARACTER_DRAFT_INVALID. A blank text is NOT a shape refusal: the
// parser owns the answer, and an empty string is refused there as
// FINALIZED_CHARACTER_RESPONSE_BODY_INVALID (:179), exactly as the reference's
// JSON.parse refuses it.
//
// # The admission
//
// The scope is the frozen project epoch, never anything from the request body:
// productionDurableOpen.borrowAuthorDatabase (production_durable_linux.go:313-364)
// parses the frozen three-key context, names the caller's own cookie/tab owner,
// refuses a project this lane never opened, re-proves the live lease and the
// admitted handle identity through the runtime, and hands over one
// (*sql.DB, release) pair whose release re-verifies the permit - so the permit
// adopts the content identity this commit wrote instead of leaving the next
// operation refused with AUTHOR_WRITE_STALE. A stale, foreign, closed or absent
// lease is refused there before any SQL runs, which is the Go side of the
// reference's assertCurrent (:65) and of projectAccess.assertCurrentProjectContext
// (generation-controller.ts:186). The scope handed to the store is then the
// borrowed project and the borrowed lease, spelled the way the sibling read spells
// it (production_finalized_character_linux.go:243-244).
//
// # The frozen context
//
// The context is read through store.FinalizedCharacters.ReadContext
// (internal/store/finalized_character_context.go:185-204) - the same call
// `finalized-character:read-context` serves - so the two operations can never
// disagree about which source they proved. That read opens its own
// BeginTx(ReadOnly: true) and commits without a write, so this adapter wraps
// nothing in a second transaction: the commit below owns the only write
// transaction on the path, and nesting one would add a lock without adding an
// invariant. The store's own commit then re-reads the context as the first
// statement of its BEGIN IMMEDIATE (finalized_character_commit.go:206-213) and
// compares it field for field (:210-219), so the bytes this file parsed against
// are the bytes it writes into.
//
// # The parser call
//
// ParseResponse is called with `text` and with the context just read, converted
// member by member into the parser's leaf mirror (state_response.go:57-84). Only
// the members the parser reads are converted: the source chapter number (:195-197,
// :206), the frozen prose (:190-192) and the frozen identities (:211, :215,
// :219-222). The field snapshots and the epochs are deliberately not copied: the
// parser never reads a frozen field list, and a scope it cannot see is a scope it
// cannot trust.
//
// A parse refusal is returned as the parser raised it: its error value's message
// IS the reference's code (:226-296), and its *EvidenceError carries the update
// index and the reason beside a bare code, not prose. The handler maps it exactly
// once, in the table below.
//
// # The receipts, and the two keys this lane omits
//
// The store's receipt is returned exactly as the store produced it:
// {applied, unchanged, candidates} in the reference's key order
// (finalized_character_commit.go:123-131). The reference's own IPC answer spreads
// two further keys onto it (finalized-character-generation.ts:81): `unresolved`,
// copied from the proof's parsed response, and `proposalBatchId`, minted by the
// character-proposal service. This lane adds neither, and not only because the
// store's receipt has no such member:
//
//   - `unresolved` is provably empty whenever a receipt exists. The store refuses
//     the whole commit when the re-parsed answer holds one occurrence
//     (finalized_character_commit.go:225-228, the reference's :436), so a receipt
//     can only be the answer to a response whose every update resolved to a frozen
//     character. A key that is always [] is not a fact this lane has to invent.
//   - `proposalBatchId` names a proposal batch from a service with no Go port (no
//     file in this module names CharacterProposalService), and the reference only
//     sets it for an answer that still carried a nameless occurrence
//     (finalized-character-generation.ts:79-80) - which the refusal above excludes.
//
// So the answer is the store value, and a caller reads {applied, unchanged,
// candidates} from it. Reshaping it into the desktop envelope would be inventing
// two facts this lane cannot prove.
//
// originProjectId is the empty string: the reference passes
// `this.transferOrigin()` (:77), which is the main-verified authority of a
// cross-project transfer, and this lane records no transfer origin at all
// (production_finalized_character_linux.go:69-75 omits the same value from the read
// answer). The store treats "" as the reference's absent optional argument
// (finalized_character_commit.go:175-183), so a derived field may still
// advance within this project's own generations.
//
// # The refusals, in two layers
//
// The parser's refusals and the store's refusals are different layers and each
// keeps its own code; the mapping runs exactly once, in the handler, through the
// single mapper below. The two tables say which value is published under which
// string, so a code and its value cannot drift apart unnoticed. A value this lane
// does not name is replaced by the reference's unmapped-failure code,
// generationIpcErrorCode's last line (generation-controller.ts:101): never a
// SQLite message, a path, a draft body or a provider sentence.
//
// productionFinalizedCharacterCommitStates hands its errors back untouched, so
// errors.Is still names the refusal to a test - and mapping twice would map the
// mapped value (a plain error carrying only the code) and publish the
// unmapped-failure code for a refusal that has a name.
//
// The borrow's own refusals are NOT run through that mapper: they are already
// symbolic (PRODUCTION_CLOSED, PROJECT_NOT_OPEN, PROJECT_SESSION_STALE,
// AUTHOR_WRITE_STALE), exactly as the sibling read returns them, and so are a
// malformed frozen context (PROJECT_SESSION_REQUIRED, 400), the body guard above
// (400) and the fail-closed half-borrow arm.
type productionFinalizedCharacterCommit struct {
	durable *productionDurableOpen
}

const (
	// productionFinalizedCharacterCommitName is the one name this file binds.
	productionFinalizedCharacterCommitName = "finalized-character:commit"
	// productionFinalizedCharacterCommitInvalid is the reference's own request
	// guard (finalized-character-generation.ts:66-67): the body is not an object,
	// carries a key outside the allowlist, or does not carry the model's answer as
	// text.
	productionFinalizedCharacterCommitInvalid = "GENERATION_CHARACTER_COMMIT_INVALID"
	// The two body keys, spelled once so the allowlist and the reads cannot drift.
	productionFinalizedCharacterCommitDraftKey = "draftId"
	productionFinalizedCharacterCommitTextKey  = "text"
)

// productionFinalizedCharacterCommitParseRefusals is the parser's refusal set with
// the code each one is published under. Every code is the value's own string
// (internal/characterstate/state_response.go:226-296), so the two halves of this
// family publish one vocabulary; the table exists so the mapping is stated once,
// next to the line that raises it.
var productionFinalizedCharacterCommitParseRefusals = [...]struct {
	refusal error
	code    string
}{
	// :179 JSON.parse refused the trimmed, unfenced body. Port-only code (the
	// reference's SyntaxError carries none) and kept distinct on purpose: the
	// desktop owner treats a SyntaxError as a repairable body, not as a refused
	// contract (state_response.go:251-259).
	{characterstate.ErrBodyInvalid, "FINALIZED_CHARACTER_RESPONSE_BODY_INVALID"},
	// :180 !record(parsed) || !Array.isArray(parsed.updates) || parsed.updates.length > 1_000
	{characterstate.ErrUpdatesInvalid, "FINALIZED_CHARACTER_UPDATES_INVALID"},
	// :184 !record(input) || !record(input.currentState)
	{characterstate.ErrUpdateInvalid, "FINALIZED_CHARACTER_UPDATE_INVALID"},
	// :203 a state value that is not text, and :206-208 an empty currentState or an
	// updatedAtChapter that is not the frozen chapter.
	{characterstate.ErrFieldInvalid, "FINALIZED_CHARACTER_FIELD_INVALID"},
	// :217 an occurrence with neither a name nor an id.
	{characterstate.ErrIdentityRequired, "FINALIZED_CHARACTER_IDENTITY_REQUIRED"},
	// :225 the same characterId twice in one answer.
	{characterstate.ErrDuplicateID, "FINALIZED_CHARACTER_DUPLICATE_ID"},
	// :186 no evidence object at all.
	{characterstate.ErrEvidenceMissing, "FINALIZED_CHARACTER_EVIDENCE_MISSING"},
	// :191-192 a text that occurs nowhere, or more than once, in the frozen prose.
	{characterstate.ErrEvidenceNotUnique, "FINALIZED_CHARACTER_EVIDENCE_NOT_UNIQUE"},
	// :195-199 bad offsets, or a text the frozen prose does not hold at them.
	{characterstate.ErrEvidenceInvalid, "FINALIZED_CHARACTER_EVIDENCE_INVALID"},
}

// productionFinalizedCharacterCommitStoreRefusals is the store's refusal set with
// the code each one is published under. The first twelve values are
// internal/store/finalized_character_commit.go's own declarations
// (:61-101 plus the package's ErrProjectNotOpen, projectcore.go:12); the rest are
// raised by the read this adapter performs first
// (finalized_character_context.go:24-46), by the freeze-receipt reader it depends
// on (summary.go:22) and by the projection refresh the commit runs
// (character_roster.go:405, :440). The read family's table
// (production_finalized_character_linux.go:169-190) publishes the shared ones under
// these same strings, so one refusal never has two codes.
var productionFinalizedCharacterCommitStoreRefusals = [...]struct {
	refusal error
	code    string
}{
	// summary-repository.ts:433-434 the frozen identity is no longer what the
	// caller holds, or :447 a frozen field snapshot does not hash to its value.
	{store.ErrFinalizedCharacterContextChanged, "FINALIZED_CHARACTER_CONTEXT_CHANGED"},
	// summary-repository.ts:436 an answer with an occurrence that resolved to no
	// frozen character, and :227 the same code from the candidate normalizer.
	{store.ErrFinalizedCharacterIdentityRequired, "FINALIZED_CHARACTER_IDENTITY_REQUIRED"},
	// summary-repository.ts:452 decideDerivedPatch answered source-conflict.
	{store.ErrFinalizedCharacterSourceConflict, "FINALIZED_CHARACTER_SOURCE_CONFLICT"},
	// summary-repository.ts:453 decideDerivedPatch answered field-conflict.
	{store.ErrFinalizedCharacterFieldConflict, "FINALIZED_CHARACTER_FIELD_CONFLICT"},
	// summary-repository.ts:222 the freeze receipt a staged candidate needs is gone.
	{store.ErrFinalizedCharacterSnapshotRequired, "FINALIZED_CHARACTER_SNAPSHOT_REQUIRED"},
	// summary-repository.ts:232 the proposed evidence does not sit on the frozen
	// prose at the offsets it claims.
	{store.ErrFinalizedCharacterEvidenceInvalid, "FINALIZED_CHARACTER_EVIDENCE_INVALID"},
	// summary-repository.ts:219, :245, :249 and :658: a candidate list this store
	// cannot even normalize.
	{store.ErrContinuityCharacterStateCandidatesInvalid, "CONTINUITY_CHARACTER_STATE_CANDIDATES_INVALID"},
	// summary-repository.ts:663 the frozen source is not the snapshot's source.
	{store.ErrContinuitySourceStale, "CONTINUITY_SOURCE_STALE"},
	// summary-repository.ts:666 the projection generation moved under the answer.
	{store.ErrContinuityGenerationStale, "CONTINUITY_GENERATION_STALE"},
	// summary-repository.ts:686 the snapshot row and its receipt disagree.
	{store.ErrContinuityProjectionMissing, "CONTINUITY_PROJECTION_MISSING"},
	// summary-repository.ts:240 the legacy roster cannot name a character
	// unambiguously.
	{store.ErrContinuityRosterNameConflict, "CONTINUITY_ROSTER_NAME_CONFLICT"},
	// summary-repository.ts:426 and :650 a missing database handle. The borrow never
	// hands one over, so this only answers a repository rebuilt without a pool.
	{store.ErrProjectNotOpen, "PROJECT_NOT_OPEN"},
	// summary-repository.ts:402 a blank project or epoch in the scope the admission
	// supplied.
	{store.ErrFinalizedCharacterScopeRequired, "FINALIZED_CHARACTER_SCOPE_REQUIRED"},
	// summary-repository.ts:406 and :409: no finalized source, or a newer finalized
	// draft of the same chapter superseded it.
	{store.ErrFinalizedCharacterSourceChanged, "FINALIZED_CHARACTER_SOURCE_CHANGED"},
	// summary-repository.ts:413 a frozen character's row is absent or retired.
	{store.ErrFinalizedCharacterIDRetired, "FINALIZED_CHARACTER_ID_RETIRED"},
	// summary-repository.ts:84 a stored provenance revision that is not a safe
	// integer.
	{store.ErrFinalizedCharacterFieldRevisionInvalid, "FINALIZED_CHARACTER_FIELD_REVISION_INVALID"},
	// summary-repository.ts:75 a freeze receipt that does not hash to its own bytes.
	{store.ErrSummaryCharacterSnapshotInvalid, "FINALIZED_CHARACTER_SNAPSHOT_INVALID"},
	// Port-only precondition (finalized_character_context.go:41-45). Unreachable
	// through ReadContext, which opens its own transaction; mapped so a future
	// nesting variant cannot leak an unmapped error.
	{store.ErrFinalizedCharacterReadTransactionRequired, "FINALIZED_CHARACTER_READ_TRANSACTION_REQUIRED"},
	// The projection refresh of an applied field refuses a character row whose text
	// has no UTF-8 representation (character_roster.go:405, :440).
	{store.ErrCharacterInputUnsupported, "CHARACTER_INPUT_UNSUPPORTED"},
}

// newProductionFinalizedCharacterCommit wraps the lane's durable bridge, the only
// object that can name the author database.
func newProductionFinalizedCharacterCommit(durable *productionDurableOpen) *productionFinalizedCharacterCommit {
	return &productionFinalizedCharacterCommit{durable: durable}
}

// Bind installs `finalized-character:commit` and nothing else. A nil or closed
// bridge installs nothing, so the name keeps failing closed at
// GO_OPERATION_NOT_MIGRATED instead of being published as a handler that can only
// refuse. Bind refuses an uninventoried name (OPERATION_NOT_EXPOSED) and a double
// bind (OPERATION_ALREADY_BOUND), so a collision with another binder is a startup
// error rather than silent dead code.
func (c *productionFinalizedCharacterCommit) Bind(server *transport.Server) error {
	if c == nil || c.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	return server.Bind(productionFinalizedCharacterCommitName,
		func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
			// Wire arity is the request arity (one, generation-controller.ts:219)
			// plus the frozen context.
			if len(args) != 2 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			// The reference refuses a missing or malformed session with
			// GENERATION_PROJECT_SESSION_REQUIRED (generation-controller.ts:184). The
			// frozen context is this lane's transport envelope, and every binder in it
			// answers with the transport's own code.
			admitted, err := projectsession.ParseContext(args[1])
			if err != nil {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
			}
			// The body guard runs before the borrow, so a body this lane cannot read
			// never reaches the database.
			draftID, content, err := productionFinalizedCharacterCommitRequestBody(args[0])
			if err != nil {
				return nil, err
			}
			if ctx == nil {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			db, release, err := c.durable.borrowAuthorDatabase(ctx, caller, args[1])
			if err != nil {
				// Already symbolic: a lease, ownership or closed-lane refusal. It is
				// not a store refusal, so it does not go through the mapper below.
				return nil, err
			}
			if db == nil || release == nil {
				// The bridge never returns a half-open borrow; if it ever does the
				// lane fails closed rather than writing a database nobody admitted.
				return nil, errors.New(productionFinalizedCharacterRequestFailed)
			}
			defer release()
			receipt, err := productionFinalizedCharacterCommitStates(ctx, db, draftID, content,
				store.FinalizedCharacterScope{ProjectID: admitted.ProjectID, Epoch: admitted.LeaseID})
			if err != nil {
				// The one and only mapping layer: the parser's refusal and the
				// store's refusal are turned into their symbolic codes here, and
				// nowhere else.
				return nil, productionFinalizedCharacterCommitFailure(err)
			}
			return receipt, nil
		})
}

// productionFinalizedCharacterCommitRequestBody is the `{ draftId, text }`
// projection of this lane's commit body. The allowlist reproduces
// finalized-character-generation.ts:66-67 - the commit is the one body of this
// family that refuses a key it does not read - and the draft id is the read's own
// guard, reused so one value question cannot answer with two codes. A text that is
// not text is the commit's own GENERATION_CHARACTER_COMMIT_INVALID; a blank text is
// left to the parser, which owns the answer.
func productionFinalizedCharacterCommitRequestBody(value any) (int64, string, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return 0, "", productionFinalizedCharacterCommitRefusal()
	}
	for key := range object {
		if key != productionFinalizedCharacterCommitDraftKey && key != productionFinalizedCharacterCommitTextKey {
			return 0, "", productionFinalizedCharacterCommitRefusal()
		}
	}
	draftID, err := productionFinalizedCharacterDraft(object)
	if err != nil {
		return 0, "", err
	}
	content, ok := object[productionFinalizedCharacterCommitTextKey].(string)
	if !ok {
		return 0, "", productionFinalizedCharacterCommitRefusal()
	}
	return draftID, content, nil
}

// productionFinalizedCharacterCommitRefusal is the reference's request guard as a
// client error (finalized-character-generation.ts:66-67), refused with 400 the way
// this lane's other request-shaped guards are. It is an HTTPError, so the handler
// returns it unchanged rather than running it through the refusal mapper, which
// knows nothing about it and would publish the unmapped-failure code instead.
func productionFinalizedCharacterCommitRefusal() error {
	return security.NewHTTPError(http.StatusBadRequest, productionFinalizedCharacterCommitInvalid)
}

// productionFinalizedCharacterCommitStates is the served commit over an already
// admitted database and the admitted scope, in the reference's order: the frozen
// context is read first (:69-75), the answer is parsed against it (:74, :58 of the
// proof module, the reference's :435), and only then is the store's commit asked to
// write (:77). It is split out of the handler so the answer and its refusals can be
// exercised over an admitted fixture without an HTTP round trip, and so nothing in
// it can quietly take a context, a project or an epoch from the request. Its errors
// are the parser's and the store's own values, unmapped: the handler is the one
// place that turns them into wire codes, so a caller can still ask errors.Is which
// refusal it was.
func productionFinalizedCharacterCommitStates(ctx context.Context, db *sql.DB, draftID int64, content string,
	scope store.FinalizedCharacterScope) (*store.FinalizedCharacterStateCommitReceipt, error) {
	// One read-only transaction, opened and committed by the store itself
	// (finalized_character_context.go:185-204). The result is the same value
	// `finalized-character:read-context` serves, so the commit and the read can
	// never prove two different sources.
	frozen, err := store.NewFinalizedCharacters(db).ReadContext(ctx, draftID, scope)
	if err != nil {
		return nil, err
	}
	if frozen == nil {
		// ReadContext refuses instead of answering nothing; a nil context here is
		// the fail-closed half-borrow shape, not an empty edition.
		return nil, errors.New(productionFinalizedCharacterRequestFailed)
	}
	// The parser is called with the frozen context this adapter just proved, never
	// with one derived from the request (state_response.go:298-312).
	response, err := characterstate.ParseResponse(content,
		productionFinalizedCharacterCommitParseContext(*frozen))
	if err != nil {
		return nil, err
	}
	// One BEGIN IMMEDIATE, opened and committed by the store itself
	// (finalized_character_commit.go:189-201). It re-reads this same context as its
	// first statement and re-parses the answer (:206-228), so the two halves cannot
	// be split by another writer.
	receipt, err := store.NewFinalizedCharacters(db).CommitFinalizedCharacterStates(ctx, *frozen, response, "")
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		// The store answers a receipt or a refusal; a nil receipt would be a write
		// this lane cannot account for.
		return nil, errors.New(productionFinalizedCharacterRequestFailed)
	}
	return receipt, nil
}

// productionFinalizedCharacterCommitParseContext is the adapter's conversion of the
// store's frozen context into the parser's leaf mirror (state_response.go:57-84).
// This is where the two halves meet: the chapter the model must match
// (:195-197, :206), the immutable prose its evidence offsets count in
// (:190-192) and the frozen identities it may resolve (:211, :215, :219-222). The
// field snapshots, the epochs and the projection generation stay behind - the
// parser reads none of them, and the scope a caller must not forge is exactly what
// it must not see.
func productionFinalizedCharacterCommitParseContext(
	frozen store.FinalizedCharacterContext) characterstate.Context {
	parsed := characterstate.Context{
		Source:         characterstate.ContextSource{ChapterNumber: frozen.Source.ChapterNumber},
		Content:        frozen.Content,
		IdentityStatus: frozen.IdentityStatus,
		Characters:     make([]characterstate.ContextCharacter, 0, len(frozen.Characters)),
	}
	for _, character := range frozen.Characters {
		parsed.Characters = append(parsed.Characters, characterstate.ContextCharacter{
			CharacterID:         character.CharacterID,
			DisplayNameSnapshot: character.DisplayNameSnapshot,
			Aliases:             character.Aliases,
		})
	}
	return parsed
}

// productionFinalizedCharacterCommitParseCode is the parser's symbolic code for one
// of its refusals, or "" for anything it does not name.
func productionFinalizedCharacterCommitParseCode(err error) string {
	for _, entry := range productionFinalizedCharacterCommitParseRefusals {
		if errors.Is(err, entry.refusal) {
			return entry.code
		}
	}
	return ""
}

// productionFinalizedCharacterCommitStoreCode is the store's symbolic code for one
// of its refusals, or "" for anything this lane does not name.
func productionFinalizedCharacterCommitStoreCode(err error) string {
	for _, entry := range productionFinalizedCharacterCommitStoreRefusals {
		if errors.Is(err, entry.refusal) {
			return entry.code
		}
	}
	return ""
}

// productionFinalizedCharacterCommitFailure is the refusal the browser receives:
// the transport's own error unchanged, the parser's code, the store's code, or the
// reference's unmapped-failure code. Never a SQLite message, a path, a draft body
// or a provider sentence. The two layers are asked in turn here and nowhere else;
// a parser value is a different error value from the store value that publishes the
// same string (FINALIZED_CHARACTER_IDENTITY_REQUIRED, FINALIZED_CHARACTER_EVIDENCE_INVALID),
// so the order cannot change the answer.
func productionFinalizedCharacterCommitFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if code := productionFinalizedCharacterCommitParseCode(err); code != "" {
		return errors.New(code)
	}
	if code := productionFinalizedCharacterCommitStoreCode(err); code != "" {
		return errors.New(code)
	}
	return errors.New(productionFinalizedCharacterRequestFailed)
}
