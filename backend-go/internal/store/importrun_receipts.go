package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The durable import run's effect receipts. A receipt is the run's own promise
// that an effect was prepared, and then the proof that it was committed exactly
// once: prepared is a row, committed is that row's state plus the effect's own
// read-back.
//
// Ported from electron/repositories/import-run-repository.ts:
//
//	getEffectReceipt     :2330-2343  a read, and it must never write
//	prepareEffectReceipt :2345-2408  INSERTs the receipt and its ledger row
//	commitEffectReceipt  :2410-2530  one immediate transaction: effect first,
//	                                 checkpoint second, receipt state last
//
// The receipt is keyed by the reference's own key, never by the caller's opinion
// of it. expectedImportRunEffectKey (src/shared/import-run.ts:148) is the only
// legal key and assertImportRunEffectReceiptMetadata (:170) is what enforces it,
// which is why IMPORT_RUN_RECEIPT_KEY_INVALID is raised for a blank or
// over-long effectKey/batchId (after trimming) and for nothing else: a key of
// legal shape that is not the expected one is a CORRUPT receipt, because only
// the read-back can see the difference. Moving that check one statement earlier
// would refuse a row the reference accepts; one later and it would accept a row
// the reference refuses.
//
// The order inside commitEffectReceipt is load-bearing. IMPORT_RUN_RECEIPT_CONFLICT
// exists only in prepare (an existing row whose namespace, key, kind or payload
// hash disagrees) and IMPORT_RUN_RECEIPT_NOT_FOUND only in commit (no row at all
// for that run/stage/batch), so the two are always distinguishable; and a replay
// of an already committed receipt returns before the execution check, before the
// checkpoint check and before the effect, so it writes nothing.
//
// What is deliberately NOT here: three of the four effect families that
// commitEffectReceipt dispatches to.
//
//   - project-global-facts      ImportGlobalFactsRepository.commit
//     (electron/repositories/import-global-facts-repository.ts:162). No Go
//     family: the table exists (internal/admission/baseline_v7_ddl.go:277) but
//     the repository does not, and the effect needs the roster and character
//     identity families to write.
//   - chapter-blueprint-range   BlueprintRepository.commitRange. Go has
//     Blueprints.CommitRange (internal/store/blueprints.go:277), but it opens its
//     own immediate transaction (:300) and refuses a payload carrying
//     generationRunHandle (:283), so it cannot be nested inside this one without
//     either a deadlock or a divergence.
//   - author-finalized-batch    FinalizedDraftImportRepository.preview/commit.
//     Not ported at all; internal/store/draft_authority.go:19 only names it.
//
// project-writing-style is implemented, because it is the one kind whose
// committed authority read-back is empty in the reference
// (assertCommittedEffectAuthority returns at import-run-repository.ts:510), so
// nothing outside this file is needed to write it or to verify it.
//
// The other three kinds refuse with ErrImportRunReceiptKindUnbound, this port's
// own code. The reference has no such code; it is named here so a binder cannot
// mistake a missing family for a data problem.
type importRunReceipt struct {
	SchemaVersion   int64
	RunID           string
	EffectNamespace string
	EffectKey       string
	Stage           string
	BatchID         string
	Kind            string
	PayloadHash     string
	State           string
	Payload         map[string]any
	EffectReceipt   any
	Committed       bool
	CreatedAt       string
	UpdatedAt       string
}

// ImportRunPrepareEffectReceiptRequest is ImportRunPrepareEffectReceiptRequest
// (src/shared/import-run.ts:186) as it crosses the wire: the payload is the
// decoded JSON value, so numbers arrive as float64 exactly like the renderer's.
type ImportRunPrepareEffectReceiptRequest struct {
	RunID     string
	Stage     string
	BatchID   string
	EffectKey string
	Kind      string
	Payload   any
}

// ImportRunGenerationSlot is the sourceManifest.importSlot identity a generation
// run binds to, and the third argument prepareEffectReceipt and
// commitEffectReceipt hand to the source proof.
type ImportRunGenerationSlot struct {
	RunID   string
	Stage   string
	BatchID string
}

// ImportRunGenerationSourceAssert is the caller-supplied generation source proof
// (assertGenerationSources in the reference). The store never fabricates it: nil
// is the reference's own nil-argument case, where only the database binding
// decides whether a handle is required. The controller always supplies the proof
// (electron/controllers/db-controller.ts:425), so a binder that passes nil makes
// the handle requirement weaker than the original's, never stronger.
type ImportRunGenerationSourceAssert func(handle any, slot ImportRunGenerationSlot) error

// ErrImportRunReceiptKindUnbound is this port's own refusal and has no
// counterpart in the reference, which implements all four effect kinds. It is a
// symbolic code so a binder can carry it unchanged, but it must never be
// presented to an author as an original code.
var ErrImportRunReceiptKindUnbound = errors.New("IMPORT_RUN_RECEIPT_KIND_UNBOUND")

// errImportRunReceiptInvalid is the reference's bare throw new Error() in
// assertEffectPayloadSchema (:280), assertEffectPayloadBinding (:347),
// assertCommittedEffectSchema (:368) and rowToEffectReceipt's own guards. Its
// text never reaches a caller: every one of those call sites is inside
// rowToEffectReceipt's try/catch, which reports IMPORT_RUN_RECEIPT_CORRUPT
// instead. The lowercase message keeps that detail from leaking if a future
// caller forgets the mapping.
var errImportRunReceiptInvalid = errors.New("import run receipt payload does not match its recorded kind")

const (
	// importRunReceiptSchemaVersion mirrors IMPORT_RUN_EFFECT_RECEIPT_SCHEMA_VERSION
	// (src/shared/import-run.ts:63).
	importRunReceiptSchemaVersion int64 = 1
	// importRunReceiptMaxPayloadBytes mirrors MAX_EFFECT_RECEIPT_PAYLOAD_BYTES
	// (electron/repositories/import-run-repository.ts:189).
	importRunReceiptMaxPayloadBytes = 16 * 1024 * 1024
	// importRunReceiptMaxEffectKey and importRunReceiptMaxBatchID are the two
	// bounds checked by the single expression that raises
	// IMPORT_RUN_RECEIPT_KEY_INVALID (:2348).
	importRunReceiptMaxEffectKey = 200
	importRunReceiptMaxBatchID   = 160
	// importRunReceiptSpace is the JavaScript WhiteSpace + LineTerminator set,
	// so a trim here matches String.prototype.trim exactly. Go's TrimSpace would
	// leave a byte-order mark in place.
	importRunReceiptSpace = "\u0009\u000a\u000b\u000c\u000d\u0020\u00a0\u1680" +
		"\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a" +
		"\u2028\u2029\u202f\u205f\u3000\ufeff"
	// importRunReceiptSafeMax is Number.MAX_SAFE_INTEGER.
	importRunReceiptSafeMax int64 = 9007199254740991
)

