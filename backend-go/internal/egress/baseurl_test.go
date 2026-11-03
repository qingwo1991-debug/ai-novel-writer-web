package egress

import (
	"errors"
	"testing"
)

func TestSavedBaseURLUsesDispatchOriginPolicyWithoutDialing(t *testing.T) {
	client, err := New([]string{"https://allowed.example:8443", "http://127.0.0.1:8317"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, value := range []string{"https://allowed.example:8443/v1", "http://127.0.0.1:8317/models"} {
		if err := client.CheckBaseURL(value); err != nil {
			t.Fatalf("admitted base %q rejected: %v", value, err)
		}
	}
	if err := client.CheckBaseURL("https://denied.example/v1"); !errors.Is(err, ErrNotAllowed) {
		t.Fatal("untrusted origin admitted", err)
	}
	for _, value := range []string{"https://allowed.example:8443/v1?key=private", "https://allowed.example:8443/v1#fragment", "https://user:pass@allowed.example:8443/v1", "ftp://allowed.example:8443/v1", "not a URL"} {
		if err := client.CheckBaseURL(value); !errors.Is(err, ErrBaseURLInvalid) {
			t.Fatalf("malformed base %q admitted: %v", value, err)
		}
	}
	var nilClient *Client
	if err := nilClient.CheckBaseURL("https://allowed.example:8443/v1"); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatal("missing policy admitted URL", err)
	}
}
