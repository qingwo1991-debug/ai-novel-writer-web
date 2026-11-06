//go:build linux

package main

// This file proves the success path of cloud-backup:cancel. Until now every
// assertion on the name was a refusal - not running, an invalid id, an id smuggled
// into another operation - so neither the {"success":true,"cancelled":true} answer
// (production_cloud_backup_linux.go:683-699) nor the effect of the cancel func b.run
// registered (production_cloud_backup_linux.go:705-728) had ever been observed.
//
// The in-flight operation is a real cloud-backup:backup over real HTTP, held at the
// first statement of its operation body. That statement is b.lane()
// (production_cloud_backup_linux.go:403), which takes the open lane's own mutex, so a
// backup that cannot take it has already registered its operation id and has
// captured nothing. The run is released only after the cancel has answered, so
// whatever it does next is decided by an operation context that is already cancelled:
// a run that ignored the cancel would publish a generation and report success, which
// the assertions below catch.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/cloudbackup"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionCloudBackupCancelTestWait bounds the two facts that settle on the
	// backup's own goroutine: its operation id appearing in the registry, and its
	// answer arriving after the cancel.
	productionCloudBackupCancelTestWait = 15 * time.Second
)

// productionCloudBackupCancelLane builds the lane productionCloudBackupLane builds
// and hands back the binder and the open runtime as well, because this test has to
// block the run inside the lane and then read the operation registry the cancel
// handler reads.
func productionCloudBackupCancelLane(t *testing.T, root string) (http.Handler, *productionCloudBackup, *productionOpen) {
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
		"go-production-cloud-backup")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	backup := newProductionCloudBackup(open, config)
	if bindErr := backup.Bind(server); bindErr != nil {
		t.Fatal(bindErr)
	}
	return server, backup, open
}

// productionCloudBackupCancelRunning reports whether one operation id is registered
// as in flight, read under the same lock b.run and cancel use.
func productionCloudBackupCancelRunning(backup *productionCloudBackup, operationID string) bool {
	backup.mu.Lock()
	defer backup.mu.Unlock()
	_, running := backup.operations[operationID]
	return running
}

