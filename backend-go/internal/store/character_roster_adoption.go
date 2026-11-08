package store

import (
	"context"
	"database/sql"
	"errors"
	"unicode/utf8"
)

// Port of adoptLegacyCards (electron/services/legacy-roster-source.ts:71-104).
// The reference does the replay, the source re-proof, the projection refresh and
// the approval receipt in ONE immediate transaction (:80-103) so a failure after
// the projection refresh rolls the projection and the receipt back together. This
// file is that transaction, on the same pattern the author-roster commit already
// uses (character_roster_author.go:156-437): one reserved connection, one
// BEGIN IMMEDIATE, the projection refresh (characterIdentityRefreshProjection)
// and the approval INSERT on the connection it was given.
//
// The one fact this file cannot compute is the raw-row factsHash: it is a sha256
// over JSON.stringify of the three SELECT * row sets in column order, and the
// only encoder that reproduces that byte shape is the caller's (the production
// lane's productionLegacyRosterJSON). So the caller passes a `source` proof that
// runs on the same connection and returns the CAS tokens; everything else stays
// here, which is exactly the one-transaction port the old refusal asked for.

var (
	// legacy-roster-source.ts:75, :87, :88, :92, :94, :97.
	ErrLegacyRosterAdoptionInvalid         = errors.New("LEGACY_ROSTER_ADOPTION_INVALID")
	ErrLegacyRosterAdoptionReceiptInvalid  = errors.New("LEGACY_ROSTER_ADOPTION_RECEIPT_INVALID")
	ErrLegacyRosterAdoptionNonceConflict   = errors.New("LEGACY_ROSTER_ADOPTION_NONCE_CONFLICT")
	ErrLegacyRosterAdoptionCardsRequired   = errors.New("LEGACY_ROSTER_CARDS_REQUIRED")
	ErrLegacyRosterAdoptionSourceChanged   = errors.New("LEGACY_ROSTER_ADOPTION_SOURCE_CHANGED")
	ErrLegacyRosterAdoptionReadbackInvalid = errors.New("LEGACY_ROSTER_ADOPTION_READBACK_INVALID")
)

const (
	legacyRosterAdoptionPrefix = "legacy-cards-adoption:"
	legacyRosterAdoptionKind   = "legacy-cards-adoption"
	legacyRosterAdoptionMaxID  = 200
)

// LegacyRosterAdoptionRequest is AdoptLegacyCardsRequest (legacy-roster-source.ts:20-26).
type LegacyRosterAdoptionRequest struct {
	OperationID             string
	ExpectedRevision        int64
	ExpectedLegacyHash      string
	ExpectedIdentityRevision int64
	ExpectedFactsHash       string
}

// LegacyRosterAdoptionSource is the subset of LegacyRosterSource the adoption
// CAS consumes. The raw-row facts hash is the caller's (see the header).
type LegacyRosterAdoptionSource struct {
	RawLegacy        string
	LegacyHash       string
	IdentityRevision int64
	ActiveIDs        []string
	FactsHash        string
}

// LegacyRosterAdoptionReceipt is AdoptLegacyCardsReceipt (legacy-roster-source.ts:27-32).
type LegacyRosterAdoptionReceipt struct {
	Success     bool           `json:"success"`
	OperationID string         `json:"operationId"`
	Snapshot    map[string]any `json:"snapshot"`
	ActiveIDs   []string       `json:"activeIds"`
}

