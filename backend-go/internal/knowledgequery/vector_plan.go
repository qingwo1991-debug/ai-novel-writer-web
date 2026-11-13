package knowledgequery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

// ErrVectorQueryUnavailable is the execution boundary. The current native
// Lance bridge exports typed snapshots but has no vector query operation.
// A qualified plan cannot be presented as a search result or used to bypass
// the original full-text fallback.
var ErrVectorQueryUnavailable = errors.New("KNOWLEDGE_VECTOR_QUERY_UNAVAILABLE")

// VectorRequest contains a trusted embedding result and the model identity
// used to produce it. Query vectors from a different model or distance metric
// are never sent to the active space.
type VectorRequest struct {
	Query               []float64
	ModelFingerprint    string
	DistanceMetric      string
	TopK                int
	Scope               *ChapterScope
	ExcludedCorpusKinds []string
}

// VectorPlan is a copied, bounded query specification for a future native
// executor. It contains no native handle and grants no source read authority.
// ExcludedDocumentIDs are managed copies whose current text differs from the
// indexed text; the executor must enforce them before returning any rows.
type VectorPlan struct {
	TableName           string
	TableVersion        uint64
	Generation          int64
	Dimension           int
	ModelFingerprint    string
	DistanceMetric      string
	Query               []float64
	TopK                int
	Scope               *ChapterScope
	ExcludedCorpusKinds []string
	ExcludedDocumentIDs []string
	ApplyChapterScope   bool
	ApplyCorpusFilter   bool
}

// PrepareVectorPlan validates the same bounded native-admitted typed snapshot
// and managed copies as the FTS capture. A nil plan and nil error means the
// active space cannot serve this request, so the caller may use SearchFTS.
// A non-nil plan still requires a separately qualified native query executor.
// The caller must hold source writer exclusion and prove source/copy equality
// before and after constructing this plan; this function does not open author
// data or establish those physical proofs.
func PrepareVectorPlan(ctx context.Context, snapshot knowledgedomain.Snapshot, root *os.Root, budget knowledgecopy.Budget, request VectorRequest) (*VectorPlan, error) {
	if err := check(ctx); err != nil {
		return nil, err
	}
	if err := validateVectorRequest(request); err != nil {
		return nil, err
	}
	capture, err := NewSearchCapture(ctx, snapshot, root, budget)
	if err != nil {
		return nil, err
	}
	if request.TopK == 0 || len(request.Query) == 0 || request.ModelFingerprint == "" || request.DistanceMetric == "" || len(snapshot.Registry) == 0 {
		return nil, nil
	}
	var registry struct {
		ActiveGeneration *json.Number `json:"activeGeneration"`
		Spaces           []struct {
			Generation       json.Number `json:"generation"`
			TableName        string      `json:"tableName"`
			Status           string      `json:"status"`
			VectorDimension  json.Number `json:"vectorDimension"`
			ModelFingerprint string      `json:"modelFingerprint"`
			DistanceMetric   string      `json:"distanceMetric"`
		} `json:"spaces"`
	}
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Registry))
	decoder.UseNumber()
	if err := decoder.Decode(&registry); err != nil {
		return nil, ErrDomain
	}
	if registry.ActiveGeneration == nil {
		return nil, nil
	}
	active := integer(*registry.ActiveGeneration)
	for _, space := range registry.Spaces {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if integer(space.Generation) != active || space.Status != "active" {
			continue
		}
		if int64(len(request.Query)) != integer(space.VectorDimension) ||
			space.ModelFingerprint != strings.TrimSpace(request.ModelFingerprint) ||
			space.DistanceMetric != strings.ToLower(strings.TrimSpace(request.DistanceMetric)) {
			return nil, nil
		}
		var version uint64
		found := false
		chapterField, corpusField := false, false
		for _, table := range snapshot.Tables {
			if table.Name == space.TableName {
				version = table.Version
				found = true
				for _, field := range table.Schema {
					if field.Name == "chapterNumber" {
						chapterField = true
					}
					if field.Name == "corpusKind" {
						corpusField = true
					}
				}
				break
			}
		}
		if !found {
			return nil, ErrDomain
		}
		excluded := make([]string, 0, len(capture.stale))
		for id := range capture.stale {
			excluded = append(excluded, id)
		}
		sort.Strings(excluded)
		plan := &VectorPlan{
			TableName: space.TableName, TableVersion: version,
			Generation: active, Dimension: len(request.Query),
			ModelFingerprint: space.ModelFingerprint, DistanceMetric: space.DistanceMetric,
			Query: append([]float64(nil), request.Query...), TopK: request.TopK,
			ExcludedCorpusKinds: append([]string(nil), request.ExcludedCorpusKinds...),
			ExcludedDocumentIDs: excluded,
			ApplyChapterScope:   chapterField && request.Scope != nil,
			ApplyCorpusFilter:   corpusField && len(request.ExcludedCorpusKinds) > 0,
		}
		if request.Scope != nil {
			copy := *request.Scope
			plan.Scope = &copy
		}
		return plan, nil
	}
	return nil, nil
}

