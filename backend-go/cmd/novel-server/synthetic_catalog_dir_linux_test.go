//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyntheticCatalogDirectoryRejectsUnsafeExistingEntry(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, syntheticCatalogDirectoryName)
	if err := ensureSyntheticCatalogDirectory(root, directory); err != nil {
		t.Fatal(err)
	}
	if err := ensureSyntheticCatalogDirectory(root, directory); err != nil {
		t.Fatal("safe existing metadata refused", err)
	}
	if err := os.Chmod(directory, 0750); err != nil {
		t.Fatal(err)
	}
	if err := ensureSyntheticCatalogDirectory(root, directory); err == nil {
		t.Fatal("permissive metadata directory accepted")
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, directory); err != nil {
		t.Fatal(err)
	}
	if err := ensureSyntheticCatalogDirectory(root, directory); err == nil {
		t.Fatal("metadata symlink accepted")
	}
}
