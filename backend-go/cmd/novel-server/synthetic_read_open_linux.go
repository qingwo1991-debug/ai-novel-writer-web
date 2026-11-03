//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectgrant"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectpublish"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// The schema comes only from an administrator supplied contract. This opt-in
// synthetic bridge preserves the browser's original path-shaped request while
// using a process-local catalog key for every author admission.
type syntheticReadOpenConfig struct{ Schema admission.SchemaContract }

// syntheticReadOpen is the read-only synthetic lane. It embeds the shared
// read surface and selects the lifecycle's admitted private snapshot as its
// transaction source; the durable lane embeds the same surface with its own
// reader-per-request transaction source.
type syntheticReadOpen struct {
	syntheticReadSurface
}

type syntheticLibraryRecoveryGuard struct {
	held       *writerlock.Lock
	libraryDir string
	check      projectcatalog.RecoveryCheck
}

const syntheticStageParentName = ".go-publish-stage"
const syntheticCatalogDirectoryName = ".go-synthetic-catalog"
const syntheticRecoveryProbeID = "synthetic-recovery-probe"

func newSyntheticReadOpen(library *projectfs.Library, libraryDir string, config syntheticReadOpenConfig) (*syntheticReadOpen, error) {
	if library == nil || library.Path() != libraryDir || !underTempRoot(libraryDir) || !privateSyntheticGlobalRoot(filepath.Dir(libraryDir)) || len(config.Schema) == 0 {
		return nil, errors.New("SYNTHETIC_READ_CONFIGURATION_INVALID")
	}
	workspace, err := os.MkdirTemp(os.TempDir(), "novel-read-open-")
	if err != nil {
		return nil, err
	}
	remove := func() { _ = os.RemoveAll(workspace) }
	scratch := filepath.Join(workspace, "scratch")
	if err := os.Mkdir(scratch, 0700); err != nil {
		remove()
		return nil, err
	}
	dataRoot := filepath.Dir(libraryDir)
	held, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		remove()
		return nil, err
	}
	lockOwned := true
	defer func() {
		if lockOwned {
			_ = held.Close()
		}
	}()
	// Recent navigation records survive a clean process restart. This location
	// is server-owned under the already private synthetic root; process-local
	// catalog keys and reader copies still live only for this process.
	metadata := filepath.Join(dataRoot, syntheticCatalogDirectoryName)
	if err := ensureSyntheticCatalogDirectory(dataRoot, metadata); err != nil {
		remove()
		return nil, err
	}
	readback := func(ctx context.Context, path, id string, recovery projectpublish.JournalRecovery) error {
		if recovery.ProjectPath != path || recovery.Class != projectpublish.JournalSwitched {
			return authorruntime.ErrSwitchedDraftUnverified
		}
		readConfig := projectaccess.Config{DataRoot: dataRoot, LibraryRoot: libraryDir,
			ProjectPath: path, ScratchParent: scratch, MaxDatabaseBytes: 64 << 20, Schema: config.Schema}
		journalName := "novel-exchange-" + recovery.JournalID + ".prepared.json"
		facts, err := authorruntime.VerifyRestartedSwitchedDraftUnderLock(ctx, held, library,
			id, path, filepath.Join(dataRoot, syntheticStageParentName), journalName, readConfig)
		if err != nil {
			return err
		}
		if facts.ProjectID != id || facts.ProjectPath != path || facts.DraftID != recovery.EditedDraftID ||
			facts.EditedTreeHash != recovery.VerifiedNewSourceTreeHash {
			return authorruntime.ErrSwitchedDraftUnverified
		}
		return nil
	}
	check, err := syntheticRecoveryCheck(libraryDir, held, readback)
	if err != nil {
		remove()
		return nil, err
	}
	catalog, err := projectcatalog.OpenWithRecoveryCheck(library, metadata, check)
	if err != nil {
		remove()
		return nil, err
	}
	lifecycle, err := authorruntime.NewReadLifecycleWithHeldLock(catalog, projectaccess.Config{
		DataRoot: dataRoot, LibraryRoot: libraryDir,
		ScratchParent: scratch, MaxDatabaseBytes: 64 << 20, Schema: config.Schema,
	}, held)
	if err != nil {
		remove()
		return nil, err
	}
	lockOwned = false
	commits, err := authorruntime.NewDraftCommitService(lifecycle, library,
		filepath.Join(dataRoot, syntheticStageParentName), projectpublish.Limits{
			MaxFiles: 4096, MaxBytes: 128 << 20, MaxDepth: 32,
		})
	if err != nil {
		_ = lifecycle.Shutdown()
		remove()
		return nil, err
	}
	grants, err := projectgrant.New(library)
	if err != nil {
		_ = lifecycle.Shutdown()
		remove()
		return nil, err
	}
	return &syntheticReadOpen{syntheticReadSurface: syntheticReadSurface{
		library: library, catalog: catalog, lifecycle: lifecycle,
		commits: commits, grants: grants, keys: make(map[string]string), workspace: workspace, recovery: check,
		readTx: lifecycleReadTx(lifecycle),
	}}, nil
}

