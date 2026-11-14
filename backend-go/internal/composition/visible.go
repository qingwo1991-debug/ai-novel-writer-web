// Package composition is the pure port of the shared visible-composition
// algorithm and its receipt.
//
// The reference implementation is two TypeScript sources plus their callers:
//
//   - src/shared/visible-continuation.ts - the append algorithm
//     (composeVisibleContinuation, overlappingVisiblePrefixLength,
//     removeLeadingNonWhitespaceCharacters) and its two constants.
//   - electron/repositories/generation-run-repository.ts - the receipt: the
//     private visibleComposition helper (:460-513), compositionFrom (:659-674),
//     composeVisible (:675-696), hydrateReceipt (:361-375) and textHash (:92).
//
// The enforcement order of that repository method is part of the contract, not
// an implementation detail, so it is reproduced here in the same sequence:
// agent-round immutability, cancelled root, algorithm/operation pairing, output
// contract, algorithm membership, artifact-id shape, per-source hydrate
// integrity, per-source purpose, terminal and trusted status, progress, then -
// for a compose - previous-receipt integrity, algorithm changed, expected hash
// and regression.
//
// Nothing here performs I/O: no database, no HTTP, no filesystem. Inputs are
// plain structs, the output is a receipt, and every refusal is one symbolic
// error value, so the same bytes can be produced from a test table and from the
// production lane.
//
// Every text operation is carried out in UTF-16 code units, because that is what
// the original measures with String.prototype.slice, String.prototype.length and
// String.prototype[index]. A byte- or rune-oriented port would disagree with the
// original on the 1600-unit tail/head window of any text carrying a non-BMP
// character. The hash is taken over the UTF-8 encoding of the resulting string,
// in which an unpaired surrogate becomes U+FFFD exactly as utf16.Decode writes
// it, so both implementations hash the same bytes even when a window boundary
// splits a surrogate pair.
package composition

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode"
	"unicode/utf16"
)

// VisibleContinuationVersion is VISIBLE_CONTINUATION_VERSION
// (src/shared/visible-continuation.ts:1): the default algorithm of
// generation:compose-visible whenever the fourth wire argument is absent.
const VisibleContinuationVersion = "visible-append-v1"

// VisibleTailChars is CONTINUATION_VISIBLE_TAIL_CHARS
// (src/shared/visible-continuation.ts:2).
const VisibleTailChars = 1600

// minVisibleOverlapChars is MIN_VISIBLE_OVERLAP_CHARS
// (src/shared/visible-continuation.ts:3). A shorter overlap is not a repeated
// tail, so the addition is appended whole.
const minVisibleOverlapChars = 48

// MinVisibleOverlapChars is minVisibleOverlapChars, exported so a caller can
// assert the floor it depends on instead of restating the literal.
const MinVisibleOverlapChars = minVisibleOverlapChars

// maxCompositionSources is the non-outline cap on a composition's artifact list
// (electron/repositories/generation-run-repository.ts:466).
const maxCompositionSources = 32

// agentRoundOperation matches main-generation-owner.ts:1572, whose run freezes
// its own composition.
const agentRoundOperation = "agent-round"

// chapterDraftOperation is the only operation a draft algorithm belongs to
// (main-generation-owner.ts:1576).
const chapterDraftOperation = "chapter-draft"

// plotOutlineManifestOperation is the operation plotOutlinePolicy accepts
// (src/shared/plot-outline-contract.ts:100).
const plotOutlineManifestOperation = "generate-plot-outline"

// visibleTextContract is the only output contract a composition serves
// (generation-run-repository.ts:679).
const visibleTextContract = "visible-text"

// cancelledRootStatus is the root status that freezes a run's composition
// (main-generation-owner.ts:1575).
const cancelledRootStatus = "cancelled"

// draftReconcilePurpose is DRAFT_RECONCILE_PURPOSE (src/shared/draft-reconciliation.ts:12).
const draftReconcilePurpose = "chapter-draft-reconcile"

// draftShortOutlinePurpose is DRAFT_SHORT_OUTLINE_PURPOSE (src/shared/draft-short-outline.ts:3).
const draftShortOutlinePurpose = "chapter-draft-short-outline"

// draftVisibleOne is one of the two algorithms of src/shared/draft-visible-text.ts.
const draftVisibleOne = "draft-visible-v1"

