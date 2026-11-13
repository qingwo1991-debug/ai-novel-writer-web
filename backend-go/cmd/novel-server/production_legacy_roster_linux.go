//go:build linux

// This file serves two of the seven `legacy-roster` names - the source proof
// and the adoption - and production_legacy_roster_serve_linux.go serves the
// run's begin/read/cancel beside it, production_legacy_roster_execute_serve_linux.go
// serves execute, and production_legacy_roster_stage_serve_linux.go serves stage,
// so all seven are served:
//
//	legacy-roster:read-source      web/operations.json:263  served here
//	legacy-roster:adopt-existing   web/operations.json:264  served (production_legacy_roster_adopt_linux.go)
//	legacy-roster:begin            web/operations.json:265  served (production_legacy_roster_serve_linux.go)
//	legacy-roster:read             web/operations.json:266  served (same file)
//	legacy-roster:execute          web/operations.json:267  served (production_legacy_roster_execute_serve_linux.go)
//	legacy-roster:stage            web/operations.json:268  served (production_legacy_roster_stage_serve_linux.go)
//	legacy-roster:cancel           web/operations.json:269  served (same file)
//
// # What this family actually is
//
// It is NOT a chapter-directory or old-export reader. The "existing novel" it
// repairs is the cast text the legacy project already holds: the whole family
// reads `character_roster_meta.legacy_markdown` (the preserved old roster, raw
// bytes, never re-parsed from headings by a client -
// src/shared/legacy-roster-generation-pure.ts:41-44) plus the actual
// `characters`, `character_aliases` and `character_relationships` rows, and the
// run it opens is a generation run over that text. So the source is a project
// fact, not a path: nothing here walks a directory, and nothing here can rewrite
// a chapter file, because no handler in this file opens a project file at all -
// every read goes through the borrowed, already-admitted author database.
//
// # The reference
//
//	electron/controllers/generation-controller.ts:233-239
//	  register('legacy-roster:read-source', 0, owner => owner.readLegacyRosterSource())
//	  register('legacy-roster:adopt-existing', 1, (owner, request) => owner.adoptLegacyCards(request))
//	  register('legacy-roster:begin' | ':read' | ':execute' | ':stage' | ':cancel', 1, ...)
//	electron/services/legacy-roster-source.ts:45-61        readLegacyRosterSource
//	electron/services/legacy-roster-source.ts:71-104       adoptLegacyCards
//	electron/services/legacy-roster-generation.ts:1-69     the staged, resumable extraction
//	electron/services/legacy-roster-generation-context.ts:8-27  capture + read context
//	electron/services/legacy-roster-generation-proof.ts:11-45   the artifact proof
//	electron/services/main-generation-owner.ts:1109-1190   the five run names
//	src/shared/legacy-roster-generation-pure.ts:55-66      buildLegacyRosterTask
//
// # What read-source answers
//
// src/shared/legacy-roster-generation.ts:23 is the wire declaration:
//
//	'legacy-roster:read-source': { args: []; return: LegacyRosterSource }
//
//	rawLegacy          character_roster_meta.legacy_markdown, byte for byte
//	legacyHash         sha256 hex of rawLegacy (legacy-roster-source.ts:59)
//	identityRevision   character_identity_meta.revision for 'main' (:51)
//	activeIds          the character_id of every row with retired = 0, in
//	                   character_id order (:60)
//	factsHash          sha256 hex over the ACTUAL rows - characters, aliases and
//	                   relationships, retired identities included - and never over
//	                   the stored meta.fact_hash (:60, and the doc comment above
//	                   it: "Actual rows, including retired identities and author
//	                   state, not stored meta.fact_hash")
//	snapshot           CharacterRosterRepository.read(db), i.e. the store's own
//	                   ReadRoster answer: schemaVersion, revision, migrationState,
//	                   status, entries, identityRevision, aliases,
//	                   renderedMarkdown, projectionHash, factHash,
//	                   nameOnlyFactHash and legacyMarkdown when non-empty
//	genre              project_core.genre, whatever type it holds (the column is
//	                   nullable, so a NULL answers null exactly like the reference
//	                   object, and is not coerced into "")
//	writingLanguage    resolveWritingLanguage(project_core.writing_language)
//	                   (src/shared/writing-language.ts:7-11): one of zh-CN/en-US,
//	                   anything else answered as zh-CN
//
// The five CAS tokens the adoption needs are therefore public before it is
// offered: legacyHash and factsHash and identityRevision and the snapshot
// revision, plus activeIds. That is what makes `read-source` worth serving while
// the write is not (see below): an author can be told, truthfully and in their
// own project's terms, whether the project is waiting for a repair
// (legacy_markdown_pending / legacy_repair_required: no cards and legacy text),
// has cards to adopt (legacy_cards_preserved: status inconsistent, active ids
// present) or needs no repair at all - without this lane inventing a single
// character.
//
// # How a source is proved
//
// The reference's order, reproduced statement for statement, because the order
// decides which code a project that fails twice answers with
// (legacy-roster-source.ts:48-61):
//
//  1. `character_identity_meta` must exist at all (:49) -> LEGACY_ROSTER_ID_SCHEMA_REQUIRED;
//  2. one `project_core` row must exist (:50), and the identity revision (:51) and
//     the legacy markdown (:52) must be values a source can be built from (:53)
//     -> LEGACY_ROSTER_SOURCE_INVALID. The reference folds those three reads into
//     this one guard, and so does this file. Note what :53 asks of the markdown: it
//     tests the type, not the content, so an empty preserved roster is a source
//     (the store's status derivation is what later calls it nothing to repair);
//  3. every `characters.character_id` must be non-blank text (:54-55) -> LEGACY_ROSTER_ID_INVALID;
//  4. the fact rows and the roster snapshot are read and hashed (:56-60), and the
//     store's own roster refusals are published unchanged.
//
// The reference also runs these five statements inside one transaction (:46) so
// the answer describes a single state of the file. This file issues them in the
// same order on the borrowed handle and opens no transaction of its own, because
// the exclusivity that transaction buys is already held: the durable lane owns the
// project's writable database and the legacy writer lock is exclusive, so no other
// writer can exist while a call is in flight
// (internal/authorruntime/draft_read.go:38-42, and the same reasoning at
// production_review_reads_linux.go:143-147), which is why store's own roster read
// can reserve its `BEGIN IMMEDIATE` snapshot without blocking. A transaction here
// would be a second snapshot beside the store's, not a stricter one.
//
// Two properties follow, and both are tested:
//
//   - the digest is over bytes, not over meaning: `legacyHash` is sha256 of the
//     exact UTF-8 markdown, so a resumed run can prove it read the same text, and
//     a replayed read of an unchanged project answers the identical bytes;
//   - `factsHash` covers retired rows. A hash taken over the active ids alone
//     would answer the same value after the author removed a character, so a
//     stale adoption request would still match and a removed character could be
//     resurrected into the roster. The reference hashes the rows themselves
//     (:60), and this file does the same.
//
// # How the bytes are canonicalized
//
// JSON.stringify of a better-sqlite3 `SELECT *` row is: an object keyed in the
// statement's column order, with JS string escaping and JS number formatting.
// encoding/json cannot reproduce it (it escapes <, >, & by default and always
// escapes U+2028/U+2029, which JSON.stringify leaves as literal code points), so
// productionLegacyRosterJSON below is a small JS-faithful encoder and is the only
// serializer the facts hash is taken over. Values this encoder cannot reproduce
// byte for byte are refused rather than hashed approximately: invalid UTF-8, a
// real outside [1e-6, 1e21) whose Go and JavaScript exponent spellings differ,
// and a safe-integer violation. A BLOB is encoded as
// {"type":"Buffer","data":[...]}, which is what JSON.stringify does to a Buffer.
//
// # adopt-existing: served now (production_legacy_roster_adopt_linux.go)
//
// The adoption is not a second extraction. It re-reads the same source, rebuilds
// the read-only projection from the cards the author already has, and records the
// author's explicit approval - it writes no character field, no alias, no
// relationship, no dynamic state and no legacy markdown
// (electron/services/legacy-roster-source.ts:71-104, and the S06D evidence note
// docs/research/novel-quality-modernization/s06d-legacy-roster-evidence.md).
//
// It was refused because the reference does all of it in ONE transaction:
//
//	const requestHash = textHash(JSON.stringify(original)), operationId = prefix + request.operationId
//	return db.transaction(() => {                    // :80
//	  ...replay the stored envelope for this operationId (:81-90)
//	  ...re-prove the source against all four CAS tokens (:91-94)
//	  refreshCharacterIdentityProjection(db)          // :95
//	  ...read the roster back, require status ready and the legacy text kept (:96-97)
//	  db.prepare('INSERT INTO character_identity_approvals ...').run(...)  // :101
//	  return receipt
//	}).immediate()                                    // :103
//
// so a call that fails anywhere between :95 and :101 rolls the projection AND
// the receipt back together, and the database is left exactly as it was. In this
// module that transaction cannot be built:
//
//   - the projection writer is store's characterIdentityRefreshProjection
//     (internal/store/character_roster.go:562), reachable only through
//     (*Characters).RefreshIdentityProjection (:591), which reserves its own
//     connection and runs its own `BEGIN IMMEDIATE`
//     (internal/store/reviewrevision.go:41);
//   - the receipt row lives in character_identity_approvals, written either by
//     characterIdentityCommit behind (*CharacterIdentities).CommitIdentities
//     (internal/store/character_identity.go:161) or by the author-roster commit
//     (internal/store/character_roster_author.go:433), and both open their own
//     transaction. The author-roster path also refuses any roster whose status is
//     not ready or empty (internal/store/character_roster_author.go:188-189), which
//     is exactly the inconsistent, card-preserving state an adoption starts from,
//     so it cannot perform this write even if the transaction were shared;
//   - no exported seam in internal/store hands a caller's *sql.Conn to either
//     (grep: the only conn callback signatures are unexported `immediate`
//     helpers), and this change may not add one.
//
// A handler built out of those two calls would commit the projection first and
// the receipt second, or the receipt first and the projection second. Either way
// a failure in between leaves a half-adopted roster with no honest way back, and
// the failed call would NOT leave the file byte-identical - which is the one thing
// this family's write must guarantee. Serving nothing keeps the name at the
// fail-closed 501 GO_OPERATION_NOT_MIGRATED (internal/transport/server.go), which
// is what a name with a missing port is supposed to say.
//
// That port is now built: (*Characters).AdoptLegacyRoster
// (internal/store/character_roster_adoption.go) runs the projection refresh and
// the approval insert on one reserved connection inside one `immediate` scope,
// reusing characterIdentityRefreshProjection, so the projection is not re-rendered
// in this package. The raw-row facts hash stays here (productionLegacyRosterJSON)
// and is passed back in through the `source` callback.
//
// # begin/read/cancel/execute/stage: all served
//
// begin/read/cancel are the run's own state, and they are served in
// production_legacy_roster_serve_linux.go over the generation owner, using the
// finalization/editor-inline pattern this session re-tested: a shipped prompt
// template in internal/prompttemplate, openRun via ledger.CallJSON("open"),
// dispatch's reproduction for sourceStatus, and the ordinary service's stop for
// cancel. The three old fences are gone:
//
//  1. begin's request now carries the run. The manifest is hand-written with the
//     legacyRoster* members spliced in as ordered JSON text, exactly like
//     productionFinalizationManifestText, so the reduced selection allowlist
//     (syntheticParseSelection) never sees them.
//  2. The ledger now admits the manifest: glFamilyManifestAllowed gained the
//     legacyRosterContext arm for operation "legacy-character-roster-repair"
//     (internal/store/generation_ledger.go:426).
//  3. The run's proof is a prompt, and it is now the shipped one. The prompt is
//     NOT a builtin-prompt-templates key - legacy_roster does not ship there;
//     LEGACY_ROSTER_SYSTEM_PROMPT and CHARACTER_ROSTER_JSON_CONTRACT ship in
//     src/shared/legacy-roster-generation-pure.ts and reach this binary as the
//     committed asset internal/prompttemplate/legacy-roster-v1.json.
//
// execute is served too, in production_legacy_roster_execute_serve_linux.go. The
// loop's first task is the shipped buildLegacyRosterTask (frozen at begin by
// productionLegacyRosterTaskText), its dispatch/readback is
// production_retry_draft_outline_linux.go's pattern, and its two non-happy
// branches - the length-stop replacement prompt
// (structured-replacement-prompt.ts:3-41) and the JSON-repair prompt
// (legacy-roster-generation-pure.ts:38-39, :50-52) - now ship as the committed
// asset internal/prompttemplate/legacy-roster-loop-v1.json. The run's own
// candidate gate is parseLegacyRosterJson plus the candidate closure
// validateLegacyRosterCandidate applies; production_legacy_roster_execute_serve_linux.go
// records that divergence and the attempt-count accounting.
//
// stage is served too, in production_legacy_roster_stage_serve_linux.go. It proves
// the run and its settled artifact, re-proves the frozen binding, and hands the
// canonical source to CharacterProposalService.stage's legacy arm
// (legacy-roster-generation.ts:52-68, the INSERT at character-proposal-service.ts:
// 191-192) in one transaction that lives in the store because the proof and the
// projection are one read of the same facts (internal/store/legacy_roster_proposal.go).
// The proof the earlier refusal note listed as missing is that store file's
// legacyRosterProposalProof, the writer is its Stage, and the legacyRosterProposal
// effect key has its arm in the ledger's allowlist; the frozen manifest carries the
// modelReceipt.modelRevision the proof's last step reads.
//
// # Refusals are symbolic, and there is exactly one mapping layer
//
// productionLegacyRosterFailure is the only place a refusal becomes a wire code.
// Its table names every refusal this file and the store's roster read can raise,
// and the fallback is the controller's own unmapped-failure code
// (generationIpcErrorCode, generation-controller.ts:88-101) - never a SQLite
// message, a path or a row of the author's text. The borrow's own symbolic
// refusals (PRODUCTION_CLOSED, PROJECT_NOT_OPEN, PROJECT_SESSION_STALE,
// AUTHOR_WRITE_STALE) are returned unchanged, as are a malformed frozen context
// (PROJECT_SESSION_REQUIRED, 400) and the fail-closed half-borrow arm.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionLegacyRosterReadSourceName is web/operations.json:263. It is the
	// only name this file binds.
	productionLegacyRosterReadSourceName = "legacy-roster:read-source"

	// The six names web/operations.json:264-269 inventories beside it. All six are
	// bound by the family's other files - adopt-existing here, begin/read/cancel in
	// production_legacy_roster_serve_linux.go, execute in
	// production_legacy_roster_execute_serve_linux.go and stage in
	// production_legacy_roster_stage_serve_linux.go - so
	// productionLegacyRosterSeams publishes nothing and
	// TestProductionLaneRefusesLegacyRosterNames asserts it.
	productionLegacyRosterAdoptExistingName = "legacy-roster:adopt-existing"
	productionLegacyRosterBeginName         = "legacy-roster:begin"
	productionLegacyRosterReadName          = "legacy-roster:read"
	productionLegacyRosterExecuteName       = "legacy-roster:execute"
	productionLegacyRosterStageName         = "legacy-roster:stage"
	productionLegacyRosterCancelName        = "legacy-roster:cancel"

	// productionLegacyRosterRequestFailed is generation-controller.ts:101: the
	// answer for every failure this lane cannot name.
	productionLegacyRosterRequestFailed = "GENERATION_REQUEST_FAILED"

	// productionLegacyRosterMaxSafeInteger is Number.MAX_SAFE_INTEGER, the bound
	// the reference's `revision(value)` and JSON number handling both use.
	productionLegacyRosterMaxSafeInteger = 9007199254740991

	// The two values resolveWritingLanguage can answer
	// (src/shared/writing-language.ts:1, :5).
	productionLegacyRosterLanguageZhCN = "zh-CN"
	productionLegacyRosterLanguageEnUS = "en-US"

	// The identity table whose absence is the reference's first refusal
	// (electron/repositories/character-repository.ts:8-10).
	productionLegacyRosterIdentityTable = "character_identity_meta"

	// The columns this file reads by name. They are spelled once so the guards and
	// the readers cannot drift.
	productionLegacyRosterIDColumn      = "character_id"
	productionLegacyRosterRetiredColumn = "retired"
)

