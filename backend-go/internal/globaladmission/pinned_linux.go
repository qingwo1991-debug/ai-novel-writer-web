//go:build linux

package globaladmission

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// PinnedReady keeps read-only handles to the completed migration authority.
// The caller owns the global single-writer lock for this object's lifetime.
// Identity checks detect path replacement; VerifyCompleted still validates the
// current receipt and generation semantics at each use boundary.
type PinnedReady struct {
	mu            sync.Mutex
	roots         Roots
	ready         Ready
	chain         []pinnedPathPart
	rootFD        int
	controlFD     int
	receiptFD     int
	generationsFD int
	dataFD        int
	receiptBytes  []byte
	closed        bool
}

type pinnedPathPart struct {
	name string
	dev  uint64
	ino  uint64
}

func pinnedStat(fd int, kind uint32) (syscall.Stat_t, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != kind {
		return stat, fail("GLOBAL_ROOT_UNSAFE")
	}
	if kind == syscall.S_IFREG && (stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0) {
		return stat, fail("GLOBAL_JSON_INVALID")
	}
	return stat, nil
}

func pinnedSame(fd, expectedFD int, kind uint32) error {
	a, err := pinnedStat(fd, kind)
	if err != nil {
		return err
	}
	b, err := pinnedStat(expectedFD, kind)
	if err != nil || a.Dev != b.Dev || a.Ino != b.Ino {
		return fail("GLOBAL_ROOT_UNSAFE")
	}
	return nil
}

func pinnedOpenDir(parentFD int, name string) (int, error) {
	fd, err := syscall.Openat(parentFD, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fail("GLOBAL_ROOT_UNSAFE")
	}
	return fd, nil
}

