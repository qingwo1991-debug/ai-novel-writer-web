//go:build lance_native && cgo

package lancenative

/*
#cgo CFLAGS: -I${SRCDIR}/../../native-lance/include
#include <stdlib.h>
#include "novel_lance.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unsafe"
)

// VectorRequest is a low-level query against an already validated private
// capture. The model fingerprint is a binding supplied by the caller; Lance
// does not store or authenticate it. Do not call this without registry and
// document-copy currentness checks in the owning knowledge service.
type VectorRequest struct {
	Table            string    `json:"table"`
	Version          uint64    `json:"version"`
	Generation       uint64    `json:"generation"`
	Dimension        int       `json:"dimension"`
	Metric           string    `json:"metric"`
	ModelFingerprint string    `json:"modelFingerprint"`
	TopK             int       `json:"topK"`
	Vector           []float32 `json:"vector"`
}

type VectorResult struct {
	Table            string           `json:"table"`
	Version          uint64           `json:"version"`
	Generation       uint64           `json:"generation"`
	Dimension        int              `json:"dimension"`
	Metric           string           `json:"metric"`
	ModelFingerprint string           `json:"modelFingerprint"`
	Records          []map[string]any `json:"records"`
}

const maxVectorResults = 4 * 1024 * 1024

func validVectorRequest(request VectorRequest) bool {
	if request.Generation == 0 || request.Version == 0 || request.Dimension <= 0 || request.Dimension > 4096 ||
		request.Table != fmt.Sprintf("chunks__space_%d", request.Generation) ||
		request.TopK <= 0 || request.TopK > 100 || len(request.Vector) != request.Dimension ||
		request.ModelFingerprint == "" || len(request.ModelFingerprint) > 512 || strings.ContainsRune(request.ModelFingerprint, 0) {
		return false
	}
	switch request.Metric {
	case "l2", "cosine", "dot":
	default:
		return false
	}
	zero := true
	for _, value := range request.Vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
		zero = zero && value == 0
	}
	return request.Metric != "cosine" || !zero
}

// QueryVector performs a synchronous native flat search on a private copy.
// Cancellation is checked before and after dispatch; it cannot interrupt a
// native call already in progress. This API never mutates the Lance database.
func QueryVector(ctx context.Context, path string, request VectorRequest) (VectorResult, error) {
	if ctx == nil || !validVectorRequest(request) {
		return VectorResult{}, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return VectorResult{}, err
	}
	p, plen, freePath, err := pathBytes(path)
	if err != nil {
		return VectorResult{}, err
	}
	defer freePath()
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > 64*1024 {
		return VectorResult{}, ErrInput
	}
	body := C.CBytes(raw)
	defer C.free(body)
	var result, diagnostic C.NvBytes
	status := C.nv_lance_vector_query((*C.uint8_t)(p), plen, (*C.uint8_t)(body), C.size_t(len(raw)), &result, &diagnostic)
	defer C.nv_lance_free(&result)
	defer C.nv_lance_free(&diagnostic)
	if status != 0 || result.ptr == nil || result.len == 0 || result.len > maxVectorResults {
		return VectorResult{}, fmt.Errorf("%w (status %d)", ErrNative, int32(status))
	}
	var decoded VectorResult
	if err := json.Unmarshal(C.GoBytes(unsafe.Pointer(result.ptr), C.int(result.len)), &decoded); err != nil ||
		decoded.Table != request.Table || decoded.Version != request.Version ||
		decoded.Generation != request.Generation || decoded.Dimension != request.Dimension ||
		decoded.Metric != request.Metric || decoded.ModelFingerprint != request.ModelFingerprint ||
		len(decoded.Records) > request.TopK {
		return VectorResult{}, ErrNative
	}
	for _, record := range decoded.Records {
		distance, ok := record["_distance"].(float64)
		if !ok || math.IsNaN(distance) || math.IsInf(distance, 0) {
			return VectorResult{}, ErrNative
		}
	}
	if err := ctx.Err(); err != nil {
		return VectorResult{}, err
	}
	return decoded, nil
}
