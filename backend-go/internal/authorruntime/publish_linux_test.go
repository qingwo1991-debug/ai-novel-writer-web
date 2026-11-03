//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestSealedDatabaseSnapshotRetainsExactPrivateBytesUnderLegacyLock(t *testing.T) {
	catalog, key, config, source, original := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	sealed, err := reader.SealDatabaseSnapshot(context.Background(), parent, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sealed.Close()
	manifest := sealed.Manifest()
	if manifest.Version != 1 || manifest.ProjectID == "" || len(manifest.Members) != 1 || manifest.Members[0].Name != "snapshot.db" {
		t.Fatalf("unexpected membership: %+v", manifest)
	}
	digest := sha256.Sum256(original)
	if manifest.Members[0].Bytes != int64(len(original)) || manifest.Members[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("copy identity differs from source: %+v", manifest.Members[0])
	}
	copyBytes, err := os.ReadFile(filepath.Join(sealed.Directory(), "snapshot.db"))
	if err != nil || !bytes.Equal(copyBytes, original) {
		t.Fatal("private staged bytes differ", err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(sealed.Directory(), sealedSnapshotManifest))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk DatabaseSnapshotManifest
	if err := json.Unmarshal(manifestBytes, &onDisk); err != nil || onDisk.ProjectID != manifest.ProjectID || len(onDisk.Members) != len(manifest.Members) {
		t.Fatalf("sealed manifest: %+v %v", onDisk, err)
	}
	if err := sealed.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("lock released while sealed backup was prepared: %v", err)
	}
	actual, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatal("author source changed", err)
	}
	if err := sealed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sealed.Directory(), "snapshot.db")); err != nil {
		t.Fatal("Close removed durable private copy", err)
	}
	if err := sealed.Verify(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed evidence verified: %v", err)
	}
}

func TestSealedDatabaseSnapshotRejectsPrivateTamperingAndMembershipDrift(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	sealed, err := reader.SealDatabaseSnapshot(context.Background(), parent, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sealed.Close()
	if err := os.WriteFile(filepath.Join(sealed.Directory(), "snapshot.db-wal"), []byte("foreign WAL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sealed.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("new private WAL accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(sealed.Directory(), "snapshot.db-wal")); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(sealed.Directory(), "snapshot.db"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sealed.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed private bytes accepted: %v", err)
	}
}

func TestSealedDatabaseSnapshotRefusesUnsafeParentAndSourceDrift(t *testing.T) {
	catalog, key, config, source, _ := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	inside := filepath.Join(config.DataRoot, "unauthorized-backup")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.SealDatabaseSnapshot(context.Background(), inside, 64<<20); !errors.Is(err, ErrSnapshotUnsafe) {
		t.Fatalf("backup under author root admitted: %v", err)
	}
	unsafe := t.TempDir()
	if err := os.Chmod(unsafe, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.SealDatabaseSnapshot(context.Background(), unsafe, 64<<20); !errors.Is(err, ErrSnapshotUnsafe) {
		t.Fatalf("public backup parent admitted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.SealDatabaseSnapshot(ctx, unsafe, 64<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backup admitted: %v", err)
	}
	private := t.TempDir()
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(source, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("noncooperating synthetic source change")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.SealDatabaseSnapshot(context.Background(), private, 64<<20); !errors.Is(err, ErrChanged) {
		t.Fatalf("source drift accepted: %v", err)
	}
}
