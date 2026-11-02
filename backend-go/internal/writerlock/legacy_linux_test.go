//go:build linux

package writerlock

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// All fixtures are synthetic private temporary directories. No author data,
// production paths, shell-generated lock names, or external services are used.
func legacyFixture(t *testing.T, root string, mode os.FileMode, data []byte) string {
	t.Helper()
	path := filepath.Join(root, legacyLockName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1234567890, 123456789)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return path
}

func legacyStat(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func legacyUnchanged(t *testing.T, path string, before fs.FileInfo) {
	t.Helper()
	after := legacyStat(t, path)
	b := before.Sys().(*syscall.Stat_t)
	a := after.Sys().(*syscall.Stat_t)
	if !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() || b.Uid != a.Uid || b.Gid != a.Gid || b.Nlink != a.Nlink || b.Atim != a.Atim || b.Ctim != a.Ctim || b.Mtim != a.Mtim {
		t.Fatal("legacy entry inode, mode, ownership, links, size or timestamps changed")
	}
}

func TestLegacyAllowedModesPreserveEntry(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			root := privateDirectory(t)
			path := legacyFixture(t, root, mode, nil)
			before := legacyStat(t, path)
			for i := 0; i < 3; i++ {
				lock, err := AcquireLegacy(root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { lock.Close() })
				if err := lock.Verify(); err != nil {
					t.Fatalf("verify legacy mode %o: %v", mode, err)
				}
				other, err := AcquireLegacy(root)
				if other != nil {
					other.Close()
				}
				if other != nil || !errors.Is(err, ErrLocked) {
					t.Fatalf("existing legacy entry must be busy: %v", err)
				}
				legacyUnchanged(t, path, before)
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
				if err := lock.Verify(); !errors.Is(err, ErrUnsafe) {
					t.Fatalf("closed verify: %v", err)
				}
				legacyUnchanged(t, path, before)
			}
			data, err := os.ReadFile(path)
			if err != nil || len(data) != 0 {
				t.Fatalf("empty legacy bytes changed: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != legacyLockName {
				t.Fatalf("unexpected lock namespace mutation: %v", err)
			}
		})
	}
}

