//go:build lance_native && cgo

package knowledgequery

import (
	"context"
	"os"
	"path/filepath"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/lancenative"
)

// NativeLanceVectorExecutor is an adapter over the read-only native ABI. The
// path must come from a trusted, pinned private knowledge capture under the
// caller's source lock. This type does not admit an author path by itself.
type NativeLanceVectorExecutor struct {
	privatePath string
	pathInfo    os.FileInfo
}

func NewNativeLanceVectorExecutor(privatePath string) (*NativeLanceVectorExecutor, error) {
	if privatePath == "" || !filepath.IsAbs(privatePath) || filepath.Clean(privatePath) != privatePath {
		return nil, ErrInput
	}
	info, err := os.Lstat(privatePath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInput
	}
	return &NativeLanceVectorExecutor{privatePath: privatePath, pathInfo: info}, nil
}

func (n *NativeLanceVectorExecutor) QueryVector(ctx context.Context, plan VectorPlan) (VectorResponse, error) {
	if n == nil || n.privatePath == "" || plan.Generation <= 0 || len(plan.Query) != plan.Dimension {
		return VectorResponse{}, ErrVectorQueryUnavailable
	}
	before, err := os.Lstat(n.privatePath)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, n.pathInfo) {
		return VectorResponse{}, ErrDomain
	}
	vector := make([]float32, len(plan.Query))
	for i, value := range plan.Query {
		vector[i] = float32(value)
	}
	result, err := lancenative.QueryVector(ctx, n.privatePath, lancenative.VectorRequest{
		Table: plan.TableName, Version: plan.TableVersion, Generation: uint64(plan.Generation),
		Dimension: plan.Dimension, Metric: plan.DistanceMetric,
		ModelFingerprint: plan.ModelFingerprint, TopK: plan.TopK, Vector: vector,
	})
	if err != nil {
		return VectorResponse{}, err
	}
	after, err := os.Lstat(n.privatePath)
	if err != nil || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(after, n.pathInfo) {
		return VectorResponse{}, ErrDomain
	}
	response := VectorResponse{TableName: result.Table, TableVersion: result.Version,
		Generation: int64(result.Generation), Dimension: result.Dimension,
		ModelFingerprint: result.ModelFingerprint, DistanceMetric: result.Metric,
		Hits: make([]VectorHit, 0, len(result.Records))}
	for _, row := range result.Records {
		hit, err := NativeVectorIdentitySafe(row)
		if err != nil {
			return VectorResponse{}, err
		}
		response.Hits = append(response.Hits, hit)
	}
	if err := ctx.Err(); err != nil {
		return VectorResponse{}, err
	}
	return response, nil
}

var _ VectorExecutor = (*NativeLanceVectorExecutor)(nil)