// importRunReceiptRow is ImportRunEffectReceiptRow (:143).
type importRunReceiptRow struct {
	RunID             string
	SchemaVersion     int64
	EffectNamespace   string
	EffectKey         string
	Stage             string
	BatchID           string
	Kind              string
	PayloadJSON       string
	PayloadHash       string
	State             string
	EffectReceiptJSON sql.NullString
	CreatedAt         string
	UpdatedAt         string
}

const importRunReceiptRowColumns = `run_id, schema_version, effect_namespace, effect_key, stage,
 batch_id, kind, payload_json, payload_hash, state, effect_receipt_json, created_at, updated_at`

func readImportRunReceiptRow(ctx context.Context, q importRunQueryer, runID, stage, batchID string) (importRunReceiptRow, bool, error) {
	var row importRunReceiptRow
	err := q.QueryRowContext(ctx, `SELECT `+importRunReceiptRowColumns+` FROM import_run_receipts
 WHERE run_id = ? AND stage = ? AND batch_id = ?`, runID, stage, batchID).Scan(
		&row.RunID, &row.SchemaVersion, &row.EffectNamespace, &row.EffectKey, &row.Stage,
		&row.BatchID, &row.Kind, &row.PayloadJSON, &row.PayloadHash, &row.State,
		&row.EffectReceiptJSON, &row.CreatedAt, &row.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return importRunReceiptRow{}, false, nil
	}
	if err != nil {
		return importRunReceiptRow{}, false, err
	}
	return row, true, nil
}

// importRunReceiptSnapshot is the ImportRunEffectReceipt wire shape
// (src/shared/import-run.ts:100). effectReceipt is omitted while prepared, the
// same way JSON.stringify drops an undefined property.
func (receipt importRunReceipt) snapshot() map[string]any {
	snapshot := map[string]any{
		"schemaVersion":   receipt.SchemaVersion,
		"runId":           receipt.RunID,
		"effectNamespace": receipt.EffectNamespace,
		"effectKey":       receipt.EffectKey,
		"stage":           receipt.Stage,
		"batchId":         receipt.BatchID,
		"kind":            receipt.Kind,
		"payloadHash":     receipt.PayloadHash,
		"state":           receipt.State,
		"payload":         receipt.Payload,
		"createdAt":       receipt.CreatedAt,
		"updatedAt":       receipt.UpdatedAt,
	}
	if receipt.Committed {
		snapshot["effectReceipt"] = receipt.EffectReceipt
	}
	return snapshot
}

func importRunReceiptTrim(value string) string { return strings.Trim(value, importRunReceiptSpace) }

// importRunReceiptUTF16Length counts UTF-16 code units, which is what a
// JavaScript string length is. Counting bytes instead would refuse a legal
// multi-byte key one statement early.
func importRunReceiptUTF16Length(value string) int64 {
	length := int64(0)
	for _, character := range value {
		if character > 0xffff {
			length += 2
			continue
		}
		length++
	}
	return length
}

// importRunReceiptJSON encodes without HTML escaping and without the encoder's
// trailing newline, so the bytes match JSON.stringify.
func importRunReceiptJSON(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

// importRunReceiptField and importRunReceiptOrdered keep canonicalize's sorted
// key order on the wire: encoding/json sorts map keys itself, but it sorts them
// by its own rule, not canonicalize's.
type importRunReceiptField struct {
	key   string
	value any
}

type importRunReceiptOrdered []importRunReceiptField

func (ordered importRunReceiptOrdered) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for index, field := range ordered {
		if index > 0 {
			buffer.WriteByte(',')
		}
		name, err := importRunReceiptJSON(field.key)
		if err != nil {
			return nil, err
		}
		value, err := importRunReceiptJSON(field.value)
		if err != nil {
			return nil, err
		}
		buffer.WriteString(name)
		buffer.WriteByte(':')
		buffer.WriteString(value)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// importRunReceiptIndex mirrors a JavaScript integer-index property: it
// enumerates before every other key, in numeric order.
func importRunReceiptIndex(key string) (int64, bool) {
	if key == "" || (len(key) > 1 && key[0] == '0') {
		return 0, false
	}
	number, err := strconv.ParseInt(key, 10, 64)
	if err != nil || number < 0 || number >= 4294967295 {
		return 0, false
	}
	return number, strconv.FormatInt(number, 10) == key
}

// importRunReceiptKeyLess is the ASCII subset of canonicalize's
// localeCompare(..., 'en-US') ordering, the same rule blueprints.go:210
// documents for the same reason: integer-index keys first, then a
// case-insensitive compare, then lowercase before uppercase.
func importRunReceiptKeyLess(left, right string) bool {
	leftNumber, leftIndex := importRunReceiptIndex(left)
	rightNumber, rightIndex := importRunReceiptIndex(right)
	if leftIndex != rightIndex {
		return leftIndex
	}
	if leftIndex {
		return leftNumber < rightNumber
	}
	leftFolded, rightFolded := strings.ToLower(left), strings.ToLower(right)
	if leftFolded != rightFolded {
		return leftFolded < rightFolded
	}
	return left > right
}

// importRunReceiptCanonical is canonicalize (:240): sort object keys, keep
// array order, recurse. It is this port's own encoding, never a re-implementation
// of ICU collation, which is why the key order above is restricted to the ASCII
// keys these four payload shapes actually use.
func importRunReceiptCanonical(value any) (any, error) {
	switch entry := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(entry))
		for key := range entry {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(left, right int) bool {
			return importRunReceiptKeyLess(keys[left], keys[right])
		})
		ordered := make(importRunReceiptOrdered, 0, len(keys))
		for _, key := range keys {
			child, err := importRunReceiptCanonical(entry[key])
			if err != nil {
				return nil, err
			}
			ordered = append(ordered, importRunReceiptField{key: key, value: child})
		}
		return ordered, nil
	case []any:
		items := make([]any, len(entry))
		for index, item := range entry {
			child, err := importRunReceiptCanonical(item)
			if err != nil {
				return nil, err
			}
			items[index] = child
		}
		return items, nil
	}
	return value, nil
}

