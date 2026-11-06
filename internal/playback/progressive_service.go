// SPDX-License-Identifier: GPL-2.0-only

package playback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/images"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

var (
	// ErrProgressiveInvalid identifies a malformed progressive request.
	ErrProgressiveInvalid = errors.New("invalid progressive request")
	// ErrProgressiveBackend identifies an incomplete or invalid progressive backend response.
	ErrProgressiveBackend = errors.New("progressive backend failure")
	// ErrProgressiveUnsupported identifies a valid request that Deadfin cannot copy or transcode.
	ErrProgressiveUnsupported = errors.New("unsupported progressive stream copy")
)

// ProgressiveProcessOpener starts the single process that produces a progressive response.
// Process ownership stays with the caller, which decides how the returned body is served and closed.
type ProgressiveProcessOpener interface {
	Open(context.Context, ProgressiveOpenRequest) (io.ReadCloser, error)
}

// ProgressiveVideoStreamer is the handler-facing progressive video seam.
type ProgressiveVideoStreamer interface {
	OpenProgressive(context.Context, ProgressiveRequest) (ProgressiveResponse, error)
}

// ProgressiveOpenRequest is the process-facing portion of a progressive request.
type ProgressiveOpenRequest struct {
	OutputPath     string
	Args           []string
	PlaySessionID  string
	DeviceID       string
	SourceRevision string
	OpenSource     func() (*sourcefile.Source, error)
}

// ProgressiveServiceConfig supplies the existing source selector and the caller-owned process seam.
type ProgressiveServiceConfig struct {
	Sources      DirectFileSourceService
	TranscodeDir string
	Opener       ProgressiveProcessOpener
	// Capabilities returns the encoder capabilities for a new audio
	// transcode. Nil selects no encoders.
	Capabilities func() encoding.Capabilities
	// AudioVBR returns EncodingOptions.EnableAudioVbr. Nil disables VBR.
	AudioVBR func() bool
}

// ProgressiveRequest contains the transport facts needed to determine whether a local video can be remuxed.
type ProgressiveRequest struct {
	UserID        dto.UUID
	ItemID        dto.UUID
	MediaSourceID string
	Container     string

	DeviceID      string
	PlaySessionID string
	UserAgent     string

	VideoCodecs []string
	AudioCodecs []string
	VideoCodec  string
	AudioCodec  string

	VideoStreamIndex            *int
	AudioStreamIndex            *int
	SubtitleStreamIndex         *int
	SubtitleDeliveryMethod      *SubtitleDeliveryMethod
	StartTimeTicks              int64
	AllowVideoStreamCopy        bool
	AllowAudioStreamCopy        bool
	EnableAutoStreamCopy        bool
	RequireAvc                  bool
	RequireNonAnamorphic        bool
	DeInterlace                 bool
	Width                       *int
	Height                      *int
	MaxWidth                    *int
	MaxHeight                   *int
	MaxFramerate                *float32
	Framerate                   *float32
	VideoBitrate                *int
	Profile                     *string
	Level                       *string
	MaxRefFrames                *int
	MaxVideoBitDepth            *int
	Rotation                    *int
	AudioBitrate                *int
	MaxAudioChannels            *int
	AudioChannels               *int
	TranscodingMaxAudioChannels *int
	AudioSampleRate             *int
	MaxAudioBitDepth            *int
	StreamOptions               StreamOptions
	CopyTimestamps              bool
	Head                        bool
}

// ProgressiveResponse describes the generated progressive stream. Body is nil for HEAD.
type ProgressiveResponse struct {
	Body        io.ReadCloser
	ContentType string
	OutputPath  string
}

// ProgressiveService is the focused local-file progressive stream application seam.
type ProgressiveService struct {
	sources      DirectFileSourceService
	transcodeDir string
	opener       ProgressiveProcessOpener
	capabilities func() encoding.Capabilities
	audioVbr     func() bool
}

// NewProgressiveService creates the focused progressive stream service.
func NewProgressiveService(config ProgressiveServiceConfig) *ProgressiveService {
	return &ProgressiveService{
		sources:      config.Sources,
		transcodeDir: config.TranscodeDir,
		opener:       config.Opener,
		capabilities: config.Capabilities,
		audioVbr:     config.AudioVBR,
	}
}

func (service *ProgressiveService) currentCapabilities() encoding.Capabilities {
	if service.capabilities == nil {
		return encoding.Capabilities{}
	}
	return service.capabilities()
}

