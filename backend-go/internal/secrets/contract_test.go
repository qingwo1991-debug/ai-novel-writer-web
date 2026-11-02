package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNodeGoSecretContract consumes only the explicitly named public synthetic
// fixture generated on GitHub Actions. No author key, default key path, or host
// credentials are read. The separate original TypeScript oracle subsequently
// decrypts the Go outputs, so both directions exercise the actual wire format.
func TestNodeGoSecretContract(t *testing.T) {
	path := os.Getenv("GO_SECRET_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("GO_SECRET_CONTRACT is required on GitHub Actions")
		}
		t.Skip("public Node/Go fixture is generated only by the GitHub Actions oracle")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatal("contract path must be canonical absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	type contractCase struct {
		Plaintext string `json:"plaintext"`
		Encrypted string `json:"encrypted"`
	}
	var contract struct {
		FormatVersion int `json:"formatVersion"`
		PublicTestKeyHex string `json:"publicTestKeyHex"`
		Cases []contractCase `json:"cases"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1024 * 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("contract must contain exactly one JSON document: %v", err)
	}
	if contract.FormatVersion != 1 || contract.PublicTestKeyHex != strings.Repeat("47", 32) || len(contract.Cases) != 3 {
		t.Fatal("unexpected public fixture schema, key, version, or case count")
	}
	key, err := hex.DecodeString(contract.PublicTestKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	if contract.Cases[0].Plaintext != "" || contract.Cases[1].Plaintext != "Go / Node 凭据兼容 😀 <>&\u2028\u2029\u0000" || contract.Cases[2].Plaintext != strings.Repeat("x", MaxSecretBytes) {
		t.Fatal("contract is missing empty, Unicode/NUL, or maximum-size case")
	}
	output := make([]contractCase, 0, len(contract.Cases))
	for i, tc := range contract.Cases {
		envelope, err := base64.StdEncoding.Strict().DecodeString(tc.Encrypted)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		plain, err := s.DecryptString(envelope)
		if err != nil || plain != tc.Plaintext {
			t.Fatalf("Node to Go case %d: %v", i, err)
		}
		generated, err := s.EncryptString(tc.Plaintext)
		if err != nil {
			t.Fatalf("Go encryption case %d: %v", i, err)
		}
		if bytes.Equal(generated, envelope) {
			t.Fatalf("case %d unexpectedly reused Node nonce", i)
		}
		output = append(output, contractCase{Plaintext: tc.Plaintext, Encrypted: base64.StdEncoding.EncodeToString(generated)})
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(path + ".go.json", os.O_WRONLY | os.O_CREATE | os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := out.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := out.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
