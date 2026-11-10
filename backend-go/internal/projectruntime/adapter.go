//go:build linux

// Package projectruntime adapts the durable author runtime to the transport
// project lifecycle protocol. It owns no admission logic of its own: every
// decision is delegated to authorruntime, which holds the single legacy writer
// lock, the ActiveCoordinator lease fence and the confined author database.
package projectruntime

import (
	"context"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// ErrInvalid means the adapter received a caller or target it cannot map.
var ErrInvalid = errors.New("PROJECT_RUNTIME_INVALID")

// Manager implements transport.ProjectLifecycleProvider for the durable Go
// author runtime. It supports exactly one durable runtime per process because
// the underlying legacy writer lock is process-exclusive by design.
type Manager struct {
	runtime *authorruntime.ReadLifecycle
}

// NewManager binds the adapter to an already constructed durable read/write
// lifecycle. The lifecycle must own the retained legacy lock.
func NewManager(runtime *authorruntime.ReadLifecycle) (*Manager, error) {
	if runtime == nil {
		return nil, ErrInvalid
	}
	return &Manager{runtime: runtime}, nil
}

func owner(caller transport.Caller) (projectsession.Owner, error) {
	if caller.SessionID == "" || caller.ClientID == "" {
		return projectsession.Owner{}, ErrInvalid
	}
	return projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, nil
}

// OpenProject resolves a server-catalog key (never a caller path) and returns
// the browser Open DTO backed by the author's own database.
func (m *Manager) OpenProject(ctx context.Context, caller transport.Caller, target any, requestToken string, requestedPath any) (any, error) {
	if m == nil || m.runtime == nil {
		return nil, ErrInvalid
	}
	key, ok := target.(string)
	if !ok || key == "" {
		return nil, ErrInvalid
	}
	who, err := owner(caller)
	if err != nil {
		return nil, err
	}
	// requestedPath is renderer metadata for the frontend and never resolves an
	// arbitrary filesystem target; the catalog key alone is authority.
	if requestedPath != nil {
		if path, ok := requestedPath.(string); !ok || path == "" {
			return nil, ErrInvalid
		}
	}
	return m.runtime.OpenProjectDurable(ctx, who, key, requestToken)
}

// CloseProject validates the frozen context and closes the admitted database
// before acknowledging.
func (m *Manager) CloseProject(ctx context.Context, caller transport.Caller, projectPath string, expected any) (any, error) {
	if m == nil || m.runtime == nil {
		return nil, ErrInvalid
	}
	who, err := owner(caller)
	if err != nil {
		return nil, err
	}
	frozen, err := frozenContext(expected)
	if err != nil {
		return nil, err
	}
	if frozen.ProjectPath != projectPath {
		return nil, ErrInvalid
	}
	if err := m.runtime.CloseProjectDurable(ctx, who, frozen); err != nil {
		return nil, err
	}
	return true, nil
}

// ProjectRuntimeContext reports the live frozen context for this caller, or nil
// when no project is open.
func (m *Manager) ProjectRuntimeContext(ctx context.Context, caller transport.Caller) (any, error) {
	if m == nil || m.runtime == nil {
		return nil, ErrInvalid
	}
	who, err := owner(caller)
	if err != nil {
		return nil, err
	}
	frozen, present, err := m.runtime.CurrentProjectDurable(ctx, who)
	if err != nil {
		return nil, err
	}
	if !present {
		// The browser contract is {activeProjectPath, dbReady}; the frozen
		// {projectId, leaseId, projectPath} keys stay for the existing consumers
		// so a caller can still feed this DTO back as a frozen context.
		return map[string]any{
			"projectId":         nil,
			"leaseId":           nil,
			"projectPath":       nil,
			"activeProjectPath": nil,
			"dbReady":           false,
		}, nil
	}
	path := frozen.ProjectPath
	return map[string]any{
		"projectId":         frozen.ProjectID,
		"leaseId":           frozen.LeaseID,
		"projectPath":       frozen.ProjectPath,
		"activeProjectPath": path,
		"dbReady":           true,
	}, nil
}

// SaveProject writes an allowlisted project_core patch under the frozen context.
func (m *Manager) SaveProject(ctx context.Context, caller transport.Caller, projectID string, patch map[string]any, expected any) (any, error) {
	if m == nil || m.runtime == nil {
		return nil, ErrInvalid
	}
	who, err := owner(caller)
	if err != nil {
		return nil, err
	}
	frozen, err := frozenContext(expected)
	if err != nil {
		return nil, err
	}
	if frozen.ProjectID != projectID {
		return nil, ErrInvalid
	}
	project, err := m.runtime.SaveCore(ctx, who, frozen, patch)
	if err != nil {
		return nil, err
	}
	return map[string]any{"success": true, "project": project}, nil
}

// frozenContext accepts the exact three-key frozen context the frontend sends
// after an open: projectId, leaseId, projectPath. Unknown shapes fail closed.
func frozenContext(raw any) (projectsession.Context, error) {
	return projectsession.ParseContext(raw)
}
