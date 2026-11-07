//go:build linux

package main

// This file serves the two `cloud-backup:*` names the desktop answers by writing
// and clearing a credential: cloud-backup:connect and
// cloud-backup:clear-credential (web/operations.json:67,73).
//
// # What the two names do
//
// connect admits the reference's account - endpoint, username, secret
// (electron/services/cloud-credential-store.ts:34-39) - seals the secret with a
// key this server owns, and publishes it as the store's one credential record
// (internal/cloudbackup/credential_linux.go). clear-credential removes that
// record. A second connect replaces the first, which is the reference's reselect:
// save() filters the same account id before it appends
// (electron/services/cloud-credential-store.ts:206-211).
//
// The wire shapes are the reference's own, unchanged:
//
//	connect          { accountId?, endpoint, username, secret }
//	                 -> { success, state: "connected", account, warning? }
//	                 (src/shared/cloud-backup.ts:130-137)
//	clear-credential [ localEndpointAccountId ]
//	                 -> { success, state: "unconfigured", affectedBindings }
//	                 (src/shared/cloud-backup.ts:175-179)
//
// Clearing when nothing is stored is a success, not a refusal. The reference's
// clear returns whether it removed anything and the controller discards that
// answer: `try { deps.credentialStore.clear(accountId) }` then
// `return { success: true as const, state: 'unconfigured' as const, affectedBindings }`
// (electron/controllers/cloud-backup-controller.ts:570-571).
//
// # What these names do not claim
//
// Neither name dials, and this lane still derives no dialable origin from a
// request endpoint: internal/egress admits only the immutable origin set the
// committed model profiles produce (production_model_egress_linux.go:47-56), and
// no administrator flag exists here to widen it. So the record is the account of
// the family's wire shape kept exactly as the desktop keeps it - the port that
// was missing - and not a claim that a remote destination was proved reachable.
// The desktop probes before it saves
// (electron/controllers/cloud-backup-controller.ts:288); this lane has no
// admittable origin to probe, so it answers `connected` on the strength of a
// sealed record it re-read from disk, and reports the account as
// `server-encrypted` - the one persistence the reference reserves for a key the
// server owns (electron/services/cloud-credential-store.ts:18-25) - never as
// `os-backed`. The served backup keeps writing to the server's own generation
// store; the binding stays derived.
//
// # The secret
//
// It is read once, validated, sealed and dropped. It is never formatted into a
// refusal, never logged and never part of an answer: every failure below is a
// symbolic code, and the account view this file builds carries only the account
// id, endpoint, username and persistence the reference puts in
// CloudBackupAccountView (src/shared/cloud-backup.ts:18-23). The one server-side
// read that can return it (cloudbackup.ResolveCredential) is not called by any
// operation bound here.
//
// # The account this lane has
//
// Exactly one: the reserved local account the six served names already speak
// (productionCloudBackupLocalAccountID). A request that names another account id
// is refused with CLOUD_BACKUP_CREDENTIAL_UNAVAILABLE - the family's own answer
// for an account id with no credential behind it
// (production_cloud_backup_linux.go:332-337) - instead of minting a second
// account no binding could ever reach.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/cloudbackup"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/secrets"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionCloudBackupCredentialPersistence is the persistence the reference
	// reserves for a key the server owns (SafeStorageLike.persistence
	// 'server-encrypted', electron/services/cloud-credential-store.ts:18-25). It is
	// deliberately not 'os-backed': no OS keychain is involved, and it is
	// deliberately not 'session-only': the record survives the process.
	productionCloudBackupCredentialPersistence = "server-encrypted"
	// productionCloudBackupCredentialMaxSecretBytes is the AES envelope's own
	// plaintext ceiling (internal/secrets.MaxSecretBytes, 64 KiB), which is also
	// the reference's MAX_SECRET_BYTES
	// (electron/services/cloud-credential-store.ts:15). The store enforces the same
	// value; naming it here lets the request be refused before a store root is
	// opened.
	productionCloudBackupCredentialMaxSecretBytes = secrets.MaxSecretBytes
	// productionCloudBackupCredentialMaxFieldBytes bounds the two non-secret fields
	// this file reads as text. Both are far below the store's own ceilings
	// (endpoint 2048, username 1024 runes), so a field this long is refused as input
	// rather than carried into the store.
	productionCloudBackupCredentialMaxFieldBytes = 4096
)

// productionCloudBackupCredentials is the binder for the two credential names. It
// owns no authority of its own: the transport's authenticated session is the only
// caller gate this lane has, the lane's open state gates the store, and the store
// itself admits or refuses every field. The record is one per deployment - the
// reserved local account - so this is a single destination account, not a second
// account namespace a role model would have to arbitrate.
type productionCloudBackupCredentials struct {
	open   *productionOpen
	config productionConfig

	// mu guards the lazily opened store and its remembered refusal. The store is
	// opened on first use, so binding this family still writes nothing at startup.
	mu       sync.Mutex
	store    *cloudbackup.Store
	storeErr error
}

