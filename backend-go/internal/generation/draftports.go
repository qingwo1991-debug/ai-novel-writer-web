package generation

// The draft text algorithms live in internal/draftalg, a leaf package with no
// dependencies beyond the standard library, because internal/store needs them
// for generation:commit-draft and internal/generation's own tests import
// internal/store: keeping the implementations here would make that pair an
// import cycle in the test binary. These names re-export the leaf so every
// existing caller in this package and in the transport layer keeps compiling
// against the same API, and there is still exactly one implementation.

import (
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
)

const (
	// DraftUnitAlgorithmVersion is DRAFT_UNIT_ALGORITHM_VERSION (draft-units.ts:3).
	DraftUnitAlgorithmVersion = draftalg.DraftUnitAlgorithmVersion
	// DraftVisibleTextVersionV1 is the legacy projection (draft-visible-text.ts:4).
	DraftVisibleTextVersionV1 = draftalg.DraftVisibleTextVersionV1
	// DraftVisibleTextVersion is DRAFT_VISIBLE_TEXT_VERSION (draft-visible-text.ts:3).
	DraftVisibleTextVersion = draftalg.DraftVisibleTextVersion
	// DraftCondensePurpose is DRAFT_CONDENSE_PURPOSE (draft-visible-text.ts:9).
	DraftCondensePurpose = draftalg.DraftCondensePurpose
	// VisibleContinuationVersion is VISIBLE_CONTINUATION_VERSION (visible-continuation.ts:1).
	VisibleContinuationVersion = draftalg.VisibleContinuationVersion
	// ContinuationVisibleTailChars is the overlap window (visible-continuation.ts:2).
	ContinuationVisibleTailChars = draftalg.ContinuationVisibleTailChars
)

// ErrDraftTargetUnitsInvalid is DRAFT_TARGET_UNITS_INVALID (draft-units.ts:6).
var ErrDraftTargetUnitsInvalid = draftalg.ErrDraftTargetUnitsInvalid

// DraftUnitRange is the [minimum, maximum] window a target allows.
type DraftUnitRange = draftalg.DraftUnitRange

// DraftTargetUnitRange is draftTargetUnitRange (draft-units.ts:22-25).
func DraftTargetUnitRange(targetUnits int64) (DraftUnitRange, error) {
	return draftalg.DraftTargetUnitRange(targetUnits)
}

// ParseDraftTargetUnits is parseDraftTargetUnits (draft-units.ts:27-46).
func ParseDraftTargetUnits(text string) (int64, bool) { return draftalg.ParseDraftTargetUnits(text) }

// DraftTargetUnitsUsable is draftTargetUnitsUsable (draft-units.ts:48-50).
func DraftTargetUnitsUsable(targetUnits int64) bool {
	return draftalg.DraftTargetUnitsUsable(targetUnits)
}

// CountDraftUnits is countDraftUnits (draft-units.ts:52-140).
func CountDraftUnits(text string) int { return draftalg.CountDraftUnits(text) }

// CountLegacyDraftUnitsV1 is countLegacyDraftUnitsV1 (draft-units.ts:142-158).
func CountLegacyDraftUnitsV1(text string) int { return draftalg.CountLegacyDraftUnitsV1(text) }

// IsDraftVisibleTextVersion reports whether a receipt names a knowable projection.
func IsDraftVisibleTextVersion(value string) bool { return draftalg.IsDraftVisibleTextVersion(value) }

// StripDraftThinkingTags is stripDraftThinkingTags (draft-visible-text.ts:52-66).
func StripDraftThinkingTags(text string) string { return draftalg.StripDraftThinkingTags(text) }

// SanitizeDraftText is sanitizeDraftText (draft-visible-text.ts:68-84).
func SanitizeDraftText(text string, version ...string) string {
	return draftalg.SanitizeDraftText(text, version...)
}

// ComposeDraftVisibleContinuation is composeDraftVisibleContinuation
// (draft-visible-text.ts:86-101).
func ComposeDraftVisibleContinuation(existing, addition string, version ...string) string {
	return draftalg.ComposeDraftVisibleContinuation(existing, addition, version...)
}

// ComposeDraftVisibleSources is composeDraftVisibleSources
// (draft-visible-text.ts:103-127).
func ComposeDraftVisibleSources(texts []string, version ...string) (string, string) {
	return draftalg.ComposeDraftVisibleSources(texts, version...)
}

// TextHash is textHash (draft-visible-text.ts:129-131).
func TextHash(text string) string { return draftalg.TextHash(text) }

// ComposeVisibleContinuation is composeVisibleContinuation
// (visible-continuation.ts:5-22).
func ComposeVisibleContinuation(existing, addition string) string {
	return draftalg.ComposeVisibleContinuation(existing, addition)
}