// The three refusals this file raises itself. Each is the reference's own code
// (legacy-roster-source.ts:49, :51-53, :54-55).
var (
	// productionLegacyRosterIDSchemaRequired is :49, raised before anything is read.
	productionLegacyRosterIDSchemaRequired = errors.New("LEGACY_ROSTER_ID_SCHEMA_REQUIRED")

	// productionLegacyRosterSourceInvalid is :50-53: no project_core row, an
	// identity revision that is not a safe non-negative integer, or a legacy
	// markdown that is not text. The reference folds those three reads into one
	// guard, and so does this file.
	productionLegacyRosterSourceInvalid = errors.New("LEGACY_ROSTER_SOURCE_INVALID")

	// productionLegacyRosterIDInvalid is :55: a character row whose id is not
	// non-blank text. It is checked over every row, retired ones included.
	productionLegacyRosterIDInvalid = errors.New("LEGACY_ROSTER_ID_INVALID")
)

// productionLegacyRosterSource is LegacyRosterSource
// (electron/services/legacy-roster-source.ts:11-21) in the reference's key order.
type productionLegacyRosterSource struct {
	Snapshot         map[string]any `json:"snapshot"`
	RawLegacy        string         `json:"rawLegacy"`
	LegacyHash       string         `json:"legacyHash"`
	IdentityRevision int64          `json:"identityRevision"`
	ActiveIDs        []string       `json:"activeIds"`
	FactsHash        string         `json:"factsHash"`
	// Genre is `any` on purpose: project_core.genre is a nullable column and the
	// reference copies it as it is, so a NULL answers null instead of "".
	Genre           any    `json:"genre"`
	WritingLanguage string `json:"writingLanguage"`
}

