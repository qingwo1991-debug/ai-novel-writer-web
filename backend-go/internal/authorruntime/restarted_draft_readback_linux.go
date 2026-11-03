//go:build linux

package authorruntime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectpublish"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// VerifyRestartedSwitchedDraftUnderLock verifies a persisted edited draft
// exchange after process restart. It needs no in-memory Snapshot or catalog
// key: the catalog remains fenced while recovery is checked. The journal must
// already be durably SWITCHED and both trees must still match its PREPARED
// record. It returns only readback facts, never a save ACK or catalog clearance.
// The caller owns library and held. A cleanup failure retains the writer lock
// and returns the Reader for diagnosis; process restart is then required.
func VerifyRestartedSwitchedDraftUnderLock(ctx context.Context, held *writerlock.Lock,
	library *projectfs.Library, projectID, projectPath, stageParent, journalName string,
	config projectaccess.Config) (result SwitchedDraftReadback, retErr error) {
	if ctx == nil || held == nil || library == nil || projectID == "" ||
		config.ProjectPath != projectPath || config.LibraryRoot != library.Path() ||
		filepath.Dir(projectPath) != library.Path() {
		return result, ErrSwitchedDraftUnverified
	}
	if err := held.VerifyLegacy(config.DataRoot); err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	recovery, err := projectpublish.RecoverExchangeJournal(ctx, held, config.DataRoot,
		projectID, projectPath, stageParent, journalName)
	if err != nil || recovery.Class != projectpublish.JournalSwitched ||
		recovery.EditedDraftID <= 0 || recovery.EditedDraftID > switchedDraftMaxSafeInteger ||
		!readbackHashValid(recovery.EditedOperationHash) ||
		!readbackHashValid(recovery.EditedProofHash) ||
		!readbackHashValid(recovery.VerifiedNewSourceTreeHash) ||
		recovery.NewSourceDevice == 0 || recovery.NewSourceInode == 0 {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	project, err := library.OpenProject(projectPath)
	if err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	if project.ProjectID() != projectID || !restartedProjectMatchesRecovery(project, recovery) {
		_ = project.Close()
		return result, ErrSwitchedDraftUnverified
	}
	reader, err := openReadOnlyPinnedProjectUnderLock(ctx, held, project, config)
	if err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			retainErr := held.Retain()
			result = SwitchedDraftReadback{}
			retErr = errors.Join(retErr, ErrSwitchedDraftUnverified,
				&SwitchedReadbackCleanupError{Reader: reader, Cause: err}, retainErr)
		}
	}()
	report, err := reader.Report()
	if err != nil || report.ProjectID != projectID || !report.LegacyGatewayExcluded {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	var body string
	var words int64
	err = reader.Read(ctx, func(tx *sql.Tx) error {
		var rawBody, rawWords any
		if err := tx.QueryRowContext(ctx, `SELECT c.body, d.word_count
 FROM drafts d JOIN contents c ON c.id = d.content_id WHERE d.id = ?`, recovery.EditedDraftID).Scan(&rawBody, &rawWords); err != nil {
			return err
		}
		var ok bool
		body, ok = rawBody.(string)
		if !ok || !utf8.ValidString(body) || len(body) > 16<<20 {
			return ErrSwitchedDraftUnverified
		}
		words, ok = rawWords.(int64)
		if !ok || words < 0 || words > switchedDraftMaxSafeInteger {
			return ErrSwitchedDraftUnverified
		}
		return nil
	})
	if err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	decoded, err := hex.DecodeString(recovery.VerifiedNewSourceTreeHash)
	if err != nil || len(decoded) != sha256.Size {
		return result, ErrSwitchedDraftUnverified
	}
	var treeDigest [sha256.Size]byte
	copy(treeDigest[:], decoded)
	proof := switchedDraftReadbackHash(recovery.EditedDraftID, words, body, treeDigest)
	if hex.EncodeToString(proof[:]) != recovery.EditedProofHash {
		return result, ErrSwitchedDraftUnverified
	}
	again, err := projectpublish.RecoverExchangeJournal(ctx, held, config.DataRoot,
		projectID, projectPath, stageParent, journalName)
	if err != nil || again != recovery {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	if err := held.VerifyLegacy(config.DataRoot); err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	contentDigest := sha256.Sum256([]byte(body))
	return SwitchedDraftReadback{ProjectID: projectID, ProjectPath: projectPath,
		DraftID: recovery.EditedDraftID, WordCount: words,
		ContentSHA256:   hex.EncodeToString(contentDigest[:]),
		EditedTreeHash:  recovery.VerifiedNewSourceTreeHash,
		ReadbackSHA256:  recovery.EditedProofHash,
		NewSourceDevice: recovery.NewSourceDevice,
		NewSourceInode:  recovery.NewSourceInode}, nil
}

// Compare the journal's verified visible-source inode with the inode pinned
// by OpenProject itself. A pathname Lstat would not bind the private Reader to
// the same tree if a same-ID project were substituted between observations.
func restartedProjectMatchesRecovery(project *projectfs.Project, recovery projectpublish.JournalRecovery) bool {
	if project == nil || recovery.Class != projectpublish.JournalSwitched ||
		recovery.NewSourceDevice == 0 || recovery.NewSourceInode == 0 {
		return false
	}
	device, inode, err := project.StorageIdentity()
	return err == nil && device == recovery.NewSourceDevice && inode == recovery.NewSourceInode
}
