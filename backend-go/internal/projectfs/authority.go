package projectfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ErrPortableAuthorityUnsupported is a rejection-only preflight result. Native
// operations do not yet implement portable receipt admission or runtime replay.
var ErrPortableAuthorityUnsupported = errors.New("PORTABLE_AUTHORITY_UNSUPPORTED")

// These installed names come from electron/services/portable-runtime-freeze.ts,
// portable-transfer-authority.ts and project-restore-service.ts. Archive paths
// are not installed paths: restore moves the history into canonical storage.
const (
	portableRuntimeFreezeName      = "portable-runtime-freeze.json"
	portableTransferAuthorityName  = "portable-transfer-authority.json"
	portableRestoreReservationName = ".portable-restore-reservation.json"
)

// portableRestoreJournalName mirrors installJournalPath in the original restore
// service on the supported Linux platform. Project.path is already a canonical
// absolute physical path, so normalized(targetRoot) is exactly that path. Only
// the resulting single component is used, relative to the pinned library root.
func portableRestoreJournalName(projectPath string) string {
	hash := sha256.Sum256([]byte(projectPath))
	return fmt.Sprintf(".portable-project-restore-%x.json", hash[:16])
}

// VerifyNoPortableAuthority rejects ANY presence at the original installed
// runtime-freeze, transfer-authority, restore-reservation and target-specific
// restore-journal names, including malformed JSON, symlinks (even dangling),
// directories, hardlinks, FIFOs and oversized files. It never opens or parses a
// marker, an original SQLite database, or an author-content file. Only existing
// project identity validation reads the bounded canonical manifest/migration
// journal; those are not accepted as portable authority.
//
// This is a gate, NOT an implementation of portable transfer/replay semantics.
// Call it during early freeze and again under FULL_FROZEN, BEFORE parent staging.
// The caller must continuously exclude source/restore/filesystem writers through
// staging and final verification. Repeated fixed-name sampling cannot detect a
// hostile swap-away-and-back or prove future absence. No tree is enumerated; the
// original restore's variable temporary names remain guarded by its fixed
// reservation and target-specific journal, not by walking author directories.
func (p *Project) VerifyNoPortableAuthority(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || p.library == nil {
		return ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.checkIdentityLocked(); err != nil {
		return portableAuthorityIdentityError(err)
	}
	markers := [...]struct {
		root *os.Root
		name string
	}{
		{p.storage, portableRuntimeFreezeName},
		{p.storage, portableTransferAuthorityName},
		{p.root, portableRestoreReservationName},
		{p.library.root, portableRestoreJournalName(p.path)},
	}
	// A second fixed membership pass detects observed creation after a name
	// was checked in the first pass, without reading any marker contents.
	for pass := 0; pass < 2; pass++ {
		for _, marker := range markers {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := marker.root.Lstat(marker.name); err == nil || !errors.Is(err, fs.ErrNotExist) {
				// Inspection errors are uncertainty, never proof of absence.
				// Do not expose the underlying PathError or marker contents.
				return ErrPortableAuthorityUnsupported
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.checkIdentityLocked(); err != nil {
		return portableAuthorityIdentityError(err)
	}
	return ctx.Err()
}

// Existing capability checks can return host PathErrors. This preflight exposes
// only symbolic existing project errors, with unknown filesystem failures mapped
// to changed identity. It deliberately does not wrap or join the original error.
func portableAuthorityIdentityError(err error) error {
	for _, symbolic := range []error{
		ErrClosed, ErrManifestChanged, ErrManifestInvalid, ErrMigrationRequired,
		ErrRecoveryRequired, ErrUnsafeDirectory, ErrUnsafeFile, ErrTooLarge,
		ErrInvalidPath, ErrUnsupported, ErrChanged,
	} {
		if errors.Is(err, symbolic) {
			return symbolic
		}
	}
	return ErrChanged
}
