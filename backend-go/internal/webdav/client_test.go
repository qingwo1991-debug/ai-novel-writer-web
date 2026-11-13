package webdav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testBook = "book:@+"
const testProject = "11111111-1111-4111-8111-111111111111"
const testA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const testB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

type fixture struct {
	mu           sync.Mutex
	files        map[string][]byte
	puts         []string
	requests     []string
	server       *httptest.Server
	xml          string
	dropSuffix   string
	corrupt      bool
	cancelSuffix string
	cancel       context.CancelFunc
	redirect     string
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{files: map[string][]byte{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.EscapedPath())
		if r.Header.Get("Authorization") == "" {
			t.Error("missing credentials")
		}
		if f.redirect != "" {
			w.Header().Set("Location", f.redirect)
			w.WriteHeader(302)
			return
		}
		switch r.Method {
		case "MKCOL":
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("unconditional MKCOL")
			}
			w.WriteHeader(201)
		case "PUT":
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("unconditional overwrite")
			}
			f.puts = append(f.puts, r.URL.EscapedPath())
			b, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			if _, ok := f.files[r.URL.EscapedPath()]; ok {
				w.WriteHeader(412)
				return
			}
			if f.corrupt && strings.HasSuffix(r.URL.Path, f.dropSuffix) && len(b) > 0 {
				b[len(b)-1] ^= 1
			}
			f.files[r.URL.EscapedPath()] = b
			if strings.HasSuffix(r.URL.Path, f.cancelSuffix) && f.cancelSuffix != "" && f.cancel != nil {
				f.cancel()
			}
			if f.dropSuffix != "" && strings.HasSuffix(r.URL.Path, f.dropSuffix) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			w.WriteHeader(201)
		case "GET":
			b, ok := f.files[r.URL.EscapedPath()]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			_, _ = w.Write(b)
		case "PROPFIND":
			body := f.xml
			if body == "" {
				body = `<d:multistatus xmlns:d="DAV:"><d:response><d:href>` + r.URL.EscapedPath() + `</d:href></d:response>`
				for _, id := range []string{testA, testB} {
					body += `<d:response><d:href>` + r.URL.EscapedPath() + id + `/</d:href></d:response>`
				}
				body += `</d:multistatus>`
			}
			w.WriteHeader(207)
			_, _ = io.WriteString(w, body)
		default:
			w.WriteHeader(405)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *fixture) account() Account {
	return Account{f.server.URL + "/dav/", "writer", "PUBLIC-synthetic-secret"}
}
func serviceFor(t *testing.T, f *fixture, id string) *Service {
	t.Helper()
	s, err := New(Options{HTTPDispatch: f.server.Client().Transport.RoundTrip, Authorize: func(_ context.Context, u *url.URL) error {
		if u.Scheme+"://"+u.Host != f.server.URL {
			return errors.New("denied")
		}
		return nil
	}, GenerationIDFactory: func() string { return id }, Now: func() time.Time { return time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func appendFixture(t *testing.T, s *Service, f *fixture, ctx context.Context, b []byte) (Generation, error) {
	t.Helper()
	return s.AppendGeneration(ctx, AppendInput{Account: f.account(), CloudBookID: testBook, Archive: bytes.NewReader(b), ArchiveSHA256: digestBytes(b), ArchiveByteSize: int64(len(b)), OriginProjectID: testProject, PortableSnapshotGeneration: "portable-a", ParentGenerationIDs: []string{"parent-b", "parent-a"}})
}
func TestAppendListAndInternalStream(t *testing.T) {
	f := newFixture(t)
	s := serviceFor(t, f, testA)
	b := []byte("PUBLIC 合成\x00😀 archive")
	if _, err := appendFixture(t, s, f, context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := appendFixture(t, serviceFor(t, f, testB), f, context.Background(), b); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	puts := append([]string{}, f.puts...)
	f.mu.Unlock()
	if len(puts) != 6 {
		t.Fatalf("puts %v", puts)
	}
	for i, name := range []string{"archive.ainovel", "manifest.json", "completion.json"} {
		if !strings.HasSuffix(puts[i], name) {
			t.Fatal(puts)
		}
	}
	listed, err := s.ListGenerations(context.Background(), f.account(), testBook)
	if err != nil || len(listed) != 2 || !listed[0].HasSibling {
		t.Fatalf("catalog %+v %v", listed, err)
	}
	stream, g, err := s.OpenGeneration(context.Background(), f.account(), testBook, testA)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(stream)
	closeErr := stream.Close()
	if err != nil || closeErr != nil || !bytes.Equal(actual, b) || g.ArchiveSHA256 != digestBytes(b) {
		t.Fatalf("stream %v %v", err, closeErr)
	}
	// Completed retry is readback-only, never overwrites immutable objects.
	if _, err := appendFixture(t, s, f, context.Background(), b); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.puts) != 6 {
		t.Fatal("retry overwrote existing generation")
	}
}
func TestUncertainPUTReadbackAndNoCommitOnCorruption(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			f := newFixture(t)
			f.dropSuffix = "archive.ainovel"
			f.corrupt = corrupt
			_, err := appendFixture(t, serviceFor(t, f, testA), f, context.Background(), []byte("PUBLIC archive"))
			if corrupt && err != RemoteConflict {
				t.Fatalf("expected conflict got %v", err)
			}
			if !corrupt && err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, put := range f.puts {
				if corrupt && strings.HasSuffix(put, "completion.json") {
					t.Fatal("committed corrupt partial generation")
				}
			}
		})
	}
}
func TestCancellationBeforeCommitMarker(t *testing.T) {
	for _, suffix := range []string{"archive.ainovel", "manifest.json"} {
		t.Run(suffix, func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.cancelSuffix = suffix
			f.cancel = cancel
			_, err := appendFixture(t, serviceFor(t, f, testA), f, ctx, []byte("PUBLIC"))
			if err != Cancelled {
				t.Fatalf("cancel: %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, p := range f.puts {
				if strings.HasSuffix(p, "completion.json") {
					t.Fatal("marker after cancel")
				}
			}
		})
	}
}
func TestLocalHashSizeMismatchNeverCommits(t *testing.T) {
	for _, bad := range []string{"hash", "short", "long"} {
		t.Run(bad, func(t *testing.T) {
			f := newFixture(t)
			s := serviceFor(t, f, testA)
			b := []byte("PUBLIC archive")
			in := AppendInput{Account: f.account(), CloudBookID: testBook, Archive: bytes.NewReader(b), ArchiveSHA256: digestBytes(b), ArchiveByteSize: int64(len(b)), OriginProjectID: testProject, PortableSnapshotGeneration: "portable-a"}
			switch bad {
			case "hash":
				in.ArchiveSHA256 = strings.Repeat("0", 64)
			case "short":
				in.ArchiveByteSize++
			case "long":
				in.ArchiveByteSize--
			}
			if _, err := s.AppendGeneration(context.Background(), in); err == nil {
				t.Fatal("accepted invalid local stream")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, p := range f.puts {
				if strings.HasSuffix(p, "manifest.json") || strings.HasSuffix(p, "completion.json") {
					t.Fatal("published after local mismatch")
				}
			}
		})
	}
}
func TestMissingCompletionAndWrongArchiveExcluded(t *testing.T) {
	f := newFixture(t)
	s := serviceFor(t, f, testA)
	if _, err := appendFixture(t, s, f, context.Background(), []byte("PUBLIC")); err != nil {
		t.Fatal(err)
	}
	where, _ := GenerationLocations(f.account().Endpoint, testBook, testA)
	f.mu.Lock()
	f.files[where.Archive.EscapedPath()][0] ^= 1
	f.mu.Unlock()
	listed, err := s.ListGenerations(context.Background(), f.account(), testBook)
	if err != nil || len(listed) != 0 {
		t.Fatalf("invalid catalog %v %v", listed, err)
	}
	stream, _, err := s.OpenGeneration(context.Background(), f.account(), testBook, testA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(stream); err != RemoteInvalid {
		t.Fatalf("hash error %v", err)
	}
	_ = stream.Close()
	f.mu.Lock()
	delete(f.files, where.Completion.EscapedPath())
	f.mu.Unlock()
	if _, _, err := s.OpenGeneration(context.Background(), f.account(), testBook, testA); err != NotFound {
		t.Fatal(err)
	}
}
func TestRedirectAndAuthorityEveryRequest(t *testing.T) {
	f := newFixture(t)
	f.redirect = "https://untrusted.invalid/stolen"
	calls := 0
	authorizations := 0
	s, err := New(Options{HTTPDispatch: func(r *http.Request) (*http.Response, error) {
		calls++
		return f.server.Client().Transport.RoundTrip(r)
	}, Authorize: func(_ context.Context, u *url.URL) error { authorizations++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckConnection(context.Background(), f.account()); err != Network || calls != 1 || authorizations != 1 {
		t.Fatalf("redirect %v calls %d auth %d", err, calls, authorizations)
	}
	s.options.Authorize = func(_ context.Context, u *url.URL) error { return errors.New("PUBLIC-secret transport URL") }
	if err := s.CheckConnection(context.Background(), f.account()); err != Network || calls != 1 {
		t.Fatal("unauthorized dispatch or leaked error", err)
	}
	// Authorizer URL mutation cannot alter the actual destination snapshot.
	f.redirect = ""
	s.options.Authorize = func(_ context.Context, u *url.URL) error { u.Host = "evil.invalid"; return nil }
	if err := s.CheckConnection(context.Background(), f.account()); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{}); err != InputInvalid {
		t.Fatal("default transport permitted")
	}
}
func TestBoundedHostileDAVXML(t *testing.T) {
	root, _ := url.Parse("https://example.invalid/dav/books/book/generations/")
	wrap := func(href string) string {
		return `<d:multistatus xmlns:d="DAV:"><d:response><d:href>` + href + `</d:href></d:response></d:multistatus>`
	}
	valid := root.Path + testA + "/"
	bodies := []string{
		`<!DOCTYPE x [<!ENTITY e SYSTEM "file:///secret">]>` + wrap("&e;"),
		`<d:multistatus xmlns:d="DAV:"><![CDATA[hidden]]></d:multistatus>`,
		wrap("//evil.invalid/" + testA), wrap("https://evil.invalid/" + testA),
		wrap(root.Path + "%2e%2e/" + testA + "/"), wrap(root.Path + testA + "%2fchild/"),
		wrap(root.Path + testA + "/?secret=1"), wrap(root.Path + testA + "/#fragment"),
		wrap(root.Path + testA + "%5c"), wrap(root.Path + testA + "%00"),
		wrap("https://writer:secret@example.invalid" + valid),
		wrap(valid) + wrap(valid), strings.Replace(wrap(valid), "</d:href>", "</d:href><d:href>"+valid+"</d:href>", 1),
		strings.Replace(wrap(valid), "</d:response>", "</d:wrong>", 1),
		`<x:multistatus xmlns:x="evil:"><x:response><x:href>` + valid + `</x:href></x:response></x:multistatus>`,
	}
	for i, b := range bodies {
		if _, err := GenerationHrefs([]byte(b), root, 1<<20); err != DAVResponseInvalid {
			t.Fatalf("hostile %d accepted: %v", i, err)
		}
	}
	ids, err := GenerationHrefs([]byte(wrap(valid)), root, 1<<20)
	if err != nil || len(ids) != 1 || ids[0] != testA {
		t.Fatalf("valid %v %v", ids, err)
	}
	if _, err := GenerationHrefs([]byte(wrap(valid)), root, 8); err != ResponseTooLarge {
		t.Fatal(err)
	}
	// A malicious href causes no secondary request.
	f := newFixture(t)
	f.xml = wrap("http://evil.invalid/" + testA)
	if _, err := serviceFor(t, f, testA).ListGenerations(context.Background(), f.account(), testBook); err != DAVResponseInvalid {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatal(f.requests)
	}
}
func TestMetadataExactKeysAndCatalogParentSet(t *testing.T) {
	m := Manifest{1, "book", testA, []string{"p-b", "p-a"}, "2026-09-21T00:00:00.000Z", testProject, "snapshot", ArchiveDescriptor{"archive.ainovel", strings.Repeat("0", 64), 0}}
	b, _ := wireJSON(m)
	parsed, err := ParseManifest(b)
	if err != nil || parsed.GenerationID != testA {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte(strings.Replace(string(b), `"version":1`, `"version":1,"extra":true`, 1)), []byte(strings.Replace(string(b), `"version":1`, `"version":1,"version":1`, 1)), []byte(strings.Replace(string(b), `"byteSize":0`, `"byteSize":null`, 1)), []byte(strings.Replace(string(b), `"byteSize":0`, `"byteSize":9007199254740992`, 1)), append(b, []byte(`{}`)...)} {
		if _, err := ParseManifest(bad); err != RemoteInvalid {
			t.Fatalf("accepted hostile metadata %s", bad)
		}
	}
	other := m
	other.GenerationID = testB
	other.ParentGenerationIDs = []string{"p-a", "p-b"}
	c := Catalog([]Manifest{m, other})
	if len(c) != 2 || !c[0].HasSibling || c[0].SiblingGenerationIDs[0] != testB || c[1].ParentGenerationIDs[0] != "p-a" {
		t.Fatal(c)
	}
}
func TestHostileResponsesAndCloseIntegrity(t *testing.T) {
	for _, test := range []struct {
		status   int
		length   string
		body     string
		expected Error
	}{
		{401, "", "raw-secret-error", Auth}, {403, "", "secret", Auth}, {404, "", "secret", NotFound},
		{500, "", "secret", Network}, {207, "-1", "", RemoteInvalid}, {207, "3", "x", RemoteInvalid},
		{207, "1048577", "", ResponseTooLarge},
	} {
		t.Run(fmt.Sprint(test.status, test.length), func(t *testing.T) {
			s, _ := New(Options{Authorize: func(context.Context, *url.URL) error { return nil }, HTTPDispatch: func(r *http.Request) (*http.Response, error) {
				header := http.Header{}
				if test.length != "" {
					header.Set("Content-Length", test.length)
				}
				return &http.Response{StatusCode: test.status, Header: header, Body: io.NopCloser(strings.NewReader(test.body)), ContentLength: -1}, nil
			}})
			err := s.CheckConnection(context.Background(), Account{"https://example.invalid/", "writer", "PUBLIC"})
			if err != test.expected {
				t.Fatalf("got %v want %v", err, test.expected)
			}
		})
	}
	b := []byte("PUBLIC")
	v := newVerifier(context.Background(), io.NopCloser(bytes.NewReader(b)), ArchiveDescriptor{SHA256: digestBytes(b), ByteSize: int64(len(b))}, 100)
	if err := v.Close(); err != RemoteInvalid {
		t.Fatal("early close incorrectly verified", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	v = newVerifier(ctx, io.NopCloser(bytes.NewReader(b)), ArchiveDescriptor{SHA256: digestBytes(b), ByteSize: int64(len(b))}, 100)
	cancel()
	if _, err := v.Read(make([]byte, 8)); err != Cancelled {
		t.Fatal(err)
	}
}
