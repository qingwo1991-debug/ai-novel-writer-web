//go:build !linux

// Package writerlock provides a Linux-only advisory writer exclusion lock.
// Other platforms fail closed; no filesystem objects are created or modified.
package writerlock

import "errors"

var (
	ErrLocked      = errors.New("writerlock_busy")
	ErrUnsafe      = errors.New("writerlock_unsafe")
	ErrIO          = errors.New("writerlock_io")
	ErrRetained    = errors.New("writerlock_retained")
	ErrUnsupported = errors.New("writerlock_unsupported")
)

// Lock is unconstructible through Acquire on unsupported platforms.
type Lock struct{}

// Acquire fails closed on non-Linux platforms, including for existing paths.
func Acquire(directory string) (*Lock, error) {
	return nil, ErrUnsupported
}

// AcquireLegacy fails closed on non-Linux platforms without touching the path.
func AcquireLegacy(directory string) (*Lock, error) {
	return nil, ErrUnsupported
}

// Verify fails closed on unsupported platforms, including for nil/zero locks.
func (lock *Lock) Verify() error {
	return ErrUnsupported
}

// VerifyLegacy has the same fail-closed API on unsupported platforms.
func (lock *Lock) VerifyLegacy(directory string) error {
	return ErrUnsupported
}

// Close is a no-op for a nil or zero unsupported-platform Lock.
func (lock *Lock) Close() error {
	return nil
}

func (lock *Lock) Retain() error { return ErrUnsupported }