// NewProgressiveVideoService creates the handler-facing progressive video service.
func NewProgressiveVideoService(config ProgressiveServiceConfig) *ProgressiveService {
	return NewProgressiveService(config)
}

// OpenProgressive ports the progressive branch of VideosController.GetVideoStream
// for a local source: it copies or transcodes each stream, builds its pure
// encoding job, and opens one process.
func (service *ProgressiveService) OpenProgressive(ctx context.Context, request ProgressiveRequest) (ProgressiveResponse, error) {
	if err := validateProgressiveRequest(request); err != nil {
		return ProgressiveResponse{}, err
	}
	if service == nil || service.sources == nil || service.transcodeDir == "" {
		return ProgressiveResponse{}, fmt.Errorf("%w: incomplete progressive service configuration", ErrProgressiveBackend)
	}

	source, err := service.sources.StaticSource(ctx, request.UserID, request.ItemID, request.MediaSourceID)
	if err != nil {
		return ProgressiveResponse{}, err
	}
	video, audio, err := progressiveSourceStreams(source, request)
	if err != nil {
		return ProgressiveResponse{}, err
	}
	caps := service.currentCapabilities()
	copyVideo := canCopyVideo(source, video, request, caps)
	copyAudio := canCopyAudio(audio, request)
	if err := validateProgressiveTranscode(source, video, request, copyVideo, copyAudio, caps); err != nil {
		return ProgressiveResponse{}, err
	}
	if !hasCanonicalSourceAuthority(source) {
		return ProgressiveResponse{}, fmt.Errorf("%w: media source has incomplete canonical file authority", ErrProgressiveUnsupported)
	}

	outputContainer, extension, contentType := progressiveOutput(request.Container)
	job := progressiveEncodingJob(source, video, audio, outputContainer, progressiveRenditionIdentityOutputPath, request)
	if !copyVideo {
		progressiveVideoTranscode(&job, source, video, request)
	}
	if !copyAudio {
		progressiveAudioTranscode(&job, audio, request)
	}
	identityArgs, err := encoding.BuildProgressiveArgs(job, caps)
	if err != nil {
		return ProgressiveResponse{}, fmt.Errorf("%w: build progressive family identity: %v", ErrProgressiveUnsupported, err)
	}
	outputPath := progressiveOutputPath(service.transcodeDir, request.UserAgent, request.DeviceID, request.PlaySessionID, source.SourceRevision, identityArgs, extension)
	job.OutputPath = outputPath
	args, err := encoding.BuildProgressiveArgs(job, caps)
	if err != nil {
		return ProgressiveResponse{}, fmt.Errorf("%w: build progressive job: %v", ErrProgressiveUnsupported, err)
	}

	response := ProgressiveResponse{ContentType: contentType, OutputPath: outputPath}
	if request.Head {
		return response, nil
	}
	return service.openProcess(ctx, response, source, args, request.PlaySessionID, request.DeviceID)
}

// openProcess opens the one ffmpeg process that writes response.OutputPath
// and attaches its growing output as the response body.
func (service *ProgressiveService) openProcess(ctx context.Context, response ProgressiveResponse, source MediaSourceInfo, args []string, playSessionID, deviceID string) (ProgressiveResponse, error) {
	if service.opener == nil {
		return ProgressiveResponse{}, fmt.Errorf("%w: incomplete progressive service configuration", ErrProgressiveBackend)
	}
	body, err := service.opener.Open(ctx, ProgressiveOpenRequest{
		OutputPath:     response.OutputPath,
		Args:           args,
		PlaySessionID:  playSessionID,
		DeviceID:       deviceID,
		SourceRevision: source.SourceRevision,
		OpenSource: func() (*sourcefile.Source, error) {
			facts := source.FileFacts
			return sourcefile.Open(source.Path, sourcefile.Expected{
				Size: facts.Size, ModifiedAt: facts.ModifiedAt, Inode: facts.Inode, ChangedAt: facts.ChangedAt,
			})
		},
	})
	if err != nil {
		return ProgressiveResponse{}, err
	}
	if body == nil {
		return ProgressiveResponse{}, fmt.Errorf("%w: process opener returned nil body", ErrProgressiveBackend)
	}
	response.Body = body
	return response, nil
}

