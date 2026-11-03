//go:build linux

package globalconfig

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
)

const modelFile = "models.json"

var ErrModelCorrupt = errors.New("LLM_MODEL_CONFIG_CORRUPT")
var ErrModelInvalid = errors.New("LLM_MODEL_INVALID")
var ErrCredentialEndpointChanged = errors.New("CREDENTIAL_ENDPOINT_CHANGED")
var ErrModelReferenced = errors.New("LLM_MODEL_REFERENCED")
var ErrModelNotFound = errors.New("LLM_MODEL_NOT_FOUND")
var ErrModelDeleteConfigCorrupt = errors.New("LLM_GLOBAL_CONFIG_CORRUPT")
var ErrModelDeleteConfigChanged = errors.New("LLM_GLOBAL_CONFIG_CHANGED")

// ModelRecord is a private persisted profile. apiKey is plaintext in the
// original models.json; callers must keep records inside the server and use the
// public response sanitizer before returning any profile to a browser.
type ModelRecord map[string]any

func readModelsRaw(fd int) ([]byte, bool, error) {
	fileFD, err := syscall.Openat(fd, modelFile, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err == syscall.ENOENT {
		return nil, true, nil
	}
	if err == syscall.ELOOP {
		return nil, false, ErrUnsafeFile
	}
	if err != nil {
		return nil, false, ErrIO
	}
	file := os.NewFile(uintptr(fileFD), "global-model-config")
	defer file.Close()
	var stat syscall.Stat_t
	if syscall.Fstat(fileFD, &stat) != nil {
		return nil, false, ErrIO
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0 {
		return nil, false, ErrUnsafeFile
	}
	if stat.Size < 0 || stat.Size > MaxConfigBytes {
		return nil, false, ErrTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil {
		return nil, false, ErrIO
	}
	if len(raw) > MaxConfigBytes {
		return nil, false, ErrTooLarge
	}
	return raw, false, nil
}

func readModels(fd int) ([]ModelRecord, error) {
	raw, missing, err := readModelsRaw(fd)
	if err != nil {
		return nil, err
	}
	if missing {
		return []ModelRecord{}, nil
	}
	value, err := decode(raw)
	if err != nil {
		return nil, ErrModelCorrupt
	}
	array, ok := value.([]any)
	if !ok {
		return nil, ErrModelCorrupt
	}
	models := make([]ModelRecord, 0, len(array))
	seen := make(map[string]bool)
	for _, item := range array {
		model, ok := item.(map[string]any)
		if !ok {
			return nil, ErrModelCorrupt
		}
		id, ok := model["id"].(string)
		if !ok || id == "" || seen[id] {
			return nil, ErrModelCorrupt
		}
		_, ok = model["apiKey"].(string)
		if !ok {
			return nil, ErrModelCorrupt
		}
		seen[id] = true
		models = append(models, model)
	}
	return models, nil
}

// ListPrivateModels returns detached private records. It must never be wired
// directly to a public RPC operation because models.json contains API keys.
func (s *Service) ListPrivateModels() ([]ModelRecord, error) {
	var models []ModelRecord
	err := s.locked(func(fd int) error { var err error; models, err = readModels(fd); return err })
	return models, err
}

func modelEndpoint(model ModelRecord) (provider, protocol, base string, ok bool) {
	provider, a := model["provider"].(string)
	protocol, b := model["protocol"].(string)
	base, c := model["baseUrl"].(string)
	return provider, protocol, strings.TrimRight(base, "/"), a && b && c
}

func detachedModel(candidate ModelRecord) (ModelRecord, error) {
	raw, err := json.Marshal(candidate)
	if err != nil || len(raw) > MaxConfigBytes {
		return nil, ErrModelInvalid
	}
	value, err := decode(raw)
	if err != nil {
		return nil, ErrModelInvalid
	}
	model, ok := value.(map[string]any)
	if !ok {
		return nil, ErrModelInvalid
	}
	id, ok := model["id"].(string)
	if !ok || id == "" {
		return nil, ErrModelInvalid
	}
	if _, _, _, ok := modelEndpoint(model); !ok {
		return nil, ErrModelInvalid
	}
	if _, ok := model["apiKey"].(string); !ok {
		return nil, ErrModelInvalid
	}
	delete(model, "hasCredential")
	delete(model, "savedModelId")
	return model, nil
}

// SaveModel preserves a stored credential only when the caller submits an
// empty key for the same id/provider/protocol/endpoint. Endpoint allowlisting,
// model capability validation, and authenticated caller checks belong to the
// trusted adapter that invokes this method. A failed read never overwrites a
// damaged model file or drops a key.
func (s *Service) SaveModel(candidate ModelRecord) error {
	model, err := detachedModel(candidate)
	if err != nil {
		return err
	}
	return s.locked(func(fd int) error {
		models, err := readModels(fd)
		if err != nil {
			return err
		}
		id := model["id"].(string)
		key := model["apiKey"].(string)
		index := -1
		for i, existing := range models {
			if existing["id"] == id {
				index = i
				break
			}
		}
		if index >= 0 && key == "" {
			previous := models[index]
			if previousKey := previous["apiKey"].(string); previousKey != "" {
				p1, q1, b1, oldOK := modelEndpoint(previous)
				p2, q2, b2, newOK := modelEndpoint(model)
				if !oldOK || !newOK || p1 != p2 || q1 != q2 || b1 != b2 {
					return ErrCredentialEndpointChanged
				}
				model["apiKey"] = previousKey
			}
		}
		if index >= 0 {
			models[index] = model
		} else {
			models = append(models, model)
		}
		var output bytes.Buffer
		encoder := json.NewEncoder(&output)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(models); err != nil {
			return ErrModelInvalid
		}
		encoded := bytes.TrimSuffix(output.Bytes(), []byte("\n"))
		if len(encoded) > MaxConfigBytes {
			return ErrTooLarge
		}
		return writeModels(fd, encoded)
	})
}

// DeleteUnreferencedModel removes only a model with no references anywhere in
// the private global config. A referenced model requires a journaled cross-file
// transaction before it can be deleted safely; this method deliberately does
// not implement the public llm:delete-model operation. The recursive check is
// conservative about unknown future config fields so a new reference cannot
// silently become dangling. Caller admission and exclusion of legacy writers
// remain the responsibility of the trusted runtime.
func (s *Service) DeleteUnreferencedModel(id string) error {
	if id == "" || len(id) > 512 || strings.ContainsRune(id, 0) {
		return ErrModelInvalid
	}
	return s.locked(func(fd int) error {
		models, err := readModels(fd)
		if err != nil {
			return err
		}
		index := -1
		for i, model := range models {
			if model["id"] == id {
				index = i
				break
			}
		}
		if index < 0 {
			return ErrModelNotFound
		}
		configBefore, missing, err := readConfig(fd)
		if err != nil {
			return err
		}
		if !missing {
			var value any
			value, err = decode(configBefore)
			if err != nil {
				return ErrModelDeleteConfigCorrupt
			}
			if _, ok := value.(map[string]any); !ok {
				return ErrModelDeleteConfigCorrupt
			}
			if referencesModel(value, id) {
				return ErrModelReferenced
			}
		}
		// Recheck the whole config document immediately before replacing models.
		// The root flock excludes other Go services; this also detects accidental
		// writes from a process that does not participate in that flock.
		configNow, missingNow, err := readConfig(fd)
		if err != nil {
			return err
		}
		if missing != missingNow || !bytes.Equal(configBefore, configNow) {
			return ErrModelDeleteConfigChanged
		}
		next := append(append([]ModelRecord(nil), models[:index]...), models[index+1:]...)
		var output bytes.Buffer
		encoder := json.NewEncoder(&output)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(next); err != nil {
			return ErrModelInvalid
		}
		encoded := bytes.TrimSuffix(output.Bytes(), []byte("\n"))
		if len(encoded) > MaxConfigBytes {
			return ErrTooLarge
		}
		return writeModels(fd, encoded)
	})
}

func referencesModel(value any, id string) bool {
	switch current := value.(type) {
	case string:
		return current == id
	case map[string]any:
		for _, child := range current {
			if referencesModel(child, id) {
				return true
			}
		}
	case []any:
		for _, child := range current {
			if referencesModel(child, id) {
				return true
			}
		}
	}
	return false
}

func writeModels(fd int, data []byte) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrIO
	}
	temporary := ".models.json." + hex.EncodeToString(nonce[:]) + ".tmp"
	tempFD, err := syscall.Openat(fd, temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return ErrIO
	}
	file := os.NewFile(uintptr(tempFD), "global-model-owned-temporary")
	installed := false
	defer func() {
		file.Close()
		if !installed {
			syscall.Unlinkat(fd, temporary)
		}
	}()
	if written, err := file.Write(data); err != nil || written != len(data) {
		return ErrIO
	}
	if err := file.Sync(); err != nil {
		return ErrIO
	}
	if err := file.Close(); err != nil {
		return ErrIO
	}
	// The existing destination is rechecked just before replacement. Our caller
	// holds the root directory flock and the pinned descriptor throughout.
	if _, err := readModels(fd); err != nil {
		return err
	}
	if err := syscall.Renameat(fd, temporary, fd, modelFile); err != nil {
		return ErrIO
	}
	installed = true
	if err := syscall.Fsync(fd); err != nil {
		return ErrIO
	}
	return nil
}