// AdoptLegacyRoster runs the adoption. `raw` is the request envelope the wire
// decoded; `source` proves the source inside the same transaction, over the
// reserved connection, so the CAS tokens and the writes observe one state. It
// returns the receipt the browser receives, or one of the ErrLegacyRoster* codes.
func (r *Characters) AdoptLegacyRoster(ctx context.Context, raw map[string]any, source func(context.Context, *sql.Conn) (LegacyRosterAdoptionSource, error)) (LegacyRosterAdoptionReceipt, error) {
	if r == nil || r.db == nil {
		return LegacyRosterAdoptionReceipt{}, ErrProjectNotOpen
	}
	if source == nil {
		return LegacyRosterAdoptionReceipt{}, ErrLegacyRosterAdoptionInvalid
	}
	request, err := legacyRosterAdoptionParse(raw)
	if err != nil {
		return LegacyRosterAdoptionReceipt{}, err
	}
	requestHash := legacyRosterAdoptionRequestHash(request)
	operationID := legacyRosterAdoptionPrefix + request.OperationID
	var receipt LegacyRosterAdoptionReceipt
	err = NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		// :81-90 a previous receipt is replayed, never rewritten.
		prior, err := rrMaps(ctx, conn, "SELECT payload_hash,receipt_json FROM character_identity_approvals WHERE operation_id=?", operationID)
		if err != nil {
			return err
		}
		if len(prior) > 0 {
			return legacyRosterAdoptionReplay(request, requestHash, prior[0], &receipt)
		}
		// :91 the source, re-proved on this connection: the source guards and the
		// raw-row facts hash come first, so a broken source answers the legacy
		// codes before the store's own roster read.
		src, err := source(ctx, conn)
		if err != nil {
			return err
		}
		snapshot, err := charactersRoster(ctx, conn)
		if err != nil {
			return err
		}
		revision, ok := rrInteger(snapshot["revision"], 0)
		if !ok {
			return ErrCharacterInputUnsupported
		}
		// :92 the adoption only exists for preserved, inconsistent cards.
		if rosterText(snapshot["migrationState"]) != "legacy_cards_preserved" ||
			rosterText(snapshot["status"]) != "inconsistent" || len(src.ActiveIDs) == 0 {
			return ErrLegacyRosterAdoptionCardsRequired
		}
		// :93-94 the four CAS tokens.
		if revision != request.ExpectedRevision || src.IdentityRevision != request.ExpectedIdentityRevision ||
			src.LegacyHash != request.ExpectedLegacyHash || src.FactsHash != request.ExpectedFactsHash {
			return ErrLegacyRosterAdoptionSourceChanged
		}
		// :95 the read-only projection, rebuilt from the cards the author already
		// has, on this same connection.
		if err := characterIdentityRefreshProjection(ctx, conn); err != nil {
			return err
		}
		// :96-97 the read-back: ready state, ready migration, and the preserved
		// text still present exactly as it was.
		readback, err := charactersRoster(ctx, conn)
		if err != nil {
			return err
		}
		if rosterText(readback["status"]) != "ready" || rosterText(readback["migrationState"]) != "ready" {
			return ErrLegacyRosterAdoptionReadbackInvalid
		}
		if legacyMarkdown, present := readback["legacyMarkdown"]; (src.RawLegacy != "" && (!present || rosterText(legacyMarkdown) != src.RawLegacy)) || (src.RawLegacy == "" && present) {
			return ErrLegacyRosterAdoptionReadbackInvalid
		}
		// :99-102 the receipt, the envelope and the approval row, in the
		// reference's key order.
		receiptObject := plotObject{
			{"success", true},
			{"operationId", request.OperationID},
			{"snapshot", readback},
			{"activeIds", legacyRosterAdoptionActiveIDs(src.ActiveIDs)},
		}
		receiptText, err := plotJSON(receiptObject)
		if err != nil {
			return ErrLegacyRosterAdoptionReadbackInvalid
		}
		receiptHash := summaryHash(receiptText)
		envelopeText, err := plotJSON(plotObject{
			{"version", int64(1)},
			{"kind", legacyRosterAdoptionKind},
			{"request", legacyRosterAdoptionRequestObject(request)},
			{"requestHash", requestHash},
			{"receipt", receiptObject},
			{"receiptHash", receiptHash},
		})
		if err != nil {
			return ErrLegacyRosterAdoptionReadbackInvalid
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO character_identity_approvals(operation_id,payload_hash,receipt_json) VALUES(?,?,?)", operationID, requestHash, envelopeText); err != nil {
			return err
		}
		receipt = LegacyRosterAdoptionReceipt{
			Success:     true,
			OperationID: request.OperationID,
			Snapshot:    readback,
			ActiveIDs:   src.ActiveIDs,
		}
		return nil
	})
	if err != nil {
		return LegacyRosterAdoptionReceipt{}, err
	}
	return receipt, nil
}

// legacyRosterAdoptionParse is the request guard (legacy-roster-source.ts:72-75):
// exactly the five keys, a non-blank operationId of at most 200 characters, safe
// non-negative revisions, and two 64-hex hashes.
func legacyRosterAdoptionParse(raw map[string]any) (LegacyRosterAdoptionRequest, error) {
	if !pfAllow(raw, "operationId", "expectedRevision", "expectedLegacyHash", "expectedIdentityRevision", "expectedFactsHash") {
		return LegacyRosterAdoptionRequest{}, ErrLegacyRosterAdoptionInvalid
	}
	operationID := pfText(raw, "operationId")
	revision, revisionOK := pfInteger(raw, "expectedRevision")
	identityRevision, identityOK := pfInteger(raw, "expectedIdentityRevision")
	legacyHash := pfText(raw, "expectedLegacyHash")
	factsHash := pfText(raw, "expectedFactsHash")
	if summaryTrim(operationID) == "" || utf8.RuneCountInString(operationID) > legacyRosterAdoptionMaxID ||
		!revisionOK || revision < 0 || !identityOK || identityRevision < 0 ||
		!pfDigest(legacyHash) || !pfDigest(factsHash) {
		return LegacyRosterAdoptionRequest{}, ErrLegacyRosterAdoptionInvalid
	}
	return LegacyRosterAdoptionRequest{
		OperationID:              operationID,
		ExpectedRevision:         revision,
		ExpectedLegacyHash:       legacyHash,
		ExpectedIdentityRevision: identityRevision,
		ExpectedFactsHash:        factsHash,
	}, nil
}

