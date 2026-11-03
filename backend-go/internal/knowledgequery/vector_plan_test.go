package knowledgequery

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestVectorPlanExactActiveIdentityAndCopyFence(t *testing.T) {
	snapshot, root, path := queryFixture(t)
	queryCopy(t, path, "doc-星海🙂", "edited text", true, true, false)
	queryAddSpace(t, &snapshot, path, "active", 2, true)
	request := VectorRequest{Query: []float64{0.25, 0.5}, ModelFingerprint: "synthetic", DistanceMetric: "l2", TopK: 5,
		Scope: &ChapterScope{From: 1, To: 2}, ExcludedCorpusKinds: []string{"reference"}}
	plan, err := PrepareVectorPlan(context.Background(), snapshot, root, queryBudget(), request)
	if err != nil || plan == nil {
		t.Fatal(plan, err)
	}
	if plan.TableName != "chunks__space_1" || plan.TableVersion != 1 || plan.Generation != 1 || plan.Dimension != 2 ||
		plan.ModelFingerprint != "synthetic" || plan.DistanceMetric != "l2" || plan.TopK != 5 ||
		!reflect.DeepEqual(plan.ExcludedDocumentIDs, []string{"doc-星海🙂"}) || !plan.ApplyCorpusFilter || plan.ApplyChapterScope {
		t.Fatalf("wrong bounded plan: %+v", plan)
	}
	request.Query[0] = 7
	request.Scope.From = 99
	request.ExcludedCorpusKinds[0] = "changed"
	if plan.Query[0] != 0.25 || plan.Scope.From != 1 || plan.ExcludedCorpusKinds[0] != "reference" {
		t.Fatal("plan borrowed mutable request data")
	}
}

func TestVectorPlanFallbackOnMismatchedOrInactiveSpace(t *testing.T) {
	for _, name := range []string{"dimension", "model", "metric", "missing-identity", "zero-topk", "inactive"} {
		t.Run(name, func(t *testing.T) {
			snapshot, root, path := queryFixture(t)
			queryCopy(t, path, "doc-星海🙂", "current text", true, false, false)
			active := name != "inactive"
			status := "active"
			if !active {
				status = "building"
			}
			queryAddSpace(t, &snapshot, path, status, 2, active)
			request := VectorRequest{Query: []float64{0.25, 0.5}, ModelFingerprint: "synthetic", DistanceMetric: "l2", TopK: 5}
			switch name {
			case "dimension":
				request.Query = []float64{0.25}
			case "model":
				request.ModelFingerprint = "other"
			case "metric":
				request.DistanceMetric = "cosine"
			case "missing-identity":
				request.ModelFingerprint = ""
			case "zero-topk":
				request.TopK = 0
			}
			plan, err := PrepareVectorPlan(context.Background(), snapshot, root, queryBudget(), request)
			if plan != nil || err != nil {
				t.Fatalf("unsafe vector plan: %+v, %v", plan, err)
			}
		})
	}
}

func TestVectorPlanRejectsMalformedAndCancelledInputs(t *testing.T) {
	snapshot, root, path := queryFixture(t)
	queryCopy(t, path, "doc-星海🙂", "current text", true, false, false)
	queryAddSpace(t, &snapshot, path, "active", 2, true)
	base := VectorRequest{Query: []float64{0.25, 0.5}, ModelFingerprint: "synthetic", DistanceMetric: "l2", TopK: 5}
	bad := base
	bad.Query = []float64{math.NaN(), 0.5}
	if _, err := PrepareVectorPlan(context.Background(), snapshot, root, queryBudget(), bad); !errors.Is(err, ErrInput) {
		t.Fatalf("nonfinite vector accepted: %v", err)
	}
	bad = base
	bad.Query = []float64{math.MaxFloat64, 0.5}
	if _, err := PrepareVectorPlan(context.Background(), snapshot, root, queryBudget(), bad); !errors.Is(err, ErrInput) {
		t.Fatalf("float32-overflowing vector accepted: %v", err)
	}
	bad = base
	bad.TopK = 101
	if _, err := PrepareVectorPlan(context.Background(), snapshot, root, queryBudget(), bad); !errors.Is(err, ErrInput) {
		t.Fatalf("unbounded result request accepted: %v", err)
	}
	badSnapshot := snapshot
	badSnapshot.Registry = json.RawMessage(`{"version":1,"activeGeneration":99,"spaces":[]}`)
	if _, err := PrepareVectorPlan(context.Background(), badSnapshot, root, queryBudget(), base); !errors.Is(err, ErrDomain) {
		t.Fatalf("invalid active pointer accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareVectorPlan(ctx, snapshot, root, queryBudget(), base); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}
