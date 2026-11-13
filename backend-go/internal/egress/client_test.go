package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, origins ...string) *Client {
	t.Helper()
	client, err := New(origins)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func newTestRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return request
}

func TestOriginConfiguration(t *testing.T) {
	for _, origins := range [][]string{
		nil, {}, {""}, {" "}, {"https://allowed.test", ""},
		{"https://*.test"}, {"https://allowed.test/path"},
		{"https://allowed.test//"}, {"https://allowed.test/%2f"},
		{"https://allowed.test?"}, {"https://allowed.test?token=secret"},
		{"https://allowed.test#"}, {"https://allowed.test#secret"},
		{"https://user:secret@allowed.test"}, {"https://@allowed.test"},
		{"https://allowed.test:abc"}, {"https://allowed.test:"},
		{"https://allowed.test:65536"}, {"https://allowed.test:-1"},
		{"https://allowed.test:+443"}, {"https://allowed.test:443:80"},
		{"https://.allowed.test"}, {"https://allowed..test"},
		{"https://allowed.test."}, {"https://%61llowed.test"},
		{"https://allowed.test\\@other.test"}, {"https://allowed.test\n" + "other.test"},
		{"file:///etc/passwd"}, {"ftp://allowed.test"}, {"//allowed.test"},
		{"https:allowed.test"}, {"https://-allowed.test"}, {"https://allowed_.test"},
		{"http://127.1"}, {"http://2130706433"}, {"http://0177.0.0.1"},
		{"http://0x7f000001"}, {"http://example.123"}, {"http://example.0x"},
		{"http://[::1%25eth0]"}, {"http://[127.0.0.1]"},
		{"http://::1"}, {"http://[::1]garbage"}, {"https://例子.test"},
	} {
		client, err := New(origins)
		if err != ErrPolicyInvalid || client != nil {
			t.Errorf("invalid configuration returned client=%v err=%v", client, err)
		}
	}
	for _, test := range []struct {
		configured string
		origin     string
	}{
		{" HTTPS://ExAmPlE.TeSt:00443/ ", "https://example.test"},
		{"HTTP://LOCALHOST:00080", "http://localhost"},
		{"https://example.test:8443", "https://example.test:8443"},
		{"http://127.0.0.1:08181", "http://127.0.0.1:8181"},
		{"http://192.168.1.15:8317", "http://192.168.1.15:8317"},
		{"https://[2001:0DB8:0:0:0:0:0:1]:443/", "https://[2001:db8::1]"},
		{"http://[::1]:80", "http://[::1]"},
		{"https://xn--fsqu00a.test", "https://xn--fsqu00a.test"},
	} {
		client := newTestClient(t, test.configured)
		if _, exists := client.origins[test.origin]; !exists || len(client.origins) != 1 {
			t.Errorf("normalized origins = %v, want %q", client.origins, test.origin)
		}
	}
}

