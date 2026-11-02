//go:build linux

package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func writePublicKey(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestLoadExistingMissingDoesNotCreate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "missing", "key")
	if _, err := LoadExistingKey(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read-only loader wrote directories: %v", err)
	}
}

func TestCreatePreservePrivateKey(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "new", "nested", "server-credentials.key")
	first, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatal("incorrect key length")
	}
	info, err := os.Lstat(path)
	if err != nil || !privateKey(info) {
		t.Fatalf("key must be single-link owned regular 0600: %v", err)
	}
	for _, dir := range []string{filepath.Join(root, "new"), filepath.Dir(path)} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("private created directory: %v", err)
		}
	}
	for _, load := range []func(string) ([]byte, error){LoadExistingKey, LoadOrCreateKey} {
		next, err := load(path)
		if err != nil || !bytes.Equal(first, next) {
			t.Fatalf("key preservation: %v", err)
		}
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(info, after) {
		t.Fatalf("existing key replaced: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "server-credentials.key" {
		t.Fatalf("temporary file leaked: %v", err)
	}
}

func TestExistingCorruptAndPermissionsNeverOverwritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"empty", nil, 0o600},
		{"short", []byte("damaged synthetic public data"), 0o600},
		{"long", bytes.Repeat([]byte{0x47}, 33), 0o600},
		{"world-readable", bytes.Repeat([]byte{0x47}, 32), 0o644},
		{"read-only", bytes.Repeat([]byte{0x47}, 32), 0o400},
		{"executable", bytes.Repeat([]byte{0x47}, 32), 0o700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			writePublicKey(t, path, tc.data, tc.mode)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, load := range []func(string) ([]byte, error){LoadExistingKey, LoadOrCreateKey} {
				if _, err := load(path); !errors.Is(err, ErrKeyInvalid) {
					t.Fatalf("accepted invalid key: %v", err)
				}
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() {
				t.Fatalf("invalid key replaced or chmodded: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, tc.data) {
				t.Fatalf("invalid key content changed: %v", err)
			}
		})
	}
}

func TestRejectLinksAndNonregularKeys(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling-symlink", "hardlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "key")
			target := filepath.Join(root, "target")
			writePublicKey(t, target, bytes.Repeat([]byte{0x47}, 32), 0o600)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("target", path)
			case "dangling-symlink":
				err = os.Symlink("absent", path)
			case "hardlink":
				err = os.Link(target, path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreateKey(path); !errors.Is(err, ErrKeyInvalid) {
				t.Fatalf("accepted %s key: %v", kind, err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("rejected key entry changed: %v", err)
			}
		})
	}
}

func TestRejectUnsafeAncestorsAndPaths(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "physical")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink("physical", alias); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(root, "writable")
	if err := os.Mkdir(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "relative/key", "/", root + "/./key", root + "//key", root + "/../key", root + "/bad\\key", root + "/bad\x00key", filepath.Join(alias, "key"), filepath.Join(writable, "key"), filepath.Join(writable, "child", "key")} {
		if _, err := LoadOrCreateKey(path); !errors.Is(err, ErrKeyInvalid) {
			t.Fatalf("unsafe path %q: %v", path, err)
		}
	}
	entries, err := os.ReadDir(physical)
	if err != nil || len(entries) != 0 {
		t.Fatalf("followed symlink ancestor: %v", err)
	}
}

func TestConcurrentCreateUsesOneKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "key")
	const count = 32
	results := make([][]byte, count)
	errorsFound := make([]error, count)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errorsFound[i] = LoadOrCreateKey(path)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < count; i++ {
		if errorsFound[i] != nil || len(results[i]) != 32 || !bytes.Equal(results[0], results[i]) {
			t.Fatalf("creator %d: %v", i, errorsFound[i])
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "key" {
		t.Fatalf("concurrent temporary file leaked: %v", err)
	}
}

func TestExclusivePublishPreservesExistingWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	root, name, err := keyParent(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	winner := bytes.Repeat([]byte{0x47}, 32)
	writePublicKey(t, path, winner, 0o600)
	if err := createKeyAt(root, dir, name); err != nil {
		t.Fatal(err)
	}
	got, err := readKeyAt(root, dir, name)
	if err != nil || !bytes.Equal(got, winner) {
		t.Fatalf("exclusive publish overwrote winner: %v", err)
	}
	// An invalid racing destination must also be preserved, never repaired.
	writePublicKey(t, path, []byte("corrupt public fixture"), 0o600)
	if err := createKeyAt(root, dir, name); err != nil {
		t.Fatal(err)
	}
	if _, err := readKeyAt(root, dir, name); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("accepted corrupt race winner: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "corrupt public fixture" {
		t.Fatalf("corrupt race winner modified: %v", err)
	}
}

func TestPinnedParentDoesNotReuseDestination(t *testing.T) {
	base := t.TempDir()
	original := filepath.Join(base, "original")
	moved := filepath.Join(base, "moved")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	root, name, err := keyParent(filepath.Join(original, "key"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createKeyAt(root, dir, name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(original, "key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reused raw destination: %v", err)
	}
	if _, err := LoadExistingKey(filepath.Join(moved, "key")); err != nil {
		t.Fatalf("publication did not use pinned parent: %v", err)
	}
}
