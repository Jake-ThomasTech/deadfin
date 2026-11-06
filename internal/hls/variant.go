// SPDX-License-Identifier: GPL-2.0-only

package hls

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/keyframes"
)

// KeyframeSource supplies metadata-derived keyframes for remux playlists.
type KeyframeSource interface {
	Keyframes(context.Context, dto.UUID, string) (keyframes.Data, bool, error)
}

// MainPlaylistRequest contains the resolved source facts for a VOD media
// playlist. EndpointPrefix and Query are intentionally opaque byte strings.
type MainPlaylistRequest struct {
	ItemID                    dto.UUID
	MediaSourceID             string
	FilePath                  string
	SegmentLengthMilliseconds int64
	RunTimeTicks              int64
	SegmentContainer          string
	EndpointPrefix            string
	Query                     string
	IsRemuxVideo              bool
	Keyframes                 KeyframeSource
}

type playlistSegment struct {
	startTicks  int64
	lengthTicks int64
	// firstKeyframe and endKeyframe bound the source keyframes of a
	// keyframe-snapped segment as [firstKeyframe, endKeyframe).
	firstKeyframe int
	endKeyframe   int
}

// CreateMainPlaylist renders a VOD media playlist. Remuxed MKV sources use
// metadata keyframes when supplied; all other streams use equal segments.
func CreateMainPlaylist(ctx context.Context, request MainPlaylistRequest) (string, error) {
	if request.SegmentLengthMilliseconds <= 0 || request.RunTimeTicks <= 0 {
		return "", fmt.Errorf("invalid segment length (%d) or runtime ticks (%d)", request.SegmentLengthMilliseconds, request.RunTimeTicks)
	}
	segmentTicks, err := segmentTicksFromMilliseconds(request.SegmentLengthMilliseconds)
	if err != nil {
		return "", err
	}
	if err := validatePlaylistRequest(request); err != nil {
		return "", err
	}
	if err := preflightMainPlaylist(request, segmentTicks); err != nil {
		return "", err
	}

	segments, err := mainPlaylistSegments(ctx, request)
	if err != nil {
		return "", err
	}
	if len(segments) == 0 {
		return "", fmt.Errorf("no playlist segments")
	}
	if err := checkPlaylistSegmentCount(int64(len(segments))); err != nil {
		return "", err
	}
	playlistSize, err := mainPlaylistSize(request, segments)
	if err != nil {
		return "", err
	}

	extension := segmentExtension(request.SegmentContainer)
	isFMP4 := strings.EqualFold(extension, ".mp4")
	version := "3"
	if isFMP4 {
		version = "7"
	}
	maxTicks := int64(0)
	for _, segment := range segments {
		if segment.lengthTicks > maxTicks {
			maxTicks = segment.lengthTicks
		}
	}

	var builder strings.Builder
	builder.Grow(playlistSize)
	builder.WriteString("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:")
	builder.WriteString(version)
	builder.WriteString("\n#EXT-X-TARGETDURATION:")
	builder.WriteString(strconv.FormatInt(ceilTicksToSeconds(maxTicks), 10))
	builder.WriteString("\n#EXT-X-MEDIA-SEQUENCE:0\n")
	if isFMP4 {
		builder.WriteString("#EXT-X-MAP:URI=\"")
		builder.WriteString(request.EndpointPrefix)
		builder.WriteString("-1")
		builder.WriteString(extension)
		appendSegmentQuery(&builder, request.Query, 0, 0)
		builder.WriteString("\"\n")
	}
	for index, segment := range segments {
		builder.WriteString("#EXTINF:")
		appendDuration(&builder, segment.lengthTicks)
		builder.WriteString(", nodesc\n")
		builder.WriteString(request.EndpointPrefix)
		builder.WriteString(strconv.Itoa(index))
		builder.WriteString(extension)
		appendSegmentQuery(&builder, request.Query, segment.startTicks, segment.lengthTicks)
		builder.WriteByte('\n')
	}
	builder.WriteString("#EXT-X-ENDLIST\n")
	return builder.String(), nil
}

