//go:build linux

package projectfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCanonicalAssetMembershipWithoutContentRead(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "populated"}[populated], func(t *testing.T) {
			library, path, storage := fixture(t)
			if populated {
				for _, name := range []string{"lancedb", "knowledge-copies"} {
					if err := os.Mkdir(filepath.Join(storage, name), 0700); err != nil {
						t.Fatal(err)
					}
					put(t, filepath.Join(storage, name, "synthetic-child"), []byte("canonical body preserved not parsed"))
				}
				put(t, filepath.Join(storage, "embedding-spaces.json"), []byte("not parsed - membership only"))
			}
			_, p := openFixture(t, library, path)
			proof, err := p.InventoryCanonicalAssets(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if proof.Summary() != (CanonicalAssets{LanceDB: populated, EmbeddingRegistry: populated, KnowledgeCopies: populated}) {
				t.Fatal(proof.Summary())
			}
			if err := proof.VerifyUnchanged(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCanonicalAssetRejectsUnsafeFixedEntries(t *testing.T) {
	for _, kind := range []string{"lance-symlink", "lance-file", "copies-file", "registry-dir", "registry-hardlink", "registry-fifo"} {
		t.Run(kind, func(t *testing.T) {
			library, path, storage := fixture(t)
			switch kind {
			case "lance-symlink":
				link(t, "missing", filepath.Join(storage, "lancedb"), true)
			case "lance-file":
				put(t, filepath.Join(storage, "lancedb"), nil)
			case "copies-file":
				put(t, filepath.Join(storage, "knowledge-copies"), nil)
			case "registry-dir":
				if err := os.Mkdir(filepath.Join(storage, "embedding-spaces.json"), 0700); err != nil {
					t.Fatal(err)
				}
			case "registry-hardlink":
				source := filepath.Join(storage, "source")
				put(t, source, nil)
				link(t, source, filepath.Join(storage, "embedding-spaces.json"), false)
			case "registry-fifo":
				if err := syscall.Mkfifo(filepath.Join(storage, "embedding-spaces.json"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, p := openFixture(t, library, path)
			if _, err := p.InventoryCanonicalAssets(context.Background()); err == nil {
				t.Fatal("unsafe inventory admitted")
			}
		})
	}
}
func TestCanonicalAssetObservedReplacementCreationAndMetadata(t *testing.T) {
	for _, kind := range []string{"create", "replace", "metadata", "remove"} {
		t.Run(kind, func(t *testing.T) {
			library, path, storage := fixture(t)
			name := filepath.Join(storage, "embedding-spaces.json")
			if kind != "create" {
				put(t, name, []byte("original"))
			}
			_, p := openFixture(t, library, path)
			proof, err := p.InventoryCanonicalAssets(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "create":
				put(t, name, []byte("new"))
			case "replace":
				remove(t, name)
				put(t, name, []byte("replacement"))
			case "metadata":
				if err := os.Chmod(name, 0644); err != nil {
					t.Fatal(err)
				}
			case "remove":
				remove(t, name)
			}
			if err := proof.VerifyUnchanged(context.Background()); !errors.Is(err, ErrChanged) {
				t.Fatal(err)
			}
		})
	}
}
func TestCanonicalAssetCancellationAndClosedCapabilities(t *testing.T) {
	library, path, _ := fixture(t)
	_, p := openFixture(t, library, path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.InventoryCanonicalAssets(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	proof, err := p.InventoryCanonicalAssets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := proof.VerifyUnchanged(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	p.Close()
	if err := proof.VerifyUnchanged(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := p.InventoryCanonicalAssets(nil); !errors.Is(err, ErrInvalidPath) {
		t.Fatal(err)
	}
	if err := (&AssetInventory{}).VerifyUnchanged(context.Background()); !errors.Is(err, ErrInvalidPath) {
		t.Fatal(err)
	}
}
