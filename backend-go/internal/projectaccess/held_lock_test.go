package projectaccess

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestPreflightUnderLockKeepsExclusionAcrossAdmissions(t *testing.T) {
	config := accessFixture(t)
	before := sourceBytes(t, config)
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for range 2 {
		report, err := PreflightUnderLock(context.Background(), config, held)
		if err != nil || !report.LegacyGatewayExcluded || report.SQLite.UserVersion != 7 {
			t.Fatalf("held admission failed: %+v %v", report, err)
		}
		if _, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
			t.Fatalf("admission released caller lock: %v", err)
		}
	}
	bad := config
	bad.Schema = nil
	if report, err := PreflightUnderLock(context.Background(), bad, held); err == nil || report.ProjectID != "" {
		t.Fatalf("bad schema admitted: %+v %v", report, err)
	}
	if err := held.VerifyLegacy(config.DataRoot); err != nil {
		t.Fatalf("failure closed caller lock: %v", err)
	}
	if !bytes.Equal(before, sourceBytes(t, config)) {
		t.Fatal("author database changed")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	accessClean(t, config)
}

func TestPreflightUnderLockRequiresExactLiveLegacyRoot(t *testing.T) {
	config := accessFixture(t)
	if _, err := PreflightUnderLock(context.Background(), config, nil); !errors.Is(err, writerlock.ErrUnsafe) {
		t.Fatalf("nil lock accepted: %v", err)
	}
	private, err := writerlock.Acquire(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := PreflightUnderLock(context.Background(), config, private); !errors.Is(err, writerlock.ErrUnsafe) || report.ProjectID != "" {
		t.Fatalf("wrong policy accepted: %+v %v", report, err)
	}
	private.Close()
	other := accessFixture(t)
	// Both configurations are valid. Reusing one root's lock for another
	// must fail before any project or scratch staging.
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := PreflightUnderLock(context.Background(), other, held); !errors.Is(err, writerlock.ErrUnsafe) || report.ProjectID != "" {
		t.Fatalf("wrong root accepted: %+v %v", report, err)
	}
	held.Close()
	if report, err := PreflightUnderLock(context.Background(), config, held); !errors.Is(err, writerlock.ErrUnsafe) || report.ProjectID != "" {
		t.Fatalf("closed lock accepted: %+v %v", report, err)
	}
	accessClean(t, config)
	accessClean(t, other)
}
