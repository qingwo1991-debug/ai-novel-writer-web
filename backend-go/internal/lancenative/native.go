//go:build lance_native && cgo

// Package lancenative is an isolated synthetic compatibility spike. It is not
// used by main, author admission or production repositories. Paths are trusted
// private CI copies; the caller must exclude all concurrent filesystem writers.
package lancenative

/*
#cgo CFLAGS: -I${SRCDIR}/../../native-lance/include
#include <stdlib.h>
#include "novel_lance.h"
*/
import "C"
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var ErrInput = errors.New("LANCE_NATIVE_INPUT_INVALID")
var ErrNative = errors.New("LANCE_NATIVE_OPERATION_FAILED")

const maxManifest = 1024 * 1024
const maxDomain = 64 * 1024 * 1024

// Table describes a pinned current snapshot. Import creates a fresh history;
// it does not claim to recreate historical versions or migrate author assets.
type Table struct {
	Name    string `json:"name"`
	Version uint64 `json:"version"`
	Rows    uint64 `json:"rows"`
	File    string `json:"file"`
}
type Manifest struct {
	Tables []Table `json:"tables"`
}

func validPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\\")
}
func pathBytes(path string) (unsafe.Pointer, C.size_t, func(), error) {
	if !validPath(path) {
		return nil, 0, nil, ErrInput
	}
	value := C.CBytes([]byte(path))
	return value, C.size_t(len(path)), func() { C.free(value) }, nil
}
func decode(status C.int32_t, result *C.NvBytes, diagnostic *C.NvBytes) (Manifest, error) {
	defer C.nv_lance_free(result)
	defer C.nv_lance_free(diagnostic)
	if status != 0 {
		return Manifest{}, fmt.Errorf("%w (status %d)", ErrNative, int32(status))
	}
	if result.len == 0 || result.len > maxManifest || result.ptr == nil {
		return Manifest{}, ErrNative
	}
	raw := C.GoBytes(unsafe.Pointer(result.ptr), C.int(result.len))
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.Tables == nil {
		return Manifest{}, ErrNative
	}
	return manifest, nil
}

// Native calls are synchronous and cannot be interrupted through Go context.
// Cancellation is checked before dispatch and after native work has completed;
// no goroutine returns early or destroys native resources still in use.
func Export(ctx context.Context, path, out string) (Manifest, error) {
	if ctx == nil {
		return Manifest{}, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if path == out {
		return Manifest{}, ErrInput
	}
	p, plen, free, err := pathBytes(path)
	if err != nil {
		return Manifest{}, err
	}
	defer free()
	o, olen, freeOut, err := pathBytes(out)
	if err != nil {
		return Manifest{}, err
	}
	defer freeOut()
	var result, diagnostic C.NvBytes
	status := C.nv_lance_export((*C.uint8_t)(p), plen, (*C.uint8_t)(o), olen, &result, &diagnostic)
	manifest, err := decode(status, &result, &diagnostic)
	if err != nil {
		return Manifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}
func Import(ctx context.Context, staging, input string, manifest Manifest) (Manifest, error) {
	if ctx == nil {
		return Manifest{}, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if staging == input {
		return Manifest{}, ErrInput
	}
	s, slen, freeStage, err := pathBytes(staging)
	if err != nil {
		return Manifest{}, err
	}
	defer freeStage()
	i, ilen, freeInput, err := pathBytes(input)
	if err != nil {
		return Manifest{}, err
	}
	defer freeInput()
	raw, err := json.Marshal(manifest)
	if err != nil || len(raw) > maxManifest {
		return Manifest{}, ErrInput
	}
	body := C.CBytes(raw)
	defer C.free(body)
	var result, diagnostic C.NvBytes
	status := C.nv_lance_import((*C.uint8_t)(s), slen, (*C.uint8_t)(i), ilen, (*C.uint8_t)(body), C.size_t(len(raw)), &result, &diagnostic)
	restored, err := decode(status, &result, &diagnostic)
	if err != nil {
		return Manifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	return restored, nil
}

// ReadDomain returns only the fixed export sidecar, never a caller-supplied
// filename or an author's source file. It deliberately leaves domain decoding
// and the stricter accepted schema to its caller. Like Export, this requires a
// private trusted CI copy with no concurrent same-user filesystem writers.
// Cancellation is checked before and after synchronous file work, not mid-read.
// Returned errors contain no source paths, native diagnostics or row values.
func ReadDomain(ctx context.Context, out string) ([]byte, error) {
	if ctx == nil {
		return nil, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := EnsureExistingCopy(out); err != nil {
		return nil, ErrInput
	}
	root, err := os.Lstat(out)
	if err != nil || !root.IsDir() || root.Mode().Perm()&0077 != 0 {
		return nil, ErrInput
	}
	rootStat, ok := root.Sys().(*syscall.Stat_t)
	if !ok || rootStat.Uid != uint32(os.Geteuid()) {
		return nil, ErrInput
	}
	path := filepath.Join(out, "domain.json")
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w (status 2)", ErrNative)
	}
	beforeStat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || beforeStat.Nlink != 1 || beforeStat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("%w (status 2)", ErrNative)
	}
	if before.Size() > maxDomain {
		return nil, fmt.Errorf("%w (status 3)", ErrNative)
	}
	// O_NONBLOCK avoids blocking if a prohibited concurrent replacement is a
	// FIFO; fstat still requires the opened object to be a private regular file.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%w (status 2)", ErrNative)
	}
	file := os.NewFile(uintptr(fd), "domain.json")
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w (status 6)", ErrNative)
	}
	openedStat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 || openedStat.Nlink != 1 || openedStat.Uid != uint32(os.Geteuid()) || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%w (status 2)", ErrNative)
	}
	if opened.Size() > maxDomain {
		return nil, fmt.Errorf("%w (status 3)", ErrNative)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxDomain+1))
	if err != nil {
		return nil, fmt.Errorf("%w (status 6)", ErrNative)
	}
	if len(raw) > maxDomain {
		return nil, fmt.Errorf("%w (status 3)", ErrNative)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

// EnsureExistingCopy validates a physical directory before invoking an API that
// could otherwise create absent database directories. Rust repeats this guard.
func EnsureExistingCopy(path string) error {
	if !validPath(path) {
		return ErrInput
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrInput
		}
		if current == string(filepath.Separator) {
			return nil
		}
	}
}
