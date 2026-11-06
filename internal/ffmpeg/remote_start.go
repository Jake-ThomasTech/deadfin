// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/deadfin-org/deadfin/internal/encoding"
)

// remoteStartTimeout bounds one remote placement, queue wait included. The
// runner applies the operator's queue timeout; this bound only stops a
// runner that never answers.
const remoteStartTimeout = 2 * time.Minute

// startRemote offers an HLS family to the remote runner. It returns true when
// the remote runner owns the start (success or a recorded start failure) and
// false when the caller must start ffmpeg locally.
func (manager *TranscodeManager) startRemote(job *transcodeJob) bool {
	if job.hls == nil {
		return false
	}
	// A remote-only family cannot run here (#854): every path that would
	// start it locally fails the start instead.
	local := func() bool {
		if job.hls.remoteOnly {
			manager.failStartAfterPrepare(job, ErrRemoteOnly)
			return true
		}
		return false
	}
	runner := manager.options.RemoteRunner
	// An embedded text subtitle makes libass read the media file itself;
	// that burn-in stays local, as does a family marked local.
	if runner == nil || job.hls.subtitleSource != "" || job.hls.local {
		return local()
	}
	assets, rewrite, ok := manager.remoteAssets(job)
	if !ok {
		return local()
	}
	sidecars, ok := manager.openRemoteSidecars(job)
	if !ok {
		return local()
	}
	discard := sidecars.discard

	request := RemoteHLSJob{
		Args:           rewrite(manager.canonicalRemoteArgs(job)),
		Assets:         assets,
		Directory:      filepath.Dir(job.hls.playlist),
		Playlist:       job.hls.playlist,
		SegmentPattern: job.hls.pattern,
		PlaySessionID:  job.hls.playSessionID,
		DeviceID:       job.hls.deviceID,
		SegmentLength:  job.hls.segmentLength,
		Facts:          job.hls.facts,
		RemoteOnly:     job.hls.remoteOnly,
		Build:          manager.remoteArgsBuilder(job, rewrite),
		Stderr:         sidecars.destination,
	}
	manager.mu.Lock()
	closed := manager.closed
	request.LocalJobs = manager.localJobsLocked()
	if job.source != nil {
		request.SourcePath = job.source.Path()
		request.Source = job.source.File()
		request.SourceSize = job.source.Fingerprint().Size
	}
	manager.mu.Unlock()
	if closed {
		discard()
		manager.failStartAfterPrepare(job, ErrManagerClosed)
		return true
	}
	parent := job.startCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, remoteStartTimeout)
	remote, err := runner.StartHLS(ctx, request)
	cancel()
	if err != nil {
		discard()
		if errors.Is(err, ErrRemoteRefused) || parent.Err() != nil {
			// Placement refused the job, or every waiting request gave up:
			// nothing runs.
			manager.failStartAfterPrepare(job, err)
			return true
		}
		if job.hls.remoteOnly {
			manager.failStartAfterPrepare(job, fmt.Errorf("%w: %w", ErrRemoteOnly, err))
			return true
		}
		if !errors.Is(err, ErrNoRemoteRunner) && manager.options.Logger != nil {
			manager.options.Logger.Warn("remote transcode start failed; running locally", "output", job.output, "error", err.Error())
		}
		return false
	}

	manager.mu.Lock()
	manager.attachRemoteLocked(job, remote, sidecars)
	manager.addWorkerLocked()
	close(job.startDone)
	if job.requests == 0 && !manager.closed {
		manager.armIdleLocked(job)
	}
	manager.mu.Unlock()
	go manager.wait(job)
	return true
}

// remoteSidecars are the stderr, log and throttle resources of one remote
// family. The runner writes ffmpeg's stderr to destination.
type remoteSidecars struct {
	stderr      *os.File
	log         *os.File
	reader      *io.PipeReader
	writer      *io.PipeWriter
	source      *os.File
	destination *os.File
}

