//go:build !lance_native || !cgo

package projectaccess

import (
	"context"
	"errors"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeKnowledgeRequiresExplicitNativeBuildBeforeFilesystem(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "data-not-created")
	c := Config{DataRoot: missing, LibraryRoot: filepath.Join(missing, "projects"), ProjectPath: filepath.Join(missing, "projects/synthetic"), ScratchParent: filepath.Join(root, "scratch-not-created"), MaxDatabaseBytes: 1, Knowledge: &KnowledgeConfig{Native: true}} // Other knowledge limits must be valid so refusal is specifically native build gating.
	c.Knowledge.Copy = projectfs.KnowledgeBudget{MaxBytes: 1024, MaxEntries: 10, MaxDepth: 4}
	c.Knowledge.Metadata = knowledgecopy.Budget{MaxMetadataBytes: 1024, MaxCopies: 10}
	report, err := Preflight(context.Background(), c)
	if !errors.Is(err, ErrConfiguration) || report.ProjectID != "" {
		t.Fatal(report, err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("native gate touched missing data root", err)
	}
}
