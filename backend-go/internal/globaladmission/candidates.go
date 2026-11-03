//go:build linux

package globaladmission

import (
	"context"
	"os"
	"path/filepath"
)

// PreflightSource checks the two selected-object roots without writing them.
// It reports only which root currently owns selected data; model credentials
// never leave this package. A later publisher must recheck this choice under
// the same global exclusion before copying any bytes.
func PreflightSource(ctx context.Context, roots Roots) (string, error) {
	if ctx == nil {
		return "", fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := rootsSafe(roots); err != nil {
		return "", err
	}
	if err := validateGeneration(roots.LegacySource); err != nil {
		return "", err
	}
	if err := validateGeneration(roots.CanonicalTarget); err != nil {
		return "", err
	}
	legacy, err := selectedFiles(roots.LegacySource)
	if err != nil {
		return "", err
	}
	canonical, err := selectedFiles(roots.CanonicalTarget)
	if err != nil {
		return "", err
	}
	if len(legacy) != 0 && len(canonical) != 0 {
		return "", fail(candidateConflict(roots.LegacySource, roots.CanonicalTarget))
	}
	if len(legacy) != 0 {
		return "legacy", nil
	}
	if len(canonical) != 0 {
		return "canonical", nil
	}
	return "empty", nil
}

// The original TypeScript collision gate compares only apiKey on models with
// the same ID. Both model arrays were already validated by PreflightSource.
func candidateConflict(legacy, canonical string) string {
	left, leftErr := modelKeys(filepath.Join(legacy, "models.json"))
	right, rightErr := modelKeys(filepath.Join(canonical, "models.json"))
	if leftErr != nil || rightErr != nil {
		return "GLOBAL_DUAL_ROOT_CONFLICT"
	}
	for id, key := range left {
		if other, exists := right[id]; exists && other != key {
			return "GLOBAL_MODEL_CREDENTIALS_DIFFER"
		}
	}
	return "GLOBAL_DUAL_ROOT_CONFLICT"
}

type candidateKey struct {
	present bool
	value   string
}

func modelKeys(path string) (map[string]candidateKey, error) {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return map[string]candidateKey{}, nil
	} else if err != nil {
		return nil, err
	}
	value, err := parseJSON(path)
	if err != nil {
		return nil, err
	}
	rows, ok := value.([]any)
	if !ok {
		return nil, fail("GLOBAL_CONFIG_SCHEMA_INVALID")
	}
	keys := make(map[string]candidateKey, len(rows))
	for _, raw := range rows {
		model, ok := record(raw)
		if !ok {
			return nil, fail("GLOBAL_MODEL_ID_INVALID")
		}
		id, ok := model["id"].(string)
		if !ok {
			return nil, fail("GLOBAL_MODEL_ID_INVALID")
		}
		value, present := model["apiKey"]
		key := candidateKey{present: present}
		if present {
			key.value, ok = value.(string)
			if !ok {
				return nil, fail("GLOBAL_MODEL_SCHEMA_INVALID")
			}
		}
		keys[id] = key
	}
	return keys, nil
}
