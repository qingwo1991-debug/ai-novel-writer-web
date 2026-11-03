//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// ConfirmSwitchedAfterRestart finishes only an exchange already visible at
// both paths. It never exchanges directories or acknowledges an author save.
// The exact PREPARED journal must classify as switched-unconfirmed under a
// continuously held legacy lock. Failure after marker creation leaves that
// marker in place for conservative restart classification.
func ConfirmSwitchedAfterRestart(ctx context.Context, held *writerlock.Lock,
	dataRoot, projectID, projectPath, stageParent, name string) (result JournalRecovery, retErr error) {
	if ctx == nil || held == nil || projectID == "" || !validJournalName(name) ||
		!canonical(dataRoot) || !canonical(projectPath) || !canonical(stageParent) || !childOf(dataRoot, projectPath) {
		return result, ErrJournalInvalid
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return result, err
	}
	initial, err := RecoverExchangeJournal(ctx, held, dataRoot, projectID, projectPath, stageParent, name)
	if err != nil || initial.Class != JournalSwitchedUnconfirmed || initial.SwitchedMarked ||
		initial.EditedDraftID <= 0 || !validJournalHash(initial.EditedOperationHash) || !validJournalHash(initial.EditedProofHash) {
		return result, errors.Join(ErrChanged, err)
	}
	projectRoot, projectInfo, err := openPinnedDirectory(projectPath, nil)
	if err != nil {
		return result, ErrChanged
	}
	defer func() { retErr = errors.Join(retErr, projectRoot.Close()) }()
	storagePath := filepath.Join(projectPath, projectfs.StorageDirectory)
	if stageParent == storagePath || childOf(storagePath, stageParent) || childOf(stageParent, storagePath) {
		return result, ErrJournalInvalid
	}
	parentInfo, err := privateDirectory(stageParent)
	if err != nil {
		return result, ErrChanged
	}
	visibleSource, err := os.Lstat(storagePath)
	if err != nil || !visibleSource.IsDir() || visibleSource.Mode()&os.ModeSymlink != 0 {
		return result, ErrChanged
	}
	parent, openedParent, err := openPinnedDirectory(stageParent, visibleSource)
	if err != nil || !os.SameFile(parentInfo, openedParent) {
		if parent != nil {
			_ = parent.Close()
		}
		return result, ErrChanged
	}
	defer func() { retErr = errors.Join(retErr, parent.Close()) }()
	raw, err := readJournalFile(parent, name)
	if err != nil {
		return result, err
	}
	var prepared journalPrepared
	if err := strictJournalJSON(raw, &prepared); err != nil ||
		!validPrepared(prepared, dataRoot, projectID, projectPath, stageParent, name) ||
		prepared.ID != initial.JournalID || filepath.Join(stageParent, prepared.StageName) != initial.StagePath ||
		prepared.EditedDraftID != initial.EditedDraftID || prepared.EditedOperationHash != initial.EditedOperationHash ||
		prepared.EditedProofHash != initial.EditedProofHash {
		return result, ErrJournalInvalid
	}
	parentID, ok := inodeOf(openedParent)
	if !ok || parentID != prepared.Parent {
		return result, ErrChanged
	}
	if err := verifyUnconfirmedRestart(ctx, held, parent, openedParent, projectInfo, prepared, name, raw); err != nil {
		return result, err
	}
	// Persist the two namespace parents before claiming that the already
	// exchanged names survived a crash. Neither sync changes source contents.
	if err := errors.Join(syncRoot(projectRoot), syncRoot(parent)); err != nil {
		return result, err
	}
	if err := verifyUnconfirmedRestart(ctx, held, parent, openedParent, projectInfo, prepared, name, raw); err != nil {
		return result, err
	}
	marker, err := json.Marshal(journalSwitched{Format: 1, PreparedHash: journalDigest(raw)})
	if err != nil {
		return result, err
	}
	if err := writeJournalFile(parent, name+".switched", marker); err != nil {
		// A short write or failed sync may have left a marker. Never interpret
		// the current process's observation as durable success after that.
		return result, err
	}
	confirmed, err := RecoverExchangeJournal(ctx, held, dataRoot, projectID, projectPath, stageParent, name)
	if err != nil || confirmed.Class != JournalSwitched || !confirmed.SwitchedMarked ||
		confirmed.JournalID != initial.JournalID || confirmed.StagePath != initial.StagePath ||
		confirmed.EditedDraftID != initial.EditedDraftID || confirmed.EditedOperationHash != initial.EditedOperationHash ||
		confirmed.EditedProofHash != initial.EditedProofHash ||
		confirmed.VerifiedNewSourceTreeHash != prepared.StageTreeHash ||
		confirmed.NewSourceDevice != prepared.Stage.Device || confirmed.NewSourceInode != prepared.Stage.Number {
		return result, errors.Join(ErrChanged, err)
	}
	return confirmed, nil
}

func verifyUnconfirmedRestart(ctx context.Context, held *writerlock.Lock, parent *os.Root,
	parentInfo, projectInfo fs.FileInfo, prepared journalPrepared, name string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := held.VerifyLegacy(prepared.DataRoot); err != nil {
		return err
	}
	currentParent, err := privateDirectory(prepared.StageParent)
	if err != nil || !os.SameFile(currentParent, parentInfo) ||
		!samePinnedJournalProject(prepared.ProjectPath, projectInfo) {
		return ErrChanged
	}
	currentRaw, err := readJournalFile(parent, name)
	if err != nil || !bytes.Equal(currentRaw, raw) {
		return ErrChanged
	}
	if _, err := parent.Lstat(name + ".switched"); !errors.Is(err, fs.ErrNotExist) {
		return ErrChanged
	}
	limits := Limits{MaxFiles: prepared.MaxFiles, MaxBytes: prepared.MaxBytes, MaxDepth: prepared.MaxDepth}
	sourcePath := filepath.Join(prepared.ProjectPath, projectfs.StorageDirectory)
	stagePath := filepath.Join(prepared.StageParent, prepared.StageName)
	sourceID, sourceHash, sourceErr := inspectJournalTree(ctx, sourcePath, limits)
	stageID, stageHash, stageErr := inspectJournalTree(ctx, stagePath, limits)
	if sourceErr != nil || stageErr != nil || sourceID != prepared.Stage || stageID != prepared.Source ||
		sourceHash != prepared.StageTreeHash || stageHash != prepared.SourceTreeHash {
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrChanged
	}
	// Recheck pathnames after both full scans; the lock excludes cooperative
	// writers, while an unexpected replacement remains an explicit refusal.
	sourceFinal, sourceErr := os.Lstat(sourcePath)
	stageFinal, stageErr := os.Lstat(stagePath)
	finalSourceID, sourceOK := inodeOf(sourceFinal)
	finalStageID, stageOK := inodeOf(stageFinal)
	finalParent, parentErr := privateDirectory(prepared.StageParent)
	finalRaw, rawErr := readJournalFile(parent, name)
	if sourceErr != nil || stageErr != nil || parentErr != nil || rawErr != nil ||
		!sourceOK || !stageOK || finalSourceID != sourceID || finalStageID != stageID ||
		!os.SameFile(finalParent, parentInfo) || !samePinnedJournalProject(prepared.ProjectPath, projectInfo) ||
		!bytes.Equal(finalRaw, raw) {
		return ErrChanged
	}
	if _, err := parent.Lstat(name + ".switched"); !errors.Is(err, fs.ErrNotExist) {
		return ErrChanged
	}
	return held.VerifyLegacy(prepared.DataRoot)
}
