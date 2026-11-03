//go:build linux

package writerlock

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func privateDirectory(t *testing.T) string {
	t.Helper()
	// Test runners may configure a symlinked TMPDIR. Admit only its physical
	// canonical path, just as production administrator configuration must.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRetainPreventsAccidentalWriterLockRelease(t *testing.T) {
	root := privateDirectory(t)
	held, err := AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	// Tests alone can clear the poison so the runner does not leak a lock.
	t.Cleanup(func() {
		held.mu.Lock()
		held.retained = false
		held.mu.Unlock()
		retainedLocks.Delete(held)
		_ = held.Close()
	})
	if err := held.Retain(); err != nil {
		t.Fatal(err)
	}
	if _, anchored := retainedLocks.Load(held); !anchored {
		t.Fatal("retained lock lost its process-level reference")
	}
	if err := held.Close(); !errors.Is(err, ErrRetained) {
		t.Fatalf("retained lock was released: %v", err)
	}
	if next, err := AcquireLegacy(root); next != nil || !errors.Is(err, ErrLocked) {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("another writer acquired retained lock: %v", err)
	}
	if err := held.VerifyLegacy(root); err != nil {
		t.Fatalf("retained lock lost its physical anchor: %v", err)
	}
}

func TestAcquireReleasePreservesInode(t *testing.T) {
	root := privateDirectory(t)
	path := filepath.Join(root, lockName)
	first, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	before, err := os.Lstat(path)
	if err != nil || !safeLock(before) {
		t.Fatalf("expected empty single-link owned 0600 lock: %v", err)
	}
	if lock, err := Acquire(root); lock != nil || !errors.Is(err, ErrLocked) {
		if lock != nil {
			lock.Close()
		}
		t.Fatalf("same-process second acquisition must be busy: %v", err)
	}
	// Concurrent closes must not double-close a recycled file descriptor.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := first.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 3; i++ {
		next, err := Acquire(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := next.Close(); err != nil {
			t.Fatal(err)
		}
		after, err := os.Lstat(path)
		if err != nil || !sameLock(before, after) {
			t.Fatalf("release/reacquire modified persistent lock: %v", err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != lockName {
		t.Fatalf("unexpected lock namespace mutation: %v", err)
	}
	var nilLock *Lock
	if err := nilLock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := new(Lock).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSameProcessHasOneWinner(t *testing.T) {
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
			lock, err := Acquire(root)
			results <- result{lock, err}
		}()
	}
	close(start)
	var winners []*Lock
	for i := 0; i < count; i++ {
		result := <-results
		if result.err == nil {
			if result.lock == nil {
				t.Error("successful acquisition returned nil")
				continue
			}
			winners = append(winners, result.lock)
			t.Cleanup(func() { result.lock.Close() })
		} else if !errors.Is(result.err, ErrLocked) {
			t.Errorf("contender must return busy, not %v", result.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("expected exactly one same-process winner, got %d", len(winners))
	}
	if err := winners[0].Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Acquire(root)
	if err != nil {
		t.Fatalf("reacquire after concurrent contenders: %v", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRejectUnsafeLockEntriesWithoutMutation(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling-symlink", "hardlink", "directory", "fifo", "nonempty", "readable", "writable", "readonly", "executable", "setuid", "setgid", "sticky", "foreign-owner"} {
		t.Run(kind, func(t *testing.T) {
			root := privateDirectory(t)
			path := filepath.Join(root, lockName)
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
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			default:
				mode := os.FileMode(0o600)
				switch kind {
				case "nonempty":
					data = []byte("unrecognized lock fixture")
				case "readable":
					mode = 0o644
				case "writable":
					mode = 0o622
				case "readonly":
					mode = 0o400
				case "executable":
					mode = 0o700
				case "setuid":
					mode |= os.ModeSetuid
				case "setgid":
					mode |= os.ModeSetgid
				case "sticky":
					mode |= os.ModeSticky
				case "foreign-owner":
					if os.Geteuid() != 0 {
						t.Skip("changing ownership requires root")
					}
				}
				if err = os.WriteFile(path, data, 0o600); err == nil {
					err = os.Chmod(path, mode)
				}
				if err == nil && kind == "foreign-owner" {
					err = os.Chown(path, 65534, -1)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := Acquire(root)
			if lock != nil {
				lock.Close()
			}
			if lock != nil || !errors.Is(err, ErrUnsafe) {
				t.Fatalf("accepted unsafe %s: %v", kind, err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() {
				t.Fatalf("unsafe lock was replaced or repaired: %v", err)
			}
			b := before.Sys().(*syscall.Stat_t)
			a := after.Sys().(*syscall.Stat_t)
			if b.Uid != a.Uid || b.Gid != a.Gid || b.Nlink != a.Nlink || b.Ctim != a.Ctim || b.Mtim != a.Mtim {
				t.Fatal("unsafe lock metadata changed")
			}
			if kind == "nonempty" {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("unknown lock content changed: %v", err)
				}
			}
			if kind == "symlink" || kind == "dangling-symlink" {
				got, err := os.Readlink(path)
				if err != nil || (got != "target" && got != "missing") {
					t.Fatalf("symlink changed: %v", err)
				}
			}
		})
	}
}

func TestRejectUnsafeDirectoryAndPaths(t *testing.T) {
	base := privateDirectory(t)
	physical := filepath.Join(base, "physical")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink("physical", alias); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o702, 0o720, 0o777} {
		root := filepath.Join(base, fmt.Sprintf("writable-%o", mode))
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(root, "child")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{root, child} {
			if lock, err := Acquire(path); lock != nil || !errors.Is(err, ErrUnsafe) {
				if lock != nil {
					lock.Close()
				}
				t.Fatalf("accepted writable directory chain: %v", err)
			}
		}
	}
	regular := filepath.Join(base, "regular")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{"", "/", "relative", base + "/", base + "//physical", base + "/./physical", base + "/../physical", base + "/bad\\name", base + "/bad\x00name", filepath.Join(base, "missing"), filepath.Join(base, "missing", "child"), alias, filepath.Join(alias, "child"), regular}
	for _, path := range paths {
		lock, err := Acquire(path)
		if lock != nil {
			lock.Close()
		}
		if lock != nil || !errors.Is(err, ErrUnsafe) {
			t.Fatalf("accepted unsafe directory input: %v", err)
		}
		if strings.Contains(err.Error(), base) || strings.Contains(err.Error(), "bad") {
			t.Fatal("error exposed path input")
		}
	}
	if _, err := os.Lstat(filepath.Join(base, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Acquire created missing directory: %v", err)
	}
	entries, err := os.ReadDir(physical)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink directory was followed: %v", err)
	}
}

func TestOwned0755DirectoryAccepted(t *testing.T) {
	root := privateDirectory(t)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidPreexistingLockReused(t *testing.T) {
	root := privateDirectory(t)
	path := filepath.Join(root, lockName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !sameLock(before, after) {
		t.Fatalf("valid existing lock was modified: %v", err)
	}
}

func TestPinnedDirectorySwapsFailClosed(t *testing.T) {
	for _, component := range []string{"root", "ancestor", "symlink"} {
		t.Run(component, func(t *testing.T) {
			base := privateDirectory(t)
			parent := filepath.Join(base, "parent")
			root := filepath.Join(parent, "root")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			anchor, err := openDirectory(root)
			if err != nil {
				t.Fatal(err)
			}
			defer anchor.close()
			original := root
			if component == "ancestor" {
				original = parent
			}
			moved := original + "-moved"
			if err := os.Rename(original, moved); err != nil {
				t.Fatal(err)
			}
			if component == "symlink" {
				err = os.Symlink(filepath.Base(moved), original)
			} else {
				err = os.MkdirAll(root, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if anchor.valid() {
				t.Fatal("detached physical anchor accepted")
			}
			if lock, err := acquirePinned(anchor); lock != nil || !errors.Is(err, ErrUnsafe) {
				if lock != nil {
					lock.Close()
				}
				t.Fatalf("directory replacement must fail closed: %v", err)
			}
			for _, path := range []string{filepath.Join(root, lockName), filepath.Join(moved, lockName), filepath.Join(moved, "root", lockName)} {
				if _, err := os.Lstat(path); err == nil {
					t.Fatal("failed anchored acquisition created a lock")
				} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestLockEntrySwapsFailValidation(t *testing.T) {
	for _, kind := range []string{"replacement", "symlink", "hardlink", "permissions", "size"} {
		t.Run(kind, func(t *testing.T) {
			root := privateDirectory(t)
			anchor, err := openDirectory(root)
			if err != nil {
				t.Fatal(err)
			}
			defer anchor.close()
			file, identity, err := openLock(anchor)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			path := filepath.Join(root, lockName)
			switch kind {
			case "replacement", "symlink":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if kind == "replacement" {
					err = os.WriteFile(path, nil, 0o600)
				} else {
					err = os.Symlink(lockName+".old", path)
				}
			case "hardlink":
				err = os.Link(path, path+".other")
			case "permissions":
				err = os.Chmod(path, 0o644)
			case "size":
				_, err = file.Write([]byte("unknown"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if sameEntry(anchor, file, identity) {
				t.Fatal("changed lock inode/metadata accepted")
			}
		})
	}
}

func TestCloseNeverUnlinksReplacement(t *testing.T) {
	root := privateDirectory(t)
	lock, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	path := filepath.Join(root, lockName)
	moved := path + ".old"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	data := []byte("administrator replacement must remain unchanged")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("Close changed replacement: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Close changed replacement content: %v", err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("Close removed held inode: %v", err)
	}
}

// The helper is the Go test executable itself, not a shell, flock utility, or
// separately built binary. Its lock remains held until its stdin is released.
func TestWriterLockHelperProcess(t *testing.T) {
	if os.Getenv("ANW_WRITERLOCK_HELPER") != "1" {
		return
	}
	input := bufio.NewReader(os.Stdin)
	fmt.Fprintln(os.Stdout, "ready")
	if _, err := input.ReadString('\n'); err != nil {
		os.Exit(2)
	}
	lock, err := Acquire(os.Getenv("ANW_WRITERLOCK_ROOT"))
	if err != nil {
		if errors.Is(err, ErrLocked) {
			fmt.Fprintln(os.Stdout, "busy")
			os.Exit(0)
		}
		fmt.Fprintln(os.Stdout, "failed")
		os.Exit(3)
	}
	fmt.Fprintln(os.Stdout, "won")
	if _, err := input.ReadString('\n'); err != nil {
		os.Exit(4)
	}
	if err := lock.Close(); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

type lockChild struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	stderr bytes.Buffer
}

func startLockChild(t *testing.T, root string) *lockChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	child := &lockChild{cmd: exec.CommandContext(ctx, executable, "-test.run=^TestWriterLockHelperProcess$")}
	child.cmd.Env = append(os.Environ(), "ANW_WRITERLOCK_HELPER=1", "ANW_WRITERLOCK_ROOT="+root)
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
	if line := child.line(t); line != "ready" {
		t.Fatalf("helper readiness: %q", line)
	}
	return child
}

func (child *lockChild) line(t *testing.T) string {
	t.Helper()
	line, err := child.output.ReadString('\n')
	if err != nil {
		t.Fatalf("helper protocol: %v", err)
	}
	return strings.TrimSuffix(line, "\n")
}

func (child *lockChild) signal(t *testing.T) {
	t.Helper()
	if _, err := io.WriteString(child.input, "continue\n"); err != nil {
		t.Fatalf("helper signal: %v", err)
	}
}

func (child *lockChild) wait(t *testing.T) {
	t.Helper()
	if err := child.cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v; stderr: %s", err, child.stderr.String())
	}
}

func TestCrossProcessContendersHaveOneWinner(t *testing.T) {
	root := privateDirectory(t)
	const count = 6
	children := make([]*lockChild, count)
	for i := range children {
		children[i] = startLockChild(t, root)
	}
	for _, child := range children {
		child.signal(t)
	}
	var winners []*lockChild
	for _, child := range children {
		switch result := child.line(t); result {
		case "won":
			winners = append(winners, child)
		case "busy":
		default:
			t.Fatalf("unexpected helper acquisition: %q", result)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("expected one child-process winner, got %d", len(winners))
	}
	if lock, err := Acquire(root); lock != nil || !errors.Is(err, ErrLocked) {
		if lock != nil {
			lock.Close()
		}
		t.Fatalf("parent acquired while child holds lock: %v", err)
	}
	before, err := os.Stat(filepath.Join(root, lockName))
	if err != nil {
		t.Fatal(err)
	}
	winners[0].signal(t)
	for _, child := range children {
		child.wait(t)
	}
	lock, err := Acquire(root)
	if err != nil {
		t.Fatalf("parent reacquisition after child releases: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(root, lockName))
	if err != nil || !sameLock(before, after) {
		t.Fatalf("child release changed lock inode: %v", err)
	}
}

func TestChildCannotAcquireParentLockAndCanReacquire(t *testing.T) {
	root := privateDirectory(t)
	lock, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	child := startLockChild(t, root)
	child.signal(t)
	if line := child.line(t); line != "busy" {
		t.Fatalf("child acquired parent-held flock: %q", line)
	}
	child.wait(t)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	child = startLockChild(t, root)
	child.signal(t)
	if line := child.line(t); line != "won" {
		t.Fatalf("child could not acquire released flock: %q", line)
	}
	child.signal(t)
	child.wait(t)
}

func TestLockDescriptorNotInheritedByExec(t *testing.T) {
	root := privateDirectory(t)
	lock, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	// Start the child before releasing the parent. A missing CLOEXEC would
	// leave the parent's open file description (and flock) alive in the child.
	child := startLockChild(t, root)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	child.signal(t)
	if line := child.line(t); line != "won" {
		t.Fatalf("exec inherited a live lock descriptor: %q", line)
	}
	child.signal(t)
	child.wait(t)
}

func TestKernelReleasesLockOnChildExit(t *testing.T) {
	root := privateDirectory(t)
	child := startLockChild(t, root)
	child.signal(t)
	if line := child.line(t); line != "won" {
		t.Fatalf("child acquisition: %q", line)
	}
	before, err := os.Stat(filepath.Join(root, lockName))
	if err != nil {
		t.Fatal(err)
	}
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.cmd.Wait(); err == nil {
		t.Fatal("killed helper unexpectedly exited successfully")
	}
	lock, err := Acquire(root)
	if err != nil {
		t.Fatalf("kernel did not release exited process lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(root, lockName))
	if err != nil || !sameLock(before, after) {
		t.Fatalf("child exit removed/changed lock inode: %v", err)
	}
}
