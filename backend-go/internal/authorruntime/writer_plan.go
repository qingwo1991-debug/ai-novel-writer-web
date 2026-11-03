//go:build linux

package authorruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

var ErrPublicationPlanIncomplete = errors.New("AUTHOR_PUBLICATION_PLAN_INCOMPLETE")
var ErrPublicationAuthorityUnavailable = errors.New("AUTHOR_PUBLICATION_AUTHORITY_UNAVAILABLE")

// SQLitePublicationPlan binds a logical row proof to an exact, fsynced private
// database file set and the Reader's continuously held legacy gateway lock.
// It is preparatory evidence, not a writable handle or a restore capability.
// A future publisher must retain this Reader and lock through CAS, source
// readback, durable rollback finalization and cleanup. It must independently
// provide a pinned writable source, a complete project rollback protocol and
// knowledge-asset publication authority before any author write is attempted.
type SQLitePublicationPlan struct {
	reader   *Reader
	backup   *SealedDatabaseSnapshot
	schema   admission.SchemaContract
	evidence admission.Evidence
	manifest DatabaseSnapshotManifest
}

// PrepareSQLitePublicationPlan only accepts a backup produced from this exact
// Reader. Physical DB/WAL identities must match the independently proved
// logical snapshot. No source SQLite connection or store repository is opened.
func (reader *Reader) PrepareSQLitePublicationPlan(ctx context.Context, schema admission.SchemaContract, backup *SealedDatabaseSnapshot) (*SQLitePublicationPlan, error) {
	if reader == nil || ctx == nil || backup == nil || backup.reader != reader {
		return nil, ErrPublicationPlanIncomplete
	}
	contract := cloneSchemaContract(schema)
	if err := backup.Verify(ctx); err != nil {
		return nil, err
	}
	evidence, err := reader.AssessSQLiteWriteCandidate(ctx, contract)
	if err != nil {
		return nil, err
	}
	manifest := backup.Manifest()
	if manifest.ProjectID != reader.report.ProjectID || !publicationPhysicalIdentity(manifest, evidence) {
		return nil, ErrPublicationPlanIncomplete
	}
	if err := backup.Verify(ctx); err != nil {
		return nil, err
	}
	return &SQLitePublicationPlan{
		reader: reader, backup: backup, schema: contract,
		evidence: clonePublicationEvidence(evidence), manifest: manifest,
	}, nil
}

// Verify rechecks source, lock, private bytes, knowledge preflight and the
// full logical row digest. It must be called at every future publish boundary;
// a successful call by itself does not grant source write authority.
func (plan *SQLitePublicationPlan) Verify(ctx context.Context) error {
	if plan == nil || plan.reader == nil || plan.backup == nil || ctx == nil || plan.backup.reader != plan.reader {
		return ErrPublicationPlanIncomplete
	}
	if err := plan.backup.Verify(ctx); err != nil {
		return err
	}
	current, err := plan.reader.AssessSQLiteWriteCandidate(ctx, plan.schema)
	if err != nil {
		return err
	}
	if plan.manifest.ProjectID != plan.reader.report.ProjectID || !samePublicationEvidence(current, plan.evidence) || !sameDatabaseManifest(plan.backup.Manifest(), plan.manifest) || !publicationPhysicalIdentity(plan.manifest, current) {
		return ErrPublicationPlanIncomplete
	}
	return plan.backup.Verify(ctx)
}

// PublicationAuthority always fails until a separate source-write and restore
// implementation proves transaction fencing, durable rollback and complete
// knowledge authority. Existing store CAS methods must run only inside that
// future implementation, never through this read-only plan.
func (plan *SQLitePublicationPlan) PublicationAuthority(ctx context.Context) error {
	if err := plan.Verify(ctx); err != nil {
		return err
	}
	return ErrPublicationAuthorityUnavailable
}

// Evidence returns a defensive copy for diagnostics, never a write token.
func (plan *SQLitePublicationPlan) Evidence() (admission.Evidence, error) {
	if plan == nil || plan.reader == nil {
		return admission.Evidence{}, ErrPublicationPlanIncomplete
	}
	return clonePublicationEvidence(plan.evidence), nil
}

func publicationPhysicalIdentity(manifest DatabaseSnapshotManifest, evidence admission.Evidence) bool {
	if manifest.Version != 1 || manifest.ProjectID == "" || evidence.Domain == nil || evidence.Domain.AuthorContentHash == "" || len(manifest.Members) == 0 {
		return false
	}
	db, wal := "", ""
	for _, member := range manifest.Members {
		switch member.Name {
		case "snapshot.db":
			if db != "" || member.Bytes <= 0 {
				return false
			}
			db = member.SHA256
		case "snapshot.db-wal":
			if wal != "" {
				return false
			}
			wal = member.SHA256
		case "snapshot.db-shm", "snapshot.db-journal":
		default:
			return false
		}
	}
	if wal == "" {
		empty := sha256.Sum256(nil)
		wal = hex.EncodeToString(empty[:])
	}
	return db != "" && db == evidence.ContentIdentity.Database && wal == evidence.ContentIdentity.WAL
}

func sameDatabaseManifest(left, right DatabaseSnapshotManifest) bool {
	if left.Version != right.Version || left.ProjectID != right.ProjectID || len(left.Members) != len(right.Members) {
		return false
	}
	for i := range left.Members {
		if left.Members[i] != right.Members[i] {
			return false
		}
	}
	return true
}

func samePublicationEvidence(left, right admission.Evidence) bool {
	if left.UserVersion != right.UserVersion || left.SchemaFingerprint != right.SchemaFingerprint || left.ContentIdentity != right.ContentIdentity || left.Domain == nil || right.Domain == nil || left.Domain.AuthorContentHash != right.Domain.AuthorContentHash {
		return false
	}
	return samePublicationCounts(left.TableCounts, right.TableCounts) && samePublicationCounts(left.Domain.TableCounts, right.Domain.TableCounts)
}

func samePublicationCounts(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		actual, ok := right[key]
		if !ok || actual != value {
			return false
		}
	}
	return true
}

func clonePublicationEvidence(value admission.Evidence) admission.Evidence {
	copy := value
	copy.TableCounts = make(map[string]int64, len(value.TableCounts))
	for key, count := range value.TableCounts {
		copy.TableCounts[key] = count
	}
	if value.Domain != nil {
		domain := *value.Domain
		domain.TableCounts = make(map[string]int64, len(value.Domain.TableCounts))
		for key, count := range value.Domain.TableCounts {
			domain.TableCounts[key] = count
		}
		copy.Domain = &domain
	}
	return copy
}

func cloneSchemaContract(value admission.SchemaContract) admission.SchemaContract {
	copy := make(admission.SchemaContract, len(value))
	for i, entry := range value {
		copy[i] = entry
		copy[i].Fingerprints = append([]string(nil), entry.Fingerprints...)
	}
	return copy
}
