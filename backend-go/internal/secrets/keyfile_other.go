//go:build !linux

package secrets

// Private key admission currently requires Linux descriptor metadata, nofollow,
// link count, and directory locking semantics. Other platforms fail closed;
// explicitly supplied keys and the wire envelope remain portable.
func LoadExistingKey(path string) ([]byte, error) {
	return nil, ErrUnsupported
}

func LoadOrCreateKey(path string) ([]byte, error) {
	return nil, ErrUnsupported
}
