// SPDX-License-Identifier: GPL-2.0-only

package hls_test

import (
	"context"
	"errors"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/hls"
	"github.com/deadfin-org/deadfin/internal/keyframes"
)

func TestCreateMainPlaylistPreservesUpstreamTSVODBytes(t *testing.T) {
	request := hls.MainPlaylistRequest{
		FilePath:                  "/media/feature.mkv",
		SegmentLengthMilliseconds: 6_000,
		RunTimeTicks:              130_000_000,
		SegmentContainer:          "ts",
		EndpointPrefix:            "hls1/main/",
		Query:                     "?api_key=token",
	}

	got, err := hls.CreateMainPlaylist(t.Context(), request)
	if err != nil {
		t.Fatalf("CreateMainPlaylist() error = %v", err)
	}
	const want = "#EXTM3U\n" +
		"#EXT-X-PLAYLIST-TYPE:VOD\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:6.000000, nodesc\n" +
		"hls1/main/0.ts?api_key=token&runtimeTicks=0&actualSegmentLengthTicks=60000000\n" +
		"#EXTINF:6.000000, nodesc\n" +
		"hls1/main/1.ts?api_key=token&runtimeTicks=60000000&actualSegmentLengthTicks=60000000\n" +
		"#EXTINF:1.000000, nodesc\n" +
		"hls1/main/2.ts?api_key=token&runtimeTicks=120000000&actualSegmentLengthTicks=10000000\n" +
		"#EXT-X-ENDLIST\n"
	if got != want {
		t.Errorf("TS VOD playlist bytes differ:\n got: %q\nwant: %q", got, want)
	}
}

func TestCreateMainPlaylistPreservesUpstreamFMP4VODBytes(t *testing.T) {
	request := hls.MainPlaylistRequest{
		FilePath:                  "/media/feature.mp4",
		SegmentLengthMilliseconds: 6_000,
		RunTimeTicks:              130_000_000,
		SegmentContainer:          "fmp4",
		EndpointPrefix:            "hls1/main/",
		Query:                     "?api_key=token",
	}

	got, err := hls.CreateMainPlaylist(t.Context(), request)
	if err != nil {
		t.Fatalf("CreateMainPlaylist() error = %v", err)
	}
	const want = "#EXTM3U\n" +
		"#EXT-X-PLAYLIST-TYPE:VOD\n" +
		"#EXT-X-VERSION:7\n" +
		"#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXT-X-MAP:URI=\"hls1/main/-1.mp4?api_key=token&runtimeTicks=0&actualSegmentLengthTicks=0\"\n" +
		"#EXTINF:6.000000, nodesc\n" +
		"hls1/main/0.mp4?api_key=token&runtimeTicks=0&actualSegmentLengthTicks=60000000\n" +
		"#EXTINF:6.000000, nodesc\n" +
		"hls1/main/1.mp4?api_key=token&runtimeTicks=60000000&actualSegmentLengthTicks=60000000\n" +
		"#EXTINF:1.000000, nodesc\n" +
		"hls1/main/2.mp4?api_key=token&runtimeTicks=120000000&actualSegmentLengthTicks=10000000\n" +
		"#EXT-X-ENDLIST\n"
	if got != want {
		t.Errorf("fMP4 VOD playlist bytes differ:\n got: %q\nwant: %q", got, want)
	}
}

