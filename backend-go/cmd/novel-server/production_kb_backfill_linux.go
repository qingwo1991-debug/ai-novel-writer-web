//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves kb:backfill-vectors. It has two honest branches:
//
//  1. No embedding model is configured (or the committed config names none that
//     is usable). updateVectors returns
//     errProductionKnowledgeEmbeddingUnconfigured before reading or writing
//     anything, and the answer is the reference's own up-to-date branch:
//     `{ success: true, processed: 0, failed: 0 }` (knowledge-base.ts:788-790).
//     The whole store, including the canonical snapshot, stays byte-identical,
//     which is what the FTS-only lane has always answered.
//
//  2. An embedding model is configured. The active space's missing chunks are
//     embedded through the committed model-egress policy and written to
//     internal/knowledgevector's store; the answer is
//     `{ success, processed, failed }` with real counts. A batch that fails is
//     counted in `failed` (and named symbolically in `error`), the chunks it
//     could not cover stay vectorless, and the space stays building so a search
//     degrades to FTS rather than mixing distances.
//
// The handler validates the frozen project path through
// productionKnowledgeRequestPath - the same PROJECT_CONTEXT_CHANGED fence every
// knowledge name uses - and opens the store only through the lane's own
// productionKnowledgeOpenStorage/refuseLegacyVectors pair, so the
// LEGACY_VECTOR_MIGRATION_BLOCKED guard is unchanged.
func (k *productionKnowledgeSearches) backfillVectors(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	projectPath, err := productionKnowledgeRequestPath(args[0], contextValue)
	if err != nil {
		return nil, err
	}
	if k == nil || k.imports == nil {
		return map[string]any{"success": true, "processed": int64(0), "failed": int64(0)}, nil
	}
	release, err := k.imports.enter(projectPath)
	if err != nil {
		return nil, err
	}
	defer release()
	storage, err := productionKnowledgeOpenStorage(projectPath)
	if err != nil {
		return map[string]any{"success": true, "processed": int64(0), "failed": int64(0)}, nil
	}
	defer func() { _ = storage.Close() }()
	if err := storage.refuseLegacyVectors(); err != nil {
		return productionKnowledgeImportFailure(err), nil
	}
	index, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return map[string]any{"success": true, "processed": int64(0), "failed": int64(0)}, nil
	}
	processed, failed, updateErr := k.imports.updateVectors(ctx, storage, index)
	if errors.Is(updateErr, errProductionKnowledgeEmbeddingUnconfigured) {
		return map[string]any{"success": true, "processed": int64(0), "failed": int64(0)}, nil
	}
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	answer := map[string]any{
		"success":   updateErr == nil && failed == 0,
		"processed": processed,
		"failed":    failed,
	}
	if updateErr != nil {
		answer["error"] = updateErr.Error()
		return answer, nil
	}
	if err := storage.verify(); err != nil {
		return nil, err
	}
	return answer, nil
}