// importRunReceiptPayload is canonicalPayload's result (:251).
type importRunReceiptPayload struct {
	json  string
	hash  string
	value map[string]any
}

// importRunReceiptCanonicalPayload is canonicalPayload: an object is required,
// the canonical encoding is bounded, and the hash is over exactly those bytes.
// An array passes the reference's object test and is refused one statement later
// by the payload schema, so it is accepted here too.
func importRunReceiptCanonicalPayload(payload any) (importRunReceiptPayload, error) {
	invalid := importRunReceiptPayload{}
	if payload == nil {
		return invalid, errors.New("IMPORT_RUN_RECEIPT_PAYLOAD_INVALID")
	}
	switch payload.(type) {
	case map[string]any, []any:
	default:
		return invalid, errors.New("IMPORT_RUN_RECEIPT_PAYLOAD_INVALID")
	}
	canonical, err := importRunReceiptCanonical(payload)
	if err != nil {
		return invalid, errors.New("IMPORT_RUN_RECEIPT_PAYLOAD_INVALID")
	}
	encoded, err := importRunReceiptJSON(canonical)
	if err != nil {
		return invalid, errors.New("IMPORT_RUN_RECEIPT_PAYLOAD_INVALID")
	}
	if int64(len(encoded)) > importRunReceiptMaxPayloadBytes {
		return invalid, errors.New("IMPORT_RUN_RECEIPT_PAYLOAD_TOO_LARGE")
	}
	sum := sha256.Sum256([]byte(encoded))
	record, _ := payload.(map[string]any)
	return importRunReceiptPayload{json: encoded, hash: hex.EncodeToString(sum[:]), value: record}, nil
}

func importRunReceiptIsRecord(value any) bool {
	_, ok := value.(map[string]any)
	return ok
}

func importRunReceiptExactKeys(value map[string]any, expected ...string) bool {
	if len(value) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, present := value[key]; !present {
			return false
		}
	}
	return true
}

func importRunReceiptDigest(value any) bool {
	text, ok := value.(string)
	return ok && importRunSHA256.MatchString(text)
}

// importRunReceiptSafeInteger is Number.isSafeInteger: integral and inside the
// exactly representable range, so an int64 conversion cannot lose a value.
func importRunReceiptSafeInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case int64:
		if number < -importRunReceiptSafeMax || number > importRunReceiptSafeMax {
			return 0, false
		}
		return number, true
	case int:
		return importRunReceiptSafeInteger(int64(number))
	case int32:
		return int64(number), true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number ||
			number < -float64(importRunReceiptSafeMax) || number > float64(importRunReceiptSafeMax) {
			return 0, false
		}
		return int64(number), true
	case json.Number:
		parsed, err := number.Int64()
		if err != nil {
			floating, floatErr := number.Float64()
			if floatErr != nil {
				return 0, false
			}
			return importRunReceiptSafeInteger(floating)
		}
		return importRunReceiptSafeInteger(parsed)
	}
	return 0, false
}

func importRunReceiptHandle(value any) bool {
	handle, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if !importRunReceiptExactKeys(handle, "projectId", "epoch", "rootActionId", "runId") {
		return false
	}
	for _, key := range []string{"projectId", "epoch", "rootActionId", "runId"} {
		text, ok := handle[key].(string)
		if !ok || importRunReceiptTrim(text) == "" {
			return false
		}
	}
	return true
}

// importRunReceiptExpectedKey is expectedImportRunEffectKey
// (src/shared/import-run.ts:148): the receipt's key is derived from its own
// kind, stage and batch id, never accepted from the request.
func importRunReceiptExpectedKey(kind, stage, batchID string) (string, bool) {
	switch {
	case kind == "project-global-facts" && stage == "global" && batchID == "done":
		return "global-facts", true
	case kind == "project-writing-style" && stage == "style" && batchID == "done":
		return "writing-style", true
	case kind == "chapter-blueprint-range" && stage == "blueprints" &&
		parseImportRunChapterBatchCheckpointID(batchID) != nil:
		return "blueprints:" + batchID, true
	case kind == "author-finalized-batch" && stage == "author-commit" && batchID == "done":
		return "author-finalized-batch", true
	}
	return "", false
}

// importRunReceiptAssertStage is validateEffectStage (:604). An unknown kind has
// no expected stage and is therefore always a mismatch, exactly like the
// original's undefined comparison.
func importRunReceiptAssertStage(kind, stage string) error {
	expected, known := map[string]string{
		"project-global-facts":    "global",
		"project-writing-style":   "style",
		"chapter-blueprint-range": "blueprints",
		"author-finalized-batch":  "author-commit",
	}[kind]
	if !known || expected != stage {
		return errors.New("IMPORT_RUN_RECEIPT_STAGE_MISMATCH")
	}
	return nil
}

