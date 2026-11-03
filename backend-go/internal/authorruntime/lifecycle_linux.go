//go:build linux

package authorruntime

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// ReadLifecycle composes trusted catalog resolution, one continuously held
// legacy writer lock, private read-only admission, and exact session/tab
// ownership. It does not expose a writable repository or the browser's full
// project-open DTO, so it must not be bound as ProjectLifecycleProvider.
type ReadLifecycle struct {
	catalog *projectcatalog.Store
	config  projectaccess.Config
	active  *projectsession.ActiveCoordinator
}

type admittedRead struct {
	reader      *Reader
	context     projectsession.Context
	knowledgeMu sync.Mutex
	knowledge   *projectaccess.KnowledgeRead
}

func (a *admittedRead) Context() projectsession.Context  { return a.context }
func (a *admittedRead) Verify(ctx context.Context) error { return a.reader.Verify(ctx) }
func (a *admittedRead) Close() error                     { return a.reader.Close() }

// NewReadLifecycle transfers the newly acquired legacy lock to the active
// coordinator. The caller supplies only trusted administrator configuration;
// per-project paths are resolved from catalog capabilities during Open.
func NewReadLifecycle(catalog *projectcatalog.Store, config projectaccess.Config) (*ReadLifecycle, error) {
	if catalog == nil || config.DataRoot == "" || config.LibraryRoot == "" || config.ScratchParent == "" || config.ProjectPath != "" {
		return nil, ErrInvalid
	}
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		return nil, err
	}
	active, err := projectsession.NewActiveCoordinator(config.DataRoot, held)
	if err != nil {
		_ = held.Close()
		return nil, err
	}
	return &ReadLifecycle{catalog: catalog, config: config, active: active}, nil
}

// Open admits only a process-local catalog key. The current project remains
// live when a later candidate fails or a newer request supersedes it. The
// returned context is an internal lease, not a browser project-open response.
func (l *ReadLifecycle) Open(ctx context.Context, owner projectsession.Owner, key, requestToken string) (projectsession.Context, error) {
	if l == nil || l.active == nil || l.catalog == nil {
		return projectsession.Context{}, ErrInvalid
	}
	return l.active.Open(ctx, owner, key, requestToken, func(ctx context.Context, held *writerlock.Lock) (projectsession.Admitted, error) {
		project, err := l.catalog.Resolve(key)
		if err != nil {
			return nil, err
		}
		path, id := project.Path(), project.ProjectID()
		if err := project.Close(); err != nil {
			return nil, err
		}
		config := l.config
		config.ProjectPath = path
		reader, err := OpenReadOnlyUnderLock(ctx, held, l.catalog, key, config)
		if err != nil {
			return nil, err
		}
		report, err := reader.Report()
		if err != nil || report.ProjectID != id {
			_ = reader.Close()
			return nil, errors.Join(ErrChanged, err)
		}
		var nonce [24]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			_ = reader.Close()
			return nil, ErrInvalid
		}
		return &admittedRead{reader: reader, context: projectsession.Context{
			ProjectID: id, ProjectPath: path, LeaseID: base64.RawURLEncoding.EncodeToString(nonce[:]),
		}}, nil
	})
}

// Current verifies the source, private DB and live lock before reporting the
// exact current lease. Absence is distinct from a failed verification.
func (l *ReadLifecycle) Current(ctx context.Context, owner projectsession.Owner) (projectsession.Context, bool, error) {
	if l == nil || l.active == nil {
		return projectsession.Context{}, false, ErrInvalid
	}
	return l.active.Current(ctx, owner)
}

// Read holds the exact owner lease across source verification and the entire
// read-only SQLite transaction. It cannot return the transaction to callers.
func (l *ReadLifecycle) Read(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, operation func(*sql.Tx) error) error {
	if l == nil || l.active == nil || operation == nil {
		return ErrInvalid
	}
	handle, done, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return err
	}
	defer done()
	admitted, ok := handle.(*admittedRead)
	if !ok {
		return ErrInvalid
	}
	return admitted.reader.Read(ctx, operation)
}

func (l *ReadLifecycle) Close(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) error {
	if l == nil || l.active == nil {
		return ErrInvalid
	}
	return l.active.CloseProject(ctx, owner, expected)
}

// Shutdown closes every admitted DB before releasing the shared gateway lock.
func (l *ReadLifecycle) Shutdown() error {
	if l == nil || l.active == nil {
		return ErrInvalid
	}
	return l.active.Shutdown()
}
