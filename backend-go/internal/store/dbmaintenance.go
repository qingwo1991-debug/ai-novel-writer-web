package store

// This file is the store port behind the three destructive `db:*` writes the
// production lane could not serve, quoted against the reference:
//
//	electron/repositories/blueprint-repository.ts:806-812
//	  static clearAll(): void { const tx = db.transaction(() => {
//	    clearBlueprintFactsWithinTransaction(db) }); tx() }
//	electron/repositories/blueprint-repository.ts:197-208
//	  clearBlueprintFactsWithinTransaction: retire blueprint recovery, then
//	    DELETE FROM blueprint_character_sync_operations
//	    DELETE FROM blueprint_commit_operations
//	    DELETE FROM blueprints
//	electron/repositories/project-clear-repository.ts:87-166
//	  clearGeneratedData(options): one db.transaction; generatedText deletes
//	    finalized_draft_import_operations, post_process_steps, post_process_runs,
//	    reviews, revisions, drafts, contents, summary_snapshots; blueprints calls
//	    clearBlueprintFactsWithinTransaction; creativeFields retires every roster
//	    entry through commitAuthorCharacterRoster, clears the legacy architecture
//	    and blanks eight project_core text columns. Returns
//	    { cleared: ProjectClearScope[]; physicalFilesDeleted: number }.
//	electron/repositories/import-global-facts-repository.ts:162-230
//	  commit(): the ledger row is the idempotency key; a different payload hash is
//	    IMPORT_GLOBAL_FACTS_OPERATION_CONFLICT, the same payload hash replays as
//	    `{ ...stored, idempotent: true }`, and the identity-schema arm writes the
//	    core, the ledger row and one staged character proposal in one transaction.
//
// # What the reference preserves, and what this file preserves with it
//
// clearGeneratedData never touches project_core, characters, character_aliases,
// character_roster_meta, blueprints (unless that scope is requested) or
// chapter_deletion_operations: only the eight generated-text tables above are
// deleted, and the draft/contents deletes rely on the reference's foreign keys
// (electron/database.ts:30 sets `foreign_keys = ON`) cascading into the
// finalization receipts that belong to those drafts. clearAll deletes only the
// three blueprint tables, so every draft and every body survives it. Nothing here
// reorders those statements: `DELETE FROM contents` after `DELETE FROM drafts` is
// what keeps the drafts.content_id RESTRICT edge satisfiable.
//
// # The one transaction
//
// Each operation opens exactly one BEGIN IMMEDIATE reservation through
// dbMaintenanceImmediate, which is the same hand-written transaction the package
// already trusts (internal/store/reviewrevision.go:41-61): one reserved
// connection, a rollback under a fresh five-second context on every failure path,
// and a commit that only happens after the body returned nil. The package keeps
// that handle unexported, so this file mirrors it instead of exporting a second
// mechanism or wrapping the writes in per-statement transactions. A failure
// anywhere in the body therefore leaves the author file byte-identical.
//
// # What this file deliberately does not do
//
//   - It issues no DDL and no lazy repair. The reference's clear path calls
//     ensureBlueprintCommitSchema (blueprint-repository.ts:176-194) first; that
//     repair is inert for a clear, because the rows it re-inserts are the rows the
//     very next statement deletes, and admission only admits the pinned v7 schema
//     where those tables already exist. A database without them refuses instead
//     of clearing half of it.
//   - It does not port GenerationRunRepository.retireBlueprintRecoveryWithinTransaction
//     (generation-run-repository.ts:613-619). That retirement needs the recovery
//     classifier (readBlueprintRecovery, :546-589) and the portable-freeze proof
//     (:790-796), neither of which this package owns. The consequence is named in
//     the handler's comment: a stale directory run can still offer a recovery after
//     a clear, which can re-create blueprints the author asked to delete but can
//     never delete author content.
//   - It does not move the finalized chapter `.txt` files project-clear-repository.ts:41-53
//     sends to the project trash. The store owns SQL only; the wire answer reports
//     physicalFilesDeleted 0 so no caller can believe files were removed.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrCharacterRosterRevisionStale is the reference's
	// CHARACTER_ROSTER_REVISION_STALE (import-global-facts-repository.ts:190).
	ErrCharacterRosterRevisionStale = errors.New("CHARACTER_ROSTER_REVISION_STALE")

	// ErrClearLegacyRosterUnsupported refuses the
	// `migrationState === 'legacy_cards_preserved'` arm of
	// project-clear-repository.ts:121-126 until the legacy adoption path is ported.
	// Refusing is the only safe answer: adopting nothing and then clearing the
	// legacy architecture would delete cards the author never had persisted.
	ErrClearLegacyRosterUnsupported = errors.New("PROJECT_CLEAR_LEGACY_ROSTER_UNSUPPORTED")

	// The import-global-facts refusals, spelled as the reference spells them
	// (import-global-facts-repository.ts:36-45, :105, :110, :175, :206).
	ErrImportGlobalFactsOperationInvalid   = errors.New("IMPORT_GLOBAL_FACTS_OPERATION_INVALID")
	ErrImportGlobalFactsRequestInvalid     = errors.New("IMPORT_GLOBAL_FACTS_REQUEST_INVALID")
	ErrImportGlobalFactsCharactersInvalid  = errors.New("IMPORT_GLOBAL_FACTS_CHARACTERS_INVALID")
	ErrImportGlobalFactsCoreMissing        = errors.New("IMPORT_GLOBAL_FACTS_PROJECT_CORE_MISSING")
	ErrImportGlobalFactsOperationConflict  = errors.New("IMPORT_GLOBAL_FACTS_OPERATION_CONFLICT")
	ErrImportGlobalFactsReceiptCorrupt     = errors.New("IMPORT_GLOBAL_FACTS_RECEIPT_CORRUPT")
	ErrImportGlobalFactsSuperseded         = errors.New("IMPORT_GLOBAL_FACTS_SUPERSEDED")
	ErrImportGlobalFactsReadBackFailed     = errors.New("IMPORT_GLOBAL_FACTS_READ_BACK_FAILED")

	// ErrImportGlobalFactsRosterArmRequired is this lane's own refusal for a
	// receipt the pre-M02 roster arm wrote (`{...stored, roster: ...}`,
	// import-global-facts-repository.ts:209-228). Replaying it would mean
	// re-committing a roster this lane has no novel_import writer for, so it is
	// refused by name instead of being reported as corrupt.
	ErrImportGlobalFactsRosterArmRequired = errors.New("IMPORT_GLOBAL_FACTS_ROSTER_ARM_REQUIRED")
)