// importRunReceiptAssertPayloadSchema is assertEffectPayloadSchema (:280).
func importRunReceiptAssertPayloadSchema(kind string, payload map[string]any) error {
	if payload == nil {
		return errImportRunReceiptInvalid
	}
	generation := make([]string, 0, 1)
	if _, present := payload["generationRunHandle"]; present {
		if !importRunReceiptHandle(payload["generationRunHandle"]) {
			return errImportRunReceiptInvalid
		}
		generation = append(generation, "generationRunHandle")
	}
	switch kind {
	case "author-finalized-batch":
		if !importRunReceiptExactKeys(payload, "operationId", "runId", "authorityFingerprint", "manifestFingerprint") {
			return errImportRunReceiptInvalid
		}
		if _, ok := payload["operationId"].(string); !ok {
			return errImportRunReceiptInvalid
		}
		if _, ok := payload["runId"].(string); !ok {
			return errImportRunReceiptInvalid
		}
		if !importRunReceiptDigest(payload["authorityFingerprint"]) || !importRunReceiptDigest(payload["manifestFingerprint"]) {
			return errImportRunReceiptInvalid
		}
		return nil
	case "project-writing-style":
		keys := append([]string{"writingStyle"}, generation...)
		if !importRunReceiptExactKeys(payload, keys...) {
			return errImportRunReceiptInvalid
		}
		style, ok := payload["writingStyle"].(string)
		if !ok || importRunReceiptTrim(style) == "" {
			return errImportRunReceiptInvalid
		}
		return nil
	case "project-global-facts":
		keys := append([]string{"operationId", "expectedRosterRevision", "core", "characterEntries"}, generation...)
		if !importRunReceiptExactKeys(payload, keys...) {
			return errImportRunReceiptInvalid
		}
		if _, ok := payload["operationId"].(string); !ok {
			return errImportRunReceiptInvalid
		}
		if _, ok := importRunReceiptSafeInteger(payload["expectedRosterRevision"]); !ok {
			return errImportRunReceiptInvalid
		}
		if !importRunReceiptIsRecord(payload["core"]) {
			return errImportRunReceiptInvalid
		}
		entries, ok := payload["characterEntries"].([]any)
		if !ok || len(entries) == 0 {
			return errImportRunReceiptInvalid
		}
		return nil
	}
	keys := append([]string{"mode", "operationId", "startChapter", "endChapter", "blueprints"}, generation...)
	if !importRunReceiptExactKeys(payload, keys...) {
		return errImportRunReceiptInvalid
	}
	if mode, _ := payload["mode"].(string); mode != "replace-range" {
		return errImportRunReceiptInvalid
	}
	if _, ok := payload["operationId"].(string); !ok {
		return errImportRunReceiptInvalid
	}
	if _, ok := importRunReceiptSafeInteger(payload["startChapter"]); !ok {
		return errImportRunReceiptInvalid
	}
	if _, ok := importRunReceiptSafeInteger(payload["endChapter"]); !ok {
		return errImportRunReceiptInvalid
	}
	blueprints, ok := payload["blueprints"].([]any)
	if !ok || len(blueprints) == 0 {
		return errImportRunReceiptInvalid
	}
	return nil
}

// importRunReceiptAssertPayloadBinding is assertEffectPayloadBinding (:347): the
// payload's operation id is derived from the run (and, for a blueprint range,
// from the batch), so a receipt cannot be replayed under a different one.
func importRunReceiptAssertPayloadBinding(kind, runID, batchID string, payload map[string]any) error {
	switch kind {
	case "author-finalized-batch":
		if batchID != "done" {
			return errImportRunReceiptInvalid
		}
		return nil
	case "project-global-facts":
		operationID, ok := payload["operationId"].(string)
		if !ok || operationID != "novel-import-global-"+runID {
			return errImportRunReceiptInvalid
		}
		return nil
	case "chapter-blueprint-range":
		checkpoint := parseImportRunChapterBatchCheckpointID(batchID)
		if checkpoint == nil {
			return errImportRunReceiptInvalid
		}
		operationID, ok := payload["operationId"].(string)
		if !ok {
			return errImportRunReceiptInvalid
		}
		start, startOK := importRunReceiptSafeInteger(payload["startChapter"])
		end, endOK := importRunReceiptSafeInteger(payload["endChapter"])
		if !startOK || !endOK || start != checkpoint.startChapter || end != checkpoint.endChapter {
			return errImportRunReceiptInvalid
		}
		want := "import-blueprints-" + runID + "-" + strconv.FormatInt(start, 10) + "-" + strconv.FormatInt(end, 10)
		if operationID != want {
			return errImportRunReceiptInvalid
		}
		return nil
	}
	return nil
}

// importRunReceiptAssertAuthorBinding is assertAuthorEffectRunBinding (:346). An
// author-finalized-batch receipt only belongs to a run whose purpose, stage and
// two fingerprints it names, and a committed one is additionally allowed to sit
// on any later stage of the same run.
func importRunReceiptAssertAuthorBinding(row importRunRow, kind string, payload map[string]any, committed bool) error {
	if kind != "author-finalized-batch" {
		return nil
	}
	unbound := errors.New("IMPORT_RUN_AUTHOR_RECEIPT_UNBOUND")
	stageMatches := row.Stage == "author-commit"
	if !stageMatches && committed {
		switch row.Stage {
		case "author-publish", "author-postprocess", "global", "style", "blueprints", "refresh", "completed":
			stageMatches = true
		}
	}
	if row.Purpose != "author-manuscript" || !stageMatches || payload == nil {
		return unbound
	}
	runID, runOK := payload["runId"].(string)
	operationID, operationOK := payload["operationId"].(string)
	authority, authorityOK := payload["authorityFingerprint"].(string)
	manifest, manifestOK := payload["manifestFingerprint"].(string)
	if !runOK || !operationOK || !authorityOK || !manifestOK {
		return unbound
	}
	if runID != row.ID || operationID != "author-import:"+row.ID ||
		authority != row.AuthorityFingerprint || manifest != row.ManifestFingerprint {
		return unbound
	}
	return nil
}

func importRunReceiptCanonicalEqual(left, right any) bool {
	leftCanonical, err := importRunReceiptCanonical(left)
	if err != nil {
		return false
	}
	rightCanonical, err := importRunReceiptCanonical(right)
	if err != nil {
		return false
	}
	leftJSON, err := importRunReceiptJSON(leftCanonical)
	if err != nil {
		return false
	}
	rightJSON, err := importRunReceiptJSON(rightCanonical)
	if err != nil {
		return false
	}
	return leftJSON == rightJSON
}