func preflightMainPlaylist(request MainPlaylistRequest, segmentTicks int64) error {
	count := playlistSegmentCount(request.RunTimeTicks, segmentTicks)
	if count > maxPlaylistSegmentCount {
		// A remux source can collapse an otherwise enormous equal plan to a
		// small keyframe plan. Preserve that existing path past the CLR array
		// ceiling, where equal-segment planning already cannot proceed.
		if count <= maxCLRSegmentCount {
			return errPlaylistOutputLimit
		}
		return nil
	}
	return mainEqualPlaylistSize(request, segmentTicks)
}

func mainEqualPlaylistSize(request MainPlaylistRequest, segmentTicks int64) error {
	count := playlistSegmentCount(request.RunTimeTicks, segmentTicks)
	if err := checkPlaylistSegmentCount(count); err != nil {
		return err
	}

	maxTicks := segmentTicks
	if request.RunTimeTicks < maxTicks {
		maxTicks = request.RunTimeTicks
	}
	counter := playlistSizeCounter{}
	addMainPlaylistHeaderSize(&counter, request, maxTicks)
	for index, startTicks := int64(0), int64(0); index < count; index++ {
		lengthTicks := segmentTicks
		if remaining := request.RunTimeTicks - startTicks; remaining < lengthTicks {
			lengthTicks = remaining
		}
		addMainPlaylistSegmentSize(&counter, request, int(index), startTicks, lengthTicks)
		startTicks += lengthTicks
	}
	counter.addString("#EXT-X-ENDLIST\n")
	_, err := counter.result()
	return err
}

func mainPlaylistSize(request MainPlaylistRequest, segments []playlistSegment) (int, error) {
	maxTicks := int64(0)
	for _, segment := range segments {
		if segment.lengthTicks > maxTicks {
			maxTicks = segment.lengthTicks
		}
	}

	counter := playlistSizeCounter{}
	addMainPlaylistHeaderSize(&counter, request, maxTicks)
	for index, segment := range segments {
		addMainPlaylistSegmentSize(&counter, request, index, segment.startTicks, segment.lengthTicks)
	}
	counter.addString("#EXT-X-ENDLIST\n")
	return counter.result()
}

func addMainPlaylistHeaderSize(counter *playlistSizeCounter, request MainPlaylistRequest, maxTicks int64) {
	extension := segmentExtension(request.SegmentContainer)
	isFMP4 := strings.EqualFold(extension, ".mp4")
	version := "3"
	if isFMP4 {
		version = "7"
	}
	counter.addString("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:")
	counter.addString(version)
	counter.addString("\n#EXT-X-TARGETDURATION:")
	counter.add(decimalInt64Length(ceilTicksToSeconds(maxTicks)))
	counter.addString("\n#EXT-X-MEDIA-SEQUENCE:0\n")
	if !isFMP4 {
		return
	}
	counter.addString("#EXT-X-MAP:URI=\"")
	counter.addString(request.EndpointPrefix)
	counter.addString("-1")
	counter.addString(extension)
	addSegmentQuerySize(counter, request.Query, 0, 0)
	counter.addString("\"\n")
}

func addMainPlaylistSegmentSize(counter *playlistSizeCounter, request MainPlaylistRequest, index int, startTicks, lengthTicks int64) {
	counter.addString("#EXTINF:")
	counter.add(durationStringLength(lengthTicks))
	counter.addString(", nodesc\n")
	counter.addString(request.EndpointPrefix)
	counter.add(decimalInt64Length(int64(index)))
	counter.addString(segmentExtension(request.SegmentContainer))
	addSegmentQuerySize(counter, request.Query, startTicks, lengthTicks)
	counter.add(1)
}