func validateProgressiveRequest(request ProgressiveRequest) error {
	if request.ItemID == dto.NilUUID {
		return fmt.Errorf("%w: item ID is required", ErrProgressiveInvalid)
	}
	if request.PlaySessionID == "" {
		return fmt.Errorf("%w: play session ID is required", ErrProgressiveInvalid)
	}
	if request.StartTimeTicks < 0 {
		return fmt.Errorf("%w: start ticks must not be negative", ErrProgressiveInvalid)
	}
	if request.VideoStreamIndex != nil && *request.VideoStreamIndex < -1 {
		return fmt.Errorf("%w: invalid video stream index", ErrProgressiveInvalid)
	}
	if request.AudioStreamIndex != nil && *request.AudioStreamIndex < -1 {
		return fmt.Errorf("%w: invalid audio stream index", ErrProgressiveInvalid)
	}
	if request.SubtitleStreamIndex != nil && *request.SubtitleStreamIndex < -1 {
		return fmt.Errorf("%w: invalid subtitle stream index", ErrProgressiveInvalid)
	}
	if request.Container != "ts" && request.Container != "fmp4" && request.Container != "mp4" && request.Container != "mkv" {
		return fmt.Errorf("%w: output container %q", ErrProgressiveInvalid, request.Container)
	}
	return nil
}

func progressiveSourceStreams(source MediaSourceInfo, request ProgressiveRequest) (*MediaStream, *MediaStream, error) {
	if !safeProgressiveSourcePath(source.Path) {
		return nil, nil, fmt.Errorf("%w: source path is not a safe local path", ErrProgressiveInvalid)
	}
	if source.Protocol != MediaProtocolFile || source.IsRemote || source.IsInfiniteStream || source.LiveStreamID != "" || progressiveDiscSource(source) {
		return nil, nil, ErrProgressiveUnsupported
	}
	var video *MediaStream
	if request.VideoStreamIndex != nil && *request.VideoStreamIndex != -1 {
		video = source.MediaStream(StreamTypeVideo, *request.VideoStreamIndex)
	} else {
		video = source.VideoStream()
	}
	if video == nil || video.IsExternal {
		return nil, nil, ErrProgressiveUnsupported
	}
	var audio *MediaStream
	if request.AudioStreamIndex != nil && *request.AudioStreamIndex != -1 {
		audio = source.MediaStream(StreamTypeAudio, *request.AudioStreamIndex)
	} else {
		audio = source.DefaultAudioStream(nil)
	}
	if audio == nil || audio.IsExternal {
		return nil, nil, ErrProgressiveUnsupported
	}
	return video, audio, nil
}

func safeProgressiveSourcePath(path string) bool {
	if path == "" || path[0] == '-' {
		return false
	}
	for _, runeValue := range path {
		if runeValue < 0x20 || runeValue == 0x7f {
			return false
		}
	}
	return true
}

func progressiveDiscSource(source MediaSourceInfo) bool {
	if source.VideoType == nil {
		return false
	}
	return *source.VideoType == VideoTypeIso || *source.VideoType == VideoTypeDVD || *source.VideoType == VideoTypeBluRay || *source.VideoType == VideoTypeHDVD
}

func canCopyVideo(source MediaSourceInfo, video *MediaStream, request ProgressiveRequest, caps encoding.Capabilities) bool {
	if video == nil || !request.AllowVideoStreamCopy || !strings.EqualFold(video.Codec, "h264") || !containsProgressiveCodec(request.VideoCodecs, video.Codec) {
		return false
	}
	return canCopyVideoForCodecWithLevel(source, video, request, "h264", progressiveH264VideoLevel(request, "h264"), caps)
}

// canCopyVideoForCodec applies video stream-copy constraints whose stream
// options are qualified by the actual source codec. Callers retain ownership
// of codec/container eligibility before entering this shared predicate.
func canCopyVideoForCodec(source MediaSourceInfo, video *MediaStream, request ProgressiveRequest, qualifier string, caps encoding.Capabilities) bool {
	return canCopyVideoForCodecWithLevel(source, video, request, qualifier, progressiveVideoLevel(request, qualifier), caps)
}

