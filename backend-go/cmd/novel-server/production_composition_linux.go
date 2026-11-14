//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/composition"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's half of the visible-composition family: the
// two names that turn committed candidates into the text the editor shows and
// edits back from.
//
//	generation:compose-visible           electron/controllers/generation-controller.ts:270
//	generation:read-visible-composition  electron/controllers/generation-controller.ts:282
//
// Neither had a Go implementation; both were inventoried in web/operations.json
// and left at the fail-closed 501.
//
// # The wire, verified against the controller
//
// The controller pops the frozen project session and then compares the remaining
// argument count with the operation's arity, with one exception
// (generation-controller.ts:184): generation:compose-visible also accepts three
// arguments, because its fourth - the algorithm - is optional. The wire therefore
// carries [handle, ids, hash, algorithm?, sessionContext] - four or five elements
// - and read-visible-composition carries [handle, sessionContext]. Anything else
// is refused, and a request whose trailing context is not the exact three-key map
// is refused with the family's session code, exactly like the rest of this lane.
//
// The answers are the receipt itself and null, not the mutating business DTO: the
// renderer reads receipt.text / receipt.textHash straight off the resolved value
// (src/services/workflows/commands/base-command.ts:363,
// architecture.command.ts:1415), so a composition failure has to reject with a
// symbolic code rather than resolve to {success:false}. That is why this file
// installs the family's read envelope (internal/transport/generation.go:304-315)
// even though compose-visible writes a ledger row.
//
// # The write goes through the ledger, never around it
//
// compose-visible persists the receipt into the attempt's usage receipt as
// $.visibleComposition (generation-run-repository.ts:691-693) and read answers it
// from there (:659-674). The write here is the ledger's recordEffect method, the
// one root-mutation path this lane already uses: it re-admits the root through
// mutableRoot (internal/store/generation_ledger.go:494-575), refuses a key that no
// operation owns, refuses a portable frozen row, and applies the patch inside the
// same IMMEDIATE transaction. No SQL statement in this file ever writes.
//
// # What is read, and why it is read here
//
// The projection is the repository's own receiptSelect
// (generation-run-repository.ts:85-90) scoped to the run's root, exactly like
// readState (:378-397): attempt rows in rowid order, left-joined to their artifact
// row, plus the newest stored composition of the run (:661). Every decision -
// each refusal, the composition and its hash - belongs to the pure package
// internal/composition, which performs no I/O at all; this file only resolves rows
// into that package's structs and hands the receipt back.
type productionComposition struct {
	owner *productionGeneration
}

// productionCompositionUnmatchable stands in for a third wire argument that was
// present but not a string: no SHA-256 hex digest can equal it, which is the same
// verdict the original's identity comparison reaches.
const productionCompositionUnmatchable = "\x00"

// productionCompositionRowQuery is receiptSelect scoped the way readState scopes
// it: every attempt under the run's root, in ledger order, with its artifact row.
const productionCompositionRowQuery = `SELECT t.attempt_id,t.run_id,t.rowid,t.attempt_json,t.usage_receipt_json,
 a.artifact_id,a.run_id,a.artifact_json,a.revision,a.status
 FROM generation_attempts t LEFT JOIN generation_artifacts a ON a.attempt_id=t.attempt_id
 WHERE t.root_action_id=? ORDER BY t.rowid`

// productionCompositionIdentity is usage.artifactIdentity.
type productionCompositionIdentity struct {
	// ArtifactID is usage.artifactIdentity.artifactId.
	ArtifactID string `json:"artifactId"`

	// Epoch is usage.artifactIdentity.epoch.
	Epoch string `json:"epoch"`

	// Fingerprint is usage.artifactIdentity.fingerprint, kept raw so it can be
	// canonically re-encoded before it is compared.
	Fingerprint json.RawMessage `json:"fingerprint"`
}

// productionCompositionResult is the part of usage.result the composition reads.
type productionCompositionResult struct {
	FinishReason string `json:"finishReason"`
}

// productionCompositionUsage is the usage-receipt projection visibleComposition
// reads: the purpose, the artifact identity, the finish reason, and the receipt
// this run already stored.
type productionCompositionUsage struct {
	// Purpose is usage.purpose, "" when absent.
	Purpose string `json:"purpose"`

	// ArtifactIdentity is usage.artifactIdentity, nil when absent.
	ArtifactIdentity *productionCompositionIdentity `json:"artifactIdentity"`

	// Result is usage.result, nil when absent.
	Result *productionCompositionResult `json:"result"`

	// VisibleComposition is the receipt this run already stores, kept raw so an
	// absent value and a null value stay distinguishable.
	VisibleComposition json.RawMessage `json:"visibleComposition"`
}