func addSegmentQuerySize(counter *playlistSizeCounter, query string, runtimeTicks, lengthTicks int64) {
	if query == "" || query == "?" {
		counter.add(1)
	} else {
		counter.addString(query)
		counter.add(1)
	}
	counter.addString("runtimeTicks=")
	counter.add(decimalInt64Length(runtimeTicks))
	counter.addString("&actualSegmentLengthTicks=")
	counter.add(decimalInt64Length(lengthTicks))
}

func durationStringLength(ticks int64) int {
	negative := ticks < 0
	magnitude := uint64(ticks)
	if negative {
		magnitude = uint64(-(ticks + 1)) + 1
	}

	wholeSeconds := magnitude / uint64(ticksPerSecond)
	fraction := magnitude % uint64(ticksPerSecond)
	microseconds := fraction / 10
	if fraction%10 >= 5 {
		microseconds++
		if microseconds == 1_000_000 {
			wholeSeconds++
		}
	}

	length := decimalUint64Length(wholeSeconds) + 1 + 6
	if negative {
		length++
	}
	return length
}

// KeyframeSegment is one segment of a keyframe-snapped remux playlist. It
// spans the source keyframes [FirstKeyframe, EndKeyframe) of its plan.
type KeyframeSegment struct {
	StartTicks    int64
	LengthTicks   int64
	FirstKeyframe int
	EndKeyframe   int
}

// KeyframePlan is the keyframe-snapped segment list that CreateMainPlaylist
// advertises for a remux.
type KeyframePlan struct {
	KeyframeTicks []int64
	Segments      []KeyframeSegment
}

// RemuxKeyframePlan returns the plan that CreateMainPlaylist advertises for
// request when it snaps segments to source keyframes. It returns false when
// the playlist uses equal-length segments, and also when a segment spans no
// keyframe, because such a plan cannot be cut one keyframe at a time.
func RemuxKeyframePlan(ctx context.Context, request MainPlaylistRequest) (KeyframePlan, bool, error) {
	if !keyframePlaylistEligible(request) {
		return KeyframePlan{}, false, nil
	}
	data, found, err := request.Keyframes.Keyframes(ctx, request.ItemID, request.MediaSourceID)
	if err != nil || !found {
		return KeyframePlan{}, false, nil
	}
	for index := 1; index < len(data.KeyframeTicks); index++ {
		if data.KeyframeTicks[index] <= data.KeyframeTicks[index-1] {
			return KeyframePlan{}, false, nil
		}
	}
	segments, err := snapKeyframeSegments(data, request.SegmentLengthMilliseconds)
	if err != nil {
		return KeyframePlan{}, false, err
	}
	plan := KeyframePlan{KeyframeTicks: data.KeyframeTicks, Segments: make([]KeyframeSegment, len(segments))}
	for index, segment := range segments {
		if segment.firstKeyframe >= segment.endKeyframe {
			return KeyframePlan{}, false, nil
		}
		plan.Segments[index] = KeyframeSegment{
			StartTicks:    segment.startTicks,
			LengthTicks:   segment.lengthTicks,
			FirstKeyframe: segment.firstKeyframe,
			EndKeyframe:   segment.endKeyframe,
		}
	}
	return plan, true, nil
}

func keyframePlaylistEligible(request MainPlaylistRequest) bool {
	return request.IsRemuxVideo && !request.ItemID.IsNil() && request.Keyframes != nil && extractionAllowed(request.FilePath)
}

func mainPlaylistSegments(ctx context.Context, request MainPlaylistRequest) ([]playlistSegment, error) {
	if keyframePlaylistEligible(request) {
		// A failed or missing extraction falls back to equal-length
		// segments; it does not fail the playlist (DynamicHlsPlaylistGenerator.cs).
		data, found, err := request.Keyframes.Keyframes(ctx, request.ItemID, request.MediaSourceID)
		if err == nil && found {
			return snapKeyframeSegments(data, request.SegmentLengthMilliseconds)
		}
	}
	return equalSegments(request.RunTimeTicks, request.SegmentLengthMilliseconds)
}