// DatabaseMaintenance borrows one already admitted author database. Construction
// performs no I/O, and no method opens, migrates or switches a project.
type DatabaseMaintenance struct{ db *sql.DB }

func NewDatabaseMaintenance(db *sql.DB) *DatabaseMaintenance { return &DatabaseMaintenance{db: db} }

// dbMaintenanceImmediate is reviewrevision.go:41-61 with the receiver changed: one
// reserved connection, BEGIN IMMEDIATE, a rollback that survives a cancelled
// request, and a commit only after the body returned nil. It is unexported for the
// same reason the review/revision handle is: the store owns its transactions, and
// no caller may hold a write lock it did not close.
func (m *DatabaseMaintenance) dbMaintenanceImmediate(ctx context.Context, fn func(*sql.Conn) error) (retErr error) {
	if m == nil || m.db == nil {
		return ErrProjectNotOpen
	}
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("rollback db maintenance: %w", err))
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// dbMaintenanceExec runs the statements in the order given, on the caller's
// connection and inside the caller's transaction.
func dbMaintenanceExec(ctx context.Context, conn *sql.Conn, statements []string) error {
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// blueprintFactDeletes is clearBlueprintFactsWithinTransaction's three deletes
// (blueprint-repository.ts:205-207), in its order: the child ledger first, because
// blueprint_character_sync_operations references blueprint_commit_operations with
// ON DELETE CASCADE and an explicit delete of the parent first would silently
// count rows the reference still had.
func blueprintFactDeletes() []string {
	return []string{
		"DELETE FROM blueprint_character_sync_operations",
		"DELETE FROM blueprint_commit_operations",
		"DELETE FROM blueprints",
	}
}

// generatedTextDeletes is project-clear-repository.ts:102-109 in its exact order.
// reviews and revisions go before drafts because both reference drafts with
// ON DELETE CASCADE and contents with ON DELETE RESTRICT; drafts go before
// contents for the same RESTRICT edge.
func generatedTextDeletes() []string {
	return []string{
		"DELETE FROM finalized_draft_import_operations",
		"DELETE FROM post_process_steps",
		"DELETE FROM post_process_runs",
		"DELETE FROM reviews",
		"DELETE FROM revisions",
		"DELETE FROM drafts",
		"DELETE FROM contents",
		"DELETE FROM summary_snapshots",
	}
}

// ClearBlueprints ports BlueprintRepository.clearAll (blueprint-repository.ts:806-812):
// one transaction, the three deletes, no return value, and no receipt, so calling
// it twice is the same success because the second call deletes nothing.
func (m *DatabaseMaintenance) ClearBlueprints(ctx context.Context) error {
	if m == nil || m.db == nil {
		return ErrProjectNotOpen
	}
	// A chapter-blueprint-directory run that is still open can re-create the rows this
	// clears: the reference retires the recovery inside the same transaction
	// (blueprint-repository.ts:613-619) and this lane has no port for that classifier.
	// Refusing while a run is live is the honest alternative to clearing rows a live
	// writer would put back.
	if err := m.dbMaintenanceNoRunningGeneration(ctx); err != nil {
		return err
	}
	return m.dbMaintenanceImmediate(ctx, func(conn *sql.Conn) error {
		return dbMaintenanceExec(ctx, conn, blueprintFactDeletes())
	})
}

// ErrGenerationRunInFlight refuses a destructive clear while a generation run is open.
// It is this repository's own code, not the reference's: the reference instead retires
// the run inside the transaction, which needs a classifier this lane does not have.
var ErrGenerationRunInFlight = errors.New("BLUEPRINT_CLEAR_RUN_IN_FLIGHT")

// dbMaintenanceNoRunningGeneration reports whether any generation run is still open.
func (m *DatabaseMaintenance) dbMaintenanceNoRunningGeneration(ctx context.Context) error {
	var found int64
	err := m.db.QueryRowContext(ctx,
		`SELECT 1 FROM generation_runs WHERE status = 'running' LIMIT 1`).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrGenerationRunInFlight
}

// ClearGeneratedOptions is ProjectClearOptions (project-clear-repository.ts:16-20).
type ClearGeneratedOptions struct {
	GeneratedText  bool
	Blueprints     bool
	CreativeFields bool
}

// ClearGeneratedRequest is one clear call: the requested scopes, the frozen author
// session creativeFields needs, and the operation id the roster retirement is
// keyed by. An empty OperationID is replaced with `project-clear:<uuid>`, which is
// the reference's own shape (project-clear-repository.ts:120).
type ClearGeneratedRequest struct {
	Options     ClearGeneratedOptions
	Scope       RosterAuthorScope
	OperationID string
}

// ClearGeneratedResult is ProjectClearResult without the physical file count,
// which this lane always reports as zero because it moves no files.
type ClearGeneratedResult struct{ Cleared []string }

// ClearGeneratedData ports ProjectClearRepository.clearGeneratedData
// (project-clear-repository.ts:87-166). Scopes are applied in the reference's
// order - generatedText, blueprints, creativeFields - and append to `cleared` in
// that same order, so the wire answer is the reference's array and not a set.
func (m *DatabaseMaintenance) ClearGeneratedData(ctx context.Context, request ClearGeneratedRequest) (ClearGeneratedResult, error) {
	if m == nil || m.db == nil {
		return ClearGeneratedResult{}, ErrProjectNotOpen
	}
	cleared := make([]string, 0, 3)
	err := m.dbMaintenanceImmediate(ctx, func(conn *sql.Conn) error {
		if request.Options.GeneratedText {
			if err := dbMaintenanceExec(ctx, conn, generatedTextDeletes()); err != nil {
				return err
			}
			cleared = append(cleared, "generatedText")
		}
		if request.Options.Blueprints {
			if err := dbMaintenanceExec(ctx, conn, blueprintFactDeletes()); err != nil {
				return err
			}
			cleared = append(cleared, "blueprints")
		}
		if request.Options.CreativeFields {
			if err := dbMaintenanceClearCreativeFields(ctx, conn, request); err != nil {
				return err
			}
			cleared = append(cleared, "creativeFields")
		}
		return nil
	})
	if err != nil {
		return ClearGeneratedResult{}, err
	}
	return ClearGeneratedResult{Cleared: cleared}, nil
}

// dbMaintenanceClearCreativeFields is project-clear-repository.ts:118-148: retire
// every roster entry through the author's own retirement path (an empty
// manual_edit), clear the legacy architecture, refresh the projection, then blank
// the eight text columns. The legacy-adoption state refuses before the first
// statement of the arm.
func dbMaintenanceClearCreativeFields(ctx context.Context, conn *sql.Conn, request ClearGeneratedRequest) error {
	if summaryTrim(request.Scope.ProjectID) == "" || summaryTrim(request.Scope.Epoch) == "" {
		return ErrCharacterAuthorScopeRequired
	}
	snapshot, err := charactersRoster(ctx, conn)
	if err != nil {
		return err
	}
	if rosterText(snapshot["migrationState"]) == "legacy_cards_preserved" {
		return ErrClearLegacyRosterUnsupported
	}
	entries, _ := snapshot["entries"].([]map[string]any)
	if len(entries) > 0 {
		operationID := summaryTrim(request.OperationID)
		if operationID == "" {
			generated, uuidErr := dbMaintenanceUUID()
			if uuidErr != nil {
				return uuidErr
			}
			operationID = "project-clear:" + generated
		}
		normalized, err := rosterAuthorNormalize(map[string]any{
			"operationId":              operationID,
			"schemaVersion":            int64(1),
			"intent":                   "manual_edit",
			"expectedRevision":         snapshot["revision"],
			"expectedIdentityRevision": snapshot["identityRevision"],
			"entries":                  []any{},
		})
		if err != nil {
			return err
		}
		requestHash, err := rosterAuthorRequestHash(request.Scope, normalized)
		if err != nil {
			return err
		}
		// The reference passes `() => {}` as the current-holder assertion because the
		// whole clear is already one transaction inside the frozen session.
		if _, err := rosterAuthorCommit(ctx, conn, request.Scope, normalized, requestHash,
			"author-roster-request:"+normalized.operationID, "author-roster-effect:"+normalized.operationID,
			func() error { return nil }); err != nil {
			return err
		}
	}
	// clearLegacyCharacterArchitecture (character-roster-repository.ts:874-878).
	if _, err := conn.ExecContext(ctx, "UPDATE character_roster_meta SET legacy_markdown='' WHERE id='main'"); err != nil {
		return err
	}
	if err := characterIdentityRefreshProjection(ctx, conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `UPDATE project_core SET writing_style = '', reference_works = '',
 global_guidance = '', golden_finger = '', premise = '', worldbuilding = '', synopsis = '',
 character_states = '', updated_at = datetime('now') WHERE id = 'main'`)
	return err
}

func dbMaintenanceUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// ImportGlobalFactsCore is ImportGlobalFactsCore (src/shared/import-global-facts.ts:4-20)
// in its declared key order, which is also the order the reference's
// normalizedRequest keeps and hashRequest hashes.
type ImportGlobalFactsCore struct {
	Genre              string `json:"genre"`
	SubGenre           string `json:"subGenre"`
	TargetAudience     string `json:"targetAudience"`
	TotalChapters      int64  `json:"totalChapters"`
	WordsPerChapter    int64  `json:"wordsPerChapter"`
	PlotStructure      string `json:"plotStructure"`
	NarrativePov       string `json:"narrativePov"`
	GoldenFinger       string `json:"goldenFinger"`
	GlobalGuidance     string `json:"globalGuidance"`
	CoreOutline        string `json:"coreOutline"`
	WorldSetting       string `json:"worldSetting"`
	ProtagonistProfile string `json:"protagonistProfile"`
	Premise            string `json:"premise"`
	Worldbuilding      string `json:"worldbuilding"`
	Synopsis           string `json:"synopsis"`
}

// importGlobalFactsRequest is normalizedRequest's answer
// (import-global-facts-repository.ts:64-69): generationRunHandle is dropped, the
// thirteen text fields are trimmed, and the two integers are re-derived.
type importGlobalFactsRequest struct {
	OperationID            string                `json:"operationId"`
	ExpectedRosterRevision int64                 `json:"expectedRosterRevision"`
	Core                   ImportGlobalFactsCore `json:"core"`
	CharacterEntries       []map[string]any      `json:"characterEntries"`
}

// importGlobalFactsEvidence is CharacterProposalStageEvidence:
// { proposalBatchId, sourceHash } (src/shared/character-proposal.ts:28).
type importGlobalFactsEvidence struct {
	ProposalBatchID string `json:"proposalBatchId"`
	SourceHash      string `json:"sourceHash"`
}

// importGlobalFactsReceipt is the identity-schema arm of ImportGlobalFactsReceipt
// (src/shared/import-global-facts.ts:36-48).
type importGlobalFactsReceipt struct {
	OperationID       string                     `json:"operationId"`
	PayloadHash       string                     `json:"payloadHash"`
	Idempotent        bool                       `json:"idempotent"`
	Core              ImportGlobalFactsCore      `json:"core"`
	CharacterProposal *importGlobalFactsEvidence `json:"characterProposal"`
	ProposalSource    *importGlobalFactsRequest  `json:"proposalSource"`
}

// CommitGlobalFacts ports ImportGlobalFactsRepository.commit
// (import-global-facts-repository.ts:162-230) for the arm this lane's admitted
// schema always takes (`hasCharacterIdentitySchema`, :188). One transaction covers
// the roster-revision staleness check, the project core write, the receipt row and
// the staged character proposal; nothing is written when any of them fails.
func (m *DatabaseMaintenance) CommitGlobalFacts(ctx context.Context, projectID string, candidate map[string]any) (map[string]any, error) {
	if m == nil || m.db == nil {
		return nil, ErrProjectNotOpen
	}
	request, err := importGlobalFactsNormalize(candidate)
	if err != nil {
		return nil, err
	}
	payloadHash, err := importGlobalFactsHashRequest(request)
	if err != nil {
		return nil, err
	}
	batchID, err := importGlobalFactsBatchID(projectID, request.OperationID)
	if err != nil {
		return nil, err
	}
	sourceHash, err := importGlobalFactsSourceHash(payloadHash, request.CharacterEntries)
	if err != nil {
		return nil, err
	}
	var wire map[string]any
	err = m.dbMaintenanceImmediate(ctx, func(conn *sql.Conn) error {
		existing, err := importGlobalFactsExisting(ctx, conn, request.OperationID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.payloadHash != payloadHash {
				return ErrImportGlobalFactsOperationConflict
			}
			stored, err := importGlobalFactsParseReceipt(request.OperationID, *existing)
			if err != nil {
				return err
			}
			if err := importGlobalFactsAssertCurrent(ctx, conn, *stored); err != nil {
				return err
			}
			// :178 `if (stored.characterProposal) return { ...stored, idempotent: true }`
			replay := *stored
			replay.Idempotent = true
			wire, err = importGlobalFactsWire(&replay, request.OperationID)
			return err
		}
		identity, err := summaryHasIdentity(ctx, conn)
		if err != nil {
			return err
		}
		if !identity {
			return ErrCharacterIDSchemaRequired
		}
		// :189-190 the roster revision the renderer read must still be current. A
		// missing meta row is the reference's own `undefined !== number` answer, which
		// is the staleness refusal and not a different code.
		//
		// The reference also re-probes the project manifest here (:191-194). This lane
		// cannot: the only path this store function is given is the borrowed project's,
		// and borrowAuthorDatabase already refused any path the startup inventory did
		// not issue a manifest-backed catalog key for, so that proof happened before
		// this transaction opened.
		var revision int64
		if err := conn.QueryRowContext(ctx, "SELECT revision FROM character_roster_meta WHERE id='main'").Scan(&revision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCharacterRosterRevisionStale
			}
			return err
		}
		if revision != request.ExpectedRosterRevision {
			return ErrCharacterRosterRevisionStale
		}
		// :199 ProjectCoreRepository.update(request.core), then :200 coreSnapshot().
		if err := importGlobalFactsUpdateCore(ctx, conn, request.Core); err != nil {
			return err
		}
		core, err := importGlobalFactsCoreSnapshot(ctx, conn)
		if err != nil {
			return err
		}
		receipt := importGlobalFactsReceipt{OperationID: request.OperationID, PayloadHash: payloadHash,
			Core: *core, CharacterProposal: &importGlobalFactsEvidence{ProposalBatchID: batchID, SourceHash: sourceHash},
			ProposalSource: &request}
		encoded, err := importRunReceiptJSON(receipt)
		if err != nil {
			return err
		}
		// :203 the receipt is written before the proposal is staged, exactly as the
		// reference orders them: stage reads this row back to prove its own source.
		if _, err := conn.ExecContext(ctx, `INSERT INTO import_global_fact_operations(operation_id,payload_hash,receipt_json)
 VALUES(?,?,?)`, request.OperationID, payloadHash, encoded); err != nil {
			return err
		}
		// :204-205 service.stage(source): one proposal row, no roster write.
		if err := importGlobalFactsStageProposal(ctx, conn, projectID, request, batchID, sourceHash); err != nil {
			return err
		}
		// :206 the same evidence service.evidence() answers must equal the receipt's.
		evidence, err := importGlobalFactsEvidenceOf(ctx, conn, request.OperationID, batchID)
		if err != nil {
			return err
		}
		if evidence.SourceHash != sourceHash {
			return ErrImportGlobalFactsReadBackFailed
		}
		wire, err = importGlobalFactsWire(&receipt, request.OperationID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

type importGlobalFactsOperation struct{ payloadHash, receipt string }

func importGlobalFactsExisting(ctx context.Context, conn *sql.Conn, operationID string) (*importGlobalFactsOperation, error) {
	rows, err := rrMaps(ctx, conn, `SELECT operation_id,payload_hash,receipt_json
 FROM import_global_fact_operations WHERE operation_id=?`, operationID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	payloadHash, _ := rows[0]["payload_hash"].(string)
	receipt, _ := rows[0]["receipt_json"].(string)
	if payloadHash == "" || receipt == "" {
		return nil, ErrImportGlobalFactsReceiptCorrupt
	}
	return &importGlobalFactsOperation{payloadHash: payloadHash, receipt: receipt}, nil
}

// importGlobalFactsParseReceipt is parseReceipt (:98-107): the stored envelope must
// name its own operation and payload hash, and the proposal source it carries must
// hash back to that payload hash.
func importGlobalFactsParseReceipt(operationID string, row importGlobalFactsOperation) (*importGlobalFactsReceipt, error) {
	var receipt importGlobalFactsReceipt
	if err := json.Unmarshal([]byte(row.receipt), &receipt); err != nil {
		return nil, ErrImportGlobalFactsReceiptCorrupt
	}
	if receipt.OperationID != operationID || receipt.PayloadHash != row.payloadHash {
		return nil, ErrImportGlobalFactsReceiptCorrupt
	}
	if receipt.CharacterProposal == nil || receipt.ProposalSource == nil {
		// The pre-M02 roster arm writes { roster, proposalSource: undefined } and has
		// no writer here; it is refused by name, never replayed as success.
		return nil, ErrImportGlobalFactsRosterArmRequired
	}
	hash, err := importGlobalFactsHashRequest(*receipt.ProposalSource)
	if err != nil || hash != row.payloadHash {
		return nil, ErrImportGlobalFactsReceiptCorrupt
	}
	return &receipt, nil
}

// importGlobalFactsAssertCurrent is assertCurrentReceipt (:108-116): the saved core
// must still be the live core and the staged proposal must still prove to the saved
// evidence, or the operation is SUPERSEDED.
func importGlobalFactsAssertCurrent(ctx context.Context, conn *sql.Conn, stored importGlobalFactsReceipt) error {
	current, err := importGlobalFactsCoreSnapshot(ctx, conn)
	if err != nil {
		return err
	}
	if *current != stored.Core {
		return ErrImportGlobalFactsSuperseded
	}
	if stored.CharacterProposal == nil {
		return ErrImportGlobalFactsRosterArmRequired
	}
	evidence, err := importGlobalFactsEvidenceOf(ctx, conn, stored.OperationID, stored.CharacterProposal.ProposalBatchID)
	if err != nil {
		return err
	}
	if evidence.SourceHash != stored.CharacterProposal.SourceHash {
		return ErrImportGlobalFactsSuperseded
	}
	return nil
}

// importGlobalFactsEvidenceOf is service.evidence() (character-proposal-service.ts:154-157)
// read back out of the database: the row must carry this project's source key and
// its stored hash must be the hash of its own bytes, and the source hash is
// recomputed from the receipt the ledger actually holds rather than trusted.
func importGlobalFactsEvidenceOf(ctx context.Context, conn *sql.Conn, operationID, batchID string) (importGlobalFactsEvidence, error) {
	rows, err := rrMaps(ctx, conn, `SELECT source_key,source_hash,raw_value
 FROM character_identity_proposals WHERE proposal_id=?`, batchID)
	if err != nil {
		return importGlobalFactsEvidence{}, err
	}
	if len(rows) == 0 {
		return importGlobalFactsEvidence{}, ErrImportGlobalFactsReadBackFailed
	}
	sourceKey, _ := rows[0]["source_key"].(string)
	hash, _ := rows[0]["source_hash"].(string)
	raw, _ := rows[0]["raw_value"].(string)
	if !strings.HasPrefix(sourceKey, "character-proposal-v1:") || hash == "" || rosterHash(raw) != hash {
		return importGlobalFactsEvidence{}, ErrImportGlobalFactsReadBackFailed
	}
	ledger, err := rrMaps(ctx, conn, `SELECT payload_hash,receipt_json
 FROM import_global_fact_operations WHERE operation_id=?`, operationID)
	if err != nil || len(ledger) == 0 {
		return importGlobalFactsEvidence{}, ErrImportGlobalFactsReadBackFailed
	}
	payloadHash, _ := ledger[0]["payload_hash"].(string)
	receipt, _ := ledger[0]["receipt_json"].(string)
	var stored importGlobalFactsReceipt
	if err := json.Unmarshal([]byte(receipt), &stored); err != nil || stored.ProposalSource == nil {
		return importGlobalFactsEvidence{}, ErrImportGlobalFactsReadBackFailed
	}
	proved, err := importGlobalFactsSourceHash(payloadHash, stored.ProposalSource.CharacterEntries)
	if err != nil {
		return importGlobalFactsEvidence{}, err
	}
	return importGlobalFactsEvidence{ProposalBatchID: batchID, SourceHash: proved}, nil
}

// importGlobalFactsStageProposal is CharacterProposalService.stage for kind
// 'import' (character-proposal-service.ts:159-193): one character_identity_proposals
// row holding the proof over the receipt's own entries and the batch resolved
// against the live identity revision. It writes no character, alias, approval or
// roster row, so the author's characters survive the import untouched.
func importGlobalFactsStageProposal(ctx context.Context, conn *sql.Conn, projectID string,
	request importGlobalFactsRequest, batchID, sourceHash string) error {
	revision, aliases, err := importGlobalFactsIdentity(ctx, conn)
	if err != nil {
		return err
	}
	items := make([]any, 0, len(request.CharacterEntries))
	for index, entry := range request.CharacterEntries {
		items = append(items, importGlobalFactsItem(index, request.OperationID, entry, aliases, projectID, revision))
	}
	envelope := map[string]any{
		"version":   int64(1),
		"projectId": projectID,
		"proof": map[string]any{
			"items":      importGlobalFactsProofItems(request.OperationID, request.CharacterEntries),
			"sourceHash": sourceHash,
			"provenance": nil,
		},
		"batch": map[string]any{
			"proposalBatchId": batchID,
			"revision":        revision,
			"status":          "pending-approval",
			"source":          map[string]any{"kind": "import", "operationId": request.OperationID},
			"items":           items,
		},
	}
	encoded, err := importRunReceiptJSON(envelope)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO character_identity_proposals(proposal_id,owner_character_id,source_key,source_hash,raw_value,candidate_ids_json)
 VALUES(?,NULL,?,?,?,?)`, batchID, "character-proposal-v1:"+projectID, rosterHash(encoded), encoded, "[]")
	return err
}

// importGlobalFactsProofItems is the proof half: the item without resolution, in
// the declared CharacterProposalItem order (sourceId, fields, relationships,
// rawValue) with the entry's eleven static fields and nothing else.
func importGlobalFactsProofItems(operationID string, entries []map[string]any) []any {
	items := make([]any, 0, len(entries))
	for index, entry := range entries {
		items = append(items, importGlobalFactsItemBase(index, operationID, entry))
	}
	return items
}

func importGlobalFactsItemBase(index int, operationID string, entry map[string]any) map[string]any {
	fields := make(map[string]any, len(characterproposalStaticFields))
	for _, name := range characterproposalStaticFields {
		if value, present := entry[name]; present {
			fields[name] = value
		}
	}
	relationships := make([]any, 0)
	if list, ok := entry["relationships"].([]any); ok {
		for _, raw := range list {
			relation, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			relationships = append(relationships, map[string]any{
				"targetName": relation["target"],
				"relation":   relation["relation"],
				"raw":        relation,
			})
		}
	}
	return map[string]any{
		"selectionKey":  "import:" + strconv.Itoa(index),
		"sourceId":      operationID + ":" + strconv.Itoa(index),
		"fields":        fields,
		"relationships": relationships,
		"rawValue":      entry,
	}
}

func importGlobalFactsItem(index int, operationID string, entry map[string]any, aliases []map[string]any,
	projectID string, revision int64) map[string]any {
	item := importGlobalFactsItemBase(index, operationID, entry)
	name, _ := entry["name"].(string)
	item["resolution"] = importGlobalFactsResolve(aliases, name, projectID, item["sourceId"].(string), revision)
	return item
}

// importGlobalFactsResolve is resolveScopedCharacterIdentity
// (src/shared/character-identity.ts:116-130) plus the retry resolveItems performs
// for a non-finalized source: a scoped miss is retried without the source key, so a
// proposal can still point at a character this project already knows by name.
func importGlobalFactsResolve(aliases []map[string]any, name, projectID, sourceKey string, revision int64) map[string]any {
	resolution := importGlobalFactsResolveScoped(aliases, name, projectID, sourceKey, revision)
	if resolution["status"] == "unresolved" {
		resolution = importGlobalFactsResolveScoped(aliases, name, projectID, "", revision)
	}
	return resolution
}

func importGlobalFactsResolveScoped(aliases []map[string]any, name, projectID, sourceKey string, revision int64) map[string]any {
	seen := make(map[string]bool, len(aliases))
	candidates := make([]string, 0, 1)
	for _, alias := range aliases {
		if rosterText(alias["name"]) != name {
			continue
		}
		from, ok := rrInteger(alias["validFrom"], 0)
		if !ok || from > revision {
			continue
		}
		if alias["validThrough"] != nil {
			through, ok := rrInteger(alias["validThrough"], 0)
			if !ok || revision > through {
				continue
			}
		}
		if sourceKey != "" && rosterText(alias["sourceKey"]) != sourceKey {
			continue
		}
		id := rosterText(alias["characterId"])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		candidates = append(candidates, id)
	}
	sort.Strings(candidates)
	if len(candidates) > 1 {
		return map[string]any{"status": "ambiguous", "candidateIds": candidates}
	}
	if len(candidates) == 1 && sourceKey != "" {
		return map[string]any{"status": "resolved", "characterId": candidates[0]}
	}
	return map[string]any{"status": "unresolved", "candidateIds": candidates}
}

// importGlobalFactsIdentity is identitySnapshot (character-proposal-service.ts:323-346):
// the identity revision and every live character's aliases in one consistent read
// on the caller's transaction.
func importGlobalFactsIdentity(ctx context.Context, conn *sql.Conn) (int64, []map[string]any, error) {
	var revision int64
	if err := conn.QueryRowContext(ctx, "SELECT revision FROM character_identity_meta WHERE id='main'").Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, ErrCharacterIDSchemaRequired
		}
		return 0, nil, err
	}
	aliases, err := rrMaps(ctx, conn, `SELECT a.character_id AS characterId,a.name,a.source_key AS sourceKey,
 a.valid_from AS validFrom,a.valid_through AS validThrough
 FROM character_aliases a JOIN characters c ON c.character_id=a.character_id
 WHERE c.retired=0 ORDER BY a.character_id,a.name,a.source_key,a.valid_from`)
	if err != nil {
		return 0, nil, err
	}
	return revision, aliases, nil
}

// importGlobalFactsNormalize is normalizedRequest (:34-70).
func importGlobalFactsNormalize(candidate map[string]any) (importGlobalFactsRequest, error) {
	var request importGlobalFactsRequest
	if candidate == nil {
		return request, ErrImportGlobalFactsRequestInvalid
	}
	operationID, ok := candidate["operationId"].(string)
	if !ok {
		return request, ErrImportGlobalFactsOperationInvalid
	}
	operationID = strings.TrimSpace(operationID)
	if operationID == "" || importRunReceiptUTF16Length(operationID) > 160 {
		return request, ErrImportGlobalFactsOperationInvalid
	}
	revision, ok := rrInteger(candidate["expectedRosterRevision"], 0)
	if !ok {
		return request, ErrImportGlobalFactsRequestInvalid
	}
	coreMap, ok := candidate["core"].(map[string]any)
	if !ok || coreMap == nil {
		return request, ErrImportGlobalFactsRequestInvalid
	}
	core, err := importGlobalFactsCore(coreMap)
	if err != nil {
		return request, err
	}
	raw, ok := candidate["characterEntries"].([]any)
	if !ok || len(raw) == 0 {
		return request, ErrImportGlobalFactsCharactersInvalid
	}
	// The HTTP envelope decodes with UseNumber, so a card's numbers arrive as
	// json.Number while the same card parsed back out of the stored receipt is a
	// float64. Both sides of the payload hash must be the same Go shape, so every
	// entry is re-read through this package's JSON.stringify-compatible encoder
	// before it is hashed or stored.
	canonical, err := importGlobalFactsCanonical(raw)
	if err != nil {
		return request, ErrImportGlobalFactsRequestInvalid
	}
	raw, ok = canonical.([]any)
	if !ok || len(raw) == 0 {
		return request, ErrImportGlobalFactsCharactersInvalid
	}
	entries := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok || entry == nil {
			return request, ErrImportGlobalFactsCharactersInvalid
		}
		name, ok := entry["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return request, ErrImportGlobalFactsCharactersInvalid
		}
		if !importGlobalFactsRole(rosterText(entry["role"])) {
			return request, ErrImportGlobalFactsCharactersInvalid
		}
		relations, ok := entry["relationships"].([]any)
		if !ok {
			return request, ErrImportGlobalFactsCharactersInvalid
		}
		for _, element := range relations {
			relation, ok := element.(map[string]any)
			if !ok || relation == nil {
				return request, ErrImportGlobalFactsCharactersInvalid
			}
			target, ok := relation["target"].(string)
			if !ok || strings.TrimSpace(target) == "" {
				return request, ErrImportGlobalFactsCharactersInvalid
			}
			kind, ok := relation["relation"].(string)
			if !ok || strings.TrimSpace(kind) == "" {
				return request, ErrImportGlobalFactsCharactersInvalid
			}
		}
		entries = append(entries, entry)
	}
	return importGlobalFactsRequest{OperationID: operationID, ExpectedRosterRevision: revision,
		Core: core, CharacterEntries: entries}, nil
}

func importGlobalFactsRole(value string) bool {
	switch value {
	case "protagonist", "antagonist", "supporting", "minor":
		return true
	}
	return false
}

// importGlobalFactsCanonical round-trips a decoded JSON value through this
// package's encoder, which is the encoder the payload hash is taken over, so the
// wire's json.Number and a re-parsed receipt's float64 encode identically.
func importGlobalFactsCanonical(value any) (any, error) {
	encoded, err := importRunReceiptJSON(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// importGlobalFactsCore is the thirteen trimmed text fields plus the two integer
// and two vocabulary checks of CORE_TEXT_FIELDS/PLOT_STRUCTURES/NARRATIVE_POVS
// (:25-55).
func importGlobalFactsCore(source map[string]any) (ImportGlobalFactsCore, error) {
	var core ImportGlobalFactsCore
	text := func(key string, target *string) error {
		value, ok := source[key].(string)
		if !ok {
			return ErrImportGlobalFactsRequestInvalid
		}
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return ErrImportGlobalFactsRequestInvalid
		}
		*target = trimmed
		return nil
	}
	for _, field := range []struct {
		key    string
		target *string
	}{
		{"genre", &core.Genre}, {"subGenre", &core.SubGenre}, {"targetAudience", &core.TargetAudience},
		{"goldenFinger", &core.GoldenFinger}, {"globalGuidance", &core.GlobalGuidance},
		{"coreOutline", &core.CoreOutline}, {"worldSetting", &core.WorldSetting},
		{"protagonistProfile", &core.ProtagonistProfile}, {"premise", &core.Premise},
		{"worldbuilding", &core.Worldbuilding}, {"synopsis", &core.Synopsis},
	} {
		if err := text(field.key, field.target); err != nil {
			return core, err
		}
	}
	structure, ok := source["plotStructure"].(string)
	if !ok || !importGlobalFactsPlotStructure(structure) {
		return core, ErrImportGlobalFactsRequestInvalid
	}
	core.PlotStructure = structure
	pov, ok := source["narrativePov"].(string)
	if !ok || !importGlobalFactsNarrativePov(pov) {
		return core, ErrImportGlobalFactsRequestInvalid
	}
	core.NarrativePov = pov
	if core.TotalChapters, ok = rrInteger(source["totalChapters"], 1); !ok {
		return core, ErrImportGlobalFactsRequestInvalid
	}
	if core.WordsPerChapter, ok = rrInteger(source["wordsPerChapter"], 1); !ok {
		return core, ErrImportGlobalFactsRequestInvalid
	}
	return core, nil
}

func importGlobalFactsPlotStructure(value string) bool {
	switch value {
	case "three_act", "heros_journey", "save_the_cat", "kishotenketsu", "multi_thread", "freeform":
		return true
	}
	return false
}

func importGlobalFactsNarrativePov(value string) bool {
	switch value {
	case "third_limited", "first_person", "third_omniscient", "multi_pov":
		return true
	}
	return false
}

func importGlobalFactsHashRequest(request importGlobalFactsRequest) (string, error) {
	encoded, err := importRunReceiptJSON(request)
	if err != nil {
		return "", err
	}
	return rosterHash(encoded), nil
}

func importGlobalFactsSourceHash(payloadHash string, entries []map[string]any) (string, error) {
	encoded, err := importRunReceiptJSON([]any{payloadHash, entries})
	if err != nil {
		return "", err
	}
	return rosterHash(encoded), nil
}

// importGlobalFactsBatchID is `cpb:` plus the hash of the project and the canonical
// source (character-proposal-service.ts:175, import-global-facts-repository.ts:196).
func importGlobalFactsBatchID(projectID, operationID string) (string, error) {
	encoded, err := importRunReceiptJSON([]any{projectID, map[string]any{"kind": "import", "operationId": operationID}})
	if err != nil {
		return "", err
	}
	return "cpb:" + rosterHash(encoded), nil
}

func importGlobalFactsUpdateCore(ctx context.Context, conn *sql.Conn, core ImportGlobalFactsCore) error {
	_, err := conn.ExecContext(ctx, `UPDATE project_core SET genre=?,sub_genre=?,target_audience=?,
 total_chapters=?,words_per_chapter=?,plot_structure=?,narrative_pov=?,golden_finger=?,
 global_guidance=?,core_outline=?,world_setting=?,protagonist_profile=?,premise=?,worldbuilding=?,
 synopsis=?,updated_at=datetime('now') WHERE id='main'`,
		core.Genre, core.SubGenre, core.TargetAudience, core.TotalChapters, core.WordsPerChapter,
		core.PlotStructure, core.NarrativePov, core.GoldenFinger, core.GlobalGuidance, core.CoreOutline,
		core.WorldSetting, core.ProtagonistProfile, core.Premise, core.Worldbuilding, core.Synopsis)
	return err
}

// importGlobalFactsCoreSnapshot is coreSnapshot (:76-96) over the fifteen columns
// the core owns; a missing main row is the reference's own refusal.
func importGlobalFactsCoreSnapshot(ctx context.Context, conn *sql.Conn) (*ImportGlobalFactsCore, error) {
	var core ImportGlobalFactsCore
	err := conn.QueryRowContext(ctx, `SELECT COALESCE(genre,''),COALESCE(sub_genre,''),
 COALESCE(target_audience,''),COALESCE(total_chapters,0),COALESCE(words_per_chapter,0),
 COALESCE(plot_structure,''),COALESCE(narrative_pov,''),COALESCE(golden_finger,''),
 COALESCE(global_guidance,''),COALESCE(core_outline,''),COALESCE(world_setting,''),
 COALESCE(protagonist_profile,''),COALESCE(premise,''),COALESCE(worldbuilding,''),
 COALESCE(synopsis,'') FROM project_core WHERE id='main'`).Scan(
		&core.Genre, &core.SubGenre, &core.TargetAudience, &core.TotalChapters, &core.WordsPerChapter,
		&core.PlotStructure, &core.NarrativePov, &core.GoldenFinger, &core.GlobalGuidance,
		&core.CoreOutline, &core.WorldSetting, &core.ProtagonistProfile, &core.Premise,
		&core.Worldbuilding, &core.Synopsis)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrImportGlobalFactsCoreMissing
	}
	if err != nil {
		return nil, err
	}
	return &core, nil
}

// importGlobalFactsWire is the receipt as the controller returns it: plain maps, so
// no struct field order or pointer ever reaches the public sanitizer.
func importGlobalFactsWire(receipt *importGlobalFactsReceipt, operationID string) (map[string]any, error) {
	core, err := importGlobalFactsCoreMap(receipt.Core)
	if err != nil {
		return nil, err
	}
	evidence := map[string]any{"proposalBatchId": "", "sourceHash": ""}
	if receipt.CharacterProposal != nil {
		evidence = map[string]any{"proposalBatchId": receipt.CharacterProposal.ProposalBatchID,
			"sourceHash": receipt.CharacterProposal.SourceHash}
	}
	source := map[string]any{"operationId": operationID, "expectedRosterRevision": int64(0),
		"core": core, "characterEntries": []any{}}
	if receipt.ProposalSource != nil {
		source = map[string]any{"operationId": receipt.ProposalSource.OperationID,
			"expectedRosterRevision": receipt.ProposalSource.ExpectedRosterRevision,
			"core":                   core, "characterEntries": receipt.ProposalSource.CharacterEntries}
	}
	return map[string]any{
		"operationId":       receipt.OperationID,
		"payloadHash":       receipt.PayloadHash,
		"idempotent":        receipt.Idempotent,
		"core":              core,
		"characterProposal": evidence,
		"proposalSource":    source,
	}, nil
}

func importGlobalFactsCoreMap(core ImportGlobalFactsCore) (map[string]any, error) {
	encoded, err := importRunReceiptJSON(core)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(encoded), &object); err != nil {
		return nil, err
	}
	return object, nil
}

// characterproposalStaticFields is the eleven-field order every identity write
// uses (src/shared/character-roster.ts:79-92): the roster entry's fields without
// currentState and relationships.
var characterproposalStaticFields = []string{"name", "role", "gender", "age", "appearance",
	"personality", "background", "abilities", "motivation", "arc", "notes"}