// productionLegacyRoster is the borrowed lane. It holds no database: the one
// served handler borrows the project the frozen context names, so it can never
// read a database the admission did not prove.
type productionLegacyRoster struct {
	durable *productionDurableOpen
}

// newProductionLegacyRoster is the constructor production_open_linux.go wires.
func newProductionLegacyRoster(durable *productionDurableOpen) *productionLegacyRoster {
	return &productionLegacyRoster{durable: durable}
}

// Bind installs `legacy-roster:read-source` and `legacy-roster:adopt-existing`.
// A nil or closed bridge installs nothing, so every name of the family keeps
// failing closed at GO_OPERATION_NOT_MIGRATED instead of being published as a
// handler that can only refuse.
//
// The read-source wire arity is the request arity (zero,
// generation-controller.ts:233) plus the frozen context, so exactly one argument
// arrives and it IS the context. The adoption is arity one plus the context, so
// two arguments arrive: the request and the context. The borrow runs after the
// context is parsed, which is the reference's own order: the controller proves
// the project context before it dispatches (generation-controller.ts:184-186).
func (l *productionLegacyRoster) Bind(server *transport.Server) error {
	if l == nil || l.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if err := server.Bind(productionLegacyRosterReadSourceName,
		func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
			if len(args) != 1 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			if _, err := projectsession.ParseContext(args[0]); err != nil {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
			}
			if ctx == nil {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			// read-source answers with the source a run froze, so a database
			// that changed under the run must be refused rather than
			// described: this borrow keeps the strict content-identity check.
			db, release, err := l.durable.borrowAuthorDatabaseStrict(ctx, caller, args[0])
			if err != nil {
				// Already symbolic: a lease, ownership or closed-lane refusal.
				return nil, err
			}
			if db == nil || release == nil {
				return nil, errors.New(productionLegacyRosterRequestFailed)
			}
			defer release()
			answer, err := productionLegacyRosterReadSource(ctx, db)
			if err != nil {
				return nil, productionLegacyRosterFailure(err)
			}
			return answer, nil
		}); err != nil {
		return err
	}
	return server.Bind(productionLegacyRosterAdoptExistingName, l.adoptExisting)
}