// importRunReceiptAssertCommittedSchema is assertCommittedEffectSchema (:368),
// minus nothing: every branch of it is a pure function of the payload and the
// effect receipt.
func importRunReceiptAssertCommittedSchema(kind string, payload map[string]any, effectReceipt any) error {
	effect, ok := effectReceipt.(map[string]any)
	if !ok || payload == nil {
		return errImportRunReceiptInvalid
	}
	if kind == "author-finalized-batch" {
		payloadOperation, _ := payload["operationId"].(string)
		operation, _ := effect["operationId"].(string)
		if operation != payloadOperation {
			return errImportRunReceiptInvalid
		}
		if _, ok := effect["payloadHash"].(string); !ok {
			return errImportRunReceiptInvalid
		}
		chapterNumbers, numbersOK := effect["chapterNumbers"].([]any)
		drafts, draftsOK := effect["drafts"].([]any)
		if !numbersOK || !draftsOK || len(chapterNumbers) != len(drafts) {
			return errImportRunReceiptInvalid
		}
		return nil
	}
	fillEmpty := false
	if mode, _ := effect["mode"].(string); mode == "fill-empty" {
		fillEmpty = true
	}
	if fillEmpty {
		if _, ok := effect["suggestions"].([]any); !ok {
			return errImportRunReceiptInvalid
		}
	}
	fillKeys := make([]string, 0, 2)
	if fillEmpty {
		fillKeys = append(fillKeys, "mode", "suggestions")
	}
	switch kind {
	case "project-writing-style":
		keys := append([]string{"writingStyle"}, fillKeys...)
		if !importRunReceiptExactKeys(effect, keys...) {
			return errImportRunReceiptInvalid
		}
		style, ok := effect["writingStyle"].(string)
		if !ok {
			return errImportRunReceiptInvalid
		}
		if !fillEmpty {
			payloadStyle, _ := payload["writingStyle"].(string)
			if style != payloadStyle {
				return errImportRunReceiptInvalid
			}
		}
		return nil
	case "project-global-facts":
		if proposal, ok := effect["characterProposal"].(map[string]any); ok {
			domainPayload := map[string]any{}
			for key, value := range payload {
				if key == "generationRunHandle" {
					continue
				}
				domainPayload[key] = value
			}
			keys := append([]string{"operationId", "payloadHash", "idempotent", "core", "characterProposal", "proposalSource"}, fillKeys...)
			if !importRunReceiptExactKeys(effect, keys...) {
				return errImportRunReceiptInvalid
			}
			payloadOperation, _ := payload["operationId"].(string)
			operation, _ := effect["operationId"].(string)
			if operation != payloadOperation || !importRunReceiptDigest(effect["payloadHash"]) {
				return errImportRunReceiptInvalid
			}
			if _, ok := effect["idempotent"].(bool); !ok {
				return errImportRunReceiptInvalid
			}
			if !importRunReceiptIsRecord(effect["core"]) {
				return errImportRunReceiptInvalid
			}
			if !importRunReceiptCanonicalEqual(effect["proposalSource"], domainPayload) {
				return errImportRunReceiptInvalid
			}
			if !importRunReceiptExactKeys(proposal, "proposalBatchId", "sourceHash") {
				return errImportRunReceiptInvalid
			}
			batchID, _ := proposal["proposalBatchId"].(string)
			if !strings.HasPrefix(batchID, "cpb:") || !importRunSHA256.MatchString(strings.TrimPrefix(batchID, "cpb:")) {
				return errImportRunReceiptInvalid
			}
			if !importRunReceiptDigest(proposal["sourceHash"]) {
				return errImportRunReceiptInvalid
			}
			return nil
		}
		if !importRunReceiptExactKeys(effect, "operationId", "payloadHash", "idempotent", "core", "roster") {
			return errImportRunReceiptInvalid
		}
		payloadOperation, _ := payload["operationId"].(string)
		operation, _ := effect["operationId"].(string)
		if operation != payloadOperation || !importRunReceiptDigest(effect["payloadHash"]) {
			return errImportRunReceiptInvalid
		}
		if _, ok := effect["idempotent"].(bool); !ok {
			return errImportRunReceiptInvalid
		}
		if !importRunReceiptIsRecord(effect["core"]) || !importRunReceiptIsRecord(effect["roster"]) {
			return errImportRunReceiptInvalid
		}
		return nil
	}
	keys := append([]string{
		"mode", "operationId", "payloadHash", "idempotent", "startChapter", "endChapter",
		"chapterNumbers", "snapshot", "characterSyncInput", "characterSyncOperation",
	}, fillKeys...)
	if !importRunReceiptExactKeys(effect, keys...) {
		return errImportRunReceiptInvalid
	}
	payloadOperation, _ := payload["operationId"].(string)
	operation, _ := effect["operationId"].(string)
	payloadMode, _ := payload["mode"].(string)
	mode, _ := effect["mode"].(string)
	if operation != payloadOperation || mode != payloadMode {
		return errImportRunReceiptInvalid
	}
	payloadStart, payloadStartOK := importRunReceiptSafeInteger(payload["startChapter"])
	payloadEnd, payloadEndOK := importRunReceiptSafeInteger(payload["endChapter"])
	start, startOK := importRunReceiptSafeInteger(effect["startChapter"])
	end, endOK := importRunReceiptSafeInteger(effect["endChapter"])
	if !payloadStartOK || !payloadEndOK || !startOK || !endOK || start != payloadStart || end != payloadEnd {
		return errImportRunReceiptInvalid
	}
	if _, ok := effect["payloadHash"].(string); !ok {
		return errImportRunReceiptInvalid
	}
	if _, ok := effect["idempotent"].(bool); !ok {
		return errImportRunReceiptInvalid
	}
	for _, key := range []string{"chapterNumbers", "snapshot", "characterSyncInput"} {
		if _, ok := effect[key].([]any); !ok {
			return errImportRunReceiptInvalid
		}
	}
	if !importRunReceiptIsRecord(effect["characterSyncOperation"]) {
		return errImportRunReceiptInvalid
	}
	return nil
}

