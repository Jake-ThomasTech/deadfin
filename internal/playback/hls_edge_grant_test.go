// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/playback"
)

// TestHLSServiceMainAddsTheEdgeGrantToSegmentURLs (#619): with an edge
// grant configured, every segment URL of the media playlist carries the
// grant of its family, and a segment request that carries the grant joins
// the same family. Without the configuration the playlist does not change.
func TestHLSServiceMainAddsTheEdgeGrantToSegmentURLs(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/feature.mp4")
	runtime := int64(130_000_000)
	sourceInfo.RunTimeTicks = &runtime
	manager := &hlsFakeManager{segment: "segment"}
	config := hlsServiceConfig(&progressiveFakeSource{source: sourceInfo}, manager)
	var grantedPlaylist, grantedPattern, grantedSession string
	var grantedItem dto.UUID
	config.EdgeGrant = func(itemID dto.UUID, playSessionID, playlistPath, segmentPattern string) string {
		grantedItem, grantedSession, grantedPlaylist, grantedPattern = itemID, playSessionID, playlistPath, segmentPattern
		return "dfe1.claims.mac"
	}
	service := playback.NewHLSService(config)
	request := hlsTranscodeRequest()
	request.RawQuery = "api_key=token-3"

	response, err := service.Main(t.Context(), request)
	if err != nil {
		t.Fatalf("Main(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	const want = "hls/main/0.ts?api_key=token-3&DeadfinEdge=dfe1.claims.mac&runtimeTicks=0&actualSegmentLengthTicks=60000000"
	if !strings.Contains(playlist, want) || strings.Count(playlist, "DeadfinEdge=") != strings.Count(playlist, ".ts?") {
		t.Fatalf("Main() playlist = %q, want the grant on every segment URL", playlist)
	}
	if grantedItem != request.ItemID || grantedSession != request.PlaySessionID || !strings.HasSuffix(grantedPlaylist, ".m3u8") || !strings.Contains(grantedPattern, "%d") {
		t.Fatalf("grant inputs = %s %q %q %q", grantedItem, grantedSession, grantedPlaylist, grantedPattern)
	}

	request.RawQuery = "api_key=token-3&DeadfinEdge=dfe1.claims.mac&runtimeTicks=0&actualSegmentLengthTicks=60000000"
	if _, err := service.Segment(t.Context(), request); err != nil {
		t.Fatalf("Segment(): %v", err)
	}
	if manager.lastRestart.PlaylistPath != grantedPlaylist || manager.lastRestart.SegmentPattern != grantedPattern {
		t.Fatalf("segment family = %q %q, want the granted family %q %q", manager.lastRestart.PlaylistPath, manager.lastRestart.SegmentPattern, grantedPlaylist, grantedPattern)
	}

	// A stale grant in the request is replaced, not repeated.
	request.RawQuery = "api_key=token-3&DeadfinEdge=stale"
	response, err = service.Main(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replaced := readHLSResponse(t, response); strings.Contains(replaced, "stale") || !strings.Contains(replaced, "DeadfinEdge=dfe1.claims.mac") {
		t.Fatalf("Main() with a stale grant = %q", replaced)
	}

	plain := newHLSService(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	request.RawQuery = "api_key=token-3"
	response, err = plain.Main(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readHLSResponse(t, response), "DeadfinEdge") {
		t.Fatal("a server without an edge added a grant")
	}
}