// newProductionCloudBackupCredentials returns the adapter for the lane built by
// productionHandlerWithContract. It opens nothing and resolves nothing here.
func newProductionCloudBackupCredentials(open *productionOpen, config productionConfig) *productionCloudBackupCredentials {
	return &productionCloudBackupCredentials{open: open, config: config}
}

// Bind installs the two names. Both are inventoried (web/operations.json:67,73)
// and neither is bound by the six-name binder, so this cannot collide with it.
func (c *productionCloudBackupCredentials) Bind(server *transport.Server) error {
	if server == nil || c == nil || c.open == nil {
		return errProductionCloudBackupClosed
	}
	for _, binding := range []struct {
		name    string
		handler transport.Operation
	}{
		{productionCloudBackupConnectName, c.connect},
		{productionCloudBackupClearName, c.clearCredential},
	} {
		if err := server.Bind(binding.name, binding.handler); err != nil {
			return err
		}
	}
	return nil
}

// connect answers cloud-backup:connect: it admits one account, seals its secret
// at rest and answers the account the published record was re-read as. Every
// input is validated before the store is opened, so a refused credential leaves
// the store directory exactly as it was - a first connect that is refused does
// not even create it.
func (c *productionCloudBackupCredentials) connect(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	request, ok := productionCloudBackupObject(args)
	if !ok || !productionCloudBackupKeys(request,
		[]string{"endpoint", "username", "secret"}, []string{"accountId"}) {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	accountID := productionCloudBackupLocalAccountID
	if supplied, present := request["accountId"]; present {
		value, valid := productionCloudBackupText(supplied)
		if !valid || value != productionCloudBackupLocalAccountID {
			// One destination account exists. An id with no destination behind it
			// is refused rather than stored where nothing could read it.
			return productionCloudBackupFailure(productionCloudBackupCredentialMissing, ""), nil
		}
	}
	endpoint, endpointOK := productionCloudBackupCredentialField(request["endpoint"])
	username, usernameOK := productionCloudBackupCredentialField(request["username"])
	secret, secretOK := productionCloudBackupCredentialSecretArg(request["secret"])
	if !endpointOK || !usernameOK || !secretOK {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	credential, normalizeErr := cloudbackup.NormalizeCredential(cloudbackup.CredentialInput{
		AccountID: accountID,
		Endpoint:  endpoint,
		Username:  username,
		Secret:    secret,
	})
	if normalizeErr != nil {
		// Empty, malformed or oversized: the store's own admission decides, and the
		// browser's closed union has one code for all three.
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	store, storeErr := c.storeFor()
	if storeErr != nil {
		return productionCloudBackupFailure(productionCloudBackupStateNotSaved, ""), nil
	}
	metadata, writeErr := store.StoreCredential(ctx, credential)
	if writeErr != nil {
		return productionCloudBackupFailure(productionCloudBackupCredentialCode(writeErr), ""), nil
	}
	return map[string]any{
		"success": true,
		"state":   "connected",
		"account": productionCloudBackupStoredAccountView(metadata),
	}, nil
}

// clearCredential answers cloud-backup:clear-credential. The argument is the bare
// account id the reference's channel takes, and clearing an account that holds no
// record is a success - the reference answers success either way.
func (c *productionCloudBackupCredentials) clearCredential(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if c == nil || len(args) != 1 {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	accountID, ok := productionCloudBackupCredentialField(args[0])
	if !ok || accountID != productionCloudBackupLocalAccountID {
		return productionCloudBackupFailure(productionCloudBackupCredentialMissing, ""), nil
	}
	store, storeErr := c.storeFor()
	if storeErr != nil {
		return productionCloudBackupFailure(productionCloudBackupStateNotSaved, ""), nil
	}
	if _, clearErr := store.ClearCredential(ctx, accountID); clearErr != nil {
		return productionCloudBackupFailure(productionCloudBackupCredentialCode(clearErr), ""), nil
	}
	// affectedBindings is 0 because this lane stores no binding: the binding the
	// six served names report is derived from the caller's own lease, so there is
	// no second document to mark unconfigured.
	return map[string]any{
		"success":          true,
		"state":            "unconfigured",
		"affectedBindings": 0,
	}, nil
}

// storeFor opens the store on first use. A refusal is remembered: a store root
// that is not the private directory this package created does not become usable
// on a later call.
func (c *productionCloudBackupCredentials) storeFor() (*cloudbackup.Store, error) {
	if c == nil || c.open == nil {
		return nil, cloudbackup.ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store != nil {
		return c.store, nil
	}
	if c.storeErr != nil {
		return nil, c.storeErr
	}
	c.open.mu.Lock()
	closed, layout := c.open.closed, c.open.layout
	c.open.mu.Unlock()
	if closed {
		c.storeErr = errProductionCloudBackupClosed
		return nil, c.storeErr
	}
	store, err := cloudbackup.Open(filepath.Join(layout.dataRoot, productionCloudBackupDirectory), cloudbackup.Limits{
		MaxArchiveBytes: c.config.MaxDatabaseBytes,
		MaxGenerations:  productionCloudBackupMaxGenerations,
	})
	if err != nil {
		c.storeErr = err
		return nil, err
	}
	c.store = store
	return store, nil
}

// productionCloudBackupCredentialAccountView is the account cloud-backup:view
// answers with once the credential store is read: the stored account when one is
// stored, and the frozen session-only account when none is. It reports presence
// only - the account id, endpoint, username and persistence - so the secret
// cannot reach a wire through it, and it creates nothing: a lane that never stored
// a credential answers the session-only view with the data root untouched.
func productionCloudBackupCredentialAccountView(ctx context.Context, b *productionCloudBackup, principal string) map[string]any {
	if b == nil || b.open == nil {
		return productionCloudBackupAccountView(principal)
	}
	b.open.mu.Lock()
	closed, layout := b.open.closed, b.open.layout
	b.open.mu.Unlock()
	if closed {
		return productionCloudBackupAccountView(principal)
	}
	directory := filepath.Join(layout.dataRoot, productionCloudBackupDirectory)
	if info, statErr := os.Lstat(directory); statErr != nil || !info.IsDir() {
		return productionCloudBackupAccountView(principal)
	}
	store, storeErr := cloudbackup.Open(directory, cloudbackup.DefaultLimits())
	if storeErr != nil {
		return productionCloudBackupAccountView(principal)
	}
	metadata, present, credentialErr := store.Credential(ctx, productionCloudBackupLocalAccountID)
	if credentialErr != nil || !present {
		// A store this call cannot read is a refusal at connect and clear
		// (CLOUD_BACKUP_LOCAL_STATE_NOT_SAVED); the view keeps the answer it can
		// prove, which is that it found no account to report.
		return productionCloudBackupAccountView(principal)
	}
	return productionCloudBackupStoredAccountView(metadata)
}

// productionCloudBackupStoredAccountView is one CloudBackupAccountView for a
// stored credential (src/shared/cloud-backup.ts:18-23). It is built from the
// record's non-secret metadata only, so it has no field a secret could occupy.
func productionCloudBackupStoredAccountView(metadata cloudbackup.CredentialMetadata) map[string]any {
	return map[string]any{
		"accountId":   metadata.AccountID,
		"endpoint":    metadata.Endpoint,
		"username":    metadata.Username,
		"persistence": productionCloudBackupCredentialPersistence,
	}
}

// productionCloudBackupCredentialCode maps one credential refusal onto the
// browser's closed CloudBackupErrorCode union (src/shared/cloud-backup.ts:53-69).
// A credential the store will not admit is an input refusal; anything it could
// not read or write is the reference's local-state refusal
// (electron/controllers/cloud-backup-controller.ts:293-294). It never formats
// the cause into the answer.
func productionCloudBackupCredentialCode(err error) string {
	switch {
	case err == nil:
		return productionCloudBackupInputInvalid
	case errors.Is(err, cloudbackup.ErrCredentialInvalid),
		errors.Is(err, cloudbackup.ErrCredentialTooLarge),
		errors.Is(err, cloudbackup.ErrInput):
		return productionCloudBackupInputInvalid
	case errors.Is(err, cloudbackup.ErrCancelled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return productionCloudBackupCancelled
	}
	return productionCloudBackupStateNotSaved
}

// productionCloudBackupCredentialField reads one non-empty, valid-UTF-8 text
// field of the credential request. It bounds the length before the store's own
// narrower ceilings apply, so no request string is carried further than it can
// be used.
func productionCloudBackupCredentialField(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || text == "" || len(text) > productionCloudBackupCredentialMaxFieldBytes || !utf8.ValidString(text) {
		return "", false
	}
	return text, true
}

// productionCloudBackupCredentialSecretArg reads the one field this lane must
// never keep. The value is admitted here only so the refusal can be decided before a
// store root is opened; it is handed to the store's admission and then dropped.
// It is never logged, formatted or answered.
func productionCloudBackupCredentialSecretArg(value any) (string, bool) {
	secret, ok := value.(string)
	if !ok || secret == "" || len(secret) > productionCloudBackupCredentialMaxSecretBytes || !utf8.ValidString(secret) {
		return "", false
	}
	return secret, true
}