func TestLegacyCreate0600AndIndependentNamespace(t *testing.T) {
	root := privateDirectory(t)
	old, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { old.Close() })
	oldPath := filepath.Join(root, lockName)
	before := legacyStat(t, oldPath)
	legacy, err := AcquireLegacy(root)
	if err != nil {
		t.Fatalf("unrelated .novel-writer.lock must not pretend legacy coordination: %v", err)
	}
	t.Cleanup(func() { legacy.Close() })
	if info := legacyStat(t, filepath.Join(root, legacyLockName)); !safeLock(info) {
		t.Fatal("new legacy lock is not empty single-link euid-owned exact0600")
	}
	if err := legacy.Verify(); err != nil {
		t.Fatal(err)
	}
	if err := old.Verify(); err != nil {
		t.Fatal(err)
	}
	legacyUnchanged(t, oldPath, before)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err = AcquireLegacy(root)
	if err != nil {
		t.Fatalf("old lock must not prevent legacy reacquire: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	legacyUnchanged(t, oldPath, before)
}

func TestLegacyRejectUnsafeEntriesWithoutMutation(t *testing.T) {
	for _, kind := range []string{"0640", "0666", "0400", "0700", "setuid", "setgid", "sticky", "symlink", "dangling-symlink", "hardlink", "nonempty", "fifo", "directory", "foreign-owner"} {
		t.Run(kind, func(t *testing.T) {
			root := privateDirectory(t)
			path := filepath.Join(root, legacyLockName)
			target := filepath.Join(root, "target")
			var err error
			var data []byte
			switch kind {
			case "symlink", "dangling-symlink", "hardlink":
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if kind == "hardlink" {
					err = os.Link(target, path)
				} else if kind == "symlink" {
					err = os.Symlink("target", path)
				} else {
					err = os.Symlink("missing", path)
				}
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			case "directory":
				err = os.Mkdir(path, 0o700)
			default:
				mode := os.FileMode(0o600)
				switch kind {
				case "0640":
					mode = 0o640
				case "0666":
					mode = 0o666
				case "0400":
					mode = 0o400
				case "0700":
					mode = 0o700
				case "setuid":
					mode |= os.ModeSetuid
				case "setgid":
					mode |= os.ModeSetgid
				case "sticky":
					mode |= os.ModeSticky
				case "nonempty":
					data = []byte("foreign legacy content must survive")
				case "foreign-owner":
					if os.Geteuid() != 0 {
						t.Skip("foreign-owner fixture requires root; deliberately skipped on non-root CI")
					}
				}
				legacyFixture(t, root, mode, data)
				if kind == "foreign-owner" {
					err = os.Chown(path, 65534, -1)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := legacyStat(t, path)
			var targetBefore fs.FileInfo
			if kind == "symlink" || kind == "hardlink" {
				targetBefore = legacyStat(t, target)
			}
			lock, err := AcquireLegacy(root)
			if lock != nil {
				lock.Close()
			}
			if lock != nil || !errors.Is(err, ErrUnsafe) {
				t.Fatalf("accepted unsafe legacy %s: %v", kind, err)
			}
			legacyUnchanged(t, path, before)
			if targetBefore != nil {
				legacyUnchanged(t, target, targetBefore)
			}
			if kind == "nonempty" {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("foreign legacy bytes changed: %v", err)
				}
			}
			if kind == "symlink" || kind == "dangling-symlink" {
				got, err := os.Readlink(path)
				want := "target"
				if kind == "dangling-symlink" {
					want = "missing"
				}
				if err != nil || got != want {
					t.Fatalf("legacy symlink changed: %v", err)
				}
			}
		})
	}
}

func TestLegacyVerifyDetectsEntryChanges(t *testing.T) {
	for _, kind := range []string{"rename-replacement", "unlink-replacement", "unlink", "symlink", "hardlink", "permissions", "allowed-mode-change", "nonempty", "mtime", "ctime-only"} {
		t.Run(kind, func(t *testing.T) {
			root := privateDirectory(t)
			path := legacyFixture(t, root, 0o600, nil)
			lock, err := AcquireLegacy(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { lock.Close() })
			if err := lock.Verify(); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "rename-replacement", "symlink":
				err = os.Rename(path, path + ".old")
				if err == nil {
					if kind == "symlink" {
						err = os.Symlink(legacyLockName + ".old", path)
					} else {
						err = os.WriteFile(path, nil, 0o600)
					}
				}
			case "unlink-replacement", "unlink":
				err = os.Remove(path)
				if err == nil && kind == "unlink-replacement" {
					err = os.WriteFile(path, nil, 0o600)
				}
			case "hardlink":
				err = os.Link(path, path + ".other")
			case "permissions":
				err = os.Chmod(path, 0o640)
			case "allowed-mode-change":
				err = os.Chmod(path, 0o644)
			case "nonempty":
				err = os.WriteFile(path, []byte("changed"), 0o600)
			case "mtime":
				stamp := time.Unix(1500000000, 0)
				err = os.Chtimes(path, stamp, stamp)
			case "ctime-only":
				// Keep inode, mode, size and mtime unchanged but alter ctime.
				before := legacyStat(t, path)
				stamp := before.Sys().(*syscall.Stat_t)
				// Ensure timestamp granularity does not hide the ctime change.
				time.Sleep(10 * time.Millisecond)
				err = os.Chtimes(path, time.Unix(int64(stamp.Atim.Sec), int64(stamp.Atim.Nsec)), before.ModTime())
				if err == nil && legacyStat(t, path).Sys().(*syscall.Stat_t).Ctim == stamp.Ctim {
					t.Fatal("fixture filesystem did not expose changed ctime")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := lock.Verify(); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("Verify accepted %s: %v", kind, err)
			}
			if kind != "unlink" {
				beforeClose := legacyStat(t, path)
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
				legacyUnchanged(t, path, beforeClose)
				if kind == "nonempty" {
					got, err := os.ReadFile(path)
					if err != nil || string(got) != "changed" {
						t.Fatalf("Verify/Close modified replacement bytes: %v", err)
					}
				}
			}
		})
	}
}

func TestLegacyVerifyUsesItsOwnNameAndPolicy(t *testing.T) {
	root := privateDirectory(t)
	legacyFixture(t, root, 0o644, nil)
	legacy, err := AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { legacy.Close() })
	old, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { old.Close() })
	if err := os.Remove(filepath.Join(root, lockName)); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Verify(); err != nil {
		t.Fatalf("legacy Verify inspected unrelated old name or exact0600 policy: %v", err)
	}
	if err := old.Verify(); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("old Verify did not inspect its own unlinked entry: %v", err)
	}
}

