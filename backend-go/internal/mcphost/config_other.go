//go:build !linux

package mcphost

// This package's admitted platform is Linux: the server image is a single static
// Linux executable. Another host keeps the conservative refusal until an
// equivalent no-follow, owner-aware file admission exists here, so a build for
// that host cannot read an author configuration through a weaker check than the
// one the reference lane performed.
func readConfigFile(string) ([]byte, error) {
	return nil, ErrConfigUnreadable
}
