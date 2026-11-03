//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// RecentProjects projects private catalog metadata into the browser DTO.
// Process-local catalog keys are never included in this navigation response.
func (s *syntheticReadOpen) RecentProjects(ctx context.Context, _ transport.Caller) ([]transport.RecentNavigationProject, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items, err := s.catalog.List()
	if err != nil {
		return nil, err
	}
	result := make([]transport.RecentNavigationProject, 0, len(items))
	for _, item := range items {
		result = append(result, transport.RecentNavigationProject{
			Name: item.Name, Path: item.Path, UpdatedAt: item.UpdatedAt, ProjectID: item.ProjectID,
		})
	}
	return result, nil
}

// ListDirectory holds an exact read lease while walking a pinned project root.
// Transport derives relative from the browser request; only the admitted
// context path is used to select the trusted project.
func (s *syntheticReadOpen) ListDirectory(ctx context.Context, caller transport.Caller, rawExpected any, relative string) ([]projectfs.VisibleNode, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	expected, err := projectsession.ParseContext(rawExpected)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	var tree []projectfs.VisibleNode
	err = s.lifecycle.Read(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected, func(_ *sql.Tx) error {
		project, err := s.library.OpenProject(expected.ProjectPath)
		if err != nil {
			return err
		}
		if project.ProjectID() != expected.ProjectID {
			_ = project.Close()
			return errors.New("PROJECT_SESSION_STALE")
		}
		tree, err = project.ListVisibleTree(ctx, relative, projectfs.MaxVisibleTreeEntries, projectfs.MaxVisibleTreeDepth)
		closeErr := project.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return nil, err
	}
	return tree, nil
}
