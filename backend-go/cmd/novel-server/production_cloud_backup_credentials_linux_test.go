//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/cloudbackup"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves the two credential names over real HTTP through the frozen
// session: no handler is called directly, every answer read here is the answer
// the browser would receive, and the secret is compared against the bytes on
// disk as well as against every response body.
//
// The lane is built exactly as production_open_linux.go will build it: the
// project lifecycle (so project:open can issue the lease cloud-backup:view
// names), the six served cloud-backup names, and then the two names this change
// adds. A collision between the two binders fails here, so this file cannot
// prove a wiring production would refuse.

const (
	productionCloudBackupCredentialEndpoint           = "https://backup.example.invalid/dav"
	productionCloudBackupCredentialNormalizedEndpoint = "https://backup.example.invalid/dav/"
	productionCloudBackupCredentialUsername           = "backup-author"
	productionCloudBackupCredentialReselectedUsername = "backup-author-reselected"
	// productionCloudBackupCredentialSecret is the reference fixture's secret: the
	// WebDAV password the desktop keeps, and the one value that must never reach a
	// wire or a file. The reselect test stores a different secret over it.
	productionCloudBackupCredentialSecret           = productionCloudBackupTestSecret
	productionCloudBackupCredentialReselectedSecret = productionCloudBackupTestSecret + "-reselected"
	// productionCloudBackupCredentialAbsent is the tree digest of a store root that
	// does not exist yet: a refusal on a fresh lane must leave exactly that.
	productionCloudBackupCredentialAbsent = "absent"
	// productionCloudBackupCredentialViewSource and ...ViewSeam name the one-line
	// seam cloud-backup:view needs to read the store this change ships. The seam is
	// asserted through productionSeamContains because CI formats the source in
	// place before it runs these tests.
	productionCloudBackupCredentialViewSource = "production_cloud_backup_linux.go"
	productionCloudBackupCredentialViewSeam   = "productionCloudBackupCredentialAccountView(ctx,b,caller.Principal)"
	productionCloudBackupCredentialRecordKey  = "encryptedSecretBase64"
)

// productionCloudBackupCredentialLane opens the real production lane over a
// disposable data root and installs the three binders these names need, in the
// order production_open_linux.go installs them.
func productionCloudBackupCredentialLane(t *testing.T, root string) http.Handler {
	t.Helper()
	config := productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes}
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, config)
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-cloud-backup-credentials")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	// The six served names first, then the two this change adds: the same order and
	// the same two binders production_open_linux.go wires, so a name one binder
	// already owns fails here instead of being reported as served.
	if bindErr := newProductionCloudBackup(open, config).Bind(server); bindErr != nil {
		t.Fatal(bindErr)
	}
	if bindErr := newProductionCloudBackupCredentials(open, config).Bind(server); bindErr != nil {
		t.Fatal(bindErr)
	}
	bound := server.BoundOperations()
	for _, name := range []string{productionCloudBackupConnectName, productionCloudBackupClearName} {
		if !productionCloudBackupCredentialBound(bound, name) {
			t.Fatalf("%s is not bound by the credential binder; bound=%v", name, bound)
		}
	}
	return server
}

// productionCloudBackupCredentialBound reports whether one name is in the sorted
// set the server really bound.
func productionCloudBackupCredentialBound(bound []string, name string) bool {
	for _, candidate := range bound {
		if candidate == name {
			return true
		}
	}
	return false
}

// productionCloudBackupCredentialConnectArgs is one connect request. The account
// id is omitted unless the case supplies one, exactly like the wire's optional
// field (src/shared/cloud-backup.ts:104-109).
func productionCloudBackupCredentialConnectArgs(endpoint, username, secret string) []any {
	return []any{map[string]any{
		"endpoint": endpoint,
		"username": username,
		"secret":   secret,
	}}
}

