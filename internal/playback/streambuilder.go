// SPDX-License-Identifier: GPL-2.0-only

package playback

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
)

// StreamBuilder ports MediaBrowser.Model.Dlna.StreamBuilder at b04614e.
type StreamBuilder struct{ transcoderSupport TranscoderSupport }

var (
	supportedHLSVideoCodecs    = []string{"h264", "hevc", "vp9", "av1"}
	supportedHLSAudioCodecsTS  = []string{"aac", "ac3", "eac3", "mp3"}
	supportedHLSAudioCodecsMP4 = []string{"aac", "ac3", "eac3", "mp3", "alac", "flac", "opus", "dts", "truehd"}
)

func NewStreamBuilder(support TranscoderSupport) *StreamBuilder {
	return &StreamBuilder{transcoderSupport: support}
}

func (b *StreamBuilder) GetOptimalVideoStream(options *MediaOptions) (*StreamInfo, error) {
	if err := validateOptions(options, true); err != nil {
		return nil, err
	}
	streams := make([]*StreamInfo, 0, len(options.MediaSources))
	for i := range options.MediaSources {
		source := &options.MediaSources[i]
		if options.MediaSourceID != "" && !strings.EqualFold(source.ID, options.MediaSourceID) {
			continue
		}
		stream, err := b.buildVideoItem(source, options)
		if err != nil {
			return nil, err
		}
		if stream != nil {
			streams = append(streams, stream)
		}
	}
	return selectBest(streams, maxBitrate(options, false)), nil
}

func (b *StreamBuilder) GetOptimalAudioStream(options *MediaOptions) (*StreamInfo, error) {
	if err := validateOptions(options, false); err != nil {
		return nil, err
	}
	streams := make([]*StreamInfo, 0, len(options.MediaSources))
	for i := range options.MediaSources {
		source := &options.MediaSources[i]
		if options.MediaSourceID != "" && !strings.EqualFold(source.ID, options.MediaSourceID) {
			continue
		}
		stream, err := b.buildAudioItem(source, options)
		if err != nil {
			return nil, err
		}
		if stream != nil {
			streams = append(streams, stream)
		}
	}
	return selectBest(streams, maxBitrate(options, true)), nil
}

func validateOptions(options *MediaOptions, video bool) error {
	if options == nil || options.Profile == nil {
		return fmt.Errorf("profile is required")
	}
	if options.MediaSources == nil {
		return fmt.Errorf("media sources are required")
	}
	if options.ItemID == "" && options.DeviceID == "" {
		return fmt.Errorf("device ID is required when item ID is empty")
	}
	if video && options.MediaSourceID == "" {
		if options.AudioStreamIndex != nil {
			return fmt.Errorf("media source ID is required when a specific audio stream is requested")
		}
		if options.SubtitleStreamIndex != nil {
			return fmt.Errorf("media source ID is required when a specific subtitle stream is requested")
		}
	}
	return nil
}

func (b *StreamBuilder) newInfo(source *MediaSourceInfo, options *MediaOptions, mediaType DlnaProfileType) StreamInfo {
	info := NewStreamInfo()
	if id, err := dto.ParseUUID(options.ItemID); err == nil {
		info.ItemID = id
	}
	info.MediaType = mediaType
	info.MediaSource = source
	info.RunTimeTicks = source.RunTimeTicks
	info.Context = options.Context
	info.DeviceProfile = options.Profile
	if options.DeviceID != "" {
		info.DeviceID = stringPtr(options.DeviceID)
	}
	if options.Profile.ID != "" {
		info.DeviceProfileID = stringPtr(strings.ReplaceAll(options.Profile.ID, "-", ""))
	}
	return info
}

func stringPtr(value string) *string { return &value }
func streamPtr[T any](value T) *T    { return &value }

