// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// These tests pin the HLS sidecars to Jellyfin's JobLogger,
// TranscodingThrottler, and TranscodingSegmentCleaner behavior. The sidecars
// stay package-private; TranscodeManager remains their sole public owner.

func TestJobLoggerParsesProgressAndRejectsUnknownOrMalformedUnits(t *testing.T) {
	runtimeTicks := int64(120 * time.Second / 100)
	for _, tc := range []struct {
		name       string
		line       string
		wantReport *jobLoggerProgress
	}{
		{
			name: "separate fps with time size and bitrate",
			line: "frame=372 fps= 23.976 q=28.0 size=123kB time=00:00:15.500 bitrate=312.5kbits/s speed=1.00x",
			wantReport: &jobLoggerProgress{
				Position:        45*time.Second + 500*time.Millisecond,
				FPS:             23.976,
				Percent:         100 * 45.5 / 120,
				BytesTranscoded: 123 * 1024,
				Bitrate:         320_000,
			},
		},
		{
			name: "inline fps is reported without runtime progress",
			line: "frame=1 fps=29.970 q=0.0 size=0kB time=N/A bitrate=N/A",
			wantReport: &jobLoggerProgress{
				FPS: 29.970,
			},
		},
		{
			name: "unknown size and bitrate units are not guessed",
			line: "frame=1 size=2MB time=not-a-time bitrate=1Mbits/s",
		},
		{
			name: "malformed fields do not create progress",
			line: "fps=broken time=00:99:99 size=kb bitrate=kbits/s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reports []jobLoggerProgress
			options := jobLoggerOptions{
				Clock:          clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)),
				Stderr:         strings.NewReader(tc.line + "\n"),
				Log:            &bytes.Buffer{},
				RunTimeTicks:   &runtimeTicks,
				StartTimeTicks: int64(30 * time.Second / 100),
				MaxOutput:      4 << 10,
				Report: func(progress jobLoggerProgress) {
					reports = append(reports, progress)
				},
			}
			if tc.name == "inline fps is reported without runtime progress" {
				options.RunTimeTicks = nil
			}

			logger := newJobLogger(options)
			if err := logger.Run(t.Context()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if tc.wantReport == nil {
				if len(reports) != 0 {
					t.Fatalf("progress reports = %#v, want none", reports)
				}
				return
			}
			if got, want := len(reports), 1; got != want {
				t.Fatalf("progress reports = %#v, want one report", reports)
			}
			assertJobLoggerProgress(t, reports[0], *tc.wantReport)
		})
	}
}

func TestJobLoggerParsesCRLFAndCRProgressRecordsBeforeEOF(t *testing.T) {
	runtimeTicks := int64(120 * time.Second / 100)
	var reports []jobLoggerProgress
	logger := newJobLogger(jobLoggerOptions{
		Stderr: strings.NewReader(
			"frame=1 fps=10.0 time=00:00:10.000\r" +
				"frame=2 fps=20.0 time=00:00:20.000\n" +
				"frame=3 fps=30.0 time=00:00:30.000\r\n"),
		Log:          &bytes.Buffer{},
		RunTimeTicks: &runtimeTicks,
		MaxOutput:    4 << 10,
		Report: func(progress jobLoggerProgress) {
			reports = append(reports, progress)
		},
	})

	if err := logger.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := len(reports), 3; got != want {
		t.Fatalf("CR/LF/CRLF progress reports = %#v, want %d independently parsed records", reports, want)
	}
	for index, want := range []jobLoggerProgress{
		{Position: 10 * time.Second, FPS: 10, Percent: 100 * 10.0 / 120.0},
		{Position: 20 * time.Second, FPS: 20, Percent: 100 * 20.0 / 120.0},
		{Position: 30 * time.Second, FPS: 30, Percent: 100 * 30.0 / 120.0},
	} {
		assertJobLoggerProgress(t, reports[index], want)
	}
}