// importRunReceiptParse is rowToEffectReceipt (:564) without its try/catch: the
// caller maps every failure to IMPORT_RUN_RECEIPT_CORRUPT, exactly like the
// original's corruptedReceipt.
//
// The original's last step, assertCommittedEffectAuthority (:508), compares a
// committed receipt against the authoritative effect of its own family. That
// comparison is empty for project-writing-style (:510) and is therefore honoured
// here for the one kind this lane can commit; for the other three it needs a
// family this lane does not have (see the file header), so their committed
// receipts are validated structurally only. That is weaker detection, never a
// wrong answer: nothing is invented and nothing is written.
func importRunReceiptParse(row importRunReceiptRow, run importRunRow) (importRunReceipt, error) {
	if row.State != "prepared" && row.State != "committed" {
		return importRunReceipt{}, errImportRunReceiptInvalid
	}
	var decoded any
	if err := json.Unmarshal([]byte(row.PayloadJSON), &decoded); err != nil {
		return importRunReceipt{}, errImportRunReceiptInvalid
	}
	canonical, err := importRunReceiptCanonical(decoded)
	if err != nil {
		return importRunReceipt{}, errImportRunReceiptInvalid
	}
	encoded, err := importRunReceiptJSON(canonical)
	if err != nil {
		return importRunReceipt{}, errImportRunReceiptInvalid
	}
	sum := sha256.Sum256([]byte(encoded))
	if encoded != row.PayloadJSON || hex.EncodeToString(sum[:]) != row.PayloadHash {
		return importRunReceipt{}, errImportRunReceiptInvalid
	}
	payload, ok := decoded.(map[string]any)
	if !ok {
		return importRunReceipt{}, errImportRunReceiptInvalid
	}
	if err := importRunReceiptAssertPayloadSchema(row.Kind, payload); err != nil {
		return importRunReceipt{}, err
	}
	if err := importRunReceiptAssertPayloadBinding(row.Kind, row.RunID, row.BatchID, payload); err != nil {
		return importRunReceipt{}, err
	}
	committed := row.State == "committed"
	if err := importRunReceiptAssertAuthorBinding(run, row.Kind, payload, committed); err != nil {
		return importRunReceipt{}, err
	}
	hasEffect := row.EffectReceiptJSON.Valid && row.EffectReceiptJSON.String != ""
	var effectReceipt any
	if hasEffect {
		if err := json.Unmarshal([]byte(row.EffectReceiptJSON.String), &effectReceipt); err != nil {
			return importRunReceipt{}, errImportRunReceiptInvalid
		}
	}
	if (!committed && hasEffect) || (committed && !hasEffect) {
		return importRunReceipt{}, errImportRunReceiptInvalid
	}
	if committed {
		if err := importRunReceiptAssertCommittedSchema(row.Kind, payload, effectReceipt); err != nil {
			return importRunReceipt{}, err
		}
	}
	expectedNamespace := "import:" + run.Purpose + ":" + run.ID
	expectedKey, keyKnown := importRunReceiptExpectedKey(row.Kind, row.Stage, row.BatchID)
	if row.SchemaVersion != importRunReceiptSchemaVersion || row.RunID != run.ID ||
		run.EffectNamespace != expectedNamespace || row.EffectNamespace != expectedNamespace ||
		!keyKnown || row.EffectKey != expectedKey {
		return importRunReceipt{}, errImportRunReceiptInvalid
	}
	return importRunReceipt{
		SchemaVersion:   row.SchemaVersion,
		RunID:           row.RunID,
		EffectNamespace: row.EffectNamespace,
		EffectKey:       row.EffectKey,
		Stage:           row.Stage,
		BatchID:         row.BatchID,
		Kind:            row.Kind,
		PayloadHash:     row.PayloadHash,
		State:           row.State,
		Payload:         payload,
		EffectReceipt:   effectReceipt,
		Committed:       committed,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}, nil
}

// importRunReceiptValidate is rowToEffectReceipt's try/catch (:565-601): the
// payload, the lineage and the metadata are each all-or-nothing, and any failure
// is a corrupt receipt rather than a replay.
func importRunReceiptValidate(row importRunReceiptRow, run importRunRow) (importRunReceipt, error) {
	receipt, err := importRunReceiptParse(row, run)
	if err != nil {
		return importRunReceipt{}, errors.New("IMPORT_RUN_RECEIPT_CORRUPT")
	}
	return receipt, nil
}

// importRunRecordPreparedEffect is recordPreparedEffect (:941): the ledger row is
// the receipt's shadow. A prepared receipt is partial, a committed one complete,
// and an existing row must already agree with the receipt it claims.
func importRunRecordPreparedEffect(ctx context.Context, q importRunQueryer, row importRunRow,
	namespace, effectKey, stage, batchID, state string) error {
	ledger, err := importRunHasLedger(ctx, q)
	if err != nil || !ledger {
		return err
	}
	expected := "partial"
	if state == "committed" {
		expected = "complete"
	}
	var existingNamespace, existingKey, existingState, evidence string
	err = q.QueryRowContext(ctx, `SELECT effect_namespace, effect_key, state, evidence_kind
 FROM import_effect_ledger WHERE run_id = ? AND stage = ? AND batch_id = ?`,
		row.ID, stage, batchID).Scan(&existingNamespace, &existingKey, &existingState, &evidence)
	if errors.Is(err, sql.ErrNoRows) {
		diagnostic := ""
		if expected == "partial" {
			diagnostic = "EFFECT_PREPARED_NOT_COMMITTED"
		}
		_, err = q.ExecContext(ctx, `INSERT INTO import_effect_ledger(
 run_id, stage, batch_id, effect_namespace, effect_key, state, evidence_kind, diagnostic_code
) VALUES(?, ?, ?, ?, ?, ?, 'receipt', ?)`,
			row.ID, stage, batchID, namespace, effectKey, expected, diagnostic)
		return err
	}
	if err != nil {
		return err
	}
	if existingNamespace != namespace || existingKey != effectKey || existingState != expected || evidence != "receipt" {
		return errors.New("IMPORT_RUN_EFFECT_LEDGER_MISMATCH")
	}
	return nil
}

// importRunReceiptGenerationBound is the reference's bound query (:2380): a
// generation run in this database already binding this import slot.
func importRunReceiptGenerationBound(ctx context.Context, q importRunQueryer, runID, stage, batchID string) (bool, error) {
	var present int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'generation_runs'`).Scan(&present)
	if err != nil {
		return false, err
	}
	if present == 0 {
		return false, nil
	}
	var bound int64
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM generation_runs
 WHERE json_extract(binding_json, '$.sourceManifest.importSlot.runId') = ?
   AND json_extract(binding_json, '$.sourceManifest.importSlot.stage') = ?
   AND json_extract(binding_json, '$.sourceManifest.importSlot.batchId') = ?`,
		runID, stage, batchID).Scan(&bound)
	if err != nil {
		return false, err
	}
	return bound > 0, nil
}

// importRunReceiptAssertPreparedGeneration is prepareEffectReceipt's generation
// block (:2374-2384). A truthy handle is any non-null value, because JavaScript
// truthiness is what the original tests; nothing is decoded here.
func importRunReceiptAssertPreparedGeneration(ctx context.Context, q importRunQueryer,
	assertSources ImportRunGenerationSourceAssert, runID, stage, batchID string, payload map[string]any) error {
	switch stage {
	case "global", "style", "blueprints":
	default:
		return nil
	}
	handle := payload["generationRunHandle"]
	bound, err := importRunReceiptGenerationBound(ctx, q, runID, stage, batchID)
	if err != nil {
		return err
	}
	if (assertSources != nil || bound) && handle == nil {
		return errors.New("GENERATION_IMPORT_EFFECT_HANDLE_REQUIRED")
	}
	if assertSources != nil && handle != nil {
		return assertSources(handle, ImportRunGenerationSlot{RunID: runID, Stage: stage, BatchID: batchID})
	}
	return nil
}