// productionCompositionArtifact is the persisted candidate payload
// (VisibleArtifact), the half of the row hydrateReceipt verifies.
type productionCompositionArtifact struct {
	// ArtifactID is the payload's own artifactId.
	ArtifactID string `json:"artifactId"`

	// AttemptID is the payload's attemptId.
	AttemptID string `json:"attemptId"`

	// RootActionID is the payload's rootActionId.
	RootActionID string `json:"rootActionId"`

	// ProjectID is the payload's projectId.
	ProjectID string `json:"projectId"`

	// Epoch is the payload's epoch.
	Epoch string `json:"epoch"`

	// Fingerprint is the payload's fingerprint, kept raw so it can be canonically
	// re-encoded before it is compared.
	Fingerprint json.RawMessage `json:"fingerprint"`

	// Revision is the payload's revision, which the row's column must still equal.
	Revision int64 `json:"revision"`

	// Text is the candidate prose.
	Text string `json:"text"`

	// TextHash is the hash the payload claims for Text.
	TextHash string `json:"textHash"`
}

// productionCompositionRows is the read projection the pure package consumes: the
// root-scoped artifact map, the run's newest stored receipt, the root status that
// freezes a composition, and the attempt that owns each artifact - the row the
// receipt is written onto.
type productionCompositionRows struct {
	// lookup is the root-scoped artifact map, keyed by artifact id.
	lookup map[string]composition.Artifact

	// stored is the run's newest stored receipt, nil when it stores none.
	stored *composition.Stored

	// rootStatus is generation_roots action_json's status.
	rootStatus string

	// attemptOf resolves an artifact id to the attempt its receipt is written on.
	attemptOf map[string]string
}

// newProductionComposition wraps the one generation owner this lane built, so both
// names share generation:begin's registry, model leases and ledger admission
// instead of constructing a second owner. It never fails: a nil owner installs
// nothing, and both names keep answering the fail-closed 501 instead of being
// installed as handlers that could only refuse.
func newProductionComposition(owner *productionGeneration) *productionComposition {
	return &productionComposition{owner: owner}
}

// Bind installs the two composition names. It is a plain server.Bind per name, not
// BindGeneration: that binder installs a whole family atomically and would collide
// with the generation:begin, generation:execute and lifecycle names
// production_open_linux.go already binds.
func (c *productionComposition) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if c == nil || c.owner == nil || c.owner.inner == nil {
		return nil
	}
	if err := server.Bind("generation:compose-visible", productionVisibleOperation([]int{3, 4}, c.composeVisible)); err != nil {
		return err
	}
	return server.Bind("generation:read-visible-composition", productionVisibleOperation([]int{1}, c.readVisibleComposition))
}

// productionVisibleOperation is the family adapter (internal/transport/
// generation.go:260-306) with this family's one optional argument: the wire arity
// is any accepted request arity plus the frozen expected context, and every
// failure leaves as a symbolic code instead of a business DTO.
func productionVisibleOperation(arities []int, call func(context.Context, transport.Caller, map[string]any, []any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		arity, matched := 0, false
		for _, candidate := range arities {
			if len(args) == candidate+1 {
				arity, matched = candidate, true
				break
			}
		}
		if !matched {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		raw, sameShape := args[arity].(map[string]any)
		if !sameShape || len(raw) != 3 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		expected := make(map[string]any, 3)
		for _, key := range []string{"projectId", "leaseId", "projectPath"} {
			text, ok := raw[key].(string)
			if !ok || text == "" || !utf8.ValidString(text) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
			}
			expected[key] = text
		}
		if ctx == nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		value, err := call(ctx, caller, expected, args[:arity])
		if err != nil {
			return nil, productionVisibleFailure(err)
		}
		return value, nil
	}
}

// productionVisibleFailure keeps a symbolic code and never a message, a path or a
// ledger detail: an HTTPError travels as itself, anything else becomes its
// all-caps code or REQUEST_FAILED.
func productionVisibleFailure(err error) error {
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return security.NewHTTPError(public.Status, public.Code)
	}
	return security.NewHTTPError(http.StatusInternalServerError, productionCompositionCode(err))
}

