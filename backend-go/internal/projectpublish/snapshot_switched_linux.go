//go:build linux

package projectpublish

import (
	"context"
	"os"
	"path/filepath"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// SwitchedObservation identifies the new visible storage at one instant. It
// grants neither publication nor re-admission authority; a runtime must keep
// its handle-drain barrier and independently open the new source.
type SwitchedObservation struct {
	NewSourcePath     string
	OldSourcePath     string
	NewSourceDevice   uint64
	NewSourceInode    uint64
	NewSourceTreeHash string
}

// Abandon closes the snapshot capability without deleting either directory.
// Use it after an uncertain or failed post-swap observation so a generic
// Close cannot mistake the old source for a disposable stage. It deliberately
// does not claim that an exchange was durable or safe to re-admit.
func (s *Snapshot) Abandon() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.parent == nil {
		return ErrInvalid
	}
	err := s.parent.Close()
	s.parent = nil
	s.closed = true
	return err
}

// RetainSwitched observes an exchange performed by a separate, journaled
// publisher. It checks that both pinned directory inodes exchanged places,
// that the old source bytes survived at stageName, and that the new visible
// tree matches the expected edited-tree digest. Only after those checks does
// Close stop treating stageName as a disposable private copy. A failed check
// never removes either tree.
func (s *Snapshot) RetainSwitched(ctx context.Context, expectedEditedTreeHash string) (*SwitchedObservation, error) {
	if s == nil || ctx == nil || !validJournalHash(expectedEditedTreeHash) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.removed || s.parent == nil || s.lock == nil {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.lock.VerifyLegacy(s.dataRoot); err != nil {
		return nil, err
	}
	parentNow, err := privateDirectory(s.parentPath)
	if err != nil || !os.SameFile(parentNow, s.parentInfo) {
		return nil, ErrChanged
	}
	projectRootNow, err := os.Lstat(s.projectPath)
	if err != nil || !os.SameFile(projectRootNow, s.projectRootInfo) {
		return nil, ErrChanged
	}
	oldPath := filepath.Join(s.parentPath, s.stageName)
	oldNow, err := privateDirectory(oldPath)
	if err != nil || !os.SameFile(oldNow, s.storageInfo) {
		return nil, ErrChanged
	}
	newPath := filepath.Join(s.projectPath, projectfs.StorageDirectory)
	newNow, err := privateDirectory(newPath)
	if err != nil || !os.SameFile(newNow, s.stageInfo) {
		return nil, ErrChanged
	}
	oldRoot, err := s.parent.OpenRoot(s.stageName)
	if err != nil {
		return nil, ErrChanged
	}
	oldOpened, oldStatErr := oldRoot.Stat(".")
	if oldStatErr != nil || !os.SameFile(oldOpened, s.storageInfo) {
		_ = oldRoot.Close()
		return nil, ErrChanged
	}
	oldEntries, oldScanErr := scan(ctx, oldRoot, s.limits)
	oldCloseErr := oldRoot.Close()
	if oldScanErr != nil {
		return nil, oldScanErr
	}
	if oldCloseErr != nil {
		return nil, oldCloseErr
	}
	if !sameEntries(oldEntries, s.entries) {
		return nil, ErrChanged
	}
	newRoot, err := os.OpenRoot(newPath)
	if err != nil {
		return nil, ErrChanged
	}
	newOpened, newStatErr := newRoot.Stat(".")
	if newStatErr != nil || !os.SameFile(newOpened, s.stageInfo) {
		_ = newRoot.Close()
		return nil, ErrChanged
	}
	newEntries, newScanErr := scan(ctx, newRoot, s.limits)
	newCloseErr := newRoot.Close()
	if newScanErr != nil {
		return nil, newScanErr
	}
	if newCloseErr != nil {
		return nil, newCloseErr
	}
	if hashTreeEntries(newEntries) != expectedEditedTreeHash {
		return nil, ErrChanged
	}
	oldAfter, oldErr := s.parent.Lstat(s.stageName)
	newAfter, newErr := os.Lstat(newPath)
	if oldErr != nil || newErr != nil || !os.SameFile(oldAfter, s.storageInfo) || !os.SameFile(newAfter, s.stageInfo) {
		return nil, ErrChanged
	}
	project, err := s.library.OpenProject(s.projectPath)
	if err != nil {
		return nil, err
	}
	// Storage and manifest inodes intentionally change on exchange, so the
	// pre-swap projectfs.Identity cannot be reused for the new admission.
	if project.ProjectID() != s.projectID {
		_ = project.Close()
		return nil, ErrChanged
	}
	verifyErr := project.VerifyNoPortableAuthority(ctx)
	closeErr := project.Close()
	if verifyErr != nil {
		return nil, verifyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	newFinal, err := os.Lstat(newPath)
	if err != nil || !os.SameFile(newFinal, s.stageInfo) {
		return nil, ErrChanged
	}
	projectRootFinal, rootErr := os.Lstat(s.projectPath)
	parentFinal, parentErr := privateDirectory(s.parentPath)
	oldFinal, oldErr := os.Lstat(oldPath)
	if rootErr != nil || parentErr != nil || oldErr != nil ||
		!os.SameFile(projectRootFinal, s.projectRootInfo) ||
		!os.SameFile(parentFinal, s.parentInfo) || !os.SameFile(oldFinal, s.storageInfo) {
		return nil, ErrChanged
	}
	if err := s.lock.VerifyLegacy(s.dataRoot); err != nil {
		return nil, err
	}
	newID, ok := inodeOf(s.stageInfo)
	if !ok {
		return nil, ErrInvalid
	}
	s.switched = true
	return &SwitchedObservation{NewSourcePath: newPath, OldSourcePath: oldPath,
		NewSourceDevice: newID.Device, NewSourceInode: newID.Number,
		NewSourceTreeHash: expectedEditedTreeHash}, nil
}
