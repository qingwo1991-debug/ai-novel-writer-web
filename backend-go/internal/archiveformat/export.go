package archiveformat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

// ExportSource is a repeatable, owner-admitted source. Open must return a reader
// for a private snapshot or an already verified asset, never a request path.
type ExportSource struct {
	Entry Entry
	Open  func() (io.ReadCloser, error)
}

// SourceEvidence is produced by the source SQLite snapshot and schema verifier.
// The codec cannot derive it from ZIP bytes.
type SourceEvidence struct {
	SchemaVersion     int    `json:"schemaVersion"`
	SchemaFingerprint string `json:"schemaFingerprint"`
	TableCount        int    `json:"tableCount"`
	FieldCount        int    `json:"fieldCount"`
}

// ExportSnapshot is the complete private-copy input supplied by a trusted
// project owner. CheckCurrent verifies the exact session/tab/frozen context and
// VerifyUnchanged checks SQLite and every asset against the first snapshot.
// Both proofs are repeated after writing and before the caller can publish.
type ExportSnapshot struct {
	Manifest          Manifest
	Sources           []ExportSource
	RuntimeFreeze     []byte
	TransferAuthority []byte
	Evidence          SourceEvidence
	CheckCurrent      func(context.Context) error
	VerifyUnchanged   func(context.Context) error
}

type ExportReceipt struct {
	OriginProjectID            string         `json:"originProjectId"`
	SnapshotGeneration         string         `json:"snapshotGeneration"`
	TargetSHA256               string         `json:"targetSha256"`
	TargetByteSize             uint64         `json:"targetByteSize"`
	SourceEvidence             SourceEvidence `json:"sourceEvidence"`
	EntryCount                 int            `json:"entryCount"`
	RequiresRuntimeFreezeGuard bool           `json:"requiresRuntimeFreezeGuard"`
}

// lazyExportReader keeps descriptor use constant while Build streams entries
// in manifest order. Build asks for one byte beyond each declared size, so an
// exact source reaches EOF and closes before the next source opens.
type lazyExportReader struct {
	open    func() (io.ReadCloser, error)
	current io.ReadCloser
	closed  bool
}

func (r *lazyExportReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.EOF
	}
	if r.current == nil {
		var err error
		r.current, err = r.open()
		if err != nil {
			return 0, err
		}
		if r.current == nil {
			return 0, failure("PORTABLE_ASSET_MISSING")
		}
	}
	n, err := r.current.Read(p)
	if err == io.EOF {
		closeErr := r.Close()
		if closeErr != nil {
			return n, closeErr
		}
	}
	return n, err
}

func (r *lazyExportReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.current != nil {
		return r.current.Close()
	}
	return nil
}

// ExportPrivate writes only to the caller's unpublished, private staging
// writer. A failed call leaves partial bytes; the owner MUST discard them.
// No author-project path is opened or written by this package.
func ExportPrivate(ctx context.Context, output io.Writer, snapshot ExportSnapshot, limits Limits) (ExportReceipt, error) {
	var empty ExportReceipt
	if ctx == nil || output == nil || snapshot.CheckCurrent == nil || snapshot.VerifyUnchanged == nil {
		return empty, failure("PORTABLE_EXPORT_AUTHORITY_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if snapshot.Evidence.SchemaVersion != 7 || !hashPattern.MatchString(snapshot.Evidence.SchemaFingerprint) || snapshot.Evidence.TableCount <= 0 || snapshot.Evidence.FieldCount <= 0 {
		return empty, failure("PORTABLE_EXPORT_SOURCE_EVIDENCE_INVALID")
	}
	m := snapshot.Manifest
	if m.SourceSchemaVersion != 7 || len(m.Entries) != len(snapshot.Sources) {
		return empty, failure("PORTABLE_ARCHIVE_INVALID")
	}
	// A portable DB and both sidecars are required for a restorable copy. Check
	// the sidecars' hashes against the manifest before opening any source stream.
	var database, freeze, transfer int
	for _, entry := range m.Entries {
		switch entry.Path {
		case "project.db":
			if entry.Disposition == "portable-database" {
				database++
			}
		case "portable-runtime-freeze.json":
			if entry.Disposition == "history-projection" && sha256Hex(snapshot.RuntimeFreeze) == entry.SHA256 && uint64(len(snapshot.RuntimeFreeze)) == entry.ByteSize {
				freeze++
			}
		case ".ai-novel/portable-transfer-authority.json":
			if entry.Disposition == "transfer-receipt" && sha256Hex(snapshot.TransferAuthority) == entry.SHA256 && uint64(len(snapshot.TransferAuthority)) == entry.ByteSize {
				transfer++
			}
		}
	}
	if database != 1 || freeze != 1 || transfer != 1 {
		return empty, failure("PORTABLE_EXPORT_AUTHORITY_REQUIRED")
	}
	_, authority, err := ValidatePortableBoundary(m, snapshot.RuntimeFreeze, snapshot.TransferAuthority)
	if err != nil {
		return empty, err
	}
	if authority == nil || len(m.TransferReceiptIDs) == 0 || m.TransferReceiptIDs[0] != authority.ReceiptID {
		return empty, failure("PORTABLE_EXPORT_AUTHORITY_REQUIRED")
	}
	if err := snapshot.CheckCurrent(ctx); err != nil {
		return empty, failure("PORTABLE_SOURCE_CHANGED")
	}
	if err := snapshot.VerifyUnchanged(ctx); err != nil {
		return empty, failure("PORTABLE_SOURCE_CHANGED")
	}
	opened := make([]*lazyExportReader, 0, len(snapshot.Sources))
	defer func() {
		for _, reader := range opened {
			_ = reader.Close()
		}
	}()
	sources := make([]Source, 0, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		if source.Open == nil {
			return empty, failure("PORTABLE_EXPORT_AUTHORITY_REQUIRED")
		}
		reader := &lazyExportReader{open: source.Open}
		opened = append(opened, reader)
		sources = append(sources, Source{Entry: source.Entry, Reader: reader})
	}
	digest := sha256.New()
	archiveReceipt, err := Build(io.MultiWriter(output, digest), m, sources, limits)
	if err != nil {
		return empty, err
	}
	if err := snapshot.CheckCurrent(ctx); err != nil {
		return empty, failure("PORTABLE_SOURCE_CHANGED")
	}
	if err := snapshot.VerifyUnchanged(ctx); err != nil {
		return empty, failure("PORTABLE_SOURCE_CHANGED")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return ExportReceipt{m.OriginProjectID, m.SnapshotGeneration, hex.EncodeToString(digest.Sum(nil)), archiveReceipt.ArchiveBytes, snapshot.Evidence, archiveReceipt.EntryCount, true}, nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// IsExportAuthorityFailure lets the transport fold proof failures to a public
// code while retaining cancellation and I/O failures as ordinary errors.
func IsExportAuthorityFailure(err error) bool {
	var portable *Error
	return errors.As(err, &portable)
}
