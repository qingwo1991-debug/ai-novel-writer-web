//go:build linux

package projectfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func streamDestination(t *testing.T) (string, *os.Root) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return path, root
}

func assertNoStagedDatabase(t *testing.T, root *os.Root) {
	t.Helper()
	for _, suffix := range databaseSuffixes {
		if _, err := root.Lstat("snapshot.db" + suffix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("unexpected staged suffix %q: %v", suffix, err)
		}
	}
}

func TestStageDatabaseFullSetOwnedAndPrivate(t *testing.T) {
	library, path, storage := fixture(t)
	var total int64
	for _, suffix := range databaseSuffixes {
		data := []byte("synthetic streaming database" + suffix)
		put(t, filepath.Join(storage, DatabaseName+suffix), data)
		total += int64(len(data))
	}
	_, p := openFixture(t, library, path)
	destinationPath, destination := streamDestination(t)
	proof, err := p.StageDatabase(context.Background(), destination, total)
	if err != nil {
		t.Fatal(err)
	}
	if proof == nil || len(proof.entries) != len(databaseSuffixes) {
		t.Fatalf("missing proof set: %#v", proof)
	}
	for _, suffix := range databaseSuffixes {
		name := "snapshot.db" + suffix
		info, err := destination.Lstat(name)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !singleLink(info) {
			t.Fatalf("unsafe staged file %q: %v", name, info.Mode())
		}
		sourceInfo, err := os.Stat(filepath.Join(storage, DatabaseName+suffix))
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(info, sourceInfo) {
			t.Fatal("staged file aliases source")
		}
		source, err := os.ReadFile(filepath.Join(storage, DatabaseName+suffix))
		if err != nil {
			t.Fatal(err)
		}
		staged, err := os.ReadFile(filepath.Join(destinationPath, name))
		if err != nil || !bytes.Equal(source, staged) {
			t.Fatalf("suffix %q differs: %v", suffix, err)
		}
	}
	// Private SQLite admission may rewrite/remove staged sidecars. Verification
	// deliberately checks the original source, not the private copy.
	put(t, filepath.Join(destinationPath, "snapshot.db"), []byte("private verifier rewrite"))
	remove(t, filepath.Join(destinationPath, "snapshot.db-shm"))
	if err := proof.VerifyUnchanged(context.Background()); err != nil {
		t.Fatalf("unchanged source after private changes: %v", err)
	}
	if _, err := p.StageDatabase(context.Background(), destination, total); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("reused destination: %v", err)
	}
}