// validateVectorRequest is the bounded request admission both plan builders
// share, so an external-store plan cannot accept a shape the native one refuses.
func validateVectorRequest(request VectorRequest) error {
	if request.TopK < 0 || request.TopK > 100 || len(request.Query) > 4096 || len(request.ExcludedCorpusKinds) > 32 ||
		len(request.ModelFingerprint) > 1024 || !utf8.ValidString(request.ModelFingerprint) ||
		len(request.DistanceMetric) > 32 || !utf8.ValidString(request.DistanceMetric) {
		return ErrInput
	}
	if request.Scope != nil && (request.Scope.From < 0 || request.Scope.To < request.Scope.From || request.Scope.To > 9007199254740991) {
		return ErrInput
	}
	for _, value := range request.Query {
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > math.MaxFloat32 {
			return ErrInput
		}
	}
	for _, kind := range request.ExcludedCorpusKinds {
		if len(kind) > 1024 || !utf8.ValidString(kind) {
			return ErrInput
		}
	}
	return nil
}

// PrepareVectorPlanOverSpaces is PrepareVectorPlan for an admitted vector store
// that keeps its vectors outside the canonical domain snapshot. It validates the
// same bounded snapshot, managed copies and request, but takes the registry from
// the caller's already-verified store and the space table's version from that
// store's own pin instead of requiring a “chunks__space_<generation>“ table to
// appear in snapshot.Tables: a real vector table of 4096-dimension rows cannot
// fit the domain validator's hard cell budget, so it is not part of the
// canonical document. The plan is still refused unless the registry's active
// space matches the request's fingerprint, dimension and metric exactly.
//
// A nil plan and nil error means the external space cannot serve this request,
// so the caller may use SearchFTS. A non-nil plan still requires a qualified
// executor (SearchWithAdmittedVectorOverSpaces).
func PrepareVectorPlanOverSpaces(ctx context.Context, snapshot knowledgedomain.Snapshot, root *os.Root, budget knowledgecopy.Budget, request VectorRequest, registry VectorSpaceRegistry, tableVersion uint64) (*VectorPlan, error) {
	if err := check(ctx); err != nil {
		return nil, err
	}
	if err := validateVectorRequest(request); err != nil {
		return nil, err
	}
	capture, err := NewSearchCapture(ctx, snapshot, root, budget)
	if err != nil {
		return nil, err
	}
	if request.TopK == 0 || len(request.Query) == 0 || request.ModelFingerprint == "" || request.DistanceMetric == "" {
		return nil, nil
	}
	if tableVersion == 0 {
		return nil, ErrDomain
	}
	space, ok := registry.ActiveSpace()
	if !ok {
		return nil, nil
	}
	if len(request.Query) != space.VectorDimension ||
		space.ModelFingerprint != strings.TrimSpace(request.ModelFingerprint) ||
		space.DistanceMetric != strings.ToLower(strings.TrimSpace(request.DistanceMetric)) {
		return nil, nil
	}
	excluded := make([]string, 0, len(capture.stale))
	for id := range capture.stale {
		excluded = append(excluded, id)
	}
	sort.Strings(excluded)
	plan := &VectorPlan{
		TableName: space.TableName, TableVersion: tableVersion,
		Generation: space.Generation, Dimension: space.VectorDimension,
		ModelFingerprint: space.ModelFingerprint, DistanceMetric: space.DistanceMetric,
		Query: append([]float64(nil), request.Query...), TopK: request.TopK,
		ExcludedCorpusKinds: append([]string(nil), request.ExcludedCorpusKinds...),
		ExcludedDocumentIDs: excluded,
		ApplyChapterScope:   capture.hasChapter && request.Scope != nil,
		ApplyCorpusFilter:   capture.hasCorpusKind && len(request.ExcludedCorpusKinds) > 0,
	}
	if request.Scope != nil {
		copy := *request.Scope
		plan.Scope = &copy
	}
	return plan, nil
}