// productionCompositionCode is internal/transport/generation.go:317's
// generationSymbolicCode, which is unexported: one all-caps symbolic code, or
// REQUEST_FAILED for anything else, so no SQLite diagnostic, path or provider
// message can reach the browser.
func productionCompositionCode(err error) string {
	if err == nil {
		return "REQUEST_FAILED"
	}
	code := err.Error()
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		code = public.Code
	}
	if len(code) < 2 {
		return "REQUEST_FAILED"
	}
	for index, c := range code {
		if !(c >= 'A' && c <= 'Z') && !(index > 0 && (c >= '0' && c <= '9' || c == '_')) {
			return "REQUEST_FAILED"
		}
	}
	return code
}

// composeVisible answers generation:compose-visible: it validates the four or three
// wire arguments, resolves the run and its ledger rows under the live lease, lets
// the pure package decide, then stores the receipt through the ledger.
func (c *productionComposition) composeVisible(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
	handle, err := syntheticParseHandle(args[0])
	if err != nil {
		return nil, err
	}
	request := composition.Request{}
	if ids, wellFormed := productionCompositionIDs(args[1]); wellFormed {
		request.ArtifactIDs = ids
	} else {
		request.IDsMalformed = true
	}
	request.ExpectedHash = productionCompositionExpectedHash(args[2])
	if len(args) > 3 {
		algorithm, isText := args[3].(string)
		if !isText {
			request.AlgorithmMalformed = true
		} else {
			request.Algorithm = &algorithm
		}
	}
	return c.compose(ctx, caller, expected, handle, request, true)
}

// readVisibleComposition answers generation:read-visible-composition: the stored
// receipt, re-composed and re-hashed from the ledger, or null when the run stores
// none. Nothing is written on this path.
func (c *productionComposition) readVisibleComposition(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
	handle, err := syntheticParseHandle(args[0])
	if err != nil {
		return nil, err
	}
	return c.compose(ctx, caller, expected, handle, composition.Request{}, false)
}

// compose is the shared body of the two names. It re-admits the frozen session and
// the live writer lease first - a stale, foreign or absent lease is refused before
// a row is read - resolves the handle against the committed run, reads the ledger,
// and then either composes and stores or only verifies and re-reads.
func (c *productionComposition) compose(ctx context.Context, caller transport.Caller, expected map[string]any, handle syntheticHandle, request composition.Request, write bool) (any, error) {
	_, scope, ledger, db, release, err := c.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer release()
	_, binding, err := productionCompositionRun(ctx, ledger, scope.Project, handle)
	if err != nil {
		return nil, err
	}
	manifest := syntheticMap(binding["sourceManifest"])
	rows, err := productionCompositionReadRows(ctx, db, binding, handle.RunID, handle.RootActionID)
	if err != nil {
		return nil, err
	}
	request.Lookup = rows.lookup
	request.Stored = rows.stored
	request.RunID = handle.RunID
	request.RootActionID = handle.RootActionID
	request.Operation = syntheticString(manifest["operation"])
	request.OutputContract = productionCompositionContract(manifest)
	request.RootStatus = rows.rootStatus
	request.PlotOutline = composition.PlotOutlinePolicy(manifest)
	// requireRun(handle, true) refuses a compose whose frozen epoch is no longer
	// the live lease (main-generation-owner.ts:164). A read has no such gate: a
	// committed run outlives the lease it was frozen under, which is exactly why
	// the handle carries its own epoch.
	request.EpochStale = write && handle.Epoch != scope.Epoch
	var receipt *composition.Receipt
	if write {
		receipt, err = composition.Compose(request)
	} else {
		receipt, err = composition.Read(request)
	}
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, nil
	}
	if write {
		last := receipt.ArtifactIDs[len(receipt.ArtifactIDs)-1]
		if err := productionCompositionStore(ctx, ledger, handle.RunID, rows.attemptOf[last], receipt); err != nil {
			return nil, err
		}
	}
	wire := productionCompositionWire(receipt)
	if !write {
		// A read answers the receipt plus the run's frozen author inputs
		// (main-generation-owner.ts:1583-1586); a compose answers the receipt
		// alone (:1581). A JSON object carries no order, so this projection is a
		// map and authorInputs is simply absent when the manifest froze none,
		// which is what the original's undefined field becomes.
		if inputs, frozen := manifest["authorInputs"]; frozen && inputs != nil {
			wire["authorInputs"] = inputs
		}
	}
	return wire, nil
}

// productionCompositionContract reads the run's frozen output contract.
//
// The reference freezes it as sourceManifest.outputContract
// (generation-run-repository.ts:679). This lane's own admission freezes the same
// value under the key "output" instead
// (cmd/novel-server/synthetic_generation_linux.go:711, syntheticManifest), so a
// run opened by generation:begin carries the contract only under that name.
// Reading the reference key first and falling back to the lane's name keeps the
// reference semantics for a real Electron-written database while still serving a
// run this lane opened; the fallback disappears as soon as the manifest freezes
// the reference key.
func productionCompositionContract(manifest map[string]any) string {
	if contract := syntheticString(manifest["outputContract"]); contract != "" {
		return contract
	}
	return syntheticString(manifest["output"])
}