func maxBitrate(options *MediaOptions, audio bool) int64 {
	if value := options.MaxBitrateFor(audio); value != nil {
		return int64(*value)
	}
	return 0
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

func hlsAudioCodecs(container string) []string {
	if strings.EqualFold(container, "mp4") {
		return supportedHLSAudioCodecsMP4
	}
	return supportedHLSAudioCodecsTS
}

func selectBest(streams []*StreamInfo, limit int64) *StreamInfo {
	// LINQ OrderBy/ThenBy is stable. SliceStable retains the final IndexOf key.
	sort.SliceStable(streams, func(i, j int) bool {
		fileDirectRank := func(stream *StreamInfo) int {
			if stream.PlayMethod == PlayMethodDirectPlay && stream.MediaSource != nil && stream.MediaSource.Protocol == MediaProtocolFile {
				return 0
			}
			return 1
		}
		playRank := func(stream *StreamInfo) int {
			if stream.PlayMethod == PlayMethodDirectPlay || stream.PlayMethod == PlayMethodDirectStream {
				return 0
			}
			return 1
		}
		fileRank := func(stream *StreamInfo) int {
			if stream.MediaSource != nil && stream.MediaSource.Protocol == MediaProtocolFile {
				return 0
			}
			return 1
		}
		delta := func(stream *StreamInfo) int64 {
			if limit == 0 || stream.MediaSource == nil || stream.MediaSource.Bitrate == nil {
				return 0
			}
			return int64(math.Abs(float64(int64(*stream.MediaSource.Bitrate) - limit)))
		}
		if a, c := fileDirectRank(streams[i]), fileDirectRank(streams[j]); a != c {
			return a < c
		}
		if a, c := playRank(streams[i]), playRank(streams[j]); a != c {
			return a < c
		}
		if a, c := fileRank(streams[i]), fileRank(streams[j]); a != c {
			return a < c
		}
		return delta(streams[i]) < delta(streams[j])
	})
	if len(streams) == 0 {
		return nil
	}
	return streams[0]
}

func NormalizeMediaSourceFormatIntoSingleContainer(input string, profile *DeviceProfile, kind DlnaProfileType, play *DirectPlayProfile) string {
	if profile == nil || input == "" || !strings.Contains(input, ",") {
		return input
	}
	profiles := profile.DirectPlayProfiles
	if play != nil {
		profiles = []DirectPlayProfile{*play}
	}
	for _, format := range SplitContainers(input) {
		for _, candidate := range profiles {
			if candidate.Type == kind && candidate.SupportsContainer(format) {
				return format
			}
		}
	}
	return input
}

func (b *StreamBuilder) buildVideoItem(source *MediaSourceInfo, options *MediaOptions) (*StreamInfo, error) {
	info := b.newInfo(source, options, DlnaVideo)
	info.AlwaysBurnInSubtitleWhenTranscoding = options.AlwaysBurnInSubtitleWhenTranscoding
	info.SubtitleStreamIndex = options.SubtitleStreamIndex
	if info.SubtitleStreamIndex == nil {
		info.SubtitleStreamIndex = defaultSubtitleIndex(source, options.Profile.SubtitleProfiles)
	}
	var subtitle *MediaStream
	if info.SubtitleStreamIndex != nil {
		subtitle = source.MediaStream(StreamTypeSubtitle, *info.SubtitleStreamIndex)
	}

	audio := source.DefaultAudioStream(firstIndex(options.AudioStreamIndex, source.DefaultAudioStreamIndex))
	if audio != nil {
		info.AudioStreamIndex = streamPtr(audio.Index)
	} else if options.AudioStreamIndex != nil {
		// The requested index remains observable even if probing has not yet
		// provided its stream record.
		info.AudioStreamIndex = streamPtr(*options.AudioStreamIndex)
	}
	candidates := candidateAudioStreams(source, options, audio)
	video := source.VideoStream()

	limitExceeded := b.bitrateExceeded(source, maxBitrate(options, false))
	canPlay := options.EnableDirectPlay && (options.ForceDirectPlay || !limitExceeded)
	canStream := options.EnableDirectStream && (options.ForceDirectStream || !limitExceeded)
	if source.VideoType != nil && (*source.VideoType == VideoTypeDVD || *source.VideoType == VideoTypeBluRay) {
		canPlay = false
	}

	reasons := TranscodeReason(0)
	if limitExceeded {
		reasons |= ReasonContainerBitrateExceedsLimit
	}
	var profile *DirectPlayProfile
	var method *PlayMethod
	var selected *MediaStream
	if canPlay || canStream {
		var directReasons TranscodeReason
		var err error
		profile, method, selected, directReasons, err = b.videoDirectProfile(options, source, video, audio, candidates, subtitle, canPlay, canStream)
		if err != nil {
			return nil, err
		}
		reasons |= directReasons
	}

	var directProfile *DirectPlayProfile
	if method != nil {
		directProfile = profile
		info.PlayMethod = *method
		container := NormalizeMediaSourceFormatIntoSingleContainer(source.Container, options.Profile, DlnaVideo, profile)
		info.Container = &container
		if video != nil {
			info.VideoCodecs = []string{video.Codec}
		}
		if *method == PlayMethodDirectPlay {
			info.SubProtocol = StreamProtocolHTTP
			if selected != nil {
				info.AudioStreamIndex = streamPtr(selected.Index)
				info.AudioCodecs = []string{selected.Codec}
			}
		} else {
			container, videoCodec, audioCodec := "", "", ""
			if profile != nil {
				container, videoCodec, audioCodec = profile.Container, profile.VideoCodec, profile.AudioCodec
			}
			b.setDirectPlayOptions(options, source, &info, profile, video)
			b.buildStreamVideo(&info, options, source, video, audio, candidates, container, videoCodec, audioCodec)
		}
		if subtitle != nil {
			container := ""
			if profile != nil {
				container = profile.Container
			}
			p := GetSubtitleProfile(source, subtitle, options.Profile.SubtitleProfiles, *method, b.transcoderSupport, container, info.SubProtocol)
			info.SubtitleDeliveryMethod, info.SubtitleFormat = p.Method, stringPtr(p.Format)
		}
	}
	info.TranscodeReasons = reasons

	if method == nil {
		transcode, play, err := b.videoTranscodeProfile(options, source, video, audio)
		if err != nil {
			return nil, err
		}
		if transcode != nil && play != nil {
			info.PlayMethod = PlayMethodTranscode
			container := transcode.Container
			info.Container = &container
			info.SubProtocol = transcode.Protocol
			source.TranscodingContainer, source.TranscodingSubProtocol = container, transcode.Protocol
			b.applyTranscodeProfile(&info, transcode)
			b.buildStreamVideo(&info, options, source, video, audio, candidates, transcode.Container, transcode.VideoCodec, transcode.AudioCodec)
			if subtitle != nil {
				p := GetSubtitleProfile(source, subtitle, options.Profile.SubtitleProfiles, PlayMethodTranscode, b.transcoderSupport, transcode.Container, transcode.Protocol)
				info.SubtitleDeliveryMethod, info.SubtitleFormat = p.Method, stringPtr(p.Format)
				info.SubtitleCodecs = []string{p.Format}
			}
			if info.TranscodeReasons&(VideoReasons|ReasonContainerBitrateExceedsLimit) != 0 {
				applyTranscodeConditions(&info, transcode.Conditions, "", true, true)
			}
		}
	}
	source.Container = NormalizeMediaSourceFormatIntoSingleContainer(source.Container, options.Profile, DlnaVideo, directProfile)
	return &info, nil
}

func firstIndex(values ...*int) *int {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func candidateAudioStreams(source *MediaSourceInfo, options *MediaOptions, selected *MediaStream) []*MediaStream {
	if selected == nil {
		return nil
	}
	candidates := []*MediaStream{selected}
	if source.DefaultAudioIndexSource&AudioIndexUser != 0 || options.AudioStreamIndex != nil && *options.AudioStreamIndex >= 0 {
		return candidates
	}
	all := func(match func(*MediaStream) bool) []*MediaStream {
		result := make([]*MediaStream, 0)
		for i := range source.MediaStreams {
			stream := &source.MediaStreams[i]
			if stream.Type == StreamTypeAudio && match(stream) {
				result = append(result, stream)
			}
		}
		return result
	}
	if source.DefaultAudioIndexSource == AudioIndexNone {
		candidates = all(func(*MediaStream) bool { return true })
		if selected.IsDefault {
			candidates = all(func(stream *MediaStream) bool { return stream.IsDefault })
		}
	}
	if source.DefaultAudioIndexSource&AudioIndexLanguage != 0 {
		candidates = all(func(stream *MediaStream) bool { return stringValue(stream.Language) == stringValue(selected.Language) })
		if source.DefaultAudioIndexSource&AudioIndexDefault != 0 {
			defaults := make([]*MediaStream, 0, len(candidates))
			for _, stream := range candidates {
				if stream.IsDefault {
					defaults = append(defaults, stream)
				}
			}
			if len(defaults) != 0 {
				candidates = defaults
			} else {
				candidates = all(func(stream *MediaStream) bool { return stream.IsDefault })
			}
		}
	} else if source.DefaultAudioIndexSource&AudioIndexDefault != 0 {
		candidates = all(func(stream *MediaStream) bool { return stream.IsDefault })
	}
	return candidates
}

func (b *StreamBuilder) bitrateExceeded(source *MediaSourceInfo, max int64) bool {
	if source.IsRemote {
		return false
	}
	requested := int64(math.MaxInt32)
	if max > 0 {
		requested = max
	}
	bitrate := int64(40000000)
	if source.Bitrate != nil {
		bitrate = int64(*source.Bitrate)
	}
	return bitrate > requested
}

type directDecision struct {
	profile *DirectPlayProfile
	method  *PlayMethod
	audio   *MediaStream
	reasons TranscodeReason
	rank    int
	order   int
}

func (b *StreamBuilder) videoDirectProfile(options *MediaOptions, source *MediaSourceInfo, video, audio *MediaStream, candidates []*MediaStream, subtitle *MediaStream, canPlay, canStream bool) (*DirectPlayProfile, *PlayMethod, *MediaStream, TranscodeReason, error) {
	if options.ForceDirectPlay {
		method := PlayMethodDirectPlay
		return nil, &method, audio, 0, nil
	}
	if options.ForceDirectStream {
		method := PlayMethodDirectStream
		return nil, &method, audio, 0, nil
	}
	containerReasons, err := b.containerCompatibility(options, source, source.Container, video)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	videoReasons := TranscodeReason(0)
	if video != nil {
		videoReasons, err = b.videoCompatibility(options, source, source.Container, video)
		if err != nil {
			return nil, nil, nil, 0, err
		}
	}
	audioReasons := make(map[*MediaStream]TranscodeReason, len(candidates))
	for _, candidate := range candidates {
		failure, err := b.audioDirectCompatibility(options, source, source.Container, candidate, true)
		if err != nil {
			return nil, nil, nil, 0, err
		}
		audioReasons[candidate] = failure
	}
	subtitleReasons := TranscodeReason(0)
	if subtitle != nil {
		p := GetSubtitleProfile(source, subtitle, options.Profile.SubtitleProfiles, PlayMethodDirectPlay, b.transcoderSupport, source.Container, StreamProtocolHTTP)
		if p.Method != SubtitleDrop && p.Method != SubtitleExternal && p.Method != SubtitleEmbed {
			subtitleReasons |= ReasonSubtitleCodecNotSupported
		}
	}

	containerSupported := false
	all := make([]directDecision, 0)
	for order := range options.Profile.DirectPlayProfiles {
		profile := &options.Profile.DirectPlayProfiles[order]
		if profile.Type != DlnaVideo {
			continue
		}
		directReasons := TranscodeReason(0)
		if !profile.SupportsContainer(source.Container) {
			directReasons |= ReasonContainerNotSupported
		} else {
			containerSupported = true
		}
		videoCodec := ""
		if video != nil {
			videoCodec = video.Codec
		}
		if !profile.SupportsVideoCodec(videoCodec) {
			directReasons |= ReasonVideoCodecNotSupported
		}
		var selected *MediaStream
		if len(candidates) != 0 {
			for _, candidate := range candidates {
				if profile.SupportsAudioCodec(candidate.Codec) {
					selected = candidate
					break
				}
			}
			if selected == nil {
				directReasons |= ReasonAudioCodecNotSupported
			}
		}
		failure := directReasons | containerReasons | subtitleReasons
		if failure&ReasonVideoCodecNotSupported == 0 {
			failure |= videoReasons
		}
		if failure&ReasonAudioCodecNotSupported == 0 && selected != nil {
			failure |= audioReasons[selected]
		}
		directStreamFailure := failure &^ DirectStreamReasons
		var method *PlayMethod
		// C# MediaSourceInfo constructor defaults all support flags to true. A Go
		// struct literal has no constructor call, so an all-false triplet is its
		// uninitialized equivalent; explicit JSON false remains distinguishable
		// whenever at least one support flag is set.
		defaultSupports := !source.SupportsTranscoding && !source.SupportsDirectStream && !source.SupportsDirectPlay
		if failure == 0 && canPlay && (source.SupportsDirectPlay || defaultSupports) {
			value := PlayMethodDirectPlay
			method = &value
		} else if directStreamFailure == 0 && canStream && (source.SupportsDirectStream || defaultSupports) {
			value := PlayMethodDirectStream
			method = &value
		}
		all = append(all, directDecision{profile: profile, method: method, audio: selected, reasons: failure, rank: reasonRank(failure), order: order})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].method != nil && all[j].method == nil {
			return true
		}
		if all[i].method == nil && all[j].method != nil {
			return false
		}
		if all[i].method != nil && all[j].method != nil && *all[i].method != *all[j].method {
			return *all[i].method > *all[j].method
		}
		if all[i].rank != all[j].rank {
			return all[i].rank > all[j].rank
		}
		return all[i].order < all[j].order
	})
	for _, candidate := range all {
		if candidate.method != nil {
			return candidate.profile, candidate.method, candidate.audio, candidate.reasons, nil
		}
	}
	for _, candidate := range all {
		if !containerSupported || candidate.reasons&ReasonContainerNotSupported == 0 {
			if candidate.reasons != 0 {
				return nil, nil, nil, candidate.reasons, nil
			}
		}
	}
	return nil, nil, nil, ReasonDirectPlayError, nil
}

