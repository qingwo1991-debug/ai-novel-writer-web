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
	"reflect"
	"strings"
	"syscall"
	"unicode/utf8"
)

const modelDeleteJournalName = ".model-delete.journal"
const maxModelDeleteJournalBytes = 6*MaxConfigBytes + (64 << 10)

var ErrModelDeleteJournalInvalid = errors.New("LLM_MODEL_DELETE_JOURNAL_INVALID")
var ErrModelDeleteJournalConflict = errors.New("LLM_MODEL_DELETE_JOURNAL_CONFLICT")

// ModelDeletion is the original browser result projection. No model profile or
// credential is returned. A caller must still enforce session and project policy.
type ModelDeletion struct {
	DefaultModelID          any               `json:"defaultModelId"`
	DefaultEmbeddingModelID any               `json:"defaultEmbeddingModelId"`
	StageModelIDs           map[string]string `json:"stageModelIds"`
}

// modelDeleteJournal is a private write-ahead roll-forward record. Both target
// documents are fully pinned before either replacement. After a crash, the
// next Service operation accepts only the exact old or exact new bytes for
// each file and finishes the same update; unrelated edits fail closed.
type modelDeleteJournal struct {
	Version       int    `json:"version"`
	ModelID       string `json:"modelId"`
	ConfigMissing bool   `json:"configMissing"`
	ConfigOld     []byte `json:"configOld"`
	ConfigNew     []byte `json:"configNew"`
	ModelsMissing bool   `json:"modelsMissing"`
	ModelsOld     []byte `json:"modelsOld"`
	ModelsNew     []byte `json:"modelsNew"`
}

func validModelDeleteJournal(j modelDeleteJournal) bool {
	if j.Version != 1 || j.ModelID == "" || len(j.ModelID) > 512 || !utf8.ValidString(j.ModelID) || strings.ContainsRune(j.ModelID, 0) ||
		len(j.ConfigOld) > MaxConfigBytes || len(j.ConfigNew) > MaxConfigBytes ||
		len(j.ModelsOld) > MaxConfigBytes || len(j.ModelsNew) > MaxConfigBytes {
		return false
	}
	if j.ConfigMissing {
		if len(j.ConfigOld) != 0 || len(j.ConfigNew) != 0 {
			return false
		}
	} else {
		for _, data := range [][]byte{j.ConfigOld, j.ConfigNew} {
			value, err := decode(data)
			if err != nil {
				return false
			}
			if _, ok := value.(map[string]any); !ok {
				return false
			}
		}
	}
	if j.ModelsMissing && len(j.ModelsOld) != 0 {
		return false
	}
	if !j.ModelsMissing && !validModelDeleteBytes(j.ModelsOld) {
		return false
	}
	if !validModelDeleteBytes(j.ModelsNew) {
		return false
	}
	oldRaw := j.ModelsOld
	if j.ModelsMissing {
		oldRaw = []byte("[]")
	}
	oldValue, _ := decode(oldRaw)
	newValue, _ := decode(j.ModelsNew)
	filtered := make([]any, 0, len(oldValue.([]any)))
	for _, item := range oldValue.([]any) {
		if item.(map[string]any)["id"] != j.ModelID {
			filtered = append(filtered, item)
		}
	}
	return reflect.DeepEqual(filtered, newValue)
}

func validModelDeleteBytes(data []byte) bool {
	value, err := decode(data)
	if err != nil {
		return false
	}
	models, ok := value.([]any)
	if !ok {
		return false
	}
	seen := make(map[string]bool, len(models))
	for _, entry := range models {
		model, ok := entry.(map[string]any)
		if !ok {
			return false
		}
		id, ok := model["id"].(string)
		if !ok || id == "" || seen[id] {
			return false
		}
		if _, ok := model["apiKey"].(string); !ok {
			return false
		}
		seen[id] = true
	}
	return true
}

