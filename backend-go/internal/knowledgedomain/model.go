// Package knowledgedomain validates already-copied, private synthetic native
// Lance snapshots. It has no storage access, migration authority, or logical
// hash/equivalence claim. Callers own raw-input limits and JSON UseNumber decoding.
package knowledgedomain

import "encoding/json"

type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Nullable bool `json:"nullable"`
	ListSize int `json:"listSize"`
	Metadata bool `json:"metadata"`
	Children []Field `json:"children"`
}

type Table struct {
	Name string `json:"name"`
	Version uint64 `json:"version"`
	Metadata bool `json:"metadata"`
	Schema []Field `json:"schema"`
	Records []map[string]any `json:"records"`
}

// Registry absent or JSON null represents no registry asset in the synthetic
// snapshot wire format. Non-null registry objects retain original unknown-field
// tolerance; this package is not a hostile JSON parser or a raw-file verifier.
type Snapshot struct {
	Tables []Table `json:"tables"`
	Registry json.RawMessage `json:"registry"`
}

// Every budget entry must be positive and no greater than its hard limit.
// Zero, negative, or above-hard entries are invalid. Limits are aggregate across
// the entire snapshot.
// Cells count each record value plus every vector element. Text bytes count
// table/schema names and type strings, record keys/string values (including
// malformed string vector elements), and raw registry bytes, not numeric lexemes.
// Numeric JSON lexemes have a separate 128-byte bound. Schema has at most the
// allowlisted fields and one scalar vector child; arbitrary nesting is forbidden.
type Budget struct {
	MaxTables int
	MaxRows int
	MaxCells int
	MaxTextBytes int
}

const (
	HardMaxTables = 32
	HardMaxRows = 10000
	HardMaxCells = 200000
	HardMaxTextBytes = 32 << 20
)

type Summary struct {
	TableCount int `json:"tableCount"`
	Documents int `json:"documents"`
	Chunks int `json:"chunks"`
	Spaces int `json:"spaces"`
	ActiveGeneration *int64 `json:"activeGeneration"`
	EmbeddingRows int `json:"embeddingRows"`
}
