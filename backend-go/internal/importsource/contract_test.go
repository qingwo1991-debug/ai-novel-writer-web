package importsource

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

type originalSource struct {
	LocationAliasDigest string `json:"locationAliasDigest"`
	FileAliasDigest     string `json:"fileAliasDigest"`
	DisplayName         string `json:"displayName"`
	MediaType           string `json:"mediaType"`
	Size                int64  `json:"size"`
}
type originalIdentity struct {
	CanonicalLocation string `json:"canonicalLocation"`
	FileIdentity      string `json:"fileIdentity"`
}
type originalContract struct {
	Format          int
	Platform        string
	KeyHex          string
	OriginalModules []string
	ParseCases      []struct {
		Name, Raw string
		Identity  originalIdentity
		Sources   []originalSource
		Chapters  []Chapter
		Summary   Summary
	}
	EPUBCases []struct {
		Name, Raw, Error string
		Result           []map[string]string
	}
	IdentityCases []struct {
		Name    string
		Sources []originalIdentity
		Purpose Purpose
		Result  []originalSource
		Error   string
	}
	ResolverCases []struct {
		Name    string
		Sources []originalIdentity
		Purpose Purpose
		Encoded []originalSource
		Result  ResolvedIdentity
		Aliases []struct {
			AliasDigest string `json:"alias_digest"`
			AliasKind   string `json:"alias_kind"`
			SourceID    string `json:"source_id"`
		}
	}
	CountCases []struct {
		Text   string
		Result int
	}
	ActualStoreChecks []string
}

