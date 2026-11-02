//go:build !linux

package admission

import "os"

// This staged admission lane currently ships only on the Linux Go web backend.
// Refuse unsupported platforms rather than silently weakening link validation.
func singleLink(info os.FileInfo) bool { return false }