func ensureSyntheticCatalogDirectory(dataRoot, directory string) error {
	if !privateSyntheticGlobalRoot(dataRoot) || filepath.Dir(directory) != dataRoot {
		return errors.New("SYNTHETIC_CATALOG_UNSAFE")
	}
	created := false
	if err := os.Mkdir(directory, 0700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("SYNTHETIC_CATALOG_UNSAFE")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("SYNTHETIC_CATALOG_UNSAFE")
	}
	fd, err := syscall.Open(directory, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	opened := os.NewFile(uintptr(fd), directory)
	actual, statErr := opened.Stat()
	closeErr := opened.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(info, actual) {
		return errors.Join(statErr, closeErr, errors.New("SYNTHETIC_CATALOG_UNSAFE"))
	}
	if created {
		parent, err := os.Open(dataRoot)
		if err != nil {
			return err
		}
		syncErr := parent.Sync()
		closeErr := parent.Close()
		return errors.Join(syncErr, closeErr)
	}
	return nil
}

// The library-only synthetic route must hold the same legacy lock and check
// the same persistent journal directory. Otherwise it could list metadata
// while a prior read-enabled process left recovery unresolved.
func newSyntheticLibraryRecoveryGuard(libraryDir string) (*syntheticLibraryRecoveryGuard, error) {
	if !underTempRoot(libraryDir) || !privateSyntheticGlobalRoot(filepath.Dir(libraryDir)) {
		return nil, errors.New("SYNTHETIC_RECOVERY_STAGE_UNSAFE")
	}
	held, err := writerlock.AcquireLegacy(filepath.Dir(libraryDir))
	if err != nil {
		return nil, err
	}
	check, err := syntheticRecoveryCheck(libraryDir, held, nil)
	if err != nil {
		_ = held.Close()
		return nil, err
	}
	return &syntheticLibraryRecoveryGuard{held: held, libraryDir: libraryDir, check: check}, nil
}

func (g *syntheticLibraryRecoveryGuard) VerifyRecovery() error {
	if g == nil || g.check == nil {
		return errors.New("SYNTHETIC_READ_CLOSED")
	}
	return g.check(filepath.Join(g.libraryDir, ".go-recovery-probe"), syntheticRecoveryProbeID)
}

func (g *syntheticLibraryRecoveryGuard) Close() error {
	if g == nil || g.held == nil {
		return nil
	}
	return g.held.Close()
}

func syntheticRecoveryCheck(libraryDir string, held *writerlock.Lock, readback authorruntime.SwitchedReadback) (projectcatalog.RecoveryCheck, error) {
	dataRoot := filepath.Dir(libraryDir)
	stageParent := filepath.Join(dataRoot, syntheticStageParentName)
	if err := os.Mkdir(stageParent, 0700); err == nil {
		// A first creation must survive a crash before any journal is written.
		parent, openErr := os.Open(dataRoot)
		if openErr != nil {
			return nil, openErr
		}
		syncErr := parent.Sync()
		closeErr := parent.Close()
		if syncErr != nil || closeErr != nil {
			return nil, errors.Join(syncErr, closeErr)
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if !privateSyntheticGlobalRoot(stageParent) || !sameSyntheticFilesystem(stageParent, libraryDir) {
		return nil, errors.New("SYNTHETIC_RECOVERY_STAGE_UNSAFE")
	}
	check, err := authorruntime.CatalogJournalRecoveryCheck(context.Background(), held, dataRoot, stageParent, readback)
	if err != nil {
		return nil, err
	}
	if err := check(filepath.Join(libraryDir, ".go-recovery-probe"), syntheticRecoveryProbeID); err != nil {
		return nil, err
	}
	return check, nil
}

func sameSyntheticFilesystem(a, b string) bool {
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	if err != nil {
		return false
	}
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Dev == rightStat.Dev
}
