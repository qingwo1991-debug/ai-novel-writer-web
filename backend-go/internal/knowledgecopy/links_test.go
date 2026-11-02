//go:build linux

package knowledgecopy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func putLinkedCopy(t *testing.T, path, id, content, indexed string, edited, dirty bool) {
	t.Helper()
	put(t, filepath.Join(path, "knowledge-copies"), hashText(id)+".json", copyJSON(content, indexed, edited, dirty))
}

func requireLinks(t *testing.T, root *os.Root, docs []DocumentRef, b Budget, want LinkSummary) {
	t.Helper()
	got, err := ValidateLinks(context.Background(), root, docs, b)
	if err != nil || got != want {
		t.Fatalf("summary=%+v error=%v expected=%+v", got, err, want)
	}
}

func requireLinksFailure(t *testing.T, ctx context.Context, root *os.Root, docs []DocumentRef, b Budget, want error) {
	t.Helper()
	got, err := ValidateLinks(ctx, root, docs, b)
	if !errors.Is(err, want) || got != (LinkSummary{}) {
		t.Fatalf("summary=%+v error=%v expected=%v", got, err, want)
	}
}

func TestLinksCurrentStatesAndEditedIndependence(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	states := []struct {
		id, content, indexed string
		edited, dirty        bool
	}{
		{"current", "plain", hashText("plain"), false, false},
		{"edited", "legitimate edit", hashText("legitimate edit"), true, false},
		{"dirty", "pending", hashText("pending"), false, true},
		{"mismatch", "changed", hashText("old"), false, false},
		{"both", "pending edit", "", true, true},
	}
	var docs []DocumentRef
	for _, item := range states {
		putLinkedCopy(t, path, item.id, item.content, item.indexed, item.edited, item.dirty)
		docs = append(docs, DocumentRef{ID: item.id, FilePath: "knowledge-copy:" + item.id})
	}
	requireLinks(t, root, docs, budget(), LinkSummary{Documents: 5, Managed: 5, Present: 5, Current: 2, Dirty: 2, Edited: 2, HashMismatch: 2})
}

func TestLinksMissingOptionalDirectoryAndMissingFileNonfatal(t *testing.T) {
	for _, withDirectory := range []bool{false, true} {
		path, root := fixture(t)
		if withDirectory {
			copiesDir(t, path)
		}
		docs := []DocumentRef{{ID: "missing", FilePath: "knowledge-copy:missing"}, {ID: "unmanaged", FilePath: "not-a-file"}}
		requireLinks(t, root, docs, budget(), LinkSummary{Documents: 2, Managed: 1, Missing: 1})
	}
}

func TestLinksOrphanAndUnmanagedCopiesRemainUnreferenced(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	putLinkedCopy(t, path, "managed", "a", hashText("a"), false, false)
	putLinkedCopy(t, path, "unmanaged", "b", hashText("b"), true, true)
	putLinkedCopy(t, path, "orphan", "c", "", false, false)
	docs := []DocumentRef{{ID: "managed", FilePath: "knowledge-copy:"}, {ID: "unmanaged", FilePath: ""}}
	requireLinks(t, root, docs, budget(), LinkSummary{Documents: 2, Managed: 1, Present: 1, Current: 1, Unreferenced: 2})
	requireLinks(t, root, nil, budget(), LinkSummary{Unreferenced: 3})
}

func TestLinksMarkerPrefixExactAndSuffixIgnored(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	for _, id := range []string{"id", "other"} {
		putLinkedCopy(t, path, id, "value", hashText("value"), false, false)
	}
	for _, marker := range []string{"knowledge-copy:id", "knowledge-copy:", "knowledge-copy:other", "knowledge-copy:../other", "knowledge-copy:任意🙂"} {
		requireLinks(t, root, []DocumentRef{{ID: "id", FilePath: marker}}, budget(), LinkSummary{Documents: 1, Managed: 1, Present: 1, Current: 1, Unreferenced: 1})
	}
	for _, marker := range []string{"", "Knowledge-copy:id", "knowledge-copy", " knowledge-copy:id", "/tmp/nonexistent", "xknowledge-copy:id"} {
		requireLinks(t, root, []DocumentRef{{ID: "id", FilePath: marker}}, budget(), LinkSummary{Documents: 1, Unreferenced: 2})
	}
}

func TestLinksExactUTF8DocumentHashAndNoPathInterpretation(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	docs := []DocumentRef{
		{ID: "文档Ω🙂", FilePath: "knowledge-copy:ignored"},
		{ID: "../outside\x00\\file", FilePath: "knowledge-copy:"},
		{ID: "é", FilePath: "knowledge-copy:"},
		{ID: "e\u0301", FilePath: "knowledge-copy:"},
	}
	for _, doc := range docs {
		putLinkedCopy(t, path, doc.ID, "🙂Ω正文", hashText("🙂Ω正文"), false, false)
	}
	requireLinks(t, root, docs, budget(), LinkSummary{Documents: 4, Managed: 4, Present: 4, Current: 4})
}