// productionCloudBackupCredentialTree hashes every name, mode and byte under the
// store root, so "the directory is unchanged" is a claim about bytes rather than
// about one file still existing. A root that does not exist answers "absent",
// which is its own byte-level state.
func productionCloudBackupCredentialTree(t *testing.T, root string) string {
	t.Helper()
	directory := productionCloudBackupStoreDirectory(root)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return productionCloudBackupCredentialAbsent
	}
	if err != nil {
		t.Fatalf("inspect the store root: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("the store root is not a directory (mode %v)", info.Mode())
	}
	digest := sha256.New()
	walkErr := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relErr := filepath.Rel(directory, path)
		if relErr != nil {
			return relErr
		}
		item, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if _, err := io.WriteString(digest, relative+"|"+item.Mode().String()+"|"); err != nil {
			return err
		}
		if item.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			sum := sha256.Sum256(data)
			if _, err := digest.Write(sum[:]); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(digest, "\n"); err != nil {
			return err
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk the store root: %v", walkErr)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// productionCloudBackupCredentialRecord reads the sealed record's bytes and
// proves the file itself is private before anything reads it as a credential.
func productionCloudBackupCredentialRecord(t *testing.T, store *cloudbackup.Store) []byte {
	t.Helper()
	path := store.CredentialFile()
	if path == "" {
		t.Fatal("the store answered no credential path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat the credential record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the credential record is mode %v, not 0600", info.Mode().Perm())
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the credential record: %v", readErr)
	}
	return data
}

// productionCloudBackupCredentialNoSecret fails when one body carries the secret
// in cleartext or in base64. It never prints the value it looked for.
func productionCloudBackupCredentialNoSecret(t *testing.T, body, secret string) {
	t.Helper()
	if strings.Contains(body, secret) {
		t.Fatal("an answer carried the credential secret in cleartext")
	}
	if strings.Contains(body, base64.StdEncoding.EncodeToString([]byte(secret))) {
		t.Fatal("an answer carried the credential secret in base64")
	}
}

// productionCloudBackupCredentialStoreHolds reports whether any file under the
// store root contains a value, so "the secret is not at rest" is a claim about
// every byte the store wrote rather than about one file.
func productionCloudBackupCredentialStoreHolds(t *testing.T, root, value string) bool {
	t.Helper()
	directory := productionCloudBackupStoreDirectory(root)
	found := false
	walkErr := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), value) {
			found = true
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, os.ErrNotExist) {
		t.Fatalf("walk the store root: %v", walkErr)
	}
	return found
}

