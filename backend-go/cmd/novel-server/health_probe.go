package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// healthProbeTimeout bounds one liveness probe. It is deliberately shorter than
// any orchestrator's own timeout so a hung server reports as unhealthy instead
// of a timeout the operator cannot attribute.
const healthProbeTimeout = 4 * time.Second

// healthProbeClient is the client the probe uses: no proxy (the container talks
// to itself, and a proxy would turn a local liveness check into an egress test),
// no redirect following, and a hard timeout.
func healthProbeClient() *http.Client {
	return &http.Client{
		Timeout: healthProbeTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: healthProbeTimeout}).DialContext,
			ResponseHeaderTimeout: healthProbeTimeout,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// healthProbeAddress turns a listen address into one a probe can dial. A server
// bound to 0.0.0.0 or :: is reachable on loopback, and a bare port is not a
// valid dial target, so both are rewritten. A configured loopback or specific
// interface address is kept as written.
func healthProbeAddress(listen string) (string, error) {
	trimmed := strings.TrimSpace(listen)
	if trimmed == "" {
		return "", errors.New("HEALTHCHECK_LISTEN_EMPTY")
	}
	host, port, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", errors.New("HEALTHCHECK_LISTEN_INVALID")
	}
	if strings.TrimSpace(port) == "" {
		return "", errors.New("HEALTHCHECK_LISTEN_INVALID")
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return net.JoinHostPort(host, port), nil
}

// probeHealthz performs the one liveness check the container healthcheck runs:
// the server must answer 200 with a body that declares itself the Go runtime.
//
// The runtime check is not cosmetic. The image this replaces answered the same
// path with runtime "node"; a stale container, a wrong image tag or a proxy in
// front of the port would then all look healthy while nothing the author clicks
// works. Requiring runtime "go" makes the probe identify the process it is
// meant to be watching.
func probeHealthz(client *http.Client, address string) error {
	if client == nil {
		return errors.New("HEALTHCHECK_CLIENT_MISSING")
	}
	request, err := http.NewRequest(http.MethodGet, "http://"+address+"/healthz", nil)
	if err != nil {
		return errors.New("HEALTHCHECK_REQUEST_INVALID")
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("health probe could not reach %s: %w", address, err)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health probe saw status %d from %s", response.StatusCode, address)
	}
	var body struct {
		OK      bool   `json:"ok"`
		Runtime string `json:"runtime"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return errors.New("health probe body is not JSON")
	}
	if !body.OK {
		return errors.New("health probe body reports not ok")
	}
	if body.Runtime != "go" {
		// The value is not echoed: it is an untrusted server's claim, and a probe
		// that prints it can be made to print anything.
		return errors.New("health probe answered a runtime that is not this server")
	}
	return nil
}

// runHealthProbe is the -healthcheck entry point. It exits 0 only when the
// server on the configured listen address is this binary and reports ok.
func runHealthProbe(listen string) error {
	address, err := healthProbeAddress(listen)
	if err != nil {
		return err
	}
	return probeHealthz(healthProbeClient(), address)
}
