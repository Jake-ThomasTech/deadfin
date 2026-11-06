// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/deadfin-org/deadfin/internal/encoding"
)

// LiveRemuxer reads a live HLS source with one ffmpeg process and streams
// it as MPEG-TS, for the Live TV stream broker (docs/spikes/livetv.md
// §0.1: one ingest per channel). The source URL can carry provider
// credentials, so ffmpeg's diagnostics are discarded and errors carry only
// the exit status.
type LiveRemuxer struct {
	// Path is the ffmpeg executable.
	Path string
}

// Remux starts ffmpeg for input. The process lives until ctx ends, the
// returned reader is closed, or the source ends; the reader's owner must
// close it.
func (r LiveRemuxer) Remux(ctx context.Context, input encoding.LiveInput) (io.ReadCloser, error) {
	if strings.TrimSpace(r.Path) == "" {
		return nil, errors.New("live remux: ffmpeg path is empty")
	}
	args, err := encoding.BuildLiveRemuxArgs(input)
	if err != nil {
		return nil, fmt.Errorf("live remux: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(ctx, r.Path, args...)
	// ffmpeg's HLS demuxer can start helper children; kill the group.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("live remux: %w", err)
	}
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("live remux: start ffmpeg: %w", err)
	}
	return &liveRemuxStream{command: command, stdout: stdout, cancel: cancel}, nil
}

// liveRemuxStream is ffmpeg's stdout. Read reports a failed exit at the end
// of the output; Close stops the process and waits for it.
type liveRemuxStream struct {
	command *exec.Cmd
	stdout  io.ReadCloser
	cancel  context.CancelFunc

	once    sync.Once
	waitErr error
}

func (s *liveRemuxStream) Read(p []byte) (int, error) {
	n, err := s.stdout.Read(p)
	if errors.Is(err, io.EOF) {
		if waitErr := s.wait(); waitErr != nil {
			return n, fmt.Errorf("live remux: ffmpeg: %w", waitErr)
		}
	}
	return n, err
}

func (s *liveRemuxStream) Close() error {
	s.cancel()
	_ = s.wait()
	return nil
}

func (s *liveRemuxStream) wait() error {
	s.once.Do(func() {
		s.waitErr = s.command.Wait()
		s.cancel()
	})
	return s.waitErr
}