// draftVisibleTwo is the other algorithm of src/shared/draft-visible-text.ts.
const draftVisibleTwo = "draft-visible-v2"

// PlotOutlineProtocol is PLOT_OUTLINE_PROTOCOL, the algorithm an outline run
// composes under. It is a sibling slice; naming it here lets a caller state
// which protocol this port refuses rather than inventing a code.
const PlotOutlineProtocol = "chapter-parts-v1"

// Refusal codes, verbatim from electron/repositories/generation-run-repository.ts
// (:466, :472, :474, :478, :505, :669, :683, :685, :687) and
// electron/services/main-generation-owner.ts (:1571, :1575).
const CodeOutputInvalid = "GENERATION_COMPOSITION_OUTPUT_INVALID"
const CodeAlgorithmInvalid = "GENERATION_COMPOSITION_ALGORITHM_INVALID"
const CodeAlgorithmChanged = "GENERATION_COMPOSITION_ALGORITHM_CHANGED"
const CodeInvalid = "GENERATION_COMPOSITION_INVALID"
const CodeSourceInvalid = "GENERATION_COMPOSITION_SOURCE_INVALID"
const CodeSourceNotTerminal = "GENERATION_COMPOSITION_SOURCE_NOT_TERMINAL"
const CodeSourceUntrusted = "GENERATION_COMPOSITION_SOURCE_UNTRUSTED"
const CodeNoProgress = "GENERATION_COMPOSITION_NO_PROGRESS"
const CodeHashMismatch = "GENERATION_COMPOSITION_HASH_MISMATCH"
const CodeRegression = "GENERATION_COMPOSITION_REGRESSION"
const CodeIntegrityFailed = "GENERATION_COMPOSITION_INTEGRITY_FAILED"

// CodeEpochStale is main-generation-owner.ts:164, the lease gate a compose
// reaches after the agent-round gate and before the cancelled-root gate.
const CodeEpochStale = "GENERATION_EPOCH_STALE"
const CodeArtifactIntegrity = "ARTIFACT_INTEGRITY_FAILED"
const CodeActionCancelled = "GENERATION_ACTION_CANCELLED"
const CodeAgentControlImmutable = "GENERATION_AGENT_CONTROL_IMMUTABLE"

// Refusal is one symbolic refusal. It carries no message, path or author text:
// the code is what the renderer matches on and what may safely reach a log.
type Refusal struct {
	Code string
}

// Error makes Refusal an error and answers with the bare code, the shape the
// original throws.
func (r *Refusal) Error() string {
	if r == nil {
		return ""
	}
	return r.Code
}

// The refusals this port can produce, one value per code. They are returned by
// pointer identity, so a caller may compare with errors.Is or compare Code.
var ErrOutputInvalid = &Refusal{Code: CodeOutputInvalid}
var ErrAlgorithmInvalid = &Refusal{Code: CodeAlgorithmInvalid}
var ErrAlgorithmChanged = &Refusal{Code: CodeAlgorithmChanged}
var ErrInvalid = &Refusal{Code: CodeInvalid}
var ErrSourceInvalid = &Refusal{Code: CodeSourceInvalid}
var ErrSourceNotTerminal = &Refusal{Code: CodeSourceNotTerminal}
var ErrSourceUntrusted = &Refusal{Code: CodeSourceUntrusted}
var ErrNoProgress = &Refusal{Code: CodeNoProgress}
var ErrHashMismatch = &Refusal{Code: CodeHashMismatch}
var ErrRegression = &Refusal{Code: CodeRegression}
var ErrIntegrityFailed = &Refusal{Code: CodeIntegrityFailed}
var ErrEpochStale = &Refusal{Code: CodeEpochStale}
var ErrArtifactIntegrity = &Refusal{Code: CodeArtifactIntegrity}
var ErrActionCancelled = &Refusal{Code: CodeActionCancelled}
var ErrAgentControlImmutable = &Refusal{Code: CodeAgentControlImmutable}

// SourceRef is one composed source reference, { artifactId, revision, textHash }
// (generation-run-repository.ts:487, :510). The revision and the hash are the
// ones the artifact payload records, not the joined row's columns, exactly like
// the original.
type SourceRef struct {
	ArtifactID string `json:"artifactId"`
	Revision   int64  `json:"revision"`
	TextHash   string `json:"textHash"`
}