func canCopyVideoForCodecWithLevel(source MediaSourceInfo, video *MediaStream, request ProgressiveRequest, qualifier string, requestedLevel *float64, caps encoding.Capabilities) bool {
	if video == nil {
		return false
	}
	if video.IsInterlaced && (request.DeInterlace || progressiveDeinterlaceRequested(request, qualifier)) {
		return false
	}
	if request.RequireNonAnamorphic && video.IsAnamorphic != nil && *video.IsAnamorphic {
		return false
	}
	if progressiveSubtitleEncodeRequested(request) {
		return false
	}
	if strings.EqualFold(video.Codec, "h264") && request.RequireAvc && video.IsAVC != nil && !*video.IsAVC {
		return false
	}
	if request.MaxWidth != nil && (video.Width == nil || *video.Width > *request.MaxWidth) {
		return false
	}
	if request.MaxHeight != nil && (video.Height == nil || *video.Height > *request.MaxHeight) {
		return false
	}
	if maximumFramerate := progressiveCopyMaxFramerate(request); maximumFramerate != nil {
		frameRate := video.ReferenceFrameRate()
		if frameRate == nil || *frameRate > *maximumFramerate+0.05 {
			return false
		}
	}
	if request.VideoBitrate != nil && (video.BitRate == nil || *video.BitRate > *request.VideoBitrate) {
		return false
	}
	if !progressiveProfileSupported(video.Profile, progressiveVideoProfile(request, qualifier)) || progressiveExceedsFloat(video.Level, requestedLevel) || progressiveExceedsInt(video.RefFrames, progressiveVideoRefFrames(request, qualifier)) || progressiveExceedsInt(video.BitDepth, progressiveVideoBitDepth(request, qualifier)) || !progressiveRangeTypeSupported(video, progressiveVideoRangeTypes(request, qualifier), caps) || !progressiveRotationSupported(video.Rotation, progressiveVideoRotation(request, qualifier)) {
		return false
	}
	if strings.EqualFold(source.Container, "avi") && strings.EqualFold(video.Codec, "h264") && (video.IsAVC == nil || !*video.IsAVC) {
		return false
	}
	return true
}

func canCopyAudio(audio *MediaStream, request ProgressiveRequest) bool {
	if !request.AllowAudioStreamCopy || !request.EnableAutoStreamCopy || !strings.EqualFold(audio.Codec, "ac3") || !containsProgressiveCodec(request.AudioCodecs, audio.Codec) {
		return false
	}
	if progressiveExceedsInt(audio.BitDepth, progressiveAudioBitDepth(request)) {
		return false
	}
	if maximum := progressiveAudioChannels(request); maximum != nil && (audio.Channels == nil || *audio.Channels <= 0 || *audio.Channels > *maximum) {
		return false
	}
	if request.AudioSampleRate != nil && (audio.SampleRate == nil || *audio.SampleRate <= 0 || *audio.SampleRate > *request.AudioSampleRate) {
		return false
	}
	return request.AudioBitrate == nil || audio.BitRate == nil || *audio.BitRate <= *request.AudioBitrate
}

func containsProgressiveCodec(codecs []string, codec string) bool {
	for _, codecsValue := range codecs {
		for candidate := range strings.SplitSeq(codecsValue, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), codec) {
				return true
			}
		}
	}
	return false
}

func progressiveCopyMaxFramerate(request ProgressiveRequest) *float32 {
	if request.MaxFramerate != nil {
		return request.MaxFramerate
	}
	return request.Framerate
}

func progressiveOutputFrameRate(video *MediaStream, request ProgressiveRequest) float64 {
	if request.Framerate != nil {
		return float64(*request.Framerate)
	}
	if request.MaxFramerate != nil {
		frameRate := video.ReferenceFrameRate()
		if frameRate != nil && *frameRate > *request.MaxFramerate {
			return float64(*request.MaxFramerate)
		}
	}
	return 0
}

func progressiveProfileSupported(source, requested *string) bool {
	if source == nil || *source == "" || requested == nil || *requested == "" {
		return true
	}
	sourceProfile := strings.ReplaceAll(*source, " ", "")
	requestedProfiles := strings.FieldsFunc(*requested, func(r rune) bool { return r == ',' || r == '|' })
	for _, candidate := range requestedProfiles {
		if strings.EqualFold(strings.ReplaceAll(candidate, " ", ""), sourceProfile) {
			return true
		}
	}
	if len(requestedProfiles) == 0 {
		return true
	}
	sourceScore := progressiveH264ProfileScore(sourceProfile)
	requestedScore := progressiveH264ProfileScore(strings.ReplaceAll(requestedProfiles[0], " ", ""))
	return sourceScore >= 0 && requestedScore >= 0 && sourceScore <= requestedScore
}

