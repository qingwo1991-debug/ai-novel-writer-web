//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// JournalTarget identifies the project named by a strictly validated PREPARED
// marker. It is an observation only: a caller must still classify the full
// journal and both trees before treating this target as unrelated to a project.
type JournalTarget struct {
	ProjectID    string
	ProjectPath  string
	StagePath    string
	PreparedHash string
}

// InspectPreparedJournalTarget reads a marker through a pinned private parent
// while the legacy writer lock is continuously held. It does not authorize a
// source exchange, recovery clearance, or removal of the marker.
func InspectPreparedJournalTarget(ctx context.Context, held *writerlock.Lock, dataRoot, stageParent, name string) (target JournalTarget, retErr error) {
	var zero JournalTarget
	if ctx == nil || held == nil || !validJournalName(name) || !canonical(dataRoot) || !canonical(stageParent) {
		return zero, ErrJournalInvalid
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return zero, err
	}
	parentInfo, err := privateDirectory(stageParent)
	if err != nil {
		return zero, ErrJournalInvalid
	}
	root, opened, err := openPinnedDirectory(stageParent, nil)
	if err != nil || !os.SameFile(parentInfo, opened) {
		if root != nil {
			_ = root.Close()
		}
		return zero, ErrJournalInvalid
	}
	defer func() {
		if err := root.Close(); err != nil {
			target = zero
			retErr = errors.Join(retErr, ErrJournalInvalid, err)
		}
	}()
	raw, err := readJournalFile(root, name)
	if err != nil {
		return zero, err
	}
	var prepared journalPrepared
	if err := strictJournalJSON(raw, &prepared); err != nil ||
		!validPrepared(prepared, dataRoot, prepared.ProjectID, prepared.ProjectPath, stageParent, name) ||
		!childOf(dataRoot, prepared.ProjectPath) ||
		!validTargetStageName(prepared.StageName) {
		return zero, ErrJournalInvalid
	}
	storagePath := filepath.Join(prepared.ProjectPath, projectfs.StorageDirectory)
	if stageParent == storagePath || childOf(storagePath, stageParent) || childOf(stageParent, storagePath) {
		return zero, ErrJournalInvalid
	}
	parentID, ok := inodeOf(opened)
	if !ok || parentID != prepared.Parent {
		return zero, ErrJournalInvalid
	}
	// The marker's project ID is untrusted. Bind it to the manifest reached
	// through a pinned library/project capability before exposing an owner.
	if err := verifyJournalTargetOwner(ctx, prepared); err != nil {
		return zero, ErrJournalInvalid
	}
	finalRaw, err := readJournalFile(root, name)
	parentFinal, parentErr := privateDirectory(stageParent)
	if err != nil || !bytes.Equal(finalRaw, raw) || parentErr != nil || !os.SameFile(parentFinal, opened) {
		return zero, ErrJournalInvalid
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return JournalTarget{ProjectID: prepared.ProjectID, ProjectPath: prepared.ProjectPath,
		StagePath: filepath.Join(stageParent, prepared.StageName), PreparedHash: journalDigest(raw)}, nil
}

func verifyJournalTargetOwner(ctx context.Context, prepared journalPrepared) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	library, err := projectfs.OpenLibrary(filepath.Dir(prepared.ProjectPath))
	if err != nil {
		return err
	}
	project, err := library.OpenProject(prepared.ProjectPath)
	if err != nil {
		return errors.Join(err, library.Close())
	}
	if project.ProjectID() != prepared.ProjectID {
		err = ErrJournalInvalid
	}
	return errors.Join(err, project.Close(), library.Close())
}

func validTargetStageName(name string) bool {
	const prefix = "novel-private-publish-"
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+32 {
		return false
	}
	for _, c := range name[len(prefix):] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