// TestCreateMainPlaylistWritesEmptyRawQueryWithoutAnExtraSeparator defends the
// URI query boundary: an otherwise-empty raw query still introduces the first
// generated query parameter directly, for both supported segment formats.
func TestCreateMainPlaylistWritesEmptyRawQueryWithoutAnExtraSeparator(t *testing.T) {
	for _, tc := range []struct {
		name, segmentContainer, want string
	}{
		{
			name:             "TS segment URI",
			segmentContainer: "ts",
			want: "#EXTM3U\n" +
				"#EXT-X-PLAYLIST-TYPE:VOD\n" +
				"#EXT-X-VERSION:3\n" +
				"#EXT-X-TARGETDURATION:1\n" +
				"#EXT-X-MEDIA-SEQUENCE:0\n" +
				"#EXTINF:1.000000, nodesc\n" +
				"hls1/main/0.ts?runtimeTicks=0&actualSegmentLengthTicks=10000000\n" +
				"#EXT-X-ENDLIST\n",
		},
		{
			name:             "fMP4 init and segment URIs",
			segmentContainer: "fmp4",
			want: "#EXTM3U\n" +
				"#EXT-X-PLAYLIST-TYPE:VOD\n" +
				"#EXT-X-VERSION:7\n" +
				"#EXT-X-TARGETDURATION:1\n" +
				"#EXT-X-MEDIA-SEQUENCE:0\n" +
				"#EXT-X-MAP:URI=\"hls1/main/-1.mp4?runtimeTicks=0&actualSegmentLengthTicks=0\"\n" +
				"#EXTINF:1.000000, nodesc\n" +
				"hls1/main/0.mp4?runtimeTicks=0&actualSegmentLengthTicks=10000000\n" +
				"#EXT-X-ENDLIST\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hls.CreateMainPlaylist(t.Context(), hls.MainPlaylistRequest{
				FilePath:                  "/media/feature.mkv",
				SegmentLengthMilliseconds: 6_000,
				RunTimeTicks:              10_000_000,
				SegmentContainer:          tc.segmentContainer,
				EndpointPrefix:            "hls1/main/",
				Query:                     "?",
			})
			if err != nil {
				t.Fatalf("CreateMainPlaylist() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("empty-query %s playlist bytes differ:\n got: %q\nwant: %q", tc.segmentContainer, got, tc.want)
			}
		})
	}
}

func TestCreateMainPlaylistSnapsEligibleRemuxToMetadataKeyframes(t *testing.T) {
	itemID, err := dto.ParseUUID("b6561ca4cfb64e7ca62e9166c0a42e30")
	if err != nil {
		t.Fatalf("ParseUUID() error = %v", err)
	}
	request := hls.MainPlaylistRequest{
		ItemID:                    itemID,
		MediaSourceID:             "primary",
		FilePath:                  "/media/feature.mkv",
		SegmentLengthMilliseconds: 6_000,
		RunTimeTicks:              350_000_000,
		SegmentContainer:          "ts",
		EndpointPrefix:            "hls1/main/",
		Query:                     "?session=golden",
		IsRemuxVideo:              true,
		Keyframes: variantKeyframeSource{
			data: keyframes.Data{
				TotalDurationTicks: 350_000_000,
				KeyframeTicks:      []int64{0, 104_270_000, 208_540_000, 312_400_000},
			},
			found: true,
		},
	}

	got, err := hls.CreateMainPlaylist(t.Context(), request)
	if err != nil {
		t.Fatalf("CreateMainPlaylist() error = %v", err)
	}
	const want = "#EXTM3U\n" +
		"#EXT-X-PLAYLIST-TYPE:VOD\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:11\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:10.427000, nodesc\n" +
		"hls1/main/0.ts?session=golden&runtimeTicks=0&actualSegmentLengthTicks=104270000\n" +
		"#EXTINF:10.427000, nodesc\n" +
		"hls1/main/1.ts?session=golden&runtimeTicks=104270000&actualSegmentLengthTicks=104270000\n" +
		"#EXTINF:10.386000, nodesc\n" +
		"hls1/main/2.ts?session=golden&runtimeTicks=208540000&actualSegmentLengthTicks=103860000\n" +
		"#EXTINF:3.760000, nodesc\n" +
		"hls1/main/3.ts?session=golden&runtimeTicks=312400000&actualSegmentLengthTicks=37600000\n" +
		"#EXT-X-ENDLIST\n"
	if got != want {
		t.Errorf("keyframe-snapped remux playlist bytes differ:\n got: %q\nwant: %q", got, want)
	}
}

func TestCreateMainPlaylistFallsBackToEqualSegmentsWhenExtractionIsDisallowed(t *testing.T) {
	request := hls.MainPlaylistRequest{
		ItemID:                    dto.NilUUID,
		MediaSourceID:             "primary",
		FilePath:                  "/media/feature.flv",
		SegmentLengthMilliseconds: 6_000,
		RunTimeTicks:              130_000_000,
		SegmentContainer:          "ts",
		EndpointPrefix:            "hls1/main/",
		Query:                     "?session=golden",
		IsRemuxVideo:              true,
		Keyframes: variantKeyframeSource{
			data: keyframes.Data{
				TotalDurationTicks: 350_000_000,
				KeyframeTicks:      []int64{0, 104_270_000, 208_540_000, 312_400_000},
			},
			found: true,
		},
	}

	got, err := hls.CreateMainPlaylist(t.Context(), request)
	if err != nil {
		t.Fatalf("CreateMainPlaylist() error = %v", err)
	}
	const want = "#EXTM3U\n" +
		"#EXT-X-PLAYLIST-TYPE:VOD\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:6.000000, nodesc\n" +
		"hls1/main/0.ts?session=golden&runtimeTicks=0&actualSegmentLengthTicks=60000000\n" +
		"#EXTINF:6.000000, nodesc\n" +
		"hls1/main/1.ts?session=golden&runtimeTicks=60000000&actualSegmentLengthTicks=60000000\n" +
		"#EXTINF:1.000000, nodesc\n" +
		"hls1/main/2.ts?session=golden&runtimeTicks=120000000&actualSegmentLengthTicks=10000000\n" +
		"#EXT-X-ENDLIST\n"
	if got != want {
		t.Errorf("disallowed-extraction fallback playlist bytes differ:\n got: %q\nwant: %q", got, want)
	}
}

