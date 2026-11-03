package webdav

import (
	"context"
	"errors"
	"io"
)

// ArchiveStage is a caller-owned, private destination. Open must create an
// unpublished object; Complete may seal only that private staged archive.
// Abort must remove any partial object, including after a failed Complete.
// The caller still owns project admission and archive-format validation before
// restoring author data.
type ArchiveStage interface {
	Open() (io.WriteCloser, error)
	Complete(Generation) error
	Abort() error
}

const (
	StageFailed        Error = "WEBDAV_STAGE_FAILED"
	StageCloseFailed   Error = "WEBDAV_STAGE_CLOSE_FAILED"
	StageCleanupFailed Error = "WEBDAV_STAGE_CLEANUP_FAILED"
)

// StageGeneration fetches a completed cloud generation into an unpublished
// destination. It verifies the full archive hash and byte count, checks both
// Close results, then permits sealing the private stage. It never returns a
// path or claims that the archive is safe to restore into a live author project.
func (s *Service) StageGeneration(ctx context.Context, account Account, book, id string, stage ArchiveStage) (generation Generation, err error) {
	if stage == nil || ctx == nil {
		return Generation{}, InputInvalid
	}
	complete := false
	defer func() {
		if !complete {
			if cleanupErr := stage.Abort(); cleanupErr != nil {
				err = errors.Join(err, StageCleanupFailed)
			}
		}
	}()

	source, generation, err := s.OpenGeneration(ctx, account, book, id)
	if err != nil {
		return Generation{}, err
	}
	writer, openErr := stage.Open()
	if openErr != nil || writer == nil {
		if writer != nil {
			if closeErr := writer.Close(); closeErr != nil {
				_ = source.Close()
				return Generation{}, errors.Join(StageFailed, StageCloseFailed)
			}
		}
		_ = source.Close()
		return Generation{}, StageFailed
	}

	_, copyErr := io.CopyBuffer(writer, source, make([]byte, 128*1024))
	writerCloseErr := writer.Close()
	sourceCloseErr := source.Close()
	if copyErr != nil || sourceCloseErr != nil || writerCloseErr != nil {
		if ctx.Err() != nil {
			err = Cancelled
		} else if copyErr != nil {
			var protocol Error
			if errors.As(copyErr, &protocol) {
				err = protocol
			} else {
				err = StageFailed
			}
		} else if sourceCloseErr != nil {
			err = RemoteInvalid
		} else {
			err = StageCloseFailed
		}
		if writerCloseErr != nil && !errors.Is(err, StageCloseFailed) {
			err = errors.Join(err, StageCloseFailed)
		}
		return Generation{}, err
	}
	if ctx.Err() != nil {
		return Generation{}, Cancelled
	}
	if err := stage.Complete(generation); err != nil {
		return Generation{}, StageFailed
	}
	complete = true
	return generation, nil
}
