package globalconfig

import (
	"encoding/json"
	"math"
	"testing"
)

func appearanceAck(revision any, generation string) map[string]any {
	return map[string]any{"storageKey": AppearanceStorageKey, "profileRevision": revision, "globalGeneration": generation}
}

func TestNoMigrationAuthorityNeverReady(t *testing.T) {
	startup := NewStartup(StartupCapture{})
	if state := startup.State("backend-scoped-client", true); state.State != "pending" || state.GlobalGeneration != "" {
		t.Fatal("invented ready without migration", state)
	}
	if startup.AppearanceAck("backend-scoped-client", true, appearanceAck(1, "invented-by-request")) {
		t.Fatal("request generation became authority")
	}
	if state := startup.State("backend-scoped-client", false); state.State != "blocked" || state.Code != "" {
		t.Fatal("untrusted state leaked", state)
	}
	if state := startup.State("", true); state.State != "blocked" {
		t.Fatal("empty caller admitted", state)
	}
}

func TestStartupCapturedNoticeAndCallerScopedMonotonicAck(t *testing.T) {
	notice := &MigrationNotice{LegacySourceIgnored: true, PreservedUnknownCount: 7}
	capture := StartupCapture{Generation: "trusted-backend-generation", MigrationNotice: notice}
	startup := NewStartup(capture)
	capture.Generation = "request-generation"
	notice.PreservedUnknownCount = 999
	generation := "trusted-backend-generation"
	if !startup.AppearanceAck("alice", true, appearanceAck(3, generation)) {
		t.Fatal("valid ack rejected")
	}
	if startup.AppearanceAck("bob", false, appearanceAck(99, generation)) {
		t.Fatal("untrusted ack accepted")
	}
	if state := startup.State("bob", true); state.AppearanceRevisionFloor != nil {
		t.Fatal("other caller's floor leaked", state)
	}
	if !startup.AppearanceAck("bob", true, appearanceAck(1, generation)) {
		t.Fatal("bob inherited alice revision")
	}
	if startup.AppearanceAck("alice", true, appearanceAck(2, generation)) {
		t.Fatal("regression accepted")
	}
	if !startup.AppearanceAck("alice", true, appearanceAck(3, generation)) {
		t.Fatal("equal ack rejected")
	}
	if !startup.AppearanceAck("alice", true, appearanceAck(json.Number("5e0"), generation)) {
		t.Fatal("safe integral exponent rejected")
	}
	state := startup.State("alice", true)
	if state.State != "ready" || state.GlobalGeneration != generation || state.AppearanceRevisionFloor == nil || *state.AppearanceRevisionFloor != 5 || state.MigrationNotice.PreservedUnknownCount != 7 {
		t.Fatal(state)
	}
	state.MigrationNotice.PreservedUnknownCount = -1
	*state.AppearanceRevisionFloor = 1
	again := startup.State("alice", true)
	if *again.AppearanceRevisionFloor != 5 || again.MigrationNotice.PreservedUnknownCount != 7 {
		t.Fatal("state response aliases backend capture", again)
	}
	startup.ForgetCaller("alice")
	if startup.State("alice", true).AppearanceRevisionFloor != nil || startup.State("bob", true).AppearanceRevisionFloor == nil {
		t.Fatal("scope cleanup lost other caller")
	}
}

func TestStartupAckStrictShapeAndSafeRevision(t *testing.T) {
	startup := NewStartup(StartupCapture{Generation: "generation"})
	for _, invalid := range []any{nil, []any{}, "ack", false, map[string]any{}, map[string]any{"storageKey": AppearanceStorageKey}, map[string]any{"storageKey": AppearanceStorageKey, "profileRevision": 1, "globalGeneration": "generation", "extra": true}} {
		if startup.AppearanceAck("caller", true, invalid) {
			t.Fatalf("invalid shape accepted: %#v", invalid)
		}
	}
	for _, invalid := range []any{nil, true, "1", 0, -1, 1.5, math.Inf(1), math.NaN(), float64(9007199254740992), int64(9007199254740992), uint64(9007199254740992), json.Number("not-a-number")} {
		if startup.AppearanceAck("caller", true, appearanceAck(invalid, "generation")) {
			t.Fatalf("invalid revision accepted: %#v", invalid)
		}
	}
	for _, generation := range []string{"", "request-supplied", "generation "} {
		if startup.AppearanceAck("caller", true, appearanceAck(1, generation)) {
			t.Fatal("wrong generation accepted")
		}
	}
	ack := appearanceAck(1, "generation")
	ack["storageKey"] = []any{"wrong"}
	if startup.AppearanceAck("caller", true, ack) {
		t.Fatal("invalid storage key accepted")
	}
	if !startup.AppearanceAck("caller", true, appearanceAck(int64(9007199254740991), "generation")) {
		t.Fatal("maximum JS safe revision rejected")
	}
}

func TestBlockedStartupPrecedesGenerationAndNoTrustDisclosure(t *testing.T) {
	for _, code := range []string{BlockedGlobalData, BlockedModelCredentialsDiffer} {
		startup := NewStartup(StartupCapture{Generation: "exists-but-blocked", BlockedCode: code, MigrationNotice: &MigrationNotice{PreservedUnknownCount: 3}})
		state := startup.State("caller", true)
		if state.State != "blocked" || state.Code != code || state.GlobalGeneration != "" || state.MigrationNotice != nil {
			t.Fatal(state)
		}
		if state := startup.State("caller", false); state.Code != "" || state.State != "blocked" {
			t.Fatal("untrusted code disclosure", state)
		}
		if startup.AppearanceAck("caller", true, appearanceAck(1, "exists-but-blocked")) {
			t.Fatal("blocked ack accepted")
		}
	}
}
