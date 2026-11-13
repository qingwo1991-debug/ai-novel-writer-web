package webdav

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestEveryProtocolRequestRequiresFreshAuthority(t *testing.T) {
	f := newFixture(t)
	authorized := 0
	dispatched := 0
	s, err := New(Options{
		Authorize: func(ctx context.Context, u *url.URL) error {
			authorized++
			if u.Scheme+"://"+u.Host != f.server.URL {
				t.Error("wrong origin")
			}
			return nil
		},
		HTTPDispatch: func(r *http.Request) (*http.Response, error) {
			dispatched++
			if authorized != dispatched {
				t.Error("dispatch without fresh authority")
			}
			return f.server.Client().Transport.RoundTrip(r)
		},
		GenerationIDFactory: func() string { return testA },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendFixture(t, s, f, context.Background(), []byte("PUBLIC")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListGenerations(context.Background(), f.account(), testBook); err != nil {
		t.Fatal(err)
	}
	if authorized != dispatched || dispatched < 15 {
		t.Fatalf("authority %d dispatch %d", authorized, dispatched)
	}
}
func TestArchiveTransferDeadlineAndCancelledRead(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancel"}[cancelled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "MKCOL" {
					w.WriteHeader(201)
					return
				}
				if r.Method == "GET" {
					w.WriteHeader(404)
					return
				}
				if r.Method == "PUT" {
					_, _ = io.Copy(io.Discard, r.Body)
					select {
					case started <- struct{}{}:
					default:
					}
					select {
					case <-r.Context().Done():
					case <-time.After(250 * time.Millisecond):
					}
					return
				}
				w.WriteHeader(405)
			}))
			defer server.Close()
			s, err := New(Options{HTTPDispatch: server.Client().Transport.RoundTrip, Authorize: func(context.Context, *url.URL) error { return nil }, GenerationIDFactory: func() string { return testA }, TransferTimeout: 40 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if cancelled {
				go func() {
					select {
					case <-started:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			b := []byte("PUBLIC")
			_, err = s.AppendGeneration(ctx, AppendInput{Account: Account{server.URL + "/dav/", "writer", "PUBLIC"}, CloudBookID: "book", Archive: strings.NewReader(string(b)), ArchiveSHA256: digestBytes(b), ArchiveByteSize: int64(len(b)), OriginProjectID: testProject, PortableSnapshotGeneration: "portable"})
			if cancelled && err != Cancelled {
				t.Fatalf("cancel %v", err)
			}
			if !cancelled && err == nil {
				t.Fatal("stalled upload succeeded")
			}
		})
	}
}
func TestHostileDispatcherNeverLeaksRemoteErrors(t *testing.T) {
	for _, mode := range []string{"error", "followed", "compressed", "oversized", "lying-length", "duplicate-length"} {
		t.Run(mode, func(t *testing.T) {
			closed := false
			s, _ := New(Options{Authorize: func(context.Context, *url.URL) error { return nil }, HTTPDispatch: func(r *http.Request) (*http.Response, error) {
				if mode == "error" {
					return nil, errors.New("https://user:PUBLIC-secret@evil.invalid")
				}
				h := http.Header{}
				response := &http.Response{StatusCode: 207, Header: h, ContentLength: -1, Body: &closeObserver{Reader: strings.NewReader("PUBLIC"), closed: &closed}}
				switch mode {
				case "followed":
					response.Request = &http.Request{URL: &url.URL{Scheme: "https", Host: "evil.invalid", Path: "/"}}
				case "compressed":
					h.Set("Content-Encoding", "gzip")
				case "oversized":
					h.Set("Content-Length", "1048577")
				case "lying-length":
					h.Set("Content-Length", "7")
				case "duplicate-length":
					h["Content-Length"] = []string{"6", "6"}
				}
				return response, nil
			}})
			err := s.CheckConnection(context.Background(), Account{"https://example.invalid/", "writer", "PUBLIC"})
			if err == nil || strings.Contains(err.Error(), "PUBLIC") || strings.Contains(err.Error(), "://") {
				t.Fatalf("unsafe remote error %v", err)
			}
			if mode != "error" && !closed {
				t.Fatal("response body leaked")
			}
		})
	}
}

type closeObserver struct {
	io.Reader
	closed *bool
}

func (c *closeObserver) Close() error { *c.closed = true; return nil }
