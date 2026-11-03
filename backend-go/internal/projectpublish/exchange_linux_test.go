//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

func TestExchangePlanValidatesButCannotPublish(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	before, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	plan, err := PrepareExchangePlan(ctx, snapshot, HandleDrainFunc(func(_ context.Context, path string) error {
		called++
		if path != f.projectPath {
			t.Fatalf("drain received untrusted path %q", path)
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || plan.EntryCount() < 4 || plan.TotalBytes() <= 0 || plan.SourceTreeSHA256() == ([32]byte{}) {
		t.Fatal("incomplete exchange assessment", called, plan.EntryCount(), plan.TotalBytes())
	}
	if err := plan.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.stageParent, 0755); err != nil {
		t.Fatal(err)
	}
	if err := plan.Verify(ctx); !errors.Is(err, ErrChanged) {
		t.Fatalf("public stage parent retained exchange plan: %v", err)
	}
	if err := os.Chmod(f.stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := plan.Exchange(ctx); !errors.Is(err, ErrPublishUnavailable) {
		t.Fatalf("plan unexpectedly published: %v", err)
	}
	if err := RecoverExchange(ctx, f.projectPath); !errors.Is(err, ErrPublishUnavailable) {
		t.Fatalf("nonexistent journal was recovered: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("assessment changed author bytes: %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Verify(ctx); !errors.Is(err, ErrInvalid) {
		t.Fatalf("closed snapshot remained exchange-ready: %v", err)
	}
}

func TestExchangePlanRejectsMissingDrainAndPublicParent(t *testing.T) {
	for _, change := range []string{"nil-drain", "public-parent", "drain-refusal"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close()
			if change == "public-parent" {
				if err := os.Chmod(f.stageParent, 0755); err != nil {
					t.Fatal(err)
				}
			}
			var drain HandleDrain = HandleDrainFunc(func(context.Context, string) error { return nil })
			if change == "nil-drain" {
				drain = nil
			} else if change == "drain-refusal" {
				drain = HandleDrainFunc(func(context.Context, string) error { return ErrPublishUnavailable })
			}
			plan, err := PrepareExchangePlan(ctx, snapshot, drain)
			if plan != nil || err == nil {
				t.Fatal("unqualified exchange plan accepted", change)
			}
		})
	}
}

func TestExchangePlanAcceptsFaithfulNondefaultMode(t *testing.T) {
	f := newFixture(t)
	if err := os.Chmod(filepath.Join(f.storage, "unknown-author-file.bin"), 0640); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	mode, err := os.Stat(filepath.Join(stage, "unknown-author-file.bin"))
	if err != nil || mode.Mode().Perm() != 0640 {
		t.Fatalf("stage lost original permission mode: %v, %v", mode, err)
	}
	plan, err := PrepareExchangePlan(ctx, snapshot, HandleDrainFunc(func(context.Context, string) error { return nil }))
	if err != nil || plan == nil {
		t.Fatalf("faithful private copy refused: %v", err)
	}
	if err := plan.Exchange(ctx); !errors.Is(err, ErrPublishUnavailable) {
		t.Fatalf("preflight opened source exchange: %v", err)
	}
}

func TestExchangePlanRechecksAfterDrain(t *testing.T) {
	for _, change := range []string{"source", "private"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close()
			plan, err := PrepareExchangePlan(ctx, snapshot, HandleDrainFunc(func(context.Context, string) error {
				root := f.storage
				if change == "private" {
					privatePath, pathErr := snapshot.PrivatePath()
					if pathErr != nil {
						return pathErr
					}
					root = privatePath
				}
				return os.WriteFile(filepath.Join(root, "unknown-author-file.bin"), []byte("mutated during drain"), 0600)
			}))
			if plan != nil || !errors.Is(err, ErrChanged) {
				t.Fatalf("drain-time mutation accepted: %v", err)
			}
		})
	}
}
