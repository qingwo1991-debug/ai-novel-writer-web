//go:build linux

package cloudbackup

// This file is the cloud-backup store's at-rest credential record: the one
// secret the family administers through cloud-backup:connect and
// cloud-backup:clear-credential.
//
// # Where a credential lives
//
// Two reserved names at the store root, both dot-prefixed, and that prefix is a
// proof rather than a convention: bookPattern (store.go:97) requires an
// alphanumeric first byte, so no book id - and therefore no path this store
// derives from one - can ever name either of them.
//
//	.cloud-backup-credential.json   the record: account id, endpoint, username, sealed secret
//	.credential.key      the 32-byte AES-256 key this server owns
//
// # How the secret is sealed
//
// internal/secrets is this repository's at-rest convention for a key the server
// holds: the AES-256-GCM envelope ("ANWSEC01", aad
// "AI-Novel-Writer/server-secret/v1") that the sealed generation lane already
// resolves model credentials through (internal/generationowner/dispatch.go:13-31
// - "*secrets.Storage satisfies it for the AES-GCM lane"). Its key-file
// primitive is reused rather than re-implemented, so this record inherits the
// checks the rest of the lane applies to a private key rather than inventing
// weaker ones: the key must be current-UID-owned, exactly 0600, a regular file
// with one hard link and exactly 32 bytes, and its directory must be owned and
// not writable by others (internal/secrets/keyfile_linux.go:113-134). The
// envelope's plaintext ceiling (secrets.MaxSecretBytes) is this record's
// ceiling too, so the two cannot drift apart.
//
// # What is never written, logged or answered
//
// The secret is cleartext exactly once, in the caller's memory, between
// NormalizeCredential and EncryptString. It is never formatted into an error,
// never logged, and never part of a CredentialMetadata: the metadata this store
// answers with is the same non-secret half the desktop reports as
// CloudBackupAccountView (src/shared/cloud-backup.ts:18-23). ResolveCredential
// is the one server-side read, and no wired operation calls it; its consumer is
// the remote destination this lane does not have yet.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/secrets"
)

const (
	// credentialFileName is the one record this store holds. It is dot-prefixed
	// for the reason this file's header gives: outside bookPattern, so outside
	// every path a book id can derive.
	credentialFileName = ".cloud-backup-credential.json"
	// credentialKeyName is the server-owned key the record is sealed with. It
	// outlives the record: clearCredential removes the credential, not the key,
	// because the key belongs to the store rather than to one account.
	credentialKeyName = ".credential.key"
	// credentialTemporaryPrefix is the dot-prefixed staging name one atomic
	// publish writes before its single rename. A leftover is never read.
	credentialTemporaryPrefix = ".credential-"
	// credentialVersion and credentialPersistence are written into every record
	// and required on every read, so a document edited by hand is not mistaken
	// for one this store sealed.
	credentialVersion     = 1
	credentialPersistence = "server-encrypted"
	// maxCredentialEndpointBytes and maxCredentialUsernameRunes are the
	// reference's own ceilings (electron/services/cloud-credential-store.ts:13-14:
	// MAX_ENDPOINT_LENGTH 2048, MAX_USERNAME_LENGTH 1024).
	maxCredentialEndpointBytes = 2048
	maxCredentialUsernameRunes = 1024
	// maxCredentialDocumentBytes bounds the record: the sealed plaintext ceiling
	// (64 KiB) plus the envelope header, base64's 4/3 expansion and the metadata.
	maxCredentialDocumentBytes int64 = 128 * 1024
)

// Refusals are symbolic and finite, like the store's own (store.go:45-61): no
// host value and no secret is ever wrapped into one, because a caller may print
// it.
var (
	// ErrCredentialInvalid refuses a credential whose account id, endpoint,
	// username or secret is empty or malformed.
	ErrCredentialInvalid = errors.New("CLOUD_BACKUP_CREDENTIAL_INVALID")
	// ErrCredentialTooLarge refuses a secret, an endpoint, a username or a record
	// document above its ceiling.
	ErrCredentialTooLarge = errors.New("CLOUD_BACKUP_CREDENTIAL_TOO_LARGE")
	// ErrCredentialSealed refuses a record this store cannot read back: a document
	// it did not write, a missing key, or a ciphertext the key cannot authenticate.
	ErrCredentialSealed = errors.New("CLOUD_BACKUP_CREDENTIAL_SEALED")
)

