package knowledgequery

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

// VectorHit is the small projection required from a native Lance result. A
// provider must convert its typed rows to this shape without accepting a path
// or model identity from a browser request.
type VectorHit struct {
	ID         string
	DocumentID string
	FileName   string
	Text       string
	Distance   float64
}

// VectorResponse repeats the active-space identity so that a stale or foreign
// native result cannot be mistaken for a result from the admitted snapshot.
type VectorResponse struct {
	TableName        string
	TableVersion     uint64
	Generation       int64
	Dimension        int
	ModelFingerprint string
	DistanceMetric   string
	Hits             []VectorHit
}

// VectorExecutor owns the private native path and its lifetime. The caller
// supplies only an admitted, copied plan; the executor must not derive a path
// from an HTTP argument. The implementation can be backed by
// lancenative.QueryVector in native builds without linking it into the normal
// Go candidate image.
type VectorExecutor interface {
	QueryVector(context.Context, VectorPlan) (VectorResponse, error)
}

type VectorExecutorFunc func(context.Context, VectorPlan) (VectorResponse, error)

func (f VectorExecutorFunc) QueryVector(ctx context.Context, plan VectorPlan) (VectorResponse, error) {
	if f == nil {
		return VectorResponse{}, ErrVectorQueryUnavailable
	}
	return f(ctx, plan)
}

// SearchWithAdmittedVector accepts only a complete, externally verified
// private capture. VerifyCurrent must prove the exact project/source/copies
// before and after the query; it is not an optional optimization. When the
// active vector space is absent or mismatched, the admitted FTS fallback is
// used. A qualified vector plan with filters is refused because the current
// native ABI applies topK before any scope/corpus/copy filtering. Filtering
// those rows in Go would silently omit lower-ranked valid hits.
func SearchWithAdmittedVector(ctx context.Context, snapshot knowledgedomain.Snapshot, root *os.Root, budget knowledgecopy.Budget, request VectorRequest, queryText string, executor VectorExecutor, verifyCurrent func(context.Context) error) ([]SearchResult, error) {
	if err := check(ctx); err != nil {
		return nil, err
	}
	if root == nil || verifyCurrent == nil || len(queryText) > 32*1024 || !utf8.ValidString(queryText) {
		return nil, ErrInput
	}
	if err := verifyCurrent(ctx); err != nil {
		return nil, err
	}
	plan, err := PrepareVectorPlan(ctx, snapshot, root, budget, request)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		capture, err := NewSearchCapture(ctx, snapshot, root, budget)
		if err != nil {
			return nil, err
		}
		results, err := capture.SearchFTS(ctx, queryText, request.TopK, request.Scope, request.ExcludedCorpusKinds)
		if err != nil {
			return nil, err
		}
		if err := verifyCurrent(ctx); err != nil {
			return nil, err
		}
		return results, nil
	}
	if plan.Scope != nil || len(plan.ExcludedCorpusKinds) != 0 || len(plan.ExcludedDocumentIDs) != 0 || executor == nil {
		return nil, ErrVectorQueryUnavailable
	}
	if plan.Generation <= 0 || plan.TableVersion == 0 || plan.TableName != fmt.Sprintf("chunks__space_%d", plan.Generation) ||
		plan.Dimension <= 0 || plan.Dimension > 4096 || plan.TopK <= 0 || plan.TopK > 100 ||
		(plan.DistanceMetric != "l2" && plan.DistanceMetric != "cosine" && plan.DistanceMetric != "dot") {
		return nil, ErrVectorQueryUnavailable
	}
	// The native ABI takes float32. Do not dispatch a vector which became
	// non-finite or a zero cosine vector after narrowing.
	query := make([]float64, len(plan.Query))
	zero := true
	for i, value := range plan.Query {
		narrowed := float32(value)
		if math.IsInf(float64(narrowed), 0) || math.IsNaN(float64(narrowed)) {
			return nil, ErrInput
		}
		query[i] = float64(narrowed)
		zero = zero && narrowed == 0
	}
	if plan.DistanceMetric == "cosine" && zero {
		return nil, ErrInput
	}
	plan.Query = query
	response, err := executor.QueryVector(ctx, *plan)
	if err != nil {
		return nil, err
	}
	results, err := validateVectorResponse(snapshot, *plan, response)
	if err != nil {
		return nil, err
	}
	if err := verifyCurrent(ctx); err != nil {
		return nil, err
	}
	return results, nil
}