// Receipt is VisibleCompositionReceipt
// (src/shared/generation-owner-contract.ts:39-46) without the read-only
// authorInputs projection the owner adds when it answers
// generation:read-visible-composition. The field order is the order the original
// builds, which is also the order of the JSON the browser receives.
type Receipt struct {
	Algorithm   string      `json:"algorithm"`
	Text        string      `json:"text"`
	TextHash    string      `json:"textHash"`
	ArtifactIDs []string    `json:"artifactIds"`
	Sources     []SourceRef `json:"sources"`
}

// Stored is the receipt as it is persisted: the original destructures the text
// out and stores the rest (generation-run-repository.ts:691-693), so the stored
// object is algorithm, textHash, artifactIds, sources - in that order.
type Stored struct {
	Algorithm   string      `json:"algorithm"`
	TextHash    string      `json:"textHash"`
	ArtifactIDs []string    `json:"artifactIds"`
	Sources     []SourceRef `json:"sources"`
}

// StoredOf projects a receipt into its persisted shape.
func StoredOf(receipt *Receipt) *Stored {
	if receipt == nil {
		return nil
	}
	return &Stored{
		Algorithm:   receipt.Algorithm,
		TextHash:    receipt.TextHash,
		ArtifactIDs: append([]string(nil), receipt.ArtifactIDs...),
		Sources:     append([]SourceRef(nil), receipt.Sources...),
	}
}

// Artifact is one candidate row as the ledger stores it: the artifact row joined
// to its attempt (receiptSelect, generation-run-repository.ts:85-90), with the
// artifact payload's own identity fields kept separate from the row's, because
// hydrateReceipt compares the two (:361-375).
type Artifact struct {
	// Present is false when the ledger holds no artifact payload for that id.
	Present bool

	// ArtifactID is generation_artifacts.artifact_id.
	ArtifactID string

	// AttemptID is the attempt the artifact row belongs to.
	AttemptID string

	// RunID is generation_artifacts.run_id.
	RunID string

	// Status is generation_artifacts.status.
	Status string

	// Revision is the generation_artifacts.revision column.
	Revision int64

	// Ordinal is the attempt's rowid, the original's ordinal.
	Ordinal int64

	// AttemptStatus is attempt_json's status.
	AttemptStatus string

	// FinishReason is usage.result.finishReason, "" when absent.
	FinishReason string

	// Purpose is usage.purpose, "" when absent.
	Purpose string

	// HasIdentity is true when usage.artifactIdentity is an object.
	HasIdentity bool

	// IdentityArtifactID is usage.artifactIdentity.artifactId.
	IdentityArtifactID string

	// IdentityEpoch is usage.artifactIdentity.epoch.
	IdentityEpoch string

	// IdentityFingerprint is the canonical JSON of the identity's fingerprint.
	IdentityFingerprint string

	// BindingProjectID is the run binding's projectId.
	BindingProjectID string

	// BindingEpoch is the run binding's epoch.
	BindingEpoch string

	// BindingFingerprint is the canonical JSON of the binding's fingerprint.
	BindingFingerprint string

	// PayloadArtifactID is artifact_json's artifactId.
	PayloadArtifactID string

	// PayloadAttemptID is artifact_json's attemptId.
	PayloadAttemptID string

	// PayloadRevision is artifact_json's revision.
	PayloadRevision int64

	// ProjectID is artifact_json's projectId.
	ProjectID string

	// Epoch is artifact_json's epoch.
	Epoch string

	// RootActionID is artifact_json's rootActionId.
	RootActionID string

	// Fingerprint is the canonical JSON of artifact_json's fingerprint.
	Fingerprint string

	// Text is artifact_json's text.
	Text string

	// TextHash is artifact_json's textHash.
	TextHash string
}

