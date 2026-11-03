//go:build linux

package knowledgecopy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveManagedCopyCASAndUnknownMetadata(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	id := "作者资料-😀"
	name := hashText(id) + ".json"
	original := []byte(`{"content":"原文","indexedHash":"` + hashText("原文") + `","edited":false,"indexDirty":false,"future":{"keep":1}}`)
	put(t, filepath.Join(path, "knowledge-copies"), name, original)
	refs := []DocumentRef{{ID: id, FilePath: "knowledge-copy:" + id}}
	if err := SaveManagedCopy(context.Background(), root, refs, budget(), id, "新文\n😀", hashText("原文")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(path, "knowledge-copies", name))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["content"] != "新文\n😀" || got["indexedHash"] != hashText("原文") || got["edited"] != true || got["indexDirty"] != true {
		t.Fatalf("copy update = %#v", got)
	}
	if future, ok := got["future"].(map[string]any); !ok || future["keep"] != float64(1) {
		t.Fatalf("unknown metadata lost: %#v", got)
	}
	if err := SaveManagedCopy(context.Background(), root, refs, budget(), id, "stale overwrite", hashText("原文")); !errors.Is(err, ErrCopyChanged) {
		t.Fatalf("stale CAS = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(path, "knowledge-copies", name))
	if err != nil || string(after) != string(raw) {
		t.Fatalf("stale write changed file: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(path, "knowledge-copies"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary file retained: %v, %v", entries, err)
	}
}

func TestSaveManagedCopyRequiresAdmittedPresentManagedDocument(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	putLinkedCopy(t, path, "managed", "content", hashText("content"), false, false)
	before, err := os.ReadFile(filepath.Join(path, "knowledge-copies", hashText("managed")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id   string
		refs []DocumentRef
		want error
	}{
		{"managed", nil, ErrCopyUnavailable},
		{"managed", []DocumentRef{{ID: "managed", FilePath: "ordinary.txt"}}, ErrCopyUnavailable},
		{"missing", []DocumentRef{{ID: "missing", FilePath: "knowledge-copy:missing"}}, ErrCopyUnavailable},
	}
	for _, test := range cases {
		if err := SaveManagedCopy(context.Background(), root, test.refs, budget(), test.id, "replacement", hashText("content")); !errors.Is(err, test.want) {
			t.Fatalf("%s: %v", test.id, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(path, "knowledge-copies", hashText("managed")+".json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("unauthorized write changed file: %v", err)
	}
}

func TestSaveManagedCopyRejectsUnsafeEntryAndOversizeReplacement(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	id := "safe"
	name := hashText(id) + ".json"
	copyPath := filepath.Join(path, "knowledge-copies", name)
	putLinkedCopy(t, path, id, "old", hashText("old"), false, false)
	refs := []DocumentRef{{ID: id, FilePath: "knowledge-copy:" + id}}
	base, err := Validate(context.Background(), root, budget())
	if err != nil {
		t.Fatal(err)
	}
	b := budget()
	b.MaxMetadataBytes = base.MetadataBytes + 1
	if err := SaveManagedCopy(context.Background(), root, refs, b, id, "replacement longer than original", hashText("old")); !errors.Is(err, ErrBudget) {
		t.Fatalf("oversize = %v", err)
	}
	before, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(copyPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(path, "outside.json")
	put(t, path, "outside.json", before)
	if err := os.Symlink(outside, copyPath); err != nil {
		t.Fatal(err)
	}
	if err := SaveManagedCopy(context.Background(), root, refs, budget(), id, "replacement", hashText("old")); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlink = %v", err)
	}
	after, err := os.ReadFile(outside)
	if err != nil || string(after) != string(before) {
		t.Fatalf("outside changed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(path, "knowledge-copies"))
	if err != nil || len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("unexpected files: %v %v", entries, err)
	}
}
