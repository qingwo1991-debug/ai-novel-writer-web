package projectaccess

import (
	"bytes"
	"context"
	"errors"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func accessFixture(t *testing.T) Config {
	t.Helper()
	oracle := os.Getenv("GO_SQLITE_CONTRACT")
	if oracle == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original oracle required")
		}
		t.Skip("GitHub original schema contract required")
	}
	f, err := os.Open(oracle)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := admission.ParseContract(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	library := filepath.Join(dataRoot, "projects")
	project := filepath.Join(library, "synthetic-author")
	storage := filepath.Join(project, ".ai-novel")
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schemaVersion":1,"kind":"ai-novel-project","projectId":"8b5929e6-cda4-43e2-a15c-f232571f0036","createdAt":"2026-01-01T00:00:00.000Z","storageFormat":"ai-novel","storageVersion":1}`)
	writeAccess(t, filepath.Join(storage, "project.json"), manifest)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(oracle), "baseline-v7.db"))
	if err != nil {
		t.Fatal(err)
	}
	writeAccess(t, filepath.Join(storage, "project.db"), data)
	scratch := t.TempDir()
	if err := os.Chmod(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	return Config{DataRoot: dataRoot, LibraryRoot: library, ProjectPath: project, ScratchParent: scratch, MaxDatabaseBytes: 64 * 1024 * 1024, Schema: schema}
}
func writeAccess(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.WriteFile(path, value, 0600); err != nil {
		t.Fatal(err)
	}
}
func sourceBytes(t *testing.T, c Config) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.ProjectPath, ".ai-novel", "project.db"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func accessClean(t *testing.T, c Config) {
	t.Helper()
	entries, err := os.ReadDir(c.ScratchParent)
	if err != nil || len(entries) != 0 {
		t.Fatal("scratch leaked", entries, err)
	}
	guard, err := writerlock.AcquireLegacy(c.DataRoot)
	if err != nil {
		t.Fatal("writer leaked", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestPreflightReadOnlyEvidenceAndCleanup(t *testing.T) {
	c := accessFixture(t)
	before := sourceBytes(t, c)
	for range 2 {
		report, err := Preflight(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if !report.LegacyGatewayExcluded || report.ProjectID == "" || report.SQLite.UserVersion != 7 || report.SQLite.ContentIdentity.Database == "" {
			t.Fatalf("incomplete evidence %+v", report)
		}
		accessClean(t, c)
	}
	if !bytes.Equal(before, sourceBytes(t, c)) {
		t.Fatal("source was changed")
	}
}
func TestPreflightExclusionHeldAcrossSQLiteAdmission(t *testing.T) {
	c := accessFixture(t)
	reached := make(chan struct{})
	resume := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	var got error
	finished := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(finished)
		_, got = preflight(context.Background(), c, func(*projectfs.Project) error {
			close(reached)
			select {
			case <-resume:
				return nil
			case <-time.After(15 * time.Second):
				return errors.New("SYNTHETIC_HOLD_TIMEOUT")
			}
		})
	}()
	select {
	case <-reached:
	case <-finished:
		t.Fatalf("failed before hold: %v", got)
	case <-time.After(20 * time.Second):
		t.Fatal("preflight did not reach hold")
	}
	if _, err := Preflight(context.Background(), c); !errors.Is(err, writerlock.ErrLocked) {
		t.Error("second preflight did not lose", err)
	}
	if _, err := writerlock.AcquireLegacy(c.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		t.Error("gateway not excluded", err)
	}
	close(resume)
	wg.Wait()
	if got != nil {
		t.Fatal(got)
	}
	accessClean(t, c)
}
func TestPreflightSourceDriftAndCancellation(t *testing.T) {
	for _, kind := range []string{"db", "manifest", "authority", "lock-replaced", "cancel", "schema", "budget", "hook-error"} {
		t.Run(kind, func(t *testing.T) {
			c := accessFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "schema" {
				c.Schema = nil
			}
			if kind == "budget" {
				c.MaxDatabaseBytes = 1
			}
			hook := func(*projectfs.Project) error {
				switch kind {
				case "db":
					path := filepath.Join(c.ProjectPath, ".ai-novel", "project.db")
					file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						return err
					}
					_, err = file.Write([]byte("synthetic extra"))
					file.Close()
					return err
				case "manifest":
					path := filepath.Join(c.ProjectPath, ".ai-novel", "project.json")
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					return os.WriteFile(path, append(data, ' '), 0600)
				case "authority":
					writeAccess(t, filepath.Join(c.ProjectPath, ".ai-novel", "portable-runtime-freeze.json"), []byte("malformed reject without parsing"))
				case "lock-replaced":
					path := filepath.Join(c.DataRoot, ".native-web.gateway.lock")
					if err := os.Rename(path, path+".old"); err != nil {
						return err
					}
					return os.WriteFile(path, nil, 0600)
				case "cancel":
					cancel()
				case "hook-error":
					return errors.New("SYNTHETIC_AFTER_STAGE_STOP")
				}
				return nil
			}
			report, err := preflight(ctx, c, hook)
			if err == nil || report.ProjectID != "" || report.LegacyGatewayExcluded {
				t.Fatal("refusal returned capability/evidence", report, err)
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if kind == "authority" && !errors.Is(err, projectfs.ErrPortableAuthorityUnsupported) {
				t.Fatal(err)
			}
			accessClean(t, c)
		})
	}
}
func TestPreflightRejectsMarkersBeforeStaging(t *testing.T) {
	for _, name := range []string{"portable-runtime-freeze.json", "portable-transfer-authority.json"} {
		t.Run(name, func(t *testing.T) {
			c := accessFixture(t)
			path := filepath.Join(c.ProjectPath, ".ai-novel", name)
			writeAccess(t, path, []byte("secret marker contents never parsed"))
			before := sourceBytes(t, c)
			report, err := Preflight(context.Background(), c)
			if !errors.Is(err, projectfs.ErrPortableAuthorityUnsupported) || report.ProjectID != "" {
				t.Fatal(report, err)
			}
			if !bytes.Equal(before, sourceBytes(t, c)) {
				t.Fatal("original changed")
			}
			accessClean(t, c)
		})
	}
}
func TestPreflightRenamedScratchClearsAuthorBytesWithoutDeletingReplacement(t *testing.T) {
	c := accessFixture(t)
	renamed := ""
	report, err := preflight(context.Background(), c, func(*projectfs.Project) error {
		entries, err := os.ReadDir(c.ScratchParent)
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			return errors.New("SYNTHETIC_TEMP_COUNT")
		}
		original := filepath.Join(c.ScratchParent, entries[0].Name())
		renamed = original + "-moved"
		if err := os.Rename(original, renamed); err != nil {
			return err
		}
		if err := os.Mkdir(original, 0700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(original, "replacement-owned"), []byte("must not delete"), 0600)
	})
	if !errors.Is(err, ErrCleanup) || report.ProjectID != "" {
		t.Fatal(report, err)
	}
	entries, err := os.ReadDir(renamed)
	if err != nil || len(entries) != 0 {
		t.Fatal("author snapshot bytes retained after rename", entries, err)
	}
	all, err := os.ReadDir(c.ScratchParent)
	if err != nil || len(all) != 2 {
		t.Fatal("replacement removed", all, err)
	}
}
func TestPreflightConfigurationAndEarlyCancellation(t *testing.T) {
	c := accessFixture(t)
	for _, change := range []func(*Config){func(v *Config) { v.DataRoot = "relative" }, func(v *Config) { v.LibraryRoot = v.DataRoot }, func(v *Config) { v.ProjectPath = v.LibraryRoot }, func(v *Config) { v.ScratchParent = filepath.Join(v.DataRoot, "scratch") }, func(v *Config) { v.MaxDatabaseBytes = 0 }} {
		bad := c
		change(&bad)
		if _, err := Preflight(context.Background(), bad); !errors.Is(err, ErrConfiguration) {
			t.Fatal(err)
		}
	}
	if _, err := Preflight(nil, c); !errors.Is(err, ErrConfiguration) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Preflight(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(c.DataRoot, ".native-web.gateway.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("early refusal touched root", err)
	}
}
func TestPreflightRejectsDualRootMigrationAndSymlink(t *testing.T) {
	for _, kind := range []string{"vela", "pending-journal", "library-symlink", "scratch-symlink", "scratch-permissions"} {
		t.Run(kind, func(t *testing.T) {
			c := accessFixture(t)
			switch kind {
			case "vela":
				if err := os.Mkdir(filepath.Join(c.ProjectPath, ".vela"), 0700); err != nil {
					t.Fatal(err)
				}
			case "pending-journal":
				dir := filepath.Join(c.ProjectPath, ".ai-novel-migration")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				writeAccess(t, filepath.Join(dir, "journal.json"), []byte(`{"version":1,"phase":"copying"}`))
			case "library-symlink":
				original := c.LibraryRoot
				if err := os.Rename(original, original+"-physical"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original+"-physical", original); err != nil {
					t.Fatal(err)
				}
			case "scratch-symlink":
				original := c.ScratchParent
				if err := os.Rename(original, original+"-physical"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original+"-physical", original); err != nil {
					t.Fatal(err)
				}
			case "scratch-permissions":
				if err := os.Chmod(c.ScratchParent, 0755); err != nil {
					t.Fatal(err)
				}
			}
			report, err := Preflight(context.Background(), c)
			if err == nil || report.ProjectID != "" {
				t.Fatal(report, err)
			}
			accessClean(t, c)
		})
	}
}