func reasonRank(reason TranscodeReason) int {
	rankings := []TranscodeReason{ReasonVideoCodecNotSupported, VideoCodecReasons, ReasonAudioCodecNotSupported, AudioCodecReasons, ContainerReasons}
	for index, flag := range rankings {
		if reason&flag != 0 {
			return index + 1
		}
	}
	return len(rankings) + 1
}

func (b *StreamBuilder) setDirectPlayOptions(options *MediaOptions, source *MediaSourceInfo, info *StreamInfo, profile *DirectPlayProfile, video *MediaStream) {
	container := NormalizeMediaSourceFormatIntoSingleContainer(source.Container, options.Profile, DlnaVideo, profile)
	source.TranscodingContainer, source.TranscodingSubProtocol = container, StreamProtocolHTTP
	info.Container, info.SubProtocol = &container, StreamProtocolHTTP
	if video != nil {
		info.VideoCodecs = []string{video.Codec}
	}
	if profile != nil {
		info.AudioCodecs = SplitContainers(profile.AudioCodec)
	}
}

// hasTSTranscodingProfile reports a video transcoding profile with the ts
// container for the context. A source with UseMostCompatibleTranscodingProfile
// uses only those profiles; when the client sends none, it uses the client's
// other profiles (D-89). Upstream then has no profile and falls back to a
// progressive stream; Deadfin serves no progressive stream for a live source.
func hasTSTranscodingProfile(profile *DeviceProfile, context EncodingContext) bool {
	for index := range profile.TranscodingProfiles {
		candidate := &profile.TranscodingProfiles[index]
		if candidate.Type == DlnaVideo && candidate.Context == context && strings.EqualFold(candidate.Container, "ts") {
			return true
		}
	}
	return false
}

func (b *StreamBuilder) videoTranscodeProfile(options *MediaOptions, source *MediaSourceInfo, video, audio *MediaStream) (*TranscodingProfile, *PlayMethod, error) {
	defaultSupports := !source.SupportsTranscoding && !source.SupportsDirectStream && !source.SupportsDirectPlay
	if !source.SupportsTranscoding && !source.SupportsDirectStream && !defaultSupports {
		return nil, nil, nil
	}
	bestVideo, bestAudio := 4, 4
	var best *TranscodingProfile
	mostCompatible := source.UseMostCompatibleTranscodingProfile && hasTSTranscodingProfile(options.Profile, options.Context)
	for index := range options.Profile.TranscodingProfiles {
		profile := &options.Profile.TranscodingProfiles[index]
		if profile.Type != DlnaVideo || profile.Context != options.Context || (mostCompatible && !strings.EqualFold(profile.Container, "ts")) {
			continue
		}
		videoRank := 3
		if video != nil && options.AllowVideoStreamCopy && ContainsContainer(profile.VideoCodec, video.Codec) {
			failure, err := b.videoCompatibility(options, source, profile.Container, video)
			if err != nil {
				return nil, nil, err
			}
			if failure == 0 {
				videoRank = 1
			} else {
				videoRank = 2
			}
		}
		audioRank := 3
		if audio != nil && options.AllowAudioStreamCopy {
			for _, codec := range SplitContainers(profile.AudioCodec) {
				failure, err := b.audioCompatibility(options, source, profile.Container, audio, codec, true, false)
				if err != nil {
					return nil, nil, err
				}
				if failure == 0 {
					if strings.EqualFold(codec, audio.Codec) {
						audioRank = 1
					} else if audioRank > 2 {
						audioRank = 2
					}
				}
				if audioRank == 1 {
					break
				}
			}
		}
		if best == nil || videoRank < bestVideo || videoRank == bestVideo && audioRank < bestAudio {
			best, bestVideo, bestAudio = profile, videoRank, audioRank
		}
	}
	if best == nil {
		return nil, nil, nil
	}
	method := PlayMethodTranscode
	if bestVideo == 1 {
		method = PlayMethodDirectStream
	}
	return best, &method, nil
}