// importRunReceiptAssertCommitGeneration is commitEffectReceipt's generation
// guard (:2445-2449). Unlike prepare, it only runs when the payload carries a
// handle, and a handle without an available proof is its own refusal.
func importRunReceiptAssertCommitGeneration(assertSources ImportRunGenerationSourceAssert,
	runID, stage, batchID string, payload map[string]any) error {
	handle := payload["generationRunHandle"]
	if handle == nil {
		return nil
	}
	receiptStage := stage == "global" || stage == "style" || stage == "blueprints"
	if assertSources == nil || !receiptStage {
		return errors.New("GENERATION_GUARD_REQUIRED")
	}
	return assertSources(handle, ImportRunGenerationSlot{RunID: runID, Stage: stage, BatchID: batchID})
}

// importRunReceiptWritingStyle reads the one column the author's project states,
// so a missing main row reads as the empty string exactly like get()?.writingStyle.
func importRunReceiptWritingStyle(ctx context.Context, q importRunQueryer) (string, bool, error) {
	var style sql.NullString
	err := q.QueryRowContext(ctx, `SELECT writing_style FROM project_core WHERE id = 'main'`).Scan(&style)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return style.String, true, nil
}

// importRunReceiptWriteWritingStyle is the single-column statement
// ProjectCoreRepository.update({ writingStyle }) emits
// (internal/store/projectcore.go:156-183). It is written here, inside this
// transaction, because ProjectCore.Update would take its own pooled connection
// and deadlock against this transaction's reserved write lock.
func importRunReceiptWriteWritingStyle(ctx context.Context, q importRunQueryer, style string) error {
	_, err := q.ExecContext(ctx, `UPDATE project_core SET writing_style = ?, updated_at = datetime('now')
 WHERE id = 'main'`, style)
	return err
}

// importRunReceiptCommitWritingStyle is the project-writing-style arm of
// commitEffectReceipt's switch (:2462-2482). Author manuscripts only fill blanks;
// a reference import replaces. Both branches produce the receipt the original
// produces, including the fill-empty suggestions the author is offered instead of
// an overwrite.
func importRunReceiptCommitWritingStyle(ctx context.Context, q importRunQueryer,
	payload map[string]any, fillMode string) (any, error) {
	raw, ok := payload["writingStyle"].(string)
	if !ok || importRunReceiptTrim(raw) == "" {
		return nil, errors.New("IMPORT_RUN_RECEIPT_PAYLOAD_INVALID")
	}
	style := importRunReceiptTrim(raw)
	if fillMode != "fill-empty" {
		if err := importRunReceiptWriteWritingStyle(ctx, q, style); err != nil {
			return nil, err
		}
		return map[string]any{"writingStyle": style}, nil
	}
	current, present, err := importRunReceiptWritingStyle(ctx, q)
	if err != nil {
		return nil, err
	}
	existing := ""
	if present {
		existing = importRunReceiptTrim(current)
	}
	suggestions := make([]any, 0, 1)
	if existing == "" {
		if err := importRunReceiptWriteWritingStyle(ctx, q, style); err != nil {
			return nil, err
		}
	} else if existing != style {
		suggestions = append(suggestions, map[string]any{
			"field":    "writingStyle",
			"current":  existing,
			"inferred": style,
		})
	}
	stored, present, err := importRunReceiptWritingStyle(ctx, q)
	if err != nil {
		return nil, err
	}
	if !present {
		stored = ""
	}
	return map[string]any{
		"mode":         "fill-empty",
		"writingStyle": stored,
		"suggestions":  suggestions,
	}, nil
}

// importRunReceiptCommitEffect is the switch in commitEffectReceipt (:2450-2505).
// The three unbound kinds name a family this lane does not have; they never write
// and never advance the checkpoint, because the reference applies the checkpoint
// only after the effect succeeds.
func importRunReceiptCommitEffect(ctx context.Context, q importRunQueryer,
	receipt importRunReceipt, fillMode string) (any, error) {
	switch receipt.Kind {
	case "project-writing-style":
		return importRunReceiptCommitWritingStyle(ctx, q, receipt.Payload, fillMode)
	case "project-global-facts", "chapter-blueprint-range", "author-finalized-batch":
		return nil, ErrImportRunReceiptKindUnbound
	}
	return nil, errors.New("IMPORT_RUN_RECEIPT_KIND_UNSUPPORTED")
}