// productionLegacyRosterQuerier is the query surface the source proof runs over.
// Both *sql.DB and a reserved *sql.Conn satisfy it, so the same proof serves the
// read-source handler and the adoption's in-transaction source read.
type productionLegacyRosterQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// productionLegacyRosterSourceFacts is the pre-snapshot half of a source proof:
// every CAS field derived from the raw rows, before the roster snapshot is read.
type productionLegacyRosterSourceFacts struct {
	RawLegacy        string
	LegacyHash       string
	IdentityRevision int64
	ActiveIDs        []string
	FactsHash        string
	Genre            any
	WritingLanguage  string
}

// productionLegacyRosterReadSource is readLegacyRosterSource
// (legacy-roster-source.ts:45-61) over an already admitted database. It is split
// out of the handler so the source proof can be exercised without an HTTP round
// trip, and so nothing in it can take a project id, a path or an epoch from a
// request. Its errors are the store's and its own values, unmapped: the handler is
// the one place that turns them into wire codes, so a test can still ask errors.Is
// which refusal it was.
//
// The statement order is the reference's (:48-61) because it decides which code a
// project that fails twice answers with: the source guards and the raw-row facts
// hash come first, and the roster snapshot is read last.
func productionLegacyRosterReadSource(ctx context.Context, db *sql.DB) (*productionLegacyRosterSource, error) {
	if db == nil {
		return nil, store.ErrProjectNotOpen
	}
	facts, err := productionLegacyRosterSourceFactsOn(ctx, db)
	if err != nil {
		return nil, err
	}
	// :59 the snapshot is the last read, exactly as in the reference's object
	// literal: an unreadable roster therefore answers the store's own refusal, not
	// a partial source.
	snapshot, err := store.NewCharacters(db).ReadRoster(ctx)
	if err != nil {
		return nil, err
	}
	return &productionLegacyRosterSource{
		Snapshot:         snapshot,
		RawLegacy:        facts.RawLegacy,
		LegacyHash:       facts.LegacyHash,
		IdentityRevision: facts.IdentityRevision,
		ActiveIDs:        facts.ActiveIDs,
		FactsHash:        facts.FactsHash,
		Genre:            facts.Genre,
		WritingLanguage:  facts.WritingLanguage,
	}, nil
}

