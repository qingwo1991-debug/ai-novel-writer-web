//go:build linux

package main

import (
	"context"
	"database/sql"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves kb:backfill-vectors, which production_knowledge_search_linux.go
// used to refuse at GO_OPERATION_NOT_MIGRATED. The refusal claimed the lane could
// not backfill vectors because no vector store exists. That is also the honest
// SUCCESS answer: with no vector store there is nothing to backfill, and the
// reference itself answers `{ success: true, processed: 0, failed: 0 }` for every
// branch where no embeddings have to be written (knowledge-base.ts:761-762,
// :788-790, :791-795). This lane is permanently FTS-only: its store carries
// `registry: null` and no space table (production_knowledge_import_linux.go:78-102),
// and its bridge has no vector query executor
// (internal/knowledgequery/vector_plan.go:19-27).
//
// What the answer means
//
// processed:0 states what this lane did - zero vectors backfilled, zero rows
// written - and never claims vectors exist. The renderer reads the count as
// advisory: use-knowledge-base.ts:96-99 shows a success toast whose text depends
// only on `processed === 0` ("检查完成，当前检索模型不需要重建" / "Check complete.
// The current retrieval model needs no rebuild.") and never blocks the UI on a
// pending-vector count. That zero-count branch is exactly this answer, so serving
// it cannot stall the renderer.
//
// The handler validates the frozen project path through
// productionKnowledgeRequestPath - the same PROJECT_CONTEXT_CHANGED fence every
// knowledge name uses - and returns the honest no-op. It opens no store and writes
// nothing, so the foreign-project and stale-lease refusals that fence it run
// before any byte of the author's store is reached.
func (k *productionKnowledgeSearches) backfillVectors(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	if _, err := productionKnowledgeRequestPath(args[0], contextValue); err != nil {
		return nil, err
	}
	return map[string]any{
		"success":   true,
		"processed": int64(0),
		"failed":    int64(0),
	}, nil
}
