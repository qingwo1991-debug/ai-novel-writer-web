package knowledgequery

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

// This file decodes the embedding-space registry an external (non-snapshot)
// vector store publishes. knowledgedomain.Validate validates the same shape for
// a registry carried inside the canonical snapshot; an external store's file is
// not the canonical snapshot, so it needs its own bounded decoder whose rules
// match that validator's space rules exactly:
//
//   - version 1, at most 32 spaces, one space per generation and per table name;
//   - a table name of the form chunks__space_<generation>;
//   - status active|inactive|building, a dimension in 1..4096 (the ceiling
//     SearchWithAdmittedVector enforces), a non-empty model fingerprint, a
//     created timestamp and metric l2|cosine|dot;
//   - activeGeneration names the one space whose status is active, and no space
//     is active when the pointer is absent.
//
// The registry never carries a credential and never carries author text, so a
// decode failure is symbolic and safe to surface.
const (
	vectorSpaceMaxSpaces = 32
	vectorSpaceMaxBytes  = 4 << 20
)

// VectorSpace is one registered embedding space, the projection
// PrepareVectorPlanOverSpaces needs and the browser registry publishes.
type VectorSpace struct {
	Generation       int64
	TableName        string
	Status           string
	VectorDimension  int
	ModelFingerprint string
	DistanceMetric   string
	CreatedAt        string
}

// VectorSpaceRegistry is a decoded embedding-spaces.json: the pointer to the
// active generation plus every registered space. It is a value type so a caller
// can hand it to PrepareVectorPlanOverSpaces without sharing mutable state.
type VectorSpaceRegistry struct {
	ActiveGeneration *int64
	Spaces           []VectorSpace
}

// DecodeVectorSpaceRegistry validates and copies one registry document. Any
// malformed field is ErrDomain; unavailable input is ErrInput. It never returns
// partial state.
func DecodeVectorSpaceRegistry(ctx context.Context, raw []byte) (VectorSpaceRegistry, error) {
	if err := check(ctx); err != nil {
		return VectorSpaceRegistry{}, err
	}
	if len(raw) == 0 || len(raw) > vectorSpaceMaxBytes || !utf8.Valid(raw) {
		return VectorSpaceRegistry{}, ErrInput
	}
	var wire struct {
		Version          *json.Number `json:"version"`
		ActiveGeneration *json.Number `json:"activeGeneration"`
		Spaces           []struct {
			Generation       *json.Number `json:"generation"`
			TableName        string       `json:"tableName"`
			Status           string       `json:"status"`
			VectorDimension  *json.Number `json:"vectorDimension"`
			ModelFingerprint string       `json:"modelFingerprint"`
			DistanceMetric   string       `json:"distanceMetric"`
			CreatedAt        string       `json:"createdAt"`
		} `json:"spaces"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return VectorSpaceRegistry{}, ErrDomain
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return VectorSpaceRegistry{}, ErrDomain
	}
	version, versionOK := vectorSpaceInteger(wire.Version)
	if !versionOK || version != 1 || len(wire.Spaces) > vectorSpaceMaxSpaces {
		return VectorSpaceRegistry{}, ErrDomain
	}
	registry := VectorSpaceRegistry{Spaces: make([]VectorSpace, 0, len(wire.Spaces))}
	seenGeneration := make(map[int64]bool, len(wire.Spaces))
	seenTable := make(map[string]bool, len(wire.Spaces))
	active := int64(0)
	activeSeen := false
	for _, space := range wire.Spaces {
		if err := check(ctx); err != nil {
			return VectorSpaceRegistry{}, err
		}
		generation, generationOK := vectorSpaceInteger(space.Generation)
		dimension, dimensionOK := vectorSpaceInteger(space.VectorDimension)
		if !generationOK || generation < 0 || seenGeneration[generation] || !dimensionOK ||
			dimension < 1 || dimension > 4096 || !vectorSpaceTableName(space.TableName, generation) ||
			seenTable[space.TableName] || !vectorSpaceStatus(space.Status) ||
			space.ModelFingerprint == "" || len(space.ModelFingerprint) > 1024 ||
			space.CreatedAt == "" || len(space.CreatedAt) > 128 ||
			!vectorSpaceMetric(space.DistanceMetric) {
			return VectorSpaceRegistry{}, ErrDomain
		}
		seenGeneration[generation] = true
		seenTable[space.TableName] = true
		if space.Status == "active" {
			if activeSeen {
				return VectorSpaceRegistry{}, ErrDomain
			}
			activeSeen = true
			active = generation
		}
		registry.Spaces = append(registry.Spaces, VectorSpace{
			Generation: generation, TableName: space.TableName, Status: space.Status,
			VectorDimension: int(dimension), ModelFingerprint: space.ModelFingerprint,
			DistanceMetric: space.DistanceMetric, CreatedAt: space.CreatedAt,
		})
	}
	pointer, pointerOK := vectorSpaceInteger(wire.ActiveGeneration)
	if pointerOK != activeSeen || (activeSeen && pointer != active) {
		return VectorSpaceRegistry{}, ErrDomain
	}
	if activeSeen {
		value := active
		registry.ActiveGeneration = &value
	}
	return registry, nil
}

// ActiveSpace returns the one space the registry points at, if any.
func (r VectorSpaceRegistry) ActiveSpace() (VectorSpace, bool) {
	if r.ActiveGeneration == nil {
		return VectorSpace{}, false
	}
	for _, space := range r.Spaces {
		if space.Generation == *r.ActiveGeneration && space.Status == "active" {
			return space, true
		}
	}
	return VectorSpace{}, false
}

func vectorSpaceInteger(value *json.Number) (int64, bool) {
	if value == nil {
		return 0, false
	}
	text := string(*value)
	if len(text) == 0 || len(text) > 128 {
		return 0, false
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

func vectorSpaceStatus(status string) bool {
	return status == "active" || status == "inactive" || status == "building"
}

func vectorSpaceMetric(metric string) bool {
	return metric == "l2" || metric == "cosine" || metric == "dot"
}

// vectorSpaceTableName is the domain's spaceTable rule for one exact generation.
func vectorSpaceTableName(name string, generation int64) bool {
	return name == "chunks__space_"+strconv.FormatInt(generation, 10)
}