// productionCloudBackupCancelRegistered reports a channel closed once b.run has
// registered one operation id. It observes a fact that settles on the backup's own
// goroutine; the window is bounded, so a run that never starts fails the test here
// instead of hanging it.
func productionCloudBackupCancelRegistered(backup *productionCloudBackup, operationID string) <-chan struct{} {
	registered := make(chan struct{})
	go func() {
		deadline := time.Now().Add(productionCloudBackupCancelTestWait)
		for !productionCloudBackupCancelRunning(backup, operationID) {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		close(registered)
	}()
	return registered
}

// productionCloudBackupCancelPublishedFiles names every regular file under the store
// root, so "nothing was published" is asserted over the tree rather than over one
// guessed path: a published generation always writes its archive, manifest and
// completion there.
func productionCloudBackupCancelPublishedFiles(t *testing.T, storeRoot string) []string {
	t.Helper()
	files := make([]string, 0, 8)
	if _, statErr := os.Lstat(storeRoot); errors.Is(statErr, os.ErrNotExist) {
		return files
	} else if statErr != nil {
		t.Fatalf("stat the store root: %v", statErr)
	}
	walkErr := filepath.WalkDir(storeRoot, func(path string, entry fs.DirEntry, scanErr error) error {
		if scanErr != nil {
			return scanErr
		}
		if entry.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk the store root: %v", walkErr)
	}
	return files
}

// productionCloudBackupCancelAnswer decodes one cloud-backup answer body into the
// fields the cancellation assertions need.
func productionCloudBackupCancelAnswer(t *testing.T, body string) map[string]any {
	t.Helper()
	var envelope struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Result == nil {
		t.Fatalf("the answer is not the operation envelope: %v %s", err, body)
	}
	return envelope.Result
}

// TestProductionCloudBackupCancelStopsAnInFlightBackup is the success path: a backup
// is running, the cancel names its operation id and answers truthfully, the run
// stops without publishing anything, and the same cancel is refused once the run is
// gone.
func TestProductionCloudBackupCancelStopsAnInFlightBackup(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler, backup, open := productionCloudBackupCancelLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)
	before := productionCloudBackupDatabaseDigest(t, projectPath)

	// The lane lock is the one place a served backup waits on real state, and b.lane()
	// is the first statement of the operation body, so the run is in flight and has
	// captured nothing while the test holds it.
	open.mu.Lock()
	released := false
	release := func() {
		if !released {
			released = true
			open.mu.Unlock()
		}
	}
	defer release()

	answers := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answers <- author.call(t, productionCloudBackupBackupName, []any{map[string]any{
			"operationId":         productionCloudBackupTestUUID,
			"projectSession":      session,
			"disclosureConfirmed": true,
		}})
	}()
	registered := productionCloudBackupCancelRegistered(backup, productionCloudBackupTestUUID)
	select {
	case <-registered:
	case early := <-answers:
		t.Fatalf("cloud-backup:backup answered before it registered an operation: %s", early.Body.String())
	case <-time.After(productionCloudBackupCancelTestWait):
		t.Fatal("waited for cloud-backup:backup to register its operation id: the run never became in flight")
	}

	status, body, result := productionCloudBackupInvoke(t, author, productionCloudBackupCancelName,
		[]any{productionCloudBackupTestUUID})
	if status != http.StatusOK {
		t.Fatalf("cloud-backup:cancel answered HTTP %d instead of a business envelope: %s", status, body)
	}
	if cancelled, _ := result["cancelled"].(bool); !cancelled {
		t.Fatalf("cloud-backup:cancel did not report the running operation as cancelled: %s", body)
	}
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("cloud-backup:cancel refused the one operation that was running: %s", body)
	}

	// Release the run: its operation context is cancelled by now, so it must refuse
	// instead of publishing the generation it was about to build.
	release()

	var answer *httptest.ResponseRecorder
	select {
	case answer = <-answers:
	case <-time.After(productionCloudBackupCancelTestWait):
		t.Fatal("waited for the cancelled backup to answer: the run did not stop")
	}
	if answer.Code != http.StatusOK {
		t.Fatalf("the cancelled backup answered HTTP %d: %s", answer.Code, answer.Body.String())
	}
	stopped := productionCloudBackupCancelAnswer(t, answer.Body.String())
	if success, _ := stopped["success"].(bool); success {
		t.Fatalf("a backup cancelled mid-run still reported success: %s", answer.Body.String())
	}
	if state, _ := stopped["state"].(string); state != "failed" {
		t.Fatalf("a cancelled backup reported state %v instead of failed: %s", state, answer.Body.String())
	}
	if code, _ := stopped["errorCode"].(string); code == "" {
		t.Fatalf("a cancelled backup refused without a symbolic code: %s", answer.Body.String())
	}
	if operationID, _ := stopped["operationId"].(string); operationID != productionCloudBackupTestUUID {
		t.Fatalf("a cancelled backup answered the operation id %v it was not given: %s", operationID, answer.Body.String())
	}

	// Nothing was published: no catalogue entry, no generation directory, no archive,
	// manifest or completion anywhere under the store.
	store := productionCloudBackupStoreForReadback(t, root)
	generations, listErr := store.List(context.Background(), productionDurableProjectID)
	switch {
	case listErr == nil && len(generations) != 0:
		t.Fatalf("a cancelled backup published %d generations", len(generations))
	case listErr != nil && !errors.Is(listErr, cloudbackup.ErrNotFound):
		t.Fatalf("the store could not be listed after a cancelled backup: %v", listErr)
	}
	if published := productionCloudBackupCancelPublishedFiles(t, productionCloudBackupStoreDirectory(root)); len(published) != 0 {
		t.Fatalf("a cancelled backup left %d files under the store, first %v", len(published), published[0])
	}

	// The run is gone, so the registry holds nothing and the same cancel is now the id
	// this process never saw.
	if productionCloudBackupCancelRunning(backup, productionCloudBackupTestUUID) {
		t.Fatal("the finished operation is still registered after its run returned")
	}
	finished := []any{productionCloudBackupTestUUID}
	if code := productionCloudBackupRefusal(t, author, productionCloudBackupCancelName, finished); code != productionCloudBackupOperationNotFound {
		t.Fatalf("cancelling a finished operation answered %v instead of the not-found code", code)
	}

	// A cancelled backup only reads the author's project.
	if after := productionCloudBackupDatabaseDigest(t, projectPath); after != before {
		t.Fatalf("a cancelled backup changed the author database: %v -> %v", before, after)
	}
}