func TestPrivateTransportAndPolicyOwnership(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("ALL_PROXY", "http://proxy.invalid:3128")
	origins := []string{"https://allowed.test"}
	client := newTestClient(t, origins...)
	other := newTestClient(t, origins...)
	origins[0] = "https://other.test"
	if _, ok := client.origins["https://allowed.test"]; !ok {
		t.Fatal("caller mutated policy")
	}
	transport := client.transport
	if transport == other.transport || transport == http.DefaultTransport || transport.Proxy != nil {
		t.Fatal("transport is shared or inherits proxy configuration")
	}
	if transport.DialContext == nil || transport.DialTLSContext != nil || transport.DialTLS != nil {
		t.Fatal("unexpected dial configuration")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify ||
		transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.TLSClientConfig.RootCAs != nil ||
		transport.TLSClientConfig.VerifyConnection != nil || transport.TLSClientConfig.VerifyPeerCertificate != nil {
		t.Fatal("TLS validation is not the default verifier with minimum TLS 1.2")
	}
	if transport.TLSHandshakeTimeout != 10*time.Second || transport.ResponseHeaderTimeout != 30*time.Second || client.native.Timeout != 0 {
		t.Fatal("unexpected phase timeouts or streaming body deadline")
	}
	if client.native.Jar != nil || client.native.CheckRedirect == nil {
		t.Fatal("ambient cookies or missing redirect prohibition")
	}
	if err := client.native.CheckRedirect(nil, nil); err != ErrRedirectForbidden {
		t.Fatalf("CheckRedirect = %v", err)
	}
	client.Close()
	client.Close()
	var zero *Client
	zero.Close()
	if response, err := zero.Do(nil); response != nil || err != ErrPolicyInvalid {
		t.Fatalf("nil client: response=%v err=%v", response, err)
	}
}

func TestAllowedOriginAndOtherReceiverDenied(t *testing.T) {
	var allowedCalls, deniedCalls atomic.Int32
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedCalls.Add(1)
		if r.URL.RequestURI() != "/v1/chat?token=opaque" {
			t.Errorf("request URI = %q", r.URL.RequestURI())
		}
		_, _ = io.WriteString(w, "allowed")
	}))
	defer allowed.Close()
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deniedCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer denied.Close()
	client := newTestClient(t, allowed.URL)
	response, err := client.Do(newTestRequest(t, allowed.URL+"/v1/chat?token=opaque"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(data) != "allowed" {
		t.Fatalf("body=%q err=%v", data, err)
	}
	if response, err := client.Do(newTestRequest(t, denied.URL+"/v1/chat?token=secret")); err != ErrNotAllowed || response != nil {
		t.Fatalf("denied dispatch: response=%v err=%v", response, err)
	}
	if allowedCalls.Load() != 1 || deniedCalls.Load() != 0 {
		t.Fatalf("receiver calls: allowed=%d denied=%d", allowedCalls.Load(), deniedCalls.Load())
	}
}

func TestDispatchValidatesExactFullURLAndHost(t *testing.T) {
	client := newTestClient(t, "https://allowed.test")
	for _, target := range []string{
		"https://allowed.test.other.test/path", "https://other.test/https://allowed.test",
		"http://allowed.test/path", "https://allowed.test:8443/path",
		"https://allowed.test@other.test/", "https://user:secret@allowed.test/",
		"https://@allowed.test/", "https://allowed.test./", "https://allowed..test/",
	} {
		response, err := client.Do(newTestRequest(t, target))
		if response != nil || err != ErrNotAllowed {
			t.Errorf("invalid target yielded response=%v err=%v", response, err)
		}
	}
	for _, override := range []string{"other.test", "allowed.test:443", "ALLOWED.TEST", "allowed.test@other.test"} {
		request := newTestRequest(t, "https://allowed.test/v1?secret=opaque")
		request.Host = override
		if response, err := client.Do(request); response != nil || err != ErrNotAllowed {
			t.Errorf("Host override yielded response=%v err=%v", response, err)
		}
	}
	for _, host := range []string{"", "ALLOWED.TEST:443"} {
		request := newTestRequest(t, "https://ALLOWED.TEST:443/v1%2Fchat?token=opaque#fragment")
		request.Host = host
		snapshot, err := client.snapshot(request)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.URL.String() != "https://allowed.test/v1%2Fchat?token=opaque#fragment" || snapshot.Host != "" {
			t.Fatalf("full normalized snapshot = %v Host=%q", snapshot.URL, snapshot.Host)
		}
		if request.URL.Host != "ALLOWED.TEST:443" || request.Host != host {
			t.Fatal("validation mutated caller request")
		}
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.URL = nil },
		func(r *http.Request) { r.URL.User = url.User("") },
		func(r *http.Request) { r.URL.Opaque = "//other.test/" },
		func(r *http.Request) { r.URL.OmitHost = true },
		func(r *http.Request) { r.URL.Host = "%61llowed.test" },
		func(r *http.Request) { r.URL.RawPath = "/contradictory" },
		func(r *http.Request) { r.URL.RawFragment = "contradictory" },
		func(r *http.Request) { r.URL.RawQuery = "secret=opaque\n" },
		func(r *http.Request) { r.RequestURI = "https://other.test/" },
	} {
		request := newTestRequest(t, "https://allowed.test/path#fragment")
		mutate(request)
		if response, err := client.Do(request); response != nil || err != ErrNotAllowed {
			t.Errorf("malformed URL struct yielded response=%v err=%v", response, err)
		}
	}
	if response, err := client.Do(nil); response != nil || err != ErrNotAllowed {
		t.Fatalf("nil request: response=%v err=%v", response, err)
	}
	boundary := client.native.Transport
	if response, err := boundary.RoundTrip(newTestRequest(t, "https://other.test/")); response != nil || err != ErrNotAllowed {
		t.Fatalf("dispatch boundary did not revalidate: response=%v err=%v", response, err)
	}
}