func readModelDeleteJournal(fd int) (*modelDeleteJournal, error) {
	journalFD, err := syscall.Openat(fd, modelDeleteJournalName, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrModelDeleteJournalInvalid
	}
	file := os.NewFile(uintptr(journalFD), "model-delete-journal")
	defer file.Close()
	var stat syscall.Stat_t
	if syscall.Fstat(journalFD, &stat) != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG ||
		stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0 ||
		stat.Size < 1 || stat.Size > maxModelDeleteJournalBytes {
		return nil, ErrModelDeleteJournalInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxModelDeleteJournalBytes+1))
	if err != nil || len(raw) > maxModelDeleteJournalBytes {
		return nil, ErrModelDeleteJournalInvalid
	}
	var after syscall.Stat_t
	if syscall.Fstat(journalFD, &after) != nil || after.Dev != stat.Dev || after.Ino != stat.Ino ||
		after.Size != stat.Size || int64(len(raw)) != stat.Size || after.Mtim != stat.Mtim {
		return nil, ErrModelDeleteJournalInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var journal modelDeleteJournal
	if decoder.Decode(&journal) != nil || decoder.Decode(new(any)) != io.EOF || !validModelDeleteJournal(journal) {
		return nil, ErrModelDeleteJournalInvalid
	}
	return &journal, nil
}

func writeModelDeleteJournal(fd int, journal modelDeleteJournal) error {
	if !validModelDeleteJournal(journal) {
		return ErrModelDeleteJournalInvalid
	}
	if existing, err := readModelDeleteJournal(fd); err != nil || existing != nil {
		if err != nil {
			return err
		}
		return ErrModelDeleteJournalConflict
	}
	raw, err := json.Marshal(journal)
	if err != nil || len(raw) > maxModelDeleteJournalBytes {
		return ErrModelDeleteJournalInvalid
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrIO
	}
	temporary := modelDeleteJournalName + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	tempFD, err := syscall.Openat(fd, temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return ErrIO
	}
	file := os.NewFile(uintptr(tempFD), "model-delete-journal-temporary")
	installed := false
	defer func() {
		_ = file.Close()
		if !installed {
			_ = syscall.Unlinkat(fd, temporary)
		}
	}()
	if written, err := file.Write(raw); err != nil || written != len(raw) {
		return ErrIO
	}
	if err := file.Sync(); err != nil {
		return ErrIO
	}
	if err := file.Close(); err != nil {
		return ErrIO
	}
	if err := syscall.Renameat(fd, temporary, fd, modelDeleteJournalName); err != nil {
		return ErrIO
	}
	installed = true
	if err := syscall.Fsync(fd); err != nil {
		return ErrIO
	}
	return nil
}

func matchesDeleteVersion(missing bool, raw []byte, wantedMissing bool, wanted []byte) bool {
	return missing == wantedMissing && (missing || bytes.Equal(raw, wanted))
}

// recoverModelDeletion is called under the directory flock before every
// Service action. It never guesses how to reconcile a third-party write.
func recoverModelDeletion(fd int) error {
	journal, err := readModelDeleteJournal(fd)
	if err != nil || journal == nil {
		return err
	}
	configRaw, configMissing, err := readConfig(fd)
	if err != nil {
		return err
	}
	modelsRaw, modelsMissing, err := readModelsRaw(fd)
	if err != nil {
		return err
	}
	oldConfig := matchesDeleteVersion(configMissing, configRaw, journal.ConfigMissing, journal.ConfigOld)
	newConfig := matchesDeleteVersion(configMissing, configRaw, journal.ConfigMissing, journal.ConfigNew)
	oldModels := matchesDeleteVersion(modelsMissing, modelsRaw, journal.ModelsMissing, journal.ModelsOld)
	newModels := matchesDeleteVersion(modelsMissing, modelsRaw, false, journal.ModelsNew)
	if (!oldConfig && !newConfig) || (!oldModels && !newModels) {
		return ErrModelDeleteJournalConflict
	}
	if !newConfig {
		if journal.ConfigMissing {
			return ErrModelDeleteJournalConflict
		}
		if err := atomicWrite(fd, journal.ConfigNew); err != nil {
			return err
		}
	}
	if !newModels {
		if err := writeModels(fd, journal.ModelsNew); err != nil {
			return err
		}
	}
	if err := syscall.Unlinkat(fd, modelDeleteJournalName); err != nil {
		return ErrIO
	}
	if err := syscall.Fsync(fd); err != nil {
		return ErrIO
	}
	return nil
}

