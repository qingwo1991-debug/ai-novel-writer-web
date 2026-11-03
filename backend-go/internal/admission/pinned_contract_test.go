package admission

import (
	"os"
	"reflect"
	"testing"
)

// The pinned bytes came from the original TypeScript registry in GitHub
// Actions run 41598056831 (source f3e485a05658dbf031d13e6e76a7b43ef3a9a2b3).
// CI also compares those bytes with a fresh export on every Go change.
func TestPinnedRecognizedSchemaContract(t *testing.T) {
	pinned, err := os.Open("recognized-schemas-v7.json")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	contract, err := ParseContract(pinned)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := PinnedSchemaContract()
	if err != nil || !reflect.DeepEqual(contract, embedded) {
		t.Fatalf("embedded contract differs from pinned bytes: %v", err)
	}
	if len(embedded) > 0 && len(embedded[0].Fingerprints) > 0 {
		embedded[0].Fingerprints[0] = "caller-changed"
		independent, err := PinnedSchemaContract()
		if err != nil || !reflect.DeepEqual(contract, independent) {
			t.Fatalf("caller changed embedded schema authority: %v", err)
		}
	}
	if len(contract) != CurrentSchemaVersion+1 {
		t.Fatalf("pinned contract covers %d versions, want %d", len(contract), CurrentSchemaVersion+1)
	}
	for version, entry := range contract {
		wantFingerprints := 2
		if version == 0 {
			wantFingerprints = 4
		}
		if entry.Version != version || len(entry.Fingerprints) != wantFingerprints {
			t.Fatalf("pinned version %d = %+v", version, entry)
		}
	}
	if generated := os.Getenv("GO_SQLITE_CONTRACT"); generated != "" {
		file, err := os.Open(generated)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		fresh, err := ParseContract(file)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(contract, fresh) {
			t.Fatal("pinned schema authority differs from original TypeScript registry")
		}
	}
}
