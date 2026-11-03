//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// CloseReadOnlyProject removes only this cookie/tab's exact admitted lease.
// The process-wide legacy lock remains held for other project owners and is
// released by syntheticReadOpen.Close during gateway shutdown.
func (s *syntheticReadOpen) CloseReadOnlyProject(ctx context.Context, caller transport.Caller, path string, raw any) (any, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	if path != expected.ProjectPath {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lifecycle == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	owner := projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	if err := s.lifecycle.Close(ctx, owner, expected); err != nil {
		return nil, err
	}
	if _, present, err := s.lifecycle.Current(ctx, owner); err != nil {
		return nil, err
	} else if present {
		return nil, errors.New("PROJECT_CLOSE_UNCONFIRMED")
	}
	return map[string]any{"success": true}, nil
}