func (manager *TranscodeManager) openRemoteSidecars(job *transcodeJob) (remoteSidecars, bool) {
	stderr, err := manager.openProgressiveStderr(job.output)
	if err != nil {
		return remoteSidecars{}, false
	}
	var sidecarLog *os.File
	if manager.hlsLogOpen != nil {
		sidecarLog, err = manager.hlsLogOpen(job)
	} else {
		sidecarLog, err = manager.openHLSLog(job)
	}
	if err != nil {
		_ = stderr.Close()
		return remoteSidecars{}, false
	}
	sidecarReader, sidecarWriter := io.Pipe()
	sidecarSource, sidecarDestination, err := os.Pipe()
	if err != nil {
		closeHLSStartResources(sidecarReader, sidecarWriter, sidecarLog)
		_ = stderr.Close()
		return remoteSidecars{}, false
	}
	return remoteSidecars{stderr: stderr, log: sidecarLog, reader: sidecarReader, writer: sidecarWriter, source: sidecarSource, destination: sidecarDestination}, true
}

func (sidecars remoteSidecars) discard() {
	closeHLSStartResources(sidecars.reader, sidecars.writer, sidecars.log)
	_ = sidecars.source.Close()
	_ = sidecars.destination.Close()
	_ = sidecars.stderr.Close()
}

// attachRemoteLocked makes job a running remote family and starts its
// sidecars (logger, throttler, segment cleaner).
func (manager *TranscodeManager) attachRemoteLocked(job *transcodeJob, remote RemoteProcess, sidecars remoteSidecars) {
	job.proc = remoteProcess{remote: remote}
	job.stdin = remoteStdin{writer: remote.Stdin()}
	job.stderr = sidecars.stderr
	job.logPath = sidecars.log.Name()
	job.state = transcodeRunning
	if job.sourceAux != nil {
		_ = job.sourceAux.Close()
		job.sourceAux = nil
	}
	job.sidecarStderr = sidecars.writer
	job.sidecarSource = sidecars.source
	manager.startHLSSidecarsLocked(job, sidecars.reader, sidecars.log)
	manager.startHLSSidecarPumpLocked(job, sidecars.source, sidecars.stderr, sidecars.writer)
}

// localJobsLocked counts the ffmpeg families that run on the server.
func (manager *TranscodeManager) localJobsLocked() int {
	count := 0
	for _, job := range manager.jobs {
		if _, local := job.proc.(localProcess); local && job.state == transcodeRunning {
			count++
		}
	}
	return count
}

// canonicalRemoteArgs rewrites the output operands to the canonical paths
// that RemoteHLSJob names, so a runner can match them exactly. Validation
// already proved that they normalize to the family (validateHLSOutputOperands).
func (manager *TranscodeManager) canonicalRemoteArgs(job *transcodeJob) []string {
	return manager.canonicalOutputs(job, job.args)
}

func (manager *TranscodeManager) canonicalOutputs(job *transcodeJob, argv []string) []string {
	args := append([]string(nil), argv...)
	for index, argument := range args {
		if !filepath.IsAbs(argument) {
			continue
		}
		canonical, err := manager.normalizeOutputPath(argument)
		if err == nil && (canonical == job.hls.playlist || canonical == job.hls.pattern) {
			args[index] = canonical
		}
	}
	return args
}

// remoteArgsBuilder wraps the job's argv builder so a rebuilt argv names
// the canonical output operands, as canonicalRemoteArgs does for Args.
func (manager *TranscodeManager) remoteArgsBuilder(job *transcodeJob, rewrite func([]string) []string) ArgsBuilder {
	build := job.hls.buildArgs
	if build == nil {
		return nil
	}
	return func(caps encoding.Capabilities, host encoding.HostHardware) ([]string, error) {
		args, err := build(caps, host)
		if err != nil {
			return nil, err
		}
		return rewrite(manager.canonicalOutputs(job, args)), nil
	}
}

// errRemoteSegmentLost wraps ErrRunnerLost for a waiter whose segment the
// lost host never completed.
func errRemoteSegmentLost(playlist string, index int) error {
	return fmt.Errorf("HLS playlist %q segment %d: %w", playlist, index, ErrRunnerLost)
}
