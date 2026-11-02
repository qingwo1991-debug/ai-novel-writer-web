//go:build linux

package globalconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	service, err := New(root)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { service.Close() })
	return service, root
}

func TestMissingGetDoesNotCreateOrMerge(t *testing.T) {
	service, root := newTestService(t)
	value, err := service.Get()
	if err != nil || !reflect.DeepEqual(jsonValue(t, value), jsonValue(t, DefaultConfig())) { t.Fatal(value, err) }
	value.(map[string]any)["proxy"].(map[string]any)["host"] = "not-shared"
	value, err = service.Get()
	if err != nil || value.(map[string]any)["proxy"].(map[string]any)["host"] != "" { t.Fatal("default aliases caller", err) }
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 { t.Fatal("read created files", err) }
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"models":[{"id":"synthetic","apiKey":"SYNTHETIC"}],"other":1}`), 0600); err != nil { t.Fatal(err) }
	value, err = service.Get()
	if err != nil { t.Fatal(err) }
	if _, exists := value.(map[string]any)["theme"]; exists { t.Fatal("read backfilled missing defaults") }
	if len(value.(map[string]any)) != 2 { t.Fatal("read lost arbitrary fields") }
}

func TestSetPreservesStageAndUnknownFields(t *testing.T) {
	service, root := newTestService(t)
	original := []byte(`{"stageModelIds":{"review":"current"},"models":[{"apiKey":"SYNTHETIC"}],"unknown":{"unicode":"中文🙂"},"proxy":{"host":"old","enabled":false}}`)
	if err := os.WriteFile(filepath.Join(root, "config.json"), original, 0600); err != nil { t.Fatal(err) }
	result := service.Set(map[string]any{"theme": "light", "stageModelIds": map[string]any{"review": "stale"}, "proxy": map[string]any{"host": "new"}})
	if !result.Success { t.Fatal(result) }
	value, err := service.Get()
	if err != nil { t.Fatal(err) }
	object := value.(map[string]any)
	if object["stageModelIds"].(map[string]any)["review"] != "current" || object["unknown"].(map[string]any)["unicode"] != "中文🙂" { t.Fatal("owned/unknown fields changed") }
	if len(object["proxy"].(map[string]any)) != 1 || len(object["models"].([]any)) != 1 { t.Fatal("shallow merge or models preservation differs") }
	if result := service.Set(map[string]any{"invalid": make(chan int)}); result.Error != ErrInvalidPatch.Error() { t.Fatal(result) }
}

func TestCorruptAndNonObjectRefuseExactBytes(t *testing.T) {
	for _, raw := range []string{"{BROKEN_SYNTHETIC", "[]", "null", "1", "false", `"value"`, "", "{} {}", "\uFEFF{}"} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			service, root := newTestService(t)
			file := filepath.Join(root, "config.json")
			if err := os.WriteFile(file, []byte(raw), 0600); err != nil { t.Fatal(err) }
			if result := service.Set(map[string]any{"theme": "light"}); result.Success || result.Error == "" { t.Fatal("unsafe overwrite", result) }
			actual, err := os.ReadFile(file)
			if err != nil || !bytes.Equal(actual, []byte(raw)) { t.Fatal("refusal modified bytes", err) }
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 { t.Fatal("temporary file leaked", err) }
		})
	}
}

func TestNoFollowAndRegularSingleLinkOnly(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "directory", "writable-by-other"} {
		t.Run(kind, func(t *testing.T) {
			service, root := newTestService(t)
			outside := filepath.Join(t.TempDir(), "outside.json")
			original := []byte(`{"synthetic":"outside-must-not-change"}`)
			if err := os.WriteFile(outside, original, 0600); err != nil { t.Fatal(err) }
			destination := filepath.Join(root, "config.json")
			var err error
			switch kind {
			case "symlink": err = os.Symlink(outside, destination)
			case "hardlink": err = os.Link(outside, destination)
			case "fifo": err = syscall.Mkfifo(destination, 0600)
			case "directory": err = os.Mkdir(destination, 0700)
			case "writable-by-other":
				err = os.WriteFile(destination, []byte(`{}`), 0600)
				if err == nil { err = os.Chmod(destination, 0666) }
			}
			if err != nil { t.Fatal(err) }
			if _, err := service.Get(); !errors.Is(err, ErrUnsafeFile) { t.Fatalf("unsafe read: %v", err) }
			if result := service.Set(map[string]any{"x": true}); result.Success || result.Error != ErrUnsafeFile.Error() { t.Fatal("unsafe set", result) }
			actual, err := os.ReadFile(outside)
			if err != nil || !bytes.Equal(actual, original) { t.Fatal("outside bytes changed", err) }
		})
	}
}

func TestExplicitRootValidationAndDescriptorPinning(t *testing.T) {
	for _, root := range []string{"", ".", "relative/path", "/", "/tmp/../tmp", "/tmp/", "/tmp\x00escape", "/" + strings.Repeat("x", 4097)} {
		if service, err := New(root); service != nil || !errors.Is(err, ErrUnsafePath) { t.Fatalf("admitted unsafe root %q: %v", root, err) }
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "original")
	if err := os.Mkdir(root, 0700); err != nil { t.Fatal(err) }
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(root, link); err != nil { t.Fatal(err) }
	if service, err := New(link); service != nil || !errors.Is(err, ErrUnsafePath) { t.Fatal("symlink root accepted", err) }
	if err := os.Chmod(root, 0777); err != nil { t.Fatal(err) }
	if service, err := New(root); service != nil || !errors.Is(err, ErrUnsafePath) { t.Fatal("world writable root accepted", err) }
	if err := os.Chmod(root, 0700); err != nil { t.Fatal(err) }
	service, err := New(root)
	if err != nil { t.Fatal(err) }; defer service.Close()
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil { t.Fatal(err) }
	if err := os.Mkdir(root, 0700); err != nil { t.Fatal(err) }
	if result := service.Set(map[string]any{"x": true}); !result.Success { t.Fatal(result) }
	if _, err := os.Stat(filepath.Join(root, "config.json")); !os.IsNotExist(err) { t.Fatal("replaced root name used", err) }
	if _, err := os.Stat(filepath.Join(moved, "config.json")); err != nil { t.Fatal("pinned directory not used", err) }
	service.Close()
	if _, err := service.Get(); !errors.Is(err, ErrClosed) { t.Fatal(err) }
}

func TestBoundedFileAndPatch(t *testing.T) {
	service, root := newTestService(t)
	file := filepath.Join(root, "config.json")
	oversized := []byte(strings.Repeat(" ", MaxConfigBytes+1))
	if err := os.WriteFile(file, oversized, 0600); err != nil { t.Fatal(err) }
	if _, err := service.Get(); !errors.Is(err, ErrTooLarge) { t.Fatal(err) }
	if result := service.Set(map[string]any{}); result.Error != ErrTooLarge.Error() { t.Fatal(result) }
	if err := os.Remove(file); err != nil { t.Fatal(err) }
	if result := service.Set(map[string]any{"huge": strings.Repeat("x", MaxConfigBytes)}); result.Error != ErrTooLarge.Error() { t.Fatal(result) }
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 { t.Fatal("oversized set wrote a file", err) }
}

func TestConcurrentInstancesDoNotLoseUpdates(t *testing.T) {
	root := t.TempDir()
	const count = 20
	services := make([]*Service, count)
	for i := range services {
		service, err := New(root)
		if err != nil { t.Fatal(err) }
		services[i] = service
		defer service.Close()
	}
	var wg sync.WaitGroup
	for i, service := range services {
		wg.Add(1)
		go func(i int, service *Service) {
			defer wg.Done()
			if result := service.Set(map[string]any{fmt.Sprintf("key-%d", i): i}); !result.Success { t.Errorf("concurrent set: %v", result) }
		}(i, service)
	}
	wg.Wait()
	value, err := services[0].Get()
	if err != nil { t.Fatal(err) }
	object := value.(map[string]any)
	for i := range count {
		if object[fmt.Sprintf("key-%d", i)] != json.Number(fmt.Sprint(i)) { t.Fatalf("lost concurrent update %d: %#v", i, object) }
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" { t.Fatal("temporary leak", err) }
}
