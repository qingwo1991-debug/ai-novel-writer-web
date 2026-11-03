//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// AbortPreparedJournal deliberately retires an unchanged PREPARED candidate.
// It never touches the source tree. The caller must retain the legacy writer
// lock and must not acknowledge a save from this result. Every interrupted
// deletion before the final parent sync leaves the PREPARED marker or an
// unrecognized quarantine entry; a crash that loses both has completed the
// unchanged-source abort. It never guesses that a partly removed stage is
// disposable.
func AbortPreparedJournal(ctx context.Context, held *writerlock.Lock, dataRoot, projectID, projectPath, stageParent, name string) (retErr error) {
	if ctx == nil || held == nil || !validJournalName(name) || projectID == "" ||
		!canonical(dataRoot) || !canonical(projectPath) || !canonical(stageParent) {
		return ErrJournalInvalid
	}
	recovery, err := RecoverExchangeJournal(ctx, held, dataRoot, projectID, projectPath, stageParent, name)
	if err != nil || recovery.Class != JournalPrepared || !recovery.PreparedMarked || recovery.SwitchedMarked {
		return errors.Join(ErrChanged, err)
	}
	parentInfo, err := privateDirectory(stageParent)
	if err != nil {
		return ErrChanged
	}
	root, openedParent, err := openPinnedDirectory(stageParent, nil)
	if err != nil || !os.SameFile(parentInfo, openedParent) {
		if root != nil {
			_ = root.Close()
		}
		return ErrChanged
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	raw, err := readJournalFile(root, name)
	if err != nil {
		return err
	}
	var prepared journalPrepared
	if err := strictJournalJSON(raw, &prepared); err != nil ||
		!validPrepared(prepared, dataRoot, projectID, projectPath, stageParent, name) ||
		recovery.JournalID != prepared.ID || recovery.StagePath != filepath.Join(stageParent, prepared.StageName) {
		return ErrJournalInvalid
	}
	projectRoot, projectInfo, err := openPinnedDirectory(projectPath, nil)
	if err != nil {
		return ErrChanged
	}
	defer projectRoot.Close()
	markerInfo, err := root.Lstat(name)
	if err != nil || !markerInfo.Mode().IsRegular() || !oneLink(markerInfo) {
		return ErrChanged
	}
	if err := abortPreparedVerify(ctx, held, root, openedParent, prepared, name, raw); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	quarantine := "novel-abort-" + hex.EncodeToString(nonce[:])
	if err := root.Mkdir(quarantine, 0700); err != nil {
		return err
	}
	// After this point every uncertain error preserves the original marker and
	// the quarantine directory. A restart will fail closed on that extra entry.
	quarantineRoot, err := root.OpenRoot(quarantine)
	if err != nil {
		return err
	}
	quarantineInfo, err := quarantineRoot.Stat(".")
	if err != nil {
		_ = quarantineRoot.Close()
		return err
	}
	quarantineOpen := true
	defer func() {
		if quarantineOpen {
			_ = quarantineRoot.Close()
		}
	}()
	if err := root.Rename(prepared.StageName, quarantine+"/stage"); err != nil {
		return err
	}
	movedStage, err := quarantineRoot.Stat("stage")
	movedID, ok := inodeOf(movedStage)
	if err != nil || !ok || movedID != prepared.Stage {
		return ErrChanged
	}
	if err := abortPreparedSource(ctx, held, prepared); err != nil {
		return err
	}
	if !samePinnedJournalProject(projectPath, projectInfo) {
		return ErrChanged
	}
	stageRoot, err := quarantineRoot.OpenRoot("stage")
	if err != nil {
		return err
	}
	stageEntries, err := scan(ctx, stageRoot, Limits{MaxFiles: prepared.MaxFiles, MaxBytes: prepared.MaxBytes, MaxDepth: prepared.MaxDepth})
	if err != nil || hashTreeEntries(stageEntries) != prepared.StageTreeHash {
		_ = stageRoot.Close()
		return errors.Join(ErrChanged, err)
	}
	// Delete only through the descriptor pinned to the candidate inode. A
	// replacement of the quarantine pathname cannot redirect recursion into
	// the old author source. The entry scan is bounded by the PREPARED limits.
	for i := len(stageEntries) - 1; i >= 0; i-- {
		if err := stageRoot.Remove(stageEntries[i].path); err != nil {
			_ = stageRoot.Close()
			return err
		}
	}
	if err := stageRoot.Close(); err != nil {
		return err
	}
	stageAfter, err := quarantineRoot.Lstat("stage")
	if err != nil || !os.SameFile(stageAfter, movedStage) {
		return ErrChanged
	}
	if err := quarantineRoot.Remove("stage"); err != nil {
		return err
	}
	if err := errors.Join(syncRoot(quarantineRoot), syncRoot(root)); err != nil {
		return err
	}
	if err := abortPreparedSource(ctx, held, prepared); err != nil {
		return err
	}
	if !samePinnedJournalProject(projectPath, projectInfo) {
		return ErrChanged
	}
	if _, err := root.Lstat(prepared.StageName); !errors.Is(err, fs.ErrNotExist) {
		return ErrChanged
	}
	if err := abortPreparedMarker(root, openedParent, prepared, name, raw, markerInfo); err != nil {
		return err
	}
	if err := root.Rename(name, quarantine+"/prepared"); err != nil {
		return err
	}
	movedMarker, err := quarantineRoot.Lstat("prepared")
	if err != nil || !os.SameFile(movedMarker, markerInfo) {
		return ErrChanged
	}
	movedRaw, err := readJournalFile(quarantineRoot, "prepared")
	if err != nil || !bytes.Equal(movedRaw, raw) {
		return ErrChanged
	}
	if err := quarantineRoot.Remove("prepared"); err != nil {
		return err
	}
	if err := errors.Join(syncRoot(quarantineRoot), syncRoot(root)); err != nil {
		return err
	}
	if err := abortPreparedSource(ctx, held, prepared); err != nil {
		return err
	}
	if !samePinnedJournalProject(projectPath, projectInfo) {
		return ErrChanged
	}
	parentBeforeRetire, err := privateDirectory(stageParent)
	if err != nil || !os.SameFile(parentBeforeRetire, openedParent) {
		return ErrChanged
	}
	if err := quarantineRoot.Close(); err != nil {
		return err
	}
	quarantineOpen = false
	quarantineFinal, err := root.Lstat(quarantine)
	if err != nil || !os.SameFile(quarantineFinal, quarantineInfo) {
		return ErrChanged
	}
	if err := root.Remove(quarantine); err != nil {
		return err
	}
	if err := syncRoot(root); err != nil {
		return err
	}
	if _, err := root.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
		return ErrChanged
	}
	if _, err := root.Lstat(name + ".switched"); !errors.Is(err, fs.ErrNotExist) {
		return ErrChanged
	}
	if _, err := root.Lstat(prepared.StageName); !errors.Is(err, fs.ErrNotExist) {
		return ErrChanged
	}
	if _, err := root.Lstat(quarantine); !errors.Is(err, fs.ErrNotExist) {
		return ErrChanged
	}
	parentFinal, err := privateDirectory(stageParent)
	if err != nil || !os.SameFile(parentFinal, openedParent) {
		return ErrChanged
	}
	return nil
}

