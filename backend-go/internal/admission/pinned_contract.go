package admission

import (
	"bytes"
	_ "embed"
)

// pinnedRecognizedSchemas is generated from the original TypeScript migration
// registry and checked byte for byte against a fresh export in GitHub Actions.
//
//go:embed recognized-schemas-v7.json
var pinnedRecognizedSchemas []byte

// PinnedSchemaContract returns the compiled schema authority. Parsing on each
// call gives callers independent slices; a caller cannot mutate later reads.
// This does not itself authorize opening any project or running a migration.
func PinnedSchemaContract() (SchemaContract, error) {
	return ParseContract(bytes.NewReader(pinnedRecognizedSchemas))
}
