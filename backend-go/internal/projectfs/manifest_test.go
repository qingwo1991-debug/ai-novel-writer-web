package projectfs

import (
	"bytes"
	"strings"
	"testing"
)

const fixtureID = "12345678-1234-4234-8234-123456789abc"
const fixtureManifest = `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"12345678-1234-4234-8234-123456789abc","createdAt":"2026-01-02T03:04:05.123Z","storageFormat":"ai-novel","storageVersion":1}`

func TestManifestCanonicalContract(t *testing.T) {
	m, err := ParseManifest([]byte(fixtureManifest))
	if err != nil || m.ProjectID != fixtureID || m.SchemaVersion != 1 || m.StorageVersion != 1 {
		t.Fatalf("parse = %#v, %v", m, err)
	}
	for _, date := range []string{"2026", "2026-01", "2026-01-02", "2026-01-02T03:04", "2026-01-02T03:04:05", "2026-01-02T03:04:05+08:00", "2026-01-02T03:04:05.123456789Z"} {
		t.Run(date, func(t *testing.T) {
			data := strings.Replace(fixtureManifest, "2026-01-02T03:04:05.123Z", date, 1)
			if _, err := ParseManifest([]byte(data)); err != nil {
				t.Fatalf("common ISO input rejected: %v", err)
			}
		})
	}
	// JavaScript treats 1.0 and 1e0 as the same numeric schema version.
	if _, err := ParseManifest([]byte(strings.ReplaceAll(fixtureManifest, ":1", ":1.0"))); err != nil {
		t.Fatalf("equivalent numeric versions: %v", err)
	}
}

func TestManifestRejectsInvalidAndDuplicateKeys(t *testing.T) {
	cases := map[string]string{
		"null":                               "null",
		"array":                              "[]",
		"trailing":                           fixtureManifest + `{}`,
		"extra":                              strings.Replace(fixtureManifest, `"schemaVersion":1`, `"extra":true,"schemaVersion":1`, 1),
		"missing":                            strings.Replace(fixtureManifest, `"schemaVersion":1,`, "", 1),
		"duplicate":                          strings.Replace(fixtureManifest, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1),
		"escaped duplicate":                  strings.Replace(fixtureManifest, `"schemaVersion":1`, `"schemaVersion":1,"schema\u0056ersion":1`, 1),
		"wrong UUID version":                 strings.Replace(fixtureManifest, "-4234-", "-6234-", 1),
		"wrong UUID variant":                 strings.Replace(fixtureManifest, "-8234-", "-7234-", 1),
		"wrong kind":                         strings.Replace(fixtureManifest, "ai-novel-project", "vela-project", 1),
		"string version":                     strings.Replace(fixtureManifest, `"schemaVersion":1`, `"schemaVersion":"1"`, 1),
		"null version":                       strings.Replace(fixtureManifest, `"schemaVersion":1`, `"schemaVersion":null`, 1),
		"null date":                          strings.Replace(fixtureManifest, `"2026-01-02T03:04:05.123Z"`, "null", 1),
		"rollover date":                      strings.Replace(fixtureManifest, "2026-01-02T03:04:05.123Z", "2026-02-30", 1),
		"locale date intentionally stricter": strings.Replace(fixtureManifest, "2026-01-02T03:04:05.123Z", "Jan 2 2026", 1),
		"bad date":                           strings.Replace(fixtureManifest, "2026-01-02T03:04:05.123Z", "not-a-date", 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(data)); err == nil {
				t.Fatal("invalid manifest admitted")
			}
		})
	}
	if _, err := ParseManifest(bytes.Repeat([]byte(" "), int(manifestLimit)+1)); err == nil {
		t.Fatal("oversized manifest admitted")
	}
	if _, err := ParseManifest([]byte(strings.Replace(fixtureManifest, "ai-novel-project", "ai-novel-\xffproject", 1))); err == nil {
		t.Fatal("invalid UTF-8 admitted")
	}
}

func TestStrictJSONNestedDuplicateKeys(t *testing.T) {
	for _, data := range []string{`{"x":{"a":1,"a":2}}`, `{"x":[{"a":1,"a":2}]}`, `{"x":1} {"x":2}`, `{"x":[1,]}`} {
		if _, err := strictObject([]byte(data)); err == nil {
			t.Fatalf("invalid JSON accepted: %s", data)
		}
	}
}