func TestStageDatabaseAbsentSidecarsAndUnboundedCallerBudget(t *testing.T) {
	library, path, _ := fixture(t)
	_, p := openFixture(t, library, path)
	_, destination := streamDestination(t)
	proof, err := p.StageDatabase(context.Background(), destination, int64(1<<63-1))
	if err != nil {
		t.Fatal(err)
	}
	if len(proof.entries) != 1 {
		t.Fatalf("absent source sidecars became proof members: %v", proof.entries)
	}
	for _, suffix := range databaseSuffixes[1:] {
		if _, err := destination.Lstat("snapshot.db" + suffix); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("absent sidecar %q materialized: %v", suffix, err)
		}
	}
	if err := proof.VerifyUnchanged(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func streamFileHash(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.CopyBuffer(hash, file, make([]byte, snapshotStreamBuffer)); err != nil {
		t.Fatal(err)
	}
	var digest [sha256.Size]byte
	hash.Sum(digest[:0])
	return digest
}

func TestStageDatabaseSparseLargerThanLegacyCeiling(t *testing.T) {
	// Synthetic sparse source: exercise >64 MiB without allocating its contents.
	// This test, like all Go compilation/tests, runs on GitHub only.
	library, path, storage := fixture(t)
	sourcePath := filepath.Join(storage, DatabaseName)
	file, err := os.OpenFile(sourcePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	const size int64 = 65 * 1024 * 1024
	if err := file.Truncate(size); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("synthetic end"), size-int64(len("synthetic end"))); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, p := openFixture(t, library, path)
	destinationPath, destination := streamDestination(t)
	proof, err := p.StageDatabase(context.Background(), destination, size)
	if err != nil {
		t.Fatalf("streaming >64 MiB with exact budget: %v", err)
	}
	info, err := destination.Stat("snapshot.db")
	if err != nil || info.Size() != size {
		t.Fatalf("large copy size: %v %v", info, err)
	}
	sourceHash := streamFileHash(t, sourcePath)
	if copyHash := streamFileHash(t, filepath.Join(destinationPath, "snapshot.db")); copyHash != sourceHash || proof.entries[""].hash != sourceHash {
		t.Fatal("large streamed copy/hash mismatch")
	}
	if err := proof.VerifyUnchanged(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStageDatabaseTotalBudgetAndInvalidArguments(t *testing.T) {
	library, path, storage := fixture(t)
	put(t, filepath.Join(storage, DatabaseName), []byte("12345"))
	put(t, filepath.Join(storage, DatabaseName+"-wal"), []byte("67890"))
	_, p := openFixture(t, library, path)
	for _, limit := range []int64{-1, 0, 4, 9} {
		t.Run(strconv.FormatInt(limit, 10), func(t *testing.T) {
			_, destination := streamDestination(t)
			if proof, err := p.StageDatabase(context.Background(), destination, limit); !errors.Is(err, ErrTooLarge) || proof != nil {
				t.Fatalf("budget %d: proof=%v err=%v", limit, proof, err)
			}
			assertNoStagedDatabase(t, destination)
		})
	}
	_, destination := streamDestination(t)
	if _, err := p.StageDatabase(context.Background(), destination, 10); err != nil {
		t.Fatalf("total exact budget: %v", err)
	}
	if _, err := p.StageDatabase(nil, destination, 10); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("nil context: %v", err)
	}
	if _, err := p.StageDatabase(context.Background(), nil, 10); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("nil destination: %v", err)
	}
}

func TestStageDatabaseRejectsEveryPreexistingDestinationSuffix(t *testing.T) {
	for _, suffix := range databaseSuffixes {
		for _, kind := range []string{"file", "directory", "symlink", "hardlink", "fifo"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				library, path, _ := fixture(t)
				_, p := openFixture(t, library, path)
				destinationPath, destination := streamDestination(t)
				name := "snapshot.db" + suffix
				full := filepath.Join(destinationPath, name)
				switch kind {
				case "file":
					put(t, full, []byte("preexisting"))
				case "directory":
					if err := os.Mkdir(full, 0700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					link(t, "missing", full, true)
				case "hardlink":
					put(t, filepath.Join(destinationPath, "other"), []byte("preexisting"))
					link(t, filepath.Join(destinationPath, "other"), full, false)
				case "fifo":
					if err := syscall.Mkfifo(full, 0600); err != nil {
						t.Fatal(err)
					}
				}
				before, err := destination.Lstat(name)
				if err != nil {
					t.Fatal(err)
				}
				if proof, err := p.StageDatabase(context.Background(), destination, 1024); !errors.Is(err, fs.ErrExist) || proof != nil {
					t.Fatalf("preexisting destination accepted: %v", err)
				}
				after, err := destination.Lstat(name)
				if err != nil || !sameVersion(before, after) {
					t.Fatalf("preexisting destination changed: %v", err)
				}
				for _, absent := range databaseSuffixes {
					if absent == suffix {
						continue
					}
					if _, err := destination.Lstat("snapshot.db" + absent); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("wrote another suffix before preflight: %q %v", absent, err)
					}
				}
			})
		}
	}
}

