//go:build linux

package projectfs

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// All fixtures are temporary synthetic bytes, never SQLite or author projects.
// Keep this list independent of the implementation's marker constants: each
// actual original location must independently reject even invalid JSON.
func authorityFixtureMarkers(library, project, storage string) map[string]string {
	return map[string]string{
		"runtime freeze": filepath.Join(storage, "portable-runtime-freeze.json"),
		"transfer authority": filepath.Join(storage, "portable-transfer-authority.json"),
		"restore reservation": filepath.Join(project, ".portable-restore-reservation.json"),
		"restore journal": filepath.Join(library, portableRestoreJournalName(project)),
	}
}

func assertAuthorityUnsupported(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrPortableAuthorityUnsupported) || err.Error() != "PORTABLE_AUTHORITY_UNSUPPORTED" {
		t.Fatalf("expected only symbolic unsupported error, got %v", err)
	}
	var pathError *fs.PathError
	if errors.As(err, &pathError) {
		t.Fatal("preflight leaked a filesystem path error")
	}
}

func TestVerifyNoPortableAuthorityNormalProjectAndNoContentReads(t *testing.T) {
	library, project, storage := fixture(t)
	_, p := openFixture(t, library, project)
	// Once admitted, this gate never opens the source database or unrelated
	// assets. Replacing them with unreadable/special files must not affect it.
	remove(t, filepath.Join(storage, DatabaseName))
	if err := syscall.Mkfifo(filepath.Join(storage, DatabaseName), 0600); err != nil {
		t.Fatal(err)
	}
	link(t, "does-not-exist", filepath.Join(storage, "unrelated-author-asset"), true)
	put(t, filepath.Join(storage, "vectors.json.migration-journal.json"), []byte("not portable authority"))
	// A valid switched migration journal remains separately admitted, not
	// conflated with portable restore's dedicated journal namespace.
	control := filepath.Join(project, ".ai-novel-migration")
	if err := os.Mkdir(control, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(control, "journal.json"), []byte(`{"version":1,"phase":"switched","projectId":"`+fixtureID+`"}`))
	// Another target's restore journal must not freeze this target/library.
	put(t, filepath.Join(library, portableRestoreJournalName(filepath.Join(library, "another synthetic target"))), []byte("{"))
	if err := p.VerifyNoPortableAuthority(context.Background()); err != nil {
		t.Fatalf("ordinary nonportable project rejected: %v", err)
	}
}