// productionCloudBackupCredentialView calls cloud-backup:view and returns both the
// raw body (so a leak would be visible) and the decoded answer.
func productionCloudBackupCredentialView(t *testing.T, author *productionDurableClient, session map[string]any) (string, productionCloudBackupViewWire) {
	t.Helper()
	status, body, _ := productionCloudBackupInvoke(t, author, productionCloudBackupViewName, []any{session})
	if status != http.StatusOK {
		t.Fatalf("cloud-backup:view answered HTTP %d: %s", status, body)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || len(envelope.Result) == 0 {
		t.Fatalf("cloud-backup:view answered no result: %v %s", err, body)
	}
	var view productionCloudBackupViewWire
	if err := json.Unmarshal(envelope.Result, &view); err != nil {
		t.Fatalf("cloud-backup:view answered an undecodable result: %v %s", err, body)
	}
	return body, view
}

// TestProductionCloudBackupConnectStoresEncryptedCredential is the connect half:
// a well-formed credential is sealed at rest, the answer reports the account and
// never the secret, and a second connect replaces the first - the reference's
// reselect.
func TestProductionCloudBackupConnectStoresEncryptedCredential(t *testing.T) {
	root, _ := productionDurableDataRoot(t)
	handler := productionCloudBackupCredentialLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)

	// Binding these two names writes nothing: the store root does not exist yet.
	if before := productionCloudBackupCredentialTree(t, root); before != productionCloudBackupCredentialAbsent {
		t.Fatalf("the lane wrote the store before any call: %v", before)
	}

	status, body, result := productionCloudBackupInvoke(t, author, productionCloudBackupConnectName,
		productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint,
			productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret))
	if status != http.StatusOK {
		t.Fatalf("cloud-backup:connect answered HTTP %d: %s", status, body)
	}
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("cloud-backup:connect refused a well-formed credential: %s", body)
	}
	if state, _ := result["state"].(string); state != "connected" {
		t.Fatalf("cloud-backup:connect answered state %v: %s", result["state"], body)
	}
	account, ok := result["account"].(map[string]any)
	if !ok {
		t.Fatalf("cloud-backup:connect answered no account: %s", body)
	}
	if account["accountId"] != productionCloudBackupLocalAccountID {
		t.Fatalf("the stored account is not the reserved local one: %v", account["accountId"])
	}
	if account["persistence"] != productionCloudBackupCredentialPersistence {
		t.Fatalf("the stored account reported persistence %v, not the server-sealed one", account["persistence"])
	}
	if account["endpoint"] != productionCloudBackupCredentialNormalizedEndpoint {
		t.Fatalf("the stored endpoint was not the reference's normalized form: %v", account["endpoint"])
	}
	if account["username"] != productionCloudBackupCredentialUsername {
		t.Fatalf("the stored username was not the one admitted: %v", account["username"])
	}
	if _, warned := result["warning"]; warned {
		t.Fatalf("a server-sealed credential reported a session-only warning: %s", body)
	}
	productionCloudBackupCredentialNoSecret(t, body, productionCloudBackupCredentialSecret)

	// The record on disk: sealed, private, and not the secret.
	store := productionCloudBackupStoreForReadback(t, root)
	record := productionCloudBackupCredentialRecord(t, store)
	productionCloudBackupCredentialNoSecret(t, string(record), productionCloudBackupCredentialSecret)
	if !strings.Contains(string(record), productionCloudBackupCredentialRecordKey) {
		t.Fatalf("the record does not carry the sealed secret field: %s", record)
	}
	if !strings.Contains(string(record), `"persistence":"server-encrypted"`) {
		t.Fatalf("the record does not say how it was sealed: %s", record)
	}
	if productionCloudBackupCredentialStoreHolds(t, root, productionCloudBackupCredentialSecret) {
		t.Fatal("the secret is at rest somewhere under the store root")
	}

	// The one server-side read proves the sealed bytes are the credential. It is
	// never on a wire; this call is in-process and its result is not printed.
	resolved, present, resolveErr := store.ResolveCredential(context.Background(), productionCloudBackupLocalAccountID)
	if resolveErr != nil || !present {
		t.Fatalf("the published record could not be resolved (present=%v): %v", present, resolveErr)
	}
	if resolved.Secret != productionCloudBackupCredentialSecret {
		t.Fatal("the resolved secret is not the one that was stored")
	}
	if resolved.Endpoint != productionCloudBackupCredentialNormalizedEndpoint ||
		resolved.Username != productionCloudBackupCredentialUsername {
		t.Fatalf("the resolved metadata is not the one that was stored: %v %v", resolved.Endpoint, resolved.Username)
	}

	// The reselect: a second connect replaces the first record.
	firstRecord := string(record)
	status, body, result = productionCloudBackupInvoke(t, author, productionCloudBackupConnectName,
		productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint,
			productionCloudBackupCredentialReselectedUsername, productionCloudBackupCredentialReselectedSecret))
	if status != http.StatusOK {
		t.Fatalf("the second cloud-backup:connect answered HTTP %d: %s", status, body)
	}
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("a second connect was refused: %s", body)
	}
	account, ok = result["account"].(map[string]any)
	if !ok || account["username"] != productionCloudBackupCredentialReselectedUsername {
		t.Fatalf("the second connect did not answer the replaced account: %s", body)
	}
	productionCloudBackupCredentialNoSecret(t, body, productionCloudBackupCredentialReselectedSecret)
	replaced := string(productionCloudBackupCredentialRecord(t, store))
	if replaced == firstRecord {
		t.Fatal("the second connect did not replace the record")
	}
	productionCloudBackupCredentialNoSecret(t, replaced, productionCloudBackupCredentialReselectedSecret)
	if productionCloudBackupCredentialStoreHolds(t, root, productionCloudBackupCredentialReselectedSecret) {
		t.Fatal("the replaced secret is at rest somewhere under the store root")
	}
	resolved, present, resolveErr = store.ResolveCredential(context.Background(), productionCloudBackupLocalAccountID)
	if resolveErr != nil || !present || resolved.Secret != productionCloudBackupCredentialReselectedSecret {
		t.Fatalf("the replaced record does not resolve to the second secret (present=%v): %v", present, resolveErr)
	}
}

