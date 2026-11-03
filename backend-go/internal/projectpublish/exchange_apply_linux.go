//go:build linux

package projectpublish

import "context"

// ApplyEditedExchange is the publication boundary for an edited private stage.
//
// A PREPARED journal and an ExchangePlan do not confer publication authority.
// The plan describes the unedited snapshot, so Verify rejects the edited stage.
// HandleDrain currently accepts an arbitrary callback and cannot keep every
// SQLite, Lance, and project handle closed across a namespace switch. There is
// also no author-runtime re-admission token for the new storage inode. Until
// those capabilities exist, this boundary must not exchange directory names,
// fsync a claimed switch, or mark the journal SWITCHED.
func ApplyEditedExchange(ctx context.Context, plan *ExchangePlan, journal *ExchangeJournal) error {
	if ctx == nil || plan == nil || plan.snapshot == nil || journal == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if journal.prepared.EditedDraftID <= 0 ||
		!validJournalHash(journal.prepared.EditedOperationHash) ||
		!validJournalHash(journal.prepared.EditedProofHash) {
		return ErrJournalInvalid
	}
	return ErrPublishUnavailable
}