func TestActualTypeScriptDifferential(t *testing.T) {
	filename := os.Getenv("GO_IMPORT_SOURCE_CONTRACT")
	if filename == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("GO_IMPORT_SOURCE_CONTRACT required on GitHub")
		}
		t.Skip("original TypeScript differential runs on GitHub only")
	}
	base := os.Getenv("RUNNER_TEMP")
	if base == "" || !filepath.IsAbs(base) || filename != filepath.Join(base, "novel-go-import-source", "contract.json") {
		t.Fatal("unexpected contract location")
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var fixture originalContract
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	expectedPlatform := runtime.GOOS
	if expectedPlatform == "windows" {
		expectedPlatform = "win32"
	}
	if fixture.Format != 1 || fixture.Platform != expectedPlatform || len(fixture.ParseCases) < 9 || len(fixture.EPUBCases) < 6 || len(fixture.IdentityCases) < 5 || len(fixture.ResolverCases) < 6 || len(fixture.CountCases) < 10 || len(fixture.ActualStoreChecks) != 5 {
		t.Fatal("incomplete actual contract")
	}
	for _, required := range []string{"electron/controllers/import-controller.ts", "electron/services/import-inspection-store.ts", "electron/repositories/import-source-identity-repository.ts", "electron/services/epub-extraction-service.ts", "src/shared/draft-units.ts"} {
		found := false
		for _, module := range fixture.OriginalModules {
			if module == required {
				found = true
			}
		}
		if !found {
			t.Fatalf("original module not executed: %s", required)
		}
	}
	key, err := hex.DecodeString(fixture.KeyHex)
	if err != nil || len(key) != 32 {
		t.Fatal("invalid synthetic key")
	}
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE import_source_aliases(alias_digest TEXT PRIMARY KEY,alias_kind TEXT NOT NULL,source_id TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	idMap := map[string]string{}
	for _, tc := range fixture.ResolverCases {
		t.Run("resolver/"+tc.Name, func(t *testing.T) {
			encoded := make([]EncodedIdentity, 0, len(tc.Encoded))
			for _, s := range tc.Encoded {
				encoded = append(encoded, EncodedIdentity{LocationAliasDigest: s.LocationAliasDigest, FileAliasDigest: s.FileAliasDigest})
			}
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := ResolveEncodedSources(context.Background(), tx, encoded, tc.Purpose, key)
			if err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if len(result.SourceIDs) != len(tc.Result.SourceIDs) {
				t.Fatal("source count")
			}
			mappedIDs := make([]string, len(result.SourceIDs))
			for i, id := range result.SourceIDs {
				expected := tc.Result.SourceIDs[i]
				if previous, ok := idMap[id]; ok && previous != expected {
					t.Fatalf("rename/replacement identity changed %s != %s", previous, expected)
				}
				for native, original := range idMap {
					if original == expected && native != id {
						t.Fatal("lost stable alias identity")
					}
				}
				idMap[id] = expected
				mappedIDs[i] = expected
			}
			expectedFingerprint := sourceFingerprint(mappedIDs, tc.Purpose, key)
			if expectedFingerprint != tc.Result.SourceFingerprint || result.SourceFingerprint != sourceFingerprint(result.SourceIDs, tc.Purpose, key) {
				t.Fatal("collection fingerprint differs")
			}
			for i, id := range result.SourceIDs {
				if result.SourceFingerprints[i] != sourceFingerprint([]string{id}, tc.Purpose, key) || tc.Result.SourceFingerprints[i] != sourceFingerprint([]string{mappedIDs[i]}, tc.Purpose, key) {
					t.Fatal("individual fingerprint differs")
				}
			}
			rows, err := db.Query("SELECT alias_digest,alias_kind,source_id FROM import_source_aliases ORDER BY alias_digest")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			count := 0
			for rows.Next() {
				var alias, kind, id string
				if err = rows.Scan(&alias, &kind, &id); err != nil {
					t.Fatal(err)
				}
				if count >= len(tc.Aliases) {
					t.Fatal("extra alias")
				}
				expected := tc.Aliases[count]
				if alias != expected.AliasDigest || kind != expected.AliasKind || idMap[id] != expected.SourceID {
					t.Fatalf("alias state differs %s %s %s vs %+v", alias, kind, idMap[id], expected)
				}
				count++
			}
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			if count != len(tc.Aliases) {
				t.Fatal(fmt.Sprintf("alias count %d != %d", count, len(tc.Aliases)))
			}
		})
	}
	for _, tc := range fixture.CountCases {
		if got := CountDraftUnits(tc.Text); got != tc.Result {
			t.Errorf("counter %q: %d != actual %d", tc.Text, got, tc.Result)
		}
	}
	for _, tc := range fixture.ParseCases {
		t.Run("parser/"+tc.Name, func(t *testing.T) {
			data, err := base64.StdEncoding.DecodeString(tc.Raw)
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(key, StoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			binding := Binding{"synthetic-caller", "synthetic-generation", Reference}
			summary, err := store.Inspect(binding, []CapturedSource{{DisplayName: tc.Name, Reader: bytes.NewReader(data), Identity: Identity{tc.Identity.CanonicalLocation, tc.Identity.FileIdentity}}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := store.Consume(summary.InspectionID, summary.PreviewCAS, binding)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.Chapters, tc.Chapters) {
				t.Fatalf("chapters differ\nnative %+v\nactual %+v", result.Chapters, tc.Chapters)
			}
			nativeSources := make([]originalSource, 0, len(result.Sources))
			for _, s := range result.Sources {
				nativeSources = append(nativeSources, originalSource{s.LocationAliasDigest, s.FileAliasDigest, s.DisplayName, s.MediaType, s.Size})
			}
			if !reflect.DeepEqual(nativeSources, tc.Sources) {
				t.Fatalf("source metadata/identity differ: %+v != %+v", nativeSources, tc.Sources)
			}
			summary.InspectionID = ""
			summary.PreviewCAS = ""
			summary.ExpiresAt = 0
			if !reflect.DeepEqual(summary, tc.Summary) {
				t.Fatalf("summary differs: %+v != actual %+v", summary, tc.Summary)
			}
		})
	}
	for _, tc := range fixture.EPUBCases {
		t.Run("epub/"+tc.Name, func(t *testing.T) {
			data, err := base64.StdEncoding.DecodeString(tc.Raw)
			if err != nil {
				t.Fatal(err)
			}
			result, err := EPUBPreview(data)
			if tc.Error != "" {
				if err == nil || err.Error() != tc.Error {
					t.Fatalf("error %v != actual %s", err, tc.Error)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(result, tc.Result) {
				t.Fatalf("EPUB %+v %v != actual %+v", result, err, tc.Result)
			}
		})
	}
	for _, tc := range fixture.IdentityCases {
		t.Run("identity/"+tc.Name, func(t *testing.T) {
			sources := make([]Identity, 0, len(tc.Sources))
			for _, s := range tc.Sources {
				sources = append(sources, Identity{s.CanonicalLocation, s.FileIdentity})
			}
			result, err := EncodeIdentities(sources, tc.Purpose, key, runtime.GOOS == "windows")
			if tc.Error != "" {
				if err == nil || err.Error() != tc.Error {
					t.Fatalf("error %v != actual %s", err, tc.Error)
				}
				return
			}
			if err != nil || len(result) != len(tc.Result) {
				t.Fatal(err)
			}
			for i, encoded := range result {
				if encoded.LocationAliasDigest != tc.Result[i].LocationAliasDigest || encoded.FileAliasDigest != tc.Result[i].FileAliasDigest {
					t.Fatalf("identity differs %+v %+v", encoded, tc.Result[i])
				}
			}
		})
	}
}

// Security extensions are intentionally tested independently: the TS renderer
// token is not falsely reported as a keyed/generation-bound native receipt.
func TestNativeReceiptTamperAndCapacity(t *testing.T) {
	store := newTestStore(t, StoreOptions{MaxActive: 1})
	b := Binding{"c", "g", Reference}
	summary, err := store.Inspect(b, []CapturedSource{source("n.txt", "body")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Peek(summary.InspectionID+"0", b); err == nil {
		t.Fatal("tamper accepted")
	}
	if _, err = store.Inspect(Binding{"other", "g", Reference}, []CapturedSource{source("n.txt", "body")}); err == nil || err.Error() != "IMPORT_INSPECTION_LIMIT" {
		t.Fatal(err)
	}
	store.RevokeCaller(b.Caller)
	if store.ActiveCount() != 0 {
		t.Fatal("not revoked")
	}
	key := bytes.Repeat([]byte{107}, 32)
	reference := sourceFingerprint([]string{"b", "a"}, Reference, key)
	reversed := sourceFingerprint([]string{"a", "b"}, Reference, key)
	if reference != reversed || reference == sourceFingerprint([]string{"a", "b"}, AuthorManuscript, key) || strings.Contains(reference, "synthetic") {
		t.Fatal("collection identity binding")
	}
}
