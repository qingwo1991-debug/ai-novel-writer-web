//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func emptyKnowledgeConfig() *projectaccess.KnowledgeConfig {
	return &projectaccess.KnowledgeConfig{
		Copy:     projectfs.KnowledgeBudget{MaxBytes: 16 << 20, MaxEntries: 100, MaxDepth: 8},
		Metadata: knowledgecopy.Budget{MaxMetadataBytes: 16 << 20, MaxCopies: 100},
	}
}

func TestWriteCandidateRequiresKnowledgePreflightAndFullDomain(t *testing.T) {
	catalog, key, config, source, original := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.AssessSQLiteWriteCandidate(context.Background(), config.Schema); !errors.Is(err, ErrWriteEvidenceIncomplete) {
		t.Fatalf("fixed-entry inventory was treated as complete knowledge authority: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	config.Knowledge = emptyKnowledgeConfig()
	reader, err = OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	evidence, err := reader.AssessSQLiteWriteCandidate(context.Background(), config.Schema)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.UserVersion != admission.CurrentSchemaVersion || evidence.Domain == nil || evidence.SchemaFingerprint == "" {
		t.Fatalf("logical row proof missing: %+v", evidence)
	}
	if held, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		if err == nil {
			_ = held.Close()
		}
		t.Fatalf("writer candidate released legacy exclusion: %v", err)
	}
	actual, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatal("write assessment modified author bytes", err)
	}
}

func TestWriteCandidateRejectsSourceDriftAndClosedReader(t *testing.T) {
	catalog, key, config, source, _ := readFixture(t)
	config.Knowledge = emptyKnowledgeConfig()
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("synthetic source drift")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.AssessSQLiteWriteCandidate(context.Background(), config.Schema); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed author source admitted: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.AssessSQLiteWriteCandidate(context.Background(), config.Schema); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed reader admitted: %v", err)
	}
}