// TestCreateMainPlaylistFallsBackToEqualSegmentsWhenExtractionFails defends
// DynamicHlsPlaylistGenerator.cs: a failed or missing keyframe extraction
// yields equal-length segments and never fails the playlist request.
func TestCreateMainPlaylistFallsBackToEqualSegmentsWhenExtractionFails(t *testing.T) {
	itemID, err := dto.ParseUUID("b6561ca4cfb64e7ca62e9166c0a42e30")
	if err != nil {
		t.Fatalf("ParseUUID() error = %v", err)
	}
	for name, source := range map[string]variantKeyframeSource{
		"error": {err: errors.New("extraction failed")},
		"miss":  {},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := hls.CreateMainPlaylist(t.Context(), hls.MainPlaylistRequest{
				ItemID:                    itemID,
				MediaSourceID:             "primary",
				FilePath:                  "/media/feature.mkv",
				SegmentLengthMilliseconds: 6_000,
				RunTimeTicks:              130_000_000,
				SegmentContainer:          "ts",
				EndpointPrefix:            "hls1/main/",
				Query:                     "?session=golden",
				IsRemuxVideo:              true,
				Keyframes:                 source,
			})
			if err != nil {
				t.Fatalf("CreateMainPlaylist() error = %v", err)
			}
			const want = "#EXTM3U\n" +
				"#EXT-X-PLAYLIST-TYPE:VOD\n" +
				"#EXT-X-VERSION:3\n" +
				"#EXT-X-TARGETDURATION:6\n" +
				"#EXT-X-MEDIA-SEQUENCE:0\n" +
				"#EXTINF:6.000000, nodesc\n" +
				"hls1/main/0.ts?session=golden&runtimeTicks=0&actualSegmentLengthTicks=60000000\n" +
				"#EXTINF:6.000000, nodesc\n" +
				"hls1/main/1.ts?session=golden&runtimeTicks=60000000&actualSegmentLengthTicks=60000000\n" +
				"#EXTINF:1.000000, nodesc\n" +
				"hls1/main/2.ts?session=golden&runtimeTicks=120000000&actualSegmentLengthTicks=10000000\n" +
				"#EXT-X-ENDLIST\n"
			if got != want {
				t.Errorf("failed-extraction fallback playlist bytes differ:\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// TestCreateMainPlaylistKeepsPinnedOutOfOrderKeyframePlan defends Jellyfin's
// source-order keyframe planning, including repeated and out-of-order probe
// positions that must yield its deterministic VOD playlist rather than fail.
func TestCreateMainPlaylistKeepsPinnedOutOfOrderKeyframePlan(t *testing.T) {
	itemID, err := dto.ParseUUID("b6561ca4cfb64e7ca62e9166c0a42e30")
	if err != nil {
		t.Fatalf("ParseUUID() error = %v", err)
	}
	got, err := hls.CreateMainPlaylist(t.Context(), hls.MainPlaylistRequest{
		ItemID:                    itemID,
		MediaSourceID:             "primary",
		FilePath:                  "/media/feature.mkv",
		SegmentLengthMilliseconds: 6_000,
		RunTimeTicks:              7_063_360_000,
		SegmentContainer:          "ts",
		EndpointPrefix:            "hls1/main/",
		Query:                     "?session=golden",
		IsRemuxVideo:              true,
		Keyframes: variantKeyframeSource{
			data: keyframes.Data{
				TotalDurationTicks: 7_063_360_000,
				KeyframeTicks:      []int64{0, 103_850_000, 35_000_000, 35_000_000, 20_000_000},
			},
			found: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateMainPlaylist() error = %v", err)
	}
	const want = "#EXTM3U\n" +
		"#EXT-X-PLAYLIST-TYPE:VOD\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:696\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:10.385000, nodesc\n" +
		"hls1/main/0.ts?session=golden&runtimeTicks=0&actualSegmentLengthTicks=103850000\n" +
		"#EXTINF:695.951000, nodesc\n" +
		"hls1/main/1.ts?session=golden&runtimeTicks=103850000&actualSegmentLengthTicks=6959510000\n" +
		"#EXT-X-ENDLIST\n"
	if got != want {
		t.Errorf("out-of-order keyframe playlist bytes differ:\n got: %q\nwant: %q", got, want)
	}
}

// TestCreateMainPlaylistRoundsNearMaxInt64TargetDurationWithoutOverflow
// defends the HLS target-duration ceiling for a quotient plus remainder at the
// largest supported VOD duration.
func TestCreateMainPlaylistRoundsNearMaxInt64TargetDurationWithoutOverflow(t *testing.T) {
	got, err := hls.CreateMainPlaylist(t.Context(), hls.MainPlaylistRequest{
		FilePath:                  "/media/feature.mkv",
		SegmentLengthMilliseconds: 922_337_203_685_477,
		RunTimeTicks:              9_223_372_036_854_775_807,
		SegmentContainer:          "ts",
		EndpointPrefix:            "hls1/main/",
		Query:                     "?session=large",
	})
	if err != nil {
		t.Fatalf("CreateMainPlaylist() error = %v", err)
	}
	const want = "#EXTM3U\n" +
		"#EXT-X-PLAYLIST-TYPE:VOD\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:922337203686\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:922337203685.477000, nodesc\n" +
		"hls1/main/0.ts?session=large&runtimeTicks=0&actualSegmentLengthTicks=9223372036854770000\n" +
		"#EXTINF:0.000581, nodesc\n" +
		"hls1/main/1.ts?session=large&runtimeTicks=9223372036854770000&actualSegmentLengthTicks=5807\n" +
		"#EXT-X-ENDLIST\n"
	if got != want {
		t.Errorf("near-MaxInt64 playlist bytes differ:\n got: %q\nwant: %q", got, want)
	}
}

func TestCreateMainPlaylistRejectsZeroSegmentLengthOrRuntime(t *testing.T) {
	for _, tc := range []struct {
		name               string
		segmentLengthMilli int64
		runTimeTicks       int64
		wantError          string
	}{
		{
			name:               "zero segment length",
			segmentLengthMilli: 0,
			runTimeTicks:       10_000_000,
			wantError:          "invalid segment length (0) or runtime ticks (10000000)",
		},
		{
			name:               "zero runtime",
			segmentLengthMilli: 1_000,
			runTimeTicks:       0,
			wantError:          "invalid segment length (1000) or runtime ticks (0)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := hls.CreateMainPlaylist(t.Context(), hls.MainPlaylistRequest{
				FilePath:                  "/media/feature.mkv",
				SegmentLengthMilliseconds: tc.segmentLengthMilli,
				RunTimeTicks:              tc.runTimeTicks,
				SegmentContainer:          "ts",
				EndpointPrefix:            "hls1/main/",
				Query:                     "?session=golden",
			})
			if err == nil {
				t.Fatal("CreateMainPlaylist() error = nil, want rejection of invalid segment plan")
			}
			if got := err.Error(); got != tc.wantError {
				t.Errorf("CreateMainPlaylist() error = %q, want %q", got, tc.wantError)
			}
		})
	}
}

type variantKeyframeSource struct {
	data  keyframes.Data
	found bool
	err   error
}

func (s variantKeyframeSource) Keyframes(context.Context, dto.UUID, string) (keyframes.Data, bool, error) {
	return s.data, s.found, s.err
}

func TestMainPlaylistSegmentMatchesEqualSegments(t *testing.T) {
	request := hls.MainPlaylistRequest{SegmentLengthMilliseconds: 6000, RunTimeTicks: 130_000_000, SegmentContainer: "ts"}
	for _, test := range []struct {
		index         int
		start, length int64
	}{{0, 0, 60_000_000}, {1, 60_000_000, 60_000_000}, {2, 120_000_000, 10_000_000}} {
		start, length, err := hls.MainPlaylistSegment(t.Context(), request, test.index)
		if err != nil || start != test.start || length != test.length {
			t.Fatalf("segment %d = %d, %d, %v; want %d, %d", test.index, start, length, err, test.start, test.length)
		}
	}
	if _, _, err := hls.MainPlaylistSegment(t.Context(), request, 3); err == nil {
		t.Fatal("segment past the end was accepted")
	}
}