func trimModelStage(value string) string {
	return strings.TrimFunc(value, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
			return true
		}
		return r >= '\u2000' && r <= '\u200a'
	})
}

func stageAssignments(value any) map[string]string {
	result := map[string]string{}
	object, ok := value.(map[string]any)
	if !ok {
		return result
	}
	for _, stage := range []string{"planning", "drafting", "review", "refinement", "finalization"} {
		text, ok := object[stage].(string)
		if ok && trimModelStage(text) != "" {
			result[stage] = trimModelStage(text)
		}
	}
	return result
}

// DeleteModelAndReferences ports llm:delete-model's reference-clearing order
// to one recoverable private cross-file transaction. It must be invoked only
// after a trusted runtime has admitted the global data root and excluded the
// legacy writer. A crash rolls forward from the sealed private journal.
func (s *Service) DeleteModelAndReferences(id string) (ModelDeletion, error) {
	var result ModelDeletion
	if id == "" || len(id) > 512 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		return result, ErrModelInvalid
	}
	err := s.locked(func(fd int) error {
		models, err := readModels(fd)
		if err != nil {
			return err
		}
		modelsOld, modelsMissing, err := readModelsRaw(fd)
		if err != nil {
			return err
		}
		configOld, configMissing, err := readConfig(fd)
		if err != nil {
			return err
		}
		var config map[string]any
		if configMissing {
			config = DefaultConfig()
		} else {
			value, decodeErr := decode(configOld)
			if decodeErr != nil {
				return ErrModelDeleteConfigCorrupt
			}
			var ok bool
			config, ok = value.(map[string]any)
			if !ok {
				return ErrModelDeleteConfigCorrupt
			}
		}
		changed := false
		if config["defaultModelId"] == id {
			config["defaultModelId"] = nil
			changed = true
		}
		if config["defaultEmbeddingModelId"] == id {
			config["defaultEmbeddingModelId"] = nil
			changed = true
		}
		stages := stageAssignments(config["stageModelIds"])
		if len(stages) > 0 {
			kept := make(map[string]string, len(stages))
			stageChanged := false
			for stage, assigned := range stages {
				if assigned == id {
					stageChanged = true
				} else {
					kept[stage] = assigned
				}
			}
			if stageChanged {
				config["stageModelIds"] = kept
				stages = kept
				changed = true
			}
		}
		result = ModelDeletion{DefaultModelID: config["defaultModelId"], DefaultEmbeddingModelID: config["defaultEmbeddingModelId"], StageModelIDs: stages}
		var modelsNext []ModelRecord
		for _, model := range models {
			if model["id"] != id {
				modelsNext = append(modelsNext, model)
			}
		}
		if modelsNext == nil {
			modelsNext = []ModelRecord{}
		}
		modelsNew, err := json.MarshalIndent(modelsNext, "", "  ")
		if err != nil || len(modelsNew) > MaxConfigBytes {
			return ErrTooLarge
		}
		configNew := configOld
		if changed {
			configNew, err = json.MarshalIndent(config, "", "  ")
			if err != nil || len(configNew) > MaxConfigBytes {
				return ErrTooLarge
			}
		}
		journal := modelDeleteJournal{Version: 1, ModelID: id, ConfigMissing: configMissing,
			ConfigOld: configOld, ConfigNew: configNew, ModelsMissing: modelsMissing, ModelsOld: modelsOld, ModelsNew: modelsNew}
		if err := writeModelDeleteJournal(fd, journal); err != nil {
			return err
		}
		return recoverModelDeletion(fd)
	})
	if err != nil {
		return ModelDeletion{}, err
	}
	return result, nil
}