func validateVectorResponse(snapshot knowledgedomain.Snapshot, plan VectorPlan, response VectorResponse) ([]SearchResult, error) {
	if response.TableName != plan.TableName || response.TableVersion != plan.TableVersion ||
		response.Generation != plan.Generation || response.Dimension != plan.Dimension ||
		response.ModelFingerprint != plan.ModelFingerprint || response.DistanceMetric != plan.DistanceMetric ||
		len(response.Hits) > plan.TopK {
		return nil, ErrDomain
	}
	var indexed map[string]VectorHit
	for _, table := range snapshot.Tables {
		if table.Name != plan.TableName {
			continue
		}
		if table.Version != plan.TableVersion {
			return nil, ErrDomain
		}
		indexed = make(map[string]VectorHit, len(table.Records))
		for _, row := range table.Records {
			id, idOK := row["id"].(string)
			docID, docOK := row["docId"].(string)
			fileName, fileOK := row["fileName"].(string)
			text, textOK := row["text"].(string)
			if !idOK || !docOK || !fileOK || !textOK || id == "" || docID == "" || !utf8.ValidString(id+docID+fileName+text) {
				return nil, ErrDomain
			}
			if _, duplicate := indexed[id]; duplicate {
				return nil, ErrDomain
			}
			indexed[id] = VectorHit{ID: id, DocumentID: docID, FileName: fileName, Text: text}
		}
		break
	}
	if indexed == nil {
		return nil, ErrDomain
	}
	results := make([]SearchResult, 0, len(response.Hits))
	seen := make(map[string]bool, len(response.Hits))
	previousDistance := math.Inf(-1)
	for _, hit := range response.Hits {
		if seen[hit.ID] || hit.ID == "" || !utf8.ValidString(hit.ID+hit.DocumentID+hit.FileName+hit.Text) ||
			math.IsNaN(hit.Distance) || math.IsInf(hit.Distance, 0) || hit.Distance < previousDistance {
			return nil, ErrDomain
		}
		previousDistance = hit.Distance
		actual, exists := indexed[hit.ID]
		if !exists || actual.DocumentID != hit.DocumentID || actual.FileName != hit.FileName || actual.Text != hit.Text {
			return nil, ErrDomain
		}
		seen[hit.ID] = true
		score := 1 / (1 + hit.Distance)
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return nil, ErrDomain
		}
		results = append(results, SearchResult{Text: hit.Text, Score: score, FileName: hit.FileName})
	}
	return results, nil
}

// IsVectorUnavailable distinguishes a deliberately unsupported filtered
// vector query from invalid input or a native failure. It never turns an
// executor error into an FTS result.
func IsVectorUnavailable(err error) bool { return errors.Is(err, ErrVectorQueryUnavailable) }

// NativeVectorIdentitySafe is a small helper for adapters converting the
// native result's record maps: it rejects absent or malformed identity fields.
func NativeVectorIdentitySafe(record map[string]any) (VectorHit, error) {
	if len(record) == 0 || len(record) > 64 {
		return VectorHit{}, ErrDomain
	}
	id, idOK := record["id"].(string)
	doc, docOK := record["docId"].(string)
	file, fileOK := record["fileName"].(string)
	text, textOK := record["text"].(string)
	distance, distanceOK := record["_distance"].(float64)
	if !idOK || !docOK || !fileOK || !textOK || !distanceOK || id == "" || doc == "" ||
		!utf8.ValidString(strings.Join([]string{id, doc, file, text}, "")) ||
		math.IsNaN(distance) || math.IsInf(distance, 0) {
		return VectorHit{}, ErrDomain
	}
	return VectorHit{ID: id, DocumentID: doc, FileName: file, Text: text, Distance: distance}, nil
}