func TestLegacyVerifyRejectsUnsafeParentPermissions(t *testing.T) {
	for _, component := range []string{"root", "ancestor"} {
		t.Run(component, func(t *testing.T) {
			ancestor := privateDirectory(t)
			root := filepath.Join(ancestor, "root")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			lock, err := AcquireLegacy(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { lock.Close() })
			changed := root
			if component == "ancestor" {
				changed = ancestor
			}
			if err := os.Chmod(changed, 0o777); err != nil {
				t.Fatal(err)
			}
			if err := lock.Verify(); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("Verify accepted writable %s: %v", component, err)
			}
		})
	}
}

func TestLegacyVerifyDetectsEveryPinnedParentChange(t *testing.T) {
	for _, component := range []string{"root", "parent", "ancestor"} {
		t.Run(component, func(t *testing.T) {
			base := privateDirectory(t)
			ancestor := filepath.Join(base, "ancestor")
			parent := filepath.Join(ancestor, "parent")
			root := filepath.Join(parent, "root")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			lock, err := AcquireLegacy(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { lock.Close() })
			original := root
			if component == "parent" {
				original = parent
			} else if component == "ancestor" {
				original = ancestor
			}
			if err := os.Rename(original, original + ".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := lock.Verify(); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("Verify accepted renamed %s: %v", component, err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(root, legacyLockName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Verify/Close created replacement lock: %v", err)
			}
		})
	}
}

func TestLegacyCloseConcurrentWithVerify(t *testing.T) {
	root := privateDirectory(t)
	lock, err := AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	path := filepath.Join(root, legacyLockName)
	before := legacyStat(t, path)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := lock.Verify(); err != nil && !errors.Is(err, ErrUnsafe) {
				t.Errorf("concurrent Verify: %v", err)
			}
			if err := lock.Close(); err != nil {
				t.Errorf("concurrent Close: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, closed := range []*Lock{nil, new(Lock), lock} {
		if err := closed.Verify(); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("nil/zero/closed Verify: %v", err)
		}
		if err := closed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	legacyUnchanged(t, path, before)
}

func TestLegacyConcurrentAcquireHasOneWinner(t *testing.T) {
	root := privateDirectory(t)
	const count = 32
	type result struct {
		lock *Lock
		err  error
	}
	results := make(chan result, count)
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		go func() {
			<-start
			lock, err := AcquireLegacy(root)
			results <- result{lock, err}
		}()
	}
	close(start)
	winners := 0
	for i := 0; i < count; i++ {
		result := <-results
		if result.err == nil && result.lock != nil {
			winners++
			t.Cleanup(func() { result.lock.Close() })
			if err := result.lock.Verify(); err != nil {
				t.Error(err)
			}
		} else if result.lock != nil || !errors.Is(result.err, ErrLocked) {
			if result.lock != nil {
				result.lock.Close()
			}
			t.Errorf("legacy contender: %v", result.err)
		}
	}
	if winners != 1 {
		t.Fatalf("legacy acquisition winners: %d", winners)
	}
}

