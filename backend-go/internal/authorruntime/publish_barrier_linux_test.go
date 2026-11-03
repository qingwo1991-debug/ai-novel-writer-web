//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestLifecyclePublishBarrierClosesReaderAndRevokesLease(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "cookie", ClientID: "tab"}
	other := projectsession.Owner{SessionID: "other", ClientID: "tab"}
	first, err := lifecycle.Open(context.Background(), owner, key, "first")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := lifecycle.WithPublishBarrier(context.Background(), other, first, func(context.Context, *writerlock.Lock) error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatalf("unknown owner entered publish barrier: %v", err)
	}
	wrong := first
	wrong.LeaseID = "wrong"
	if err := lifecycle.WithPublishBarrier(context.Background(), owner, wrong, func(context.Context, *writerlock.Lock) error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatalf("wrong lease entered publish barrier: %v", err)
	}
	if err := lifecycle.WithPublishBarrier(context.Background(), owner, first, func(ctx context.Context, held *writerlock.Lock) error {
		called = true
		if err := held.VerifyLegacy(config.DataRoot); err != nil {
			return err
		}
		if err := lifecycle.Read(ctx, owner, first, func(*sql.Tx) error {
			return errors.New("retired reader was borrowed")
		}); err == nil {
			return errors.New("old lease served a read during publish")
		}
		if _, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
			return errors.New("legacy writer lock released during publish")
		}
		return nil
	}); err != nil || !called {
		t.Fatalf("publish barrier failed: %v", err)
	}
	if err := lifecycle.Read(context.Background(), owner, first, func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("old lease remained readable after publish")
	}
	second, err := lifecycle.Open(context.Background(), owner, key, "second")
	if err != nil {
		t.Fatal(err)
	}
	if second.LeaseID == first.LeaseID {
		t.Fatal("readmission reused old lease")
	}
	if err := lifecycle.Read(context.Background(), owner, second, func(tx *sql.Tx) error {
		var version int
		return tx.QueryRow("PRAGMA user_version").Scan(&version)
	}); err != nil {
		t.Fatal(err)
	}
}
