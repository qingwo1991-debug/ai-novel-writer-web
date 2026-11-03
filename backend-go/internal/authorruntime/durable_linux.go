//go:build linux

// This file adds the durable (write-capable) author project runtime on top of
// the read-only ReadLifecycle. It reuses the lifecycle's single continuously
// held legacy writer lock and its ActiveCoordinator lease fencing, replaces the
// private snapshot reader with a confined write handle on the author's own
// project.db, and performs allowlisted project_core writes under the frozen
// cookie+tab+lease context.
//
// Guarantees: one cooperating writer at a time (legacy lock + coordinator
// lease), stale-baseline rejection at the session level (frozen context),
// content-identity re-verification before and after every write, and no
// creation of missing projects or databases. It does not implement draft body
// CAS, generation, import, or knowledge writes; those remain separate gates.

package authorruntime

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var (
	// ErrDurableInvalid means the durable runtime was called with unusable
	// configuration or an admitted handle of the wrong kind.
	ErrDurableInvalid = errors.New("AUTHOR_DURABLE_INVALID")
	// ErrDurableClosed means the admitted handle is already closed.
	ErrDurableClosed = errors.New("AUTHOR_DURABLE_CLOSED")
)

// admittedAuthor is one durable, write-capable admitted project. It implements
// projectsession.Admitted so the coordinator can fence it exactly like the
// read-only handle: the lease context is stable and Verify re-proves authority.
type admittedAuthor struct {
	mu      sync.Mutex
	context projectsession.Context
	project *projectfs.Project
	permit  *projectaccess.AdmittedAuthor
	handle  *projectfs.AuthorDatabase
	core    *store.ProjectCore
	closed  bool
}

func (a *admittedAuthor) Context() projectsession.Context { return a.context }

// Verify re-proves the retained lock, the project/storage identity, the pinned
// database inode and the admission content digest.
func (a *admittedAuthor) Verify(ctx context.Context) error {
	if a == nil {
		return ErrDurableInvalid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.handle == nil {
		return ErrDurableClosed
	}
	if err := a.permit.Verify(ctx, a.project); err != nil {
		return err
	}
	return a.handle.Verify(ctx)
}

// Close releases the write handle and the admission permit but never the legacy
// lock, the catalog capability or any author file.
func (a *admittedAuthor) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	var problems []error
	if a.handle != nil {
		problems = append(problems, a.handle.Close())
		a.handle = nil
	}
	if a.permit != nil {
		problems = append(problems, a.permit.Close())
		a.permit = nil
	}
	if a.project != nil {
		problems = append(problems, a.project.Close())
		a.project = nil
	}
	return errors.Join(problems...)
}