// productionCompositionWire is the VisibleCompositionReceipt projection the
// browser receives (src/shared/generation-owner-contract.ts:39-46).
func productionCompositionWire(receipt *composition.Receipt) map[string]any {
	return map[string]any{
		"algorithm":   receipt.Algorithm,
		"text":        receipt.Text,
		"textHash":    receipt.TextHash,
		"artifactIds": append([]string(nil), receipt.ArtifactIDs...),
		"sources":     receipt.Sources,
	}
}

// productionCompositionIDs reads the selection array. A value that is not an array
// of strings is reported as malformed instead of being refused here, because the
// original refuses it inside the composition, after the algorithm and the output
// contract (generation-run-repository.ts:466).
func productionCompositionIDs(value any) ([]string, bool) {
	list, isList := value.([]any)
	if !isList {
		return nil, false
	}
	ids := make([]string, 0, len(list))
	for _, item := range list {
		text, isText := item.(string)
		if !isText {
			return nil, false
		}
		ids = append(ids, text)
	}
	return ids, true
}

// productionCompositionExpectedHash reads the third wire argument. An absent or
// null value means "no expected hash", the only shape the optional argument can
// take over a transport that serialises arguments as JSON: the renderer's
// undefined arrives as null, and refusing that would make the same renderer call
// fail on this lane while it succeeds on the desktop.
func productionCompositionExpectedHash(value any) *string {
	if value == nil {
		return nil
	}
	if text, isText := value.(string); isText {
		return &text
	}
	unmatchable := productionCompositionUnmatchable
	return &unmatchable
}