// Request is everything the two operations need, as data: the frozen run
// identity, the resolved artifact rows and the receipt the run already stores.
//
// Lookup is the row set scoped to the run's root, exactly like readState's
// artifacts map: an id the root never stored is simply absent. The stored
// receipt's own artifactIds are resolved through the same map, which is how the
// original re-composes a stored receipt to verify it.
type Request struct {
	// Operation is sourceManifest.operation.
	Operation string

	// OutputContract is sourceManifest.outputContract.
	OutputContract string

	// RootStatus is generation_roots action_json's status.
	RootStatus string

	// PlotOutline is true when the manifest describes an outline run.
	PlotOutline bool

	// EpochStale is the compose-only lease gate (requireRun(handle, true),
	// main-generation-owner.ts:164): the run's frozen epoch is no longer the live
	// project lease. A read never sets it, because a committed run outlives the
	// lease it was frozen under.
	EpochStale bool

	// RunID is the frozen binding's runId.
	RunID string

	// RootActionID is the frozen binding's rootActionId.
	RootActionID string

	// Algorithm is the fourth wire argument: nil when omitted, otherwise the
	// exact value the renderer sent, the empty string included.
	Algorithm *string

	// AlgorithmMalformed is true when the fourth argument was present but not a
	// string; no such value can name an algorithm.
	AlgorithmMalformed bool

	// ArtifactIDs is the requested composition, in wire order.
	ArtifactIDs []string

	// IDsMalformed is true when the second argument was not an array of strings.
	IDsMalformed bool

	// Lookup resolves an artifact id to its row.
	Lookup map[string]Artifact

	// ExpectedHash is the third wire argument: nil when absent, otherwise the
	// exact value, so a non-string can only ever mismatch.
	ExpectedHash *string

	// Stored is the newest receipt this run already stores, if any.
	Stored *Stored
}

// TextHash is textHash (generation-run-repository.ts:92): the SHA-256 of the
// UTF-8 bytes of the text, lower-case hex.
func TextHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// Compose is composeVisible (generation-run-repository.ts:675-696) together with
// the owner's preconditions (main-generation-owner.ts:1571-1582).
func Compose(req Request) (*Receipt, error) {
	algorithm := VisibleContinuationVersion
	if req.Algorithm != nil {
		algorithm = *req.Algorithm
	}
	// :1572 - an agent-round run's composition is frozen.
	if req.Operation == agentRoundOperation {
		return nil, ErrAgentControlImmutable
	}
	// :164 - the frozen epoch is no longer the live lease, which a compose
	// refuses before it looks at the root (a committed read never does).
	if req.EpochStale {
		return nil, ErrEpochStale
	}
	// :1575 - the root was cancelled.
	if req.RootStatus == cancelledRootStatus {
		return nil, ErrActionCancelled
	}
	// :1576 - a draft algorithm belongs to chapter drafts.
	if isDraftVersion(algorithm) && req.Operation != chapterDraftOperation {
		return nil, ErrAlgorithmInvalid
	}
	// generation-run-repository.ts:679 - only a visible-text run composes.
	if req.OutputContract != visibleTextContract {
		return nil, ErrOutputInvalid
	}
	if req.AlgorithmMalformed {
		return nil, ErrAlgorithmInvalid
	}
	next, err := visible(req, req.ArtifactIDs, algorithm, req.IDsMalformed)
	if err != nil {
		return nil, err
	}
	// :681 - previous is computed after next, and its own failure wins.
	previous, err := Read(req)
	if err != nil {
		return nil, err
	}
	if previous != nil && previous.Algorithm != algorithm {
		// :683 - an existing receipt's algorithm may not be replaced.
		return nil, ErrAlgorithmChanged
	}
	// :684 - the frozen expected hash, checked after the algorithm.
	if req.ExpectedHash != nil && next.TextHash != *req.ExpectedHash {
		return nil, ErrHashMismatch
	}
	if previous != nil {
		// :685 - the new selection has to extend the old one entry by entry; a
		// shorter or reordered selection is a regression even when the remaining
		// entries match.
		for index, artifactID := range previous.ArtifactIDs {
			current := ""
			if index < len(next.ArtifactIDs) {
				current = next.ArtifactIDs[index]
			}
			if current != artifactID {
				return nil, ErrRegression
			}
		}
	}
	return next, nil
}

