//go:build !linux

package projectfs

import "io/fs"

// Admission is explicitly Linux-scoped until equivalent descriptor/link
// semantics have been reviewed and exercised on another platform.
func platformSupported() bool              { return false }
func readFlags() int                       { return 0 }
func singleLink(info fs.FileInfo) bool     { return false }
func sameChangeTime(a, b fs.FileInfo) bool { return false }
