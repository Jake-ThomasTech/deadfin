// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func (manager *TranscodeManager) openHLSLog(job *transcodeJob) (*os.File, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("generate HLS log id: %w", err)
	}
	logger := newJobLogger(jobLoggerOptions{
		Clock:         manager.options.Clock,
		MediaSourceID: job.hls.mediaSourceID,
		NewID:         func() string { return hex.EncodeToString(id) },
	})
	name := filepath.Base(logger.Filename())
	parent, err := unix.Open(manager.logDirectory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open HLS log directory: %w", err)
	}
	defer func() { _ = unix.Close(parent) }()
	fd, err := unix.Openat(parent, name, unix.O_CREAT|unix.O_WRONLY|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open HLS log %q: %w", name, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(manager.logDirectory, name)), nil
}

func (manager *TranscodeManager) startHLSSidecarsLocked(job *transcodeJob, stderr io.Reader, log *os.File) {
	ctx, cancel := context.WithCancel(context.Background())
	job.sidecarCancel = cancel
	runtimeTicks := job.hls.runTimeTicks
	pipeReader, _ := stderr.(*io.PipeReader)
	logger := newJobLogger(jobLoggerOptions{
		Clock:          manager.options.Clock,
		Stderr:         stderr,
		Log:            log,
		RunTimeTicks:   &runtimeTicks,
		StartTimeTicks: job.hls.startTimeTicks,
		MediaSourceID:  job.hls.mediaSourceID,
		MaxOutput:      manager.options.MaxLogBytes,
		NewID:          func() string { return "managed" },
		Report: func(progress jobLoggerProgress) {
			manager.mu.Lock()
			job.positionTicks = int64(progress.Position / 100)
			job.bytesTranscoded = progress.BytesTranscoded
			job.hasBytesTranscoded = progress.BytesTranscoded != 0
			manager.mu.Unlock()
		},
	})
	throttler := newTranscodingThrottler(throttlerOptions{
		Clock:         manager.options.Clock,
		Threshold:     manager.options.ThrottleThreshold,
		Enabled:       func() bool { return true },
		Writer:        job.stdin,
		PKeySupported: manager.options.PKeySupported,
		State: func() throttlerState {
			manager.mu.Lock()
			defer manager.mu.Unlock()
			state := throttlerState{
				Exited:                   job.state == transcodeExited,
				DownloadPositionTicks:    job.downloadPositionTicks,
				TranscodingPositionTicks: job.positionTicks,
				BytesDownloaded:          job.bytesDownloaded,
			}
			if job.hasBytesTranscoded {
				bytes := job.bytesTranscoded
				state.BytesTranscoded = &bytes
			}
			return state
		},
		CacheBudget:        manager.options.CacheBytesPerSession,
		FreeFloor:          manager.options.CacheFreeBytesFloor,
		SegmentLengthTicks: job.hls.guardSegmentTicks(),
		CacheBytes:         manager.familyCacheBytes(job.hls.playlist, job.hls.matches),
		FreeBytes:          manager.transcodesFreeBytes,
	})
	job.throttler = throttler
	cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{
		Clock:         manager.options.Clock,
		Retention:     manager.options.SegmentRetention,
		SegmentLength: time.Duration(job.hls.segmentLength) * time.Second,
		SegmentStarts: job.hls.segmentStarts,
		State: func() segmentCleanerState {
			manager.mu.Lock()
			defer manager.mu.Unlock()
			return segmentCleanerState{Exited: job.state == transcodeExited, Path: job.hls.playlist, DownloadPositionTicks: job.downloadPositionTicks, HLS: true}
		},
		OpenDir: manager.openHLSSegmentDirectory,
	})
	loggerDone := make(chan struct{})
	job.sidecarLoggerDone = loggerDone
	manager.addWorkerLocked()
	go func() {
		defer manager.doneWorker()
		defer close(loggerDone)
		defer func() { _ = log.Close() }()
		if pipeReader != nil {
			defer func() { _ = pipeReader.Close() }()
		}
		_ = logger.Run(context.Background())
	}()
	manager.addWorkerLocked()
	go func() {
		defer manager.doneWorker()
		_ = throttler.Run(ctx)
	}()
	manager.addWorkerLocked()
	go func() {
		defer manager.doneWorker()
		_ = cleaner.Run(ctx)
	}()
}

