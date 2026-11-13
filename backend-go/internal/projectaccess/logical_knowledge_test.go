package projectaccess

import (
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

func TestLogicalKnowledgeRequiresNativeAdmission(t *testing.T) {
	config := KnowledgeConfig{
		Copy:     projectfs.KnowledgeBudget{MaxBytes: 1024, MaxEntries: 10, MaxDepth: 3},
		Metadata: knowledgecopy.Budget{MaxMetadataBytes: 1024, MaxCopies: 10},
		Logical:  true,
	}
	if validKnowledgeConfig(config) {
		t.Fatal("logical hash accepted without native typed and source-copy admission")
	}
}