func abortPreparedSource(ctx context.Context, held *writerlock.Lock, prepared journalPrepared) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := held.VerifyLegacy(prepared.DataRoot); err != nil {
		return err
	}
	id, digest, err := inspectJournalTree(ctx, filepath.Join(prepared.ProjectPath, projectfs.StorageDirectory),
		Limits{MaxFiles: prepared.MaxFiles, MaxBytes: prepared.MaxBytes, MaxDepth: prepared.MaxDepth})
	if err != nil || id != prepared.Source || digest != prepared.SourceTreeHash {
		return errors.Join(ErrChanged, err)
	}
	return nil
}

func abortPreparedMarker(root *os.Root, parent fs.FileInfo, prepared journalPrepared, name string, raw []byte, marker fs.FileInfo) error {
	currentParent, err := privateDirectory(prepared.StageParent)
	if err != nil || !os.SameFile(parent, currentParent) {
		return ErrChanged
	}
	currentMarker, err := root.Lstat(name)
	if err != nil || !os.SameFile(marker, currentMarker) {
		return ErrChanged
	}
	currentRaw, err := readJournalFile(root, name)
	if err != nil || !bytes.Equal(currentRaw, raw) {
		return ErrChanged
	}
	if _, err := root.Lstat(name + ".switched"); !errors.Is(err, fs.ErrNotExist) {
		return ErrChanged
	}
	return nil
}

func abortPreparedVerify(ctx context.Context, held *writerlock.Lock, root *os.Root, parent fs.FileInfo,
	prepared journalPrepared, name string, raw []byte) error {
	if err := abortPreparedSource(ctx, held, prepared); err != nil {
		return err
	}
	stageID, stageHash, err := inspectJournalTree(ctx, filepath.Join(prepared.StageParent, prepared.StageName),
		Limits{MaxFiles: prepared.MaxFiles, MaxBytes: prepared.MaxBytes, MaxDepth: prepared.MaxDepth})
	if err != nil || stageID != prepared.Stage || stageHash != prepared.StageTreeHash {
		return errors.Join(ErrChanged, err)
	}
	marker, err := root.Lstat(name)
	if err != nil {
		return err
	}
	return abortPreparedMarker(root, parent, prepared, name, raw, marker)
}
