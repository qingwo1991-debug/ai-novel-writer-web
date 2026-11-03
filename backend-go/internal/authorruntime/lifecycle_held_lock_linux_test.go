//go:build linux

package authorruntime

import (
	"errors"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestReadLifecycleWithHeldLockTransfersOnlyOnSuccess(t *testing.T) {
	catalog, _, config, _, _ := readFixture(t)
	config.ProjectPath = ""
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	transferred := false
	t.Cleanup(func() {
		if !transferred {
			_ = held.Close()
		}
	})

	if _, err := NewReadLifecycleWithHeldLock(catalog, config, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil lock accepted: %v", err)
	}
	invalid := config
	invalid.ProjectPath = config.LibraryRoot
	if _, err := NewReadLifecycleWithHeldLock(catalog, invalid, held); !errors.Is(err, ErrInvalid) {
		t.Fatalf("project-scoped config accepted: %v", err)
	}
	wrongRoot := config
	wrongRoot.DataRoot = config.LibraryRoot
	if _, err := NewReadLifecycleWithHeldLock(catalog, wrongRoot, held); !errors.Is(err, writerlock.ErrUnsafe) {
		t.Fatalf("wrong data root accepted: %v", err)
	}
	if err := held.VerifyLegacy(config.DataRoot); err != nil {
		t.Fatalf("failed constructor released caller lock: %v", err)
	}

	lifecycle, err := NewReadLifecycleWithHeldLock(catalog, config, held)
	if err != nil {
		t.Fatal(err)
	}
	transferred = true
	shutdown := false
	t.Cleanup(func() {
		if !shutdown {
			_ = lifecycle.Shutdown()
		}
	})
	competing, err := writerlock.AcquireLegacy(config.DataRoot)
	if competing != nil {
		_ = competing.Close()
	}
	if !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("transferred lock no longer held: %v", err)
	}
	if err := lifecycle.Shutdown(); err != nil {
		t.Fatal(err)
	}
	shutdown = true
	if err := held.VerifyLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrUnsafe) {
		t.Fatalf("shutdown did not release transferred lock: %v", err)
	}
	reacquired, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatalf("shutdown retained legacy lock: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}
