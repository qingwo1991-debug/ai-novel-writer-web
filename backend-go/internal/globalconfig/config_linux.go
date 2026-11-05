//go:build linux

// Package globalconfig ports global config and startup controller behavior. The
// root passed to New is a backend-owned, explicitly admitted data root, never a
// browser path. This package does not implement migration or infer its authority.
package globalconfig

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const MaxConfigBytes = 4 << 20

var (
	ErrUnsafePath = errors.New("GLOBAL_CONFIG_UNSAFE_PATH")
	ErrUnsafeFile = errors.New("GLOBAL_CONFIG_UNSAFE_FILE")
	ErrTooLarge = errors.New("GLOBAL_CONFIG_TOO_LARGE")
	ErrIO = errors.New("GLOBAL_CONFIG_IO_FAILED")
	ErrClosed = errors.New("GLOBAL_CONFIG_CLOSED")
	ErrInvalidPatch = errors.New("GLOBAL_CONFIG_INVALID_PATCH")
)

const damagedEnglish = "The global config file is damaged, so it was not overwritten. Repair or delete config.json in the app data folder, then try again."
const damagedChinese = "全局配置文件已损坏，已拒绝覆盖。请修复或删除应用数据目录里的 config.json 后重试。"

// DefaultConfig returns a fresh exact copy of DEFAULT_GLOBAL_CONFIG from
// electron/utils/config-utils.ts. Notably models and stageModelIds are absent.
func DefaultConfig() map[string]any {
	return map[string]any{
		"theme": "dark", "defaultModelId": nil,
		"autoOpenNextChapterAfterFinalize": false,
		"editorFontSize": 16, "editorFontFamily": "Noto Serif SC", "autoSaveInterval": 30,
		"proxy": map[string]any{"enabled": false, "type": "http", "host": "", "port": 7890},
	}
}

type Result struct {
	Success bool `json:"success"`
	Error string `json:"error,omitempty"`
}

// Service pins a no-follow descriptor to its explicit root. Close it at backend
// shutdown. A directory flock serializes independent Service instances/processes
// sharing the root; no lock/config file is created by New or Get.
type Service struct {
	mu sync.Mutex
	root *os.File
}

func New(rootTrustedExplicit string) (*Service, error) {
	root := rootTrustedExplicit
	if len(root) == 0 || len(root) > 4096 || strings.ContainsRune(root, '\x00') || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return nil, ErrUnsafePath
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil { return nil, ErrIO }
	for _, part := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 { syscall.Close(fd); return nil, ErrUnsafePath }
		next, openErr := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		syscall.Close(fd)
		if openErr != nil { return nil, ErrUnsafePath }
		fd = next
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil { syscall.Close(fd); return nil, ErrIO }
	// The final root must be owned and not writable by another principal. Public
	// ancestors such as /tmp are fine: all components were pinned without links.
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode & 0022 != 0 { syscall.Close(fd); return nil, ErrUnsafePath }
	return &Service{root: os.NewFile(uintptr(fd), "global-config-root")}, nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil { return nil }
	err := s.root.Close()
	s.root = nil
	if err != nil { return ErrIO }
	return nil
}

func (s *Service) locked(action func(int) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil { return ErrClosed }
	fd := int(s.root.Fd())
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX)
		if err == syscall.EINTR { continue }
		if err != nil { return ErrIO }
		break
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	if err := recoverModelDeletion(fd); err != nil { return err }
	return action(fd)
}

func decode(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil { return nil, err }
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF { return nil, errors.New("invalid trailing JSON") }
	return value, nil
}

