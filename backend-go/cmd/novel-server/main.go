// novel-server is the in-progress Go backend, not yet a production replacement.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transferhttp"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	address := flag.String("listen", "127.0.0.1:16082", "migration-only loopback address")
	policyPath := flag.String("policy", "../web/operations.json", "reviewed browser operation inventory")
	staticDir := flag.String("static-dir", "", "optional React build for isolated migration smoke")
	libraryDir := flag.String("synthetic-library-dir", "", "optional disposable library for isolated migration smoke")
	uploadDir := flag.String("synthetic-upload-dir", "", "optional private disposable upload staging for isolated migration smoke")
	globalDir := flag.String("synthetic-global-dir", "", "optional private disposable global config for isolated migration smoke")
	modelOrigins := flag.String("synthetic-model-origins", "", "comma-separated trusted HTTP(S) origins for synthetic model saves")
	readSchema := flag.String("synthetic-read-schema-contract", "", "trusted SQLite contract for disposable read-only project opens")
	flag.Parse()
	if os.Getenv("NODE_ENV") == "production" || os.Getenv("NOVEL_DATA_ROOT") != "" {
		return errors.New("migration scaffold refuses production mode and data mounts")
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("migration scaffold may listen only on a loopback IP")
	}
	if *libraryDir != "" && !underTempRoot(*libraryDir) {
		return errors.New("synthetic library must be a canonical temporary directory")
	}
	if *uploadDir != "" && !underTempRoot(*uploadDir) {
		return errors.New("synthetic uploads must use a canonical temporary directory")
	}
	if *globalDir != "" && !privateSyntheticGlobalRoot(*globalDir) {
		return errors.New("synthetic global config must use a private canonical temporary directory")
	}
	if (*globalDir == "") != (*modelOrigins == "") {
		return errors.New("synthetic model config requires both a private global root and trusted origins")
	}
	if *readSchema != "" && (*libraryDir == "" || !privateSyntheticGlobalRoot(filepath.Dir(*libraryDir))) {
		return errors.New("synthetic project reads require a library beneath a private temporary data root")
	}
	file, err := os.Open(*policyPath)
	if err != nil {
		return err
	}
	defer file.Close()
	registry, err := policy.Load(file)
	if err != nil {
		return err
	}
	var origins []string
	if *modelOrigins != "" {
		origins = strings.Split(*modelOrigins, ",")
	}
	var readConfig *syntheticReadOpenConfig
	if *readSchema != "" {
		contractFile, err := os.Open(*readSchema)
		if err != nil {
			return err
		}
		contract, parseErr := admission.ParseContract(contractFile)
		if closeErr := contractFile.Close(); parseErr == nil {
			parseErr = closeErr
		}
		if parseErr != nil {
			return fmt.Errorf("synthetic read contract: %w", parseErr)
		}
		readConfig = &syntheticReadOpenConfig{Schema: contract}
	}
	handler, cleanup, err := migrationHandlerWithModelsAndReadOpen(registry, *address, *staticDir, *libraryDir, *uploadDir, *globalDir, origins, readConfig)
	if err != nil {
		return err
	}
	defer cleanup()
	server := &http.Server{Addr: *address, Handler: handler, ReadHeaderTimeout: 30 * time.Second, ReadTimeout: 16 * time.Minute, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1024 * 1024}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Printf("Go migration scaffold at %s; no author writes; no Node fallback; not production-ready", *address)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		return server.Shutdown(shutdown)
	}
}

