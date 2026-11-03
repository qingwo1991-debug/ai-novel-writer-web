//go:build linux

package authorruntime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectpublish"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// DraftCommitService is an unbound author-only coordinator. The caller supplies
// administrator-owned publication configuration, never a browser path. The
// caller must configure the catalog with a trusted switched-journal readback
// checker; this service cannot inspect the callback's implementation.
type DraftCommitService struct {
	lifecycle   *ReadLifecycle
	library     *projectfs.Library
	stageParent string
	limits      projectpublish.Limits
}

type DraftCommitRequest struct {
	Owner           projectsession.Owner
	Expected        projectsession.Context
	CatalogKey      string
	DraftID         int64
	Content         string
	WordCount       int64
	ExpectedContent *string
}

type DraftCommitResult struct {
	Context  projectsession.Context
	Key      string
	Readback SwitchedDraftReadback
}

func NewDraftCommitService(lifecycle *ReadLifecycle, library *projectfs.Library,
	stageParent string, limits projectpublish.Limits) (*DraftCommitService, error) {
	if lifecycle == nil || lifecycle.active == nil || lifecycle.catalog == nil || library == nil ||
		library.Path() != lifecycle.config.LibraryRoot || stageParent == "" ||
		limits.MaxFiles < 2 || limits.MaxBytes <= 0 || limits.MaxDepth <= 0 {
		return nil, ErrInvalid
	}
	return &DraftCommitService{lifecycle: lifecycle, library: library, stageParent: stageParent, limits: limits}, nil
}

// CommitDraftCAS coordinates one full-body CAS save. It returns a success only
// after the old source is retained, SWITCHED is durable, the new source is read
// back through a fresh private copy, and the new Reader/key are published in
// one fenced catalog operation. It is deliberately not wired to transport.
func (s *DraftCommitService) CommitDraftCAS(ctx context.Context, request DraftCommitRequest) (DraftCommitResult, error) {
	var zero DraftCommitResult
	if s == nil || s.lifecycle == nil || s.library == nil || ctx == nil ||
		request.CatalogKey == "" || request.DraftID <= 0 || request.ExpectedContent == nil {
		return zero, ErrInvalid
	}
	// Reject an ordinary stale/read-only save before entering the one-shot
	// publisher. The private staged CAS below remains the authoritative check.
	if err := s.lifecycle.PreflightDraftCAS(ctx, request.Owner, request.Expected,
		request.DraftID, request.Content, request.WordCount, request.ExpectedContent); err != nil {
		return zero, err
	}
	expectedBody := *request.ExpectedContent
	l := s.lifecycle
	var readback SwitchedDraftReadback
	var newKey string
	newContext, err := l.active.CommitDrained(ctx, request.Owner, request.Expected, request.CatalogKey,
		func(ctx context.Context, held *writerlock.Lock, fence *projectsession.PublishFence) (candidate projectsession.Admitted, clearance projectsession.CommitClearance, retErr error) {
			projectPath, projectID := request.Expected.ProjectPath, request.Expected.ProjectID
			if err := l.catalog.HoldRecovery(projectPath, projectID); err != nil {
				return nil, nil, err
			}
			snapshot, err := projectpublish.Stage(ctx, held, l.config.DataRoot, s.library,
				projectPath, s.stageParent, s.limits)
			if err != nil {
				return nil, nil, err
			}
			defer func() {
				if snapshot != nil {
					retErr = errors.Join(retErr, snapshot.Close())
				}
			}()
			evidence, err := snapshot.EditDraftCASPrivate(ctx, request.DraftID,
				request.Content, request.WordCount, &expectedBody)
			if err != nil {
				return nil, nil, err
			}
			verifyFence := func(ctx context.Context) error {
				return fence.VerifyCommit(ctx, held, request.Owner, request.Expected, request.CatalogKey)
			}
			drain := projectpublish.HandleDrainFunc(func(ctx context.Context, path string) error {
				if path != projectPath {
					return ErrChanged
				}
				return verifyFence(ctx)
			})
			current := projectpublish.CurrentDraftAuthorityFunc(func(ctx context.Context, facts projectpublish.CurrentDraftFacts) error {
				if facts.ProjectID != projectID || facts.ProjectPath != projectPath || facts.DraftID != request.DraftID {
					return ErrChanged
				}
				return verifyFence(ctx)
			})
			journal, err := projectpublish.PrepareEditedExchangeJournal(ctx, snapshot, drain, evidence)
			if err != nil {
				return nil, nil, err
			}
			plan, err := projectpublish.PrepareEditedExchangePlan(ctx, snapshot, evidence, journal, current, drain)
			if err != nil {
				return nil, nil, err
			}
			claim := projectpublish.EditedExchangeClaim{Held: held, Fence: fence, Owner: request.Owner,
				Expected: request.Expected, OldKey: request.CatalogKey}
			if err := plan.ExchangeWithFence(ctx, claim); err != nil {
				return nil, nil, err
			}
			editedHash := hex.EncodeToString(evidence.AfterStageTreeSHA256[:])
			if _, err := snapshot.RetainSwitched(ctx, editedHash); err != nil {
				return nil, nil, err
			}
			if err := journal.MarkSwitched(ctx); err != nil {
				return nil, nil, err
			}
			recovery, err := projectpublish.RecoverExchangeJournal(ctx, held, l.config.DataRoot,
				projectID, projectPath, s.stageParent, journal.Name())
			if err != nil || recovery.Class != projectpublish.JournalSwitched ||
				recovery.EditedDraftID != request.DraftID {
				return nil, nil, errors.Join(ErrSwitchedDraftUnverified, err)
			}
			config := l.config
			config.ProjectPath = projectPath
			readback, err = VerifySwitchedDraftUnderLock(ctx, held, s.library, snapshot,
				projectPath, s.stageParent, journal.Name(), config, SwitchedDraftExpectation{
					ProjectID: projectID, DraftID: request.DraftID, Content: request.Content,
					WordCount: request.WordCount, EditedTreeHash: editedHash,
					EditedOperationHash: recovery.EditedOperationHash,
				})
			if err != nil {
				return nil, nil, err
			}
			if err := snapshot.Close(); err != nil {
				return nil, nil, err
			}
			snapshot = nil
			var nonce [24]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				return nil, nil, err
			}
			project, err := s.library.OpenProject(projectPath)
			if err != nil {
				return nil, nil, err
			}
			reader, err := openReadOnlyPinnedProjectUnderLock(ctx, held, project, config)
			if err != nil {
				return nil, nil, err
			}
			fresh := &admittedRead{reader: reader, context: projectsession.Context{
				ProjectID: projectID, ProjectPath: projectPath,
				LeaseID: base64.RawURLEncoding.EncodeToString(nonce[:]),
			}}
			return fresh, func(ctx context.Context, installed projectsession.Admitted) (string, error) {
				if installed != fresh {
					return "", ErrInvalid
				}
				promoted, err := l.catalog.PromoteVerifiedRecoveryWithBinding(ctx, reader.project,
					func(key string, verified *projectfs.Project) (func(), error) {
						return reader.bindPromotedCatalogKey(ctx, l.catalog, key, verified)
					})
				if err != nil {
					return "", err
				}
				newKey = promoted.Key
				return promoted.Key, nil
			}, nil
		})
	if err != nil {
		return zero, err
	}
	return DraftCommitResult{Context: newContext, Key: newKey, Readback: readback}, nil
}