// The util-linux flock executable opens the lock path itself. Its command is
// this test binary, with a stdin/stdout pipe protocol, never an interpolated shell.
func TestLegacyFlockHolderHelper(t *testing.T) {
	if os.Getenv("ANW_LEGACY_FLOCK_HOLDER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "held")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func requireUtilLinuxFlock(t *testing.T) {
	t.Helper()
	output, err := exec.Command("flock", "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "util-linux") {
		t.Fatalf("these interoperability tests require util-linux flock: %v; %s", err, output)
	}
}

func startLegacyFlock(t *testing.T, path string) *lockChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45 * time.Second)
	t.Cleanup(cancel)
	child := &lockChild{cmd: exec.CommandContext(ctx, "flock", "-n", path, executable, "-test.run=^TestLegacyFlockHolderHelper$")}
	child.cmd.Env = append(os.Environ(), "ANW_LEGACY_FLOCK_HOLDER=1")
	child.cmd.Stderr = &child.stderr
	child.input, err = child.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.cmd.StdoutPipe()
	if err != nil {
		child.input.Close()
		t.Fatal(err)
	}
	child.output = bufio.NewReader(output)
	if err := child.cmd.Start(); err != nil {
		child.input.Close()
		output.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.input.Close()
		if child.cmd.ProcessState == nil {
			child.cmd.Process.Kill()
			child.cmd.Wait()
		}
	})
	if line := child.line(t); line != "held" {
		t.Fatalf("util-linux holder readiness: %q", line)
	}
	return child
}

func legacyFlockContender(t *testing.T, path string, busy bool) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20 * time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "flock", "-n", path, executable, "-test.run=^TestLegacyFlockHolderHelper$")
	cmd.Env = append(os.Environ(), "ANW_LEGACY_FLOCK_HOLDER=1")
	cmd.Stdin = strings.NewReader("release\n")
	output, err := cmd.CombinedOutput()
	if busy {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(output) != 0 {
			t.Fatalf("util-linux acquired Go-held legacy flock: %v; %s", err, output)
		}
	} else if err != nil || string(output) != "held\n" {
		t.Fatalf("util-linux could not acquire released legacy flock: %v; %s", err, output)
	}
}

func TestLegacyUtilLinuxFlockBothDirections(t *testing.T) {
	requireUtilLinuxFlock(t)
	for _, mode := range []os.FileMode{0o600, 0o644} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			root := privateDirectory(t)
			path := legacyFixture(t, root, mode, nil)
			before := legacyStat(t, path)
			child := startLegacyFlock(t, path)
			lock, err := AcquireLegacy(root)
			if lock != nil {
				lock.Close()
			}
			if lock != nil || !errors.Is(err, ErrLocked) {
				t.Fatalf("Go acquired util-linux-held legacy flock: %v", err)
			}
			legacyUnchanged(t, path, before)
			child.signal(t)
			child.wait(t)
			lock, err = AcquireLegacy(root)
			if err != nil {
				t.Fatalf("Go could not acquire after util-linux release: %v", err)
			}
			t.Cleanup(func() { lock.Close() })
			if err := lock.Verify(); err != nil {
				t.Fatal(err)
			}
			legacyFlockContender(t, path, true)
			if err := lock.Verify(); err != nil {
				t.Fatalf("busy flock changed held entry: %v", err)
			}
			legacyUnchanged(t, path, before)
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			legacyFlockContender(t, path, false)
			legacyUnchanged(t, path, before)
			data, err := os.ReadFile(path)
			if err != nil || len(data) != 0 {
				t.Fatalf("interop changed original bytes: %v", err)
			}
		})
	}
}

func TestLegacyUtilLinuxCreatesEntryThenGoReuses(t *testing.T) {
	requireUtilLinuxFlock(t)
	root := privateDirectory(t)
	path := filepath.Join(root, legacyLockName)
	child := startLegacyFlock(t, path)
	before := legacyStat(t, path)
	if mode := before.Mode().Perm(); mode != 0o600 && mode != 0o644 {
		t.Fatalf("CI umask produced unsupported shell mode %o; expected0600/0644", mode)
	}
	if lock, err := AcquireLegacy(root); lock != nil || !errors.Is(err, ErrLocked) {
		if lock != nil {
			lock.Close()
		}
		t.Fatalf("Go ignored shell-created held entry: %v", err)
	}
	child.signal(t)
	child.wait(t)
	lock, err := AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Verify(); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	legacyUnchanged(t, path, before)
}