func TestStageDatabaseRejectsNonprivateAndSourceDestinations(t *testing.T) {
	library, path, storage := fixture(t)
	_, p := openFixture(t, library, path)
	destinationPath, destination := streamDestination(t)
	if err := os.Chmod(destinationPath, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.StageDatabase(context.Background(), destination, 1024); !errors.Is(err, ErrUnsafeDirectory) {
		t.Fatalf("nonprivate root: %v", err)
	}
	for _, name := range []string{library, path, storage} {
		root, err := os.OpenRoot(name)
		if err != nil {
			t.Fatal(err)
		}
		_, stageErr := p.StageDatabase(context.Background(), root, 1024)
		root.Close()
		if !errors.Is(stageErr, ErrUnsafeDirectory) {
			t.Fatalf("source destination %q: %v", name, stageErr)
		}
	}
	assertNoStagedDatabase(t, destination)
}

// A synchronous context hook makes mutation/cancellation points deterministic,
// without production hooks, timing assumptions, goroutines or sleeps.
type streamHookContext struct {
	context.Context
	hook func()
}

func (ctx streamHookContext) Err() error {
	ctx.hook()
	return ctx.Context.Err()
}

func TestStageDatabaseCancellationAndRollback(t *testing.T) {
	library, path, storage := fixture(t)
	put(t, filepath.Join(storage, DatabaseName), bytes.Repeat([]byte("s"), snapshotStreamBuffer*3))
	_, p := openFixture(t, library, path)
	for _, midCopy := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "during-copy"}[midCopy], func(t *testing.T) {
			destinationPath, destination := streamDestination(t)
			put(t, filepath.Join(destinationPath, "unrelated"), []byte("preserve"))
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ctx context.Context = base
			if !midCopy {
				cancel()
			} else {
				ctx = streamHookContext{Context: base, hook: func() {
					if info, err := destination.Stat("snapshot.db"); err == nil && info.Size() >= snapshotStreamBuffer {
						cancel()
					}
				}}
			}
			if proof, err := p.StageDatabase(ctx, destination, int64(snapshotStreamBuffer*3)); !errors.Is(err, context.Canceled) || proof != nil {
				t.Fatalf("cancel: proof=%v err=%v", proof, err)
			}
			assertNoStagedDatabase(t, destination)
			data, err := os.ReadFile(filepath.Join(destinationPath, "unrelated"))
			if err != nil || string(data) != "preserve" {
				t.Fatalf("rollback touched unrelated file: %v", err)
			}
		})
	}
}

func TestStageDatabaseDetectsMutationDuringCopy(t *testing.T) {
	for _, action := range []string{"content", "sidecar-added", "same-bytes-new-inode", "manifest-stat"} {
		t.Run(action, func(t *testing.T) {
			library, path, storage := fixture(t)
			data := bytes.Repeat([]byte("s"), snapshotStreamBuffer*3)
			put(t, filepath.Join(storage, DatabaseName), data)
			_, p := openFixture(t, library, path)
			_, destination := streamDestination(t)
			mutated := false
			ctx := streamHookContext{Context: context.Background(), hook: func() {
				info, err := destination.Stat("snapshot.db")
				if mutated || err != nil || info.Size() < snapshotStreamBuffer {
					return
				}
				mutated = true
				switch action {
				case "content":
					put(t, filepath.Join(storage, DatabaseName), bytes.Repeat([]byte("x"), len(data)))
				case "sidecar-added":
					put(t, filepath.Join(storage, DatabaseName+"-wal"), []byte("new sidecar"))
				case "same-bytes-new-inode":
					put(t, filepath.Join(storage, "replacement"), data)
					if err := os.Rename(filepath.Join(storage, "replacement"), filepath.Join(storage, DatabaseName)); err != nil {
						t.Fatal(err)
					}
				case "manifest-stat":
					if err := os.Chmod(filepath.Join(storage, ManifestName), 0400); err != nil {
						t.Fatal(err)
					}
				}
			}}
			if proof, err := p.StageDatabase(ctx, destination, int64(len(data)+1024)); err == nil || proof != nil {
				t.Fatalf("mutation accepted: proof=%v err=%v", proof, err)
			}
			if !mutated {
				t.Fatal("copy mutation hook never fired")
			}
			assertNoStagedDatabase(t, destination)
		})
	}
}

