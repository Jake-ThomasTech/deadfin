// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// annihilationGOPStarts models the production source of deadfin#799: 5.6 s
// GOPs, one keyframe-cut file per GOP, played with 3 s segments.
func annihilationGOPStarts(count int) []int64 {
	starts := make([]int64, count)
	for index := range starts {
		starts[index] = int64(index) * 56_000_000
	}
	return starts
}

// A keyframe-cut job numbers its files by source GOP. The cleaner must expire
// a file by its GOP end, not by index times the requested segment length.
// With the nominal 3 s length, a player that has fetched file 23 (end
// 134.4 s) lost files 24-38 before it asked for them, and the next request
// restarted ffmpeg every cleaner pass (deadfin#799).
func TestTranscodingSegmentCleanerExpiresKeyframeCutFilesByGOPEnd(t *testing.T) {
	var entries []string
	for index := 19; index <= 40; index++ {
		entries = append(entries, fmt.Sprintf("main%d.mp4", index))
	}
	for _, tc := range []struct {
		name   string
		starts []int64
		want   []string
	}{
		{name: "GOP boundaries", starts: annihilationGOPStarts(100), want: []string{"main19.mp4"}},
		{name: "nominal length", want: func() []string {
			var want []string
			for index := 19; index <= 38; index++ {
				want = append(want, fmt.Sprintf("main%d.mp4", index))
			}
			return want
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := clock.NewFake(time.Date(2026, time.November, 1, 2, 11, 0, 0, time.UTC))
			observed := newSidecarObservedClock(base)
			directory := newSidecarSegmentDirectory(entries)
			cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{
				Clock:         observed,
				Retention:     20 * time.Second,
				SegmentLength: 3 * time.Second,
				SegmentStarts: tc.starts,
				State: func() segmentCleanerState {
					return segmentCleanerState{Path: "/transcodes/main.m3u8", HLS: true, DownloadPositionTicks: 24 * 56_000_000}
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
			directory.waitRemoves(t, len(tc.want))
			observed.waitTimer(t, 20*time.Second)
			if got := directory.Removed(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("removed %q, want %q", got, tc.want)
			}
			cancel()
			awaitSidecarResult(t, run, "segment cleaner Run after cancellation")
		})
	}
}

// Before the first GOP ends outside the retention window, nothing expires.
func TestTranscodingSegmentCleanerKeepsKeyframeCutFilesInsideRetention(t *testing.T) {
	base := clock.NewFake(time.Date(2026, time.November, 1, 2, 11, 0, 0, time.UTC))
	observed := newSidecarObservedClock(base)
	opened := make(chan struct{}, 1)
	cleaner := newTranscodingSegmentCleaner(segmentCleanerOptions{
		Clock:         observed,
		Retention:     20 * time.Second,
		SegmentLength: 3 * time.Second,
		SegmentStarts: annihilationGOPStarts(100),
		State: func() segmentCleanerState {
			// 25 s - 20 s = 5 s: GOP 0 ends at 5.6 s.
			return segmentCleanerState{Path: "/transcodes/main.m3u8", HLS: true, DownloadPositionTicks: 250_000_000}
		},
		OpenDir: func(string) (segmentDirectory, error) {
			opened <- struct{}{}
			return newSidecarSegmentDirectory(nil), nil
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- cleaner.Run(ctx) }()
	observed.waitTimer(t, 20*time.Second)
	base.Advance(20 * time.Second)
	observed.waitTimer(t, 20*time.Second)
	select {
	case <-opened:
		t.Fatal("cleaner opened the directory with no expired GOP")
	default:
	}
	cancel()
	awaitSidecarResult(t, run, "segment cleaner Run after cancellation")
}

func TestHLSJobSegmentBoundsFollowKeyframeCuts(t *testing.T) {
	job := &hlsJob{segmentLength: 3, runTimeTicks: 300_000_000, segmentStarts: []int64{0, 56_000_000, 112_000_000, 120_000_000}}
	for _, tc := range []struct {
		index int
		want  int64
	}{{0, 56_000_000}, {1, 112_000_000}, {2, 120_000_000}, {3, 300_000_000}, {9, 300_000_000}} {
		if got := job.segmentEnd(tc.index); got != tc.want {
			t.Errorf("segmentEnd(%d) = %d, want %d", tc.index, got, tc.want)
		}
	}
	// The guard covers the requested length plus the longest GOP.
	if got, want := job.guardSegmentTicks(), int64(30_000_000+56_000_000); got != want {
		t.Errorf("guardSegmentTicks() = %d, want %d", got, want)
	}

	nominal := &hlsJob{segmentLength: 3}
	if got, want := nominal.segmentEnd(4), int64(150_000_000); got != want {
		t.Errorf("nominal segmentEnd(4) = %d, want %d", got, want)
	}
	if got, want := nominal.guardSegmentTicks(), int64(30_000_000); got != want {
		t.Errorf("nominal guardSegmentTicks() = %d, want %d", got, want)
	}
}

func TestValidateHLSRequestRejectsUnorderedSegmentStarts(t *testing.T) {
	fixture := newHLSFixture(t, "ready")
	manager := fixture.manager(t, clock.NewFake(time.Date(2026, time.November, 1, 2, 11, 0, 0, time.UTC)))
	if _, _, err := manager.validateHLSRequest(fixture.request("ordered", 3)); err != nil {
		t.Fatalf("validateHLSRequest(no SegmentStartTicks): %v", err)
	}
	for _, starts := range [][]int64{{0, 0}, {10, 5}, {-1, 5}} {
		request := fixture.request("unordered", 3)
		request.SegmentStartTicks = starts
		if _, _, err := manager.validateHLSRequest(request); err == nil {
			t.Errorf("validateHLSRequest(SegmentStartTicks %v) succeeded, want an error", starts)
		}
	}
}
