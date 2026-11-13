package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"strings"
	"testing"
)

// These keys are public synthetic test data, never deployment credentials.
func testStorage(t *testing.T) *Storage {
	t.Helper()
	s, err := New(bytes.Repeat([]byte{0x47}, KeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 24, 31, 33, 64} {
		if _, err := New(make([]byte, n)); !errors.Is(err, ErrKeyInvalid) {
			t.Fatalf("key length %d: %v", n, err)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	s := testStorage(t)
	for _, text := range []string{"", "public synthetic fixture", "密钥📝\x00e\u0301\r\n", strings.Repeat("x", MaxSecretBytes)} {
		encrypted, err := s.EncryptString(text)
		if err != nil {
			t.Fatal(err)
		}
		if len(encrypted) != 36+len(text) || string(encrypted[:8]) != "ANWSEC01" {
			t.Fatal("incorrect envelope framing")
		}
		decrypted, err := s.DecryptString(encrypted)
		if err != nil || decrypted != text {
			t.Fatalf("roundtrip length %d: %v", len(text), err)
		}
		second, err := s.EncryptString(text)
		if err != nil || bytes.Equal(second, encrypted) {
			t.Fatal("nonce was not randomized")
		}
	}
}

func TestEncryptInputAndZeroStorage(t *testing.T) {
	s := testStorage(t)
	for _, input := range []string{strings.Repeat("x", MaxSecretBytes+1), strings.Repeat("密", MaxSecretBytes/3+1), string([]byte{0xff})} {
		if _, err := s.EncryptString(input); !errors.Is(err, ErrInputInvalid) {
			t.Fatalf("input validation: %v", err)
		}
	}
	var zero Storage
	if _, err := zero.EncryptString("public"); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("zero storage encrypt: %v", err)
	}
	var missing *Storage
	if _, err := missing.EncryptString("public"); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("nil storage encrypt: %v", err)
	}
	encrypted, err := s.EncryptString("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zero.DecryptString(encrypted); !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("zero storage decrypt: %v", err)
	}
}

func TestMalformedAndTampered(t *testing.T) {
	s := testStorage(t)
	encrypted, err := s.EncryptString("synthetic public value")
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 36; n++ {
		if _, err := s.DecryptString(encrypted[:n]); !errors.Is(err, ErrCiphertextInvalid) {
			t.Fatalf("truncation at %d: %v", n, err)
		}
	}
	if _, err := s.DecryptString(append([]byte("ANWSEC01"), make([]byte, 28+MaxSecretBytes+1)...)); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("oversized envelope: %v", err)
	}
	for i := range encrypted {
		tampered := bytes.Clone(encrypted)
		tampered[i] ^= 1
		want := ErrDecryptFailed
		if i < 8 {
			want = ErrCiphertextInvalid
		}
		if _, err := s.DecryptString(tampered); !errors.Is(err, want) {
			t.Fatalf("tamper at %d: %v", i, err)
		}
	}
	for n := 36; n < len(encrypted); n++ {
		if _, err := s.DecryptString(encrypted[:n]); !errors.Is(err, ErrDecryptFailed) {
			t.Fatalf("authenticated truncation at %d: %v", n, err)
		}
	}
	wrong, err := New(bytes.Repeat([]byte{0x48}, KeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.DecryptString(encrypted); !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestWireOrderAADAndCopiedKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x47}, 32)
	s, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	key[0] ^= 1
	encrypted, err := s.EncryptString("public fixture")
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(bytes.Repeat([]byte{0x47}, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	sealed := append(bytes.Clone(encrypted[36:]), encrypted[20:36]...)
	plain, err := aead.Open(nil, encrypted[8:20], sealed, []byte("AI-Novel-Writer/server-secret/v1"))
	if err != nil || string(plain) != "public fixture" {
		t.Fatalf("Node-compatible order/AAD and key copy: %v", err)
	}
	if _, err := aead.Open(nil, encrypted[8:20], sealed, []byte("wrong aad")); err == nil {
		t.Fatal("accepted wrong AAD")
	}
}

func TestDecryptUTF8Replacement(t *testing.T) {
	s := testStorage(t)
	cases := []struct {
		input []byte
		want  string
	}{
		{[]byte{0xe2, 0x82}, "\ufffd"},
		{[]byte{0xf0, 0x9f, 0x92}, "\ufffd"},
		{[]byte{0xe2, 0x82, 'x'}, "\ufffdx"},
		{[]byte{0xe0, 0x80, 0x80}, "\ufffd\ufffd\ufffd"},
		{[]byte{0xed, 0xa0, 0x80}, "\ufffd\ufffd\ufffd"},
		{[]byte{0xf4, 0x90, 0x80, 0x80}, "\ufffd\ufffd\ufffd\ufffd"},
		{[]byte{0xff, 0xfe, 'a', 0x80}, "\ufffd\ufffda\ufffd"},
		{[]byte{0xef, 0xbf, 0xbd, 0xe2, 'x'}, "\ufffd\ufffdx"},
	}
	for _, tc := range cases {
		nonce := make([]byte, 12)
		sealed := s.aead.Seal(nil, nonce, tc.input, []byte("AI-Novel-Writer/server-secret/v1"))
		envelope := append([]byte("ANWSEC01"), nonce...)
		envelope = append(envelope, sealed[len(tc.input):]...)
		envelope = append(envelope, sealed[:len(tc.input)]...)
		got, err := s.DecryptString(envelope)
		if err != nil || got != tc.want {
			t.Fatalf("UTF-8 %x: got %q, want %q, err %v", tc.input, got, tc.want, err)
		}
	}
}
