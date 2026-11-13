package admission

import (
	"encoding/json"
	"io"
	"sort"
)

// ParseContract reads the trusted CI/application bridge export, not a file inside
// an untrusted project. No contract is inferred from the database under test.
// The fixtures list belongs to the bridge's test harness and is not admission
// authority. Unknown fields fail closed to avoid silently changing the format.
func ParseContract(reader io.Reader) (SchemaContract, error) {
	var document struct {
		FormatVersion  int `json:"formatVersion"`
		CurrentVersion int `json:"currentVersion"`
		Recognized     []struct {
			Version     int    `json:"version"`
			Fingerprint string `json:"fingerprint"`
		} `json:"recognized"`
		Fixtures []struct {
			Name        string `json:"name"`
			File        string `json:"file"`
			Version     int    `json:"version"`
			Fingerprint string `json:"fingerprint"`
		} `json:"fixtures"`
	}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, failure(CodeInvalidContract, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, failure(CodeInvalidContract, err)
	}
	if document.FormatVersion != 1 || document.CurrentVersion != CurrentSchemaVersion {
		return nil, failure(CodeInvalidContract, nil)
	}
	grouped := make(map[int][]string)
	for _, identity := range document.Recognized {
		grouped[identity.Version] = append(grouped[identity.Version], identity.Fingerprint)
	}
	versions := make([]int, 0, len(grouped))
	for version := range grouped {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	contract := make(SchemaContract, 0, len(versions))
	for _, version := range versions {
		contract = append(contract, VersionContract{Version: version, Fingerprints: grouped[version]})
	}
	if err := validateContract(contract); err != nil {
		return nil, err
	}
	return contract, nil
}