// Read is compositionFrom (generation-run-repository.ts:659-674): a stored
// receipt is never trusted as bytes. It is recomposed from the ledger and only
// returned when the recomposition still reproduces it. A nil result means the run
// stores no composition yet, which the original answers as null.
func Read(req Request) (*Receipt, error) {
	if req.Stored == nil {
		return nil, nil
	}
	// The default parameter of visibleComposition applies to a stored receipt
	// whose algorithm is absent; the comparison below still sees the absent value,
	// which is why that case is INTEGRITY_FAILED rather than ALGORITHM_INVALID.
	algorithm := req.Stored.Algorithm
	if algorithm == "" {
		algorithm = VisibleContinuationVersion
	}
	current, err := visible(req, req.Stored.ArtifactIDs, algorithm, false)
	if err != nil {
		return nil, err
	}
	if req.Stored.Algorithm != current.Algorithm || req.Stored.TextHash != current.TextHash ||
		!sameSourceRefs(req.Stored.Sources, current.Sources) {
		return nil, ErrIntegrityFailed
	}
	return current, nil
}

// visible is the private visibleComposition (generation-run-repository.ts:460-513)
// for the algorithms this port computes. malformed carries the wire-shape verdict
// for the requested id list; a stored list is already structural.
func visible(req Request, artifactIDs []string, algorithm string, malformed bool) (*Receipt, error) {
	// :462 - an outline run composes under its own protocol, which is a sibling
	// slice. A run that names any other algorithm is refused with the same code.
	if req.PlotOutline || algorithm != VisibleContinuationVersion {
		return nil, ErrAlgorithmInvalid
	}
	// :466 - the selection is a non-empty, duplicate-free list of at most 32 ids.
	if malformed || len(artifactIDs) == 0 || len(artifactIDs) > maxCompositionSources {
		return nil, ErrInvalid
	}
	seen := make(map[string]bool, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		if artifactID == "" || seen[artifactID] {
			return nil, ErrInvalid
		}
		seen[artifactID] = true
	}
	text := ""
	previousOrdinal := int64(-1)
	sources := make([]SourceRef, 0, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		artifact, found := req.Lookup[artifactID]
		// :470-471 - the row has to exist, belong to this run, not be discarded and
		// arrive in ledger order.
		if !found || artifact.ArtifactID != artifactID || artifact.RunID != req.RunID ||
			artifact.Status == "discarded" || artifact.Ordinal <= previousOrdinal {
			return nil, ErrSourceInvalid
		}
		// :473 read.receipt(row.attempt_id) -> hydrateReceipt - the payload, the
		// usage identity and the frozen binding must agree, and the payload's hash
		// must still cover its text.
		if !artifact.Present || artifact.PayloadAttemptID != artifact.AttemptID ||
			artifact.RootActionID != req.RootActionID || !artifact.HasIdentity ||
			artifact.PayloadArtifactID != artifact.ArtifactID ||
			artifact.PayloadArtifactID != artifact.IdentityArtifactID ||
			artifact.Epoch != artifact.IdentityEpoch ||
			artifact.Fingerprint != artifact.IdentityFingerprint ||
			artifact.Fingerprint != artifact.BindingFingerprint ||
			artifact.ProjectID != artifact.BindingProjectID ||
			artifact.PayloadRevision != artifact.Revision ||
			TextHash(artifact.Text) != artifact.TextHash {
			return nil, ErrArtifactIntegrity
		}
		// :473 - the two reconciliation purposes are projections, not prose.
		if artifact.Purpose == draftReconcilePurpose || artifact.Purpose == draftShortOutlinePurpose {
			return nil, ErrSourceInvalid
		}
		// :474 - only a terminal attempt with real text can be composed.
		if (artifact.AttemptStatus != "settled" && artifact.AttemptStatus != "unknown") ||
			TrimVisible(artifact.Text) == "" {
			return nil, ErrSourceNotTerminal
		}
		// :475 - only a completed or length-stopped response is trusted.
		if artifact.FinishReason != "stop" && artifact.FinishReason != "length" {
			return nil, ErrSourceUntrusted
		}
		next := TrimVisible(artifact.Text)
		if len(sources) > 0 {
			next = ComposeVisible(text, artifact.Text)
		}
		// :496 - a source that contributes no characters is no progress, and
		// :503 so is a source that adds no letter or digit to the composition.
		if TrimVisible(next) == "" {
			return nil, ErrNoProgress
		}
		if len(sources) > 0 && countVisibleRunes(next) <= countVisibleRunes(text) {
			return nil, ErrNoProgress
		}
		text = next
		previousOrdinal = artifact.Ordinal
		sources = append(sources, SourceRef{
			ArtifactID: artifactID,
			Revision:   artifact.PayloadRevision,
			TextHash:   artifact.TextHash,
		})
	}
	return &Receipt{
		Algorithm:   algorithm,
		Text:        text,
		TextHash:    TextHash(text),
		ArtifactIDs: append([]string(nil), artifactIDs...),
		Sources:     sources,
	}, nil
}