func TestLinksReferenceIdentityValidation(t *testing.T) {
	_, root := fixture(t)
	for _, docs := range [][]DocumentRef{
		{{ID: "", FilePath: "knowledge-copy:"}},
		{{ID: "same"}, {ID: "same", FilePath: "knowledge-copy:same"}},
		{{ID: string([]byte{0xff})}},
		{{ID: "valid", FilePath: string([]byte{0xff})}},
	} {
		requireLinksFailure(t, context.Background(), root, docs, budget(), ErrInput)
	}
	requireLinks(t, root, []DocumentRef{{ID: " "}, {ID: "\x00"}}, budget(), LinkSummary{Documents: 2})
}

func TestLinksBudgetsAggregateOnceAndBoundBothCounts(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	raw := copyJSON("body", hashText("body"), false, false)
	reg := registry("[]", "null")
	put(t, path, "embedding-spaces.json", reg)
	put(t, filepath.Join(path, "knowledge-copies"), hashText("doc")+".json", raw)
	docs := []DocumentRef{{ID: "doc", FilePath: "knowledge-copy:"}}
	exact := Budget{MaxMetadataBytes: int64(len(raw) + len(reg)), MaxCopies: 1}
	requireLinks(t, root, docs, exact, LinkSummary{Documents: 1, Managed: 1, Present: 1, Current: 1})
	tooSmall := exact
	tooSmall.MaxMetadataBytes--
	requireLinksFailure(t, context.Background(), root, docs, tooSmall, ErrBudget)
	requireLinksFailure(t, context.Background(), root, []DocumentRef{{ID: "a"}, {ID: "b"}}, exact, ErrBudget)
	putLinkedCopy(t, path, "orphan", "", hashText(""), false, false)
	requireLinksFailure(t, context.Background(), root, docs, Budget{MaxMetadataBytes: MaxMetadataBytes, MaxCopies: 1}, ErrBudget)
	for _, invalid := range []Budget{{}, {MaxMetadataBytes: 1}, {MaxMetadataBytes: MaxMetadataBytes + 1, MaxCopies: 1}, {MaxMetadataBytes: 1, MaxCopies: MaxCopies + 1}} {
		requireLinksFailure(t, context.Background(), root, docs, invalid, ErrInput)
	}
}

func TestLinksAggregateReferenceBytesBeforeUTF8AndHashing(t *testing.T) {
	_, root := fixture(t)
	b := Budget{MaxMetadataBytes: 4, MaxCopies: 2}
	requireLinks(t, root, []DocumentRef{{ID: "文", FilePath: "x"}}, b, LinkSummary{Documents: 1})
	for _, docs := range [][]DocumentRef{
		{{ID: "12345"}},
		{{ID: "1", FilePath: "1234"}},
		{{ID: "12"}, {ID: "34", FilePath: "5"}},
		{{ID: "文", FilePath: "文"}},
	} {
		requireLinksFailure(t, context.Background(), root, docs, b, ErrBudget)
	}
}

func TestLinksGloballyValidateMalformedUnreferencedMetadata(t *testing.T) {
	for _, asset := range []string{"registry", "orphan", "copy-name", "duplicate-key"} {
		t.Run(asset, func(t *testing.T) {
			path, root := fixture(t)
			copiesDir(t, path)
			putLinkedCopy(t, path, "doc", "valid", hashText("valid"), false, false)
			switch asset {
			case "registry":
				put(t, path, "embedding-spaces.json", []byte(`{"version":2,"spaces":[]}`))
			case "orphan":
				put(t, filepath.Join(path, "knowledge-copies"), hashText("orphan")+".json", []byte(`{}`))
			case "copy-name":
				put(t, filepath.Join(path, "knowledge-copies"), "not-a-copy.json", []byte(`{}`))
			case "duplicate-key":
				put(t, filepath.Join(path, "knowledge-copies"), hashText("orphan")+".json", []byte(`{"content":"","content":"","indexedHash":"","edited":false,"indexDirty":false}`))
			}
			requireLinksFailure(t, context.Background(), root, []DocumentRef{{ID: "doc", FilePath: "knowledge-copy:"}}, budget(), ErrInvalid)
		})
	}
}

