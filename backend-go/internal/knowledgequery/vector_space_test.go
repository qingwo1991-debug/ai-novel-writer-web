package knowledgequery

import (
	"context"
	"errors"
	"testing"
)

// The registry decoder is the gate every external vector store passes through:
// these cases pin the same space rules knowledgedomain.Validate applies to a
// registry carried inside the canonical snapshot.

const vectorSpaceValidRegistry = `{"version":1,"activeGeneration":1,"spaces":[` +
	`{"generation":1,"tableName":"chunks__space_1","status":"active","vectorDimension":2,` +
	`"modelFingerprint":"fp-a","distanceMetric":"cosine","createdAt":"2026-01-01T00:00:00.000Z"}]}`

func TestVectorSpaceRegistryDecodesTheActiveSpace(t *testing.T) {
	registry, err := DecodeVectorSpaceRegistry(context.Background(), []byte(vectorSpaceValidRegistry))
	if err != nil {
		t.Fatalf("a valid registry was refused: %v", err)
	}
	space, ok := registry.ActiveSpace()
	if !ok || space.Generation != 1 || space.TableName != "chunks__space_1" ||
		space.VectorDimension != 2 || space.ModelFingerprint != "fp-a" || space.DistanceMetric != "cosine" {
		t.Fatalf("the active space is wrong: %+v", registry)
	}
	empty, err := DecodeVectorSpaceRegistry(context.Background(), []byte(`{"version":1,"activeGeneration":null,"spaces":[]}`))
	if err != nil {
		t.Fatalf("an empty registry was refused: %v", err)
	}
	if _, ok := empty.ActiveSpace(); ok {
		t.Fatal("an empty registry reported an active space")
	}
}

func TestVectorSpaceRegistryRejectsMalformedDocuments(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"empty", ``, ErrInput},
		{"garbage", `{`, ErrDomain},
		{"version", `{"version":2,"activeGeneration":null,"spaces":[]}`, ErrDomain},
		{"trailing", vectorSpaceValidRegistry + `{}`, ErrDomain},
		{"dimension", `{"version":1,"activeGeneration":1,"spaces":[{"generation":1,"tableName":"chunks__space_1","status":"active","vectorDimension":0,"modelFingerprint":"fp","distanceMetric":"cosine","createdAt":"now"}]}`, ErrDomain},
		{"metric", `{"version":1,"activeGeneration":1,"spaces":[{"generation":1,"tableName":"chunks__space_1","status":"active","vectorDimension":2,"modelFingerprint":"fp","distanceMetric":"euclid","createdAt":"now"}]}`, ErrDomain},
		{"fingerprint", `{"version":1,"activeGeneration":1,"spaces":[{"generation":1,"tableName":"chunks__space_1","status":"active","vectorDimension":2,"modelFingerprint":"","distanceMetric":"cosine","createdAt":"now"}]}`, ErrDomain},
		{"table", `{"version":1,"activeGeneration":1,"spaces":[{"generation":2,"tableName":"chunks__space_1","status":"active","vectorDimension":2,"modelFingerprint":"fp","distanceMetric":"cosine","createdAt":"now"}]}`, ErrDomain},
		{"pointer", `{"version":1,"activeGeneration":2,"spaces":[{"generation":1,"tableName":"chunks__space_1","status":"active","vectorDimension":2,"modelFingerprint":"fp","distanceMetric":"cosine","createdAt":"now"}]}`, ErrDomain},
		{"missing pointer", `{"version":1,"activeGeneration":null,"spaces":[{"generation":1,"tableName":"chunks__space_1","status":"active","vectorDimension":2,"modelFingerprint":"fp","distanceMetric":"cosine","createdAt":"now"}]}`, ErrDomain},
		{"two active", `{"version":1,"activeGeneration":1,"spaces":[{"generation":1,"tableName":"chunks__space_1","status":"active","vectorDimension":2,"modelFingerprint":"fp","distanceMetric":"cosine","createdAt":"now"},{"generation":2,"tableName":"chunks__space_2","status":"active","vectorDimension":2,"modelFingerprint":"fp","distanceMetric":"cosine","createdAt":"now"}]}`, ErrDomain},
	}
	for _, test := range cases {
		if _, err := DecodeVectorSpaceRegistry(context.Background(), []byte(test.raw)); !errors.Is(err, test.want) {
			t.Fatalf("%s: got %v, want %v", test.name, err, test.want)
		}
	}
}

func TestVectorSpaceRegistryRefusesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DecodeVectorSpaceRegistry(ctx, []byte(vectorSpaceValidRegistry)); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled decode returned %v", err)
	}
}