func underTempRoot(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	rel, err := filepath.Rel(os.TempDir(), path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	// Reject a synthetic root whose intermediate component redirects outside
	// the temporary tree. The final root is checked again by its owner.
	current := os.TempDir()
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func privateSyntheticGlobalRoot(path string) bool {
	if !underTempRoot(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// migrationHandler mounts only explicitly admitted migration routes. It shares
// one cookie store across HTML and API responses. Unported operations retain
// the Server's explicit 501 response.
func migrationHandler(registry *policy.Registry, address, staticDir, libraryDir string) (http.Handler, func(), error) {
	return migrationHandlerWithUploads(registry, address, staticDir, libraryDir, "")
}

func migrationHandlerWithUploads(registry *policy.Registry, address, staticDir, libraryDir, uploadDir string) (http.Handler, func(), error) {
	return migrationHandlerWithModels(registry, address, staticDir, libraryDir, uploadDir, "", nil)
}

// migrationHandlerWithModels admits only a disposable private global root and
// administrator supplied exact origins. This synthetic startup capture must
// never be used as evidence that an author's real data has been migrated.
func migrationHandlerWithModels(registry *policy.Registry, address, staticDir, libraryDir, uploadDir, globalDir string, modelOrigins []string) (http.Handler, func(), error) {
	return migrationHandlerWithModelsAndReadOpen(registry, address, staticDir, libraryDir, uploadDir, globalDir, modelOrigins, nil)
}

func migrationHandlerWithModelsAndReadOpen(registry *policy.Registry, address, staticDir, libraryDir, uploadDir, globalDir string, modelOrigins []string, readConfig *syntheticReadOpenConfig) (http.Handler, func(), error) {
	if readConfig != nil && (libraryDir == "" || !underTempRoot(libraryDir) || !privateSyntheticGlobalRoot(filepath.Dir(libraryDir))) {
		return nil, nil, errors.New("synthetic project reads require a library beneath a private temporary data root")
	}
	if uploadDir != "" && !underTempRoot(uploadDir) {
		return nil, nil, errors.New("synthetic uploads must use a canonical temporary directory")
	}
	if globalDir != "" && !privateSyntheticGlobalRoot(globalDir) {
		return nil, nil, errors.New("synthetic global config must use a private canonical temporary directory")
	}
	if (globalDir == "") != (len(modelOrigins) == 0) {
		return nil, nil, errors.New("synthetic model config requires both a private global root and trusted origins")
	}
	server := transport.New(registry, security.NewOrigins("http://"+address), "go-migration-uncommitted")
	if err := server.BindNavigation(); err != nil {
		server.Close()
		return nil, nil, err
	}
	var static *transport.StaticAssets
	var library *projectfs.Library
	var uploads *transferhttp.TransferStore
	var models *globalconfig.Service
	var endpoints *egress.Client
	var readOpen *syntheticReadOpen
	cleanup := func() {
		server.Close()
		if readOpen != nil {
			_ = readOpen.Close()
		}
		if endpoints != nil {
			endpoints.Close()
		}
		if models != nil {
			_ = models.Close()
		}
		if uploads != nil {
			_ = uploads.Close()
		}
		if static != nil {
			_ = static.Close()
		}
		if library != nil {
			_ = library.Close()
		}
	}
	if globalDir != "" {
		var err error
		models, err = globalconfig.New(globalDir)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("synthetic global config: %w", err)
		}
		endpoints, err = egress.New(modelOrigins)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("synthetic model origins: %w", err)
		}
		startup := globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "synthetic-model-config-v1"})
		for _, bind := range []func() error{
			func() error { return server.BindGlobalConfig(models, startup) },
			func() error { return server.BindModelReads(models, startup) },
			func() error { return server.BindModelSave(models, startup, endpoints) },
			func() error { return server.BindModelDelete(models, startup) },
		} {
			if err := bind(); err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("synthetic model binding: %w", err)
			}
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	if libraryDir != "" {
		var err error
		library, err = projectfs.OpenLibrary(libraryDir)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("synthetic library: %w", err)
		}
		route, err := transport.NewLibraryRoute(server, library, 4096)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		mux.Handle("/api/library", route)
		if readConfig != nil {
			readOpen, err = newSyntheticReadOpen(library, libraryDir, *readConfig)
			if err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("synthetic project read: %w", err)
			}
			if err := server.BindProjectReadOpen(readOpen); err != nil {
				cleanup()
				return nil, nil, err
			}
			if err := server.BindCharacterLiveReads(readOpen); err != nil {
				cleanup()
				return nil, nil, err
			}
			if err := server.BindDraftLiveReads(readOpen); err != nil {
				cleanup()
				return nil, nil, err
			}
			if err := server.BindProjectReadClose(readOpen); err != nil {
				cleanup()
				return nil, nil, err
			}
			if err := server.BindEditorNavigation(readOpen); err != nil {
				cleanup()
				return nil, nil, err
			}
			if err := server.BindEditorStageReads(readOpen); err != nil {
				cleanup()
				return nil, nil, err
			}
			if err := server.BindEditorReviewReads(readOpen); err != nil {
				cleanup()
				return nil, nil, err
			}
		}
	}
	if uploadDir != "" {
		var err error
		uploads, err = transferhttp.NewTransferStore(uploadDir, server.SessionStore(), security.NewOrigins("http://"+address), server.ResolveTransferClient)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("synthetic uploads: %w", err)
		}
		mux.Handle("/api/upload-folders", uploads)
		mux.Handle("/api/uploads", uploads)
	}
	if staticDir != "" {
		var err error
		static, err = transport.NewStaticAssets(staticDir, server.SessionStore())
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("static assets: %w", err)
		}
		mux.Handle("/", static)
	} else {
		mux.Handle("/", server)
	}
	return mux, cleanup, nil
}