func progressiveH264ProfileScore(profile string) int {
	for score, candidate := range []string{"ConstrainedBaseline", "Baseline", "Extended", "Main", "High", "ProgressiveHigh", "ConstrainedHigh", "High10"} {
		if strings.EqualFold(profile, candidate) {
			return score
		}
	}
	return -1
}

func progressiveExceedsInt(source, maximum *int) bool {
	return source != nil && maximum != nil && *source > *maximum
}

func progressiveExceedsFloat(source, maximum *float64) bool {
	return source != nil && maximum != nil && *source > *maximum
}

func progressiveVideoProfile(request ProgressiveRequest, qualifier string) *string {
	if request.Profile != nil && *request.Profile != "" {
		return request.Profile
	}
	if value := progressiveVideoOption(request, qualifier, "profile"); value != "" {
		return &value
	}
	return nil
}

func progressiveVideoLevel(request ProgressiveRequest, qualifier string) *float64 {
	return parseOptionFloat(progressiveVideoLevelValue(request, qualifier))
}

func progressiveH264VideoLevel(request ProgressiveRequest, qualifier string) *float64 {
	level := parseOptionFloat(progressiveVideoLevelValue(request, qualifier))
	if level == nil || *level < 1 || *level >= 10 {
		return level
	}
	normalized := *level * 10
	return &normalized
}

func progressiveVideoLevelValue(request ProgressiveRequest, qualifier string) string {
	if request.Level != nil && *request.Level != "" {
		return *request.Level
	}
	return progressiveVideoOption(request, qualifier, "level")
}

func progressiveVideoRefFrames(request ProgressiveRequest, qualifier string) *int {
	if request.MaxRefFrames != nil {
		return request.MaxRefFrames
	}
	return parseOptionInt(progressiveVideoOption(request, qualifier, "maxrefframes"))
}

func progressiveVideoBitDepth(request ProgressiveRequest, qualifier string) *int {
	if request.MaxVideoBitDepth != nil {
		return request.MaxVideoBitDepth
	}
	return parseOptionInt(progressiveVideoOption(request, qualifier, "videobitdepth"))
}

func progressiveDeinterlaceRequested(request ProgressiveRequest, qualifier string) bool {
	return strings.EqualFold(progressiveVideoOption(request, qualifier, "deinterlace"), "true")
}

func progressiveVideoRangeTypes(request ProgressiveRequest, qualifier string) string {
	return progressiveVideoOption(request, qualifier, "rangetype")
}

func progressiveVideoOption(request ProgressiveRequest, qualifier, name string) string {
	if value := request.StreamOptions.Get(qualifier + "-" + name); value != "" {
		return value
	}
	return request.StreamOptions.Get(name)
}

// progressiveRangeTypeSupported ports the range-type part of
// EncodingHelper.CanStreamCopyVideo (EncodingHelper.cs:2470-2524). A Dolby
// Vision or HDR10+ source that the client range types do not name can still
// copy when ffmpeg removes the dynamic metadata that the client cannot use.
func progressiveRangeTypeSupported(video *MediaStream, requested string, caps encoding.Capabilities) bool {
	requestedTypes := splitRangeTypes(requested)
	if len(requestedTypes) == 0 {
		return true
	}
	source := video.VideoRangeType()
	if source == VideoRangeUnknown {
		return false
	}
	has := func(rangeType VideoRangeType) bool { return slices.Contains(requestedTypes, rangeType.String()) }
	requestHasHDR10, requestHasHLG, requestHasSDR, requestHasDOVI := has(VideoRangeHDR10), has(VideoRangeHLG), has(VideoRangeSDR), has(VideoRangeDOVI)
	if len(requestedTypes) == 1 && requestHasSDR && source != VideoRangeSDR {
		return false
	}
	if !requestHasDOVI && source == VideoRangeDOVI {
		return false
	}
	if has(source) ||
		(requestHasHDR10 && source == VideoRangeDOVIWithHDR10) ||
		(requestHasHLG && source == VideoRangeDOVIWithHLG) ||
		(requestHasSDR && source == VideoRangeDOVIWithSDR) ||
		(requestHasHDR10 && source == VideoRangeHDR10Plus) {
		return true
	}
	switch source {
	case VideoRangeHDR10Plus, VideoRangeHDR10, VideoRangeHLG:
		return false
	}
	return canRemoveDynamicHDR(dynamicHDRRemoval(video, requested), video, caps)
}

