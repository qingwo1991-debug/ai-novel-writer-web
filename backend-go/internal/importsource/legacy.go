package importsource

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const legacyBridgeAAD = "ai-novel:legacy-import-source-salt:v1"

// EncodeProjectIdentities is the trusted project variant of EncodeIdentities.
// It authenticates the old salt row before including old stable identities.
// The returned type deliberately has no JSON fields for those identities.
func EncodeProjectIdentities(ctx context.Context, tx *sql.Tx, sources []Identity, purpose Purpose, key []byte, windows bool) ([]EncodedIdentity, error) {
	if ctx == nil || tx == nil {
		return nil, errors.New("PROJECT_NOT_OPEN")
	}
	encoded, err := EncodeIdentities(sources, purpose, key, windows)
	if err != nil {
		return nil, err
	}
	salt, err := legacyBridgeSalt(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	if salt == nil {
		return encoded, nil
	}
	for i, source := range sources {
		location := norm.NFC.String(trim(source.CanonicalLocation))
		if windows {
			location = strings.ToLower(location)
		}
		identity := trim(source.FileIdentity)
		if identity == "" {
			identity = "canonical:" + location
		}
		encoded[i].LegacyStableIdentity = identity
	}
	return encoded, nil
}

// legacyBridgeSalt reads only the fixed encrypted row. Decryption errors are
// indistinguishable to callers and never log key, ciphertext or author paths.
func legacyBridgeSalt(ctx context.Context, tx *sql.Tx, key []byte) ([]byte, error) {
	if ctx == nil || tx == nil {
		return nil, errors.New("PROJECT_NOT_OPEN")
	}
	var tables int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'import_legacy_identity_bridge'").Scan(&tables); err != nil {
		return nil, err
	}
	if tables == 0 {
		return nil, nil
	}
	var ciphertextHex, ivHex, tagHex string
	err := tx.QueryRowContext(ctx, "SELECT ciphertext_hex, iv_hex, auth_tag_hex FROM import_legacy_identity_bridge WHERE id = 'main'").Scan(&ciphertextHex, &ivHex, &tagHex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("IMPORT_SOURCE_LEGACY_BRIDGE_INVALID")
	}
	invalid := errors.New("IMPORT_SOURCE_LEGACY_BRIDGE_INVALID")
	if len(key) != 32 || len(ciphertextHex) > 256 || len(ivHex) != 24 || len(tagHex) != 32 {
		return nil, invalid
	}
	ciphertext, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return nil, invalid
	}
	iv, err := hex.DecodeString(ivHex)
	if err != nil {
		return nil, invalid
	}
	tag, err := hex.DecodeString(tagHex)
	if err != nil {
		return nil, invalid
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, invalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, invalid
	}
	combined := append(append([]byte(nil), ciphertext...), tag...)
	salt, err := aead.Open(nil, iv, combined, []byte(legacyBridgeAAD))
	if err != nil || len(salt) != 32 {
		return nil, invalid
	}
	return salt, nil
}

func validLegacyIdentity(identity string) bool {
	return identity != "" && len(identity) <= 32768 && utf8.ValidString(identity) && !strings.ContainsRune(identity, 0)
}

// JS uses en-US localeCompare when sorting legacy collections. A singleton
// needs no collation. For multiple sources, admit only same-device Linux inode
// identities: their common punctuation prefix and decimal suffix order have
// the same en-US and bytewise ordering. Other sets fail before alias writes.
func legacySortSafe(identities []string) bool {
	if len(identities) < 2 {
		return true
	}
	device := ""
	for _, identity := range identities {
		if !strings.HasPrefix(identity, "dev:") {
			return false
		}
		parts := strings.Split(identity, ":")
		if len(parts) != 4 || parts[0] != "dev" || parts[2] != "ino" || !decimalASCII(parts[1]) || !decimalASCII(parts[3]) {
			return false
		}
		if device == "" {
			device = parts[1]
		} else if device != parts[1] {
			return false
		}
	}
	return true
}

func decimalASCII(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func legacyFingerprint(salt []byte, purpose Purpose, identities []string) (string, error) {
	if !legacySortSafe(identities) {
		return "", errors.New("IMPORT_SOURCE_LEGACY_COLLATION_UNSUPPORTED")
	}
	sorted := append([]string(nil), identities...)
	sort.Strings(sorted)
	value := struct {
		Purpose    Purpose  `json:"purpose"`
		Identities []string `json:"identities"`
	}{purpose, sorted}
	return keyedDigest(salt, "", jsonExact(value)), nil
}