// TestProductionCloudBackupConnectThenViewSeesCredential is the presence half:
// after connect the served view reports the stored account, and after clear it
// reports the existing session-only account again. Presence only - the secret is
// looked for in every body and never found.
func TestProductionCloudBackupConnectThenViewSeesCredential(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler := productionCloudBackupCredentialLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)

	// The frozen account view must read this change's store; without the one-line
	// seam it answers session-only forever and a stored credential would be
	// invisible. CI formats the source in place, so the seam is asserted as
	// compacted text rather than by line.
	source, readErr := os.ReadFile(productionCloudBackupCredentialViewSource)
	if readErr != nil {
		t.Fatalf("read the served view's source: %v", readErr)
	}
	if !productionSeamContains(string(source), productionCloudBackupCredentialViewSeam) {
		t.Fatalf("cloud-backup:view does not read the credential store: its account field must call productionCloudBackupCredentialAccountView(ctx, b, caller.Principal); %s",
			productionSeamDiagnosis(string(source), productionCloudBackupCredentialViewSeam, 0))
	}

	body, view := productionCloudBackupCredentialView(t, author, session)
	if !view.Success || view.State != "configured" {
		t.Fatalf("cloud-backup:view did not report the derived binding: %s", body)
	}
	if view.Account["persistence"] != productionCloudBackupPersistenceSessionOnly {
		t.Fatalf("cloud-backup:view reported a credential before connect: %v", view.Account["persistence"])
	}
	if view.Account["accountId"] != productionCloudBackupLocalAccountID {
		t.Fatalf("cloud-backup:view reported the wrong account before connect: %v", view.Account["accountId"])
	}

	status, connectBody, result := productionCloudBackupInvoke(t, author, productionCloudBackupConnectName,
		productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint,
			productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret))
	if status != http.StatusOK {
		t.Fatalf("cloud-backup:connect answered HTTP %d: %s", status, connectBody)
	}
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("cloud-backup:connect refused a well-formed credential: %s", connectBody)
	}

	body, view = productionCloudBackupCredentialView(t, author, session)
	if !view.Success || view.State != "configured" {
		t.Fatalf("cloud-backup:view stopped reporting the binding after connect: %s", body)
	}
	if view.Account["persistence"] != productionCloudBackupCredentialPersistence {
		t.Fatalf("cloud-backup:view reported persistence %v after connect, not the stored one", view.Account["persistence"])
	}
	if view.Account["accountId"] != productionCloudBackupLocalAccountID {
		t.Fatalf("cloud-backup:view reported the wrong stored account: %v", view.Account["accountId"])
	}
	if view.Account["endpoint"] != productionCloudBackupCredentialNormalizedEndpoint {
		t.Fatalf("cloud-backup:view did not report the stored endpoint: %v", view.Account["endpoint"])
	}
	if view.Account["username"] != productionCloudBackupCredentialUsername {
		t.Fatalf("cloud-backup:view did not report the stored username: %v", view.Account["username"])
	}
	productionCloudBackupCredentialNoSecret(t, body, productionCloudBackupCredentialSecret)

	status, clearBody, cleared := productionCloudBackupInvoke(t, author, productionCloudBackupClearName,
		[]any{productionCloudBackupLocalAccountID})
	if status != http.StatusOK {
		t.Fatalf("cloud-backup:clear-credential answered HTTP %d: %s", status, clearBody)
	}
	if success, _ := cleared["success"].(bool); !success {
		t.Fatalf("cloud-backup:clear-credential refused a stored credential: %s", clearBody)
	}

	body, view = productionCloudBackupCredentialView(t, author, session)
	if !view.Success || view.State != "configured" {
		t.Fatalf("cloud-backup:view lost the derived binding after clear: %s", body)
	}
	if view.Account["persistence"] != productionCloudBackupPersistenceSessionOnly {
		t.Fatalf("cloud-backup:view still reports a credential after clear: %v", view.Account["persistence"])
	}
	if view.Account["accountId"] != productionCloudBackupLocalAccountID ||
		view.Account["username"] != "production-author" {
		t.Fatalf("cloud-backup:view did not return the existing account after clear: %s", body)
	}
	productionCloudBackupCredentialNoSecret(t, body, productionCloudBackupCredentialSecret)
}