// ComposeVisible is composeVisibleContinuation
// (src/shared/visible-continuation.ts:28-33): append the visible addition to the
// visible existing text, dropping the part of the addition that repeats the
// existing tail.
func ComposeVisible(existing, addition string) string {
	visibleExisting := jsTrim(jsUnits(existing))
	visibleAddition := jsTrim(jsUnits(addition))
	overlap := overlappingVisiblePrefixLength(visibleExisting, visibleAddition)
	next := removeLeadingNonWhitespace(visibleAddition, overlap)
	parts := make([][]uint16, 0, 2)
	if len(visibleExisting) > 0 {
		parts = append(parts, visibleExisting)
	}
	if len(next) > 0 {
		parts = append(parts, next)
	}
	return jsText(jsTrim(joinVisible(parts)))
}

// TrimVisible is String.prototype.trim as the original relies on it.
func TrimVisible(text string) string {
	return jsText(jsTrim(jsUnits(text)))
}

// IsPlotOutlineOperation reports whether a manifest describes an outline run
// (plotOutlinePolicy, src/shared/plot-outline-contract.ts:99-100).
func IsPlotOutlineOperation(operation string) bool {
	return operation == plotOutlineManifestOperation
}

// PlotOutlinePolicy mirrors the gate of plotOutlinePolicy
// (src/shared/plot-outline-contract.ts:99-113): a run composes under
// chapter-parts-v1 only when its frozen manifest carries the full architecture
// planning scope. The reference keys the protocol on that scope, not on the
// operation name — a generate-plot-outline manifest without policy.planning
// answers null there and composes as ordinary visible text. The reduced lane
// never freezes policy.planning (the ordinary binder refuses it), so every run
// it serves takes the ordinary path exactly like the reference would.
func PlotOutlinePolicy(manifest map[string]any) bool {
	if manifest == nil || manifest["operation"] != plotOutlineManifestOperation {
		return false
	}
	policy, _ := manifest["policy"].(map[string]any)
	planning, _ := policy["planning"].(map[string]any)
	intent, _ := planning["intent"].(map[string]any)
	outlineContent, _ := planning["outlineContent"].(map[string]any)
	rangeValue, _ := intent["synopsisRange"].(map[string]any)
	if planning["kind"] != "architecture" || planning["outlineProtocol"] != PlotOutlineProtocol ||
		intent == nil || outlineContent == nil || rangeValue == nil {
		return false
	}
	from, fromOK := rangeValue["from"].(float64)
	to, toOK := rangeValue["to"].(float64)
	return fromOK && toOK && from >= 1 && to >= from
}

// overlappingVisiblePrefixLength is overlappingVisiblePrefixLength
// (src/shared/visible-continuation.ts:15-24): the longest run of characters,
// ignoring all whitespace, that ends the existing tail and starts the addition,
// searched from the full window down to the 48-character floor.
func overlappingVisiblePrefixLength(existing, addition []uint16) int {
	tail := jsStrip(jsTail(existing, VisibleTailChars))
	head := jsStrip(jsHead(addition, VisibleTailChars))
	maximum := len(tail)
	if len(head) < maximum {
		maximum = len(head)
	}
	for length := maximum; length >= minVisibleOverlapChars; length-- {
		if equalUnits(tail[len(tail)-length:], head[:length]) {
			return length
		}
	}
	return 0
}

// removeLeadingNonWhitespace is removeLeadingNonWhitespaceCharacters
// (src/shared/visible-continuation.ts:5-13): drop the first count non-whitespace
// characters of text, plus the whitespace that follows them.
func removeLeadingNonWhitespace(text []uint16, count int) []uint16 {
	if count <= 0 {
		return text
	}
	consumed := 0
	for index := 0; index < len(text); index++ {
		if !jsIsSpace(text[index]) {
			consumed++
		}
		if consumed >= count {
			return jsTrimStart(text[index+1:])
		}
	}
	return nil
}

