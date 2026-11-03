//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectgrant"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// syntheticReadTx runs one serialized read transaction while the exact browser
// cookie/tab/project lease is held. The read-only lane delegates to its
// admitted private snapshot; the durable lane opens a short-lived verified
// reader for the same project because its write admission cannot serve
// read-only transactions itself.
type syntheticReadTx func(ctx context.Context, caller transport.Caller, expected projectsession.Context, operation func(*sql.Tx) error) error

// syntheticReadSurface is the shared read/editor surface implemented by both
// the read-only synthetic lane and the durable synthetic lane. It owns the
// process-local catalog and lifecycle and never exposes a database handle or
// catalog key to the browser. readTx selects the lane-specific transaction
// source while every provider method body stays shared between the lanes.
type syntheticReadSurface struct {
	mu        sync.Mutex
	library   *projectfs.Library
	catalog   *projectcatalog.Store
	lifecycle *authorruntime.ReadLifecycle
	commits   *authorruntime.DraftCommitService
	grants    *projectgrant.Registry
	keys      map[string]string
	workspace string
	recovery  projectcatalog.RecoveryCheck
	readTx    syntheticReadTx
	closed    bool
}

// lifecycleReadTx is the read-only lane's transaction source: the lifecycle
// already holds the caller's admitted private snapshot under the exact lease.
func lifecycleReadTx(lifecycle *authorruntime.ReadLifecycle) syntheticReadTx {
	return func(ctx context.Context, caller transport.Caller, expected projectsession.Context, operation func(*sql.Tx) error) error {
		return lifecycle.Read(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected, operation)
	}
}