// credentialAccountPattern is the account namespace: one alphanumeric first byte
// then at most 127 of the characters the reference's UUID-or-name accounts use.
// The reserved local account ("local") and a version-4 UUID both match; nothing
// that could name a parent directory does.
var credentialAccountPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+\-]{0,127}$`)

// CredentialMetadata is the non-secret half of one stored credential. It is
// exactly the shape a caller may answer with (CloudBackupAccountView), and it
// carries no field that could hold the secret.
type CredentialMetadata struct {
	AccountID string
	Endpoint  string
	Username  string
}

// CredentialInput is one credential to store: the reference's
// CloudCredentialSaveInput (electron/services/cloud-credential-store.ts:34-39)
// after the wire has been read.
type CredentialInput struct {
	AccountID string
	Endpoint  string
	Username  string
	Secret    string
}

// credentialDocument is the on-disk record. Every key is written by this file and
// required on read; persistence says how the secret was sealed, so a record that
// claims another mechanism is refused instead of being decrypted hopefully.
type credentialDocument struct {
	Version               int    `json:"version"`
	AccountID             string `json:"accountId"`
	Endpoint              string `json:"endpoint"`
	Username              string `json:"username"`
	Persistence           string `json:"persistence"`
	EncryptedSecretBase64 string `json:"encryptedSecretBase64"`
}

// CredentialFile reports the server-side path of the credential record. Like
// Root and Directory it is server-side only and must never be placed on a wire:
// it is a host path, and the file below it is the one this package keeps sealed.
func (s *Store) CredentialFile() string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.root, credentialFileName)
}

// NormalizeCredential admits and normalizes one credential without touching the
// filesystem, so a caller can refuse a malformed credential before a store root
// is opened and before a byte of the directory changes.
func NormalizeCredential(input CredentialInput) (CredentialInput, error) {
	var empty CredentialInput
	if !credentialAccountPattern.MatchString(input.AccountID) {
		return empty, ErrCredentialInvalid
	}
	endpoint, err := normalizeCredentialEndpoint(input.Endpoint)
	if err != nil {
		return empty, err
	}
	if err := normalizeCredentialUsername(input.Username); err != nil {
		return empty, err
	}
	// The reference's own rule (electron/services/cloud-credential-store.ts:113-115):
	// non-empty, valid UTF-8, and no larger than the envelope can carry.
	if input.Secret == "" || !utf8.ValidString(input.Secret) {
		return empty, ErrCredentialInvalid
	}
	if len(input.Secret) > secrets.MaxSecretBytes {
		return empty, ErrCredentialTooLarge
	}
	return CredentialInput{
		AccountID: input.AccountID,
		Endpoint:  endpoint,
		Username:  input.Username,
		Secret:    input.Secret,
	}, nil
}

// normalizeCredentialEndpoint is the reference's normalizeWebDavEndpoint
// (electron/services/cloud-credential-store.ts:117-131) with this lane's one
// addition: a destination with no host is not an endpoint. Only https - or http
// to loopback - is admitted, no userinfo, query or fragment may appear, and the
// path is always answered with a trailing separator.
func normalizeCredentialEndpoint(value string) (string, error) {
	candidate := strings.TrimSpace(value)
	if candidate == "" || len(candidate) > maxCredentialEndpointBytes {
		return "", ErrCredentialInvalid
	}
	if credentialContainsControl(candidate) {
		return "", ErrCredentialInvalid
	}
	if strings.ContainsAny(candidate, "?#") {
		return "", ErrCredentialInvalid
	}
	parsed, err := url.Parse(candidate)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" {
		return "", ErrCredentialInvalid
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", ErrCredentialInvalid
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", ErrCredentialInvalid
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", ErrCredentialInvalid
	}
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if parsed.Scheme == "http" && !loopback {
		return "", ErrCredentialInvalid
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	if !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}
	return parsed.String(), nil
}

// normalizeCredentialUsername refuses an empty, oversized, invalid-UTF-8 or
// control-bearing username. The reference's rule is the same set
// (electron/services/cloud-credential-store.ts:92-111).
func normalizeCredentialUsername(value string) error {
	if value == "" || !utf8.ValidString(value) || len(value) > maxCredentialUsernameRunes*utf8.UTFMax {
		return ErrCredentialInvalid
	}
	if utf8.RuneCountInString(value) > maxCredentialUsernameRunes {
		return ErrCredentialInvalid
	}
	if credentialContainsControl(value) {
		return ErrCredentialInvalid
	}
	return nil
}

// credentialContainsControl reports whether a value carries a C0 or C1 control
// character, the same span the reference calls containsControlCharacter
// (electron/services/cloud-credential-store.ts:92-98).
func credentialContainsControl(value string) bool {
	for _, character := range value {
		if character <= 0x1f || (character >= 0x7f && character <= 0x9f) {
			return true
		}
	}
	return false
}

// StoreCredential seals one credential and publishes it as the store's single
// record. A second call replaces the first - the reference's reselect
// (electron/services/cloud-credential-store.ts:206-211 filters the same account
// id before it appends) - and nothing is answered that was not re-read from the
// published document.
func (s *Store) StoreCredential(ctx context.Context, input CredentialInput) (CredentialMetadata, error) {
	var empty CredentialMetadata
	if err := s.usable(ctx); err != nil {
		return empty, err
	}
	normalized, err := NormalizeCredential(input)
	if err != nil {
		return empty, err
	}
	if err := s.privateCredentialRoot(); err != nil {
		return empty, err
	}
	// A stored record for another account is never replaced silently: this store
	// holds one credential, and a caller that names a different account has a bug
	// that must not cost that account its secret.
	stored, storedErr := s.readCredentialDocument()
	switch {
	case storedErr == nil:
		if stored.AccountID != normalized.AccountID {
			return empty, ErrCredentialInvalid
		}
	case errors.Is(storedErr, os.ErrNotExist):
	default:
		return empty, storedErr
	}
	key, keyErr := secrets.LoadOrCreateKey(s.credentialKeyPath())
	if keyErr != nil {
		if errors.Is(keyErr, secrets.ErrKeyInvalid) {
			return empty, ErrUnsafe
		}
		return empty, keyErr
	}
	sealer, sealerErr := secrets.New(key)
	if sealerErr != nil {
		return empty, ErrCredentialSealed
	}
	sealed, sealErr := sealer.EncryptString(normalized.Secret)
	if sealErr != nil {
		if errors.Is(sealErr, secrets.ErrInputInvalid) {
			return empty, ErrCredentialInvalid
		}
		return empty, sealErr
	}
	// The envelope is proved to round-trip before a byte is published: a key file
	// that cannot open what it just sealed is a refusal here, not a credential the
	// next read discovers is unreadable.
	opened, openErr := sealer.DecryptString(sealed)
	if openErr != nil || opened != normalized.Secret {
		return empty, ErrCredentialSealed
	}
	document := credentialDocument{
		Version:               credentialVersion,
		AccountID:             normalized.AccountID,
		Endpoint:              normalized.Endpoint,
		Username:              normalized.Username,
		Persistence:           credentialPersistence,
		EncryptedSecretBase64: base64.StdEncoding.EncodeToString(sealed),
	}
	encoded, encodeErr := json.Marshal(document)
	if encodeErr != nil {
		return empty, ErrCredentialSealed
	}
	if int64(len(encoded)) > maxCredentialDocumentBytes {
		return empty, ErrCredentialTooLarge
	}
	// The record is admitted by the same parser the next read uses before it is
	// written, so this store can never publish a document it would refuse.
	if _, parseErr := parseCredentialDocument(encoded); parseErr != nil {
		return empty, parseErr
	}
	if err := s.writeCredentialDocument(encoded); err != nil {
		return empty, err
	}
	metadata, present, readErr := s.Credential(ctx, normalized.AccountID)
	if readErr != nil {
		return empty, readErr
	}
	if !present {
		return empty, ErrCredentialSealed
	}
	return metadata, nil
}

// Credential returns the stored non-secret metadata of one account. present is
// false when nothing is stored for it: a missing record and a record for another
// account are both "nothing stored", never a failure. It reads no key and
// decrypts nothing, which is why presence can be answered without the secret
// ever entering this process's memory.
func (s *Store) Credential(ctx context.Context, accountID string) (CredentialMetadata, bool, error) {
	var empty CredentialMetadata
	if err := s.usable(ctx); err != nil {
		return empty, false, err
	}
	if !credentialAccountPattern.MatchString(accountID) {
		return empty, false, ErrCredentialInvalid
	}
	document, err := s.readCredentialDocument()
	if err != nil {
		// ErrNotFound is the read helper's own answer for a record that vanished
		// between its admission and its read, so a concurrent clear is "nothing
		// stored" rather than a store failure.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotFound) {
			return empty, false, nil
		}
		return empty, false, err
	}
	if document.AccountID != accountID {
		return empty, false, nil
	}
	return CredentialMetadata{
		AccountID: document.AccountID,
		Endpoint:  document.Endpoint,
		Username:  document.Username,
	}, true, nil
}

// ResolveCredential returns one stored credential, secret included, for the one
// server-side consumer that may talk to a destination: the reference's
// resolveSecret (electron/services/cloud-credential-store.ts:214-235). It is not
// reachable from any operation this lane binds, and a refusal here is symbolic -
// a missing key or an inauthentic ciphertext never becomes a sentence carrying a
// value.
func (s *Store) ResolveCredential(ctx context.Context, accountID string) (CredentialInput, bool, error) {
	var empty CredentialInput
	if err := s.usable(ctx); err != nil {
		return empty, false, err
	}
	if !credentialAccountPattern.MatchString(accountID) {
		return empty, false, ErrCredentialInvalid
	}
	document, err := s.readCredentialDocument()
	if err != nil {
		// ErrNotFound is the read helper's own answer for a record that vanished
		// between its admission and its read, so a concurrent clear is "nothing
		// stored" rather than a store failure.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotFound) {
			return empty, false, nil
		}
		return empty, false, err
	}
	if document.AccountID != accountID {
		return empty, false, nil
	}
	sealed, decodeErr := base64.StdEncoding.DecodeString(document.EncryptedSecretBase64)
	if decodeErr != nil {
		return empty, false, ErrCredentialSealed
	}
	key, keyErr := secrets.LoadExistingKey(s.credentialKeyPath())
	if keyErr != nil {
		return empty, false, ErrCredentialSealed
	}
	sealer, sealerErr := secrets.New(key)
	if sealerErr != nil {
		return empty, false, ErrCredentialSealed
	}
	secret, openErr := sealer.DecryptString(sealed)
	if openErr != nil {
		return empty, false, ErrCredentialSealed
	}
	// What came out of the envelope is admitted by the same rules its plaintext
	// went in under, so a tampered record cannot smuggle another shape back.
	normalized, normalizeErr := NormalizeCredential(CredentialInput{
		AccountID: document.AccountID,
		Endpoint:  document.Endpoint,
		Username:  document.Username,
		Secret:    secret,
	})
	if normalizeErr != nil {
		return empty, false, ErrCredentialSealed
	}
	return normalized, true, nil
}

// ClearCredential removes the stored record. A record that is not there is not a
// failure: the reference's clear filters the account list and reports whether it
// changed anything (electron/services/cloud-credential-store.ts:237-248), and the
// controller answers success either way
// (electron/controllers/cloud-backup-controller.ts:570-571). A record belonging to
// another account is left alone: removed is false and nothing is written.
func (s *Store) ClearCredential(ctx context.Context, accountID string) (bool, error) {
	if err := s.usable(ctx); err != nil {
		return false, err
	}
	if !credentialAccountPattern.MatchString(accountID) {
		return false, ErrCredentialInvalid
	}
	if err := s.privateCredentialRoot(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	document, err := s.readCredentialDocument()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if document.AccountID != accountID {
		return false, nil
	}
	if err := os.Remove(s.CredentialFile()); err != nil {
		return false, err
	}
	if err := syncCredentialDirectory(s.root); err != nil {
		return true, err
	}
	return true, nil
}

// privateCredentialRoot proves the store root is the private directory this
// package created, with the ownership and mode a key-bearing directory must
// have: current-UID-owned and not writable by anyone else. The store's own Open
// proves the root is a physical directory; this is the narrower claim a record
// sealed with a key inside it needs.
func (s *Store) privateCredentialRoot() error {
	info, err := os.Lstat(s.root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return ErrUnsafe
	}
	if info.Mode().Perm()&0o022 != 0 {
		return ErrUnsafe
	}
	return nil
}

// credentialKeyPath is the server-side key path, derived from the admitted root
// and never from a request.
func (s *Store) credentialKeyPath() string {
	return filepath.Join(s.root, credentialKeyName)
}

// readCredentialDocument reads and validates the one record. The file itself is
// proved private before its bytes are read, so a record another user could edit
// is refused rather than parsed.
func (s *Store) readCredentialDocument() (credentialDocument, error) {
	var empty credentialDocument
	if s == nil || s.root == "" {
		return empty, ErrClosed
	}
	info, err := os.Lstat(s.CredentialFile())
	if err != nil {
		return empty, err
	}
	if !privateCredentialFile(info) {
		return empty, ErrUnsafe
	}
	data, readErr := readBounded(s.CredentialFile(), maxCredentialDocumentBytes)
	if readErr != nil {
		return empty, readErr
	}
	return parseCredentialDocument(data)
}

// writeCredentialDocument publishes one record atomically: a private temporary
// file in the same directory, fsynced, then a single rename over the record and
// a directory sync. A refused connect never reaches this function, and a failed
// write never leaves a partial record under the published name.
func (s *Store) writeCredentialDocument(data []byte) error {
	target := s.CredentialFile()
	if info, err := os.Lstat(target); err == nil {
		if !privateCredentialFile(info) {
			return ErrUnsafe
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, createErr := os.CreateTemp(s.root, credentialTemporaryPrefix+"*.tmp")
	if createErr != nil {
		return createErr
	}
	name := temporary.Name()
	published := false
	defer func() {
		if !published {
			_ = temporary.Close()
			_ = os.Remove(name)
		}
	}()
	// os.CreateTemp already asks for 0600; the explicit chmod keeps the record at
	// exactly 0600 under any umask, which is the mode the read path requires.
	if chmodErr := temporary.Chmod(fileMode); chmodErr != nil {
		return chmodErr
	}
	if _, writeErr := temporary.Write(data); writeErr != nil {
		return writeErr
	}
	if syncErr := temporary.Sync(); syncErr != nil {
		return syncErr
	}
	if closeErr := temporary.Close(); closeErr != nil {
		return closeErr
	}
	if renameErr := os.Rename(name, target); renameErr != nil {
		return renameErr
	}
	published = true
	return syncCredentialDirectory(s.root)
}

// parseCredentialDocument admits one record: exact keys only, the version and
// persistence this file writes, a canonical endpoint, a valid account id and
// username, and a base64 envelope within the ceiling. Anything else is
// ErrCredentialSealed, because the caller supplies no part of a document.
func parseCredentialDocument(data []byte) (credentialDocument, error) {
	var empty credentialDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document credentialDocument
	if err := decoder.Decode(&document); err != nil {
		return empty, ErrCredentialSealed
	}
	if err := decoder.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return empty, ErrCredentialSealed
	}
	if document.Version != credentialVersion || document.Persistence != credentialPersistence {
		return empty, ErrCredentialSealed
	}
	if !credentialAccountPattern.MatchString(document.AccountID) {
		return empty, ErrCredentialSealed
	}
	endpoint, endpointErr := normalizeCredentialEndpoint(document.Endpoint)
	if endpointErr != nil || endpoint != document.Endpoint {
		return empty, ErrCredentialSealed
	}
	if normalizeCredentialUsername(document.Username) != nil {
		return empty, ErrCredentialSealed
	}
	if document.EncryptedSecretBase64 == "" {
		return empty, ErrCredentialSealed
	}
	sealed, sealedErr := base64.StdEncoding.DecodeString(document.EncryptedSecretBase64)
	if sealedErr != nil || len(sealed) == 0 || len(sealed) > secrets.TagBytes+secrets.NonceBytes+credentialEnvelopeMagicBytes+secrets.MaxSecretBytes {
		return empty, ErrCredentialSealed
	}
	return document, nil
}

// credentialEnvelopeMagicBytes is the envelope's fixed 8-byte magic
// ("ANWSEC01"), which internal/secrets does not export; the nonce and tag
// lengths it does export are used directly below.
const credentialEnvelopeMagicBytes = len("ANWSEC01")

// privateCredentialFile reports whether one file is a record this store wrote:
// regular, exactly 0600, no setuid/setgid/sticky bit, current-UID-owned, one hard
// link, and within the document ceiling. It is the file-level counterpart of
// internal/secrets' privateKey and of internal/globalconfig's readModelsRaw
// (internal/globalconfig/models_linux.go:47-49).
func privateCredentialFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != fileMode {
		return false
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	if info.Size() <= 0 || info.Size() > maxCredentialDocumentBytes {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return false
	}
	return true
}

// syncCredentialDirectory fsyncs the store root after a publish or a removal, so
// the rename or unlink is durable before the call answers.
func syncCredentialDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
