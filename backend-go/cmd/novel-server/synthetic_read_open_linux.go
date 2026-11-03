//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
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
	closed    bool
}

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
	catalog, err := projectcatalog.Open(library, metadata)
	if err != nil {
		remove()
		return nil, err
	}
	lifecycle, err := authorruntime.NewReadLifecycle(catalog, projectaccess.Config{
		DataRoot: filepath.Dir(libraryDir), LibraryRoot: libraryDir,
		ScratchParent: scratch, MaxDatabaseBytes: 64 << 20, Schema: config.Schema,
	})
	if err != nil {
		remove()
		return nil, err
	}
	return &syntheticReadOpen{library: library, catalog: catalog, lifecycle: lifecycle,
		keys: make(map[string]string), workspace: workspace}, nil
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
