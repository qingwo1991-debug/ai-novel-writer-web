//go:build linux

package authorruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func pinnedRecoveryReader(t *testing.T, catalog *projectcatalog.Store, key string, config projectaccess.Config) *Reader {
	t.Helper()
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	project, err := catalog.Resolve(key)
	if err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	reader, err := openReadOnlyPinnedProjectUnderLock(context.Background(), held, project, config)
	if err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close recovery reader: %v", err)
			return
		}
		if err := held.Close(); err != nil {
			t.Errorf("close legacy lock: %v", err)
		}
	})
	return reader
}

func TestPromoteRecoveryReaderToCatalogKey(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader := pinnedRecoveryReader(t, catalog, key, config)
	if err := reader.promoteCatalogKey(context.Background(), catalog, key); err != nil {
		t.Fatal(err)
	}
	if err := reader.Verify(context.Background()); err != nil {
		t.Fatalf("promoted reader verification: %v", err)
	}
	if err := reader.promoteCatalogKey(context.Background(), catalog, key); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second promotion must be rejected: %v", err)
	}
	if err := reader.Verify(context.Background()); err != nil {
		t.Fatalf("second promotion changed established read authority: %v", err)
	}
}

func TestPromoteRecoveryReaderRejectsDifferentPinnedProject(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader := pinnedRecoveryReader(t, catalog, key, config)
	otherCatalog, otherKey, _, _, _ := readFixture(t)
	if err := reader.promoteCatalogKey(context.Background(), otherCatalog, otherKey); !errors.Is(err, ErrChanged) {
		t.Fatalf("different project key must fail closed: %v", err)
	}
	if err := reader.Verify(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("failed promotion retained catalog-free read authority: %v", err)
	}
}
