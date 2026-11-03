package projectaccess

import (
	"context"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgelogical"
)

// hashNativeKnowledge runs only after the native manifest, typed domain,
// registry raw bytes and managed-copy links have been independently admitted.
// It retains the bounded migration lane's explicit en-US collation profile;
// arbitrary multi-ID Unicode collections fail closed inside HashSnapshot.
func hashNativeKnowledge(ctx context.Context, snapshot knowledgedomain.Snapshot) (knowledgelogical.Summary, error) {
	budget := knowledgedomain.Budget{MaxTables: 32, MaxRows: 10000, MaxCells: 200000, MaxTextBytes: 32 * 1024 * 1024}
	return knowledgelogical.HashSnapshot(ctx, snapshot, budget, knowledgelogical.Options{Locale: "en-US"})
}