func TestJobLoggerResynchronizesAfterOversizedRecordAcrossSplitDelimiterAndEOF(t *testing.T) {
	runtimeTicks := int64(120 * time.Second / 100)
	var reports []jobLoggerProgress
	logger := newJobLogger(jobLoggerOptions{
		Stderr: &sidecarChunkReader{chunks: []string{
			strings.Repeat("x", 8<<10),
			strings.Repeat("x", 8<<10),
			strings.Repeat("x", 8<<10),
			strings.Repeat("x", 8<<10),
			strings.Repeat("x", 8<<10),
			"\r",
			"\nframe=4 fps=24.5 size=12kB time=00:00:03.125 bitrate=96.0kbits/s\r",
			"\nframe=5 fps=30.0 size=13kB time=00:00:04.250 bitrate=104.0kbits/s",
		}},
		Log:          &bytes.Buffer{},
		RunTimeTicks: &runtimeTicks,
		MaxOutput:    4 << 10,
		Report: func(progress jobLoggerProgress) {
			reports = append(reports, progress)
		},
	})

	if err := logger.Run(t.Context()); err != nil {
		t.Fatalf("Run after oversized stderr record: %v", err)
	}
	if got, want := len(reports), 2; got != want {
		t.Fatalf("progress reports after oversized record = %#v, want %d valid records", reports, want)
	}
	for index, want := range []jobLoggerProgress{
		{Position: 3*time.Second + 125*time.Millisecond, FPS: 24.5, Percent: 100 * 3.125 / 120, BytesTranscoded: 12 * 1024, Bitrate: 98_304},
		{Position: 4*time.Second + 250*time.Millisecond, FPS: 30, Percent: 100 * 4.25 / 120, BytesTranscoded: 13 * 1024, Bitrate: 106_496},
	} {
		assertJobLoggerProgress(t, reports[index], want)
	}
}

func TestJobLoggerBoundsCopiedStderrAndUsesStableLogFilename(t *testing.T) {
	fixed := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)
	var log bytes.Buffer
	logger := newJobLogger(jobLoggerOptions{
		Clock:         clock.NewFake(fixed),
		Stderr:        strings.NewReader("frame=1 fps=25\nframe=2 fps=25\n"),
		Log:           &log,
		MediaSourceID: "source-7",
		MaxOutput:     15,
		NewID: func() string {
			return "deadbeef-cafe-babe-0123-456789abcdef"
		},
	})

	if got, want := logger.Filename(), "ffmpeg-transcode-20260728T120000-source-7-deadbeef.log"; got != want {
		t.Fatalf("Filename() = %q, want exact prefix/timestamp/media-source/GUID8 shape %q", got, want)
	}
	if err := logger.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := log.String(), "frame=1 fps=25"; got != want {
		t.Fatalf("bounded stderr log = %q, want first %d raw bytes %q", got, 15, want)
	}
}