func pinnedOpenReceipt(parentFD int) (int, error) {
	fd, err := syscall.Openat(parentFD, "receipt.json", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fail("GLOBAL_JSON_INVALID")
	}
	if _, err := pinnedStat(fd, syscall.S_IFREG); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

func pinnedOpenCanonical(path string) (int, []pinnedPathPart, error) {
	if !cleanAbsolute(path) || path == "/" {
		return -1, nil, fail("GLOBAL_ROOT_UNSAFE")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, nil, fail("GLOBAL_ROOT_UNSAFE")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	chain := make([]pinnedPathPart, 0, len(parts))
	for _, name := range parts {
		if name == "" || name == "." || name == ".." {
			syscall.Close(fd)
			return -1, nil, fail("GLOBAL_ROOT_UNSAFE")
		}
		next, err := pinnedOpenDir(fd, name)
		syscall.Close(fd)
		if err != nil {
			return -1, nil, err
		}
		fd = next
		stat, err := pinnedStat(fd, syscall.S_IFDIR)
		if err != nil {
			syscall.Close(fd)
			return -1, nil, err
		}
		chain = append(chain, pinnedPathPart{name: name, dev: stat.Dev, ino: stat.Ino})
	}
	return fd, chain, nil
}

func pinnedReadReceipt(fd int) ([]byte, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG ||
		stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 || stat.Size > maxJSONBytes {
		return nil, fail("GLOBAL_JSON_INVALID")
	}
	data := make([]byte, 0, min(int(stat.Size), maxJSONBytes))
	var chunk [32 << 10]byte
	for int64(len(data)) <= maxJSONBytes {
		n, err := syscall.Pread(fd, chunk[:], int64(len(data)))
		if err != nil {
			return nil, fail("GLOBAL_JSON_INVALID")
		}
		if n == 0 {
			if int64(len(data)) != stat.Size {
				return nil, fail("GLOBAL_JSON_INVALID")
			}
			return data, nil
		}
		data = append(data, chunk[:n]...)
	}
	return nil, fail("GLOBAL_JSON_INVALID")
}

// PinCompleted accepts only a trusted caller that already holds global
// exclusion. It performs no migration and writes no author data.
func PinCompleted(ctx context.Context, roots Roots, exclusive bool) (*PinnedReady, error) {
	if ctx == nil || !exclusive {
		return nil, fail("GLOBAL_EXCLUSIVE_ACCESS_REQUIRED")
	}
	ready, err := VerifyCompleted(ctx, roots)
	if err != nil {
		return nil, err
	}
	rootFD, chain, err := pinnedOpenCanonical(roots.CanonicalTarget)
	if err != nil {
		return nil, err
	}
	p := &PinnedReady{roots: roots, ready: ready, chain: chain, rootFD: rootFD,
		controlFD: -1, receiptFD: -1, generationsFD: -1, dataFD: -1}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	if p.controlFD, err = pinnedOpenDir(p.rootFD, ".migration"); err != nil {
		return nil, err
	}
	if p.receiptFD, err = pinnedOpenReceipt(p.controlFD); err != nil {
		return nil, err
	}
	if p.generationsFD, err = pinnedOpenDir(p.rootFD, "generations"); err != nil {
		return nil, err
	}
	if p.dataFD, err = pinnedOpenDir(p.generationsFD, ready.GlobalGeneration); err != nil {
		return nil, err
	}
	if p.receiptBytes, err = pinnedReadReceipt(p.receiptFD); err != nil {
		return nil, err
	}
	if _, err = p.Verify(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *PinnedReady) verifyChain() error {
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fail("GLOBAL_ROOT_UNSAFE")
	}
	defer func() { _ = syscall.Close(fd) }()
	for _, part := range p.chain {
		next, err := pinnedOpenDir(fd, part.name)
		if err != nil {
			return err
		}
		syscall.Close(fd)
		fd = next
		stat, err := pinnedStat(fd, syscall.S_IFDIR)
		if err != nil || stat.Dev != part.dev || stat.Ino != part.ino {
			return fail("GLOBAL_ROOT_UNSAFE")
		}
	}
	return pinnedSame(fd, p.rootFD, syscall.S_IFDIR)
}

func (p *PinnedReady) verifyOpenChildren() error {
	control, err := pinnedOpenDir(p.rootFD, ".migration")
	if err != nil {
		return err
	}
	defer syscall.Close(control)
	if err := pinnedSame(control, p.controlFD, syscall.S_IFDIR); err != nil {
		return err
	}
	receipt, err := pinnedOpenReceipt(control)
	if err != nil {
		return err
	}
	defer syscall.Close(receipt)
	if err := pinnedSame(receipt, p.receiptFD, syscall.S_IFREG); err != nil {
		return err
	}
	current, err := pinnedReadReceipt(receipt)
	if err != nil || !bytes.Equal(current, p.receiptBytes) {
		return fail("GLOBAL_RECEIPT_INVALID")
	}
	generations, err := pinnedOpenDir(p.rootFD, "generations")
	if err != nil {
		return err
	}
	defer syscall.Close(generations)
	if err := pinnedSame(generations, p.generationsFD, syscall.S_IFDIR); err != nil {
		return err
	}
	data, err := pinnedOpenDir(generations, p.ready.GlobalGeneration)
	if err != nil {
		return err
	}
	defer syscall.Close(data)
	return pinnedSame(data, p.dataFD, syscall.S_IFDIR)
}

// Verify rechecks the pinned identity and the original migration semantics.
// It cannot replace caller-held exclusion against non-cooperative same-UID
// processes between a check and later use of DataRoot.
func (p *PinnedReady) Verify(ctx context.Context) (Ready, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || ctx == nil {
		return Ready{}, fail("GLOBAL_ROOT_UNSAFE")
	}
	if err := ctx.Err(); err != nil {
		return Ready{}, err
	}
	if err := p.verifyChain(); err != nil {
		return Ready{}, err
	}
	if err := p.verifyOpenChildren(); err != nil {
		return Ready{}, err
	}
	ready, err := VerifyCompleted(ctx, p.roots)
	if err != nil {
		return Ready{}, err
	}
	if ready != p.ready {
		return Ready{}, fail("GLOBAL_RECEIPT_INVALID")
	}
	if err := p.verifyChain(); err != nil {
		return Ready{}, err
	}
	if err := p.verifyOpenChildren(); err != nil {
		return Ready{}, err
	}
	return p.ready, nil
}

// Close releases the pinned handles. It is idempotent.
func (p *PinnedReady) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var result error
	for _, fd := range []int{p.dataFD, p.generationsFD, p.receiptFD, p.controlFD, p.rootFD} {
		if fd >= 0 {
			result = errors.Join(result, syscall.Close(fd))
		}
	}
	return result
}

// DataRoot returns the original verified path after another identity check.
func (p *PinnedReady) DataRoot(ctx context.Context) (string, error) {
	ready, err := p.Verify(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Clean(ready.DataRoot), nil
}
