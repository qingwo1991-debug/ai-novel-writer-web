//go:build linux

package globalconfig

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func jsonValue(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil { t.Fatal(err) }
	decoded, err := decode(data)
	if err != nil { t.Fatal(err) }
	return decoded
}

func TestActualOriginalGlobalConfigContract(t *testing.T) {
	filename := os.Getenv("GO_GLOBAL_CONFIG_CONTRACT")
	if filename == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" { t.Fatal("GO_GLOBAL_CONFIG_CONTRACT is required on GitHub Actions") }
		t.Skip("actual original global config/startup oracle runs only on GitHub Actions")
	}
	base := os.Getenv("RUNNER_TEMP")
	if !filepath.IsAbs(base) || filename != filepath.Join(base, "novel-go-global-config", "contract.json") { t.Fatal("fixture must be the explicit RUNNER_TEMP global config oracle") }
	file, err := os.Open(filename)
	if err != nil { t.Fatal(err) }
	defer file.Close()
	var contract struct {
		FormatVersion int `json:"formatVersion"`
		Authority string `json:"authority"`
		Originals []string `json:"originals"`
		DefaultConfig any `json:"defaultConfig"`
		ConfigCases []struct {
			Name string `json:"name"`
			InitialRaw *string `json:"initialRaw"`
			Patch map[string]any `json:"patch"`
			SystemLocale string `json:"systemLocale"`
			ExpectedGet any `json:"expectedGet"`
			ExpectedResult Result `json:"expectedResult"`
			AfterRaw *string `json:"afterRaw"`
			AfterValue any `json:"afterValue"`
			BeforeEntries []string `json:"beforeEntries"`
			AfterEntries []string `json:"afterEntries"`
		} `json:"configCases"`
		StartupCases []struct {
			Capture struct {
				Generation string `json:"generation"`
				BlockedCode string `json:"blockedCode"`
				Notice *struct {
					LegacySourceIgnored bool `json:"legacySourceIgnored"`
					PreservedUnknownCount int64 `json:"preservedUnknownCount"`
					Forbidden string `json:"forbidden"`
				} `json:"notice"`
			} `json:"capture"`
			Steps []struct {
				Caller string `json:"caller"`
				Trusted bool `json:"trusted"`
				Kind string `json:"kind"`
				Input any `json:"input"`
				Expected any `json:"expected"`
				TrustFailure string `json:"trustFailure"`
			} `json:"steps"`
		} `json:"startupCases"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 8<<20))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil { t.Fatal(err) }
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF { t.Fatal("contract trailing content", err) }
	if contract.FormatVersion != 1 || contract.Authority != "actual-unmodified-original-modules" || len(contract.ConfigCases) != 21 || len(contract.StartupCases) != 5 { t.Fatal("unexpected oracle schema/coverage") }
	for _, required := range []string{"electron/utils/config-utils.ts", "electron/controllers/config-controller.ts", "electron/controllers/startup-controller.ts", "electron/services/app-data-locator.ts", "electron/i18n.ts", "src/shared/appearance-profile.ts"} {
		found := false
		for _, actual := range contract.Originals { if actual == required { found = true } }
		if !found { t.Fatalf("missing actual original %s", required) }
	}
	if !reflect.DeepEqual(jsonValue(t, DefaultConfig()), contract.DefaultConfig) { t.Fatalf("default config differs: %#v", contract.DefaultConfig) }
	names := make(map[string]bool)
	for _, tc := range contract.ConfigCases {
		if names[tc.Name] { t.Fatal("duplicate config case") }; names[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "config.json")
			if tc.InitialRaw != nil { if err := os.WriteFile(file, []byte(*tc.InitialRaw), 0600); err != nil { t.Fatal(err) } }
			service, err := New(root)
			if err != nil { t.Fatal(err) }
			defer service.Close()
			actualGet, err := service.Get()
			if err != nil { t.Fatal(err) }
			if !reflect.DeepEqual(jsonValue(t, actualGet), tc.ExpectedGet) { t.Fatalf("whole config:get differs: got %#v, want %#v", actualGet, tc.ExpectedGet) }
			beforePatch := jsonValue(t, tc.Patch)
			actualResult := service.SetForLocale(tc.Patch, tc.SystemLocale)
			if actualResult != tc.ExpectedResult { t.Fatalf("whole config:set result differs: got %#v, want %#v", actualResult, tc.ExpectedResult) }
			if !reflect.DeepEqual(beforePatch, jsonValue(t, tc.Patch)) { t.Fatal("set mutated caller patch") }
			after, err := os.ReadFile(file)
			if err != nil { t.Fatal(err) }
			if !actualResult.Success {
				if tc.AfterRaw == nil || !bytes.Equal(after, []byte(*tc.AfterRaw)) { t.Fatal("refusal changed exact original bytes") }
			} else {
				decoded, err := decode(after)
				if err != nil { t.Fatal(err) }
				if !reflect.DeepEqual(decoded, tc.AfterValue) { t.Fatalf("complete file document differs: got %#v, want %#v", decoded, tc.AfterValue) }
				info, err := os.Stat(file)
				if err != nil || info.Mode().Perm() != 0600 { t.Fatal("unsafe installed file permissions", err) }
			}
			entries, err := os.ReadDir(root)
			if err != nil { t.Fatal(err) }
			entryNames := make([]string, 0, len(entries))
			for _, entry := range entries { entryNames = append(entryNames, entry.Name()) }
			if !reflect.DeepEqual(entryNames, tc.AfterEntries) { t.Fatalf("unexpected full file changes: %v != %v", entryNames, tc.AfterEntries) }
			// Reopen proves persistence rather than an in-memory configuration clone.
			if err := service.Close(); err != nil { t.Fatal(err) }
			reopened, err := New(root)
			if err != nil { t.Fatal(err) }; defer reopened.Close()
			value, err := reopened.Get()
			if err != nil { t.Fatal(err) }
			want := tc.ExpectedGet
			if actualResult.Success { want = tc.AfterValue }
			if !reflect.DeepEqual(jsonValue(t, value), want) { t.Fatal("reopened config mismatch") }
		})
	}
	for i, tc := range contract.StartupCases {
		capture := StartupCapture{Generation: tc.Capture.Generation, BlockedCode: tc.Capture.BlockedCode}
		if tc.Capture.Notice != nil { capture.MigrationNotice = &MigrationNotice{LegacySourceIgnored: tc.Capture.Notice.LegacySourceIgnored, PreservedUnknownCount: tc.Capture.Notice.PreservedUnknownCount} }
		startup := NewStartup(capture)
		if len(tc.Steps) < 25 { t.Fatal("insufficient startup trust/ack coverage") }
		for j, step := range tc.Steps {
			var actual any
			switch step.Kind {
			case "state": actual = startup.State(step.Caller, step.Trusted)
			case "ack": actual = startup.AppearanceAck(step.Caller, step.Trusted, step.Input)
			default: t.Fatal("unknown startup operation")
			}
			if !reflect.DeepEqual(jsonValue(t, actual), step.Expected) { t.Fatalf("startup case %d step %d (%s %s trust:%s): got %#v, want %#v", i, j, step.Caller, step.Kind, step.TrustFailure, actual, step.Expected) }
		}
	}
}