// productionLegacyRosterSourceFactsOn is the pre-snapshot half over a caller's
// query surface, so the adoption runs the same guards and the same raw-row facts
// hash on its reserved connection without a second pool read.
func productionLegacyRosterSourceFactsOn(ctx context.Context, q productionLegacyRosterQuerier) (*productionLegacyRosterSourceFacts, error) {
	// :49 hasCharacterIdentitySchema
	present, err := productionLegacyRosterIdentitySchema(ctx, q)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, productionLegacyRosterIDSchemaRequired
	}
	// :50 SELECT genre,writing_language FROM project_core WHERE id='main'
	core, err := productionLegacyRosterSelect(ctx, q, "SELECT genre,writing_language FROM project_core WHERE id='main'")
	if err != nil {
		return nil, err
	}
	// :51-53 the three value reads the reference folds into one guard, in its order.
	identityRevision, err := productionLegacyRosterRevision(ctx, q,
		"SELECT revision FROM character_identity_meta WHERE id='main'")
	if err != nil {
		return nil, err
	}
	rawLegacy, err := productionLegacyRosterLegacyText(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(core) != 1 || identityRevision < 0 || rawLegacy == nil {
		return nil, productionLegacyRosterSourceInvalid
	}
	writingLanguage, _ := core[0].field("writing_language")
	genre, hasGenre := core[0].field("genre")
	if !hasGenre {
		return nil, productionLegacyRosterSourceInvalid
	}
	// :40-42 the actual rows, in the reference's three orders.
	characters, err := productionLegacyRosterSelect(ctx, q,
		"SELECT * FROM characters ORDER BY character_id")
	if err != nil {
		return nil, err
	}
	activeIDs := make([]string, 0, len(characters))
	for _, row := range characters {
		identifier, ok := row.field(productionLegacyRosterIDColumn)
		if !ok {
			return nil, productionLegacyRosterIDInvalid
		}
		text, ok := identifier.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, productionLegacyRosterIDInvalid
		}
		// The reference compares `row.retired === 0`, so the value has to be the
		// integer zero: a NULL, a text "0" or a missing column is not active.
		if retired, ok := row.field(productionLegacyRosterRetiredColumn); ok && retired == int64(0) {
			activeIDs = append(activeIDs, text)
		}
	}
	aliases, err := productionLegacyRosterSelect(ctx, q,
		"SELECT * FROM character_aliases ORDER BY character_id,name,source_key,valid_from")
	if err != nil {
		return nil, err
	}
	relationships, err := productionLegacyRosterSelect(ctx, q,
		"SELECT * FROM character_relationships ORDER BY relationship_id")
	if err != nil {
		return nil, err
	}
	// :56-60 textHash(JSON.stringify({characters, aliases, relationships}))
	facts := productionLegacyRosterFacts{
		Characters:    characters,
		Aliases:       aliases,
		Relationships: relationships,
	}
	factsText, err := productionLegacyRosterJSON(facts)
	if err != nil {
		return nil, err
	}
	return &productionLegacyRosterSourceFacts{
		RawLegacy:        *rawLegacy,
		LegacyHash:       productionLegacyRosterTextHash(*rawLegacy),
		IdentityRevision: identityRevision,
		ActiveIDs:        activeIDs,
		FactsHash:        productionLegacyRosterTextHash(factsText),
		Genre:            genre,
		WritingLanguage:  productionLegacyRosterWritingLanguage(writingLanguage),
	}, nil
}

