package knowledgequery

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

func vectorExecuteFixture(t *testing.T) (VectorRequest, func(context.Context) error, func() int, func() int, func() VectorResponse, func() VectorExecutor) {
	t.Helper()
	request := VectorRequest{Query: []float64{0.1, 0.5}, ModelFingerprint: "synthetic", DistanceMetric: "l2", TopK: 2}
	checks, calls := 0, 0
	verify := func(context.Context) error { checks++; return nil }
	response := func() VectorResponse {
		return VectorResponse{TableName: "chunks__space_1", TableVersion: 1, Generation: 1,
			Dimension: 2, ModelFingerprint: "synthetic", DistanceMetric: "l2",
			Hits: []VectorHit{{ID: "first", DocumentID: "doc-星海🙂", FileName: "source.txt", Text: "indexed one", Distance: 0.25}}}
	}
	executor := func() VectorExecutor {
		return VectorExecutorFunc(func(_ context.Context, plan VectorPlan) (VectorResponse, error) {
			calls++
			if plan.TableName != "chunks__space_1" || plan.TableVersion != 1 || plan.Generation != 1 ||
				plan.Dimension != 2 || plan.ModelFingerprint != "synthetic" || plan.DistanceMetric != "l2" ||
				plan.TopK != 2 || plan.Query[0] != float64(float32(0.1)) {
				t.Fatalf("unqualified native plan: %+v", plan)
			}
			return response(), nil
		})
	}
	return request, verify, func() int { return checks }, func() int { return calls }, response, executor
}

func TestVectorExecutionUsesAdmittedIdentityAndSourceFence(t *testing.T) {
	snapshot, root, path := queryFixture(t)
	queryCopy(t, path, "doc-星海🙂", "current text", true, false, false)
	queryAddSpace(t, &snapshot, path, "active", 2, true)
	request, verify, checks, calls, _, executor := vectorExecuteFixture(t)
	results, err := SearchWithAdmittedVector(context.Background(), snapshot, root, queryBudget(), request, "indexed", executor(), verify)
	if err != nil || !reflect.DeepEqual(results, []SearchResult{{Text: "indexed one", Score: 0.8, FileName: "source.txt"}}) || calls() != 1 || checks() != 2 {
		t.Fatalf("bad vector execution: %+v %v calls=%d checks=%d", results, err, calls(), checks())
	}
}

func TestVectorExecutionFallsBackOnlyForNoVectorPlan(t *testing.T) {
	snapshot, root, path := queryFixture(t)
	queryCopy(t, path, "doc-星海🙂", "current text", true, false, false)
	queryAddSpace(t, &snapshot, path, "active", 2, true)
	request, verify, checks, calls, _, executor := vectorExecuteFixture(t)
	request.ModelFingerprint = "other-model"
	results, err := SearchWithAdmittedVector(context.Background(), snapshot, root, queryBudget(), request, "indexed", executor(), verify)
	if err != nil || len(results) != 2 || calls() != 0 || checks() != 2 {
		t.Fatalf("no-vector fallback: %+v %v calls=%d checks=%d", results, err, calls(), checks())
	}
	request.ModelFingerprint = "synthetic"
	request.Scope = &ChapterScope{From: 1, To: 2}
	if _, err := SearchWithAdmittedVector(context.Background(), snapshot, root, queryBudget(), request, "indexed", executor(), verify); !errors.Is(err, ErrVectorQueryUnavailable) || calls() != 0 {
		t.Fatalf("post-limit scope filtering accepted: %v calls=%d", err, calls())
	}
	request.Scope = nil
	request.ExcludedCorpusKinds = []string{"reference"}
	if _, err := SearchWithAdmittedVector(context.Background(), snapshot, root, queryBudget(), request, "indexed", executor(), verify); !errors.Is(err, ErrVectorQueryUnavailable) || calls() != 0 {
		t.Fatalf("post-limit corpus filtering accepted: %v calls=%d", err, calls())
	}
	request.ExcludedCorpusKinds = nil
	queryCopy(t, path, "doc-星海🙂", "dirty copy", true, true, false)
	if _, err := SearchWithAdmittedVector(context.Background(), snapshot, root, queryBudget(), request, "indexed", executor(), verify); !errors.Is(err, ErrVectorQueryUnavailable) || calls() != 0 {
		t.Fatalf("post-limit stale-copy filtering accepted: %v calls=%d", err, calls())
	}
}