func (manager *TranscodeManager) startHLSSidecarPumpLocked(job *transcodeJob, source *os.File, stderr *os.File, destination *io.PipeWriter) {
	pumpDone := make(chan struct{})
	job.sidecarPumpDone = pumpDone
	manager.addWorkerLocked()
	go func() {
		defer manager.doneWorker()
		defer close(pumpDone)
		defer func() { _ = destination.Close() }()
		logSink := &limitedWriter{writer: stderr, remaining: maxTranscodeStderr}
		buffer := make([]byte, 32<<10)
		for {
			count, err := source.Read(buffer)
			if count > 0 {
				_, _ = logSink.Write(buffer[:count])
				_, _ = destination.Write(buffer[:count])
			}
			if err != nil {
				return
			}
		}
	}()
}

// drainHLSSidecars preserves the final diagnostic bytes. The pump closes its
// writer after it reaches EOF; that EOF lets the logger finish before either
// read descriptor is released.
func (manager *TranscodeManager) drainHLSSidecars(job *transcodeJob) {
	manager.mu.Lock()
	pumpDone, loggerDone := job.sidecarPumpDone, job.sidecarLoggerDone
	manager.mu.Unlock()
	if pumpDone != nil {
		<-pumpDone
	}
	if loggerDone != nil {
		<-loggerDone
	}
	manager.stopHLSSidecars(job)
}

func (manager *TranscodeManager) stopHLSSidecars(job *transcodeJob) {
	manager.mu.Lock()
	cancel, stderr, source := job.sidecarCancel, job.sidecarStderr, job.sidecarSource
	job.sidecarCancel = nil
	job.sidecarStderr = nil
	job.sidecarSource = nil
	job.sidecarPumpDone = nil
	job.sidecarLoggerDone = nil
	manager.mu.Unlock()
	if stderr != nil {
		_ = stderr.Close()
	}
	if source != nil {
		_ = source.Close()
	}
	if cancel != nil {
		cancel()
	}
}

func (manager *TranscodeManager) stopHLSSidecarsKeepSource(job *transcodeJob) {
	manager.mu.Lock()
	cancel, stderr := job.sidecarCancel, job.sidecarStderr
	job.sidecarCancel = nil
	job.sidecarStderr = nil
	job.sidecarPumpDone = nil
	job.sidecarLoggerDone = nil
	manager.mu.Unlock()
	if stderr != nil {
		_ = stderr.Close()
	}
	if cancel != nil {
		cancel()
	}
}

type hlsSegmentDirectory struct{ file *os.File }

func (manager *TranscodeManager) openHLSSegmentDirectory(playlist string) (segmentDirectory, error) {
	parent, _, err := manager.openOutputParent(playlist)
	if err != nil {
		return nil, err
	}
	return &hlsSegmentDirectory{file: os.NewFile(uintptr(parent), filepath.Dir(playlist))}, nil
}

func (directory *hlsSegmentDirectory) Entries(batchSize int) ([]string, error) {
	entries, err := directory.file.ReadDir(batchSize)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, err
}

func (directory *hlsSegmentDirectory) Remove(name string) error {
	return unix.Unlinkat(int(directory.file.Fd()), name, 0)
}

func (directory *hlsSegmentDirectory) Close() error { return directory.file.Close() }

// familyCacheBytes sums the apparent size (st_size) of the playlist file and
// every segment file in the family directory. The directory is opened
// descriptor-relative to stay inside the transcodes root. Each file is stated
// with AT_SYMLINK_NOFOLLOW so a symlink adds zero bytes instead of following
// the link target.
func (manager *TranscodeManager) familyCacheBytes(playlist string, matches func(string) (int, bool)) func() (int64, error) {
	return func() (int64, error) {
		directory, err := manager.openHLSSegmentDirectory(playlist)
		if err != nil {
			return 0, err
		}
		defer directory.Close()
		segmentDir := directory.(*hlsSegmentDirectory)
		playlistName := filepath.Base(playlist)
		var total int64
		for {
			names, err := segmentDir.file.ReadDir(128)
			for _, entry := range names {
				name := entry.Name()
				if name != playlistName {
					if _, ok := matches(name); !ok {
						continue
					}
				}
				var stat unix.Stat_t
				if err := unix.Fstatat(int(segmentDir.file.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					return 0, err
				}
				total += stat.Size
			}
			if err == io.EOF {
				return total, nil
			}
			if err != nil {
				return 0, err
			}
		}
	}
}

// transcodesFreeBytes reports the available space on the transcodes filesystem
// (Bavail * Bsize). Every session sees the same filesystem, so one Statfs call
// gives aggregate protection across concurrent sessions with no cross-session
// accounting.
func (manager *TranscodeManager) transcodesFreeBytes() (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(manager.directory, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
