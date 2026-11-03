package webdav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type archiveStageFixture struct {
	data         bytes.Buffer
	opened       bool
	completed    bool
	aborted      bool
	failWrite    bool
	failClose    bool
	failComplete bool
	failAbort    bool
}

type archiveStageWriter struct{ stage *archiveStageFixture }

func (w archiveStageWriter) Write(p []byte) (int, error) {
	if w.stage.failWrite {
		return 0, errors.New("/private/archive path")
	}
	return w.stage.data.Write(p)
}
func (w archiveStageWriter) Close() error {
	if w.stage.failClose {
		return errors.New("/private/close path")
	}
	return nil
}
func (s *archiveStageFixture) Open() (io.WriteCloser, error) {
	s.opened = true
	return archiveStageWriter{s}, nil
}
func (s *archiveStageFixture) Complete(Generation) error {
	if s.failComplete {
		return errors.New("/private/complete path")
	}
	s.completed = true
	return nil
}
func (s *archiveStageFixture) Abort() error {
	s.aborted = true
	s.data.Reset()
	if s.failAbort {
		return errors.New("/private/abort path")
	}
	return nil
}

func TestStageGenerationPublishesOnlyVerifiedBytes(t *testing.T) {
	f := newFixture(t)
	s := serviceFor(t, f, testA)
	data := []byte("PUBLIC 合成 cloud archive")
	if _, err := appendFixture(t, s, f, context.Background(), data); err != nil {
		t.Fatal(err)
	}
	stage := &archiveStageFixture{}
	generation, err := s.StageGeneration(context.Background(), f.account(), testBook, testA, stage)
	if err != nil || !stage.opened || !stage.completed || stage.aborted || !bytes.Equal(stage.data.Bytes(), data) || generation.ArchiveSHA256 != digestBytes(data) {
		t.Fatalf("verified stage: generation=%+v err=%v stage=%+v", generation, err, stage)
	}

	// Completion and manifest remain valid while a remote archive object changes.
	locations, err := GenerationLocations(f.account().Endpoint, testBook, testA)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.files[locations.Archive.EscapedPath()][0] ^= 1
	f.mu.Unlock()
	stage = &archiveStageFixture{}
	generation, err = s.StageGeneration(context.Background(), f.account(), testBook, testA, stage)
	if err != RemoteInvalid || generation.GenerationID != "" || stage.completed || !stage.aborted || stage.data.Len() != 0 {
		t.Fatalf("corrupt archive was published: generation=%+v err=%v stage=%+v", generation, err, stage)
	}
}

func TestStageGenerationFailuresAbortWithoutLeakingPrivatePaths(t *testing.T) {
	f := newFixture(t)
	s := serviceFor(t, f, testA)
	if _, err := appendFixture(t, s, f, context.Background(), []byte("PUBLIC")); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"write", "close", "complete", "abort"} {
		t.Run(mode, func(t *testing.T) {
			stage := &archiveStageFixture{}
			switch mode {
			case "write":
				stage.failWrite = true
			case "close":
				stage.failClose = true
			case "complete":
				stage.failComplete = true
			case "abort":
				stage.failWrite, stage.failAbort = true, true
			}
			_, err := s.StageGeneration(context.Background(), f.account(), testBook, testA, stage)
			if err == nil || stage.completed || !stage.aborted || stage.data.Len() != 0 || strings.Contains(err.Error(), "/private/") {
				t.Fatalf("unsafe stage failure: err=%v stage=%+v", err, stage)
			}
			if mode == "abort" && !errors.Is(err, StageCleanupFailed) {
				t.Fatalf("cleanup failure lost: %v", err)
			}
		})
	}
}