func (b *StreamBuilder) applyTranscodeProfile(info *StreamInfo, profile *TranscodingProfile) {
	info.TranscodeSeekInfo = profile.TranscodeSeekInfo
	info.EstimateContentLength = profile.EstimateContentLength
	info.CopyTimestamps = profile.CopyTimestamps
	info.EnableSubtitlesInManifest = profile.EnableSubtitlesInManifest
	info.EnableMpegtsM2TsMode = profile.EnableMpegtsM2TsMode
	info.EnableAudioVbrEncoding = profile.EnableAudioVbrEncoding
	if value, err := strconv.Atoi(strings.TrimSpace(profile.MaxAudioChannels)); err == nil {
		info.TranscodingMaxAudioChannels = streamPtr(value)
	}
	if profile.MinSegments > 0 {
		info.MinSegments = streamPtr(int(profile.MinSegments))
	}
	if profile.SegmentLength > 0 {
		info.SegmentLength = streamPtr(int(profile.SegmentLength))
	}
}

func (b *StreamBuilder) buildStreamVideo(info *StreamInfo, options *MediaOptions, source *MediaSourceInfo, video, selected *MediaStream, candidates []*MediaStream, container, videoCodec, audioCodec string) {
	videoCodecs := SplitContainers(videoCodec)
	if len(videoCodecs) == 0 && video != nil {
		videoCodecs = []string{video.Codec}
	}
	if info.SubProtocol == StreamProtocolHLS {
		videoCodecs = filterSupported(videoCodecs, supportedHLSVideoCodecs)
	}
	info.VideoCodecs = videoCodecs
	if video != nil && !contains(videoCodecs, video.Codec) {
		info.TranscodeReasons |= ReasonVideoCodecNotSupported
	}
	if video != nil {
		info.MaxFramerate = video.ReferenceFrameRate()
		if video.Level != nil {
			info.SetOption(video.Codec, "level", strconv.FormatFloat(*video.Level, 'f', -1, 64))
		}
		if video.BitDepth != nil {
			info.SetOption(video.Codec, "videobitdepth", strconv.Itoa(*video.BitDepth))
		}
		if video.Profile != nil {
			info.SetOption(video.Codec, "profile", strings.ToLower(*video.Profile))
		}
	}
	audioCodecs := SplitContainers(audioCodec)
	if len(audioCodecs) == 0 && selected != nil {
		audioCodecs = []string{selected.Codec}
	}
	if info.SubProtocol == StreamProtocolHLS {
		audioCodecs = filterSupported(audioCodecs, hlsAudioCodecs(container))
	}
	var compatible *MediaStream
	for _, candidate := range candidates {
		if contains(audioCodecs, candidate.Codec) {
			compatible = candidate
			break
		}
	}
	channelsExceed := compatible != nil && info.TranscodingMaxAudioChannels != nil && compatible.Channels != nil && *compatible.Channels > *info.TranscodingMaxAudioChannels
	if compatible == nil && selected != nil {
		info.TranscodeReasons |= ReasonAudioCodecNotSupported
	}
	directAudioFailures := TranscodeReason(0)
	if compatible != nil {
		failure, err := b.audioCompatibility(options, source, container, compatible, "", true, false)
		if err == nil {
			directAudioFailures = failure
			info.TranscodeReasons |= failure
		}
	}
	if channelsExceed && info.TargetAudioStream() != nil {
		info.TranscodeReasons |= ReasonAudioChannelsNotSupported
		if info.TranscodingMaxAudioChannels != nil {
			info.TargetAudioStream().Channels = streamPtr(*info.TranscodingMaxAudioChannels)
		}
	}
	info.AudioCodecs = audioCodecs
	// A request which did not explicitly select a source keeps the output
	// profile's audio candidates. The server has selected a source on its
	// behalf, but it must not turn that into a client request to copy the
	// source audio stream.
	directAudioCopyAllowed := options.MediaSourceID != "" && directAudioFailures == 0
	if compatible != nil && !directAudioCopyAllowed && directAudioFailures == 0 && info.TranscodeReasons&VideoReasons != 0 {
		info.TranscodeReasons |= ReasonAudioCodecNotSupported
	}
	if compatible != nil && directAudioCopyAllowed && !channelsExceed && info.TranscodeReasons&ReasonContainerBitrateExceedsLimit == 0 {
		selected = compatible
		info.AudioStreamIndex = streamPtr(selected.Index)
		info.AudioCodecs = []string{selected.Codec}
		info.AudioSampleRate = selected.SampleRate
		qualifier := ""
		if video != nil {
			qualifier = video.Codec
		}
		channels := ""
		if selected.Channels != nil {
			channels = strconv.Itoa(*selected.Channels)
		}
		info.SetOption(qualifier, "audiochannels", channels)
		if selected.Profile != nil {
			info.SetOption(selected.Codec, "profile", strings.ToLower(*selected.Profile))
		}
		if selected.Level != nil && *selected.Level != 0 {
			info.SetOption(selected.Codec, "level", strconv.FormatFloat(*selected.Level, 'f', -1, 64))
		}
	}
	applyVideoConditions(info, options, source, video, container)
	if channelsExceed {
		info.GlobalMaxAudioChannels = info.TranscodingMaxAudioChannels
	} else {
		info.GlobalMaxAudioChannels = options.MaxAudioChannels
	}
	bitrate := getAudioBitrate(maxBitrate(options, true), first(info.TargetAudioCodec()), selected, info)
	if info.AudioBitrate == nil || *info.AudioBitrate > bitrate {
		info.AudioBitrate = streamPtr(bitrate)
	}
	applyAudioConditions(info, options, source, selected, container)
	if total := maxBitrate(options, false); total > 0 {
		available := total - int64(*info.AudioBitrate)
		current := available
		if info.VideoBitrate != nil {
			current = int64(*info.VideoBitrate)
		}
		if current > available {
			current = available
		}
		if current < 64000 {
			current = 64000
		}
		info.VideoBitrate = streamPtr(int(current))
	}
}

func filterSupported(values, allowed []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if contains(allowed, value) {
			out = append(out, value)
		}
	}
	return out
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func getAudioBitrate(total int64, codec string, audio *MediaStream, info *StreamInfo) int {
	channels := info.targetAudioChannels(codec)
	defaultRate := 192000
	if audio != nil {
		if audio.Channels != nil && channels != nil && *audio.Channels > *channels {
			defaultRate = defaultAudioBitrate(codec, *channels)
		} else if audio.Channels != nil && channels != nil && *audio.Channels <= *channels && !contains(info.AudioCodecs, audio.Codec) {
			defaultRate = defaultAudioBitrate(codec, *audio.Channels)
		} else if audio.BitRate != nil {
			defaultRate = *audio.BitRate
		} else {
			channelCount := 0
			if channels != nil {
				channelCount = *channels
			}
			defaultRate = defaultAudioBitrate(codec, channelCount)
		}
		if audio.Channels != nil && *audio.Channels == 1 && (audio.BitRate == nil || *audio.BitRate < 64000) && defaultRate > 64000 {
			defaultRate = 64000
		}
	}
	if total > 0 && defaultRate > maxAudioForTotal(total) {
		defaultRate = maxAudioForTotal(total)
	}
	return defaultRate
}

func defaultAudioBitrate(codec string, channels int) int {
	if strings.EqualFold(codec, "aac") || strings.EqualFold(codec, "mp3") || strings.EqualFold(codec, "ac3") || strings.EqualFold(codec, "eac3") {
		if channels < 2 {
			return 128000
		}
		if channels >= 6 {
			return 640000
		}
		return 384000
	}
	if strings.EqualFold(codec, "flac") || strings.EqualFold(codec, "alac") {
		if channels < 2 {
			return 768000
		}
		if channels >= 6 {
			return 3584000
		}
		return 1536000
	}
	return 192000
}

