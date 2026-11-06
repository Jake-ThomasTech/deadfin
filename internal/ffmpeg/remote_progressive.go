// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
)

// startRemoteProgressive offers a progressive transcode to the remote
// runner (#624). It returns true when the runner owns the start (success or
// a recorded start failure) and false when the caller must start ffmpeg
// locally. The runner writes the output file; the progressive reader tails
// it as for a local ffmpeg.
func (manager *TranscodeManager) startRemoteProgressive(job *transcodeJob) bool {
	runner, ok := manager.options.RemoteRunner.(RemoteProgressiveRunner)
	if !ok || job.hls != nil || job.source == nil {
		return false
	}
	stderr, err := manager.openProgressiveStderr(job.output)
	if err != nil {
		return false
	}
	request := RemoteProgressiveJob{
		Args:   append([]string(nil), job.args...),
		Output: job.output,
		// The manager closes the log file when the job ends.
		Stderr: nopWriteCloser{&limitedWriter{writer: stderr, remaining: maxTranscodeStderr}},
	}
	manager.mu.Lock()
	closed := manager.closed
	request.LocalJobs = manager.localJobsLocked()
	request.SourcePath = job.source.Path()
	request.Source = job.source.File()
	request.SourceSize = job.source.Fingerprint().Size
	manager.mu.Unlock()
	if closed {
		_ = stderr.Close()
		manager.failStartAfterPrepare(job, ErrManagerClosed)
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteStartTimeout)
	remote, err := runner.StartProgressive(ctx, request)
	cancel()
	if err != nil {
		_ = stderr.Close()
		if errors.Is(err, ErrRemoteRefused) {
			manager.failStartAfterPrepare(job, err)
			return true
		}
		if !errors.Is(err, ErrNoRemoteRunner) && manager.options.Logger != nil {
			manager.options.Logger.Warn("remote progressive transcode start failed; running locally", "output", job.output, "error", err.Error())
		}
		return false
	}

	manager.mu.Lock()
	job.proc = remoteProcess{remote: remote}
	job.stdin = remoteStdin{writer: remote.Stdin()}
	job.stderr = stderr
	job.logPath = stderr.Name()
	job.state = transcodeRunning
	manager.addWorkerLocked()
	close(job.startDone)
	if job.requests == 0 && !manager.closed {
		manager.armIdleLocked(job)
	}
	manager.mu.Unlock()
	go manager.wait(job)
	return true
}

type nopWriteCloser struct{ *limitedWriter }

func (nopWriteCloser) Close() error { return nil }
