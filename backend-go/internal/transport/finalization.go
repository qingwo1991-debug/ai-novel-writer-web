package transport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// FinalizationProvider lends one finalization-outbox repository admitted for
// this exact caller/tab and frozen three-key project context, plus a release
// that holds the project stable through the mutation. It never receives a path
// and never returns a database handle, so the write permit is composed and
// released exactly like the durable draft write's.
//
// The repository it returns MUST carry a trusted owner proof verifier: the
// store refuses every mutation without one (store/finalization_outbox.go:71-82,
// :99-101). This binder supplies the opaque proof; it is a value minted here
// from the validated frozen request, never renderer input.
type FinalizationProvider interface {
	BeginFinalization(ctx context.Context, caller Caller, expected any) (*store.FinalizationOutbox, func(), error)
}

// FinalizationProviderFunc adapts a function to FinalizationProvider.
type FinalizationProviderFunc func(context.Context, Caller, any) (*store.FinalizationOutbox, func(), error)

func (f FinalizationProviderFunc) BeginFinalization(ctx context.Context, caller Caller, expected any) (*store.FinalizationOutbox, func(), error) {
	if f == nil {
		return nil, nil, errors.New("FINALIZATION_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

// FinalizationProof is the owner proof this binder mints for exactly one
// authorized mutation. It is deliberately not reachable from the wire: the
// trusted lane's verifier accepts this type only, so a renderer cannot
// manufacture one, and it refuses any mutation that differs from the values
// frozen here. The store still re-reads the outbox row and re-verifies the
// frozen content hash inside the SAME IMMEDIATE transaction (store/
// finalization_outbox.go:179-190, :228-247, :294-305), so the proof cannot
// substitute for that snapshot check either.
type FinalizationProof struct {
	ProjectID   string
	LeaseID     string
	ProjectPath string
	// Action is the one store mutation this request may perform: commit,
	// knowledge-link or publication-pending. publication-published is never
	// authorized here because this lane owns no publisher receipt.
	Action              string
	DraftID             int64
	FinalizationID      string
	ContentHash         string
	KnowledgeDocumentID string
	LastError           string
}

// finalizationPublisherUnavailable is the one honest publication outcome this
// lane can report: the frozen outbox projection is committed, but no manuscript
// publisher exists here, so no target file is written and the row stays
// pending. The original stores String(error) in last_error and returns the same
// message (electron/services/finalization-service.ts:201-211), and the renderer
// reads committed:true as "the database fact exists, the manuscript file is
// still pending" (:202), so this is the same display state, never a silent
// success.
const finalizationPublisherUnavailable = "Error: FINALIZATION_PUBLISHER_UNAVAILABLE"

// finalizationSpace is ECMAScript's WhiteSpace + LineTerminator set, the set
// String.prototype.trim and the original repository's documentId.trim() use. It
// is the same cutset store.trimJS applies (store/drafts.go:317).
const finalizationSpace = "\u0009\u000a\u000b\u000c\u000d\u0020\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

// finalizationTargetNameParts are the characters Windows forbids in a portable
// direct-child file name (electron/services/manuscript-publisher.ts:5).
const finalizationTargetNameParts = `<>:"/\|?*`

// BindFinalization binds exactly three finalization names to the durable lane's
// finalization-outbox store. The store methods are complete but were reachable
// only from internal/store's own tests; these three binders are the transport
// callers that make them reachable, and nothing else in the family is invented.
//
// Wire arity and the frozen-context index are counted against the declarations,
// because this family does not share one shape:
//
//	finalization:commit                         snapshot at 0, context at 1 (arity 2)
//	finalization:retry                          finalizationId at 0, context at 1 (arity 2)
//	db:finalization-link-knowledge-document      draftId 0, documentId 1, path 2, context 3 (arity 4)
//
// finalization:commit and finalization:retry take no separate path argument
// because the original sends the project session itself as the last argument
// (electron/controllers/finalization-controller.ts:43-105,
// src/services/finalization-client.ts:22-26, :38-42) and the commit snapshot
// carries its own copy of the session that must agree with it
// (finalization-controller.ts:32-38). The link name is a db:* channel, which
// invokeWithProjectSession appends the frozen session to
// (src/services/ipc-client.ts:71-84, :104-116), so it is (draftId, documentId,
// expectedProjectPath, context) exactly like the other db write binders.
//
// Refusal envelopes keep the original's own shapes, including the `Error: `
// prefix: both service errors are caught by the controller's `catch` and
// returned as String(error), which prefixes (finalization-controller.ts:66-68,
// :98-100), while the conflict the service returns itself and the two codes the
// service wraps with toResult are bare (finalization-service.ts:110-116,
// :163-168, :84-88). The link name is a db-controller handler that also returns
// String(err) (:1009-1024), so everything it refuses carries the prefix.
func (s *Server) BindFinalization(provider FinalizationProvider) error {
	if provider == nil {
		return errors.New("FINALIZATION_PROVIDER_REQUIRED")
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"finalization:commit", 2, -1},
		{"finalization:retry", 2, -1},
		{"db:finalization-link-knowledge-document", 4, 2},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			fail := func(code string) (any, error) { return finalizationFailure(name, code), nil }
			if len(args) != arity {
				return fail(finalizationMissingCode(name))
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return fail("PROJECT_SESSION_REQUIRED")
			}
			if pathIndex >= 0 {
				path, ok := args[pathIndex].(string)
				if !ok || path == "" || !utf8.ValidString(path) {
					return fail("PROJECT_CONTEXT_REQUIRED")
				}
				if path != expected["projectPath"] {
					return fail("PROJECT_CONTEXT_CHANGED")
				}
			}
			switch name {
			case "finalization:commit":
				// isFinalizationSnapshot validates the shape first, and
				// snapshotMatchesContext then compares its copied session with
				// the wire context (:18-38). Both run before the borrow, so a
				// malformed snapshot never opens a repository.
				candidate, ok := args[0].(map[string]any)
				if !ok || candidate == nil {
					return fail("FINALIZATION_SNAPSHOT_INVALID")
				}
				snapshot, ok := finalizationSnapshotOf(candidate)
				if !ok {
					return fail("FINALIZATION_SNAPSHOT_INVALID")
				}
				if snapshot.projectPath != expected["projectPath"] || !finalizationContextMatches(snapshot.session, expected) {
					return fail("FINALIZATION_SNAPSHOT_SESSION_MISMATCH")
				}
				if ctx == nil {
					return fail("FINALIZATION_OPERATION_FAILED")
				}
				// The id, the hash and the portable target name are server facts
				// the original mints from the frozen request
				// (finalization-service.ts:104-105, manuscript-publisher.ts:65-105).
				// This lane writes no manuscript file, so there is no directory to
				// scan for a collision and the preferred name is frozen as-is.
				finalizationID, idErr := finalizationRandomID()
				if idErr != nil {
					return fail("FINALIZATION_OPERATION_FAILED")
				}
				input := store.FinalizationCommitInput{
					FinalizationID:  finalizationID,
					DraftID:         snapshot.draftID,
					ChapterNumber:   snapshot.chapterNumber,
					ChapterTitle:    snapshot.chapterTitle,
					Content:         snapshot.content,
					ExpectedContent: snapshot.expectedContent,
					ContentHash:     finalizationContentHash(snapshot.content),
					ContentRevision: snapshot.contentRevision,
					TargetFileName:  finalizationTargetFileName(snapshot.chapterNumber, snapshot.chapterTitle),
				}
				repository, release, err := provider.BeginFinalization(ctx, caller, expected)
				if release != nil {
					defer release()
				}
				if err != nil {
					return fail(finalizationCode(err))
				}
				if repository == nil || release == nil {
					return fail("FINALIZATION_PROVIDER_UNAVAILABLE")
				}
				// The service itself decides the conflict, so the conflict it
				// returns is bare (:110-116) while the same code thrown out of
				// the repository is prefixed. Reading the frozen row first keeps
				// both envelopes exactly as the original produces them.
				existing, err := repository.GetByDraftID(ctx, input.DraftID)
				if err != nil {
					return fail(finalizationCode(err))
				}
				if existing != nil && !finalizationFrozenSame(existing, input) {
					return finalizationBareFailure(name, "FINALIZATION_COMMIT_CONFLICT"), nil
				}
				record := existing
				if record == nil {
					proof, proofOK := finalizationProof(expected, "commit")
					if !proofOK {
						return fail("FINALIZATION_OWNER_REQUIRED")
					}
					proof.DraftID = input.DraftID
					proof.FinalizationID = input.FinalizationID
					proof.ContentHash = input.ContentHash
					committed, commitErr := repository.CommitWithOwnerProof(ctx, input, proof)
					if commitErr != nil {
						return fail(finalizationCode(commitErr))
					}
					record = committed
				}
				return finalizationPublish(ctx, repository, expected, record), nil
			case "finalization:retry":
				// retry carries only the committed id; the prose, the title and
				// the target name are read from the frozen outbox and never
				// re-accepted from the renderer (finalization-service.ts:27-31,
				// :163-168).
				finalizationID, ok := args[0].(string)
				if !ok || finalizationID == "" || !utf8.ValidString(finalizationID) {
					return fail("FINALIZATION_COMMIT_ID_REQUIRED")
				}
				if ctx == nil {
					return fail("FINALIZATION_OPERATION_FAILED")
				}
				repository, release, err := provider.BeginFinalization(ctx, caller, expected)
				if release != nil {
					defer release()
				}
				if err != nil {
					return fail(finalizationCode(err))
				}
				if repository == nil || release == nil {
					return fail("FINALIZATION_PROVIDER_UNAVAILABLE")
				}
				record, err := repository.Get(ctx, finalizationID)
				if err != nil {
					return fail(finalizationCode(err))
				}
				if record == nil {
					return finalizationBareFailure(name, "FINALIZATION_COMMIT_NOT_FOUND"), nil
				}
				return finalizationPublish(ctx, repository, expected, record), nil
			default:
				// db:finalization-link-knowledge-document records a real managed
				// knowledge-document identity on one frozen outbox row
				// (electron/controllers/db-controller.ts:1009-1024). The store
				// requires the injected owner to verify that receipt; this lane
				// owns no knowledge-base document store, so the proof binds the
				// exact finalization identity, frozen content hash and document
				// id the request carried and the store re-verifies the snapshot
				// itself. No document identity is minted or inferred here.
				draftID, ok := draftSafeInteger(args[0], 1)
				if !ok {
					return fail("FINALIZATION_INPUT_INVALID")
				}
				documentID, ok := args[1].(string)
				if !ok {
					return fail("FINALIZATION_INPUT_INVALID")
				}
				if ctx == nil {
					return fail("FINALIZATION_OPERATION_FAILED")
				}
				// The original repository trims the identity before storing it
				// (electron/repositories/finalization-repository.ts:420-424), so
				// the trimmed value is what the proof freezes.
				documentID = finalizationTrim(documentID)
				if documentID == "" {
					return fail("FINALIZATION_COMMIT_KNOWLEDGE_DOCUMENT_REQUIRED")
				}
				repository, release, err := provider.BeginFinalization(ctx, caller, expected)
				if release != nil {
					defer release()
				}
				if err != nil {
					return fail(finalizationCode(err))
				}
				if repository == nil || release == nil {
					return fail("FINALIZATION_PROVIDER_UNAVAILABLE")
				}
				existing, err := repository.GetByDraftID(ctx, draftID)
				if err != nil {
					return fail(finalizationCode(err))
				}
				if existing == nil {
					return fail("FINALIZATION_COMMIT_NOT_FOUND")
				}
				proof, proofOK := finalizationProof(expected, "knowledge-link")
				if !proofOK {
					return fail("FINALIZATION_OWNER_REQUIRED")
				}
				proof.DraftID = draftID
				proof.FinalizationID = existing.FinalizationID
				proof.ContentHash = existing.ContentHash
				proof.KnowledgeDocumentID = documentID
				linked, linkErr := repository.LinkKnowledgeDocumentWithOwnerProof(ctx, draftID, documentID, proof)
				if linkErr != nil {
					return fail(finalizationCode(linkErr))
				}
				return map[string]any{"success": true, "finalization": finalizationRecordWire(linked)}, nil
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, operation := range operations {
		if _, ok := s.registry.Lookup(operation.name); !ok {
			return errors.New("OPERATION_NOT_EXPOSED")
		}
		if _, ok := s.handlers[operation.name]; ok {
			return errors.New("OPERATION_ALREADY_BOUND")
		}
	}
	for name, handler := range handlers {
		s.handlers[name] = handler
	}
	return nil
}

// finalizationPublish mirrors publishCommitted (finalization-service.ts:
// 84-88, :174-212) with the one difference this lane must be honest about: it
// owns no manuscript publisher, so it can never reach markPublished. The
// integrity check, the already-published branch and the pending projection are
// the original's own order; only the failing publisher is replaced by the
// store's own trusted-outcome method (store/finalization_outbox.go:283-289).
// store.isAuthoritative has no exported equivalent, so supersession is not
// evaluated here; nothing is published either way, so no manuscript file is
// ever written for a superseded row. The original also asserts a portable
// runtime freeze before it touches a target (:178-184); this lane resolves no
// target at all, so that guard has nothing to protect.
func finalizationPublish(ctx context.Context, repository *store.FinalizationOutbox, expected map[string]any, record *store.FinalizationRecord) map[string]any {
	if record == nil {
		return map[string]any{"success": false, "committed": false, "error": "Error: FINALIZATION_OPERATION_FAILED"}
	}
	if finalizationContentHash(record.ContentSnapshot) != record.ContentHash {
		return finalizationResult(record, false, "FINALIZATION_SNAPSHOT_HASH_MISMATCH")
	}
	if record.PublicationStatus == "published" {
		return finalizationResult(record, true, "")
	}
	proof, ok := finalizationProof(expected, "publication-pending")
	if !ok {
		return finalizationResult(record, false, "Error: FINALIZATION_OWNER_REQUIRED")
	}
	proof.FinalizationID = record.FinalizationID
	proof.ContentHash = record.ContentHash
	proof.LastError = finalizationPublisherUnavailable
	pending, err := repository.MarkPublicationPendingWithOwnerProof(ctx, record.FinalizationID, finalizationPublisherUnavailable, proof)
	if err != nil {
		// The original logs a failed projection write and still reports the
		// publication message against the committed record (:207-211): the
		// database fact is committed either way.
		return finalizationResult(record, false, finalizationPublisherUnavailable)
	}
	return finalizationResult(pending, false, finalizationPublisherUnavailable)
}

// finalizationResult is toResult (finalization-service.ts:54-65): committed is
// true for every record the store returned, and the error key is omitted when
// there is no failure.
func finalizationResult(record *store.FinalizationRecord, success bool, failure string) map[string]any {
	result := map[string]any{"success": success, "committed": true}
	if record != nil {
		result["finalizationId"] = record.FinalizationID
		result["contentHash"] = record.ContentHash
		result["contentRevision"] = record.ContentRevision
		result["draftId"] = record.DraftID
		result["publicationStatus"] = record.PublicationStatus
	}
	if failure != "" {
		result["error"] = failure
	}
	return result
}

// finalizationFailure is the refusal envelope of the two service names: the
// controller's catch prefixes String(error), and committed:false is the
// explicit statement that no database transaction committed
// (finalization-service.ts:35-43).
func finalizationFailure(name, code string) map[string]any {
	if name == "db:finalization-link-knowledge-document" {
		return map[string]any{"success": false, "error": "Error: " + code}
	}
	return map[string]any{"success": false, "committed": false, "error": "Error: " + code}
}

// finalizationBareFailure is the refusal the original returns as a plain object
// literal, not as a thrown Error, so it carries no prefix: the service's own
// conflict (finalization-service.ts:110-116), the missing id (:163-168) and the
// frozen-snapshot mismatch (:84-88).
func finalizationBareFailure(name, code string) map[string]any {
	if name == "db:finalization-link-knowledge-document" {
		return map[string]any{"success": false, "error": code}
	}
	return map[string]any{"success": false, "committed": false, "error": code}
}

// finalizationMissingCode names what a too-short request is missing: the two
// service names refuse a request without the frozen session
// (finalization-controller.ts:55-57, :86-88) and the db name refuses one
// without its required project path (electron/utils/project-context.ts:22-30).
func finalizationMissingCode(name string) string {
	if name == "db:finalization-link-knowledge-document" {
		return "PROJECT_CONTEXT_REQUIRED"
	}
	return "PROJECT_SESSION_REQUIRED"
}

// finalizationCode is this family's finite vocabulary. The store's sentinel
// errors are already the original's codes; everything else degrades to one
// symbolic failure so a wrapped SQLite message, a path or a driver error never
// reaches the browser.
func finalizationCode(err error) string {
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil && public.Code != "" {
		return public.Code
	}
	for _, known := range []error{
		store.ErrFinalizationOwnerRequired,
		store.ErrFinalizationInputInvalid,
		store.ErrFinalizationCommitConflict,
		store.ErrFinalizationChapterMismatch,
		store.ErrFinalizationOutboxMissing,
		store.ErrFinalizationSuperseded,
		store.ErrFinalizationNotFound,
		store.ErrFinalizationSnapshotHashMismatch,
		store.ErrFinalizationSchemaUnsupported,
		store.ErrFinalizationKnowledgeDocumentRequired,
		store.ErrDraftNotFound,
		store.ErrReviewRevisionProjectNotOpen,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "FINALIZATION_OPERATION_FAILED"
}

// finalizationProof freezes the captured project/epoch every mutation of this
// request must belong to.
func finalizationProof(expected map[string]any, action string) (FinalizationProof, bool) {
	projectID, projectOK := expected["projectId"].(string)
	leaseID, leaseOK := expected["leaseId"].(string)
	projectPath, pathOK := expected["projectPath"].(string)
	if !projectOK || !leaseOK || !pathOK || projectID == "" || leaseID == "" || projectPath == "" {
		return FinalizationProof{}, false
	}
	return FinalizationProof{ProjectID: projectID, LeaseID: leaseID, ProjectPath: projectPath, Action: action}, true
}

// finalizationSnapshot is the frozen commit request the renderer captured
// (src/services/finalization-snapshot.ts:15-26). expectedContent stays nil when
// the renderer did not freeze a baseline, which the store reports as a conflict
// instead of a blind write.
type finalizationSnapshot struct {
	projectPath     string
	session         map[string]any
	draftID         int64
	chapterNumber   int64
	chapterTitle    string
	content         string
	expectedContent *string
	contentRevision int64
}

// finalizationSnapshotOf is isFinalizationSnapshot (finalization-controller.ts:
// 13-30): every field is type-checked, the session is the same three-key context
// and no value is coerced.
func finalizationSnapshotOf(candidate map[string]any) (finalizationSnapshot, bool) {
	if _, ok := candidate["tabId"].(string); !ok {
		return finalizationSnapshot{}, false
	}
	projectPath, projectOK := candidate["projectPath"].(string)
	chapterTitle, titleOK := candidate["chapterTitle"].(string)
	content, contentOK := candidate["content"].(string)
	draftID, draftOK := draftSafeInteger(candidate["draftId"], 0)
	chapterNumber, chapterOK := draftSafeInteger(candidate["chapterNumber"], 0)
	revision, revisionOK := draftSafeInteger(candidate["contentRevision"], 0)
	session, err := draftExpectedContext([]any{candidate["projectSession"]}, 0)
	if !projectOK || !titleOK || !contentOK || !draftOK || !chapterOK || !revisionOK || err != nil {
		return finalizationSnapshot{}, false
	}
	snapshot := finalizationSnapshot{
		projectPath:     projectPath,
		session:         session,
		draftID:         draftID,
		chapterNumber:   chapterNumber,
		chapterTitle:    chapterTitle,
		content:         content,
		contentRevision: revision,
	}
	if baseline, ok := candidate["expectedContent"].(string); ok {
		snapshot.expectedContent = &baseline
	}
	return snapshot, true
}

// finalizationContextMatches is snapshotMatchesContext (finalization-controller.ts:
// 32-38): the snapshot's own copy of the session must be the wire session.
func finalizationContextMatches(snapshot, expected map[string]any) bool {
	return snapshot["projectId"] == expected["projectId"] &&
		snapshot["leaseId"] == expected["leaseId"] &&
		snapshot["projectPath"] == expected["projectPath"]
}

// finalizationFrozenSame is hasSameFrozenRequest (finalization-service.ts:71-82):
// a replay of the same frozen request returns the original commit, and a
// different one for the same draft is the service's conflict.
func finalizationFrozenSame(existing *store.FinalizationRecord, input store.FinalizationCommitInput) bool {
	return existing != nil &&
		existing.DraftID == input.DraftID &&
		existing.ChapterNumber == input.ChapterNumber &&
		existing.ChapterTitle == input.ChapterTitle &&
		existing.ContentRevision == input.ContentRevision &&
		existing.ContentHash == input.ContentHash &&
		existing.ContentSnapshot == input.Content
}

// finalizationContentHash is snapshotHash (finalization-service.ts:67-69): the
// frozen prose hash over the author's exact unnormalized UTF-8 bytes.
func finalizationContentHash(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

// finalizationRandomID mints the server-side finalization identity. The original
// mints randomUUID() per request (:104), and the store deliberately ignores a
// replacement id when the frozen request replays (:213-221), so a replay keeps
// its original identity.
func finalizationRandomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
}

// finalizationTrim is String.prototype.trim over the frozen cutset above.
func finalizationTrim(value string) string {
	return strings.Trim(value, finalizationSpace)
}

// finalizationTargetFileName is manuscriptFileName
// (electron/services/manuscript-publisher.ts:52-55): the portable direct-child
// name the outbox freezes, so retry never accepts a renderer path.
func finalizationTargetFileName(chapterNumber int64, chapterTitle string) string {
	title := finalizationFileNamePart(chapterTitle)
	if title == "" {
		return fmt.Sprintf("第%d章.txt", chapterNumber)
	}
	return fmt.Sprintf("第%d章 %s.txt", chapterNumber, title)
}

// finalizationFileNamePart is sanitizeWindowsFileNamePart
// (manuscript-publisher.ts:30-43) in the same order: illegal characters first,
// then control characters, then the trim, then trailing dots and spaces, then
// the reserved-device suffix. The store re-validates the result as a portable
// direct-child name (store/finalization_outbox.go:236-241).
func finalizationFileNamePart(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character <= 0x1f || strings.ContainsRune(finalizationTargetNameParts, character) {
			builder.WriteByte('_')
			continue
		}
		builder.WriteRune(character)
	}
	part := strings.TrimRight(strings.Trim(builder.String(), finalizationSpace), ". ")
	if part == "" {
		return ""
	}
	if finalizationReservedName(part) {
		return part + "_"
	}
	return part
}

// finalizationReservedName is WINDOWS_RESERVED_FILE_NAME
// (manuscript-publisher.ts:6, :39-41) applied to the base name before the first
// dot.
func finalizationReservedName(part string) bool {
	base := strings.ToLower(part)
	if offset := strings.IndexByte(base, '.'); offset >= 0 {
		base = base[:offset]
	}
	switch base {
	case "con", "prn", "aux", "nul",
		"com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9",
		"lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
		return true
	}
	return false
}

// finalizationRecordWire is the FinalizationRecord the link name returns
// (electron/repositories/finalization-repository.ts:18-34, :96-111). publishedAt
// stays an explicit null when the row has none.
func finalizationRecordWire(record *store.FinalizationRecord) map[string]any {
	if record == nil {
		return nil
	}
	wire := map[string]any{
		"finalizationId":      record.FinalizationID,
		"draftId":             record.DraftID,
		"chapterNumber":       record.ChapterNumber,
		"chapterTitle":        record.ChapterTitle,
		"contentSnapshot":     record.ContentSnapshot,
		"contentHash":         record.ContentHash,
		"contentRevision":     record.ContentRevision,
		"targetFileName":      record.TargetFileName,
		"knowledgeDocumentId": record.KnowledgeDocumentID,
		"publicationStatus":   record.PublicationStatus,
		"lastError":           record.LastError,
		"publishedAt":         nil,
	}
	if record.PublishedAt != nil {
		wire["publishedAt"] = *record.PublishedAt
	}
	return wire
}