// legacyRosterAdoptionRequestObject is the canonical `original` object
// (legacy-roster-source.ts:77-78) in the reference's key order.
func legacyRosterAdoptionRequestObject(request LegacyRosterAdoptionRequest) plotObject {
	return plotObject{
		{"operationId", request.OperationID},
		{"expectedRevision", request.ExpectedRevision},
		{"expectedLegacyHash", request.ExpectedLegacyHash},
		{"expectedIdentityRevision", request.ExpectedIdentityRevision},
		{"expectedFactsHash", request.ExpectedFactsHash},
	}
}

func legacyRosterAdoptionRequestHash(request LegacyRosterAdoptionRequest) string {
	text, err := plotJSON(legacyRosterAdoptionRequestObject(request))
	if err != nil {
		return ""
	}
	return summaryHash(text)
}

func legacyRosterAdoptionActiveIDs(ids []string) []any {
	out := make([]any, len(ids))
	for index, id := range ids {
		out[index] = id
	}
	return out
}

// legacyRosterAdoptionReplay is :81-90: a persisted receipt is re-verified field
// by field, and either replayed or refused as a nonce conflict. The decision line
// the idempotent-or-conflict answer turns on is :88.
func legacyRosterAdoptionReplay(request LegacyRosterAdoptionRequest, requestHash string, prior map[string]any, out *LegacyRosterAdoptionReceipt) error {
	value, err := glParse(rosterText(prior["receipt_json"]))
	if err != nil {
		return ErrLegacyRosterAdoptionReceiptInvalid
	}
	envelope := pfObject(value)
	if envelope == nil {
		return ErrLegacyRosterAdoptionReceiptInvalid
	}
	version, versionOK := pfInteger(envelope, "version")
	// :83-84 version, kind and the requestHash the row stored.
	if !versionOK || version != 1 || pfText(envelope, "kind") != legacyRosterAdoptionKind ||
		pfText(envelope, "requestHash") != rosterText(prior["payload_hash"]) {
		return ErrLegacyRosterAdoptionReceiptInvalid
	}
	// :85 the request and the receipt each still hash to their recorded digests.
	requestValue := pfValue(envelope, "request")
	requestText, err := plotJSON(requestValue)
	if err != nil || summaryHash(requestText) != pfText(envelope, "requestHash") {
		return ErrLegacyRosterAdoptionReceiptInvalid
	}
	receiptValue := pfValue(envelope, "receipt")
	receiptText, err := plotJSON(receiptValue)
	if err != nil || summaryHash(receiptText) != pfText(envelope, "receiptHash") {
		return ErrLegacyRosterAdoptionReceiptInvalid
	}
	// :86-87 the receipt's success, operation id, activeIds and snapshot state.
	receipt := pfObject(receiptValue)
	activeIDsValue, activeIDsOK := pfArray(receipt, "activeIds")
	if receipt == nil || !phTruthy(pfValue(receipt, "success")) ||
		pfText(receipt, "operationId") != request.OperationID || !activeIDsOK {
		return ErrLegacyRosterAdoptionReceiptInvalid
	}
	snapshot := pfObject(pfValue(receipt, "snapshot"))
	if snapshot == nil || pfText(snapshot, "migrationState") != "ready" || pfText(snapshot, "status") != "ready" {
		return ErrLegacyRosterAdoptionReceiptInvalid
	}
	// :88 the idempotent-or-conflict decision.
	if requestHash != pfText(envelope, "requestHash") || !glSame(legacyRosterAdoptionRequestObject(request), requestValue) {
		return ErrLegacyRosterAdoptionNonceConflict
	}
	activeIDs := make([]string, 0, len(activeIDsValue))
	for _, id := range activeIDsValue {
		activeIDs = append(activeIDs, rosterText(id))
	}
	snapshotMap, ok := glPlain(snapshot).(map[string]any)
	if !ok {
		return ErrLegacyRosterAdoptionReceiptInvalid
	}
	*out = LegacyRosterAdoptionReceipt{
		Success:     true,
		OperationID: request.OperationID,
		Snapshot:    snapshotMap,
		ActiveIDs:   activeIDs,
	}
	return nil
}
