//go:build !lance_native || !cgo

package projectaccess

import (
	"context"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

func nativeKnowledgeSupported() bool { return false }
func inspectNativeKnowledge(context.Context, *stagedKnowledge, KnowledgeConfig) (knowledgedomain.Snapshot, knowledgedomain.Summary, error) {
	return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, ErrConfiguration
}