func TestSnapshotProofDetectsSourceChanges(t *testing.T) {
	for _, action := range []string{"content", "database-replaced", "wal-replaced", "sidecar-added", "sidecar-removed", "manifest-content", "manifest-replaced", "manifest-mode", "storage-replaced", "project-replaced", "library-replaced", "migration-added", "hardlink", "symlink"} {
		t.Run(action, func(t *testing.T) {
			library, path, storage := fixture(t)
			put(t, filepath.Join(storage, DatabaseName+"-wal"), []byte("synthetic wal"))
			_, p := openFixture(t, library, path)
			_, destination := streamDestination(t)
			proof, err := p.StageDatabase(context.Background(), destination, 1024)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "content":
				put(t, filepath.Join(storage, DatabaseName), []byte("changed synthetic database bytes"))
			case "database-replaced", "wal-replaced", "manifest-replaced":
				name := DatabaseName
				if action == "wal-replaced" {
					name += "-wal"
				} else if action == "manifest-replaced" {
					name = ManifestName
				}
				data, err := os.ReadFile(filepath.Join(storage, name))
				if err != nil {
					t.Fatal(err)
				}
				put(t, filepath.Join(storage, "replacement"), data)
				if err := os.Rename(filepath.Join(storage, "replacement"), filepath.Join(storage, name)); err != nil {
					t.Fatal(err)
				}
			case "sidecar-added":
				put(t, filepath.Join(storage, DatabaseName+"-journal"), []byte("new synthetic journal"))
			case "sidecar-removed":
				remove(t, filepath.Join(storage, DatabaseName+"-wal"))
			case "manifest-content":
				put(t, filepath.Join(storage, ManifestName), []byte(strings.Replace(fixtureManifest, fixtureID, "12345678-1234-4234-8234-aaaaaaaaaaaa", 1)))
			case "manifest-mode":
				if err := os.Chmod(filepath.Join(storage, ManifestName), 0400); err != nil {
					t.Fatal(err)
				}
			case "storage-replaced", "project-replaced", "library-replaced":
				name := storage
				if action == "project-replaced" {
					name = path
				} else if action == "library-replaced" {
					name = library
				}
				// Move into a separate temp root, retaining original inode until cleanup.
				if err := os.Rename(name, filepath.Join(t.TempDir(), "moved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(name, 0700); err != nil {
					t.Fatal(err)
				}
			case "migration-added":
				put(t, filepath.Join(path, ".vela"), []byte("legacy marker"))
			case "hardlink":
				link(t, filepath.Join(storage, DatabaseName), filepath.Join(storage, "new-link"), false)
			case "symlink":
				remove(t, filepath.Join(storage, DatabaseName+"-wal"))
				link(t, DatabaseName, filepath.Join(storage, DatabaseName+"-wal"), true)
			}
			if err := proof.VerifyUnchanged(context.Background()); err == nil {
				t.Fatal("changed source accepted")
			}
		})
	}
}

func TestStageDatabaseRejectsUnsafeSourceSet(t *testing.T) {
	for _, suffix := range databaseSuffixes {
		for _, kind := range []string{"symlink", "hardlink", "fifo", "directory"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				library, path, storage := fixture(t)
				_, p := openFixture(t, library, path)
				name := filepath.Join(storage, DatabaseName+suffix)
				if suffix == "" {
					remove(t, name)
				}
				switch kind {
				case "symlink":
					link(t, "missing", name, true)
				case "hardlink":
					other := filepath.Join(storage, "linked-source")
					put(t, other, []byte("synthetic hardlink"))
					link(t, other, name, false)
				case "fifo":
					if err := syscall.Mkfifo(name, 0600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(name, 0700); err != nil {
						t.Fatal(err)
					}
				}
				_, destination := streamDestination(t)
				if proof, err := p.StageDatabase(context.Background(), destination, 1024); err == nil || proof != nil {
					t.Fatalf("unsafe source accepted: %v", err)
				}
				assertNoStagedDatabase(t, destination)
			})
		}
	}
}