// productionLegacyRosterIdentitySchema is hasCharacterIdentitySchema
// (electron/repositories/character-repository.ts:8-10): the one table whose
// absence means this project has no identity lane at all.
func productionLegacyRosterIdentitySchema(ctx context.Context, q productionLegacyRosterQuerier) (bool, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", productionLegacyRosterIdentityTable)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		found = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return found, nil
}

// productionLegacyRosterRevision reads one scalar `revision` as the reference's
// `revision(value)` guard does: a safe non-negative integer. An absent row answers
// -1 so the caller can fold it into its own guard, because the reference's
// `.pluck().get()` answers undefined there and `revision(undefined)` is false.
func productionLegacyRosterRevision(ctx context.Context, q productionLegacyRosterQuerier, query string) (int64, error) {
	rows, err := productionLegacyRosterSelect(ctx, q, query)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0].values) != 1 {
		return -1, nil
	}
	// A revision stored as a REAL is still an integer to Number.isSafeInteger, so the
	// integral form is accepted here too and anything else folds into the caller's
	// guard.
	revision := int64(-1)
	switch typed := rows[0].values[0].(type) {
	case int64:
		revision = typed
	case float64:
		if typed == math.Trunc(typed) {
			revision = int64(typed)
		}
	}
	if revision < 0 {
		return -1, nil
	}
	return revision, nil
}

// productionLegacyRosterLegacyText reads the preserved legacy roster, or nil when
// the row or the column is not text. It never trims, normalizes or re-encodes: the
// reference hands the raw string on and hashes exactly those bytes.
func productionLegacyRosterLegacyText(ctx context.Context, q productionLegacyRosterQuerier) (*string, error) {
	rows, err := productionLegacyRosterSelect(ctx, q,
		"SELECT legacy_markdown FROM character_roster_meta WHERE id='main'")
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0].values) != 1 {
		return nil, nil
	}
	text, ok := rows[0].values[0].(string)
	if !ok {
		return nil, nil
	}
	return &text, nil
}

// productionLegacyRosterWritingLanguage is resolveWritingLanguage
// (src/shared/writing-language.ts:7-11).
func productionLegacyRosterWritingLanguage(value any) string {
	if text, ok := value.(string); ok && (text == productionLegacyRosterLanguageZhCN || text == productionLegacyRosterLanguageEnUS) {
		return text
	}
	return productionLegacyRosterLanguageZhCN
}

// productionLegacyRosterTextHash is the reference's textHash: sha256 hex over the
// UTF-8 bytes.
func productionLegacyRosterTextHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// productionLegacyRosterRow is one `SELECT *` row: the columns in the order the
// statement returned them, which is the order better-sqlite3 keys a row object in
// and therefore the order JSON.stringify serializes it in.
type productionLegacyRosterRow struct {
	keys   []string
	values []any
}

// field returns one column's value, and whether the statement returned it at all.
func (r productionLegacyRosterRow) field(key string) (any, bool) {
	for index, name := range r.keys {
		if name == key {
			return r.values[index], true
		}
	}
	return nil, false
}