func equalSegments(runtimeTicks, segmentLengthMilliseconds int64) ([]playlistSegment, error) {
	segmentTicks, err := segmentTicksFromMilliseconds(segmentLengthMilliseconds)
	if err != nil {
		return nil, err
	}
	count, err := checkedSegmentCount(runtimeTicks, segmentTicks)
	if err != nil {
		return nil, err
	}
	whole := runtimeTicks / segmentTicks
	remainder := runtimeTicks % segmentTicks

	segments := make([]playlistSegment, count)
	var start int64
	for index := range segments[:int(whole)] {
		segments[index] = playlistSegment{startTicks: start, lengthTicks: segmentTicks}
		start += segmentTicks
	}
	if remainder != 0 {
		segments[len(segments)-1] = playlistSegment{startTicks: start, lengthTicks: remainder}
	}
	return segments, nil
}

func segmentTicksFromMilliseconds(segmentLengthMilliseconds int64) (int64, error) {
	const ticksPerMillisecond int64 = 10_000
	if segmentLengthMilliseconds <= 0 || segmentLengthMilliseconds > math.MaxInt64/ticksPerMillisecond {
		return 0, fmt.Errorf("invalid segment length (%d milliseconds)", segmentLengthMilliseconds)
	}
	return segmentLengthMilliseconds * ticksPerMillisecond, nil
}

func snapKeyframeSegments(data keyframes.Data, segmentLengthMilliseconds int64) ([]playlistSegment, error) {
	if len(data.KeyframeTicks) > 0 && data.TotalDurationTicks < data.KeyframeTicks[len(data.KeyframeTicks)-1] {
		data.TotalDurationTicks = data.KeyframeTicks[len(data.KeyframeTicks)-1]
	}
	if data.TotalDurationTicks <= 0 {
		return nil, fmt.Errorf("invalid keyframe duration")
	}
	for _, keyframe := range data.KeyframeTicks {
		if keyframe < 0 {
			return nil, fmt.Errorf("invalid keyframe data")
		}
	}

	desiredTicks, err := segmentTicksFromMilliseconds(segmentLengthMilliseconds)
	if err != nil {
		return nil, err
	}
	desiredCut := desiredTicks
	lastKeyframe := int64(0)
	segments := make([]playlistSegment, 0)
	lastIndex := 0
	for index, keyframe := range data.KeyframeTicks {
		if keyframe >= desiredCut {
			length := keyframe - lastKeyframe
			if int64(len(segments)) >= maxPlaylistSegmentCount {
				return nil, errPlaylistOutputLimit
			}
			if int64(len(segments)) >= maxCLRSegmentCount {
				return nil, fmt.Errorf("segment count exceeds CLR maximum array length (%d)", maxCLRSegmentCount)
			}
			segments = append(segments, playlistSegment{startTicks: lastKeyframe, lengthTicks: length, firstKeyframe: lastIndex, endKeyframe: index})
			lastKeyframe, lastIndex = keyframe, index
			if desiredCut > math.MaxInt64-desiredTicks {
				break
			}
			desiredCut += desiredTicks
		}
	}
	if remaining := data.TotalDurationTicks - lastKeyframe; remaining > 0 {
		if int64(len(segments)) >= maxPlaylistSegmentCount {
			return nil, errPlaylistOutputLimit
		}
		if int64(len(segments)) >= maxCLRSegmentCount {
			return nil, fmt.Errorf("segment count exceeds CLR maximum array length (%d)", maxCLRSegmentCount)
		}
		segments = append(segments, playlistSegment{startTicks: lastKeyframe, lengthTicks: remaining, firstKeyframe: lastIndex, endKeyframe: len(data.KeyframeTicks)})
	}
	return segments, nil
}

