//go:build linux

package projectfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestEmptyCanonicalAssetDirectoryIsNotAnAsset pins the rule that an asset
// directory with nothing in it carries no author data.
//
// It matters because the write admission refuses a project whose knowledge asset
// is present but has no proven native writer (projectaccess.ErrAuthorKnowledgeUnproved).
// The shipped gateway leaves an empty .ai-novel/lancedb behind whenever the author
// clears the knowledge base, and a footprint with no bytes in it is not something
// that writer could destroy - so refusing the whole project would block opening a
// book for a directory that holds nothing.
func TestEmptyCanonicalAssetDirectoryIsNotAnAsset(t *testing.T) {
	library, path, storage := fixture(t)
	for _, name := range []string{"lancedb", "knowledge-copies"} {
		if err := os.Mkdir(filepath.Join(storage, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, project := openFixture(t, library, path)
	proof, err := project.InventoryCanonicalAssets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := proof.Summary(); got != (CanonicalAssets{}) {
		t.Fatalf("two empty asset directories were reported as assets: %+v", got)
	}
	// The directories are still tracked, so a later write that creates or replaces
	// them is still observed as a change.
	if err := proof.VerifyUnchanged(context.Background()); err != nil {
		t.Fatalf("an unchanged inventory did not verify: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storage, "lancedb", "table.lance"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := proof.VerifyUnchanged(context.Background()); err == nil {
		t.Fatal("a table appearing inside the empty asset directory went unobserved")
	}
}

// TestOnePopulatedAssetDirectoryIsStillAnAsset is the other half: the rule may
// only ignore emptiness, never content.
func TestOnePopulatedAssetDirectoryIsStillAnAsset(t *testing.T) {
	library, path, storage := fixture(t)
	for _, name := range []string{"lancedb", "knowledge-copies"} {
		if err := os.Mkdir(filepath.Join(storage, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(storage, "knowledge-copies", "copy.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, project := openFixture(t, library, path)
	proof, err := project.InventoryCanonicalAssets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := proof.Summary()
	if got.LanceDB {
		t.Fatal("the empty lancedb directory was reported as an asset")
	}
	if !got.KnowledgeCopies {
		t.Fatal("a knowledge-copies directory holding a file was reported as empty")
	}
}