// productionLegacyRosterSelect runs one statement and returns its rows in the
// driver's own column order.
func productionLegacyRosterSelect(ctx context.Context, q productionLegacyRosterQuerier, query string) ([]productionLegacyRosterRow, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	selected := make([]productionLegacyRosterRow, 0, 8)
	for rows.Next() {
		raw := make([]any, len(columns))
		addresses := make([]any, len(columns))
		for index := range raw {
			addresses[index] = &raw[index]
		}
		if err := rows.Scan(addresses...); err != nil {
			return nil, err
		}
		row := productionLegacyRosterRow{keys: append([]string(nil), columns...), values: make([]any, 0, len(columns))}
		for _, value := range raw {
			normalized, err := productionLegacyRosterValue(value)
			if err != nil {
				return nil, err
			}
			row.values = append(row.values, normalized)
		}
		selected = append(selected, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return selected, nil
}

// productionLegacyRosterValue normalizes one driver value into the value the
// canonical encoder can spell exactly like JSON.stringify. Anything it cannot is refused with the
// store's own unsupported-input value rather than hashed approximately, which is
// the same choice internal/store makes for a row it cannot represent
// (internal/store/character_roster.go:84-102).
func productionLegacyRosterValue(value any) (any, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case bool:
		return typed, nil
	case int64:
		if typed < -productionLegacyRosterMaxSafeInteger || typed > productionLegacyRosterMaxSafeInteger {
			return nil, store.ErrCharacterInputUnsupported
		}
		return typed, nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Abs(typed) > productionLegacyRosterMaxSafeInteger {
			return nil, store.ErrCharacterInputUnsupported
		}
		return typed, nil
	case string:
		if !utf8.ValidString(typed) {
			return nil, store.ErrCharacterInputUnsupported
		}
		return typed, nil
	case []byte:
		return productionLegacyRosterBuffer(typed), nil
	}
	// time.Time and every other driver type have no JSON.stringify spelling this
	// lane can reproduce.
	return nil, store.ErrCharacterInputUnsupported
}

// productionLegacyRosterBuffer is what JSON.stringify makes of a Buffer, which is
// what better-sqlite3 hands back for a BLOB column.
func productionLegacyRosterBuffer(value []byte) map[string]any {
	data := make([]any, 0, len(value))
	for _, octet := range value {
		data = append(data, int64(octet))
	}
	return map[string]any{"type": "Buffer", "data": data}
}

// productionLegacyRosterFacts is the object the facts hash is taken over
// (legacy-roster-source.ts:56-60): the three row sets, retired identities included.
type productionLegacyRosterFacts struct {
	Characters    []productionLegacyRosterRow
	Aliases       []productionLegacyRosterRow
	Relationships []productionLegacyRosterRow
}

// productionLegacyRosterJSON serializes the facts object in JSON.stringify's byte
// shape: the reference's key order (characters, aliases, relationships), each row
// an object keyed in its statement's column order.
func productionLegacyRosterJSON(facts productionLegacyRosterFacts) (string, error) {
	builder := &strings.Builder{}
	builder.WriteString(`{"characters":`)
	if err := productionLegacyRosterJSONRows(builder, facts.Characters); err != nil {
		return "", err
	}
	builder.WriteString(`,"aliases":`)
	if err := productionLegacyRosterJSONRows(builder, facts.Aliases); err != nil {
		return "", err
	}
	builder.WriteString(`,"relationships":`)
	if err := productionLegacyRosterJSONRows(builder, facts.Relationships); err != nil {
		return "", err
	}
	builder.WriteString(`}`)
	return builder.String(), nil
}

// productionLegacyRosterJSONRows serializes one row array.
func productionLegacyRosterJSONRows(builder *strings.Builder, rows []productionLegacyRosterRow) error {
	builder.WriteString("[")
	for index, row := range rows {
		if index > 0 {
			builder.WriteString(",")
		}
		builder.WriteString("{")
		for position, key := range row.keys {
			if position > 0 {
				builder.WriteString(",")
			}
			productionLegacyRosterJSONText(builder, key)
			builder.WriteString(":")
			if err := productionLegacyRosterJSONValue(builder, row.values[position]); err != nil {
				return err
			}
		}
		builder.WriteString("}")
	}
	builder.WriteString("]")
	return nil
}

// productionLegacyRosterJSONValue spells one normalized value.
func productionLegacyRosterJSONValue(builder *strings.Builder, value any) error {
	switch typed := value.(type) {
	case nil:
		builder.WriteString("null")
		return nil
	case bool:
		if typed {
			builder.WriteString("true")
			return nil
		}
		builder.WriteString("false")
		return nil
	case int64:
		builder.WriteString(strconv.FormatInt(typed, 10))
		return nil
	case float64:
		return productionLegacyRosterJSONReal(builder, typed)
	case string:
		productionLegacyRosterJSONText(builder, typed)
		return nil
	case map[string]any:
		// Only a BLOB reaches this arm, in the one shape productionLegacyRosterBuffer
		// builds. The key order is spelled here rather than ranged over, because a map
		// iteration order would change the hash.
		builder.WriteString(`{"type":"Buffer","data":`)
		data, _ := typed["data"].([]any)
		builder.WriteString("[")
		for index, octet := range data {
			if index > 0 {
				builder.WriteString(",")
			}
			octetValue, _ := octet.(int64)
			builder.WriteString(strconv.FormatInt(octetValue, 10))
		}
		builder.WriteString("]}")
		return nil
	}
	return store.ErrCharacterInputUnsupported
}

// productionLegacyRosterJSONReal spells a REAL value the way JSON.stringify does
// where Go and JavaScript agree. Outside that window the two spellings differ (Go
// pads an exponent, JavaScript does not), so the value is refused instead of
// hashed into a digest a desktop build would compute differently.
func productionLegacyRosterJSONReal(builder *strings.Builder, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > productionLegacyRosterMaxSafeInteger {
		return store.ErrCharacterInputUnsupported
	}
	if value == 0 {
		builder.WriteString("0")
		return nil
	}
	magnitude := math.Abs(value)
	if magnitude < 1e-6 || magnitude >= 1e21 {
		return store.ErrCharacterInputUnsupported
	}
	builder.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	return nil
}

// productionLegacyRosterJSONText spells a string with JSON.stringify's escaping:
// quote, reverse solidus and the five short control escapes, other C0 characters
// as \u00XX, and nothing else escaped. U+2028 and U+2029 stay literal code points,
// which is where encoding/json would have diverged.
func productionLegacyRosterJSONText(builder *strings.Builder, value string) {
	const digits = "0123456789abcdef"
	builder.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if character < 0x20 {
				builder.WriteString(`\u00`)
				builder.WriteByte(digits[(character>>4)&0xf])
				builder.WriteByte(digits[character&0xf])
				continue
			}
			builder.WriteRune(character)
		}
	}
	builder.WriteByte('"')
}

// productionLegacyRosterRefusal is one named refusal and the code it answers.
type productionLegacyRosterRefusal struct {
	refusal error
	code    string
}