// GetEffectReceipt is db:import-run-effect-receipt-get: getEffectReceipt
// (:2330-2343). A missing receipt row is null, never a refusal — that is the
// reference's own answer, and the run is not even read in that case. A receipt
// whose run row is gone, or whose payload, lineage or metadata does not
// re-derive, is IMPORT_RUN_RECEIPT_CORRUPT. It runs in one read-only snapshot and
// writes nothing.
func (r *ImportRuns) GetEffectReceipt(ctx context.Context, runID, stage, batchID string) (any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response any
	err := r.importRunReadSnapshot(ctx, func(tx *sql.Tx) error {
		row, present, err := readImportRunReceiptRow(ctx, tx, runID, stage, batchID)
		if err != nil || !present {
			return err
		}
		run, present, err := readImportRunRow(ctx, tx, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_RECEIPT_CORRUPT")
		}
		receipt, err := importRunReceiptValidate(row, run)
		if err != nil {
			return err
		}
		response = receipt.snapshot()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// PrepareEffectReceipt is db:import-run-effect-receipt-prepare: prepareEffectReceipt
// (:2345-2408), one immediate transaction.
//
// Statement order, and why it is this order:
//
//  1. trim, then refuse a blank or over-long effectKey/batchId (KEY_INVALID)
//  2. refuse a stage that does not belong to the kind (STAGE_MISMATCH)
//  3. canonicalize and bound the payload (PAYLOAD_INVALID / PAYLOAD_TOO_LARGE)
//  4. assert the execution lease (EXECUTION_STALE)
//  5. assert the author binding (AUTHOR_RECEIPT_UNBOUND)
//  6. assert the checkpoint can apply (BATCH_STAGE_MISMATCH and friends)
//  7. read the existing receipt for this run/stage/batch
//  8. on a match, promote the ledger if needed and return it; on a mismatch,
//     CONFLICT — nothing is written either way
//  9. require a generation handle where the stage demands one
//  10. INSERT the receipt, then its partial ledger row, then read both back
func (r *ImportRuns) PrepareEffectReceipt(ctx context.Context, execution ImportRunExecution,
	request ImportRunPrepareEffectReceiptRequest, assertSources ImportRunGenerationSourceAssert) (any, error) {
	effectKey := importRunReceiptTrim(request.EffectKey)
	batchID := importRunReceiptTrim(request.BatchID)
	if effectKey == "" || importRunReceiptUTF16Length(effectKey) > importRunReceiptMaxEffectKey ||
		batchID == "" || importRunReceiptUTF16Length(batchID) > importRunReceiptMaxBatchID {
		return nil, errors.New("IMPORT_RUN_RECEIPT_KEY_INVALID")
	}
	if err := importRunReceiptAssertStage(request.Kind, request.Stage); err != nil {
		return nil, err
	}
	payload, err := importRunReceiptCanonicalPayload(request.Payload)
	if err != nil {
		return nil, err
	}
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response any
	err = r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, err := importRunAssertExecutionAuthority(ctx, conn, request.RunID, execution, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if err := importRunReceiptAssertAuthorBinding(row, request.Kind, payload.value, false); err != nil {
			return err
		}
		if err := importRunAssertCheckpointCanApply(ctx, conn, row, request.Stage, batchID, "receipt"); err != nil {
			return err
		}
		existing, present, err := readImportRunReceiptRow(ctx, conn, request.RunID, request.Stage, batchID)
		if err != nil {
			return err
		}
		if present {
			if existing.EffectNamespace != row.EffectNamespace || existing.EffectKey != effectKey ||
				existing.Kind != request.Kind || existing.PayloadHash != payload.hash {
				return errors.New("IMPORT_RUN_RECEIPT_CONFLICT")
			}
			if err := importRunRecordPreparedEffect(ctx, conn, row, existing.EffectNamespace,
				existing.EffectKey, existing.Stage, existing.BatchID, existing.State); err != nil {
				return err
			}
			replayed, err := importRunReceiptValidate(existing, row)
			if err != nil {
				return err
			}
			response = replayed.snapshot()
			return nil
		}
		if err := importRunReceiptAssertPreparedGeneration(ctx, conn, assertSources,
			request.RunID, request.Stage, batchID, payload.value); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO import_run_receipts (
 run_id, schema_version, effect_namespace, effect_key, stage, batch_id, kind, payload_json, payload_hash
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			request.RunID, importRunReceiptSchemaVersion, row.EffectNamespace, effectKey,
			request.Stage, batchID, request.Kind, payload.json, payload.hash); err != nil {
			return err
		}
		if err := importRunRecordPreparedEffect(ctx, conn, row, row.EffectNamespace,
			effectKey, request.Stage, batchID, "prepared"); err != nil {
			return err
		}
		created, present, err := readImportRunReceiptRow(ctx, conn, request.RunID, request.Stage, batchID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		receipt, err := importRunReceiptValidate(created, row)
		if err != nil {
			return err
		}
		response = receipt.snapshot()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// CommitEffectReceipt is db:import-run-effect-receipt-commit: commitEffectReceipt
// (:2410-2530), one immediate transaction.
//
// Statement order, and why it is this order:
//
//  1. read the run row, NOT_FOUND if it is gone
//  2. read the receipt row, RECEIPT_NOT_FOUND if there is none
//  3. validate the stored receipt (CORRUPT)
//  4. if it is already committed, promote the ledger if needed and return: no
//     execution check, no checkpoint check, no effect, no write
//  5. assert the execution lease (EXECUTION_STALE)
//  6. assert the checkpoint can apply
//  7. require a generation proof when the payload carries a handle
//  8. write the effect
//  9. apply the batch checkpoint (promotes the ledger row, applies the deferred
//     cancel) — it insists the receipt is still prepared
//  10. only now mark the receipt committed
//  11. read the run and the receipt back and return them
//
// Steps 8 to 10 are what make a crash between them recoverable and a replay
// inert: the ledger promotion in step 9 requires a prepared receipt, so it can
// never run twice, and step 4 returns before it would.
func (r *ImportRuns) CommitEffectReceipt(ctx context.Context, runID, stage, batchID string,
	execution ImportRunExecution, assertSources ImportRunGenerationSourceAssert) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response map[string]any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_NOT_FOUND")
		}
		stored, present, err := readImportRunReceiptRow(ctx, conn, runID, stage, batchID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_RECEIPT_NOT_FOUND")
		}
		receipt, err := importRunReceiptValidate(stored, row)
		if err != nil {
			return err
		}
		if receipt.Committed {
			if err := importRunRecordPreparedEffect(ctx, conn, row, receipt.EffectNamespace,
				receipt.EffectKey, receipt.Stage, receipt.BatchID, receipt.State); err != nil {
				return err
			}
			snapshot, err := importRunSnapshot(ctx, conn, row)
			if err != nil {
				return err
			}
			response = map[string]any{
				"receipt":       receipt.snapshot(),
				"run":           snapshot,
				"cancelApplied": row.CancelRequested == 1,
			}
			return nil
		}
		if _, err := importRunAssertExecutionAuthority(ctx, conn, runID, execution, time.Now().UnixMilli()); err != nil {
			return err
		}
		if err := importRunAssertCheckpointCanApply(ctx, conn, row, stage, batchID, "receipt"); err != nil {
			return err
		}
		if err := importRunReceiptAssertCommitGeneration(assertSources, runID, stage, batchID, receipt.Payload); err != nil {
			return err
		}
		fillMode := "replace"
		if row.Purpose == "author-manuscript" {
			fillMode = "fill-empty"
		}
		effectReceipt, err := importRunReceiptCommitEffect(ctx, conn, receipt, fillMode)
		if err != nil {
			return err
		}
		_, cancelApplied, err := importRunApplyBatchCheckpoint(ctx, conn, row, stage, batchID, "receipt")
		if err != nil {
			return err
		}
		encoded, err := importRunReceiptJSON(effectReceipt)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE import_run_receipts
 SET state = 'committed', effect_receipt_json = ?, updated_at = datetime('now')
 WHERE run_id = ? AND stage = ? AND batch_id = ? AND state = 'prepared'`,
			encoded, runID, stage, batchID); err != nil {
			return err
		}
		committedRow, present, err := readImportRunReceiptRow(ctx, conn, runID, stage, batchID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		updated, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		committed, err := importRunReceiptValidate(committedRow, updated)
		if err != nil {
			return err
		}
		snapshot, err := importRunSnapshot(ctx, conn, updated)
		if err != nil {
			return err
		}
		response = map[string]any{
			"receipt":       committed.snapshot(),
			"run":           snapshot,
			"cancelApplied": cancelApplied,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}