func TestStageDatabaseRollbackPreservesForeignDestinationFiles(t *testing.T) {
	for _, replaceOwned := range []bool{false, true} {
		t.Run(map[bool]string{false: "late-sidecar", true: "replaced-owned-inode"}[replaceOwned], func(t *testing.T) {
			library, path, storage := fixture(t)
			put(t, filepath.Join(storage, DatabaseName), bytes.Repeat([]byte("s"), snapshotStreamBuffer*3))
			put(t, filepath.Join(storage, DatabaseName+"-wal"), []byte("synthetic wal"))
			_, p := openFixture(t, library, path)
			destinationPath, destination := streamDestination(t)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			changed := false
			foreignName := "snapshot.db-wal"
			if replaceOwned {
				foreignName = "snapshot.db"
			}
			ctx := streamHookContext{Context: base, hook: func() {
				info, err := destination.Stat("snapshot.db")
				if changed || err != nil || info.Size() < snapshotStreamBuffer {
					return
				}
				changed = true
				if replaceOwned {
					// Keep the old inode alive via the open write descriptor so
					// the replacement cannot accidentally reuse its identity.
					remove(t, filepath.Join(destinationPath, foreignName))
					cancel()
				}
				put(t, filepath.Join(destinationPath, foreignName), []byte("foreign destination bytes"))
			}}
			proof, err := p.StageDatabase(ctx, destination, int64(snapshotStreamBuffer*3+1024))
			if err == nil || proof != nil || !changed {
				t.Fatalf("destination mutation did not fail: proof=%v err=%v changed=%v", proof, err, changed)
			}
			if replaceOwned && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation error: %v", err)
			}
			if !replaceOwned && !errors.Is(err, fs.ErrExist) {
				t.Fatalf("lost exclusive-creation error: %v", err)
			}
			foreign, err := os.ReadFile(filepath.Join(destinationPath, foreignName))
			if err != nil || string(foreign) != "foreign destination bytes" {
				t.Fatalf("rollback removed or overwrote foreign file: %v", err)
			}
			for _, suffix := range databaseSuffixes {
				if "snapshot.db"+suffix == foreignName {
					continue
				}
				if _, err := destination.Lstat("snapshot.db" + suffix); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("owned suffix left after failure %q: %v", suffix, err)
				}
			}
		})
	}
}

func TestSnapshotProofCancellationAndBorrowedLifetime(t *testing.T) {
	library, path, _ := fixture(t)
	l, p := openFixture(t, library, path)
	_, destination := streamDestination(t)
	proof, err := p.StageDatabase(context.Background(), destination, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := proof.VerifyUnchanged(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled proof: %v", err)
	}
	if err := proof.VerifyUnchanged(nil); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("nil context: %v", err)
	}
	var nilProof *SnapshotProof
	for _, invalid := range []*SnapshotProof{nilProof, &SnapshotProof{}} {
		if err := invalid.VerifyUnchanged(context.Background()); !errors.Is(err, ErrClosed) {
			t.Fatalf("invalid proof: %v", err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := proof.VerifyUnchanged(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("proof after project close: %v", err)
	}
	if _, err := p.StageDatabase(context.Background(), destination, 1024); !errors.Is(err, ErrClosed) {
		t.Fatalf("stage after project close: %v", err)
	}
	p2, err := l.OpenProject(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	_, other := streamDestination(t)
	proof2, err := p2.StageDatabase(context.Background(), other, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := proof2.VerifyUnchanged(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("proof after library close: %v", err)
	}
}