func TestTranscodingThrottlerPausesAtFloorThresholdAndCloseUnpauses(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	commands := &sidecarCommandWriter{}
	state := throttlerState{
		DownloadPositionTicks:    int64(10 * time.Second / 100),
		TranscodingPositionTicks: int64(80 * time.Second / 100),
	}
	throttler := newTranscodingThrottler(throttlerOptions{
		Clock:         observed,
		Threshold:     5 * time.Second,
		Enabled:       func() bool { return true },
		State:         func() throttlerState { return state },
		Writer:        commands,
		PKeySupported: true,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- throttler.Run(ctx) }()

	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "p"; got != want {
		t.Fatalf("pause command = %q, want %q when 70s HLS gap exceeds 60s floor", got, want)
	}
	if err := throttler.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "pu"; got != want {
		t.Fatalf("commands after Close = %q, want pause followed by unpause %q", got, want)
	}
	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	observed.waitTimer(t, 5*time.Second)
	if got, want := commands.String(), "pu"; got != want {
		t.Fatalf("commands after a tick past Close = %q, want no new pause %q", got, want)
	}
	cancel()
	awaitSidecarResult(t, run, "throttler Run after cancellation")
}

func TestTranscodingThrottlerUsesFallbackCommandsAndCloseDoesNotWaitForTick(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	commands := &sidecarCommandWriter{}
	throttler := newTranscodingThrottler(throttlerOptions{
		Clock:     observed,
		Threshold: 60 * time.Second,
		Enabled:   func() bool { return true },
		State: func() throttlerState {
			return throttlerState{
				DownloadPositionTicks:    int64(time.Second / 100),
				TranscodingPositionTicks: int64(61 * time.Second / 100),
			}
		},
		Writer:        commands,
		PKeySupported: false,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- throttler.Run(ctx) }()

	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "c"; got != want {
		t.Fatalf("fallback pause command = %q, want %q", got, want)
	}

	closed := make(chan error, 1)
	go func() { closed <- throttler.Close(t.Context()) }()
	awaitSidecarResult(t, closed, "throttler Close while Run is waiting for its next tick")
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "c\n"; got != want {
		t.Fatalf("fallback commands after Close = %q, want %q", got, want)
	}
	cancel()
	awaitSidecarResult(t, run, "throttler Run after cancellation")
}

func TestTranscodingSegmentCleanerRespectsFloorAgeGateAndDescriptorScope(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	directory := newSidecarSegmentDirectory([]string{
		"main-1.mp4", "main0.ts", "main1.ts", "main2.ts", "main3.ts", "main99.ts", "mainx.ts", "other0.ts", "../main0.ts",
	})
	cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{
		Clock:         observed,
		Retention:     7 * time.Second,
		SegmentLength: 3 * time.Second,
		State: func() segmentCleanerState {
			return segmentCleanerState{
				Path:                  "/transcodes/main.m3u8",
				HLS:                   true,
				DownloadPositionTicks: int64(26 * time.Second / 100),
			}
		},
		OpenDir: func(path string) (segmentDirectory, error) {
			if path != "/transcodes/main.m3u8" {
				return nil, fmt.Errorf("opened %q, want playlist path", path)
			}
			return directory, nil
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- cleaner.Run(ctx) }()

	observed.waitTimer(t, 20*time.Second)
	base.Advance(20 * time.Second)
	observed.waitTimer(t, 1500*time.Millisecond)
	if got := directory.Removed(); len(got) != 0 {
		t.Fatalf("segments removed before 1500ms age gate: %q", got)
	}
	base.Advance(1500 * time.Millisecond)
	directory.waitRemoves(t, 3)
	if got, want := directory.Removed(), []string{"main0.ts", "main1.ts", "main2.ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("descriptor-relative removals = %#v, want only expired segments %#v", got, want)
	}

	if err := cleaner.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cancel()
	awaitSidecarResult(t, run, "segment cleaner Run after cancellation")
	if !directory.Closed() {
		t.Fatal("opened segment directory was not closed")
	}
}

func TestTranscodingSegmentCleanerPassesRootPlaylistPathToOpenDirOnce(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	directory := newSidecarSegmentDirectory([]string{"main0.ts"})
	var opened []string
	cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{
		Clock:         observed,
		Retention:     20 * time.Second,
		SegmentLength: 3 * time.Second,
		State: func() segmentCleanerState {
			return segmentCleanerState{Path: "main.m3u8", HLS: true, DownloadPositionTicks: int64(24 * time.Second / 100)}
		},
		OpenDir: func(path string) (segmentDirectory, error) {
			opened = append(opened, path)
			return directory, nil
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- cleaner.Run(ctx) }()

	observed.waitTimer(t, 20*time.Second)
	base.Advance(20 * time.Second)
	observed.waitTimer(t, 1500*time.Millisecond)
	base.Advance(1500 * time.Millisecond)
	directory.waitRemoves(t, 1)
	if got, want := opened, []string{"main.m3u8"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OpenDir paths = %#v, want one root playlist path %#v", got, want)
	}
	if got, want := directory.Removed(), []string{"main0.ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("root playlist removals = %#v, want %#v", got, want)
	}
	cancel()
	awaitSidecarResult(t, run, "segment cleaner Run after cancellation")
}

func TestTranscodingSegmentCleanerCloseCancelsPendingAgeGate(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	directory := newSidecarSegmentDirectory([]string{"main0.ts"})
	cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{
		Clock:         observed,
		Retention:     20 * time.Second,
		SegmentLength: 3 * time.Second,
		State: func() segmentCleanerState {
			return segmentCleanerState{
				Path:                  "/transcodes/main.m3u8",
				HLS:                   true,
				DownloadPositionTicks: int64(24 * time.Second / 100),
			}
		},
		OpenDir: func(string) (segmentDirectory, error) { return directory, nil },
	})
	run := make(chan error, 1)
	go func() { run <- cleaner.Run(t.Context()) }()

	observed.waitTimer(t, 20*time.Second)
	base.Advance(20 * time.Second)
	observed.waitTimer(t, 1500*time.Millisecond)
	if err := cleaner.Close(t.Context()); err != nil {
		t.Fatalf("Close during age gate: %v", err)
	}
	awaitSidecarResult(t, run, "segment cleaner Run after Close")
	base.Advance(1500 * time.Millisecond)
	if got := directory.Removed(); len(got) != 0 {
		t.Fatalf("Close allowed pending age-gated deletion: %q", got)
	}
}

func TestJobLoggerRuntimeProgressAcceptsNoFPSAndPartialNAFields(t *testing.T) {
	runtimeTicks := int64(120 * time.Second / 100)
	for _, tc := range []struct {
		name string
		line string
		want jobLoggerProgress
	}{
		{
			name: "runtime progress without fps",
			line: "frame=12 q=27.0 size=12kB time=00:00:10.000 bitrate=100.5kbits/s",
			want: jobLoggerProgress{Position: 10 * time.Second, Percent: 100 * 10.0 / 120.0, BytesTranscoded: 12 * 1024, Bitrate: 102_912},
		},
		{
			name: "partial N A leaves only unavailable size unset",
			line: "frame=12 fps=24.0 q=27.0 size=N/A time=00:00:10.000 bitrate=100.5kbits/s",
			want: jobLoggerProgress{Position: 10 * time.Second, FPS: 24, Percent: 100 * 10.0 / 120.0, Bitrate: 102_912},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reports []jobLoggerProgress
			logger := newJobLogger(jobLoggerOptions{
				Stderr:       strings.NewReader(tc.line + "\n"),
				Log:          &bytes.Buffer{},
				RunTimeTicks: &runtimeTicks,
				MaxOutput:    4 << 10,
				Report: func(progress jobLoggerProgress) {
					reports = append(reports, progress)
				},
			})
			if err := logger.Run(t.Context()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got, want := len(reports), 1; got != want {
				t.Fatalf("progress reports = %#v, want one independently parsed report", reports)
			}
			assertJobLoggerProgress(t, reports[0], tc.want)
		})
	}
}

func TestTranscodingThrottlerResumesWhenThrottlingBecomesDisabled(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	commands := &sidecarCommandWriter{}
	enabled := true
	throttler := newTranscodingThrottler(throttlerOptions{
		Clock:     observed,
		Threshold: time.Minute,
		Enabled:   func() bool { return enabled },
		State: func() throttlerState {
			return throttlerState{DownloadPositionTicks: int64(time.Second / 100), TranscodingPositionTicks: int64(61 * time.Second / 100)}
		},
		Writer:        commands,
		PKeySupported: true,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- throttler.Run(ctx) }()

	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "p"; got != want {
		t.Fatalf("initial command = %q, want pause %q", got, want)
	}
	observed.waitTimer(t, 5*time.Second)
	enabled = false
	base.Advance(5 * time.Second)
	observed.waitTimer(t, 5*time.Second)
	if got, want := commands.String(), "pu"; got != want {
		t.Fatalf("commands after throttling disabled = %q, want prompt resume %q", got, want)
	}
	cancel()
	awaitSidecarResult(t, run, "throttler Run after cancellation")
}

func TestTranscodingThrottlerUsesProgressiveBytesAndOutputSizeFallback(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	commands := &sidecarCommandWriter{}
	bytesTranscoded := int64(1_000)
	state := throttlerState{BytesDownloaded: 700, TranscodingPositionTicks: int64(120 * time.Second / 100), BytesTranscoded: &bytesTranscoded}
	fileSizeCalls := 0
	throttler := newTranscodingThrottler(throttlerOptions{
		Clock:     observed,
		Threshold: time.Minute,
		Enabled:   func() bool { return true },
		State:     func() throttlerState { return state },
		Writer:    commands,
		FileSize: func() (int64, error) {
			fileSizeCalls++
			return 1_000, nil
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- throttler.Run(ctx) }()

	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	observed.waitTimer(t, 5*time.Second)
	if got := commands.String(); got != "" {
		t.Fatalf("progressive command with 300-byte lead below 500-byte target = %q, want no pause", got)
	}
	if fileSizeCalls != 0 {
		t.Fatalf("FileSize calls with reported BytesTranscoded = %d, want zero", fileSizeCalls)
	}

	state.BytesDownloaded = 100
	state.BytesTranscoded = nil
	base.Advance(5 * time.Second)
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "c"; got != want {
		t.Fatalf("fallback command with 900-byte lead above 500-byte target = %q, want %q", got, want)
	}
	if fileSizeCalls != 1 {
		t.Fatalf("FileSize fallback calls = %d, want one", fileSizeCalls)
	}
	cancel()
	awaitSidecarResult(t, run, "throttler Run after cancellation")
}

func TestTranscodingThrottlerPausesAtCacheBudget(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	commands := &sidecarCommandWriter{}
	segmentTicks := int64(10 * time.Second / (100 * time.Nanosecond))
	state := throttlerState{
		DownloadPositionTicks:    segmentTicks,
		TranscodingPositionTicks: segmentTicks + 2*segmentTicks,
	}
	cacheBytes := int64(2 << 30)
	throttler := newTranscodingThrottler(throttlerOptions{
		Clock:              observed,
		Threshold:          5 * time.Second,
		Enabled:            func() bool { return true },
		State:              func() throttlerState { return state },
		Writer:             commands,
		PKeySupported:      true,
		CacheBudget:        1 << 30,
		SegmentLengthTicks: segmentTicks,
		CacheBytes:         func() (int64, error) { return cacheBytes, nil },
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- throttler.Run(ctx) }()

	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "p"; got != want {
		t.Fatalf("pause command at cache budget = %q, want %q", got, want)
	}

	cacheBytes = 1 << 29
	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "pu"; got != want {
		t.Fatalf("commands after bytes drop below budget = %q, want %q", got, want)
	}
	cancel()
	awaitSidecarResult(t, run, "throttler Run after cancellation")
}

func TestTranscodingThrottlerHoldsCapacityPauseBelowTwoSegmentLead(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	commands := &sidecarCommandWriter{}
	segmentTicks := int64(10 * time.Second / (100 * time.Nanosecond))
	state := throttlerState{
		DownloadPositionTicks:    segmentTicks,
		TranscodingPositionTicks: segmentTicks + segmentTicks,
	}
	throttler := newTranscodingThrottler(throttlerOptions{
		Clock:              observed,
		Threshold:          5 * time.Second,
		Enabled:            func() bool { return true },
		State:              func() throttlerState { return state },
		Writer:             commands,
		PKeySupported:      true,
		CacheBudget:        1 << 30,
		SegmentLengthTicks: segmentTicks,
		CacheBytes:         func() (int64, error) { return 2 << 30, nil },
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- throttler.Run(ctx) }()

	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	if got := commands.String(); got != "" {
		t.Fatalf("command with one-segment lead below two-segment guard = %q, want no pause", got)
	}
	cancel()
	awaitSidecarResult(t, run, "throttler Run after cancellation")
}

func TestTranscodingThrottlerPausesBelowFreeSpaceFloor(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	commands := &sidecarCommandWriter{}
	segmentTicks := int64(10 * time.Second / (100 * time.Nanosecond))
	state := throttlerState{
		DownloadPositionTicks:    segmentTicks,
		TranscodingPositionTicks: segmentTicks + 2*segmentTicks,
	}
	freeBytes := int64(1 << 29)
	throttler := newTranscodingThrottler(throttlerOptions{
		Clock:              observed,
		Threshold:          5 * time.Second,
		Enabled:            func() bool { return true },
		State:              func() throttlerState { return state },
		Writer:             commands,
		PKeySupported:      true,
		FreeFloor:          1 << 30,
		SegmentLengthTicks: segmentTicks,
		FreeBytes:          func() (int64, error) { return freeBytes, nil },
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- throttler.Run(ctx) }()

	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "p"; got != want {
		t.Fatalf("pause command below free-space floor = %q, want %q", got, want)
	}

	freeBytes = 2 << 30
	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	commands.waitWrites(t, 1)
	if got, want := commands.String(), "pu"; got != want {
		t.Fatalf("commands after free space recovery = %q, want %q", got, want)
	}
	cancel()
	awaitSidecarResult(t, run, "throttler Run after cancellation")
}

func TestTranscodingThrottlerCapacityClosureErrorDoesNotPause(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	commands := &sidecarCommandWriter{}
	segmentTicks := int64(10 * time.Second / (100 * time.Nanosecond))
	state := throttlerState{
		DownloadPositionTicks:    segmentTicks,
		TranscodingPositionTicks: segmentTicks + 2*segmentTicks,
	}
	throttler := newTranscodingThrottler(throttlerOptions{
		Clock:              observed,
		Threshold:          5 * time.Second,
		Enabled:            func() bool { return true },
		State:              func() throttlerState { return state },
		Writer:             commands,
		PKeySupported:      true,
		CacheBudget:        1 << 30,
		FreeFloor:          1 << 30,
		SegmentLengthTicks: segmentTicks,
		CacheBytes:         func() (int64, error) { return 0, errors.New("stat failed") },
		FreeBytes:          func() (int64, error) { return 0, errors.New("statfs failed") },
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- throttler.Run(ctx) }()

	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	observed.waitTimer(t, 5*time.Second)
	base.Advance(5 * time.Second)
	if got := commands.String(); got != "" {
		t.Fatalf("command with closure errors = %q, want no pause", got)
	}
	cancel()
	awaitSidecarResult(t, run, "throttler Run after cancellation")
}

func TestTranscodingSegmentCleanerLeavesFloorBoundaryUntouchedAndContinuesAfterRemoveFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		position time.Duration
		want     []string
	}{
		{name: "below retention floor", position: 19*time.Second + 999*time.Millisecond},
		{name: "at retention floor", position: 20 * time.Second},
		{name: "one segment beyond floor", position: 23 * time.Second, want: []string{"main0.ts", "main1.ts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
			observed := newSidecarObservedClock(base)
			directory := newSidecarSegmentDirectory([]string{"main0.ts", "main1.ts", "main2.ts"})
			cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{
				Clock:         observed,
				Retention:     7 * time.Second,
				SegmentLength: 3 * time.Second,
				State: func() segmentCleanerState {
					return segmentCleanerState{Path: "/transcodes/main.m3u8", HLS: true, DownloadPositionTicks: int64(tc.position / 100)}
				},
				OpenDir: func(string) (segmentDirectory, error) { return directory, nil },
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			run := make(chan error, 1)
			go func() { run <- cleaner.Run(ctx) }()

			observed.waitTimer(t, 20*time.Second)
			base.Advance(20 * time.Second)
			if len(tc.want) == 0 {
				observed.waitTimer(t, 20*time.Second)
			} else {
				observed.waitTimer(t, 1500*time.Millisecond)
				base.Advance(1500 * time.Millisecond)
				directory.waitRemoves(t, len(tc.want))
			}
			if got := directory.Removed(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("removed segments = %#v, want %#v", got, tc.want)
			}
			cancel()
			awaitSidecarResult(t, run, "segment cleaner Run after cancellation")
		})
	}

	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	directory := newSidecarSegmentDirectory([]string{"main0.ts", "main1.ts"})
	directory.failOnce = map[string]error{"main0.ts": fmt.Errorf("busy")}
	directory.failureAttempt = make(chan struct{}, 1)
	directory.releaseFailure = make(chan struct{})
	cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{
		Clock:         observed,
		Retention:     20 * time.Second,
		SegmentLength: 3 * time.Second,
		State: func() segmentCleanerState {
			return segmentCleanerState{Path: "/transcodes/main.m3u8", HLS: true, DownloadPositionTicks: int64(24 * time.Second / 100)}
		},
		OpenDir: func(string) (segmentDirectory, error) { return directory, nil },
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- cleaner.Run(ctx) }()

	observed.waitTimer(t, 20*time.Second)
	base.Advance(20 * time.Second)
	observed.waitTimer(t, 1500*time.Millisecond)
	base.Advance(1500 * time.Millisecond)
	directory.waitFailure(t)
	close(directory.releaseFailure)
	directory.waitRemoveOrRun(t, run)
	if got, want := directory.Removed(), []string{"main1.ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("successful removals after main0 failure = %#v, want later candidate %#v", got, want)
	}
	observed.waitTimer(t, 20*time.Second)
	base.Advance(20 * time.Second)
	observed.waitTimer(t, 1500*time.Millisecond)
	base.Advance(1500 * time.Millisecond)
	directory.waitRemoves(t, 1)
	if got, want := directory.Removed(), []string{"main1.ts", "main0.ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("periodic retry removals = %#v, want retry after later candidate %#v", got, want)
	}
	cancel()
	awaitSidecarResult(t, run, "segment cleaner Run after cancellation")
}

func TestTranscodingSegmentCleanerPagesBoundedBatchesAndRemovesExactEligibleSet(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	entries := make([]string, 0, segmentDirectoryBatchSize*2+5)
	wantRemoved := make([]string, 0, segmentDirectoryBatchSize*2+1)
	for index := range segmentDirectoryBatchSize*2 + 5 {
		name := fmt.Sprintf("main%d.ts", index)
		entries = append(entries, name)
		if index <= segmentDirectoryBatchSize*2 {
			wantRemoved = append(wantRemoved, name)
		}
	}
	entries = append(entries, "mainx.ts", "other0.ts")
	directory := newSidecarSegmentDirectory(entries)
	cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{
		Clock:         observed,
		Retention:     20 * time.Second,
		SegmentLength: time.Second,
		OpenDir:       func(string) (segmentDirectory, error) { return directory, nil },
	})
	state := segmentCleanerState{
		Path:                  "/transcodes/main.m3u8",
		HLS:                   true,
		DownloadPositionTicks: int64(276 * time.Second / 100),
	}
	cleaned := make(chan error, 1)
	go func() { cleaned <- cleaner.clean(t.Context(), state) }()
	observed.waitTimer(t, 1500*time.Millisecond)
	base.Advance(1500 * time.Millisecond)
	awaitSidecarResult(t, cleaned, "paged segment cleaner")

	if got := directory.Removed(); !reflect.DeepEqual(got, wantRemoved) {
		t.Fatalf("paged cleaner removals = %#v, want exact eligible set %#v", got, wantRemoved)
	}
	if !directory.Closed() {
		t.Fatal("paged cleaner did not close its directory")
	}
	for call, size := range directory.BatchSizes() {
		if size != segmentDirectoryBatchSize {
			t.Fatalf("Entries batch %d size = %d, want bounded size %d", call, size, segmentDirectoryBatchSize)
		}
	}
	if got := len(directory.BatchSizes()); got < 3 {
		t.Fatalf("Entries calls = %d, want multiple batches for more than %d entries", got, segmentDirectoryBatchSize)
	}
}

func TestTranscodingSegmentCleanerPagingPreservesReadErrorsAndCancellation(t *testing.T) {
	t.Run("read error closes directory", func(t *testing.T) {
		sentinel := errors.New("paged directory read failed")
		entries := make([]string, segmentDirectoryBatchSize*2)
		for index := range entries {
			entries[index] = fmt.Sprintf("other%d.ts", index)
		}
		directory := newSidecarSegmentDirectory(entries)
		directory.entryErrAt = map[int]error{2: sentinel}
		cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{SegmentLength: time.Second, OpenDir: func(string) (segmentDirectory, error) { return directory, nil }})
		err := cleaner.clean(t.Context(), segmentCleanerState{Path: "/transcodes/main.m3u8", HLS: true, DownloadPositionTicks: int64(30 * time.Second / 100)})
		if !errors.Is(err, sentinel) {
			t.Fatalf("paged clean error = %v, want injected read error", err)
		}
		if !directory.Closed() {
			t.Fatal("directory was not closed after a paged read error")
		}
	})

	t.Run("cancellation closes directory before any batch", func(t *testing.T) {
		directory := newSidecarSegmentDirectory([]string{"main0.ts"})
		cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{SegmentLength: time.Second, OpenDir: func(string) (segmentDirectory, error) { return directory, nil }})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := cleaner.clean(ctx, segmentCleanerState{Path: "/transcodes/main.m3u8", HLS: true, DownloadPositionTicks: int64(30 * time.Second / 100)}); err != nil {
			t.Fatalf("canceled paged clean = %v, want nil", err)
		}
		if got := directory.BatchSizes(); len(got) != 0 {
			t.Fatalf("Entries calls after cancellation = %#v, want none", got)
		}
		if !directory.Closed() {
			t.Fatal("directory was not closed after cancellation")
		}
	})
}

func assertJobLoggerProgress(t *testing.T, got, want jobLoggerProgress) {
	t.Helper()
	if got.Position != want.Position || got.BytesTranscoded != want.BytesTranscoded || got.Bitrate != want.Bitrate || math.Abs(float64(got.FPS-want.FPS)) > 0.0001 || math.Abs(got.Percent-want.Percent) > 0.0001 {
		t.Fatalf("progress = %#v, want %#v", got, want)
	}
}

type sidecarObservedClock struct {
	clock.Clock
	timers chan time.Duration
}

func newSidecarObservedClock(base clock.Clock) *sidecarObservedClock {
	return &sidecarObservedClock{Clock: base, timers: make(chan time.Duration, 16)}
}

func (clock *sidecarObservedClock) NewTimer(delay time.Duration) clock.Timer {
	timer := clock.Clock.NewTimer(delay)
	clock.timers <- delay
	return timer
}

func (clock *sidecarObservedClock) waitTimer(t *testing.T, want time.Duration) {
	t.Helper()
	select {
	case got := <-clock.timers:
		if got != want {
			t.Fatalf("timer delay = %s, want %s", got, want)
		}
	case <-t.Context().Done():
		t.Fatalf("waiting for %s timer: %v", want, t.Context().Err())
	}
}

type sidecarChunkReader struct {
	chunks []string
	index  int
}

func (reader *sidecarChunkReader) Read(destination []byte) (int, error) {
	if reader.index >= len(reader.chunks) {
		return 0, io.EOF
	}
	chunk := reader.chunks[reader.index]
	reader.index++
	if len(chunk) > len(destination) {
		panic("sidecarChunkReader chunk exceeds reader buffer")
	}
	return copy(destination, chunk), nil
}

type sidecarCommandWriter struct {
	mu     sync.Mutex
	data   []byte
	writes chan struct{}
}

func (writer *sidecarCommandWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	writer.data = append(writer.data, data...)
	if writer.writes == nil {
		writer.writes = make(chan struct{}, 8)
	}
	writer.writes <- struct{}{}
	writer.mu.Unlock()
	return len(data), nil
}

func (writer *sidecarCommandWriter) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return string(writer.data)
}

func (writer *sidecarCommandWriter) waitWrites(t *testing.T, count int) {
	t.Helper()
	writer.mu.Lock()
	if writer.writes == nil {
		writer.writes = make(chan struct{}, 8)
	}
	writes := writer.writes
	writer.mu.Unlock()
	for range count {
		select {
		case <-writes:
		case <-t.Context().Done():
			t.Fatalf("waiting for command write %d: %v", count, t.Context().Err())
		}
	}
}

type sidecarSegmentDirectory struct {
	mu             sync.Mutex
	entries        []string
	scanEntries    []string
	entryCursor    int
	entryEOF       bool
	batchSizes     []int
	entryCalls     int
	entryErrAt     map[int]error
	removed        []string
	failOnce       map[string]error
	failureAttempt chan struct{}
	releaseFailure chan struct{}
	closed         bool
	notify         chan struct{}
}

func newSidecarSegmentDirectory(entries []string) *sidecarSegmentDirectory {
	return &sidecarSegmentDirectory{entries: append([]string(nil), entries...), notify: make(chan struct{}, len(entries)+1)}
}

func (directory *sidecarSegmentDirectory) Entries(batchSize int) ([]string, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	directory.entryCalls++
	directory.batchSizes = append(directory.batchSizes, batchSize)
	if err := directory.entryErrAt[directory.entryCalls]; err != nil {
		return nil, err
	}
	if directory.entryEOF {
		directory.scanEntries = nil
		directory.entryCursor = 0
		directory.entryEOF = false
	}
	if directory.scanEntries == nil {
		directory.scanEntries = append([]string(nil), directory.entries...)
	}
	if directory.entryCursor == len(directory.scanEntries) {
		directory.entryEOF = true
		return nil, io.EOF
	}
	end := directory.entryCursor + batchSize
	if end > len(directory.scanEntries) {
		end = len(directory.scanEntries)
	}
	batch := append([]string(nil), directory.scanEntries[directory.entryCursor:end]...)
	directory.entryCursor = end
	if directory.entryCursor == len(directory.scanEntries) {
		directory.entryEOF = true
		return batch, io.EOF
	}
	return batch, nil
}

func (directory *sidecarSegmentDirectory) Remove(name string) error {
	if strings.ContainsRune(name, '/') {
		return fmt.Errorf("Remove received non-descriptor-relative name %q", name)
	}
	directory.mu.Lock()
	if err, failed := directory.failOnce[name]; failed {
		delete(directory.failOnce, name)
		attempt, release := directory.failureAttempt, directory.releaseFailure
		directory.mu.Unlock()
		if attempt != nil {
			attempt <- struct{}{}
		}
		if release != nil {
			<-release
		}
		return err
	}
	for index, entry := range directory.entries {
		if entry == name {
			directory.entries = append(directory.entries[:index], directory.entries[index+1:]...)
			break
		}
	}
	directory.removed = append(directory.removed, name)
	directory.notify <- struct{}{}
	directory.mu.Unlock()
	return nil
}

func (directory *sidecarSegmentDirectory) Close() error {
	directory.mu.Lock()
	directory.closed = true
	directory.mu.Unlock()
	return nil
}

func (directory *sidecarSegmentDirectory) Removed() []string {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	return append([]string(nil), directory.removed...)
}

func (directory *sidecarSegmentDirectory) BatchSizes() []int {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	return append([]int(nil), directory.batchSizes...)
}

func (directory *sidecarSegmentDirectory) Closed() bool {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	return directory.closed
}

func (directory *sidecarSegmentDirectory) waitRemoves(t *testing.T, count int) {
	t.Helper()
	for range count {
		select {
		case <-directory.notify:
		case <-t.Context().Done():
			t.Fatalf("waiting for segment removal %d: %v", count, t.Context().Err())
		}
	}
}

func (directory *sidecarSegmentDirectory) waitFailure(t *testing.T) {
	t.Helper()
	select {
	case <-directory.failureAttempt:
	case <-t.Context().Done():
		t.Fatalf("waiting for injected segment deletion failure: %v", t.Context().Err())
	}
}

func (directory *sidecarSegmentDirectory) waitRemoveOrRun(t *testing.T, run <-chan error) {
	t.Helper()
	select {
	case <-directory.notify:
	case err := <-run:
		t.Fatalf("segment cleaner exited after one deletion failure instead of continuing: %v", err)
	case <-t.Context().Done():
		t.Fatalf("waiting for later segment deletion after a failure: %v", t.Context().Err())
	}
}

func awaitSidecarResult(t *testing.T, result <-chan error, operation string) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	case <-t.Context().Done():
		t.Fatalf("%s did not complete: %v", operation, t.Context().Err())
	}
}

// TestTranscodingThrottlerMeasuresUndownloadedLeadFromFamilyStart checks that
// a family restarted by a seek is not paused before its first segment is
// served (#858). The absolute position after a seek is far past the threshold,
// but the lead over the family start is not.
func TestTranscodingThrottlerMeasuresUndownloadedLeadFromFamilyStart(t *testing.T) {
	ticks := func(seconds int) int64 { return int64(time.Duration(seconds) * time.Second / 100) }
	throttler := newTranscodingThrottler(throttlerOptions{
		Threshold:          time.Minute,
		SegmentLengthTicks: ticks(6),
	})
	for _, test := range []struct {
		name  string
		state throttlerState
		want  bool
	}{
		{"seek restart before the first segment", throttlerState{StartTimeTicks: ticks(1158), TranscodingPositionTicks: ticks(1171)}, false},
		{"seek restart 60s ahead without a download", throttlerState{StartTimeTicks: ticks(1158), TranscodingPositionTicks: ticks(1218)}, true},
		{"start at zero 60s ahead without a download", throttlerState{TranscodingPositionTicks: ticks(60)}, true},
		{"start at zero below the threshold", throttlerState{TranscodingPositionTicks: ticks(59)}, false},
		{"seek restart after a download", throttlerState{StartTimeTicks: ticks(1158), DownloadPositionTicks: ticks(1164), TranscodingPositionTicks: ticks(1224)}, true},
	} {
		if got := throttler.shouldPause(test.state); got != test.want {
			t.Errorf("%s: shouldPause = %v, want %v", test.name, got, test.want)
		}
	}
	short := newTranscodingThrottler(throttlerOptions{Threshold: time.Minute, SegmentLengthTicks: ticks(40)})
	if short.shouldPause(throttlerState{TranscodingPositionTicks: ticks(70)}) {
		t.Fatal("shouldPause = true with less than two segments written ahead of the family start")
	}
}