// productionLegacyRosterRefusals is the one mapping table of this lane. Every
// code is either the reference's own published string or the store value's own
// string, so a wire code and the thing that raised it cannot drift apart.
func productionLegacyRosterRefusals() []productionLegacyRosterRefusal {
	return []productionLegacyRosterRefusal{
		// legacy-roster-source.ts:49, :51-53, :54-55
		{productionLegacyRosterIDSchemaRequired, "LEGACY_ROSTER_ID_SCHEMA_REQUIRED"},
		{productionLegacyRosterSourceInvalid, "LEGACY_ROSTER_SOURCE_INVALID"},
		{productionLegacyRosterIDInvalid, "LEGACY_ROSTER_ID_INVALID"},
		// internal/store: the roster read's own refusals
		// (internal/store/character_roster.go:19-22) and the identity ones it can
		// raise (internal/store/characters.go:14-16).
		{store.ErrCharacterRosterSchemaUnsupported, "CHARACTER_ROSTER_SCHEMA_UNSUPPORTED"},
		{store.ErrCharacterRosterMetaMissing, "CHARACTER_ROSTER_META_MISSING"},
		{store.ErrCharacterIDSchemaRequired, "CHARACTER_ID_SCHEMA_REQUIRED"},
		{store.ErrCharacterIDMetaMissing, "CHARACTER_ID_META_MISSING"},
		{store.ErrCharacterInputUnsupported, "CHARACTER_INPUT_UNSUPPORTED"},
		// store.ErrProjectNotOpen is deliberately absent: it says this lane's
		// database is closed, not that the source is wrong, and its name is outside
		// every family the controller publishes. The fallback below answers the
		// controller's own unmapped-failure code for it, which is what a browser
		// would rewrite it to anyway.
	}
}

// productionLegacyRosterWirePrefixes is the family allowlist of
// generationIpcErrorCode (generation-controller.ts:88-101). A code outside it is
// never published, whatever its shape.
var productionLegacyRosterWirePrefixes = []string{"GENERATION_", "ROOT_BUDGET_",
	"ARTIFACT_", "MAIN_", "CHARACTER_", "FINALIZED_CHARACTER_", "LEGACY_ROSTER_"}

// productionLegacyRosterWireCode reports whether text is a code this family's
// controller would pass through unchanged.
func productionLegacyRosterWireCode(text string) bool {
	if text == "SOURCE_DRAFT_CHANGED" {
		return true
	}
	for _, prefix := range productionLegacyRosterWirePrefixes {
		if !strings.HasPrefix(text, prefix) {
			continue
		}
		rest := text[len(prefix):]
		if rest == "" {
			return false
		}
		for _, character := range rest {
			if character != '_' && (character < 'A' || character > 'Z') {
				return false
			}
		}
		return true
	}
	return false
}

// productionLegacyRosterRefusalCode is the table lookup.
func productionLegacyRosterRefusalCode(err error) string {
	for _, entry := range productionLegacyRosterRefusals() {
		if errors.Is(err, entry.refusal) {
			return entry.code
		}
	}
	return ""
}

// productionLegacyRosterFailure is the one and only mapping layer: a symbolic code
// for a refusal this lane names, the controller's own family allowlist for a code
// a shared helper already spells, and the unmapped-failure code for everything
// else - never a SQLite message, a path or a row of the author's text.
func productionLegacyRosterFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if code := productionLegacyRosterRefusalCode(err); code != "" {
		return errors.New(code)
	}
	if text := err.Error(); productionLegacyRosterWireCode(text) {
		return errors.New(text)
	}
	return errors.New(productionLegacyRosterRequestFailed)
}

// productionLegacyRosterOperations is the whole family in the order
// web/operations.json lists it (:263-269): all seven names are served now.
func productionLegacyRosterOperations() []string {
	return []string{
		productionLegacyRosterReadSourceName,
		productionLegacyRosterAdoptExistingName,
		productionLegacyRosterBeginName,
		productionLegacyRosterReadName,
		productionLegacyRosterExecuteName,
		productionLegacyRosterStageName,
		productionLegacyRosterCancelName,
	}
}

// productionLegacyRosterSeamLine is one line that has to exist before a refused
// name can be served: the file, the line inside it, and a marker that must appear
// on it. TestProductionLaneRefusesLegacyRosterNames re-reads every one of them, so
// a moved or deleted port fails this lane's test instead of leaving a note that
// points at nothing. No name is refused today, so the table is empty and the
// reader below is what the test's "served names are not seam rows" half uses.
type productionLegacyRosterSeamLine struct {
	Path   string
	Line   int
	Marker string
}

// productionLegacyRosterGap is one refused name together with the ports that keep
// it unbound.
type productionLegacyRosterGap struct {
	Operation string
	Missing   []productionLegacyRosterSeamLine
}

// productionLegacyRosterSeams is this file's whole answer for every name it does
// not bind. All seven are served now - stage proves the executed run's artifact
// against the frozen model revision and writes the staged proposal - so the table
// is empty, and TestProductionLaneRefusesLegacyRosterNames asserts both that it is
// empty and that none of the seven names appears in it.
func productionLegacyRosterSeams() []productionLegacyRosterGap {
	return nil
}

// productionLegacyRosterSeam returns the documented seam for one name, or false
// when the name is served here or unknown.
func productionLegacyRosterSeam(operation string) (productionLegacyRosterGap, bool) {
	for _, gap := range productionLegacyRosterSeams() {
		if gap.Operation == operation {
			return gap, true
		}
	}
	return productionLegacyRosterGap{}, false
}