func readConfig(fd int) (data []byte, missing bool, err error) {
	fileFD, err := syscall.Openat(fd, "config.json", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err == syscall.ENOENT { return nil, true, nil }
	if err == syscall.ELOOP { return nil, false, ErrUnsafeFile }
	if err != nil { return nil, false, ErrIO }
	file := os.NewFile(uintptr(fileFD), "global-config")
	defer file.Close()
	var stat syscall.Stat_t
	if syscall.Fstat(fileFD, &stat) != nil { return nil, false, ErrIO }
	if stat.Mode & syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode & 0022 != 0 { return nil, false, ErrUnsafeFile }
	if stat.Size < 0 || stat.Size > MaxConfigBytes { return nil, false, ErrTooLarge }
	data, err = io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil { return nil, false, ErrIO }
	if len(data) > MaxConfigBytes { return nil, false, ErrTooLarge }
	return data, false, nil
}

// Get intentionally returns any valid JSON value, not a defaults merge. This is
// config:get's internal behavior; browser transports MUST project safe fields
// rather than expose arbitrary saved fields/models/credentials to callers.
func (s *Service) Get() (any, error) {
	var value any
	err := s.locked(func(fd int) error {
		data, missing, err := readConfig(fd)
		if err != nil { return err } // safety refusals are not silently defaulted
		if missing { value = DefaultConfig(); return nil }
		value, err = decode(data)
		if err != nil { value = DefaultConfig() }
		return nil
	})
	return value, err
}

// ModelOriginsKey is the optional top-level config.json field that declares the
// exact HTTP(S) origins a server-side model executor may dial. The shipped
// renderer never writes it: it exists so an author or operator can pin an
// explicit allowlist in the same committed document the model routing reads
// instead of a synthetic command-line flag.
const ModelOriginsKey = "modelOrigins"

// ModelEgressOrigins returns the raw optional modelOrigins value of config.json
// and whether the committed document carries it. The document is the one
// llm:get-default-model reads, but the read is deliberately stricter than that
// tolerant routing projection: a config this call cannot parse is ErrConfigCorrupt
// instead of an empty allowlist, so a damaged document refuses the caller rather
// than looking like "no origins". It never writes and never interprets the value:
// the production lane owns the exact origin validation.
func (s *Service) ModelEgressOrigins() (any, bool, error) {
	var value any
	present := false
	err := s.locked(func(fd int) error {
		data, missing, readErr := readConfig(fd)
		if readErr != nil {
			return readErr
		}
		if missing {
			return nil
		}
		decoded, decodeErr := decode(data)
		if decodeErr != nil {
			return ErrConfigCorrupt
		}
		document, ok := decoded.(map[string]any)
		if !ok {
			return ErrConfigCorrupt
		}
		value, present = document[ModelOriginsKey]
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return value, present, nil
}

func (s *Service) Set(patch map[string]any) Result { return s.SetForLocale(patch, "en-US") }

// SetForLocale's locale is backend appHost locale, never a request authority.
// Writes shallow-merge and preserve stageModelIds from the existing document,
// even when the submitted snapshot supplies stale or malicious assignments.
func (s *Service) SetForLocale(patch map[string]any, systemLocale string) Result {
	var result Result
	err := s.locked(func(fd int) error {
		data, missing, err := readConfig(fd)
		if err != nil { return err }
		var existing map[string]any
		if missing { existing = DefaultConfig() } else {
			value, parseErr := decode(data)
			existing, _ = value.(map[string]any)
			if parseErr != nil || existing == nil {
				message := damagedEnglish
				if strings.HasPrefix(strings.ToLower(systemLocale), "zh") { message = damagedChinese }
				// Original mainText reads config.locale after update's refusal;
				// a literal null therefore returns this caught native JS error.
				if parseErr == nil && value == nil { message = "TypeError: Cannot read properties of null (reading 'locale')" }
				result = Result{Error: message}
				return nil
			}
		}
		encodedPatch, err := json.Marshal(patch)
		if err != nil { return ErrInvalidPatch }
		if len(encodedPatch) > MaxConfigBytes { return ErrTooLarge }
		cloned, err := decode(encodedPatch)
		if err != nil { return ErrInvalidPatch }
		copyPatch, ok := cloned.(map[string]any)
		if !ok && patch != nil { return ErrInvalidPatch }
		for key, value := range copyPatch { if key != "stageModelIds" { existing[key] = value } }
		var output bytes.Buffer
		encoder := json.NewEncoder(&output)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(existing); err != nil { return ErrInvalidPatch }
		bytes := bytes.TrimSuffix(output.Bytes(), []byte("\n"))
		if len(bytes) > MaxConfigBytes { return ErrTooLarge }
		if err := atomicWrite(fd, bytes); err != nil { return err }
		result = Result{Success: true}
		return nil
	})
	if err != nil { return Result{Error: err.Error()} }
	return result
}

func atomicWrite(fd int, data []byte) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil { return ErrIO }
	name := ".config.json." + hex.EncodeToString(nonce[:]) + ".tmp"
	tempFD, err := syscall.Openat(fd, name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil { return ErrIO }
	file := os.NewFile(uintptr(tempFD), "global-config-owned-temporary")
	installed := false
	defer func() { file.Close(); if !installed { syscall.Unlinkat(fd, name) } }()
	var stat syscall.Stat_t
	if syscall.Fstat(tempFD, &stat) != nil || stat.Mode & syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) { return ErrUnsafeFile }
	if err := file.Chmod(0600); err != nil { return ErrIO }
	if _, err := file.Write(data); err != nil { return ErrIO }
	if err := file.Sync(); err != nil { return ErrIO }
	if err := file.Close(); err != nil { return ErrIO }
	// A final no-follow/type check refuses a destination changed to a link or
	// special file while composing the update. Rename never follows its target.
	if _, _, err := readConfig(fd); err != nil { return err }
	if err := syscall.Renameat(fd, name, fd, "config.json"); err != nil { return ErrIO }
	installed = true
	if err := syscall.Fsync(fd); err != nil { return ErrIO }
	return nil
}