// TestProductionCloudBackupConnectRefusesMalformedCredential is the refusal half:
// every empty, malformed or oversized credential is refused with the browser's
// input code and the store directory is byte-identical afterwards - including a
// first call on a lane that has no store root yet.
func TestProductionCloudBackupConnectRefusesMalformedCredential(t *testing.T) {
	root, _ := productionDurableDataRoot(t)
	handler := productionCloudBackupCredentialLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)

	oversized := strings.Repeat("k", productionCloudBackupCredentialMaxSecretBytes+1)
	longPath := "https://backup.example.invalid/" + strings.Repeat("p", 2048) + "/"
	cases := []struct {
		name string
		args []any
		code string
	}{
		{"empty secret", productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint, productionCloudBackupCredentialUsername, ""), productionCloudBackupInputInvalid},
		{"oversized secret", productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint, productionCloudBackupCredentialUsername, oversized), productionCloudBackupInputInvalid},
		{"empty username", productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint, "", productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"control in username", productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint, "backup\nauthor", productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"oversized username", productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint, strings.Repeat("u", 1025), productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"empty endpoint", productionCloudBackupCredentialConnectArgs("", productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"relative endpoint", productionCloudBackupCredentialConnectArgs("backup.example.invalid/dav/", productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"plain http endpoint", productionCloudBackupCredentialConnectArgs("http://backup.example.invalid/dav/", productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"endpoint userinfo", productionCloudBackupCredentialConnectArgs("https://user:pass@backup.example.invalid/dav/", productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"endpoint query", productionCloudBackupCredentialConnectArgs("https://backup.example.invalid/dav/?a=1", productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"endpoint control", productionCloudBackupCredentialConnectArgs("https://backup.example.invalid/dav/\n", productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"oversized endpoint", productionCloudBackupCredentialConnectArgs(longPath, productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret), productionCloudBackupInputInvalid},
		{"non-string secret", []any{map[string]any{
			"endpoint": productionCloudBackupCredentialEndpoint,
			"username": productionCloudBackupCredentialUsername,
			"secret":   42,
		}}, productionCloudBackupInputInvalid},
		{"unknown field", []any{map[string]any{
			"endpoint": productionCloudBackupCredentialEndpoint,
			"username": productionCloudBackupCredentialUsername,
			"secret":   productionCloudBackupCredentialSecret,
			"path":     "/tmp/backup",
		}}, productionCloudBackupInputInvalid},
		{"second account", []any{map[string]any{
			"accountId": productionCloudBackupTestUUID,
			"endpoint":  productionCloudBackupCredentialEndpoint,
			"username":  productionCloudBackupCredentialUsername,
			"secret":    productionCloudBackupCredentialSecret,
		}}, productionCloudBackupCredentialMissing},
	}

	// A refused FIRST connect must not even create the store root.
	for _, test := range cases {
		t.Run("absent/"+test.name, func(t *testing.T) {
			status, body, result := productionCloudBackupInvoke(t, author, productionCloudBackupConnectName, test.args)
			if status != http.StatusOK {
				t.Fatalf("a refusal answered HTTP %d instead of a business envelope: %s", status, body)
			}
			if success, _ := result["success"].(bool); success {
				t.Fatalf("%s was stored where it had to be refused: %s", test.name, body)
			}
			if code, _ := result["errorCode"].(string); code != test.code {
				t.Fatalf("%s was refused with %v, not %v: %s", test.name, code, test.code, body)
			}
			if after := productionCloudBackupCredentialTree(t, root); after != productionCloudBackupCredentialAbsent {
				t.Fatalf("%s created or changed the store root", test.name)
			}
		})
	}

	// The same refusals against an existing sealed record must leave it identical.
	status, body, result := productionCloudBackupInvoke(t, author, productionCloudBackupConnectName,
		productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint,
			productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret))
	if status != http.StatusOK {
		t.Fatalf("the baseline connect answered HTTP %d: %s", status, body)
	}
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("the baseline connect was refused: %s", body)
	}
	sealed := productionCloudBackupCredentialTree(t, root)
	if sealed == productionCloudBackupCredentialAbsent {
		t.Fatal("the baseline connect wrote no store root")
	}
	for _, test := range cases {
		t.Run("sealed/"+test.name, func(t *testing.T) {
			status, body, result := productionCloudBackupInvoke(t, author, productionCloudBackupConnectName, test.args)
			if status != http.StatusOK {
				t.Fatalf("a refusal answered HTTP %d instead of a business envelope: %s", status, body)
			}
			if success, _ := result["success"].(bool); success {
				t.Fatalf("%s replaced the stored credential: %s", test.name, body)
			}
			if code, _ := result["errorCode"].(string); code != test.code {
				t.Fatalf("%s was refused with %v, not %v: %s", test.name, code, test.code, body)
			}
			if after := productionCloudBackupCredentialTree(t, root); after != sealed {
				t.Fatalf("%s changed the store directory", test.name)
			}
		})
	}
}

// TestProductionCloudBackupClearCredentialRemovesRecord is the clear half: a
// stored credential is removed, clearing again is a success rather than a
// refusal (the reference answers success either way), and a clear this lane
// cannot serve is refused without touching the record.
func TestProductionCloudBackupClearCredentialRemovesRecord(t *testing.T) {
	root, _ := productionDurableDataRoot(t)
	handler := productionCloudBackupCredentialLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)

	// Clearing before anything was stored: success, exactly like the reference.
	status, body, result := productionCloudBackupInvoke(t, author, productionCloudBackupClearName,
		[]any{productionCloudBackupLocalAccountID})
	if status != http.StatusOK {
		t.Fatalf("clearing an empty store answered HTTP %d: %s", status, body)
	}
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("clearing an empty store was refused: %s", body)
	}
	if state, _ := result["state"].(string); state != "unconfigured" {
		t.Fatalf("clearing an empty store answered state %v: %s", result["state"], body)
	}
	if affected, _ := result["affectedBindings"].(float64); affected != 0 {
		t.Fatalf("clearing an empty store answered %v affected bindings", result["affectedBindings"])
	}
	store := productionCloudBackupStoreForReadback(t, root)
	if _, err := os.Lstat(store.CredentialFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clearing an empty store left a record: %v", err)
	}

	status, body, result = productionCloudBackupInvoke(t, author, productionCloudBackupConnectName,
		productionCloudBackupCredentialConnectArgs(productionCloudBackupCredentialEndpoint,
			productionCloudBackupCredentialUsername, productionCloudBackupCredentialSecret))
	if status != http.StatusOK {
		t.Fatalf("cloud-backup:connect answered HTTP %d: %s", status, body)
	}
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("cloud-backup:connect was refused: %s", body)
	}
	sealed := productionCloudBackupCredentialTree(t, root)

	// A clear for an account this lane does not have, a malformed argument and a
	// missing argument are refusals that leave the record byte-identical.
	for _, test := range []struct {
		name string
		args []any
		code string
	}{
		{"another account", []any{"some-other-account"}, productionCloudBackupCredentialMissing},
		{"non-string account", []any{42}, productionCloudBackupCredentialMissing},
		{"no argument", []any{}, productionCloudBackupInputInvalid},
	} {
		t.Run("refused/"+test.name, func(t *testing.T) {
			code := productionCloudBackupRefusal(t, author, productionCloudBackupClearName, test.args)
			if code != test.code {
				t.Fatalf("%s was refused with %v, not %v", test.name, code, test.code)
			}
			if after := productionCloudBackupCredentialTree(t, root); after != sealed {
				t.Fatalf("%s changed the store directory", test.name)
			}
		})
	}

	status, body, result = productionCloudBackupInvoke(t, author, productionCloudBackupClearName,
		[]any{productionCloudBackupLocalAccountID})
	if status != http.StatusOK {
		t.Fatalf("cloud-backup:clear-credential answered HTTP %d: %s", status, body)
	}
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("cloud-backup:clear-credential refused a stored credential: %s", body)
	}
	if state, _ := result["state"].(string); state != "unconfigured" {
		t.Fatalf("cloud-backup:clear-credential answered state %v: %s", result["state"], body)
	}
	if affected, _ := result["affectedBindings"].(float64); affected != 0 {
		t.Fatalf("cloud-backup:clear-credential answered %v affected bindings", result["affectedBindings"])
	}
	productionCloudBackupCredentialNoSecret(t, body, productionCloudBackupCredentialSecret)
	if _, err := os.Lstat(store.CredentialFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cloud-backup:clear-credential left the record: %v", err)
	}
	if productionCloudBackupCredentialStoreHolds(t, root, productionCloudBackupCredentialSecret) {
		t.Fatal("the secret is still at rest somewhere under the store root")
	}
	if _, present, resolveErr := store.ResolveCredential(context.Background(), productionCloudBackupLocalAccountID); present || resolveErr != nil {
		t.Fatalf("the cleared record still resolves (present=%v): %v", present, resolveErr)
	}

	// A second clear is still a success, not a not-found.
	status, body, result = productionCloudBackupInvoke(t, author, productionCloudBackupClearName,
		[]any{productionCloudBackupLocalAccountID})
	if status != http.StatusOK {
		t.Fatalf("clearing twice answered HTTP %d: %s", status, body)
	}
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("clearing twice was refused: %s", body)
	}
}
