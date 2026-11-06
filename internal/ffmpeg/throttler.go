// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"io"
	"math/bits"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

type throttlerState struct {
	Exited                   bool
	DownloadPositionTicks    int64
	TranscodingPositionTicks int64
	// StartTimeTicks is the source position where the ffmpeg family started.
	// A seek restarts the family there.
	StartTimeTicks  int64
	BytesDownloaded int64
	BytesTranscoded *int64
}

type throttlerOptions struct {
	Clock         clock.Clock
	Threshold     time.Duration
	Enabled       func() bool
	State         func() throttlerState
	Writer        io.Writer
	PKeySupported bool
	FileSize      func() (int64, error)

	CacheBudget        int64
	FreeFloor          int64
	SegmentLengthTicks int64
	CacheBytes         func() (int64, error)
	FreeBytes          func() (int64, error)
}

type transcodingThrottler struct {
	options throttlerOptions
	mu      sync.Mutex
	paused  bool
	// closed stops all later pauses, so a stop command reaches a running
	// ffmpeg.
	closed bool
}

func newTranscodingThrottler(options throttlerOptions) *transcodingThrottler {
	if options.Clock == nil {
		options.Clock = clock.NewReal()
	}
	return &transcodingThrottler{options: options}
}

func (throttler *transcodingThrottler) Run(ctx context.Context) error {
	for {
		timer := throttler.options.Clock.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return nil
		case <-timer.C():
		}
		if throttler.options.Enabled == nil || !throttler.options.Enabled() {
			throttler.setPaused(false)
			continue
		}
		if throttler.options.State == nil {
			continue
		}
		state := throttler.options.State()
		if state.Exited {
			return nil
		}
		throttler.setPaused(throttler.shouldPause(state))
	}
}

func (throttler *transcodingThrottler) shouldPause(state throttlerState) bool {
	return throttler.timeShouldPause(state) || throttler.capacityShouldPause(state)
}

func (throttler *transcodingThrottler) timeShouldPause(state throttlerState) bool {
	threshold := throttler.options.Threshold
	if threshold < time.Minute {
		threshold = time.Minute
	}
	thresholdTicks := int64(threshold / (100 * time.Nanosecond))
	if thresholdTicks <= 0 {
		return false
	}

	if state.DownloadPositionTicks > 0 && state.TranscodingPositionTicks > 0 {
		if state.TranscodingPositionTicks < state.DownloadPositionTicks {
			return false
		}
		return state.TranscodingPositionTicks-state.DownloadPositionTicks >= thresholdTicks
	}
	if state.DownloadPositionTicks == 0 && state.BytesDownloaded == 0 && state.TranscodingPositionTicks > 0 {
		// No segment is downloaded yet, so measure the lead from the family
		// start. After a seek the absolute position is far past the threshold
		// before the requested segment is complete; a pause there wedges the
		// waiting client (#858).
		lead := state.TranscodingPositionTicks - state.StartTimeTicks
		if segmentLength := throttler.options.SegmentLengthTicks; segmentLength > 0 && lead < 2*segmentLength {
			return false
		}
		return lead >= thresholdTicks
	}

	if state.BytesDownloaded <= 0 || state.TranscodingPositionTicks <= 0 {
		return false
	}
	bytesTranscoded := int64(0)
	if state.BytesTranscoded != nil {
		bytesTranscoded = *state.BytesTranscoded
	} else if throttler.options.FileSize != nil {
		var err error
		bytesTranscoded, err = throttler.options.FileSize()
		if err != nil {
			return false
		}
	}
	if bytesTranscoded <= 0 || bytesTranscoded < state.BytesDownloaded {
		return false
	}

	byteLead := bytesTranscoded - state.BytesDownloaded
	return productsAtLeast(byteLead, state.TranscodingPositionTicks, bytesTranscoded, thresholdTicks)
}

// capacityShouldPause returns true when the HLS transcode-cache family exceeds
// its byte budget or the transcodes filesystem drops below its free-space floor.
// The two-segment lead guard makes the pause wedge-free: the successor of every
// serveable segment exists, so the client can always progress and the pause
// releases when the cleaner reclaims space. A closure error yields no pause,
// matching the FileSize error handling in timeShouldPause.
func (throttler *transcodingThrottler) capacityShouldPause(state throttlerState) bool {
	if throttler.options.CacheBudget <= 0 && throttler.options.FreeFloor <= 0 {
		return false
	}
	segmentLength := throttler.options.SegmentLengthTicks
	if segmentLength <= 0 || state.TranscodingPositionTicks <= 0 {
		return false
	}
	lead := state.TranscodingPositionTicks - state.DownloadPositionTicks
	if lead < 2*segmentLength {
		return false
	}
	if throttler.options.CacheBudget > 0 && throttler.options.CacheBytes != nil {
		bytes, err := throttler.options.CacheBytes()
		if err == nil && bytes >= throttler.options.CacheBudget {
			return true
		}
	}
	if throttler.options.FreeFloor > 0 && throttler.options.FreeBytes != nil {
		bytes, err := throttler.options.FreeBytes()
		if err == nil && bytes < throttler.options.FreeFloor {
			return true
		}
	}
	return false
}

func productsAtLeast(leftA, leftB, rightA, rightB int64) bool {
	leftHigh, leftLow := bits.Mul64(uint64(leftA), uint64(leftB))
	rightHigh, rightLow := bits.Mul64(uint64(rightA), uint64(rightB))
	return leftHigh > rightHigh || leftHigh == rightHigh && leftLow >= rightLow
}

// Close unpauses ffmpeg and stops all later pauses, as pinned
// TranscodingThrottler.Stop does before TranscodingJob.Stop writes "q". In the
// "c" pause ffmpeg reads stdin as a filter command, so a "q" sent to a paused
// job does not stop it.
func (throttler *transcodingThrottler) Close(context.Context) error {
	throttler.mu.Lock()
	defer throttler.mu.Unlock()
	throttler.setPausedLocked(false)
	throttler.closed = true
	return nil
}

func (throttler *transcodingThrottler) setPaused(paused bool) {
	throttler.mu.Lock()
	defer throttler.mu.Unlock()
	throttler.setPausedLocked(paused)
}

func (throttler *transcodingThrottler) setPausedLocked(paused bool) {
	if throttler.paused == paused || paused && throttler.closed {
		return
	}
	if throttler.options.Writer != nil {
		command := ""
		if throttler.options.PKeySupported {
			if paused {
				command = "p"
			} else {
				command = "u"
			}
		} else if paused {
			command = "c"
		} else {
			command = "\n"
		}
		if _, err := io.WriteString(throttler.options.Writer, command); err != nil {
			return
		}
	}
	throttler.paused = paused
}
