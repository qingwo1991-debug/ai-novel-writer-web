//go:build linux

package authorruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

var ErrWriteEvidenceIncomplete = errors.New("AUTHOR_WRITE_EVIDENCE_INCOMPLETE")
var ErrWriteKnowledgeUnproved = errors.New("AUTHOR_WRITE_KNOWLEDGE_UNPROVED")

// AssessSQLiteWriteCandidate adds a full logical row proof to the already
// admitted private SQLite copy. It returns evidence, never a write capability.
// A future journaled publisher must independently prove a safe writable source
// handle, durable rollback, complete knowledge authority, and lifecycle fencing
// before it may update the original author database.
//
// The knowledge preflight is mandatory even when the canonical assets appear
// absent: a fixed-entry inventory alone does not reject legacy knowledge
// receipts or prove the copied metadata domain. Populated knowledge requires
// complete native authority that this candidate lane cannot yet establish.
func (reader *Reader) AssessSQLiteWriteCandidate(ctx context.Context, schema admission.SchemaContract) (admission.Evidence, error) {
	if reader == nil || ctx == nil {
		return admission.Evidence{}, ErrInvalid
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if err := reader.verifyLocked(ctx); err != nil {
		return admission.Evidence{}, err
	}
	if reader.report.SQLite.UserVersion != admission.CurrentSchemaVersion || !reader.report.LegacyGatewayExcluded || reader.report.Knowledge == nil {
		return admission.Evidence{}, ErrWriteEvidenceIncomplete
	}
	assets := reader.report.CanonicalAssets
	knowledge := reader.report.Knowledge
	if assets.LanceDB || assets.EmbeddingRegistry || assets.KnowledgeCopies ||
		knowledge.Physical.LanceDB || knowledge.Physical.EmbeddingRegistry || knowledge.Physical.KnowledgeCopies || knowledge.Physical.LegacyReceipt ||
		knowledge.Metadata.RegistryPresent || knowledge.Metadata.CopiesPresent {
		return admission.Evidence{}, ErrWriteKnowledgeUnproved
	}
	// VerifySnapshotWithDomain opens by pathname. Check that this private
	// directory still names the pinned inode before and after that call.
	before, err := os.Lstat(reader.directory)
	if err != nil || !os.SameFile(before, reader.directoryInfo) {
		return admission.Evidence{}, ErrChanged
	}
	evidence, err := admission.VerifySnapshotWithDomain(ctx, filepath.Join(reader.directory, "snapshot.db"), schema)
	if err != nil {
		return admission.Evidence{}, err
	}
	after, err := os.Lstat(reader.directory)
	if err != nil || !os.SameFile(after, reader.directoryInfo) {
		return admission.Evidence{}, ErrChanged
	}
	if evidence.UserVersion != admission.CurrentSchemaVersion || evidence.Domain == nil ||
		evidence.SchemaFingerprint != reader.report.SQLite.SchemaFingerprint ||
		evidence.ContentIdentity != reader.report.SQLite.ContentIdentity {
		return admission.Evidence{}, ErrWriteEvidenceIncomplete
	}
	if err := reader.verifyLocked(ctx); err != nil {
		return admission.Evidence{}, err
	}
	return evidence, nil
}
