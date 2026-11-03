//go:build linux

package projectfs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestVisibleTreePinnedProjectShapeAndHiddenStorage(t *testing.T) {
	library, path, _ := fixture(t)
	for _, name := range []string{"02_章节", "01_设定", "empty"} {
		if err := os.Mkdir(filepath.Join(path, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(path, ".vela"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(path, ".vela", "custom.json"), []byte("{}"))
	put(t, filepath.Join(path, "02_章节", "第一章.txt"), []byte("synthetic"))
	put(t, filepath.Join(path, "z.txt"), []byte("visible"))
	put(t, filepath.Join(path, ".hidden"), []byte("private"))
	_, project := openFixture(t, library, path)
	tree, err := project.ListVisibleTree(context.Background(), ".", 30, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 4 || !tree[0].IsDir || !tree[1].IsDir || !tree[2].IsDir || tree[3].Name != "z.txt" {
		t.Fatalf("directory-first visible tree: %#v", tree)
	}
	for _, node := range tree {
		if strings.HasPrefix(node.Name, ".") || node.Path != filepath.Join(path, node.Name) {
			t.Fatalf("hidden or forged path: %+v", node)
		}
	}
	var chapters, empty *VisibleNode
	for index := range tree {
		switch tree[index].Name {
		case "02_章节":
			chapters = &tree[index]
		case "empty":
			empty = &tree[index]
		}
	}
	if chapters == nil || chapters.Children == nil || len(*chapters.Children) != 1 || (*chapters.Children)[0].Path != filepath.Join(path, "02_章节", "第一章.txt") || empty == nil || empty.Children == nil || len(*empty.Children) != 0 {
		t.Fatalf("recursive tree mismatch: %#v", tree)
	}
	wired, err := json.Marshal(tree)
	if err != nil || !strings.Contains(string(wired), `"children":[]`) || strings.Contains(string(wired), ".ai-novel") {
		t.Fatalf("browser tree shape: %s %v", wired, err)
	}
	subtree, err := project.ListVisibleTree(context.Background(), "02_章节", 10, 3)
	if err != nil || len(subtree) != 1 || subtree[0].Name != "第一章.txt" {
		t.Fatalf("subdirectory enumeration: %+v %v", subtree, err)
	}
	hiddenSubtree, err := project.ListVisibleTree(context.Background(), ".vela", 10, 3)
	if err != nil || len(hiddenSubtree) != 1 || hiddenSubtree[0].Name != "custom.json" {
		t.Fatalf("explicit hidden subdirectory enumeration: %+v %v", hiddenSubtree, err)
	}
}

func TestVisibleTreeLimitsAndUnsafeEntriesFailClosed(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			library, path, _ := fixture(t)
			target := filepath.Join(path, "target.txt")
			put(t, target, []byte("synthetic"))
			unsafe := filepath.Join(path, "unsafe.txt")
			switch kind {
			case "symlink":
				link(t, "target.txt", unsafe, true)
			case "hardlink":
				link(t, target, unsafe, false)
			case "fifo":
				// A named pipe must never be opened while listing.
				if err := syscall.Mkfifo(unsafe, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, project := openFixture(t, library, path)
			if _, err := project.ListVisibleTree(context.Background(), ".", 20, 4); err == nil {
				t.Fatal("unsafe visible entry accepted")
			}
		})
	}
	library, path, _ := fixture(t)
	for _, name := range []string{"a", "b"} {
		put(t, filepath.Join(path, name), []byte(name))
	}
	_, project := openFixture(t, library, path)
	if _, err := project.ListVisibleTree(context.Background(), ".", 1, 4); !errors.Is(err, ErrTreeLimit) {
		t.Fatalf("entry bound ignored: %v", err)
	}
	if _, err := project.ListVisibleTree(context.Background(), "../escape", 10, 4); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("traversal accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := project.ListVisibleTree(ctx, ".", 10, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled listing accepted: %v", err)
	}
}
