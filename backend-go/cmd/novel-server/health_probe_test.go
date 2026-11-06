package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHealthProbeAcceptsThisServer is the arm the container relies on: a 200 with
// the body this binary writes is healthy.
func TestHealthProbeAcceptsThisServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probe asked for %v", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"runtime":"go","electron":false,"migrationComplete":false}`))
	}))
	defer server.Close()
	if err := probeHealthz(server.Client(), strings.TrimPrefix(server.URL, "http://")); err != nil {
		t.Fatalf("healthy server refused: %v", err)
	}
}

// TestHealthProbeRefusesAnotherRuntime is the reason the runtime field is checked:
// the image being replaced answers this path too, with runtime "node". A probe
// that only looked at the status code would call that container healthy.
func TestHealthProbeRefusesAnotherRuntime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"runtime":"node","electron":false}`))
	}))
	defer server.Close()
	err := probeHealthz(server.Client(), strings.TrimPrefix(server.URL, "http://"))
	if err == nil {
		t.Fatal("a node runtime was accepted")
	}
	if strings.Contains(err.Error(), "node") {
		t.Fatalf("the probe echoed the server's claim: %v", err)
	}
}

func TestHealthProbeRefusals(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		refused bool
	}{
		{"ok", 200, `{"ok":true,"runtime":"go"}`, false},
		{"not ok", 200, `{"ok":false,"runtime":"go"}`, true},
		{"empty runtime", 200, `{"ok":true}`, true},
		{"service unavailable", 503, `{"ok":true,"runtime":"go"}`, true},
		{"not json", 200, `<!doctype html>`, true},
		{"proxy page", 200, `<html><body>502 Bad Gateway</body></html>`, true},
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(row.status)
				_, _ = w.Write([]byte(row.body))
			}))
			defer server.Close()
			err := probeHealthz(server.Client(), strings.TrimPrefix(server.URL, "http://"))
			if row.refused && err == nil {
				t.Fatal("accepted a body the probe must refuse")
			}
			if !row.refused && err != nil {
				t.Fatalf("refused a healthy body: %v", err)
			}
		})
	}
}

// TestHealthProbeReportsAnUnreachableServer proves the failure names the address
// instead of hanging: a closed port must fail fast and say where it looked.
func TestHealthProbeReportsAnUnreachableServer(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	address := strings.TrimPrefix(server.URL, "http://")
	server.Close()
	started := time.Now()
	err := probeHealthz(healthProbeClient(), address)
	if err == nil {
		t.Fatal("an unreachable server was reported healthy")
	}
	if !strings.Contains(err.Error(), address) {
		t.Fatalf("the failure does not name the address: %v", err)
	}
	if elapsed := time.Since(started); elapsed > healthProbeTimeout+time.Second {
		t.Fatalf("the probe took %v, longer than its bound", elapsed)
	}
}

// TestHealthProbeNoClient proves a nil client is refused rather than panicking in
// a caller that forgot to build one.
func TestHealthProbeNoClient(t *testing.T) {
	if err := probeHealthz(nil, "127.0.0.1:16082"); err == nil {
		t.Fatal("a nil client was accepted")
	}
}

// TestHealthProbeAddress proves the listen address is rewritten into one a probe
// can dial, and that an address that is not host:port is refused.
func TestHealthProbeAddress(t *testing.T) {
	cases := []struct {
		listen  string
		want    string
		refused bool
	}{
		{"0.0.0.0:16082", "127.0.0.1:16082", false},
		{":16082", "127.0.0.1:16082", false},
		{"127.0.0.1:16082", "127.0.0.1:16082", false},
		{"192.168.1.15:16082", "192.168.1.15:16082", false},
		{"[::]:16082", "[::1]:16082", false},
		{"16082", "", true},
		{"", "", true},
		{"0.0.0.0:", "", true},
	}
	for _, row := range cases {
		t.Run(row.listen, func(t *testing.T) {
			got, err := healthProbeAddress(row.listen)
			if row.refused {
				if err == nil {
					t.Fatalf("accepted %v as %v", row.listen, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused a dialable listen address: %v", err)
			}
			if got != row.want {
				t.Fatalf("probe address: got %v, want %v", got, row.want)
			}
		})
	}
}
