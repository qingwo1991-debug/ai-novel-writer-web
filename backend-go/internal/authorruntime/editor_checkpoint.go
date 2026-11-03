//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

const maxArchCheckpointBytes = 1 << 20

// ReadArchCheckpoint reads one fixed legacy marker under the exact active
// read lease. A second descriptor read detects replacement or in-place edits
// before the value leaves the private, source-verified lifecycle.
func (l *ReadLifecycle) ReadArchCheckpoint(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (json.RawMessage, bool, error) {
	if l == nil || l.active == nil || ctx == nil {
		return nil, false, ErrInvalid
	}
	handle, done, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return nil, false, err
	}
	defer done()
	admitted, ok := handle.(*admittedRead)
	if !ok {
		return nil, false, ErrInvalid
	}
	var result json.RawMessage
	var found bool
	err = admitted.reader.Read(ctx, func(_ *sql.Tx) error {
		first, firstFound, err := readArchCheckpointFile(ctx, admitted.reader.project)
		if err != nil {
			return err
		}
		second, secondFound, err := readArchCheckpointFile(ctx, admitted.reader.project)
		if err != nil {
			return err
		}
		if firstFound != secondFound || !bytes.Equal(first, second) {
			return ErrChanged
		}
		if !firstFound || !json.Valid(first) {
			return nil
		}
		result = json.RawMessage(first)
		found = true
		return nil
	})
	return result, found, err
}

func readArchCheckpointFile(ctx context.Context, project interface {
	OpenFileRead(string) (*os.File, error)
}) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	file, err := project.OpenFileRead("partial_arch.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() > maxArchCheckpointBytes {
		return nil, false, ErrInvalid
	}
	bytes, err := io.ReadAll(io.LimitReader(file, maxArchCheckpointBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(bytes) > maxArchCheckpointBytes {
		return nil, false, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return bytes, true, nil
}
