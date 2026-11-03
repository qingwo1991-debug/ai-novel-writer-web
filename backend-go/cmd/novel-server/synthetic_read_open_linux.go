//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// The schema comes only from an administrator supplied contract. This opt-in
// synthetic bridge preserves the browser's original path-shaped request while
// using a process-local catalog key for every author admission.
type syntheticReadOpenConfig struct{ Schema admission.SchemaContract }

type syntheticReadOpen struct {
	mu        sync.Mutex
	library   *projectfs.Library
	catalog   *projectcatalog.Store
	lifecycle *authorruntime.ReadLifecycle
	keys      map[string]string
	workspace string
	recovery  projectcatalog.RecoveryCheck
	closed    bool
}

type syntheticLibraryRecoveryGuard struct {
	held       *writerlock.Lock
	libraryDir string
	check      projectcatalog.RecoveryCheck
}

const syntheticStageParentName = ".go-publish-stage"
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
	metadata := filepath.Join(workspace, "metadata")
	scratch := filepath.Join(workspace, "scratch")
	if err := os.Mkdir(metadata, 0700); err != nil {
		remove()
		return nil, err
	}
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
	check, err := syntheticRecoveryCheck(libraryDir, held)
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
	return &syntheticReadOpen{library: library, catalog: catalog, lifecycle: lifecycle,
		keys: make(map[string]string), workspace: workspace, recovery: check}, nil
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
	check, err := syntheticRecoveryCheck(libraryDir, held)
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

func syntheticRecoveryCheck(libraryDir string, held *writerlock.Lock) (projectcatalog.RecoveryCheck, error) {
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
	check, err := authorruntime.CatalogJournalRecoveryCheck(context.Background(), held, dataRoot, stageParent, nil)
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

// VerifyRecovery gates the raw library route as well as catalog-backed RPCs.
// The dedicated probe makes even orphan journals block a library with no
// registered projects. The stage parent persists across process restarts.
func (s *syntheticReadOpen) VerifyRecovery() error {
	if s == nil {
		return errors.New("SYNTHETIC_READ_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.recovery == nil {
		return errors.New("SYNTHETIC_READ_CLOSED")
	}
	return s.recovery(filepath.Join(s.library.Path(), ".go-recovery-probe"), syntheticRecoveryProbeID)
}

func (s *syntheticReadOpen) OpenReadOnlyProject(ctx context.Context, caller transport.Caller, browserPath, token string) (any, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	if err := s.recovery(filepath.Join(s.library.Path(), ".go-recovery-probe"), syntheticRecoveryProbeID); err != nil {
		return nil, err
	}
	// Compare only against canonical entries from the trusted library. A path
	// from RPC is never passed to OpenProject or the author runtime directly.
	listed, err := s.library.ListCanonicalProjects(ctx, projectfs.MaxLibraryEntries)
	if err != nil {
		return nil, err
	}
	var matched *projectfs.ListedProject
	for _, entry := range listed {
		if entry.Path == browserPath {
			matched = &entry
			break
		}
	}
	if matched == nil {
		return nil, security.NewHTTPError(http.StatusNotFound, "PROJECT_NOT_FOUND")
	}
	key := s.keys[browserPath]
	if key != "" {
		// A revoked cached key must fail closed. Future publisher integration
		// must update this cache only after verified recovery; an unjournaled
		// inode change is never a reason to silently register a new key.
		project, resolveErr := s.catalog.Resolve(key)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if err := project.Close(); err != nil {
			return nil, err
		}
	}
	if key == "" {
		project, err := s.library.OpenProject(matched.Path)
		if err != nil {
			return nil, err
		}
		recent, registerErr := s.catalog.Register(project)
		closeErr := project.Close()
		if registerErr != nil {
			return nil, registerErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		key = recent.Key
		s.keys[browserPath] = key
	}
	return s.lifecycle.OpenProjectReadOnly(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, key, token)
}

// ReadCharactersJSON uses the same lifecycle and exact cookie/tab lease as
// project:open. The runtime owns the private snapshot and finishes JSON
// serialization before returning; this bridge never exposes a database handle.
func (s *syntheticReadOpen) ReadCharactersJSON(ctx context.Context, caller transport.Caller, raw any, operation string) (json.RawMessage, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	return s.lifecycle.ReadCharactersJSON(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected, operation)
}

// ReadDraftJSON keeps serialization inside the same admitted private-copy
// lifecycle and exact cookie/tab/project lease used for project:open.
func (s *syntheticReadOpen) ReadDraftJSON(ctx context.Context, caller transport.Caller, raw any, operation string, number int64) (json.RawMessage, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	return s.lifecycle.ReadDraftJSON(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected, operation, number)
}

func (s *syntheticReadOpen) ReadEditorStageJSON(ctx context.Context, caller transport.Caller, raw any, operation string) (json.RawMessage, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	return s.lifecycle.ReadEditorStageJSON(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected, operation)
}

func (s *syntheticReadOpen) ReadReviewRevisionJSON(ctx context.Context, caller transport.Caller, raw any, operation string, baseDraftID int64) (json.RawMessage, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	return s.lifecycle.ReadReviewRevisionJSON(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected, operation, baseDraftID)
}

func (s *syntheticReadOpen) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.lifecycle.Shutdown()
	if removeErr := os.RemoveAll(s.workspace); err == nil {
		err = removeErr
	}
	return err
}