func TestVerifyNoPortableAuthorityRejectsEveryMarkerWithoutParsing(t *testing.T) {
	kinds := []string{"malformed", "empty", "null", "empty-object", "directory", "dangling-symlink", "outside-symlink", "hardlink", "fifo", "oversized", "unreadable"}
	for _, marker := range []string{"runtime freeze", "transfer authority", "restore reservation", "restore journal"} {
		for _, kind := range kinds {
			t.Run(marker+"/"+kind, func(t *testing.T) {
				library, project, storage := fixture(t)
				_, p := openFixture(t, library, project)
				file := authorityFixtureMarkers(library, project, storage)[marker]
				switch kind {
				case "malformed":
					put(t, file, []byte("{synthetic-private-content-must-not-appear"))
				case "empty":
					put(t, file, nil)
				case "null":
					put(t, file, []byte("null"))
				case "empty-object":
					put(t, file, []byte("{}"))
				case "directory":
					if err := os.Mkdir(file, 0700); err != nil {
						t.Fatal(err)
					}
				case "dangling-symlink":
					link(t, "does-not-exist", file, true)
				case "outside-symlink":
					outside := filepath.Join(t.TempDir(), "synthetic-private-content")
					put(t, outside, []byte("must-not-be-read"))
					link(t, outside, file, true)
				case "hardlink":
					outside := filepath.Join(t.TempDir(), "synthetic-hardlink-source")
					put(t, outside, []byte("must-not-be-read"))
					link(t, outside, file, false)
				case "fifo":
					if err := syscall.Mkfifo(file, 0600); err != nil {
						t.Fatal(err)
					}
				case "oversized":
					// Sparse, larger than every original authority/history bound.
					f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
					if err != nil {
						t.Fatal(err)
					}
					truncateErr := f.Truncate(32 * 1024 * 1024)
					closeErr := f.Close()
					if truncateErr != nil || closeErr != nil {
						t.Fatalf("sparse marker setup: %v %v", truncateErr, closeErr)
					}
				case "unreadable":
					put(t, file, []byte("must-not-be-read"))
					if err := os.Chmod(file, 0); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.Lstat(file)
				if err != nil {
					t.Fatal(err)
				}
				assertAuthorityUnsupported(t, p.VerifyNoPortableAuthority(context.Background()))
				after, err := os.Lstat(file)
				if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || !sameChangeTime(before, after) {
					t.Fatalf("marker mutated by read-only preflight: %v", err)
				}
			})
		}
	}
}

func TestVerifyNoPortableAuthorityDoesNotAcceptValidFreeze(t *testing.T) {
	library, project, storage := fixture(t)
	_, p := openFixture(t, library, project)
	put(t, filepath.Join(storage, "portable-runtime-freeze.json"), []byte(`{"version":1,"originProjectId":"`+fixtureID+`","snapshotGeneration":"synthetic-generation","nonReplayable":true,"requiresRuntimeFreezeGuard":true,"records":[],"avatarReferenceProjections":[]}`))
	assertAuthorityUnsupported(t, p.VerifyNoPortableAuthority(context.Background()))
}

// Context.Err supplies a deterministic step boundary, not a production hook.
// Mutations are synchronous on the test goroutine and use only synthetic files.
type authorityHookContext struct {
	context.Context
	hook func()
}

func (ctx authorityHookContext) Err() error {
	ctx.hook()
	return ctx.Context.Err()
}

func TestVerifyNoPortableAuthorityCancellationAndInvalidHandles(t *testing.T) {
	library, project, _ := fixture(t)
	l, p := openFixture(t, library, project)
	if err := p.VerifyNoPortableAuthority(nil); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("nil context: %v", err)
	}
	var absent *Project
	if err := absent.VerifyNoPortableAuthority(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil project: %v", err)
	}
	if err := (&Project{}).VerifyNoPortableAuthority(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("zero project: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.VerifyNoPortableAuthority(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled: %v", err)
	}
	for _, boundary := range []int{2, 5, 11, 12} {
		base, cancel := context.WithCancel(context.Background())
		calls := 0
		ctx := authorityHookContext{Context: base, hook: func() {
			calls++
			if calls == boundary {
				cancel()
			}
		}}
		err := p.VerifyNoPortableAuthority(ctx)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation boundary %d: %v", boundary, err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyNoPortableAuthority(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed project: %v", err)
	}
	p2, err := l.OpenProject(project)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p2.VerifyNoPortableAuthority(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed library: %v", err)
	}
}

