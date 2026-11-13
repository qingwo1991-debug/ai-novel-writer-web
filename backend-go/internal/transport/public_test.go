package transport

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

func TestPublicBoundarySanitizesSuccessBusinessErrorsAndPrivateClones(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	secret := "TEST_TOKEN_123"
	if ok, err := server.RegisterSecret(secret); !ok || err != nil {
		t.Fatal(err)
	}
	private := map[string]any{"apiKey": secret, "nested": map[string]any{"text": "before " + secret + " after", secret: "key " + secret}, "PASSWORD": "another-private", "hasCredential": true}
	if err := server.Bind("config:get", func(context.Context, Caller, []any) (any, error) { return private, nil }); err != nil {
		t.Fatal(err)
	}
	response := rpc(t, server, cookie, csrf, "config:get", `{"args":[]}`, nil)
	if response.Code != 200 || strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), "another-private") || !strings.Contains(response.Body.String(), "[REDACTED]") {
		t.Fatal(response.Body.String())
	}
	if private["apiKey"] != secret || private["nested"].(map[string]any)["text"] != "before "+secret+" after" {
		t.Fatal("public boundary mutated private dispatch values")
	}
	if err := server.Bind("config:set", func(context.Context, Caller, []any) (any, error) {
		return map[string]any{"success": false, "error": "business " + secret}, nil
	}); err != nil {
		t.Fatal(err)
	}
	business := rpc(t, server, cookie, csrf, "config:set", `{"args":[]}`, nil)
	if business.Code != 200 || strings.Contains(business.Body.String(), secret) {
		t.Fatal(business.Body.String())
	}
	for _, err := range []error{errors.New(secret), &security.HTTPError{Status: 409, Code: secret, Message: "detail " + secret}} {
		recorder := httptest.NewRecorder()
		if e := server.writePublicError(recorder, err); e != nil {
			t.Fatal(e)
		}
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatal("public error leaked registered credential", recorder.Body.String())
		}
	}
}

func TestPublicBoundarySanitizesBeforeLiveAndReplayStorage(t *testing.T) {
	server := fixture(t)
	secret := "SSE-PRIVATE-TOKEN"
	_, err := server.RegisterSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	current, err := server.getClient("session-synthetic", "synthetic-tab-0001")
	if err != nil {
		t.Fatal(err)
	}
	defer server.unpin(current)
	caller := Caller{Principal: "synthetic-author", SessionID: "session-synthetic", ClientID: "synthetic-tab-0001"}
	_, queue, cancel, err := current.hub.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	payload := []any{map[string]any{"secret": secret, "text": "visible " + secret}}
	if err := server.Publish(caller, "stream:"+secret, payload); err != nil {
		t.Fatal(err)
	}
	live := <-queue
	if strings.Contains(string(live.Wire()), secret) || !strings.Contains(string(live.Wire()), "[REDACTED]") {
		t.Fatal(string(live.Wire()))
	}
	live.Done()
	if err := server.Publish(caller, "stream:second", payload); err != nil {
		t.Fatal(err)
	}
	second := <-queue
	second.Done()
	history, _, stop, err := current.hub.Subscribe(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if len(history) != 1 || strings.Contains(string(history[0].Wire()), secret) {
		t.Fatal("credential entered replay history")
	}
	if payload[0].(map[string]any)["secret"] != secret {
		t.Fatal("event sanitizer mutated producer values")
	}
}

// A write against a session the server no longer holds must tell that tab its
// project was lost, so the renderer reopens instead of autosaving forever.
func TestProjectSessionLossPublishesOnlyForLostSessionCodes(t *testing.T) {
	server := fixture(t)
	current, err := server.getClient("session-synthetic", "synthetic-tab-0001")
	if err != nil {
		t.Fatal(err)
	}
	defer server.unpin(current)
	caller := Caller{Principal: "synthetic-author", SessionID: "session-synthetic", ClientID: "synthetic-tab-0001"}
	_, queue, cancel, err := current.hub.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	for _, row := range []struct {
		err  error
		lost bool
	}{
		{errors.New("PROJECT_SESSION_NOT_OWNED"), true},
		{&security.HTTPError{Status: 409, Code: "PROJECT_SESSION_STALE", Message: "PROJECT_SESSION_STALE"}, true},
		{errors.New("PROJECT_CONTEXT_CHANGED"), false},
		{errors.New("private /author/project.db ENOENT"), false},
	} {
		server.publishProjectSessionLost(caller, row.err)
		select {
		case event := <-queue:
			if !row.lost {
				t.Fatal("unexpected event", string(event.Wire()))
			}
			if !strings.Contains(string(event.Wire()), "browser:project-lost") {
				t.Fatal(string(event.Wire()))
			}
			event.Done()
		default:
			if row.lost {
				t.Fatal("no project-lost event for", row.err)
			}
		}
	}
	if code := publicErrorCode(errors.New("PROJECT_SESSION_NOT_OWNED")); code != "PROJECT_SESSION_NOT_OWNED" {
		t.Fatal(code)
	}
	if code := publicErrorCode(&security.HTTPError{Status: 409, Code: "PROJECT_SESSION_STALE"}); code != "PROJECT_SESSION_STALE" {
		t.Fatal(code)
	}
	if code := publicErrorCode(errors.New("private /author/project.db")); code != "" {
		t.Fatal(code)
	}
}
