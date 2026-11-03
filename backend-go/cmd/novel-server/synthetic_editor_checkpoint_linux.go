//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

func (s *syntheticReadOpen) ReadArchCheckpoint(ctx context.Context, caller transport.Caller, raw any) (json.RawMessage, bool, error) {
	if s == nil || ctx == nil {
		return nil, false, errors.New("SYNTHETIC_READ_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, errors.New("SYNTHETIC_READ_CLOSED")
	}
	return s.lifecycle.ReadArchCheckpoint(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected)
}
