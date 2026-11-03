//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

const completedSyntheticGeneration = "12345678-1234-4234-8234-123456789abc"

func completedSyntheticFixture(t *testing.T) (string, string) {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(parent, "canonical")
	control := filepath.Join(canonical, ".migration")
	dataRoot := filepath.Join(canonical, "generations", completedSyntheticGeneration)
	for _, directory := range []string{control, dataRoot, filepath.Join(parent, "user-data"), filepath.Join(parent, "legacy")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "legacy", "retained.txt"), []byte("synthetic retained data"), 0600); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(control, "receipt.json")
	raw := `{"version":1,"generation":"` + completedSyntheticGeneration + `","completed":true,"preservedUnknownCount":1,"requiredObjects":[]}`
	if err := os.WriteFile(receipt, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return parent, receipt
}

func completedSyntheticClient(t *testing.T, handler http.Handler) func(string, string) *httptest.ResponseRecorder {
	t.Helper()
	bootstrap := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootstrap.Header.Set("X-Novel-User", "synthetic-author")
	boot := httptest.NewRecorder()
	handler.ServeHTTP(boot, bootstrap)
	if boot.Code != http.StatusOK || len(boot.Result().Cookies()) != 1 {
		t.Fatalf("bootstrap failed: %d %s", boot.Code, boot.Body.String())
	}
	var payload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &payload); err != nil || payload.CSRFToken == "" {
		t.Fatalf("bootstrap token: %v", err)
	}
	cookie := boot.Result().Cookies()[0]
	return func(op, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+op, strings.NewReader(body))
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", "client_1234567890")
		request.Header.Set("X-CSRF-Token", payload.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
}