// OpenProjectDurable admits a catalog key for durable editing and returns the
// same browser Open DTO as the read-only path, but backed by the author's own
// database. A candidate that fails admission or core read cannot replace the
// currently active project: the coordinator only commits a fully staged handle.
func (l *ReadLifecycle) OpenProjectDurable(ctx context.Context, owner projectsession.Owner, key, requestToken string) (ProjectOpenResult, error) {
	if l == nil || l.active == nil || l.catalog == nil || ctx == nil || key == "" || requestToken == "" {
		return ProjectOpenResult{}, ErrInvalid
	}
	var staged map[string]any
	identity, err := l.active.Open(ctx, owner, key, requestToken, func(ctx context.Context, held *writerlock.Lock) (projectsession.Admitted, error) {
		project, err := l.catalog.Resolve(key)
		if err != nil {
			return nil, err
		}
		path, id := project.Path(), project.ProjectID()
		config := l.config
		config.ProjectPath = path
		permit, err := projectaccess.AdmitAuthor(ctx, config, project, held)
		if err != nil {
			_ = project.Close()
			return nil, err
		}
		handle, err := project.OpenAuthorDatabase(ctx, permit)
		if err != nil {
			_ = permit.Close()
			_ = project.Close()
			return nil, err
		}
		closeFailed := true
		defer func() {
			if closeFailed {
				_ = handle.Close()
				_ = permit.Close()
				_ = project.Close()
			}
		}()
		db := handle.DB()
		if db == nil {
			return nil, ErrDurableInvalid
		}
		var nonce [24]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		candidate := projectsession.Context{ProjectID: id, ProjectPath: path, LeaseID: base64.RawURLEncoding.EncodeToString(nonce[:])}
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, err
		}
		staged, err = readProjectOpenCore(ctx, tx, candidate)
		rollbackErr := tx.Rollback()
		if err != nil {
			return nil, err
		}
		if rollbackErr != nil {
			// ROLLBACK is read-only; a failure means the handle is not clean.
			return nil, errors.Join(ErrChanged, rollbackErr)
		}
		closeFailed = false
		return &admittedAuthor{context: candidate, project: project, permit: permit, handle: handle, core: store.NewProjectCore(db)}, nil
	})
	if err != nil {
		result := ProjectOpenResult{RequestToken: requestToken, Error: err.Error(), Stale: errors.Is(err, projectsession.ErrStaleOpen)}
		current, present, currentErr := l.Current(ctx, owner)
		if currentErr != nil {
			return ProjectOpenResult{}, currentErr
		}
		if present {
			path := current.ProjectPath
			result.ActiveProjectPath = &path
			result.DBReady = true
			result.DatabaseRestored = true
		}
		return result, nil
	}
	project := staged
	if project == nil {
		// Same-key refresh does not run the staging callback; read the live core.
		admitted, release, beginErr := l.active.Begin(ctx, owner, identity)
		if beginErr != nil {
			return ProjectOpenResult{}, beginErr
		}
		author, ok := admitted.(*admittedAuthor)
		if !ok {
			release()
			return ProjectOpenResult{}, ErrDurableInvalid
		}
		author.mu.Lock()
		if author.closed || author.core == nil {
			author.mu.Unlock()
			release()
			return ProjectOpenResult{}, ErrDurableClosed
		}
		project, beginErr = author.core.Get(ctx)
		author.mu.Unlock()
		release()
		if beginErr != nil {
			return ProjectOpenResult{}, beginErr
		}
	}
	current, present, err := l.Current(ctx, owner)
	if err != nil {
		return ProjectOpenResult{}, err
	}
	if !present || current != identity {
		return ProjectOpenResult{}, projectsession.ErrStaleOpen
	}
	path := identity.ProjectPath
	return ProjectOpenResult{Success: true, Project: project, RequestToken: requestToken,
		ActiveProjectPath: &path, DatabaseRestored: true, DBReady: true}, nil
}

// CloseProjectDurable validates the frozen context for this exact caller, then
// closes the admitted write handle before acknowledging.
func (l *ReadLifecycle) CloseProjectDurable(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) error {
	if l == nil || l.active == nil {
		return ErrDurableInvalid
	}
	return l.active.CloseProject(ctx, owner, expected)
}

// CurrentProjectDurable reports the actually admitted durable context for one
// exact browser owner, or present=false when nothing is open.
func (l *ReadLifecycle) CurrentProjectDurable(ctx context.Context, owner projectsession.Owner) (projectsession.Context, bool, error) {
	if l == nil || l.active == nil {
		return projectsession.Context{}, false, ErrDurableInvalid
	}
	return l.active.Current(ctx, owner)
}

// SaveCore writes an allowlisted project_core patch under the frozen context.
// It verifies admission and content identity before the write, performs the
// update, then re-verifies and adopts the new committed digest so the same
// lease can continue. The frozen context and the single-writer lock are the
// stale-baseline guard for project_core; draft body CAS is a separate gate.
func (l *ReadLifecycle) SaveCore(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, patch map[string]any) (map[string]any, error) {
	if l == nil || l.active == nil || ctx == nil || patch == nil {
		return nil, ErrDurableInvalid
	}
	admitted, release, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return nil, err
	}
	defer release()
	author, ok := admitted.(*admittedAuthor)
	if !ok {
		return nil, ErrDurableInvalid
	}
	author.mu.Lock()
	defer author.mu.Unlock()
	if author.closed || author.handle == nil || author.core == nil {
		return nil, ErrDurableClosed
	}
	if err := author.permit.Verify(ctx, author.project); err != nil {
		return nil, err
	}
	if err := author.handle.Verify(ctx); err != nil {
		return nil, err
	}
	if err := author.core.Update(ctx, patch); err != nil {
		return nil, err
	}
	if err := author.handle.Verify(ctx); err != nil {
		return nil, err
	}
	if err := author.permit.Refresh(ctx); err != nil {
		return nil, err
	}
	return author.core.Get(ctx)
}
