//go:build !linux

package projectfs

// StorageIdentity requires the Linux physical-project admission path.
func (p *Project) StorageIdentity() (device, inode uint64, err error) {
	return 0, 0, ErrUnsupported
}