func TestVerifyNoPortableAuthorityRevalidatesPinnedIdentity(t *testing.T) {
	for _, boundary := range []int{2, 11} {
		for _, action := range []string{"manifest bytes", "manifest inode", "manifest symlink", "manifest missing", "storage replaced", "project replaced", "library replaced", "migration pending"} {
			t.Run(action+"/"+string(rune('A'+boundary)), func(t *testing.T) {
				library, project, storage := fixture(t)
				_, p := openFixture(t, library, project)
				calls := 0
				ctx := authorityHookContext{Context: context.Background(), hook: func() {
					calls++
					if calls != boundary {
						return
					}
					switch action {
					case "manifest bytes":
						put(t, filepath.Join(storage, ManifestName), []byte(strings.Replace(fixtureManifest, fixtureID, "12345678-1234-4234-8234-aaaaaaaaaaaa", 1)))
					case "manifest inode":
						replacement := filepath.Join(storage, "synthetic-manifest-replacement")
						put(t, replacement, []byte(fixtureManifest))
						if err := os.Rename(replacement, filepath.Join(storage, ManifestName)); err != nil {
							t.Fatal(err)
						}
					case "manifest symlink":
						remove(t, filepath.Join(storage, ManifestName))
						link(t, "missing", filepath.Join(storage, ManifestName), true)
					case "manifest missing":
						remove(t, filepath.Join(storage, ManifestName))
					case "storage replaced":
						if err := os.Rename(storage, storage+"-old"); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(storage, 0700); err != nil {
							t.Fatal(err)
						}
					case "project replaced":
						if err := os.Rename(project, project+"-old"); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(project, 0700); err != nil {
							t.Fatal(err)
						}
					case "library replaced":
						moved := filepath.Join(t.TempDir(), "synthetic-moved-library")
						if err := os.Rename(library, moved); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(library, 0700); err != nil {
							t.Fatal(err)
						}
					case "migration pending":
						control := filepath.Join(project, ".ai-novel-migration")
						if err := os.Mkdir(control, 0700); err != nil {
							t.Fatal(err)
						}
						put(t, filepath.Join(control, "journal.json"), []byte("{"))
					}
				}}
				err := p.VerifyNoPortableAuthority(ctx)
				if err == nil {
					t.Fatal("stale or changed capability admitted")
				}
				if !strings.HasPrefix(err.Error(), "PROJECT_") || strings.Contains(err.Error(), library) {
					t.Fatalf("identity error must remain symbolic: %v", err)
				}
				var pathError *fs.PathError
				if errors.As(err, &pathError) {
					t.Fatal("identity error leaked host path")
				}
			})
		}
	}
}

func TestVerifyNoPortableAuthorityResamplesAllFixedNames(t *testing.T) {
	for _, marker := range []string{"runtime freeze", "transfer authority", "restore reservation", "restore journal"} {
		t.Run(marker, func(t *testing.T) {
			library, project, storage := fixture(t)
			_, p := openFixture(t, library, project)
			calls := 0
			ctx := authorityHookContext{Context: context.Background(), hook: func() {
				calls++
				// The first four-name pass has completed at boundary 7.
				if calls == 7 {
					put(t, authorityFixtureMarkers(library, project, storage)[marker], []byte("{"))
				}
			}}
			assertAuthorityUnsupported(t, p.VerifyNoPortableAuthority(ctx))
		})
	}
}

func TestPortableAuthorityMarkersAgainstOriginalPureFunctionOracle(t *testing.T) {
	path := os.Getenv("GO_AUTHORITY_CONTRACT")
	if path == "" {
		t.Skip("original-source marker oracle is generated by CI only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		FormatVersion int `json:"formatVersion"`
		StorageDirectory string `json:"storageDirectory"`
		RuntimeFreezeName string `json:"runtimeFreezeName"`
		TransferAuthorityName string `json:"transferAuthorityName"`
		TransferAuthorityPath string `json:"transferAuthorityPath"`
		ReservationName string `json:"reservationName"`
		JournalCases []struct {
			ProjectPath string `json:"projectPath"`
			JournalName string `json:"journalName"`
		} `json:"journalCases"`
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.FormatVersion != 1 || contract.StorageDirectory != StorageDirectory || contract.RuntimeFreezeName != portableRuntimeFreezeName || contract.TransferAuthorityName != portableTransferAuthorityName || contract.TransferAuthorityPath != StorageDirectory+"/"+portableTransferAuthorityName || contract.ReservationName != portableRestoreReservationName || len(contract.JournalCases) < 3 {
		t.Fatal("installed marker constants drifted from original TypeScript source")
	}
	for _, item := range contract.JournalCases {
		if !canonicalAbsolute(item.ProjectPath) || portableRestoreJournalName(item.ProjectPath) != item.JournalName {
			t.Fatal("target-specific restore journal differs from original pure function")
		}
	}
}
