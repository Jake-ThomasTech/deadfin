// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// fakeProgressiveRunner places progressive jobs on fake remote processes;
// StartHLS refuses.
type fakeProgressiveRunner struct {
	fakeRemoteRunner
	progressive chan *fakeProgressiveProcess
}

type fakeProgressiveProcess struct {
	fakeRemoteProcess
	request RemoteProgressiveJob
}

func (r *fakeProgressiveRunner) StartProgressive(_ context.Context, job RemoteProgressiveJob) (RemoteProcess, error) {
	process := &fakeProgressiveProcess{request: job}
	process.job = RemoteHLSJob{Stderr: job.Stderr}
	process.done, process.quit = make(chan struct{}), make(chan struct{})
	r.progressive <- process
	return process, nil
}

// TestRemoteProgressiveTranscodeServesTheRunnerOutput covers #624: a
// progressive request goes to a progressive runner, and the reader serves
// the output that the runner appends, until the remote process ends.
func TestRemoteProgressiveTranscodeServesTheRunnerOutput(t *testing.T) {
	fixture := newProgressiveFixture(t, "hold")
	runner := &fakeProgressiveRunner{progressive: make(chan *fakeProgressiveProcess, 1)}
	manager, err := NewTranscodeManager(fixture.ffmpeg, fixture.dir, TranscodeManagerOptions{
		Clock: clock.NewReal(), OutputPollInterval: 10 * time.Millisecond, ReaderRetryInterval: 10 * time.Millisecond,
		ReaderStallTimeout: 30 * time.Second, ProgressiveIdleTimeout: 10 * time.Second, KillGracePeriod: 5 * time.Second,
		RemoteRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(context.Background()) }()
	request := fixture.request("remote.mkv")
	opened := make(chan io.ReadCloser, 1)
	go func() {
		reader, err := manager.OpenProgressive(context.Background(), request)
		if err != nil {
			t.Errorf("OpenProgressive: %v", err)
		}
		opened <- reader
	}()
	var remote *fakeProgressiveProcess
	select {
	case remote = <-runner.progressive:
	case <-time.After(fixtureTimeout):
		t.Fatal("no remote progressive placement")
	}
	if remote.request.Output != request.OutputPath || remote.request.SourcePath != fixture.input || remote.request.Source == nil {
		t.Fatalf("remote job = %+v", remote.request)
	}
	if _, err := os.Stat(fixture.starts); err == nil {
		t.Fatal("a local ffmpeg started for a remote progressive transcode")
	}
	if err := os.WriteFile(request.OutputPath, []byte("first "), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := <-opened
	if reader == nil {
		t.FailNow()
	}
	defer func() { _ = reader.Close() }()
	file, err := os.OpenFile(request.OutputPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("second")
	_ = file.Close()
	remote.end(nil)
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != "first second" {
		t.Fatalf("progressive body = %q, %v", body, err)
	}
}