func extractionAllowed(filePath string) bool {
	return strings.EqualFold(filepath.Ext(filePath), ".mkv")
}

func segmentExtension(container string) string {
	if strings.EqualFold(container, "fmp4") {
		return ".mp4"
	}
	if container == "" {
		return ""
	}
	return "." + container
}

func appendSegmentQuery(builder *strings.Builder, query string, runtimeTicks, lengthTicks int64) {
	if query == "" || query == "?" {
		builder.WriteByte('?')
	} else {
		builder.WriteString(query)
		builder.WriteByte('&')
	}
	builder.WriteString("runtimeTicks=")
	builder.WriteString(strconv.FormatInt(runtimeTicks, 10))
	builder.WriteString("&actualSegmentLengthTicks=")
	builder.WriteString(strconv.FormatInt(lengthTicks, 10))
}

func appendDuration(builder *strings.Builder, ticks int64) {
	negative := ticks < 0
	magnitude := uint64(ticks)
	if negative {
		magnitude = uint64(-(ticks + 1)) + 1
	}

	wholeSeconds := magnitude / uint64(ticksPerSecond)
	fraction := magnitude % uint64(ticksPerSecond)
	microseconds := fraction / 10
	if fraction%10 >= 5 {
		microseconds++
		if microseconds == 1_000_000 {
			wholeSeconds++
			microseconds = 0
		}
	}
	if negative {
		builder.WriteByte('-')
	}
	builder.WriteString(strconv.FormatUint(wholeSeconds, 10))
	builder.WriteByte('.')
	microsecondString := strconv.FormatUint(microseconds, 10)
	for range 6 - len(microsecondString) {
		builder.WriteByte('0')
	}
	builder.WriteString(microsecondString)
}

func ceilTicksToSeconds(ticks int64) int64 {
	wholeSeconds := ticks / ticksPerSecond
	if ticks%ticksPerSecond != 0 {
		wholeSeconds++
	}
	return wholeSeconds
}

func validatePlaylistRequest(request MainPlaylistRequest) error {
	if request.EndpointPrefix == "" || strings.ContainsAny(request.EndpointPrefix, "\r\n") {
		return fmt.Errorf("invalid endpoint prefix")
	}
	if request.Query != "" && (request.Query[0] != '?' || strings.ContainsAny(request.Query, "\r\n")) {
		return fmt.Errorf("invalid query")
	}
	if request.SegmentContainer == "" || strings.ContainsAny(request.SegmentContainer, "\r\n/?#") {
		return fmt.Errorf("invalid segment container")
	}
	return nil
}

// MainPlaylistSegment returns the start and length of one segment of the
// media playlist that CreateMainPlaylist renders for request, in ticks: the
// runtimeTicks and actualSegmentLengthTicks of its segment URL. A transcode
// handoff uses it to restart a family at a segment no client has asked for
// yet (#617).
func MainPlaylistSegment(ctx context.Context, request MainPlaylistRequest, index int) (startTicks, lengthTicks int64, err error) {
	if request.SegmentLengthMilliseconds <= 0 || request.RunTimeTicks <= 0 {
		return 0, 0, fmt.Errorf("invalid segment length (%d) or runtime ticks (%d)", request.SegmentLengthMilliseconds, request.RunTimeTicks)
	}
	segmentTicks, err := segmentTicksFromMilliseconds(request.SegmentLengthMilliseconds)
	if err != nil {
		return 0, 0, err
	}
	if err := preflightMainPlaylist(request, segmentTicks); err != nil {
		return 0, 0, err
	}
	segments, err := mainPlaylistSegments(ctx, request)
	if err != nil {
		return 0, 0, err
	}
	if index < 0 || index >= len(segments) {
		return 0, 0, fmt.Errorf("segment %d is outside the playlist (%d segments)", index, len(segments))
	}
	return segments[index].startTicks, segments[index].lengthTicks, nil
}