// splitRangeTypes splits a requested range-type list as
// EncodingJobInfo.GetRequestedRangeTypes does (EncodingJobInfo.cs:593-603).
// The names compare without case; each is returned in its canonical form
// when it parses.
func splitRangeTypes(requested string) []string {
	var result []string
	for _, candidate := range strings.FieldsFunc(requested, func(r rune) bool { return r == ',' || r == '|' }) {
		candidate = strings.TrimSpace(candidate)
		if parsed, ok := ParseVideoRangeType(candidate); ok {
			candidate = parsed.String()
		}
		if candidate != "" {
			result = append(result, candidate)
		}
	}
	return result
}

// dynamicHDRRemoval ports EncodingHelper.ShouldRemoveDynamicHdrMetadata
// (EncodingHelper.cs:1411-1466). requested is the client range-type list for
// the source codec.
func dynamicHDRRemoval(video *MediaStream, requested string) encoding.DynamicHDRRemoval {
	if video == nil || video.VideoRange() != MediaVideoRangeHDR {
		return encoding.RemoveNoDynamicHDR
	}
	requestedTypes := splitRangeTypes(requested)
	if len(requestedTypes) == 0 {
		return encoding.RemoveNoDynamicHDR
	}
	has := func(rangeType VideoRangeType) bool { return slices.Contains(requestedTypes, rangeType.String()) }
	source := video.VideoRangeType()
	// Case 1: the client plays HDR10 but not Dolby Vision with an
	// enhancement layer. Case 2: a Dolby Vision client and an invalid Dolby
	// Vision configuration.
	removeDOVI := (!has(VideoRangeDOVIWithEL) && has(VideoRangeHDR10) && source == VideoRangeDOVIWithEL) ||
		(has(VideoRangeDOVI) && source == VideoRangeDOVIInvalid)
	removeHDR10Plus := false
	if source == VideoRangeDOVIWithELHDR10Plus {
		removeHDR10Plus = has(VideoRangeDOVIWithEL) && !has(VideoRangeDOVIWithELHDR10Plus)
		removeDOVI = removeDOVI || !removeHDR10Plus
	}
	if removeDOVI {
		return encoding.RemoveDolbyVision
	}
	if removeHDR10Plus || (has(VideoRangeDOVI) && source == VideoRangeDOVIWithHDR10Plus) {
		return encoding.RemoveHDR10Plus
	}
	return encoding.RemoveNoDynamicHDR
}

// canRemoveDynamicHDR ports EncodingHelper.CanEncoderRemoveDynamicHdrMetadata
// (EncodingHelper.cs:1468-1479).
func canRemoveDynamicHDR(removal encoding.DynamicHDRRemoval, video *MediaStream, caps encoding.Capabilities) bool {
	codec := strings.ToLower(video.Codec)
	hevc := strings.Contains(codec, "hevc") || strings.Contains(codec, "265")
	av1 := strings.Contains(codec, "av1")
	switch removal {
	case encoding.RemoveDolbyVision:
		return caps.SupportsBSFOption("dovi_rpu=strip") ||
			(hevc && caps.SupportsBSFOption("hevc_metadata=remove_dovi")) ||
			(av1 && caps.SupportsBSFOption("av1_metadata=remove_dovi"))
	case encoding.RemoveHDR10Plus:
		return (hevc && caps.SupportsBSFOption("hevc_metadata=remove_hdr10plus")) ||
			(av1 && caps.SupportsBSFOption("av1_metadata=remove_hdr10plus"))
	default:
		return true
	}
}

func progressiveVideoRotation(request ProgressiveRequest, qualifier string) string {
	if request.Rotation != nil {
		return integerString(*request.Rotation)
	}
	if value := request.StreamOptions.Get("rotation"); value != "" {
		return value
	}
	return request.StreamOptions.Get(qualifier + "-rotation")
}

func progressiveRotationSupported(source *int, requested string) bool {
	if requested == "" {
		return true
	}
	rotation := 0
	if source != nil {
		rotation = *source
	}
	if rotation == 0 {
		return true
	}
	for _, candidate := range strings.FieldsFunc(requested, func(r rune) bool { return r == ',' || r == '|' }) {
		if supported := parseOptionInt(candidate); supported != nil && *supported == rotation {
			return true
		}
	}
	return false
}

