//go:build lance_native && cgo

// This executable only drives synthetic native interoperability on CI copies.
//
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/lancenative"
	"os"
	"path/filepath"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "LANCE_INTEROP_FAILED:", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("args")
	}
	root := os.Args[1]
	if err := lancenative.EnsureExistingCopy(root); err != nil {
		return err
	}
	ctx := context.Background()
	source := filepath.Join(root, "copy", "lancedb")
	if err := lancenative.EnsureExistingCopy(source); err != nil {
		return err
	}
	exported, err := lancenative.Export(ctx, source, filepath.Join(root, "native-ipc"))
	if err != nil {
		return err
	}
	if err := save(filepath.Join(root, "native-export.json"), exported); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(root, "contract.json"))
	if err != nil {
		return err
	}
	var original lancenative.Manifest
	if err := json.Unmarshal(raw, &original); err != nil {
		return err
	}
	restored, err := lancenative.Import(ctx, filepath.Join(root, "restored", "lancedb"), filepath.Join(root, "oracle-ipc"), original)
	if err != nil {
		return err
	}
	if err := save(filepath.Join(root, "native-import.json"), restored); err != nil {
		return err
	}
	reexported, err := lancenative.Export(ctx, filepath.Join(root, "restored", "lancedb"), filepath.Join(root, "native-restored-ipc"))
	if err != nil {
		return err
	}
	return save(filepath.Join(root, "native-restored-export.json"), reexported)
}
func save(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