func TestCompletedGlobalSyntheticBindsRealGenerationAndHoldsLegacyLock(t *testing.T) {
	parent, receipt := completedSyntheticFixture(t)
	handler, cleanup, err := migrationHandlerWithCompletedGlobal(migrationRegistry(t), "127.0.0.1:16082", "", parent, []string{"https://allowed.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := writerlock.AcquireLegacy(parent); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("legacy writer entered while Go handler active: %v", err)
	}
	rpc := completedSyntheticClient(t, handler)
	state := rpc("startup:get-state", `{"args":[]}`)
	if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), completedSyntheticGeneration) ||
		!strings.Contains(state.Body.String(), `"preservedUnknownCount":1`) ||
		!strings.Contains(state.Body.String(), `"legacySourceIgnored":true`) ||
		strings.Contains(state.Body.String(), "synthetic-model-config-v1") {
		t.Fatalf("startup did not use receipt: %d %s", state.Code, state.Body.String())
	}
	config := rpc("config:get", `{"args":[]}`)
	if config.Code != http.StatusOK {
		t.Fatalf("config read denied: %d %s", config.Code, config.Body.String())
	}
	saved := rpc("config:set", `{"args":[{"theme":"dark"}]}`)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"success":true`) {
		t.Fatalf("verified generation config write denied: %d %s", saved.Code, saved.Body.String())
	}
	if _, err := os.Stat(filepath.Join(parent, "canonical", "generations", completedSyntheticGeneration, "config.json")); err != nil {
		t.Fatalf("config write missed verified generation: %v", err)
	}
	// The original migration validates recognized JSON on startup. A later
	// config patch can change it; request fencing must keep checking identity
	// without inventing a second migration admission on every request.
	saved = rpc("config:set", `{"args":[{"defaultModelId":"missing-synthetic-model"}]}`)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"success":true`) {
		t.Fatalf("post-startup config patch denied: %d %s", saved.Code, saved.Body.String())
	}
	state = rpc("startup:get-state", `{"args":[]}`)
	if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), completedSyntheticGeneration) {
		t.Fatalf("identity fence rejected post-startup config patch: %d %s", state.Code, state.Body.String())
	}
	raw, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	replacement := receipt + ".replacement"
	if err := os.WriteFile(replacement, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, receipt); err != nil {
		t.Fatal(err)
	}
	blocked := rpc("config:set", `{"args":[{"theme":"light"}]}`)
	if blocked.Code != http.StatusServiceUnavailable || !strings.Contains(blocked.Body.String(), "GLOBAL_DATA_BLOCKED") {
		t.Fatalf("replaced receipt retained request authority: %d %s", blocked.Code, blocked.Body.String())
	}
	cleanup()
	lock, err := writerlock.AcquireLegacy(parent)
	if err != nil {
		t.Fatalf("cleanup retained legacy lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedGlobalSyntheticRejectsAbsentReceiptAndPublicRoot(t *testing.T) {
	parent, receipt := completedSyntheticFixture(t)
	if err := os.Remove(receipt); err != nil {
		t.Fatal(err)
	}
	if _, cleanup, err := migrationHandlerWithCompletedGlobal(migrationRegistry(t), "127.0.0.1:16082", "", parent, []string{"https://allowed.example"}); err == nil {
		cleanup()
		t.Fatal("unreceipted synthetic generation admitted")
	}
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	if _, cleanup, err := migrationHandlerWithCompletedGlobal(migrationRegistry(t), "127.0.0.1:16082", "", parent, []string{"https://allowed.example"}); err == nil {
		cleanup()
		t.Fatal("public synthetic root admitted")
	}
}

func TestCompletedGlobalSyntheticCleanupCancelsSSE(t *testing.T) {
	parent, _ := completedSyntheticFixture(t)
	handler, cleanup, err := migrationHandlerWithCompletedGlobal(migrationRegistry(t), "127.0.0.1:16082", "", parent, []string{"https://allowed.example"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	defer cleanup()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/bootstrap", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Novel-User", "synthetic-author")
	bootstrap, err := server.Client().Do(request)
	if err != nil || bootstrap.StatusCode != http.StatusOK || len(bootstrap.Cookies()) != 1 {
		t.Fatalf("bootstrap: response=%v err=%v", bootstrap, err)
	}
	cookie := bootstrap.Cookies()[0]
	_ = bootstrap.Body.Close()
	request, err = http.NewRequest(http.MethodGet, server.URL+"/api/events?client=synthetic-tab-0001", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Novel-User", "synthetic-author")
	request.AddCookie(cookie)
	events, err := server.Client().Do(request)
	if err != nil || events.StatusCode != http.StatusOK {
		t.Fatalf("SSE: response=%v err=%v", events, err)
	}
	defer events.Body.Close()
	first := make([]byte, len(": connected\n\n"))
	if _, err := io.ReadFull(events.Body, first); err != nil || !bytes.Equal(first, []byte(": connected\n\n")) {
		t.Fatalf("SSE greeting: %q %v", first, err)
	}
	done := make(chan struct{})
	go func() { cleanup(); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("SSE prevented bounded cleanup")
	}
	lock, err := writerlock.AcquireLegacy(parent)
	if err != nil {
		t.Fatalf("cleanup retained legacy lock after SSE cancellation: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedGlobalSyntheticCleanupRetainsLockForIgnoringHandler(t *testing.T) {
	parent, _ := completedSyntheticFixture(t)
	lock, err := writerlock.AcquireLegacy(parent)
	if err != nil {
		t.Fatal(err)
	}
	roots := globaladmission.Roots{LegacySource: filepath.Join(parent, "legacy"), CanonicalTarget: filepath.Join(parent, "canonical"), UserData: filepath.Join(parent, "user-data")}
	pinned, err := globaladmission.PinCompleted(context.Background(), roots, true)
	if err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	ignored := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(started); <-release })
	guarded, cleanup := guardCompletedGlobal(ignored, func() {}, lock, pinned, parent)
	requestDone := make(chan struct{})
	go func() {
		guarded.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
		close(requestDone)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("ignoring handler never started")
	}
	cleanupDone := make(chan struct{})
	go func() { cleanup(); close(cleanupDone) }()
	select {
	case <-cleanupDone:
	case <-time.After(4 * time.Second):
		t.Fatal("ignoring handler blocked cleanup indefinitely")
	}
	if _, err := writerlock.AcquireLegacy(parent); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("unsafe lock release while handler active: %v", err)
	}
	close(release)
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ignoring handler did not finish")
	}
}