func progressiveAudioChannels(request ProgressiveRequest) *int {
	if value := parseOptionInt(request.StreamOptions.Get("ac3-audiochannels")); value != nil {
		return value
	}
	if request.MaxAudioChannels != nil {
		return request.MaxAudioChannels
	}
	if request.AudioChannels != nil {
		return request.AudioChannels
	}
	return request.TranscodingMaxAudioChannels
}

func progressiveAudioBitDepth(request ProgressiveRequest) *int {
	if request.MaxAudioBitDepth != nil {
		return request.MaxAudioBitDepth
	}
	return parseOptionInt(request.StreamOptions.Get("ac3-audiobitdepth"))
}

func progressiveSubtitleEncodeRequested(request ProgressiveRequest) bool {
	return request.SubtitleStreamIndex != nil && *request.SubtitleStreamIndex >= 0 && request.SubtitleDeliveryMethod != nil && *request.SubtitleDeliveryMethod == SubtitleEncode
}

func progressiveOutput(container string) (outputContainer, extension, contentType string) {
	switch container {
	case "ts":
		return "ts", ".ts", "video/mp2t"
	case "fmp4":
		return "fmp4", ".fmp4", "video/mp4"
	case "mkv":
		return "mkv", ".mkv", "video/x-matroska"
	default: // validateProgressiveRequest has already limited this to the mp4 alias.
		return "fmp4", ".mp4", "video/mp4"
	}
}

const progressiveRenditionIdentityOutputPath = "/progressive-rendition-identity/output"

func progressiveOutputPath(transcodeDir, userAgent, deviceID, playSessionID, sourceRevision string, args []string, extension string) string {
	key := make([]byte, 0, len(userAgent)+len(deviceID)+len(playSessionID)+len(sourceRevision)+len(extension)+64)
	for _, value := range []string{"progressive-rendition-v2", userAgent, deviceID, playSessionID, sourceRevision, extension} {
		key = appendLengthPrefixedIdentity(key, value)
	}
	key = strconv.AppendInt(key, int64(len(args)), 10)
	key = append(key, ':')
	for _, argument := range args {
		key = appendLengthPrefixedIdentity(key, argument)
	}
	return filepath.Join(transcodeDir, images.MD5GUID(string(key))+extension)
}

func appendLengthPrefixedIdentity(target []byte, value string) []byte {
	target = strconv.AppendInt(target, int64(len(value)), 10)
	target = append(target, ':')
	return append(target, value...)
}

func hasCanonicalSourceAuthority(source MediaSourceInfo) bool {
	return source.SourceRevision != "" && source.FileFacts.Inode != nil && source.FileFacts.ChangedAt != nil
}

func progressiveOutputDimensions(video *MediaStream, request ProgressiveRequest) (int, int) {
	if video.Width != nil && video.Height != nil {
		width, height := *video.Width, *video.Height
		if video.Rotation != nil && (*video.Rotation == 90 || *video.Rotation == -90) {
			width, height = height, width
		}
		size := images.ResolveDimensions(images.Dimensions{Width: width, Height: height}, images.ResizeOptions{
			Width: request.Width, Height: request.Height, MaxWidth: request.MaxWidth, MaxHeight: request.MaxHeight,
		})
		return size.Width, size.Height
	}
	return progressiveDimensionFallback(request.MaxWidth, request.Width), progressiveDimensionFallback(request.MaxHeight, request.Height)
}

func progressiveDimensionFallback(maximum, fixed *int) int {
	if maximum != nil {
		return *maximum
	}
	if fixed != nil {
		return *fixed
	}
	return 0
}

