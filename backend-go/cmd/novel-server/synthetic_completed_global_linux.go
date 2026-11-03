//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// This lane accepts only an administrator-supplied private temporary parent.
// The parent determines all three roots; no request or separate flag chooses a
// migration source. It does not enable production data mounts or project data.
func migrationHandlerWithCompletedGlobal(registry *policy.Registry, address, staticDir, parent string, origins []string) (http.Handler, func(), error) {
	if !privateSyntheticGlobalRoot(parent) || len(origins) == 0 {
		return nil, nil, errors.New("completed global synthetic lane requires private temporary roots and trusted origins")
	}
	info, err := os.Lstat(parent)
	if err != nil || info.Mode().Perm() != 0700 {
		return nil, nil, errors.New("completed global synthetic parent must be private")
	}
	roots := globaladmission.Roots{
		LegacySource:    filepath.Join(parent, "legacy"),
		CanonicalTarget: filepath.Join(parent, "canonical"),
		UserData:        filepath.Join(parent, "user-data"),
	}
	lock, err := writerlock.AcquireLegacy(parent)
	if err != nil {
		return nil, nil, err
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = lock.Close()
		}
	}()
	if err := lock.VerifyLegacy(parent); err != nil {
		return nil, nil, err
	}
	pinned, err := globaladmission.PinCompleted(context.Background(), roots, true)
	if err != nil {
		return nil, nil, err
	}
	closePinned := true
	defer func() {
		if closePinned {
			_ = pinned.Close()
		}
	}()
	ready, err := pinned.Verify(context.Background())
	if err != nil {
		return nil, nil, err
	}
	capture := &globalconfig.StartupCapture{Generation: ready.GlobalGeneration,
		MigrationNotice: &globalconfig.MigrationNotice{
			LegacySourceIgnored:   ready.LegacySourceIgnored,
			PreservedUnknownCount: ready.PreservedUnknownCount,
		}}
	handler, handlerCleanup, err := migrationHandlerWithModelsReadOpenAndCapture(registry, address, staticDir, "", "", ready.DataRoot, origins, nil, capture)
	if err != nil {
		return nil, nil, err
	}
	closePinned, closeLock = false, false
	guarded, cleanup := guardCompletedGlobal(handler, handlerCleanup, lock, pinned, parent)
	return guarded, cleanup, nil
}

func guardCompletedGlobal(handler http.Handler, handlerCleanup func(), lock *writerlock.Lock, pinned *globaladmission.PinnedReady, parent string) (http.Handler, func()) {
	var gate sync.RWMutex
	var closing atomic.Bool
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			// Reject new work before cancellation; then give context-aware SSE
			// and ordinary requests a bounded chance to leave the data lane.
			closing.Store(true)
			cancelLifetime()
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				if gate.TryLock() {
					// No request can still be using the generation.
					handlerCleanup()
					_ = pinned.Close()
					_ = lock.Close()
					gate.Unlock()
					return
				}
				select {
				case <-tick.C:
				case <-deadline.C:
					// A non-cooperative handler may still hold a generation handle.
					// Keep the writer lock and those resources until process exit.
					if lock.Retain() != nil {
						// Retention failure must not release a lock while a handler
						// is still active. Wait and close only after the gate drains.
						gate.Lock()
						handlerCleanup()
						_ = pinned.Close()
						_ = lock.Close()
						gate.Unlock()
					}
					return
				}
			}
		})
	}
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if closing.Load() {
			http.Error(w, "GLOBAL_DATA_BLOCKED", http.StatusServiceUnavailable)
			return
		}
		gate.RLock()
		defer gate.RUnlock()
		if closing.Load() || lock.VerifyLegacy(parent) != nil {
			http.Error(w, "GLOBAL_DATA_BLOCKED", http.StatusServiceUnavailable)
			return
		}
		// Migration semantics are established at startup. Later config writes
		// may change recognized JSON; request fencing must protect the pinned
		// receipt and generation identity without re-running startup admission.
		if _, err := pinned.VerifyIdentity(r.Context()); err != nil {
			http.Error(w, "GLOBAL_DATA_BLOCKED", http.StatusServiceUnavailable)
			return
		}
		if closing.Load() {
			http.Error(w, "GLOBAL_DATA_BLOCKED", http.StatusServiceUnavailable)
			return
		}
		requestContext, cancelRequest := context.WithCancel(r.Context())
		stop := context.AfterFunc(lifetime, cancelRequest)
		defer stop()
		defer cancelRequest()
		handler.ServeHTTP(w, r.WithContext(requestContext))
	})
	return guarded, cleanup
}