func TestVectorExecutionRejectsForeignRowsAndNativeFailures(t *testing.T) {
	snapshot, root, path := queryFixture(t)
	queryCopy(t, path, "doc-星海🙂", "current text", true, false, false)
	queryAddSpace(t, &snapshot, path, "active", 2, true)
	request, verify, _, _, response, _ := vectorExecuteFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*VectorResponse)
	}{
		{"table", func(r *VectorResponse) { r.TableName = "chunks__space_2" }},
		{"version", func(r *VectorResponse) { r.TableVersion++ }},
		{"model", func(r *VectorResponse) { r.ModelFingerprint = "other" }},
		{"unknown-id", func(r *VectorResponse) { r.Hits[0].ID = "unknown" }},
		{"changed-text", func(r *VectorResponse) { r.Hits[0].Text = "tampered" }},
		{"duplicate", func(r *VectorResponse) { r.Hits = append(r.Hits, r.Hits[0]) }},
		{"bad-distance", func(r *VectorResponse) { r.Hits[0].Distance = math.NaN() }},
		{"unrepresentable-score", func(r *VectorResponse) { r.Hits[0].Distance = -1 }},
		{"unsorted", func(r *VectorResponse) {
			r.Hits = append(r.Hits, VectorHit{ID: "second", DocumentID: "doc-星海🙂", FileName: "source.txt", Text: "indexed two", Distance: 0.1})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := response()
			tc.change(&bad)
			exec := VectorExecutorFunc(func(context.Context, VectorPlan) (VectorResponse, error) { return bad, nil })
			if _, err := SearchWithAdmittedVector(context.Background(), snapshot, root, queryBudget(), request, "indexed", exec, verify); !errors.Is(err, ErrDomain) {
				t.Fatalf("unadmitted native row accepted: %v", err)
			}
		})
	}
	nativeErr := errors.New("NATIVE_FAILED")
	exec := VectorExecutorFunc(func(context.Context, VectorPlan) (VectorResponse, error) { return VectorResponse{}, nativeErr })
	if _, err := SearchWithAdmittedVector(context.Background(), snapshot, root, queryBudget(), request, "indexed", exec, verify); !errors.Is(err, nativeErr) {
		t.Fatalf("native failure silently fell back: %v", err)
	}
}

func TestVectorExecutionCurrentnessAndMapConversion(t *testing.T) {
	snapshot, root, path := queryFixture(t)
	queryCopy(t, path, "doc-星海🙂", "current text", true, false, false)
	queryAddSpace(t, &snapshot, path, "active", 2, true)
	request, _, _, _, response, _ := vectorExecuteFixture(t)
	changed := errors.New("SOURCE_CHANGED")
	checks, calls := 0, 0
	verify := func(context.Context) error {
		checks++
		if checks == 2 {
			return changed
		}
		return nil
	}
	exec := VectorExecutorFunc(func(context.Context, VectorPlan) (VectorResponse, error) { calls++; return response(), nil })
	if _, err := SearchWithAdmittedVector(context.Background(), snapshot, root, queryBudget(), request, "indexed", exec, verify); !errors.Is(err, changed) || calls != 1 || checks != 2 {
		t.Fatalf("source-currentness fence failed: %v calls=%d checks=%d", err, calls, checks)
	}
	if _, err := SearchWithAdmittedVector(context.Background(), snapshot, root, queryBudget(), request, "indexed", exec, nil); !errors.Is(err, ErrInput) {
		t.Fatalf("missing currentness verifier accepted: %v", err)
	}
	hit, err := NativeVectorIdentitySafe(map[string]any{"id": "first", "docId": "doc-星海🙂", "fileName": "source.txt", "text": "indexed one", "_distance": 0.25})
	if err != nil || hit.ID != "first" || hit.Distance != 0.25 {
		t.Fatal(hit, err)
	}
	if _, err := NativeVectorIdentitySafe(map[string]any{"id": "first", "docId": "doc-星海🙂", "fileName": "source.txt", "text": "indexed one"}); !errors.Is(err, ErrDomain) {
		t.Fatalf("missing native distance accepted: %v", err)
	}
}
