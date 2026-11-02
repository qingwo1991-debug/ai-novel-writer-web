//go:build !linux

// Package writerlock provides a Linux-only advisory writer exclusion lock.
// Other platforms fail closed; no filesystem objects are created or modified.
package writerlock

import "errors"

var (
	ErrLocked      = errors.New("writerlock_busy")
	ErrUnsafe      = errors.New("writerlock_unsafe")
	ErrIO          = errors.New("writerlock_io")
	ErrUnsupported = errors.New("writerlock_unsupported")
)

// Lock is unconstructible through Acquire on unsupported platforms.
type Lock struct{}

// Acquire fails closed on non-Linux platforms, including for existing paths.
func Acquire(directory string) (*Lock, error) {
	return nil, ErrUnsupported
}

// Close is a no-op for a nil or zero unsupported-platform Lock.
func (lock *Lock) Close() error {
	return nil
}