// productionCompositionRun resolves the browser handle against the committed run.
//
// It deliberately does not compare the run's frozen epoch with the calling lease: a
// lease rotates on every save while a run outlives the lease it was frozen under,
// which is why the handle carries its own epoch. The handle's epoch must still be
// the run's, the run must belong to this project, and the manifest must be frozen.
func productionCompositionRun(ctx context.Context, ledger *store.GenerationLedger, projectID string, handle syntheticHandle) (map[string]any, map[string]any, error) {
	value, err := syntheticCall(ctx, ledger, "get", handle.RunID)
	if err != nil {
		return nil, nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	if syntheticString(run["rootActionId"]) != handle.RootActionID || syntheticString(run["runId"]) != handle.RunID ||
		syntheticString(binding["projectId"]) != projectID || handle.ProjectID != projectID ||
		syntheticString(binding["epoch"]) != handle.Epoch || syntheticMap(binding["sourceManifest"]) == nil {
		return nil, nil, errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
	}
	return run, binding, nil
}

// productionCompositionReadRows projects the run's root into the pure package's
// inputs: one SELECT for the attempt and artifact rows, one for the root status.
// Neither statement writes, and neither leaves the admitted database.
func productionCompositionReadRows(ctx context.Context, db *sql.DB, binding map[string]any, runID, rootActionID string) (*productionCompositionRows, error) {
	bindingProjectID := syntheticString(binding["projectId"])
	bindingEpoch := syntheticString(binding["epoch"])
	bindingFingerprint, err := json.Marshal(binding["fingerprint"])
	if err != nil {
		return nil, err
	}
	var rootStatus sql.NullString
	if err := db.QueryRowContext(ctx,
		"SELECT json_extract(action_json,'$.status') FROM generation_roots WHERE root_action_id=?", rootActionID).Scan(&rootStatus); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, productionCompositionRowQuery, rootActionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projection := &productionCompositionRows{
		lookup:     make(map[string]composition.Artifact),
		rootStatus: rootStatus.String,
		attemptOf:  make(map[string]string),
	}
	for rows.Next() {
		var attemptID, rowRunID, attemptJSON, usageJSON string
		var ordinal int64
		var artifactID, artifactRunID, artifactJSON, artifactStatus sql.NullString
		var revision sql.NullInt64
		if err := rows.Scan(&attemptID, &rowRunID, &ordinal, &attemptJSON, &usageJSON,
			&artifactID, &artifactRunID, &artifactJSON, &revision, &artifactStatus); err != nil {
			return nil, err
		}
		var attempt struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(attemptJSON), &attempt); err != nil {
			return nil, errors.New("GENERATION_JSON_INVALID")
		}
		var usage productionCompositionUsage
		if err := json.Unmarshal([]byte(usageJSON), &usage); err != nil {
			return nil, errors.New("GENERATION_JSON_INVALID")
		}
		// compositionFrom (:661) answers the NEWEST stored composition of this run;
		// this scan is ascending, so the last one it sees is the newest.
		if rowRunID == runID && len(usage.VisibleComposition) > 0 && string(usage.VisibleComposition) != "null" {
			var stored composition.Stored
			if err := json.Unmarshal(usage.VisibleComposition, &stored); err != nil {
				return nil, errors.New("GENERATION_JSON_INVALID")
			}
			projection.stored = &stored
		}
		if !artifactID.Valid {
			continue
		}
		projection.attemptOf[artifactID.String] = attemptID
		var artifact composition.Artifact
		artifact.Present = artifactJSON.Valid
		artifact.ArtifactID = artifactID.String
		artifact.AttemptID = attemptID
		artifact.RunID = artifactRunID.String
		artifact.Status = artifactStatus.String
		artifact.Revision = revision.Int64
		artifact.Ordinal = ordinal
		artifact.AttemptStatus = attempt.Status
		artifact.Purpose = usage.Purpose
		artifact.BindingProjectID = bindingProjectID
		artifact.BindingEpoch = bindingEpoch
		artifact.BindingFingerprint = string(bindingFingerprint)
		if usage.Result != nil {
			artifact.FinishReason = usage.Result.FinishReason
		}
		if identity := usage.ArtifactIdentity; identity != nil {
			fingerprint, canonicalErr := composition.CanonicalJSON(identity.Fingerprint)
			if canonicalErr != nil {
				return nil, errors.New("GENERATION_JSON_INVALID")
			}
			artifact.HasIdentity = true
			artifact.IdentityArtifactID = identity.ArtifactID
			artifact.IdentityEpoch = identity.Epoch
			artifact.IdentityFingerprint = fingerprint
		}
		if artifactJSON.Valid {
			var payload productionCompositionArtifact
			if err := json.Unmarshal([]byte(artifactJSON.String), &payload); err != nil {
				return nil, errors.New("GENERATION_JSON_INVALID")
			}
			fingerprint, canonicalErr := composition.CanonicalJSON(payload.Fingerprint)
			if canonicalErr != nil {
				return nil, errors.New("GENERATION_JSON_INVALID")
			}
			artifact.PayloadArtifactID = payload.ArtifactID
			artifact.PayloadAttemptID = payload.AttemptID
			artifact.PayloadRevision = payload.Revision
			artifact.ProjectID = payload.ProjectID
			artifact.Epoch = payload.Epoch
			artifact.RootActionID = payload.RootActionID
			artifact.Fingerprint = fingerprint
			artifact.Text = payload.Text
			artifact.TextHash = payload.TextHash
		}
		projection.lookup[artifactID.String] = artifact
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return projection, nil
}

// productionCompositionStore writes the receipt onto the attempt that owns the last
// composed artifact, through the ledger's recordEffect: the same mutableRoot-guarded
// root mutation every other write in this lane uses, inside the same IMMEDIATE
// transaction. The reference writes the same key onto the same row
// (generation-run-repository.ts:689-693).
//
// The value is spelled with map[string]any and []any because the store's JSON
// encoder (internal/store/plottree.go:34-49) accepts exactly those nodes; the
// encoder sorts object keys, and object order carries no meaning in the reference's
// own equality (isDeepStrictEqual ignores it).
func productionCompositionStore(ctx context.Context, ledger *store.GenerationLedger, runID, attemptID string, receipt *composition.Receipt) error {
	if attemptID == "" {
		return errors.New("GENERATION_ATTEMPT_MISSING")
	}
	stored := composition.StoredOf(receipt)
	if stored == nil {
		return composition.ErrInvalid
	}
	artifactIDs := make([]any, len(stored.ArtifactIDs))
	for index, artifactID := range stored.ArtifactIDs {
		artifactIDs[index] = artifactID
	}
	sources := make([]any, len(stored.Sources))
	for index, source := range stored.Sources {
		sources[index] = map[string]any{
			"artifactId": source.ArtifactID,
			"revision":   source.Revision,
			"textHash":   source.TextHash,
		}
	}
	value := map[string]any{
		"algorithm":   stored.Algorithm,
		"textHash":    stored.TextHash,
		"artifactIds": artifactIDs,
		"sources":     sources,
	}
	_, err := syntheticCall(ctx, ledger, "recordEffect", runID, attemptID, "visibleComposition", value)
	return err
}
