package admission

import (
	"context"
)

// UpgradeSnapshotChain advances a caller-owned, exclusive PRIVATE copy through
// the installed narrow migration lanes to v7. It never copies an author source,
// establishes path provenance, or authorizes publication. The caller must keep
// the original and discard the entire private copy after ANY error: an earlier
// step may already have committed even when a later step is refused. Populated
// histories outside the narrow individual lanes remain unsupported.
func UpgradeSnapshotChain(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
	var zero Evidence
	if err := validateContract(expected); err != nil {
		return zero, err
	}
	if ctx == nil {
		return zero, failure(CodeUpgradeUnsupported, nil)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	absolute, err := snapshotPath(path)
	if err != nil {
		return zero, err
	}
	if err := privateUpgradePath(absolute); err != nil {
		return zero, err
	}
	before, err := snapshotIdentity(ctx, absolute)
	if err != nil {
		return zero, err
	}
	db, err := openEarlySnapshot(absolute, true)
	if err != nil {
		return zero, failure(CodeSnapshotRead, err)
	}
	var version int
	readErr := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	closeErr := db.Close()
	if readErr != nil {
		return zero, failure(CodeIntegrity, readErr)
	}
	if closeErr != nil {
		return zero, failure(CodeSnapshotRead, closeErr)
	}
	after, err := snapshotIdentity(ctx, absolute)
	if err != nil {
		return zero, err
	}
	if after != before {
		return zero, failure(CodeSnapshotChanged, nil)
	}
	if version < 0 {
		return zero, failure(CodeInvalidVersion, nil)
	}
	if version > CurrentSchemaVersion {
		return zero, failure(CodeNewerSchema, nil)
	}
	steps := [...]func(context.Context, string, SchemaContract) (Evidence, error){
		UpgradeSnapshotV0, UpgradeSnapshotV1, UpgradeSnapshotV2,
		UpgradeSnapshotV3, UpgradeSnapshotV4, UpgradeSnapshotV5,
		UpgradeSnapshotV6,
	}
	for version < CurrentSchemaVersion {
		proof, err := steps[version](ctx, absolute, expected)
		if err != nil {
			return zero, err
		}
		if proof.UserVersion != version+1 {
			return zero, failure(CodeUpgradeFailed, nil)
		}
		version++
	}
	return VerifySnapshotWithDomain(ctx, absolute, expected)
}