func maxAudioForTotal(total int64) int {
	switch {
	case total <= 640000:
		return 128000
	case total <= 2000000:
		return 384000
	case total <= 3000000:
		return 448000
	case total <= 4000000:
		return 640000
	case total <= 5000000:
		return 768000
	case total <= 10000000:
		return 1536000
	case total <= 15000000:
		return 2304000
	case total <= 20000000:
		return 3584000
	default:
		return 7168000
	}
}

func (b *StreamBuilder) buildAudioItem(source *MediaSourceInfo, options *MediaOptions) (*StreamInfo, error) {
	info := b.newInfo(source, options, DlnaAudio)
	if options.ForceDirectPlay {
		info.PlayMethod = PlayMethodDirectPlay
		container := NormalizeMediaSourceFormatIntoSingleContainer(source.Container, options.Profile, DlnaAudio, nil)
		info.Container = &container
		return &info, nil
	}
	if options.ForceDirectStream {
		info.PlayMethod = PlayMethodDirectStream
		container := NormalizeMediaSourceFormatIntoSingleContainer(source.Container, options.Profile, DlnaAudio, nil)
		info.Container = &container
		return &info, nil
	}
	audio := source.DefaultAudioStream(nil)
	if audio == nil {
		return nil, fmt.Errorf("audio stream is required")
	}
	profile, method, reasons, err := b.audioDirectProfile(source, audio, options)
	if err != nil {
		return nil, err
	}
	if method != nil && *method == PlayMethodDirectPlay {
		failure, err := b.audioCompatibility(options, source, source.Container, audio, "", false, false)
		if err != nil {
			return nil, err
		}
		reasons |= failure
		if failure == 0 {
			info.PlayMethod = *method
			container := NormalizeMediaSourceFormatIntoSingleContainer(source.Container, options.Profile, DlnaAudio, profile)
			info.Container = &container
			return &info, nil
		}
	}
	if method != nil && *method == PlayMethodDirectStream {
		container := source.TranscodingContainer
		if container == "" {
			container = "ts"
		}
		if profile != nil && (strings.EqualFold(profile.Container, "ts") || strings.EqualFold(profile.Container, "mp4")) {
			container = profile.Container
		}
		codecs := hlsAudioCodecs(container)
		codec := ""
		if profile != nil {
			codec = profile.AudioCodec
			if codec == "" {
				codec = profile.Container
			}
		}
		if source.TranscodingSubProtocol != StreamProtocolHLS || contains(codecs, codec) {
			info.PlayMethod = *method
			info.Container = &container
			info.TranscodeReasons = reasons
			info.SubProtocol = source.TranscodingSubProtocol
			source.TranscodingContainer = container
			return &info, nil
		}
		reasons |= ReasonAudioCodecNotSupported
	}
	for index := range options.Profile.TranscodingProfiles {
		profile := &options.Profile.TranscodingProfiles[index]
		if profile.Type != DlnaAudio || profile.Context != options.Context || b.transcoderSupport == nil || !b.transcoderSupport.CanEncodeToAudioCodec(firstNonEmpty(profile.AudioCodec, profile.Container)) {
			continue
		}
		if !source.SupportsTranscoding {
			return nil, nil
		}
		info.PlayMethod = PlayMethodTranscode
		info.Container = stringPtr(profile.Container)
		info.SubProtocol = profile.Protocol
		source.TranscodingContainer, source.TranscodingSubProtocol = profile.Container, profile.Protocol
		b.applyTranscodeProfile(&info, profile)
		failure, err := b.audioCompatibility(options, source, profile.Container, audio, profile.AudioCodec, false, false)
		if err != nil {
			return nil, err
		}
		input := audioInput(source, audio)
		for profileIndex := range options.Profile.CodecProfiles {
			codecProfile := &options.Profile.CodecProfiles[profileIndex]
			if codecProfile.Type != CodecAudio || !codecProfile.ContainsAnyCodec(profile.AudioCodec, profile.Container, false) {
				continue
			}
			applied, err := allAudioConditions(codecProfile.ApplyConditions, input, false)
			if err != nil {
				return nil, err
			}
			if applied {
				applyTranscodeConditions(&info, codecProfile.Conditions, "", true, true)
			}
		}
		info.TranscodeReasons = reasons | failure
		info.GlobalMaxAudioChannels = options.MaxAudioChannels
		configured := options.MaxBitrateFor(true)
		var bitrate int64
		bitrateSet := false
		if options.AudioTranscodingBitrate != nil {
			bitrate, bitrateSet = int64(*options.AudioTranscodingBitrate), true
		} else if options.Context == ContextStreaming && options.Profile.MusicStreamingTranscodingBitrate != nil {
			bitrate, bitrateSet = int64(*options.Profile.MusicStreamingTranscodingBitrate), true
		} else if configured != nil {
			bitrate, bitrateSet = int64(*configured), true
		}
		if !bitrateSet {
			bitrate = 128000
		}
		if configured != nil && bitrate > int64(*configured) {
			bitrate = int64(*configured)
		}
		if bitrate > int64(^uint32(0)>>1) {
			bitrate = int64(^uint32(0) >> 1)
		}
		if info.AudioBitrate != nil && int64(*info.AudioBitrate) < bitrate {
			bitrate = int64(*info.AudioBitrate)
		}
		info.AudioBitrate = streamPtr(int(bitrate))
		if len(info.AudioCodecs) == 0 && profile.AudioCodec != "" {
			info.AudioCodecs = []string{profile.AudioCodec}
		}
		return &info, nil
	}
	info.TranscodeReasons = reasons
	return &info, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func audioContainerSupported(profile *DirectPlayProfile, source *MediaSourceInfo) bool {
	return profile.SupportsContainer(source.Container) && (!ContainsContainer("mkv", source.Container) || profile.SupportsContainer("mkv"))
}

func (b *StreamBuilder) audioDirectProfile(source *MediaSourceInfo, audio *MediaStream, options *MediaOptions) (*DirectPlayProfile, *PlayMethod, TranscodeReason, error) {
	var profile *DirectPlayProfile
	for index := range options.Profile.DirectPlayProfiles {
		candidate := &options.Profile.DirectPlayProfiles[index]
		if candidate.Type == DlnaAudio && audioContainerSupported(candidate, source) && candidate.SupportsAudioCodec(audio.Codec) {
			profile = candidate
			break
		}
	}
	reasons := TranscodeReason(0)
	if profile == nil {
		for index := range options.Profile.DirectPlayProfiles {
			candidate := &options.Profile.DirectPlayProfiles[index]
			if candidate.Type == DlnaAudio && !audioContainerSupported(candidate, source) && (strings.EqualFold(candidate.AudioCodec, audio.Codec) || strings.EqualFold(candidate.Container, audio.Codec)) {
				profile = candidate
				reasons |= ReasonContainerNotSupported
				break
			}
		}
		if profile == nil {
			return nil, nil, b.directProfileReasons(source, nil, audio, options.Profile.DirectPlayProfiles), nil
		}
	}
	if source.SupportsDirectPlay && reasons == 0 {
		if !b.bitrateExceeded(source, maxBitrate(options, true)) && options.EnableDirectPlay {
			method := PlayMethodDirectPlay
			return profile, &method, 0, nil
		}
		if b.bitrateExceeded(source, maxBitrate(options, true)) {
			reasons |= ReasonContainerBitrateExceedsLimit
		}
	}
	if source.SupportsDirectStream {
		if !b.bitrateExceeded(source, maxBitrate(options, true)) && reasons == ReasonContainerNotSupported {
			method := PlayMethodDirectStream
			return profile, &method, reasons, nil
		}
		if b.bitrateExceeded(source, maxBitrate(options, true)) {
			reasons |= ReasonContainerBitrateExceedsLimit
		}
	}
	return profile, nil, reasons, nil
}

func (b *StreamBuilder) directProfileReasons(source *MediaSourceInfo, video, audio *MediaStream, profiles []DirectPlayProfile) TranscodeReason {
	kind := DlnaAudio
	if video != nil {
		kind = DlnaVideo
	}
	container, videoOK, audioOK := false, false, false
	for _, profile := range profiles {
		if profile.Type != kind || !profile.SupportsContainer(source.Container) {
			continue
		}
		container = true
		videoOK = video == nil || profile.SupportsVideoCodec(video.Codec)
		audioOK = audio == nil || profile.SupportsAudioCodec(audio.Codec)
		if videoOK && audioOK {
			break
		}
	}
	reasons := TranscodeReason(0)
	if !container {
		reasons |= ReasonContainerNotSupported
	}
	if !videoOK {
		reasons |= ReasonVideoCodecNotSupported
	}
	if !audioOK {
		reasons |= ReasonAudioCodecNotSupported
	}
	return reasons
}

func defaultSubtitleIndex(source *MediaSourceInfo, profiles []SubtitleProfile) *int {
	highest := -1
	for i := range source.MediaStreams {
		stream := &source.MediaStreams[i]
		if stream.Type == StreamTypeSubtitle && stream.Score != nil && *stream.Score > highest {
			highest = *stream.Score
		}
	}
	if highest >= 0 {
		var tied []*MediaStream
		for i := range source.MediaStreams {
			stream := &source.MediaStreams[i]
			if stream.Type == StreamTypeSubtitle && stream.Score != nil && *stream.Score == highest {
				tied = append(tied, stream)
			}
		}
		if len(tied) > 1 {
			for _, stream := range tied {
				for _, profile := range profiles {
					if profile.Method == SubtitleExternal && (isVobSubMKSProfile(profile, stream) || (!isVobSubMKSDeliveryProfile(profile) && strings.EqualFold(profile.Format, stream.Codec))) {
						return streamPtr(stream.Index)
					}
				}
			}
		}
	}
	return source.DefaultSubtitleStreamIndex
}

func (b *StreamBuilder) containerCompatibility(options *MediaOptions, source *MediaSourceInfo, container string, video *MediaStream) (TranscodeReason, error) {
	var reasons TranscodeReason
	for _, profile := range options.Profile.ContainerProfiles {
		if profile.Type != DlnaVideo || !profile.ContainsContainer(container, false) {
			continue
		}
		failures, err := failedVideoConditions(profile.Conditions, source, video)
		if err != nil {
			return 0, err
		}
		reasons |= reasonsForConditions(failures)
	}
	return reasons, nil
}

func (b *StreamBuilder) videoCompatibility(options *MediaOptions, source *MediaSourceInfo, container string, video *MediaStream) (TranscodeReason, error) {
	var reasons TranscodeReason
	for _, profile := range options.Profile.CodecProfiles {
		if profile.Type != CodecVideo || !profile.ContainsAnyCodec(video.Codec, container, false) {
			continue
		}
		applied, err := allVideoConditions(profile.ApplyConditions, source, video)
		if err != nil {
			return 0, err
		}
		if !applied {
			continue
		}
		failures, err := failedVideoConditions(profile.Conditions, source, video)
		if err != nil {
			return 0, err
		}
		reasons |= reasonsForConditions(failures)
	}
	return reasons, nil
}

func (b *StreamBuilder) audioDirectCompatibility(options *MediaOptions, source *MediaSourceInfo, container string, audio *MediaStream, video bool) (TranscodeReason, error) {
	isSecondary := false
	if secondary := source.IsSecondaryAudio(audio); secondary != nil {
		isSecondary = *secondary
	}
	reasons, err := b.audioCompatibility(options, source, container, audio, "", video, isSecondary)
	if err != nil {
		return 0, err
	}
	if audio.IsExternal {
		reasons |= ReasonAudioIsExternal
	}
	return reasons, nil
}

func (b *StreamBuilder) audioCompatibility(options *MediaOptions, source *MediaSourceInfo, container string, audio *MediaStream, codec string, video bool, isSecondary bool) (TranscodeReason, error) {
	if codec == "" {
		codec = audio.Codec
	}
	kind := CodecAudio
	if video {
		kind = CodecVideoAudio
	}
	in := audioInput(source, audio)
	in.IsSecondaryTrack = streamPtr(isSecondary)
	var reasons TranscodeReason
	for _, profile := range options.Profile.CodecProfiles {
		if profile.Type != kind || !profile.ContainsAnyCodec(codec, container, false) {
			continue
		}
		applied, err := allAudioConditions(profile.ApplyConditions, in, video)
		if err != nil {
			return 0, err
		}
		if !applied {
			continue
		}
		for _, condition := range profile.Conditions {
			ok, err := audioCondition(condition, in, video)
			if err != nil {
				return 0, err
			}
			if !ok {
				reasons |= reasonForCondition(condition)
			}
		}
	}
	return reasons, nil
}

func audioInput(source *MediaSourceInfo, audio *MediaStream) VideoAudioConditionInput {
	return VideoAudioConditionInput{AudioChannels: audio.Channels, AudioBitrate: audio.BitRate, AudioSampleRate: audio.SampleRate, AudioBitDepth: audio.BitDepth, AudioProfile: audio.Profile, IsSecondaryTrack: source.IsSecondaryAudio(audio)}
}

func allAudioConditions(conditions []ProfileCondition, in VideoAudioConditionInput, video bool) (bool, error) {
	for _, condition := range conditions {
		ok, err := audioCondition(condition, in, video)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func audioCondition(condition ProfileCondition, in VideoAudioConditionInput, video bool) (bool, error) {
	if video {
		return IsVideoAudioConditionSatisfied(condition, in)
	}
	return IsAudioConditionSatisfied(condition, in.AudioChannels, in.AudioBitrate, in.AudioSampleRate, in.AudioBitDepth)
}

func failedVideoConditions(conditions []ProfileCondition, source *MediaSourceInfo, video *MediaStream) ([]ProfileCondition, error) {
	var failed []ProfileCondition
	for _, condition := range conditions {
		ok, err := videoCondition(condition, source, video)
		if err != nil {
			return nil, err
		}
		if !ok {
			failed = append(failed, condition)
		}
	}
	return failed, nil
}

func allVideoConditions(conditions []ProfileCondition, source *MediaSourceInfo, video *MediaStream) (bool, error) {
	failed, err := failedVideoConditions(conditions, source, video)
	return len(failed) == 0, err
}

func reasonsForConditions(conditions []ProfileCondition) TranscodeReason {
	var reasons TranscodeReason
	for _, condition := range conditions {
		reasons |= reasonForCondition(condition)
	}
	return reasons
}

func videoCondition(condition ProfileCondition, source *MediaSourceInfo, video *MediaStream) (bool, error) {
	in := VideoConditionInput{NumStreams: len(source.MediaStreams), NumAudioStreams: source.StreamCount(StreamTypeAudio), NumVideoStreams: source.StreamCount(StreamTypeVideo), Timestamp: source.Timestamp}
	if video != nil {
		in.Width = video.Width
		in.Height = video.Height
		in.VideoBitDepth = video.BitDepth
		in.VideoBitrate = video.BitRate
		in.VideoProfile = video.Profile
		rangeType := video.VideoRangeType()
		in.VideoRangeType = &rangeType
		in.VideoLevel = video.Level
		in.VideoFramerate = video.ReferenceFrameRate()
		in.PacketLength = video.PacketLength
		in.IsAnamorphic = video.IsAnamorphic
		in.IsInterlaced = streamPtr(video.IsInterlaced)
		in.RefFrames = video.RefFrames
		in.VideoCodecTag = video.CodecTag
		in.IsAvc = video.IsAVC
		in.VideoRotation = video.Rotation
	}
	return IsVideoConditionSatisfied(condition, in)
}

func reasonForCondition(condition ProfileCondition) TranscodeReason {
	switch condition.Property {
	case PropAudioBitrate:
		return ReasonAudioBitrateNotSupported
	case PropAudioChannels:
		return ReasonAudioChannelsNotSupported
	case PropAudioProfile:
		return ReasonAudioProfileNotSupported
	case PropAudioSampleRate:
		return ReasonAudioSampleRateNotSupported
	case PropAudioBitDepth:
		return ReasonAudioBitDepthNotSupported
	case PropHeight, PropWidth:
		return ReasonVideoResolutionNotSupported
	case PropIsAnamorphic:
		return ReasonAnamorphicVideoNotSupported
	case PropIsInterlaced:
		return ReasonInterlacedVideoNotSupported
	case PropIsSecondaryAudio:
		return ReasonSecondaryAudioNotSupported
	case PropNumStreams:
		return ReasonStreamCountExceedsLimit
	case PropRefFrames:
		return ReasonRefFramesNotSupported
	case PropVideoBitDepth:
		return ReasonVideoBitDepthNotSupported
	case PropVideoBitrate:
		return ReasonVideoBitrateNotSupported
	case PropVideoCodecTag:
		return ReasonVideoCodecTagNotSupported
	case PropVideoFramerate:
		return ReasonVideoFramerateNotSupported
	case PropVideoLevel:
		return ReasonVideoLevelNotSupported
	case PropVideoProfile:
		return ReasonVideoProfileNotSupported
	case PropVideoRangeType:
		return ReasonVideoRangeTypeNotSupported
	case PropVideoRotation:
		return ReasonVideoRotationNotSupported
	}
	return 0
}

func applyVideoConditions(info *StreamInfo, options *MediaOptions, source *MediaSourceInfo, video *MediaStream, container string) {
	codecs := info.VideoCodecs
	if len(codecs) == 0 && video != nil {
		codecs = []string{video.Codec}
	}
	for index := len(options.Profile.CodecProfiles) - 1; index >= 0; index-- {
		profile := &options.Profile.CodecProfiles[index]
		if profile.Type != CodecVideo || !profile.ContainsAnyCodecs(codecs, container, info.SubProtocol == StreamProtocolHLS) {
			continue
		}
		ok, _ := allVideoConditions(profile.ApplyConditions, source, video)
		if !ok {
			continue
		}
		for _, codec := range codecs {
			if profile.ContainsAnyCodec(codec, container, info.SubProtocol == StreamProtocolHLS) {
				applyTranscodeConditions(info, profile.Conditions, codec, true, true)
			}
		}
	}
}

func applyAudioConditions(info *StreamInfo, options *MediaOptions, source *MediaSourceInfo, audio *MediaStream, container string) {
	var in VideoAudioConditionInput
	if audio != nil {
		in = audioInput(source, audio)
	}
	for index := len(options.Profile.CodecProfiles) - 1; index >= 0; index-- {
		profile := &options.Profile.CodecProfiles[index]
		if profile.Type != CodecVideoAudio || !profile.ContainsAnyCodecs(info.AudioCodecs, container, false) {
			continue
		}
		ok, _ := allAudioConditions(profile.ApplyConditions, in, true)
		if !ok {
			continue
		}
		for _, codec := range info.AudioCodecs {
			if profile.ContainsAnyCodec(codec, container, false) {
				applyTranscodeConditions(info, profile.Conditions, codec, true, true)
				break
			}
		}
	}
}

func applyTranscodeConditions(info *StreamInfo, conditions []ProfileCondition, qualifier string, qualified, nonQualified bool) {
	for _, condition := range conditions {
		value := condition.Value
		if value == "" {
			continue
		}
		optionAllowed := func() bool { return (qualifier == "" && nonQualified) || (qualifier != "" && qualified) }
		setIntOption := func(name string) {
			if !optionAllowed() {
				return
			}
			if condition.Condition != ConditionEquals && condition.Condition != ConditionLessThanEqual && condition.Condition != ConditionGreaterThanEqual {
				return
			}
			n, err := strconv.Atoi(value)
			if err != nil {
				return
			}
			if name == "level" {
				level := float64(n)
				if current := info.GetTargetVideoLevel(qualifier); current != nil {
					if condition.Condition == ConditionLessThanEqual && *current < level || condition.Condition == ConditionGreaterThanEqual && *current > level {
						level = *current
					}
				}
				info.SetOption(qualifier, name, strconv.FormatFloat(level, 'f', -1, 64))
				return
			}
			var current *int
			switch name {
			case "audiochannels":
				current = info.targetAudioChannels(qualifier)
			case "maxrefframes":
				current = info.GetTargetRefFrames(qualifier)
			case "videobitdepth":
				current = info.GetTargetVideoBitDepth(qualifier)
			case "level":
				if level := info.GetTargetVideoLevel(qualifier); level != nil {
					value := int(*level)
					current = &value
				}
			}
			if condition.Condition == ConditionLessThanEqual && current != nil && *current < n {
				n = *current
			} else if condition.Condition == ConditionGreaterThanEqual && current != nil && *current > n {
				n = *current
			}
			info.SetOption(qualifier, name, strconv.Itoa(n))
		}
		switch condition.Property {
		case PropAudioBitrate:
			if nonQualified {
				if n, e := strconv.Atoi(value); e == nil {
					setLimitedInt(&info.AudioBitrate, n, condition.Condition)
				}
			}
		case PropAudioSampleRate:
			if nonQualified {
				if n, e := strconv.Atoi(value); e == nil {
					setLimitedInt(&info.AudioSampleRate, n, condition.Condition)
				}
			}
		case PropAudioChannels:
			setIntOption("audiochannels")
		case PropIsAvc:
			if nonQualified {
				if b, e := strconv.ParseBool(value); e == nil && ((b && condition.Condition == ConditionEquals) || (!b && condition.Condition == ConditionNotEquals)) {
					info.RequireAvc = true
				}
			}
		case PropIsAnamorphic:
			if nonQualified {
				if b, e := strconv.ParseBool(value); e == nil && ((b && condition.Condition == ConditionEquals) || (!b && condition.Condition == ConditionNotEquals)) {
					info.RequireNonAnamorphic = true
				}
			}
		case PropIsInterlaced:
			if optionAllowed() {
				if b, e := strconv.ParseBool(value); e == nil && ((!b && condition.Condition == ConditionEquals) || (b && condition.Condition == ConditionNotEquals)) {
					info.SetOption(qualifier, "deinterlace", "true")
				}
			}
		case PropRefFrames:
			setIntOption("maxrefframes")
		case PropVideoBitDepth:
			setIntOption("videobitdepth")
		case PropVideoProfile:
			if qualifier != "" && (condition.Condition == ConditionEquals || condition.Condition == ConditionEqualsAny) {
				values := strings.FieldsFunc(value, func(r rune) bool { return r == '|' })
				candidate := strings.Join(values, ",")
				if condition.Condition == ConditionEqualsAny {
					current := info.GetOption(qualifier, "profile")
					for _, v := range values {
						if v == current {
							candidate = current
							break
						}
					}
				}
				info.SetOption(qualifier, "profile", candidate)
			}
		case PropVideoRangeType, PropVideoCodecTag, PropVideoRotation:
			if qualifier != "" {
				name := map[ConditionProperty]string{PropVideoRangeType: "rangetype", PropVideoCodecTag: "codectag", PropVideoRotation: "rotation"}[condition.Property]
				values := strings.FieldsFunc(value, func(r rune) bool { return r == '|' })
				for index := range values {
					values[index] = strings.TrimSpace(values[index])
				}
				if condition.Property == PropVideoRangeType && condition.Condition == ConditionNotEquals {
					values = []string{"Unknown", "SDR", "HDR10", "HLG", "DOVI", "DOVIWithHDR10", "DOVIWithHLG", "DOVIWithSDR", "DOVIWithEL", "DOVIWithHDR10Plus", "DOVIWithELHDR10Plus", "DOVIInvalid", "HDR10Plus"}
					excluded := strings.FieldsFunc(value, func(r rune) bool { return r == '|' })
					for index := range excluded {
						excluded[index] = strings.TrimSpace(excluded[index])
					}
					out := values[:0]
					for _, v := range values {
						found := false
						for _, x := range excluded {
							if v == x {
								found = true
							}
						}
						if !found {
							out = append(out, v)
						}
					}
					values = out
				}
				if condition.Condition == ConditionEquals || (condition.Property == PropVideoRangeType && condition.Condition == ConditionNotEquals) {
					info.SetOption(qualifier, name, strings.Join(values, ","))
				} else if condition.Condition == ConditionEqualsAny {
					current := info.GetOption(qualifier, name)
					for _, v := range values {
						if strings.EqualFold(v, current) {
							values = []string{current}
							break
						}
					}
					info.SetOption(qualifier, name, strings.Join(values, ","))
				}
			}
		case PropHeight:
			if nonQualified {
				if n, e := strconv.Atoi(value); e == nil {
					setLimitedInt(&info.MaxHeight, n, condition.Condition)
				}
			}
		case PropWidth:
			if nonQualified {
				if n, e := strconv.Atoi(value); e == nil {
					setLimitedInt(&info.MaxWidth, n, condition.Condition)
				}
			}
		case PropVideoBitrate:
			if nonQualified {
				if n, e := strconv.Atoi(value); e == nil {
					setLimitedInt(&info.VideoBitrate, n, condition.Condition)
				}
			}
		case PropVideoFramerate:
			if nonQualified {
				if n, e := strconv.ParseFloat(value, 32); e == nil {
					rate := float32(n)
					if info.MaxFramerate == nil || condition.Condition == ConditionEquals || (condition.Condition == ConditionLessThanEqual && *info.MaxFramerate > rate) || (condition.Condition == ConditionGreaterThanEqual && *info.MaxFramerate < rate) {
						info.MaxFramerate = &rate
					}
				}
			}
		case PropVideoLevel:
			setIntOption("level")
		}
	}
}

func setLimitedInt(target **int, value int, condition ConditionType) {
	if condition != ConditionEquals && condition != ConditionLessThanEqual && condition != ConditionGreaterThanEqual {
		return
	}
	if condition == ConditionEquals {
		*target = streamPtr(value)
		return
	}
	if *target == nil {
		*target = streamPtr(value)
		return
	}
	if condition == ConditionLessThanEqual && **target > value || condition == ConditionGreaterThanEqual && **target < value {
		*target = streamPtr(value)
	}
}

func GetSubtitleProfile(source *MediaSourceInfo, subtitle *MediaStream, profiles []SubtitleProfile, method PlayMethod, support TranscoderSupport, container string, protocol MediaStreamProtocol) SubtitleProfile {
	if canConsiderEmbedSubtitle(subtitle, method, protocol, container) {
		for _, conversion := range []bool{false, true} {
			for _, profile := range profiles {
				if profile.Method != SubtitleEmbed || !profile.SupportsLanguage(stringValue(subtitle.Language)) || !ContainsContainer(profile.Container, container) || (method == PlayMethodTranscode && !isSubtitleEmbedSupported(container)) {
					continue
				}
				if !conversion && subtitle.IsTextSubtitleStream() == IsTextSubtitleFormat(profile.Format) && strings.EqualFold(profile.Format, subtitle.Codec) {
					return profile
				}
				if conversion && subtitle.IsTextSubtitleStream() && subtitle.SupportsSubtitleConversionTo(profile.Format) {
					return profile
				}
			}
		}
	}
	for _, conversion := range []bool{false, true} {
		for _, profile := range profiles {
			if profile.Method != SubtitleExternal && profile.Method != SubtitleHLS {
				continue
			}
			if profile.Method == SubtitleHLS && method != PlayMethodTranscode {
				continue
			}
			if !profile.SupportsLanguage(stringValue(subtitle.Language)) {
				continue
			}
			if !subtitle.IsExternal && method == PlayMethodTranscode && !support.CanExtractSubtitles(subtitle.Codec) {
				continue
			}
			vobSubMKS := isVobSubMKSProfile(profile, subtitle)
			matches := profile.Method == SubtitleExternal && (vobSubMKS || (!isVobSubMKSDeliveryProfile(profile) && subtitle.IsTextSubtitleStream() == IsTextSubtitleFormat(profile.Format))) || profile.Method == SubtitleHLS && subtitle.IsTextSubtitleStream()
			if !matches {
				continue
			}
			requiresConversion := !vobSubMKS && !strings.EqualFold(subtitle.Codec, profile.Format)
			if !requiresConversion {
				return profile
			}
			if conversion && !source.IsInfiniteStream && subtitle.IsTextSubtitleStream() && subtitle.SupportsExternalStream && subtitle.SupportsSubtitleConversionTo(profile.Format) {
				return profile
			}
		}
	}
	return SubtitleProfile{Method: SubtitleEncode, Format: subtitle.Codec}
}

func canConsiderEmbedSubtitle(subtitle *MediaStream, method PlayMethod, protocol MediaStreamProtocol, container string) bool {
	if subtitle.IsExternal {
		return method == PlayMethodTranscode && protocol != StreamProtocolHLS && isSubtitleEmbedSupported(container)
	}
	return method != PlayMethodTranscode || protocol != StreamProtocolHLS
}

func isSubtitleEmbedSupported(container string) bool {
	return !ContainsContainer("ts,mpegts,mp4", container) && ContainsContainer("mkv,matroska", container)
}

func isVobSubMKSDeliveryProfile(profile SubtitleProfile) bool {
	return IsVobSubSubtitleFormat(profile.Format) && strings.TrimSpace(profile.Container) != "" && ContainsContainer(profile.Container, "mks")
}

func isVobSubMKSProfile(profile SubtitleProfile, subtitle *MediaStream) bool {
	return isVobSubMKSDeliveryProfile(profile) && subtitle.IsVobSubSubtitleStream() && (!subtitle.IsExternal || strings.HasSuffix(strings.ToLower(subtitle.Path), ".mks"))
}
