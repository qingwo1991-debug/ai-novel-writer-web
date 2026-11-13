// Package secrets implements the server-secret-storage.ts AES-256-GCM envelope.
// Keys are explicitly supplied by the caller; this package never reads environment
// variables, application configuration, or a default key location.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	KeyBytes       = 32
	NonceBytes     = 12
	TagBytes       = 16
	MaxSecretBytes = 64 * 1024
	magic          = "ANWSEC01"
	aad            = "AI-Novel-Writer/server-secret/v1"
	headerBytes    = len(magic) + NonceBytes + TagBytes
)

var (
	ErrKeyInvalid        = errors.New("SERVER_SECRET_KEY_INVALID")
	ErrInputInvalid      = errors.New("SERVER_SECRET_INPUT_INVALID")
	ErrCiphertextInvalid = errors.New("SERVER_SECRET_CIPHERTEXT_INVALID")
	ErrDecryptFailed     = errors.New("SERVER_SECRET_DECRYPT_FAILED")
	ErrUnsupported       = errors.New("SERVER_SECRET_PLATFORM_UNSUPPORTED")
)

// Storage keeps an expanded copy of the supplied key. It is safe for concurrent
// calls. Preserving the original key is the caller's responsibility: losing it
// makes previously encrypted credentials unrecoverable.
type Storage struct {
	aead cipher.AEAD
}

// New accepts only a raw 32-byte AES-256 key, not hex/base64 text. Callers may
// explicitly decode configuration/environment values before calling New.
func New(key []byte) (*Storage, error) {
	if len(key) != KeyBytes {
		return nil, ErrKeyInvalid
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrKeyInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Storage{aead: aead}, nil
}

// EncryptString emits MAGIC || nonce || authentication tag || ciphertext.
// Go strings must contain valid UTF-8 (JavaScript strings are encoded as UTF-8
// by Buffer). The size limit is the encoded byte length, not the rune count.
func (s *Storage) EncryptString(plainText string) ([]byte, error) {
	if len(plainText) > MaxSecretBytes || !utf8.ValidString(plainText) {
		return nil, ErrInputInvalid
	}
	if s == nil || s.aead == nil {
		return nil, ErrKeyInvalid
	}
	nonce := make([]byte, NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := s.aead.Seal(nil, nonce, []byte(plainText), []byte(aad))
	ciphertextEnd := len(sealed) - TagBytes
	result := make([]byte, 0, headerBytes+len(plainText))
	result = append(result, magic...)
	result = append(result, nonce...)
	result = append(result, sealed[ciphertextEnd:]...)
	result = append(result, sealed[:ciphertextEnd]...)
	return result, nil
}

// DecryptString authenticates before decoding UTF-8. Like Buffer.toString('utf8'),
// malformed UTF-8 is decoded with U+FFFD using maximal ill-formed subparts.
func (s *Storage) DecryptString(encrypted []byte) (string, error) {
	if len(encrypted) < headerBytes || len(encrypted) > headerBytes+MaxSecretBytes || !bytes.Equal(encrypted[:len(magic)], []byte(magic)) {
		return "", ErrCiphertextInvalid
	}
	if s == nil || s.aead == nil {
		return "", ErrDecryptFailed
	}
	nonceEnd := len(magic) + NonceBytes
	tagEnd := nonceEnd + TagBytes
	sealed := make([]byte, 0, len(encrypted)-nonceEnd)
	sealed = append(sealed, encrypted[tagEnd:]...)
	sealed = append(sealed, encrypted[nonceEnd:tagEnd]...)
	plainText, err := s.aead.Open(nil, encrypted[len(magic):nonceEnd], sealed, []byte(aad))
	if err != nil {
		return "", ErrDecryptFailed
	}
	return decodeUTF8(plainText), nil
}

func decodeUTF8(input []byte) string {
	if utf8.Valid(input) {
		return string(input)
	}
	var out strings.Builder
	out.Grow(len(input))
	for len(input) != 0 {
		r, size := utf8.DecodeRune(input)
		if r != utf8.RuneError || size != 1 {
			out.Write(input[:size])
			input = input[size:]
			continue
		}
		// DecodeRune advances one byte on error. Node consumes a valid but
		// incomplete prefix together (e.g. E2 82 => one replacement character).
		n := 1
		want := 0
		b := input[0]
		switch {
		case b >= 0xc2 && b <= 0xdf:
			want = 2
		case b >= 0xe0 && b <= 0xef:
			want = 3
		case b >= 0xf0 && b <= 0xf4:
			want = 4
		}
		for n < want && n < len(input) {
			c := input[n]
			if c < 0x80 || c > 0xbf || (n == 1 && ((b == 0xe0 && c < 0xa0) || (b == 0xed && c > 0x9f) || (b == 0xf0 && c < 0x90) || (b == 0xf4 && c > 0x8f))) {
				break
			}
			n++
		}
		out.WriteRune(utf8.RuneError)
		input = input[n:]
	}
	return out.String()
}
