//go:build !linux

package mcp

import (
	"errors"
)

// Other hosts remain conservative until an equivalent no-follow, owner-aware
// file admission is implemented. The Go production target is Linux.
func readConfigFile(string) ([]byte, error) {
	return nil, errors.New("MCP_CONFIG_INVALID")
}