func TestLinksUnsafeManagedOrOrphanFilesAndPrivateDirectories(t *testing.T) {
	for _, kind := range []string{"managed-symlink", "orphan-hardlink", "copies-symlink", "public-root", "public-copies"} {
		t.Run(kind, func(t *testing.T) {
			path, root := fixture(t)
			outside := filepath.Join(t.TempDir(), "external")
			if err := os.WriteFile(outside, copyJSON("", hashText(""), false, false), 0600); err != nil {
				t.Fatal(err)
			}
			if kind != "copies-symlink" {
				copiesDir(t, path)
			}
			var err error
			switch kind {
			case "managed-symlink":
				err = os.Symlink(outside, filepath.Join(path, "knowledge-copies", hashText("doc")+".json"))
			case "orphan-hardlink":
				err = os.Link(outside, filepath.Join(path, "knowledge-copies", hashText("orphan")+".json"))
			case "copies-symlink":
				err = os.Symlink(t.TempDir(), filepath.Join(path, "knowledge-copies"))
			case "public-root":
				err = os.Chmod(path, 0755)
			case "public-copies":
				err = os.Chmod(filepath.Join(path, "knowledge-copies"), 0755)
			}
			if err != nil {
				t.Fatal(err)
			}
			requireLinksFailure(t, context.Background(), root, []DocumentRef{{ID: "doc", FilePath: "knowledge-copy:"}}, budget(), ErrUnsafe)
		})
	}
}

func TestLinksContextNilClosedAndLegacySentinels(t *testing.T) {
	path, root := fixture(t)
	docs := []DocumentRef{{ID: "doc", FilePath: "knowledge-copy:"}}
	requireLinksFailure(t, nil, root, docs, budget(), ErrInput)
	requireLinksFailure(t, context.Background(), nil, docs, budget(), ErrInput)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requireLinksFailure(t, ctx, root, docs, budget(), context.Canceled)
	deadline, stop := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer stop()
	requireLinksFailure(t, deadline, root, docs, budget(), context.DeadlineExceeded)
	put(t, path, "vectors.json", []byte("not parsed"))
	requireLinksFailure(t, context.Background(), root, docs, budget(), ErrLegacyUnsupported)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	requireLinksFailure(t, context.Background(), root, docs, budget(), ErrUnsafe)
}

func TestLinksCancellationDuringReadAndPostReadIdentityGuard(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	putLinkedCopy(t, path, "doc", "body", hashText("body"), false, false)
	docs := []DocumentRef{{ID: "doc", FilePath: "knowledge-copy:"}}
	cancelDuring := &steppedContext{Context: context.Background(), at: 6, cancel: true}
	requireLinksFailure(t, cancelDuring, root, docs, budget(), context.Canceled)
	countCalls := &steppedContext{Context: context.Background(), at: 1 << 30}
	if _, err := ValidateLinks(countCalls, root, docs, budget()); err != nil {
		t.Fatal(err)
	}
	// The final per-copy identity check precedes the last two legacy context
	// checks and the return context check. Mutate at that guard, not after it.
	changed := &steppedContext{Context: context.Background(), at: countCalls.calls - 3}
	changed.action = func() {
		putLinkedCopy(t, path, "doc", "body with change", hashText("body with change"), false, false)
	}
	requireLinksFailure(t, changed, root, docs, budget(), ErrChanged)
}

func TestLinksNoMutationAndOnlyCamelCaseAggregateOutput(t *testing.T) {
	path, root := fixture(t)
	copiesDir(t, path)
	put(t, path, "embedding-spaces.json", append([]byte(" \n"), registry("[]", "null")...))
	putLinkedCopy(t, path, "private-id", "SYNTHETIC prose Ω🙂", hashText("SYNTHETIC prose Ω🙂"), true, false)
	paths := []string{path, filepath.Join(path, "knowledge-copies"), filepath.Join(path, "embedding-spaces.json"), filepath.Join(path, "knowledge-copies", hashText("private-id")+".json")}
	before := make(map[string]os.FileInfo)
	bytes := make(map[string]string)
	for _, file := range paths {
		info, err := os.Lstat(file)
		if err != nil {
			t.Fatal(err)
		}
		before[file] = info
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			bytes[file] = string(raw)
		}
	}
	out, err := ValidateLinks(context.Background(), root, []DocumentRef{{ID: "private-id", FilePath: "knowledge-copy:unrelated"}}, budget())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range paths {
		after, err := os.Lstat(file)
		if err != nil || !sameEntry(before[file], after) {
			t.Fatal("metadata changed", err)
		}
		if after.Mode().IsRegular() {
			raw, err := os.ReadFile(file)
			if err != nil || string(raw) != bytes[file] {
				t.Fatal("raw bytes changed", err)
			}
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"documents":1,"managed":1,"present":1,"missing":0,"current":1,"dirty":0,"edited":1,"hashMismatch":0,"unreferenced":0}`
	if string(raw) != want || strings.Contains(string(raw), "private-id") || strings.Contains(string(raw), "SYNTHETIC") {
		t.Fatal("unexpected public aggregate shape")
	}
}
