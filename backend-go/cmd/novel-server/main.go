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
	file, err := os.Open(*policyPath)
	if err != nil {
		return err
	}
	defer file.Close()
	registry, err := policy.Load(file)
	if err != nil {
		return err
	}
	handler, cleanup, err := migrationHandlerWithUploads(registry, *address, *staticDir, *libraryDir, *uploadDir)
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

// migrationHandler mounts only read-only browser routes proven by the Go
// migration. It shares one cookie store across HTML and API responses. Unported
// project operations retain the Server's explicit 501 response.
func migrationHandler(registry *policy.Registry, address, staticDir, libraryDir string) (http.Handler, func(), error) {
	return migrationHandlerWithUploads(registry, address, staticDir, libraryDir, "")
}

func migrationHandlerWithUploads(registry *policy.Registry, address, staticDir, libraryDir, uploadDir string) (http.Handler, func(), error) {
	if uploadDir != "" && !underTempRoot(uploadDir) {
		return nil, nil, errors.New("synthetic uploads must use a canonical temporary directory")
	}
	server := transport.New(registry, security.NewOrigins("http://"+address), "go-migration-uncommitted")
	if err := server.BindNavigation(); err != nil {
		server.Close()
		return nil, nil, err
	}
	var static *transport.StaticAssets
	var library *projectfs.Library
	var uploads *transferhttp.TransferStore
	cleanup := func() {
		server.Close()
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
