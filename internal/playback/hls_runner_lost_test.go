// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/deadfin-org/deadfin/internal/playback"
)

// runnerLostManager loses its remote transcode host before the first
// segment wait completes.
type runnerLostManager struct {
	*hlsFakeManager
	lost int
}

func (manager *runnerLostManager) WaitSegment(ctx context.Context, playlist string, index int, timing *playback.HLSSegmentTiming) (io.ReadCloser, error) {
	if manager.lost > 0 {
		manager.lost--
		manager.waitSegmentCalls++
		return nil, fmt.Errorf("%w: worker gone", playback.ErrHLSRunnerLost)
	}
	return manager.hlsFakeManager.WaitSegment(ctx, playlist, index, timing)
}

func TestHLSServiceSegmentRestartsOnceAtTheSegmentAfterHostLoss(t *testing.T) {
	manager := &runnerLostManager{hlsFakeManager: &hlsFakeManager{segment: "restarted segment"}, lost: 1}
	service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, manager)
	request := hlsTranscodeRequest()
	request.SegmentIndex = 5

	response, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("Segment after host loss: %v", err)
	}
	if got := readHLSResponse(t, response); got != "restarted segment" {
		t.Errorf("body = %q", got)
	}
	if manager.restartCalls != 2 || manager.waitSegmentCalls != 2 {
		t.Errorf("calls = restart:%d wait:%d, want one extra restart and wait", manager.restartCalls, manager.waitSegmentCalls)
	}
	if got := manager.restartIndexes; len(got) != 2 || got[1] != 5 {
		t.Errorf("restart indexes = %v, want the lost segment", got)
	}

	manager.lost = 2
	if _, err := service.Segment(t.Context(), request); !errors.Is(err, playback.ErrHLSRunnerLost) {
		t.Fatalf("second loss = %v, want ErrHLSRunnerLost after one retry", err)
	}
}