func progressiveEncodingJob(source MediaSourceInfo, video, audio *MediaStream, outputContainer, outputPath string, request ProgressiveRequest) encoding.EncodingJob {
	outputWidth, outputHeight := progressiveOutputDimensions(video, request)
	job := encoding.EncodingJob{
		InputPath:        source.Path,
		InputContainer:   source.Container,
		OutputPath:       outputPath,
		OutputContainer:  outputContainer,
		OutputFrameRate:  progressiveOutputFrameRate(video, request),
		OutputVideoCodec: "copy",
		OutputAudioCodec: "copy",
		OutputWidth:      outputWidth,
		OutputHeight:     outputHeight,
		StartOffsetTicks: request.StartTimeTicks,
		CopyTimestamps:   request.CopyTimestamps,
		VideoStream:      encodingVideoStream(video),
		AudioStream:      encoding.AudioStream{Index: audioIndex(source.MediaStreams, audio), Codec: audioCodec(audio), Channels: audioChannels(audio)},
	}
	job.VideoStream.Index = ffmpegStreamIndex(source.MediaStreams, video)
	return job
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nalLengthSize(value *string) string {
	if value == nil {
		return "0"
	}
	return *value
}

func intPointerValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// audioIndex returns the ffmpeg input index of the audio stream
// (EncodingHelper.FindIndex), or zero without audio.
func audioIndex(streams []MediaStream, audio *MediaStream) int {
	if audio == nil {
		return 0
	}
	return ffmpegStreamIndex(streams, audio)
}

func audioCodec(audio *MediaStream) string {
	if audio == nil {
		return ""
	}
	return audio.Codec
}

func audioChannels(audio *MediaStream) int {
	if audio == nil {
		return 0
	}
	return intPointerValue(audio.Channels)
}

// validateProgressiveTranscode refuses a progressive request that Deadfin's
// encoders cannot satisfy. A progressive video transcode encodes H.264 with
// libx264 and AAC audio (D-50), and burns in no subtitle (D-126).
func validateProgressiveTranscode(source MediaSourceInfo, video *MediaStream, request ProgressiveRequest, copyVideo, copyAudio bool, caps encoding.Capabilities) error {
	if !copyVideo {
		if len(request.VideoCodecs) != 0 && !containsProgressiveCodec(request.VideoCodecs, "h264") {
			return fmt.Errorf("%w: progressive encoder cannot satisfy requested video codecs", ErrProgressiveUnsupported)
		}
		if caps.H264Encoder != "libx264" {
			return fmt.Errorf("%w: progressive video transcode requires libx264", ErrProgressiveUnsupported)
		}
		if progressiveSubtitleEncodeRequested(request) {
			return fmt.Errorf("%w: progressive video transcode cannot burn in subtitles", ErrProgressiveUnsupported)
		}
	}
	if !copyAudio && len(request.AudioCodecs) != 0 && !containsProgressiveCodec(request.AudioCodecs, "aac") {
		return fmt.Errorf("%w: progressive encoder cannot satisfy requested audio codecs", ErrProgressiveUnsupported)
	}
	return nil
}

// progressiveVideoTranscode sets the H.264 output of a progressive video
// transcode: the bitrate, profile and size decisions of
// StreamingHelpers.GetStreamingState that the HLS path also uses.
func progressiveVideoTranscode(job *encoding.EncodingJob, source MediaSourceInfo, video *MediaStream, request ProgressiveRequest) {
	job.OutputVideoCodec = "h264"
	job.OutputVideoBitrate = hlsOutputVideoBitrate(video, request, job.OutputVideoCodec)
	if job.OutputVideoBitrate == 0 {
		job.OutputVideoBitrate = intPointerValue(source.FallbackMaxStreamingBitrate)
	}
	job.OutputVideoProfile, job.OutputVideoLevel = hlsH264OutputConstraints(request)
	job.VideoQuality = encoding.VideoQuality{
		Preset:                "veryfast",
		CRF:                   23,
		ForcedKeyFrameSeconds: 3,
		X264Options:           "subme=0:me_range=16:rc_lookahead=10:me=hex:open_gop=0",
	}
	hlsApplyResolutionNormalizer(job, video, HLSRequest{ProgressiveRequest: request})
}

// progressiveAudioTranscode sets the AAC output of a progressive transcode
// (EncodingHelper.GetAudioBitrateParam and GetNumAudioChannelsParam).
func progressiveAudioTranscode(job *encoding.EncodingJob, audio *MediaStream, request ProgressiveRequest) {
	job.OutputAudioCodec = "aac"
	job.OutputAudioChannels = hlsOutputAudioChannels(audio.Channels, progressiveAudioChannels(request))
	job.OutputAudioBitrate = encoding.AudioBitrate(request.AudioBitrate, job.OutputAudioCodec, intPointerValue(audio.Channels), job.OutputAudioChannels)
	if request.AudioSampleRate != nil && *request.AudioSampleRate > 0 {
		job.OutputAudioSampleRate = *request.AudioSampleRate
	}
}