// joinVisible is Array.prototype.join with the "\n\n" separator the original
// uses; empty parts are already filtered by the caller.
func joinVisible(parts [][]uint16) []uint16 {
	separator := jsUnits("\n\n")
	total := 0
	for index, part := range parts {
		if index > 0 {
			total += len(separator)
		}
		total += len(part)
	}
	joined := make([]uint16, 0, total)
	for index, part := range parts {
		if index > 0 {
			joined = append(joined, separator...)
		}
		joined = append(joined, part...)
	}
	return joined
}

// jsIsSpace is the JavaScript \s class, code unit by code unit: the WhiteSpace
// and LineTerminator productions of the language, which are also the set
// String.prototype.trim removes. U+0085 is deliberately not a member (Go's
// unicode.IsSpace says it is) and U+FEFF deliberately is (Go's says it is not),
// so neither library helper can stand in for this table.
func jsIsSpace(unit uint16) bool {
	switch unit {
	case 0x0009, 0x000a, 0x000b, 0x000c, 0x000d, 0x0020,
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return unit >= 0x2000 && unit <= 0x200a
}

// jsUnits re-encodes a Go string as the UTF-16 code units the original indexes.
func jsUnits(text string) []uint16 {
	return utf16.Encode([]rune(text))
}

// jsText is the inverse. An unpaired surrogate - which a window boundary can
// create - decodes to U+FFFD, the same replacement the original's UTF-8 encoding
// performs, so the hashed bytes agree.
func jsText(units []uint16) string {
	return string(utf16.Decode(units))
}

// jsTrim is String.prototype.trim over code units. Only whitespace units are
// removed, and no whitespace code point is a surrogate, so trimming units and
// trimming code points are the same operation.
func jsTrim(units []uint16) []uint16 {
	end := len(units)
	for end > 0 && jsIsSpace(units[end-1]) {
		end--
	}
	start := 0
	for start < end && jsIsSpace(units[start]) {
		start++
	}
	return units[start:end]
}

// jsTrimStart is String.prototype.trimStart over code units.
func jsTrimStart(units []uint16) []uint16 {
	start := 0
	for start < len(units) && jsIsSpace(units[start]) {
		start++
	}
	return units[start:]
}

// jsStrip is String.prototype.replace with /\s+/gu and an empty replacement.
func jsStrip(units []uint16) []uint16 {
	stripped := make([]uint16, 0, len(units))
	for _, unit := range units {
		if !jsIsSpace(unit) {
			stripped = append(stripped, unit)
		}
	}
	return stripped
}

// jsTail is String.prototype.slice(-count).
func jsTail(units []uint16, count int) []uint16 {
	if count <= 0 || len(units) == 0 {
		return nil
	}
	if count >= len(units) {
		return units
	}
	return units[len(units)-count:]
}

// jsHead is String.prototype.slice(0, count).
func jsHead(units []uint16, count int) []uint16 {
	if count <= 0 || len(units) == 0 {
		return nil
	}
	if count >= len(units) {
		return units
	}
	return units[:count]
}

// equalUnits is string equality over code units.
func equalUnits(left, right []uint16) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// sameSourceRefs is isDeepStrictEqual over two source lists.
func sameSourceRefs(left, right []SourceRef) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// isDraftVersion is isDraftVisibleTextVersion (src/shared/draft-visible-text.ts:5-7).
func isDraftVersion(algorithm string) bool {
	return algorithm == draftVisibleOne || algorithm == draftVisibleTwo
}

// countVisibleRunes counts what the original counts with /[\p{L}\p{N}]/gu
// (generation-run-repository.ts:503): one per code point in the Unicode general
// category Letter or Number.
func countVisibleRunes(text string) int {
	count := 0
	for _, symbol := range text {
		if unicode.IsLetter(symbol) || unicode.IsNumber(symbol) {
			count++
		}
	}
	return count
}

// CanonicalJSON is the comparison key for the three frozen fingerprint objects: a
// decoded value re-encoded with object keys sorted, so two payloads that differ
// only in key order - which isDeepStrictEqual ignores - produce the same key.
func CanonicalJSON(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