func TestAllRedirectStatusesDeniedWithoutAnotherDispatch(t *testing.T) {
	var followed atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer other.Close()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/receiver" {
			followed.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		location := r.URL.Query().Get("location")
		if location == "same" {
			location = server.URL + "/receiver"
		}
		if location == "other" {
			location = other.URL + "/receiver"
		}
		if location != "" {
			w.Header().Set("Location", location)
		}
		status := 302
		switch r.URL.Path {
		case "/300":
			status = 300
		case "/304":
			status = 304
		case "/307":
			status = 307
		case "/308":
			status = 308
		case "/399":
			status = 399
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "redirect body must not escape")
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, other.URL)
	for _, path := range []string{
		"/302?location=same", "/302?location=other", "/307?location=other", "/308?location=same",
		"/300", "/304", "/399", "/302?location=" + url.QueryEscape("http://user:secret@%broken/"),
	} {
		response, err := client.Do(newTestRequest(t, server.URL+path))
		if response != nil || err != ErrRedirectForbidden {
			t.Errorf("redirect yielded response=%v err=%v", response, err)
		}
	}
	if followed.Load() != 0 {
		t.Fatalf("redirects caused %d receiver calls", followed.Load())
	}
}

func TestRedirectBodyIsClosed(t *testing.T) {
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := newTestRequest(t, server.URL)
	request = request.WithContext(ctx)
	if response, err := client.Do(request); response != nil || err != ErrRedirectForbidden {
		t.Fatalf("redirect: response=%v err=%v", response, err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("redirect body not closed promptly")
	}
}

func TestRequestMutationCannotChangeSnapshot(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, r.URL.RequestURI()+"|"+r.Header.Get("X-Snapshot"))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	request := newTestRequest(t, server.URL+"/original%2Fpath?token=original#fragment")
	request.Header.Set("X-Snapshot", "original")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	type result struct {
		response *http.Response
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		response, err := client.Do(request)
		resultCh <- result{response, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		close(release)
		t.Fatal("dispatch did not start")
	}
	request.URL.Host = "other.test"
	request.URL.Path = "/mutated"
	request.URL.RawPath = ""
	request.URL.RawQuery = "token=mutated"
	request.URL.Fragment = "mutated"
	request.Host = "other.test"
	request.Header.Set("X-Snapshot", "mutated")
	close(release)
	outcome := <-resultCh
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	defer outcome.response.Body.Close()
	data, err := io.ReadAll(outcome.response.Body)
	if err != nil || string(data) != "/original%2Fpath?token=original|original" {
		t.Fatalf("body=%q err=%v", data, err)
	}
	if outcome.response.Request.URL.Fragment != "fragment" || outcome.response.Request.URL.Host == "other.test" || outcome.response.Request.Header.Get("X-Snapshot") != "original" {
		t.Fatal("response request aliases mutable caller request")
	}
}

func TestCancellationBeforeHeadersAndDuringStreaming(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			_, _ = io.WriteString(w, "first")
			w.(http.Flusher).Flush()
		}
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	for _, path := range []string{"/headers", "/stream"} {
		ctx, cancel := context.WithCancel(context.Background())
		request := newTestRequest(t, server.URL+path).WithContext(ctx)
		if path == "/headers" {
			done := make(chan error, 1)
			go func() {
				_, err := client.Do(request)
				done <- err
			}()
			<-started
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) || err.Error() != "context canceled" {
				t.Fatalf("dispatch cancellation = %v", err)
			}
		} else {
			response, err := client.Do(request)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			<-started
			prefix := make([]byte, len("first"))
			_, err = io.ReadFull(response.Body, prefix)
			if err != nil || string(prefix) != "first" {
				cancel()
				_ = response.Body.Close()
				t.Fatalf("stream prefix=%q err=%v", prefix, err)
			}
			cancel()
			_, err = io.ReadAll(response.Body)
			_ = response.Body.Close()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("stream cancellation = %v", err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Do(newTestRequest(t, server.URL).WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled context = %v", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := client.Do(newTestRequest(t, server.URL).WithContext(ctx)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired context = %v", err)
	}
}

func TestNativeFailuresAreSanitizedAndTLSVerified(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("untrusted TLS request reached handler")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	response, err := client.Do(newTestRequest(t, server.URL+"/secret-path?api_key=super-secret"))
	if response != nil || err != ErrRequestFailed || err.Error() != "WEB_ENDPOINT_REQUEST_FAILED" {
		t.Fatalf("TLS failure: response=%v err=%v", response, err)
	}
	if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "super-secret") {
		t.Fatal("native failure leaked URL")
	}
}
