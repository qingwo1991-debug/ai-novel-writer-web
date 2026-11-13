package globalconfig

import (
	"encoding/json"
	"math"
	"strconv"
	"sync"
)

const AppearanceStorageKey = "ai-novel-writer-appearance-v3"

const (
	BlockedGlobalData             = "GLOBAL_DATA_BLOCKED"
	BlockedModelCredentialsDiffer = "GLOBAL_MODEL_CREDENTIALS_DIFFER"
)

type MigrationNotice struct {
	LegacySourceIgnored   bool  `json:"legacySourceIgnored"`
	PreservedUnknownCount int64 `json:"preservedUnknownCount"`
}

// StartupCapture is supplied ONLY by trusted backend startup/migration code.
// It is not an operation payload. A root, directory, environment variable or
// caller-provided generation is not sufficient proof of completed migration.
// Until that authority exists, leave Generation empty: State returns pending.
// Browser authentication/origin/CSRF/main-frame equivalence belongs to transport.
type StartupCapture struct {
	Generation      string
	BlockedCode     string
	MigrationNotice *MigrationNotice
}

type StartupState struct {
	State                   string           `json:"state"`
	Code                    string           `json:"code,omitempty"`
	GlobalGeneration        string           `json:"globalGeneration,omitempty"`
	AppearanceRevisionFloor *int64           `json:"appearanceRevisionFloor,omitempty"`
	MigrationNotice         *MigrationNotice `json:"migrationNotice,omitempty"`
}

type Startup struct {
	mu           sync.Mutex
	capture      StartupCapture
	acknowledged map[string]int64
}

// NewStartup captures its input, including a copy of the optional notice. There
// is deliberately no per-request capture setter or public generation mutator.
func NewStartup(capture StartupCapture) *Startup {
	if capture.MigrationNotice != nil {
		notice := *capture.MigrationNotice
		capture.MigrationNotice = &notice
	}
	return &Startup{capture: capture, acknowledged: make(map[string]int64)}
}

func (s *Startup) State(caller string, trusted bool) StartupState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !trusted || caller == "" {
		return StartupState{State: "blocked"}
	}
	if s.capture.BlockedCode != "" {
		return StartupState{State: "blocked", Code: s.capture.BlockedCode}
	}
	if s.capture.Generation == "" {
		return StartupState{State: "pending"}
	}
	state := StartupState{State: "ready", GlobalGeneration: s.capture.Generation}
	if revision, ok := s.acknowledged[caller]; ok {
		state.AppearanceRevisionFloor = &revision
	}
	if s.capture.MigrationNotice != nil {
		notice := *s.capture.MigrationNotice
		state.MigrationNotice = &notice
	}
	return state
}

func safeRevision(value any) (int64, bool) {
	var number float64
	switch value := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	case float64:
		number = value
	case float32:
		number = float64(value)
	case int:
		number = float64(value)
	case int64:
		if value < 1 || value > 9007199254740991 {
			return 0, false
		}
		return value, true
	case int32:
		number = float64(value)
	case uint64:
		if value < 1 || value > 9007199254740991 {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 1 || number > 9007199254740991 || math.Trunc(number) != number {
		return 0, false
	}
	return int64(number), true
}

func (s *Startup) AppearanceAck(caller string, trusted bool, input any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !trusted || caller == "" || s.capture.BlockedCode != "" {
		return false
	}
	ack, ok := input.(map[string]any)
	if !ok || len(ack) != 3 {
		return false
	}
	if ack["storageKey"] != AppearanceStorageKey {
		return false
	}
	revision, ok := safeRevision(ack["profileRevision"])
	if !ok {
		return false
	}
	generation, ok := ack["globalGeneration"].(string)
	if !ok || s.capture.Generation == "" || generation != s.capture.Generation {
		return false
	}
	if revision < s.acknowledged[caller] {
		return false
	}
	s.acknowledged[caller] = revision
	return true
}

// ForgetCaller is called by trusted transport on permanent client disposal.
// Reload must retain the same scope/floor; a different authenticated session
// receives a different backend-assigned scope and cannot acknowledge for it.
func (s *Startup) ForgetCaller(caller string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.acknowledged, caller)
}
