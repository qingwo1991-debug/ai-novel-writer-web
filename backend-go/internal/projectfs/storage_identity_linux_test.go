//go:build linux

package projectfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStorageIdentityBindsPinnedAndVisibleDirectory(t *testing.T) {
	library, path, storage := fixture(t)
	l, p := openFixture(t, library, path)
	wantInfo, err := os.Stat(storage)
	if err != nil {
		t.Fatal(err)
	}
	want := wantInfo.Sys().(*syscall.Stat_t)
	device, inode, err := p.StorageIdentity()
	if err != nil || device != uint64(want.Dev) || inode != uint64(want.Ino) {
		t.Fatalf("pinned storage identity: device=%d inode=%d err=%v", device, inode, err)
	}
	old := filepath.Join(path, ".old-ai-novel")
	if err := os.Rename(storage, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(storage, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.StorageIdentity(); !errors.Is(err, ErrChanged) {
		t.Fatalf("replacement was accepted: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.StorageIdentity(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed project was accepted: %v", err)
	}
	if _, _, err := (*Project)(nil).StorageIdentity(); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil project was accepted: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}