// VerifyRecovery gates the raw library route as well as catalog-backed RPCs.
// The dedicated probe makes even orphan journals block a library with no
// registered projects. The stage parent persists across process restarts.
func (s *syntheticReadSurface) VerifyRecovery() error {
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

// Close releases the shared legacy writer lock and removes the process-local
// workspace. Callers nil-guard the bridge before invoking it.
func (s *syntheticReadSurface) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if err := s.lifecycle.Shutdown(); err != nil {
		// An unclosed private Reader may still own files in this workspace.
		// Leave them for process-level recovery while its writer lock stays held.
		return err
	}
	if err := os.RemoveAll(s.workspace); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// OpenReadOnlyProject resolves a browser path only against canonical library
// entries and opens the caller's private snapshot under a process-local key.
// It is bound only on the read-only lane: the durable lane binds project:open
// through BindProjects and must never double-bind it.
func (s *syntheticReadSurface) OpenReadOnlyProject(ctx context.Context, caller transport.Caller, browserPath, token string) (any, error) {
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

// OpenGrantedReadOnlyProject accepts only a grant issued by trusted server
// state. No public route issues these grants in the synthetic bridge yet.
func (s *syntheticReadSurface) OpenGrantedReadOnlyProject(ctx context.Context, caller transport.Caller, grantID, token string) (any, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.grants == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	if err := s.recovery(filepath.Join(s.library.Path(), ".go-recovery-probe"), syntheticRecoveryProbeID); err != nil {
		return nil, err
	}
	return s.lifecycle.OpenGrantedProjectReadOnly(ctx,
		projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, s.grants, grantID, token)
}

// ReadCharactersJSON uses the same lifecycle and exact cookie/tab lease as
// project:open. The runtime owns the private snapshot and finishes JSON
// serialization before returning; this bridge never exposes a database handle.
// Character reads need the read-only admission's private snapshot machinery,
// so only the read-only lane binds this provider.
func (s *syntheticReadSurface) ReadCharactersJSON(ctx context.Context, caller transport.Caller, raw any, operation string) (json.RawMessage, error) {
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

// syntheticDraftReadOperation mirrors the runtime's operation gate so the
// shared surface rejects unknown draft operations before touching the lease.
func syntheticDraftReadOperation(operation string) bool {
	switch operation {
	case "db:draft-list", "db:draft-list-all", "db:draft-get-meta", "db:draft-get-full",
		"db:draft-get-latest", "db:draft-get-finalized", "db:draft-get-max-finalized-chapter", "db:draft-next-version":
		return true
	default:
		return false
	}
}

// ReadDraftJSON keeps serialization inside the same admitted read transaction
// and exact cookie/tab/project lease used for project:open. The returned
// bytes carry no DB handle and cannot acknowledge a save to the author's
// source project.
func (s *syntheticReadSurface) ReadDraftJSON(ctx context.Context, caller transport.Caller, raw any, operation string, number int64) (json.RawMessage, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	if !syntheticDraftReadOperation(operation) {
		return nil, authorruntime.ErrInvalid
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
	var wire json.RawMessage
	err = s.readTx(ctx, caller, expected, func(tx *sql.Tx) error {
		value, err := store.ReadDraftOperation(ctx, tx, operation, number)
		if err != nil {
			return err
		}
		wire, err = json.Marshal(value)
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// CommitDraftContent is available only to the private, read-enabled synthetic
// server: the durable lane keeps no DraftCommitService, and a committed CAS
// would downgrade its write admission. The cached catalog key comes from a
// prior admitted project:open, never from a browser path.
func (s *syntheticReadSurface) CommitDraftContent(ctx context.Context, caller transport.Caller, request transport.DraftLiveWriteRequest) (transport.DraftLiveWriteAck, error) {
	var zero transport.DraftLiveWriteAck
	if s == nil || ctx == nil {
		return zero, errors.New("SYNTHETIC_READ_CLOSED")
	}
	expected, err := projectsession.ParseContext(request.Expected)
	if err != nil {
		return zero, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.commits == nil {
		return zero, errors.New("SYNTHETIC_READ_CLOSED")
	}
	key := s.keys[expected.ProjectPath]
	if key == "" {
		return zero, errors.New("PROJECT_NOT_OPEN")
	}
	owner := projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	baseline := request.ExpectedContent
	committed, err := s.commits.CommitDraftCAS(ctx, authorruntime.DraftCommitRequest{
		Owner: owner, Expected: expected, CatalogKey: key, DraftID: request.DraftID,
		Content: request.Content, WordCount: request.WordCount, ExpectedContent: &baseline,
	})
	if err != nil {
		return zero, err
	}
	if committed.Key == "" || committed.Key == key || committed.Context.ProjectID != expected.ProjectID ||
		committed.Context.ProjectPath != expected.ProjectPath || committed.Context.LeaseID == "" ||
		committed.Context.LeaseID == expected.LeaseID {
		return zero, errors.New("SYNTHETIC_COMMIT_RESULT_INVALID")
	}
	// CommitDraftCAS has verified the switched source and atomically bound the
	// fresh Reader to this key. Reopen must never reuse the revoked old key.
	s.keys[expected.ProjectPath] = committed.Key
	return transport.DraftLiveWriteAck{ProjectSession: map[string]any{
		"projectId":   committed.Context.ProjectID,
		"projectPath": committed.Context.ProjectPath,
		"leaseId":     committed.Context.LeaseID,
	}}, nil
}

// ReadEditorStageJSON keeps the exact project lease, source proof and private
// read transaction through DTO serialization. It never grants a write handle.
func (s *syntheticReadSurface) ReadEditorStageJSON(ctx context.Context, caller transport.Caller, raw any, operation string) (json.RawMessage, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	if operation != "db:project-core-get" && operation != "db:blueprint-get-all" {
		return nil, authorruntime.ErrInvalid
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
	var wire json.RawMessage
	err = s.readTx(ctx, caller, expected, func(tx *sql.Tx) error {
		var result any
		var err error
		switch operation {
		case "db:project-core-get":
			result, err = store.ReadProjectCoreTx(ctx, tx)
		case "db:blueprint-get-all":
			result, err = store.ReadBlueprintsTx(ctx, tx)
		}
		if err != nil {
			return err
		}
		wire, err = json.Marshal(editorStageWire(result))
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// editorStageWire mirrors the runtime's DTO normalization: SQLite BLOB cells
// become Buffer-shaped maps and maps recurse so the browser contract shape is
// preserved without ever exposing a raw byte handle.
func editorStageWire(value any) any {
	switch value := value.(type) {
	case []byte:
		data := make([]int, len(value))
		for i, b := range value {
			data[i] = int(b)
		}
		return map[string]any{"type": "Buffer", "data": data}
	case map[string]any:
		if value == nil {
			return nil
		}
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = editorStageWire(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = editorStageWire(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = editorStageWire(item)
		}
		return out
	default:
		return value
	}
}

// ReadReviewRevisionJSON serializes metadata under the same exact project
// lease, source proof and private read transaction as other editor reads.
// Review and revision mutations remain outside this API.
func (s *syntheticReadSurface) ReadReviewRevisionJSON(ctx context.Context, caller transport.Caller, raw any, operation string, baseDraftID int64) (json.RawMessage, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	if operation != "db:review-list" && operation != "db:revision-get-pending" && operation != "db:revision-list" {
		return nil, authorruntime.ErrInvalid
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
	var wire json.RawMessage
	err = s.readTx(ctx, caller, expected, func(tx *sql.Tx) error {
		rows, err := store.ReadReviewRevisionListTx(ctx, tx, operation, baseDraftID)
		if err != nil {
			return err
		}
		wire, err = json.Marshal(store.ReviewRevisionListWire(rows))
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// ReadReviewRevisionFullJSON serializes a full row before releasing the exact
// admitted private project read lease. It never returns a database handle.
func (s *syntheticReadSurface) ReadReviewRevisionFullJSON(ctx context.Context, caller transport.Caller, raw any, operation string, id int64) (json.RawMessage, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_READ_CLOSED")
	}
	if operation != "db:review-get-full" && operation != "db:revision-get-full" {
		return nil, authorruntime.ErrInvalid
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
	var wire json.RawMessage
	err = s.readTx(ctx, caller, expected, func(tx *sql.Tx) error {
		row, err := store.ReadReviewRevisionFullTx(ctx, tx, operation, id)
		if err != nil {
			return err
		}
		wire, err = json.Marshal(store.ReviewRevisionFullWire(row))
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// ReadAbsentReviewCycleJSON returns null only for a review with no saved
// cycle, under the same exact private read lease as other editor reads.
func (s *syntheticReadSurface) ReadAbsentReviewCycleJSON(ctx context.Context, caller transport.Caller, raw any, reviewID int64) (json.RawMessage, error) {
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
	err = s.readTx(ctx, caller, expected, func(tx *sql.Tx) error {
		return store.ReadAbsentReviewCycleTx(ctx, tx, reviewID)
	})
	if err != nil {
		return nil, err
	}
	return json.RawMessage("null"), nil
}

// ReadCurrentOverviewJSON holds the same exact project lease, private source
// verification and read transaction through aggregation and serialization.
// A project without a core row has the desktop's unavailable shape.
func (s *syntheticReadSurface) ReadCurrentOverviewJSON(ctx context.Context, caller transport.Caller, raw any) (json.RawMessage, error) {
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
	var wire json.RawMessage
	err = s.readTx(ctx, caller, expected, func(tx *sql.Tx) error {
		overview, err := store.ReadCurrentProjectOverviewTx(ctx, tx, expected.ProjectID)
		if err != nil {
			return err
		}
		if overview == nil {
			wire = json.RawMessage(`{"state":"unavailable"}`)
			return nil
		}
		wire, err = json.Marshal(overview)
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

const syntheticMaxArchCheckpointBytes = 1 << 20

// ReadArchCheckpoint reads one fixed legacy marker twice under the exact
// active read lease; a second descriptor read detects replacement or in-place
// edits before the value leaves the private, source-verified lifecycle.
func (s *syntheticReadSurface) ReadArchCheckpoint(ctx context.Context, caller transport.Caller, raw any) (json.RawMessage, bool, error) {
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
	var result json.RawMessage
	var found bool
	err = s.readTx(ctx, caller, expected, func(_ *sql.Tx) error {
		project, err := s.library.OpenProject(expected.ProjectPath)
		if err != nil {
			return err
		}
		if project.ProjectID() != expected.ProjectID {
			_ = project.Close()
			return errors.New("PROJECT_SESSION_STALE")
		}
		first, firstFound, err := readArchCheckpointFile(ctx, project)
		if err != nil {
			_ = project.Close()
			return err
		}
		second, secondFound, err := readArchCheckpointFile(ctx, project)
		closeErr := project.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if firstFound != secondFound || !bytes.Equal(first, second) {
			return authorruntime.ErrChanged
		}
		if !firstFound || !json.Valid(first) {
			return nil
		}
		result = json.RawMessage(first)
		found = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, found, nil
}

// readArchCheckpointFile reads one fixed legacy marker under the same project
// capability and mirrors the runtime's size and validity gates.
func readArchCheckpointFile(ctx context.Context, project interface {
	OpenFileRead(string) (*os.File, error)
}) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	file, err := project.OpenFileRead("partial_arch.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() > syntheticMaxArchCheckpointBytes {
		return nil, false, authorruntime.ErrInvalid
	}
	contents, err := io.ReadAll(io.LimitReader(file, syntheticMaxArchCheckpointBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(contents) > syntheticMaxArchCheckpointBytes {
		return nil, false, authorruntime.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return contents, true, nil
}

// RecentProjects projects private catalog metadata into the browser DTO.
// Process-local catalog keys are never included in this navigation response.
func (s *syntheticReadSurface) RecentProjects(ctx context.Context, _ transport.Caller) ([]transport.RecentNavigationProject, error) {
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
func (s *syntheticReadSurface) ListDirectory(ctx context.Context, caller transport.Caller, rawExpected any, relative string) ([]projectfs.VisibleNode, error) {
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
	err = s.readTx(ctx, caller, expected, func(_ *sql.Tx) error {
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

// CloseReadOnlyProject removes only this cookie/tab's exact admitted lease.
// The process-wide legacy lock remains held for other project owners and is
// released by Close during gateway shutdown. It is bound only on the
// read-only lane: the durable lane binds db:close through BindProjects and
// must never double-bind it.
func (s *syntheticReadSurface) CloseReadOnlyProject(ctx context.Context, caller transport.Caller, path string, raw any) (any, error) {
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
