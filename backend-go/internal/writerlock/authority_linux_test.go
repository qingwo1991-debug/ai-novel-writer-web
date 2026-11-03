//go:build linux

package writerlock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestVerifyLegacyBindsPolicyAndPhysicalRoot(t *testing.T) {
	root := privateDirectory(t)
	private, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	if err := private.VerifyLegacy(root); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("private policy accepted: %v", err)
	}
	legacy, err := AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if err := legacy.VerifyLegacy(root); err != nil {
		t.Fatalf("matching legacy authority rejected: %v", err)
	}
	if err := legacy.VerifyLegacy(filepath.Dir(root)); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("ancestor root accepted: %v", err)
	}
	if err := legacy.VerifyLegacy(filepath.Join(root, "child")); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("descendant root accepted: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := legacy.VerifyLegacy(root); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("closed lock accepted: %v", err)
	}
}
