// SPDX-License-Identifier: GPL-2.0-only

package playback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/hls"
	"github.com/deadfin-org/deadfin/internal/images"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

const (
	hlsPlaylistContentType = "application/x-mpegURL"
	hlsTSContentType       = "video/mp2t"
	hlsMP4ContentType      = "video/mp4"
)

// HLSManager owns the process and file lifecycle for one HLS playlist family.
// It is deliberately playback-owned so handlers do not depend on ffmpeg's
// filesystem-facing implementation.
// HLSSegmentTiming describes the exact interval represented by one requested HLS segment.
// A nil timing value preserves legacy nominal segment-length accounting.
type HLSSegmentTiming struct {
	RuntimeTicks             int64
	ActualSegmentLengthTicks int64
}

type HLSManager interface {
	StartHLS(context.Context, HLSJobRequest) error
	RestartHLSAt(context.Context, HLSJobRequest, int) error
	WaitSegment(context.Context, string, int, *HLSSegmentTiming) (io.ReadCloser, error)
	WaitPlaylist(context.Context, string, int) (string, error)
	PingPlaySession(string) bool
	StopPlaySession(context.Context, string, string) error
}

// HLSLegacyManager opens a previously produced HLS artifact by opaque name.
// It is intentionally separate from HLSManager to keep the active-process
// lifecycle seam compatible with managers that do not retain legacy artifacts.
type HLSLegacyManager interface {
	OpenLegacyHLS(context.Context, string, string) (io.ReadCloser, error)
}

// HLSVideoStreamer is the handler-facing HLS service seam.
type HLSVideoStreamer interface {
	Master(context.Context, HLSRequest) (HLSResponse, error)
	Main(context.Context, HLSRequest) (HLSResponse, error)
	Live(context.Context, HLSRequest) (HLSResponse, error)
	Segment(context.Context, HLSRequest) (HLSResponse, error)
	LegacyPlaylist(context.Context, string) (HLSResponse, error)
	LegacySegment(context.Context, string, string, string) (HLSResponse, error)
	Stop(context.Context, string, string) error
}

// HLSJobRequest is the process-facing, playlist-family portion of an HLS request.
type HLSJobRequest struct {
	PlaylistPath   string
	Live           bool
	MinSegments    int
	SegmentPattern string
	Args           []string
	// IdentityArgs is the rendition argv with no per-request seek or start
	// number. Segment requests of one rendition share it, so the manager
	// joins the running job instead of relaunching ffmpeg for each segment.
	IdentityArgs             []string
	EmbeddedSubtitleSource   string
	ExternalSubtitleSource   string
	ExternalSubtitleRevision string
	OpenExternalSubtitle     func() (*sourcefile.Source, error)
	PlaySessionID            string
	DeviceID                 string
	MediaSourceID            string
	SourceRevision           string
	OpenSource               func() (*sourcefile.Source, error)
	SegmentLengthSeconds     int
	// MaxForwardGap overrides the manager's forward reuse window, in
	// segment indexes. Zero keeps 24/SegmentLengthSeconds.
	MaxForwardGap int
	// SegmentStartTicks is the media start of each ffmpeg file, by file
	// index, for a keyframe-cut remux. Nil means SegmentLengthSeconds files.
	SegmentStartTicks []int64
	RunTimeTicks      int64
	StartTimeTicks    int64
	// TranscodingInfo is the session projection of this rendition.
	TranscodingInfo *dto.TranscodingInfo
}

// HLSTrickplayMetadata describes one image-tile rendition advertised by a
// master HLS playlist.
type HLSTrickplayMetadata struct {
	Width     int
	Height    int
	Bandwidth int
}

// HLSTrickplaySource resolves stored image-tile renditions for a media source.
type HLSTrickplaySource interface {
	TrickplayMetadata(context.Context, dto.UUID) ([]HLSTrickplayMetadata, error)
}

// HLSServiceConfig supplies the existing direct-file selector, pure encoder
// capabilities, and the caller-owned HLS lifecycle adapter.
type HLSServiceConfig struct {
	Sources      DirectFileSourceService
	TranscodeDir string
	Capabilities encoding.Capabilities
	// CurrentCapabilities, when set, returns the capabilities for the current
	// encoding.xml and replaces Capabilities for each new job.
	CurrentCapabilities func() encoding.Capabilities
	// Hardware returns the current encoding.xml acceleration options. Nil
	// selects the software path.
	Hardware   func() encoding.HardwareOptions
	Keyframes  hls.KeyframeSource
	Trickplays HLSTrickplaySource
	Manager    HLSManager
	// Subtitles and Attachments own the extraction caches that a text
	// subtitle burn-in reads. Nil makes the subtitles filter read the media
	// file.
	Subtitles   *SubtitleService
	Attachments *AttachmentService
	// AudioStart finds an audio stream that starts late, which HLS then
	// encodes with leading silence instead of copying (D-82). Nil copies.
	AudioStart AudioStartProber
}

// HLSRequest carries the stream-negotiated facts shared by all HLS endpoints.
type HLSRequest struct {
	ProgressiveRequest
	SegmentContainer                    string
	SegmentLengthSeconds                int
	SegmentLengthSet                    bool
	MinSegments                         int
	MinSegmentsSet                      bool
	SegmentIndex                        int
	RuntimeTicks                        int64
	ActualSegmentLengthTicks            int64
	ActualSegmentLengthTicksSet         bool
	EndpointPrefix                      string
	RawQuery                            string
	APIKey                              string
	Tag                                 string
	LiveStreamID                        string
	EnableSubtitlesInManifest           bool
	AlwaysBurnInSubtitleWhenTranscoding bool
	SubtitleCodec                       string
	TranscodeReasons                    string
	Static                              bool
	EnableTrickplay                     bool
	AudioOnly                           bool
	Live                                bool
}

// HLSResponse describes an HLS playlist or segment. Body is nil for HEAD.
type HLSResponse struct {
	Body        io.ReadCloser
	ContentType string
	OutputPath  string
}

// HLSService connects persisted direct-file selection to pure HLS builders and
// the playback-owned lifecycle manager.
type HLSService struct {
	sources      DirectFileSourceService
	transcodeDir string
	capabilities encoding.Capabilities
	current      func() encoding.Capabilities
	hardware     func() encoding.HardwareOptions
	keyframes    hls.KeyframeSource
	trickplays   HLSTrickplaySource
	manager      HLSManager
	subtitles    *SubtitleService
	attachments  *AttachmentService
	audioStarts  *hlsAudioStarts
}

// NewHLSService creates the handler-facing HLS service.
func NewHLSService(config HLSServiceConfig) *HLSService {
	return &HLSService{
		sources:      config.Sources,
		transcodeDir: config.TranscodeDir,
		capabilities: config.Capabilities,
		current:      config.CurrentCapabilities,
		hardware:     config.Hardware,
		keyframes:    config.Keyframes,
		trickplays:   config.Trickplays,
		manager:      config.Manager,
		subtitles:    config.Subtitles,
		attachments:  config.Attachments,
		audioStarts:  &hlsAudioStarts{prober: config.AudioStart},
	}
}

// currentCapabilities returns the encoder capabilities for a new job.
func (service *HLSService) currentCapabilities() encoding.Capabilities {
	if service.current != nil {
		return service.current()
	}
	return service.capabilities
}

// Master returns the HLS master playlist projection without starting a process.
func (service *HLSService) Master(ctx context.Context, request HLSRequest) (HLSResponse, error) {
	prepared, err := service.prepare(ctx, request)
	if err != nil {
		return HLSResponse{}, err
	}
	request = prepared.request
	variant := prepared.masterVariant()
	variant.SDRCompanion = hlsSDRCompanion(prepared)
	master := hls.MasterRequest{Variants: []hls.MasterVariant{variant}}
	if !request.AudioOnly {
		master.Subtitles = hlsMasterSubtitles(prepared.source, request)
		if len(master.Subtitles) != 0 {
			master.Variants[0].SubtitleGroup = "subs"
		}
	}
	if !request.AudioOnly && request.EnableTrickplay && !request.Live {
		trickplay, err := service.trickplayRenditions(ctx, prepared)
		if err != nil {
			return HLSResponse{}, err
		}
		master.Trickplay = trickplay
	}
	playlist, err := hls.BuildMasterPlaylist(master)
	if err != nil {
		return HLSResponse{}, fmt.Errorf("%w: build HLS master playlist: %v", ErrProgressiveUnsupported, err)
	}
	return prepared.playlistResponse(playlist)
}

// Main returns the VOD media-playlist projection without starting a process.
func (service *HLSService) Main(ctx context.Context, request HLSRequest) (HLSResponse, error) {
	prepared, err := service.prepare(ctx, request)
	if err != nil {
		return HLSResponse{}, err
	}
	request = prepared.request
	if request.EndpointPrefix == "" {
		return HLSResponse{}, fmt.Errorf("%w: HLS endpoint prefix is required", ErrProgressiveInvalid)
	}

	keyframes := service.keyframes
	if request.AudioOnly {
		keyframes = nil
	}
	playlist, err := hls.CreateMainPlaylist(ctx, hls.MainPlaylistRequest{
		ItemID:                    request.ItemID,
		MediaSourceID:             request.MediaSourceID,
		FilePath:                  prepared.source.Path,
		SegmentLengthMilliseconds: hlsMainSegmentLengthMilliseconds(prepared),
		RunTimeTicks:              int64PointerValue(prepared.source.RunTimeTicks),
		SegmentContainer:          request.SegmentContainer,
		EndpointPrefix:            request.EndpointPrefix,
		Query:                     hlsQuery(hlsSegmentQuery(request.RawQuery)),
		IsRemuxVideo:              !request.AudioOnly && prepared.job.OutputVideoCodec == "copy",
		Keyframes:                 keyframes,
	})
	if err != nil {
		return HLSResponse{}, fmt.Errorf("%w: build HLS media playlist: %v", ErrProgressiveUnsupported, err)
	}
	return prepared.playlistResponse(playlist)
}

// Live starts the playlist family, waits for manager-owned readiness, and
// rewrites only fMP4's initialization URI through the handler route.
func (service *HLSService) Live(ctx context.Context, request HLSRequest) (HLSResponse, error) {
	request.Live = true
	prepared, err := service.prepare(ctx, request)
	if err != nil {
		return HLSResponse{}, err
	}
	request = prepared.request
	response := prepared.response(hlsPlaylistContentType)
	if request.Head {
		return response, nil
	}

	jobRequest, err := service.managerRequest(prepared, true, false)
	if err != nil {
		return HLSResponse{}, err
	}
	if err := service.manager.StartHLS(ctx, jobRequest); err != nil {
		return HLSResponse{}, err
	}
	playlist, err := service.manager.WaitPlaylist(ctx, prepared.outputPath, request.MinSegments)
	if err != nil {
		return HLSResponse{}, err
	}
	identity := hlsLiveIdentity(playlist, prepared.playlistID)
	response.Body = io.NopCloser(strings.NewReader(hls.RewriteLivePlaylist(playlist, identity, request.SegmentContainer)))
	return response, nil
}

// Segment starts or joins the playlist family, delegates seek/restart policy to
// the manager, and returns the completed requested segment.
func (service *HLSService) Segment(ctx context.Context, request HLSRequest) (HLSResponse, error) {
	prepared, err := service.prepareHLS(ctx, request, true)
	if err != nil {
		return HLSResponse{}, err
	}
	request = prepared.request
	response := prepared.response(hlsSegmentContentType(request.SegmentContainer))
	if request.Head {
		return response, nil
	}

	jobRequest, err := service.managerRequest(prepared, request.Live, true)
	if err != nil {
		return HLSResponse{}, err
	}
	first, end := request.SegmentIndex, request.SegmentIndex+1
	if plan := prepared.keyframePlan; plan != nil && request.SegmentIndex >= 0 {
		if request.SegmentIndex >= len(plan.Segments) {
			return HLSResponse{}, fmt.Errorf("%w: HLS segment %d is outside the %d-segment keyframe playlist", ErrProgressiveInvalid, request.SegmentIndex, len(plan.Segments))
		}
		segment := plan.Segments[request.SegmentIndex]
		first, end = segment.FirstKeyframe, segment.EndKeyframe
	}
	if err := service.manager.RestartHLSAt(ctx, jobRequest, first); err != nil {
		return HLSResponse{}, err
	}
	var timing *HLSSegmentTiming
	if request.ActualSegmentLengthTicksSet {
		timing = &HLSSegmentTiming{
			RuntimeTicks:             request.RuntimeTicks,
			ActualSegmentLengthTicks: request.ActualSegmentLengthTicks,
		}
	}
	body, err := service.waitSegmentFiles(ctx, prepared.outputPath, first, end, timing)
	if err != nil {
		return HLSResponse{}, err
	}
	service.manager.PingPlaySession(request.PlaySessionID)
	response.Body = body
	return response, nil
}

// waitSegmentFiles returns the ffmpeg segment files [first, end) as one body.
// A keyframe-cut remux writes one file per source GOP, and an advertised
// segment is the concatenation of its GOPs; fMP4 fragments and MPEG-TS
// packets both concatenate.
func (service *HLSService) waitSegmentFiles(ctx context.Context, outputPath string, first, end int, timing *HLSSegmentTiming) (io.ReadCloser, error) {
	if end-first == 1 {
		body, err := service.manager.WaitSegment(ctx, outputPath, first, timing)
		if err != nil {
			return nil, err
		}
		if body == nil {
			return nil, fmt.Errorf("%w: HLS manager returned nil segment body", ErrProgressiveBackend)
		}
		return body, nil
	}
	parts := make([]io.ReadCloser, 0, end-first)
	for index := first; index < end; index++ {
		body, err := service.manager.WaitSegment(ctx, outputPath, index, timing)
		if err == nil && body == nil {
			err = fmt.Errorf("%w: HLS manager returned nil segment body", ErrProgressiveBackend)
		}
		if err != nil {
			for _, part := range parts {
				_ = part.Close()
			}
			return nil, err
		}
		parts = append(parts, body)
	}
	return newJoinedHLSSegment(parts), nil
}

type joinedHLSSegment struct {
	io.Reader
	parts []io.ReadCloser
}

func newJoinedHLSSegment(parts []io.ReadCloser) *joinedHLSSegment {
	readers := make([]io.Reader, len(parts))
	for index, part := range parts {
		readers[index] = part
	}
	return &joinedHLSSegment{Reader: io.MultiReader(readers...), parts: parts}
}

func (segment *joinedHLSSegment) Close() error {
	var first error
	for _, part := range segment.parts {
		if err := part.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// LegacyPlaylist returns a manager-owned historical HLS playlist by opaque ID.
func (service *HLSService) LegacyPlaylist(ctx context.Context, playlistID string) (HLSResponse, error) {
	if !validHLSArtifactToken(playlistID, false) {
		return HLSResponse{}, fmt.Errorf("%w: invalid HLS playlist ID", ErrProgressiveInvalid)
	}
	return service.openLegacyHLS(ctx, playlistID, playlistID+".m3u8", hlsPlaylistContentType)
}

// LegacySegment returns a manager-owned historical HLS or audio artifact.
func (service *HLSService) LegacySegment(ctx context.Context, playlistID, segmentID, container string) (HLSResponse, error) {
	container = strings.ToLower(container)
	if !validHLSArtifactToken(playlistID, true) || !validHLSArtifactToken(segmentID, false) || !validHLSLegacyContainer(container) {
		return HLSResponse{}, fmt.Errorf("%w: invalid HLS legacy artifact", ErrProgressiveInvalid)
	}
	artifact := segmentID + "." + container
	return service.openLegacyHLS(ctx, playlistID, artifact, hlsLegacyContentType(container))
}

func (service *HLSService) openLegacyHLS(ctx context.Context, playlistID, artifact, contentType string) (HLSResponse, error) {
	if service == nil || service.manager == nil {
		return HLSResponse{}, fmt.Errorf("%w: incomplete HLS service configuration", ErrProgressiveBackend)
	}
	manager, ok := service.manager.(HLSLegacyManager)
	if !ok {
		return HLSResponse{}, fmt.Errorf("%w: HLS manager does not support legacy artifacts", ErrProgressiveBackend)
	}
	body, err := manager.OpenLegacyHLS(ctx, playlistID, artifact)
	if err != nil {
		return HLSResponse{}, err
	}
	if body == nil {
		return HLSResponse{}, fmt.Errorf("%w: HLS manager returned nil legacy artifact body", ErrProgressiveBackend)
	}
	return HLSResponse{Body: body, ContentType: contentType, OutputPath: artifact}, nil
}

// Stop delegates exact device and play-session correlation to the manager.
func (service *HLSService) Stop(ctx context.Context, deviceID, playSessionID string) error {
	if service == nil || service.manager == nil {
		return fmt.Errorf("%w: incomplete HLS service configuration", ErrProgressiveBackend)
	}
	return service.manager.StopPlaySession(ctx, playSessionID, deviceID)
}

func (service *HLSService) trickplayRenditions(ctx context.Context, prepared preparedHLS) ([]hls.TrickplayRendition, error) {
	if service.trickplays == nil {
		return nil, nil
	}
	sourceID, err := dto.ParseUUID(prepared.request.MediaSourceID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid HLS media source ID", ErrProgressiveInvalid)
	}
	metadata, err := service.trickplays.TrickplayMetadata(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	renditions := make([]hls.TrickplayRendition, len(metadata))
	for index, item := range metadata {
		renditions[index] = hls.TrickplayRendition{
			URI:       "Trickplay/" + strconv.Itoa(item.Width) + "/tiles.m3u8?MediaSourceId=" + prepared.request.MediaSourceID + "&ApiKey=" + url.QueryEscape(prepared.request.APIKey),
			Bandwidth: item.Bandwidth,
			Width:     item.Width,
			Height:    item.Height,
		}
	}
	return renditions, nil
}

func validHLSArtifactToken(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	for _, runeValue := range value {
		if (runeValue >= 'a' && runeValue <= 'z') || (runeValue >= 'A' && runeValue <= 'Z') || (runeValue >= '0' && runeValue <= '9') || runeValue == '-' || runeValue == '_' {
			continue
		}
		return false
	}
	return true
}

func validHLSLegacyContainer(container string) bool {
	switch container {
	case "ts", "mp4", "fmp4", "mp3", "aac":
		return true
	default:
		return false
	}
}

func hlsLegacyContentType(container string) string {
	switch container {
	case "ts":
		return hlsTSContentType
	case "mp4", "fmp4":
		return hlsMP4ContentType
	case "mp3":
		return "audio/mpeg"
	default:
		return "audio/aac"
	}
}

type preparedHLS struct {
	request                  HLSRequest
	source                   MediaSourceInfo
	video                    *MediaStream
	audio                    *MediaStream
	job                      encoding.HLSJob
	capabilities             encoding.Capabilities
	args                     []string
	sourceRevision           string
	externalSubtitleRevision string
	openExternalSubtitle     func() (*sourcefile.Source, error)
	outputPath               string
	playlistID               string
	segment                  string
	// keyframePlan is set when a video-copy segment job cuts at every source
	// keyframe and Segment joins GOPs into the advertised segments.
	keyframePlan *hls.KeyframePlan
}

const hlsRenditionIdentityOutputPath = "/hls-rendition-identity/playlist.m3u8"

// hlsRenditionOutputPath names a family by its effective ffmpeg invocation
// and the currently resolved source revision. Session ownership stays in the
// key; API tokens and per-segment timing do not.
func hlsRenditionOutputPath(transcodeDir, playSessionID, deviceID, sourceRevision, subtitleRevision string, args []string) string {
	key := make([]byte, 0, len(playSessionID)+len(deviceID)+len(sourceRevision)+len(subtitleRevision)+64)
	for _, value := range []string{"hls-rendition-v3", playSessionID, deviceID, sourceRevision, subtitleRevision} {
		key = appendLengthPrefixedIdentity(key, value)
	}
	key = strconv.AppendInt(key, int64(len(args)), 10)
	key = append(key, ':')
	for _, argument := range args {
		key = appendLengthPrefixedIdentity(key, argument)
	}
	return filepath.Join(transcodeDir, images.MD5GUID(string(key))+".m3u8")
}

func (service *HLSService) prepare(ctx context.Context, request HLSRequest) (preparedHLS, error) {
	return service.prepareHLS(ctx, request, false)
}

// prepareHLS resolves the rendition. With keyframeCuts, a video-copy VOD
// rendition whose playlist snaps to source keyframes is prepared to cut at
// every keyframe (deadfin#629).
func (service *HLSService) prepareHLS(ctx context.Context, request HLSRequest, keyframeCuts bool) (preparedHLS, error) {
	if err := validateHLSRequest(request); err != nil {
		return preparedHLS{}, err
	}
	request.SegmentContainer = hlsInternalContainer(request.SegmentContainer)
	if service == nil || service.sources == nil || service.transcodeDir == "" || service.manager == nil {
		return preparedHLS{}, fmt.Errorf("%w: incomplete HLS service configuration", ErrProgressiveBackend)
	}

	source, err := service.sources.StaticSource(ctx, request.UserID, request.ItemID, request.MediaSourceID)
	if err != nil {
		return preparedHLS{}, err
	}
	if !hasCanonicalSourceAuthority(source) {
		return preparedHLS{}, fmt.Errorf("%w: media source has incomplete canonical file authority", ErrProgressiveUnsupported)
	}
	if source.ID != "" {
		request.MediaSourceID = source.ID
		request.RawQuery = hlsCanonicalMediaSourceQuery(request.RawQuery, source.ID)
	}
	request.RawQuery = hlsCanonicalAPIKeyQuery(request.RawQuery, request.APIKey)
	var video, audio *MediaStream
	if request.AudioOnly {
		audio, err = hlsAudioSourceStream(source, request)
	} else {
		video, audio, err = hlsSourceStreams(source, request)
	}
	if err != nil {
		return preparedHLS{}, err
	}
	capabilities := service.currentCapabilities()
	request = resolvedHLSRequest(request, source, video, capabilities)
	var hardware encoding.HardwareOptions
	if service.hardware != nil && !request.AudioOnly {
		hardware = service.hardware()
	}
	videoCodec := hlsTranscodeVideoCodec(request.VideoCodecs, capabilities, hardware)
	if err := validateHLSTargetCodecs(source, video, audio, request, videoCodec, capabilities); err != nil {
		return preparedHLS{}, err
	}
	var subtitle *MediaStream
	if !request.AudioOnly {
		subtitle, err = hlsSubtitleStream(source, request)
		if err != nil {
			return preparedHLS{}, err
		}
	}

	job := hlsEncodingJob(source, video, audio, subtitle, hlsRenditionIdentityOutputPath, request, videoCodec, capabilities)
	job.Hardware = hardware
	service.padLateAudio(ctx, source, audio, request, &job)
	service.useSubtitleCaches(ctx, source, subtitle, &job)
	job.Live = request.Live
	job.StartOffsetTicks = 0
	if job.Live && !request.AudioOnly {
		job.VideoQuality.Preset = "superfast"
	}
	var keyframePlan *hls.KeyframePlan
	if keyframeCuts {
		keyframePlan = service.remuxKeyframePlan(ctx, request, source, job)
		job.KeyframeCuts = keyframePlan != nil
	}
	identityArgs, err := encoding.BuildHLSArgs(job, capabilities)
	if err != nil {
		return preparedHLS{}, fmt.Errorf("%w: build HLS rendition identity: %v", ErrProgressiveUnsupported, err)
	}
	sourceRevision := source.SourceRevision
	externalSubtitleRevision, openExternalSubtitle, err := hlsExternalSubtitleAuthority(job)
	if err != nil {
		return preparedHLS{}, fmt.Errorf("%w: pin external HLS subtitle: %v", ErrProgressiveUnsupported, err)
	}
	outputPath := hlsRenditionOutputPath(service.transcodeDir, request.PlaySessionID, request.DeviceID, sourceRevision, externalSubtitleRevision, identityArgs)
	playlistID := strings.TrimSuffix(filepath.Base(outputPath), ".m3u8")
	segment := strings.TrimSuffix(outputPath, ".m3u8") + "%d" + hlsSegmentExtension(request.SegmentContainer)
	job.OutputPath = outputPath
	args, err := encoding.BuildHLSArgs(job, capabilities)
	if err != nil {
		return preparedHLS{}, fmt.Errorf("%w: build HLS job: %v", ErrProgressiveUnsupported, err)
	}
	return preparedHLS{
		request: request, source: source, video: video, audio: audio, job: job, capabilities: capabilities, args: args,
		sourceRevision: sourceRevision, externalSubtitleRevision: externalSubtitleRevision,
		openExternalSubtitle: openExternalSubtitle, outputPath: outputPath, playlistID: playlistID, segment: segment,
		keyframePlan: keyframePlan,
	}, nil
}

// remuxKeyframePlan returns the keyframe plan that Main advertises for this
// rendition, or nil when Main advertises equal-length segments.
func (service *HLSService) remuxKeyframePlan(ctx context.Context, request HLSRequest, source MediaSourceInfo, job encoding.HLSJob) *hls.KeyframePlan {
	if request.Live || request.AudioOnly || job.OutputVideoCodec != "copy" || service.keyframes == nil {
		return nil
	}
	plan, found, err := hls.RemuxKeyframePlan(ctx, hls.MainPlaylistRequest{
		ItemID:                    request.ItemID,
		MediaSourceID:             request.MediaSourceID,
		FilePath:                  source.Path,
		SegmentLengthMilliseconds: int64(request.SegmentLengthSeconds) * 1_000,
		RunTimeTicks:              int64PointerValue(source.RunTimeTicks),
		IsRemuxVideo:              true,
		Keyframes:                 service.keyframes,
	})
	if err != nil || !found {
		return nil
	}
	return &plan
}

// useSubtitleCaches points a text subtitle burn-in at the extraction caches,
// as TranscodeManager.StartFfMpeg and EncodingHelper.GetTextSubtitlesFilter
// do: all attachments are extracted first and become fontsdir, and an
// embedded ASS or SSA stream is read from its extracted file. libass then
// does not demux the whole media file each time ffmpeg starts or a seek
// restarts it. When an extraction fails, the filter reads the media
// file as before (D-61).
func (service *HLSService) useSubtitleCaches(ctx context.Context, source MediaSourceInfo, stream *MediaStream, job *encoding.HLSJob) {
	if stream == nil || job.SubtitleStream == nil || encoding.HLSSubtitleFilterSource(*job) == "" {
		return
	}
	if !stream.IsExternal && rawExtractedSubtitle(*stream) && service.subtitles != nil && service.attachments != nil {
		path, err := service.subtitles.extractedFile(ctx, source, *stream)
		if err != nil {
			return
		}
		fonts, err := service.attachments.ExtractAll(ctx, source)
		if err != nil {
			return
		}
		job.SubtitleStream.ExtractedPath = path
		job.SubtitleStream.FontsDir = fonts
		return
	}
	if service.attachments != nil {
		if fonts, err := service.attachments.ExtractAll(ctx, source); err == nil {
			job.SubtitleStream.FontsDir = fonts
		}
	}
}

func hlsExternalSubtitleAuthority(job encoding.HLSJob) (string, func() (*sourcefile.Source, error), error) {
	subtitle := job.SubtitleStream
	if subtitle == nil || encoding.HLSSubtitleFilterSource(job) == "" {
		return "", nil, nil
	}
	path := subtitle.Path
	switch {
	case subtitle.IsExternal:
	case subtitle.ExtractedPath != "":
		path = subtitle.ExtractedPath
	default:
		return "", nil, nil
	}
	pinned, err := sourcefile.Open(path, sourcefile.Expected{})
	if err != nil {
		return "", nil, err
	}
	fingerprint := pinned.Fingerprint()
	if err := pinned.VerifyPathStillCurrent(); err != nil {
		_ = pinned.Close()
		return "", nil, err
	}
	if err := pinned.Close(); err != nil {
		return "", nil, err
	}
	revision := hlsSubtitleRevision(fingerprint)
	return revision, func() (*sourcefile.Source, error) {
		source, openErr := sourcefile.Open(path, sourcefile.Expected{})
		if openErr != nil {
			return nil, openErr
		}
		if !sameHLSFingerprint(source.Fingerprint(), fingerprint) {
			_ = source.Close()
			return nil, fmt.Errorf("%w: external subtitle %q changed", sourcefile.ErrStaleSource, path)
		}
		return source, nil
	}, nil
}

func hlsSubtitleRevision(fingerprint sourcefile.Fingerprint) string {
	key := make([]byte, 0, 160)
	for _, value := range []string{
		"hls-external-subtitle-v1",
		strconv.FormatUint(fingerprint.Dev, 10),
		strconv.FormatUint(fingerprint.Ino, 10),
		strconv.FormatInt(fingerprint.Size, 10),
		strconv.FormatInt(fingerprint.MTime.Unix(), 10),
		strconv.Itoa(fingerprint.MTime.Nanosecond()),
		strconv.FormatInt(fingerprint.CTime.Unix(), 10),
		strconv.Itoa(fingerprint.CTime.Nanosecond()),
	} {
		key = appendLengthPrefixedIdentity(key, value)
	}
	sum := sha256.Sum256(key)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sameHLSFingerprint(left, right sourcefile.Fingerprint) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Size == right.Size &&
		left.MTime.Equal(right.MTime) && left.CTime.Equal(right.CTime)
}

func hlsSourceStreams(source MediaSourceInfo, request HLSRequest) (*MediaStream, *MediaStream, error) {
	if err := validateHLSSource(source, request); err != nil {
		return nil, nil, err
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
	audio, err := hlsSelectedAudioStream(source, request.ProgressiveRequest)
	if err != nil {
		return nil, nil, err
	}
	return video, audio, nil
}

func hlsAudioSourceStream(source MediaSourceInfo, request HLSRequest) (*MediaStream, error) {
	if err := validateHLSSource(source, request); err != nil {
		return nil, err
	}
	audio, err := hlsSelectedAudioStream(source, request.ProgressiveRequest)
	if err != nil {
		return nil, err
	}
	if audio == nil {
		return nil, ErrProgressiveUnsupported
	}
	return audio, nil
}

func validateHLSSource(source MediaSourceInfo, request HLSRequest) error {
	if request.LiveStreamID != "" {
		return ErrProgressiveUnsupported
	}
	if !safeProgressiveSourcePath(source.Path) {
		return fmt.Errorf("%w: source path is not a safe local path", ErrProgressiveInvalid)
	}
	if source.Protocol != MediaProtocolFile || source.IsRemote || progressiveDiscSource(source) {
		return ErrProgressiveUnsupported
	}
	if !request.Live && (source.IsInfiniteStream || source.LiveStreamID != "") {
		return ErrProgressiveUnsupported
	}
	return nil
}

func hlsSelectedAudioStream(source MediaSourceInfo, request ProgressiveRequest) (*MediaStream, error) {
	if request.AudioStreamIndex != nil && *request.AudioStreamIndex != -1 {
		audio := source.MediaStream(StreamTypeAudio, *request.AudioStreamIndex)
		if audio == nil || audio.IsExternal {
			return nil, ErrProgressiveUnsupported
		}
		return audio, nil
	}
	audio := source.DefaultAudioStream(nil)
	if audio != nil && audio.IsExternal {
		return nil, ErrProgressiveUnsupported
	}
	return audio, nil
}

func validateHLSRequest(request HLSRequest) error {
	container := request.SegmentContainer
	if container == "" {
		container = "ts"
	}
	progressive := request.ProgressiveRequest
	progressive.Container = container
	if err := validateProgressiveRequest(progressive); err != nil {
		return err
	}
	if request.SegmentLengthSeconds < 0 || (request.SegmentLengthSet && request.SegmentLengthSeconds == 0) {
		return fmt.Errorf("%w: HLS segment length must be positive", ErrProgressiveInvalid)
	}
	if request.SegmentLengthSeconds > 0 && int64(request.SegmentLengthSeconds) > int64(^uint64(0)>>1)/1_000 {
		return fmt.Errorf("%w: HLS segment length overflows milliseconds", ErrProgressiveInvalid)
	}
	if request.MinSegments < 0 {
		return fmt.Errorf("%w: HLS minimum segments must not be negative", ErrProgressiveInvalid)
	}
	if request.SegmentIndex < -1 || (request.SegmentIndex == -1 && container != "fmp4" && container != "mp4") {
		return fmt.Errorf("%w: invalid HLS segment index", ErrProgressiveInvalid)
	}
	if strings.ContainsAny(request.RawQuery, "\r\n") || strings.ContainsAny(request.EndpointPrefix, "\r\n") {
		return fmt.Errorf("%w: invalid HLS route value", ErrProgressiveInvalid)
	}
	return nil
}

func resolvedHLSRequest(request HLSRequest, source MediaSourceInfo, video *MediaStream, caps encoding.Capabilities) HLSRequest {
	if request.SegmentContainer == "" {
		request.SegmentContainer = "ts"
	}
	if !request.SegmentLengthSet && request.SegmentLengthSeconds == 0 {
		if !request.Live && !request.AudioOnly && canCopyHLSVideo(source, video, request.ProgressiveRequest, caps) {
			request.SegmentLengthSeconds = 6
		} else {
			request.SegmentLengthSeconds = 3
		}
	}
	if !request.MinSegmentsSet && request.MinSegments == 0 {
		if request.SegmentLengthSeconds >= 10 {
			request.MinSegments = 2
		} else {
			request.MinSegments = 3
		}
	}
	return request
}

func hlsInternalContainer(container string) string {
	if container == "mp4" {
		return "fmp4"
	}
	return container
}

func hlsSegmentQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	prefix := ""
	query := rawQuery
	if query[0] == '?' {
		prefix, query = "?", query[1:]
	}
	parts := strings.Split(query, "&")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			kept = append(kept, part)
			continue
		}
		if strings.EqualFold(decodedKey, "runtimeTicks") || strings.EqualFold(decodedKey, "actualSegmentLengthTicks") {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) == len(parts) {
		return rawQuery
	}
	return prefix + strings.Join(kept, "&")
}

func hlsCanonicalMediaSourceQuery(rawQuery, sourceID string) string {
	prefix := ""
	query := rawQuery
	if strings.HasPrefix(query, "?") {
		prefix, query = "?", query[1:]
	}
	if query == "" {
		return prefix + "MediaSourceId=" + url.QueryEscape(sourceID)
	}
	parts := strings.Split(query, "&")
	kept := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err == nil && strings.EqualFold(decodedKey, "MediaSourceId") {
			continue
		}
		kept = append(kept, part)
	}
	kept = append(kept, "MediaSourceId="+url.QueryEscape(sourceID))
	return prefix + strings.Join(kept, "&")
}

// hlsCanonicalAPIKeyQuery preserves non-credential query bytes and their
// order, while ensuring generated HLS URIs carry the resolved credential once
// using the stock ApiKey spelling.
func hlsCanonicalAPIKeyQuery(rawQuery, apiKey string) string {
	if apiKey == "" {
		return rawQuery
	}
	prefix := ""
	query := rawQuery
	if strings.HasPrefix(query, "?") {
		prefix, query = "?", query[1:]
	}
	parts := make([]string, 0)
	if query != "" {
		parts = strings.Split(query, "&")
	}
	kept := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err == nil && (strings.EqualFold(decodedKey, "ApiKey") || strings.EqualFold(decodedKey, "api_key")) {
			continue
		}
		kept = append(kept, part)
	}
	kept = append(kept, "ApiKey="+url.QueryEscape(apiKey))
	return prefix + strings.Join(kept, "&")
}

func hlsMasterSubtitles(source MediaSourceInfo, request HLSRequest) []hls.SubtitleRendition {
	selectedHLS := request.SubtitleDeliveryMethod != nil && *request.SubtitleDeliveryMethod == SubtitleHLS
	if progressiveSubtitleEncodeRequested(request.ProgressiveRequest) || (request.SubtitleDeliveryMethod != nil && *request.SubtitleDeliveryMethod == SubtitleDrop) || (!request.EnableSubtitlesInManifest && !selectedHLS) {
		return nil
	}
	renderings := make([]hls.SubtitleRendition, 0)
	for index := range source.MediaStreams {
		stream := &source.MediaStreams[index]
		if !stream.IsTextSubtitleStream() {
			continue
		}
		language := stringPointerValue(stream.Language)
		if language == "" {
			language = "Unknown"
		}
		selected := request.SubtitleStreamIndex != nil && *request.SubtitleStreamIndex == stream.Index
		defaultSubtitle := selected && request.SubtitleDeliveryMethod != nil && *request.SubtitleDeliveryMethod == SubtitleHLS
		renderings = append(renderings, hls.SubtitleRendition{
			GroupID:  "subs",
			Name:     stream.displayTitle(),
			URI:      request.MediaSourceID + "/Subtitles/" + strconv.Itoa(stream.Index) + "/subtitles.m3u8?SegmentLength=30&ApiKey=" + url.QueryEscape(request.APIKey),
			Language: language,
			Default:  defaultSubtitle,
			Forced:   stream.IsForced,
		})
	}
	return renderings
}

func hlsSubtitleStream(source MediaSourceInfo, request HLSRequest) (*MediaStream, error) {
	if request.SubtitleStreamIndex == nil || *request.SubtitleStreamIndex == -1 {
		return nil, nil
	}
	subtitle := source.MediaStream(StreamTypeSubtitle, *request.SubtitleStreamIndex)
	if subtitle == nil {
		return nil, ErrProgressiveUnsupported
	}
	return subtitle, nil
}

// hlsEncodingJob builds the HLS job. videoCodec is the transcode output
// codec from hlsTranscodeVideoCodec; a copyable source copies instead.
func hlsEncodingJob(source MediaSourceInfo, video, audio, subtitle *MediaStream, outputPath string, request HLSRequest, videoCodec string, caps encoding.Capabilities) encoding.HLSJob {
	var job encoding.EncodingJob
	if request.AudioOnly {
		job = hlsAudioEncodingJob(source, audio, outputPath, request)
	} else {
		job = progressiveEncodingJob(source, video, audio, request.SegmentContainer, outputPath, request.ProgressiveRequest)
		job.OutputVideoCodec = videoCodec
		if job.OutputVideoCodec == "" {
			job.OutputVideoCodec = "h264"
		}
		// The pinned caller computes the bitrate before the stream-copy
		// decision, with the requested codec (StreamingHelpers.cs:208-211).
		job.OutputVideoBitrate = hlsOutputVideoBitrate(video, request.ProgressiveRequest, job.OutputVideoCodec)
		if canCopyHLSVideo(source, video, request.ProgressiveRequest, caps) {
			job.OutputVideoCodec = "copy"
		}
		switch job.OutputVideoCodec {
		case "h264":
			job.OutputVideoProfile, job.OutputVideoLevel = hlsH264OutputConstraints(request.ProgressiveRequest)
		case "hevc", "av1":
			job.OutputVideoProfile, job.OutputVideoLevel = hlsHardwareOutputConstraints(job.OutputVideoCodec, request.ProgressiveRequest)
		}
		job.OutputWidth, job.OutputHeight = hlsOutputDimensions(video, request)
		job.VideoQuality = encoding.VideoQuality{
			Preset:                "veryfast",
			CRF:                   23,
			ForcedKeyFrameSeconds: request.SegmentLengthSeconds,
			X264Options:           "subme=0:me_range=16:rc_lookahead=10:me=hex:open_gop=0",
		}
		if job.OutputVideoCodec != "copy" {
			hlsApplyResolutionNormalizer(&job, video, request)
		}
		if job.OutputVideoCodec == "h264" && job.OutputVideoLevel == 0 {
			job.OutputVideoLevel = hlsH264FitLevel(job, video)
		}
		if subtitle != nil {
			job.SubtitleStream = &encoding.SubtitleStream{
				InputIndex:    hlsSubtitleInputIndex(subtitle),
				Index:         ffmpegStreamIndex(source.MediaStreams, subtitle),
				SubtitleIndex: ffmpegSubtitleIndex(source.MediaStreams, subtitle),
				Codec:         subtitle.Codec,
				Path:          subtitle.Path,
				BurnIn:        job.OutputVideoCodec != "copy" && (request.AlwaysBurnInSubtitleWhenTranscoding || progressiveSubtitleEncodeRequested(request.ProgressiveRequest)),
				IsExternal:    subtitle.IsExternal,
			}
		}
	}
	job.CopyTimestamps = true
	if audio == nil {
		job.OutputAudioCodec = ""
		job.OutputAudioBitrate = 0
		job.OutputAudioChannels = 0
		job.OutputAudioSampleRate = 0
	} else {
		job.OutputAudioCodec = "aac"
		if canCopyHLSAudio(audio, request.ProgressiveRequest) {
			job.OutputAudioCodec = "copy"
		}
		job.OutputAudioBitrate = hlsOutputBitrate(audio.BitRate, request.AudioBitrate)
		job.OutputAudioChannels = hlsOutputAudioChannels(audio.Channels, progressiveAudioChannels(request.ProgressiveRequest))
		if request.AudioOnly && job.OutputAudioCodec != "copy" {
			// An audio-only request (universal audio) carries the client
			// MaxStreamingBitrate as AudioBitRate. StreamingHelpers.cs:186
			// limits it with EncodingHelper.GetAudioBitrateParam.
			job.OutputAudioBitrate = encoding.AudioBitrate(request.AudioBitrate, job.OutputAudioCodec, intPointerValue(audio.Channels), job.OutputAudioChannels)
		}
		if request.AudioSampleRate != nil && *request.AudioSampleRate > 0 {
			job.OutputAudioSampleRate = *request.AudioSampleRate
		}
		if boost := parseOptionFloat(request.StreamOptions.Get("downMixAudioBoost")); job.OutputAudioCodec != "copy" && boost != nil && job.OutputAudioChannels > 0 && intPointerValue(audio.Channels) > job.OutputAudioChannels {
			job.DownmixAudioBoost = boost
		}
	}
	copyVideo := !request.AudioOnly && job.OutputVideoCodec == "copy"
	removal := encoding.RemoveNoDynamicHDR
	if copyVideo {
		removal = dynamicHDRRemoval(video, progressiveVideoRangeTypes(request.ProgressiveRequest, strings.ToLower(video.Codec)))
		// A copy whose range the client lists does not need the removal.
		// Without the ffmpeg filter it stays a plain copy; upstream adds the
		// filter unconditionally and ffmpeg fails.
		if !canRemoveDynamicHDR(removal, video, caps) {
			removal = encoding.RemoveNoDynamicHDR
		}
	}
	return encoding.HLSJob{
		EncodingJob:          job,
		SegmentLengthSeconds: request.SegmentLengthSeconds,
		StartNumber:          0,
		RunTimeTicks:         int64PointerValue(source.RunTimeTicks),
		DirectDolbyVision:    copyVideo && hlsDOVISource(video) && !hlsDOVIEnhancementLayer(video) && hlsDOVIRequested(video, request) && removal != encoding.RemoveDolbyVision,
		RemoveDynamicHDR:     removal,
	}
}

// hlsApplyResolutionNormalizer applies parity/resolution-normalizer@v1 to the
// encoded H.264 job. The cap depends on the H.264-equivalent bitrate, the
// effective target frame rate, and HDR state. It does not use source
// dimensions. The returned limits narrow the existing explicit dimensions
// before the aspect-preserving resolution step.
//
// The pinned Jellyfin caller scales the requested bitrate through
// EncodingHelper.ScaleBitrate before it passes it to the resolution
// normalizer (StreamingHelpers.cs:228-241, EncodingHelper.cs:2774-2802).
// This path produces H.264, so the same-codec low-bitrate bands raise the
// reference bitrate. h264EquivalentResolutionBitrateV1 reproduces that
// conversion. The outputBitrate stays unchanged for ffmpeg rate control.
func hlsApplyResolutionNormalizer(job *encoding.EncodingJob, video *MediaStream, request HLSRequest) {
	// Keep the source size when the client asks for no size and its bitrate
	// covers the source (StreamingHelpers.cs:213-226).
	if request.Width == nil && request.Height == nil && request.MaxWidth == nil && request.MaxHeight == nil &&
		request.VideoBitrate != nil && video.BitRate != nil && *request.VideoBitrate >= *video.BitRate {
		if video.Width != nil || video.Height != nil {
			request.MaxWidth, request.MaxHeight = video.Width, video.Height
			job.OutputWidth, job.OutputHeight = hlsOutputDimensions(video, request)
		}
		return
	}
	inputBitrate := video.BitRate
	outputBitrate := job.OutputVideoBitrate
	h264Equivalent := h264EquivalentBitrate(outputBitrate, job.OutputVideoCodec)
	var targetFps *float64
	frameRate := progressiveOutputFrameRate(video, request.ProgressiveRequest)
	if frameRate > 0 {
		frameRateValue := frameRate
		targetFps = &frameRateValue
	} else if ref := video.ReferenceFrameRate(); ref != nil && *ref > 0 {
		// When no request rate constrains the source, use the source's own
		// frame rate. Without this, the normalizer defaults to 30 fps and
		// mis-scales sources below 30 fps.
		frameRateValue := float64(*ref)
		targetFps = &frameRateValue
	}
	limits := NormalizeResolutionV1(
		inputBitrate,
		outputBitrate,
		h264Equivalent,
		request.MaxWidth,
		request.MaxHeight,
		targetFps,
		hlsIsHDRTransfer(video),
	)
	// Assign both dimensions unconditionally. The normalizer clears
	// MaxHeight when it changes MaxWidth; a nil-guard here would leave the
	// caller's stale MaxHeight in place.
	request.MaxWidth = limits.MaxWidth
	request.MaxHeight = limits.MaxHeight
	job.OutputWidth, job.OutputHeight = hlsOutputDimensions(video, request)
}

// hlsIsHDRTransfer reports whether the source uses an HDR transfer
// characteristic that the normalizer treats as HDR. It treats arib-std-b67
// (HLG) and smpte2084 (PQ) as HDR, matching the measured RC2 detection.
func hlsIsHDRTransfer(video *MediaStream) bool {
	if video == nil || video.ColorTransfer == nil {
		return false
	}
	transfer := strings.ToLower(*video.ColorTransfer)
	return transfer == "arib-std-b67" || transfer == "smpte2084"
}

func hlsH264OutputConstraints(request ProgressiveRequest) (string, int) {
	requestedProfile := progressiveVideoProfile(request, "h264")
	requestedLevel := progressiveH264VideoLevel(request, "h264")
	if requestedProfile == nil && requestedLevel == nil {
		return "", 0
	}
	profile := "high"
	if requestedProfile != nil {
		for _, candidate := range strings.FieldsFunc(*requestedProfile, func(r rune) bool { return r == ',' || r == '|' }) {
			switch strings.ToLower(strings.ReplaceAll(candidate, " ", "")) {
			case "constrainedbaseline", "baseline", "extended":
				profile = "baseline"
			case "main":
				profile = "main"
			case "high", "progressivehigh", "constrainedhigh", "high10":
				profile = "high"
			default:
				continue
			}
			break
		}
	}
	// NormalizeTranscodingLevel (EncodingHelper.cs:1895-1904) caps H.264 at
	// 5.1. Without a requested level, hlsH264FitLevel sets it from the output.
	level := 0
	if requestedLevel != nil && *requestedLevel > 0 {
		level = min(int(*requestedLevel), 51)
	}
	return profile, level
}

// hlsH264FitLevel returns the lowest H.264 level, not below 4.1, that holds
// the output frame size, macroblock rate and bitrate. The master CODECS
// token and the encoder -level use it, so the playlist advertises the level
// of the stream. Pinned upstream advertises 4.1 and passes no -level when
// the request has none, and the encoder picks a higher level for larger
// output (D-66).
func hlsH264FitLevel(job encoding.EncodingJob, video *MediaStream) int {
	width, height := job.OutputWidth, job.OutputHeight
	if (width <= 0 || height <= 0) && video.Width != nil && video.Height != nil {
		width, height = *video.Width, *video.Height
	}
	frameRate := job.OutputFrameRate
	if frameRate <= 0 {
		frameRate = hlsFrameRate(video.ReferenceFrameRate())
	}
	widthMBs, heightMBs := (width+15)/16, (height+15)/16
	frameSize := widthMBs * heightMBs
	mbps := int(math.Ceil(float64(frameSize) * frameRate))
	// H.264 Table A-1 from level 4.1 up: MaxMBPS, MaxFS, and the High
	// profile MaxBR (1.25 x MaxBR) in bit/s.
	levels := [...]struct{ level, maxMBPS, maxFS, maxBitrate int }{
		{41, 245_760, 8_192, 62_500_000},
		{42, 522_240, 8_704, 62_500_000},
		{50, 589_824, 22_080, 168_750_000},
		{51, 983_040, 36_864, 300_000_000},
		{52, 2_073_600, 36_864, 300_000_000},
	}
	for _, limits := range levels {
		// A frame dimension must not exceed sqrt(8 * MaxFS) macroblocks.
		maxSide := int(math.Sqrt(float64(8 * limits.maxFS)))
		if frameSize <= limits.maxFS && widthMBs <= maxSide && heightMBs <= maxSide && mbps <= limits.maxMBPS && job.OutputVideoBitrate <= limits.maxBitrate {
			return limits.level
		}
	}
	return 51
}

func hlsAudioEncodingJob(source MediaSourceInfo, audio *MediaStream, outputPath string, request HLSRequest) encoding.EncodingJob {
	return encoding.EncodingJob{
		InputPath:        source.Path,
		InputContainer:   source.Container,
		OutputPath:       outputPath,
		OutputContainer:  request.SegmentContainer,
		OutputAudioCodec: "copy",
		StartOffsetTicks: request.StartTimeTicks,
		CopyTimestamps:   request.CopyTimestamps,
		AudioStream: encoding.AudioStream{
			Index:    ffmpegStreamIndex(source.MediaStreams, audio),
			Codec:    audio.Codec,
			Channels: intPointerValue(audio.Channels),
		},
	}
}

// canCopyHLSVideo retains ProgressiveService's full copy constraints while
// allowing an HLS-negotiated source codec such as VP9 in place of its H.264
// transport-specific codec check.
func canCopyHLSVideo(source MediaSourceInfo, video *MediaStream, request ProgressiveRequest, caps encoding.Capabilities) bool {
	if video == nil || !request.AllowVideoStreamCopy || !containsProgressiveCodec(request.VideoCodecs, video.Codec) {
		return false
	}
	if video.VideoRangeType() == VideoRangeDOVI && !hlsDOVIRequested(video, HLSRequest{ProgressiveRequest: request}) {
		return false
	}
	return canCopyVideoForCodec(source, video, request, strings.ToLower(video.Codec), caps)
}

// canCopyHLSAudio ports EncodingHelper.CanStreamCopyAudio
// (EncodingHelper.cs:2626-2686). The source codec must be in the requested
// audio codec list; the stream must fit the bit-depth, channel, sample-rate,
// and bitrate limits. Like upstream, it does not limit the source codec
// further: StreamBuilder already keeps only the codecs that the HLS segment
// container carries (StreamBuilder.cs:983-994), so an Opus source copies
// into fMP4 segments.
func canCopyHLSAudio(audio *MediaStream, request ProgressiveRequest) bool {
	if audio == nil || !request.AllowAudioStreamCopy || !request.EnableAutoStreamCopy || audio.Codec == "" || !containsProgressiveCodec(request.AudioCodecs, audio.Codec) {
		return false
	}
	if progressiveExceedsInt(audio.BitDepth, hlsRequestedAudioBitDepth(request, audio.Codec)) {
		return false
	}
	if maximum := hlsRequestedAudioChannels(request, audio.Codec); maximum != nil && (audio.Channels == nil || *audio.Channels <= 0 || *audio.Channels > *maximum) {
		return false
	}
	if request.AudioSampleRate != nil && (audio.SampleRate == nil || *audio.SampleRate <= 0 || *audio.SampleRate > *request.AudioSampleRate) {
		return false
	}
	return request.AudioBitrate == nil || audio.BitRate == nil || *audio.BitRate <= *request.AudioBitrate
}

// hlsRequestedAudioBitDepth ports EncodingJobInfo.GetRequestedAudioBitDepth
// (EncodingJobInfo.cs:682-699): MaxAudioBitDepth, then the
// "<codec>-audiobitdepth" option.
func hlsRequestedAudioBitDepth(request ProgressiveRequest, codec string) *int {
	if request.MaxAudioBitDepth != nil {
		return request.MaxAudioBitDepth
	}
	return parseOptionInt(request.StreamOptions.Get(codec + "-audiobitdepth"))
}

// hlsRequestedAudioChannels ports EncodingJobInfo.GetRequestedAudioChannels
// (EncodingJobInfo.cs:701-728): the "<codec>-audiochannels" option, then
// MaxAudioChannels, AudioChannels, and TranscodingMaxAudioChannels.
func hlsRequestedAudioChannels(request ProgressiveRequest, codec string) *int {
	if value := parseOptionInt(request.StreamOptions.Get(codec + "-audiochannels")); value != nil {
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

// hlsTranscodeVideoCodec ports the output codec choice of
// StreamingHelpers.GetStreamingState (StreamingHelpers.cs:90-93) after
// EncodingHelper.ShiftVideoCodecsIfNeeded (EncodingHelper.cs:7575-7606):
// HEVC and AV1 move behind the other codecs unless encoding.xml allows them.
// Upstream takes the first codec; Deadfin takes the first codec it can
// encode, because it has no VP9, libx265, or libsvtav1 encoder (D-37). An
// empty list selects H.264. An empty result means no requested codec can
// be encoded.
func hlsTranscodeVideoCodec(codecs []string, caps encoding.Capabilities, options encoding.HardwareOptions) string {
	if len(codecs) == 0 {
		return "h264"
	}
	shifted := make([]string, 0, len(codecs))
	var deferred []string
	for _, codec := range codecs {
		codec = strings.ToLower(codec)
		if codec == "h265" {
			codec = "hevc"
		}
		if (codec == "hevc" && !options.AllowHevcEncoding) || (codec == "av1" && !options.AllowAv1Encoding) {
			deferred = append(deferred, codec)
			continue
		}
		shifted = append(shifted, codec)
	}
	for _, codec := range append(shifted, deferred...) {
		switch codec {
		case "h264", "hevc", "av1":
			if encoding.CanEncodeVideo(codec, caps, options) {
				return codec
			}
		}
	}
	return ""
}

// hlsHardwareOutputConstraints returns the HEVC or AV1 profile and level
// that DynamicHlsHelper.GetOutputVideoCodecLevel (DynamicHlsHelper.cs:752-789)
// reports. Deadfin encodes 8-bit HEVC and AV1 only, so the profile is Main,
// as GetVideoQualityParam forces it.
func hlsHardwareOutputConstraints(codec string, request ProgressiveRequest) (string, int) {
	level := 120
	if codec == "av1" {
		level = 19
	}
	if requested := progressiveVideoLevel(request, codec); requested != nil && *requested > 0 {
		level = int(*requested)
	} else if codec == "hevc" {
		if requested := progressiveVideoLevel(request, "h265"); requested != nil && *requested > 0 {
			level = int(*requested)
		}
	}
	// NormalizeTranscodingLevel (EncodingHelper.cs:1874-1894) caps HEVC at
	// 5.0 and AV1 at 5.3.
	if codec == "av1" {
		return "main", min(level, 15)
	}
	return "main", min(level, 150)
}

// validateHLSTargetCodecs rejects a request that Deadfin's encoder cannot
// satisfy. When the source audio cannot be copied, Deadfin encodes AAC only
// (D-50), so the requested audio codec list must include AAC.
func validateHLSTargetCodecs(source MediaSourceInfo, video, audio *MediaStream, request HLSRequest, videoCodec string, caps encoding.Capabilities) error {
	if !request.AudioOnly && !canCopyHLSVideo(source, video, request.ProgressiveRequest, caps) && videoCodec == "" {
		return fmt.Errorf("%w: HLS encoder cannot satisfy requested video codecs", ErrProgressiveUnsupported)
	}
	if !canCopyHLSAudio(audio, request.ProgressiveRequest) && len(request.AudioCodecs) != 0 && !containsProgressiveCodec(request.AudioCodecs, "aac") {
		return fmt.Errorf("%w: HLS encoder cannot satisfy requested audio codecs", ErrProgressiveUnsupported)
	}
	return nil
}

// hlsOutputVideoBitrate ports EncodingHelper.GetVideoBitrateParamValue
// (EncodingHelper.cs:2688-2729). Unless the request upscales, the source
// bitrate (raised for low bitrates) caps the request. The result is scaled
// from the source codec to the output codec, and the request and 400 Mbit/s
// cap it. Without a requested bitrate, the source bitrate applies.
func hlsOutputVideoBitrate(video *MediaStream, request ProgressiveRequest, outputCodec string) int {
	if request.VideoBitrate == nil || *request.VideoBitrate <= 0 {
		return intPointerValue(video.BitRate)
	}
	requested := *request.VideoBitrate
	bitrate := requested
	upscaling := request.Height != nil && video.Height != nil && *request.Height > *video.Height &&
		request.Width != nil && video.Width != nil && *request.Width > *video.Width
	if !upscaling && video.BitRate != nil {
		bitrate = hlsMinVideoBitrate(*video.BitRate, requested)
	}
	bitrate = min(scaleVideoBitrate(bitrate, video.Codec, outputCodec), requested)
	const maxSaneBitrate = 400_000_000
	return min(bitrate, maxSaneBitrate)
}

// hlsMinVideoBitrate ports EncodingHelper.GetMinBitrate
// (EncodingHelper.cs:2731-2745).
func hlsMinVideoBitrate(source, requested int) int {
	if source <= 2_000_000 {
		source = convertToInt32(float64(source) * 2.5)
	} else if source <= 3_000_000 {
		source *= 2
	}
	return min(source, requested)
}

func hlsOutputBitrate(source, requested *int) int {
	if requested != nil && *requested > 0 {
		return *requested
	}
	return intPointerValue(source)
}

func hlsOutputAudioChannels(source, requested *int) int {
	if source == nil {
		return intPointerValue(requested)
	}
	if requested == nil || *requested <= 0 || *source <= *requested {
		return *source
	}
	return *requested
}

func hlsSubtitleInputIndex(subtitle *MediaStream) int {
	if subtitle.IsExternal {
		return 1
	}
	return 0
}

func (service *HLSService) managerRequest(prepared preparedHLS, live, seekSegment bool) (HLSJobRequest, error) {
	job := prepared.job
	job.Live = live
	job.StartOffsetTicks = 0
	startTimeTicks := int64(0)
	if live {
		startTimeTicks = prepared.request.StartTimeTicks
	}
	if live && !prepared.request.AudioOnly {
		job.VideoQuality.Preset = "superfast"
	}
	if seekSegment {
		startTimeTicks = prepared.request.RuntimeTicks
		if prepared.request.SegmentIndex >= 0 {
			job.StartNumber = prepared.request.SegmentIndex
		}
	}
	job.StartOffsetTicks = startTimeTicks
	maxForwardGap := 0
	if plan := prepared.keyframePlan; plan != nil && seekSegment {
		job.StartNumber, job.StartOffsetTicks, startTimeTicks, maxForwardGap = hlsKeyframeRestart(*plan, prepared.request.SegmentIndex, int64PointerValue(prepared.source.RunTimeTicks))
		job.KeyframeStartTicks = startTimeTicks
	}
	var segmentStartTicks []int64
	if plan := prepared.keyframePlan; plan != nil {
		segmentStartTicks = plan.KeyframeTicks
	}
	args, err := encoding.BuildHLSArgs(job, prepared.capabilities)
	if err != nil {
		return HLSJobRequest{}, fmt.Errorf("%w: build HLS manager job: %v", ErrProgressiveUnsupported, err)
	}
	facts := prepared.source.FileFacts
	expected := sourcefile.Expected{
		Size:       facts.Size,
		ModifiedAt: facts.ModifiedAt,
		Inode:      facts.Inode,
		ChangedAt:  facts.ChangedAt,
	}
	sourcePath := prepared.source.Path
	return HLSJobRequest{
		PlaylistPath:             prepared.outputPath,
		Live:                     live,
		MinSegments:              prepared.request.MinSegments,
		SegmentPattern:           prepared.segment,
		Args:                     args,
		IdentityArgs:             prepared.args,
		EmbeddedSubtitleSource:   encoding.HLSEmbeddedSubtitleFilterSource(job),
		ExternalSubtitleSource:   hlsExternalSubtitleFilterSource(job),
		ExternalSubtitleRevision: prepared.externalSubtitleRevision,
		OpenExternalSubtitle:     prepared.openExternalSubtitle,
		PlaySessionID:            prepared.request.PlaySessionID,
		DeviceID:                 prepared.request.DeviceID,
		MediaSourceID:            prepared.request.MediaSourceID,
		SourceRevision:           prepared.sourceRevision,
		OpenSource: func() (*sourcefile.Source, error) {
			return sourcefile.Open(sourcePath, expected)
		},
		SegmentLengthSeconds: prepared.request.SegmentLengthSeconds,
		MaxForwardGap:        maxForwardGap,
		SegmentStartTicks:    segmentStartTicks,
		RunTimeTicks:         int64PointerValue(prepared.source.RunTimeTicks),
		StartTimeTicks:       startTimeTicks,
		TranscodingInfo:      hlsTranscodingInfo(prepared, job.EncodingJob),
	}, nil
}

// hlsTranscodingInfo is the TranscodingInfo upstream
// TranscodeManager.ReportTranscodingProgress reports for a job before ffmpeg
// prints progress: the actual output codecs (the source codec for a copy,
// StreamState.ActualOutputVideoCodec), the output container, size, and
// channels, the copy flags, the configured acceleration, and the
// TranscodeReasons of the request.
func hlsTranscodingInfo(prepared preparedHLS, job encoding.EncodingJob) *dto.TranscodingInfo {
	request := prepared.request
	info := &dto.TranscodingInfo{
		IsVideoDirect:    job.OutputVideoCodec == "copy",
		IsAudioDirect:    job.OutputAudioCodec == "copy",
		TranscodeReasons: []string{},
	}
	if !request.AudioOnly && job.VideoStream.Codec != "" {
		info.VideoCodec = transcodingCodec(job.OutputVideoCodec, job.VideoStream.Codec)
		width, height := job.OutputWidth, job.OutputHeight
		if info.IsVideoDirect || width <= 0 || height <= 0 {
			width, height = job.VideoStream.Width, job.VideoStream.Height
		}
		info.Width = positiveInt(width)
		info.Height = positiveInt(height)
	}
	if job.AudioStream.Codec != "" {
		info.AudioCodec = transcodingCodec(job.OutputAudioCodec, job.AudioStream.Codec)
		channels := job.OutputAudioChannels
		if info.IsAudioDirect || channels <= 0 {
			channels = job.AudioStream.Channels
		}
		info.AudioChannels = positiveInt(channels)
	}
	container := request.SegmentContainer
	if container == "" {
		container = job.OutputContainer
	}
	if container != "" {
		info.Container = &container
	}
	info.Bitrate = positiveInt(job.OutputVideoBitrate + job.OutputAudioBitrate)
	hardware := strings.ToLower(job.Hardware.Type)
	if hardware == "" {
		hardware = "none"
	}
	info.HardwareAccelerationType = &hardware
	for _, reason := range strings.Split(request.TranscodeReasons, ",") {
		if reason = strings.TrimSpace(reason); reason != "" {
			info.TranscodeReasons = append(info.TranscodeReasons, reason)
		}
	}
	return info
}

func transcodingCodec(output, source string) *string {
	codec := output
	if codec == "copy" || codec == "" {
		codec = source
	}
	if codec == "" {
		return nil
	}
	return &codec
}

func positiveInt(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}

// hlsKeyframeRestart returns the start of a keyframe-cut job for an advertised
// segment: the GOP index of its first keyframe as the start number, the input
// seek, the start time, and the forward reuse window in GOPs.
//
// An input seek to the exact keyframe time lands on the previous keyframe, so
// the seek goes 0.5 s into the GOP, as GetFastSeekCommandLineParameter does
// (EncodingHelper.cs:3046-3072), but never past the middle of a shorter GOP.
// The reuse window spans the GOPs of the 24 s before the start keyframe, the
// distance upstream allows (24/SegmentLength segments).
func hlsKeyframeRestart(plan hls.KeyframePlan, segmentIndex int, runtimeTicks int64) (startNumber int, seekTicks, startTicks int64, maxForwardGap int) {
	const (
		seekOffsetTicks = 5_000_000
		reuseTicks      = 240_000_000
	)
	if segmentIndex < 0 || segmentIndex >= len(plan.Segments) {
		return 0, 0, 0, 0
	}
	first := plan.Segments[segmentIndex].FirstKeyframe
	keyframes := plan.KeyframeTicks
	window := sort.Search(first, func(index int) bool { return keyframes[index] >= keyframes[first]-reuseTicks })
	maxForwardGap = max(first-window, 1)
	if first == 0 {
		// The first GOP starts at the first packet; no seek.
		return 0, 0, 0, maxForwardGap
	}
	startTicks = keyframes[first]
	next := runtimeTicks
	if first+1 < len(keyframes) {
		next = keyframes[first+1]
	}
	seekTicks = startTicks
	if next > startTicks {
		seekTicks += min(seekOffsetTicks, (next-startTicks)/2)
	}
	return first, seekTicks, startTicks, maxForwardGap
}

func hlsExternalSubtitleFilterSource(job encoding.HLSJob) string {
	if job.SubtitleStream == nil || !job.SubtitleStream.IsExternal && job.SubtitleStream.ExtractedPath == "" {
		return ""
	}
	return encoding.HLSSubtitleFilterSource(job)
}

func (prepared preparedHLS) response(contentType string) HLSResponse {
	return HLSResponse{ContentType: contentType, OutputPath: prepared.outputPath}
}

func (prepared preparedHLS) playlistResponse(playlist string) (HLSResponse, error) {
	response := prepared.response(hlsPlaylistContentType)
	if prepared.request.Head {
		return response, nil
	}
	response.Body = io.NopCloser(strings.NewReader(playlist))
	return response, nil
}

func (prepared preparedHLS) masterVariant() hls.MasterVariant {
	audioProfile := ""
	if prepared.audio != nil && prepared.job.OutputAudioCodec == "copy" {
		audioProfile = stringPointerValue(prepared.audio.Profile)
	}
	variant := hls.MasterVariant{
		URI:          hlsMainURI(prepared.request.RawQuery),
		Audio:        hls.MasterAudio{Codec: hlsMasterAudioCodec(prepared.job.OutputAudioCodec, audioCodec(prepared.audio)), Profile: audioProfile},
		AudioBitrate: prepared.job.OutputAudioBitrate,
	}
	if prepared.request.AudioOnly {
		return variant
	}
	copyVideo := prepared.job.OutputVideoCodec == "copy"
	if copyVideo {
		variant.Video = hls.MasterVideo{
			Codec:       prepared.video.Codec,
			Profile:     stringPointerValue(prepared.video.Profile),
			Level:       hlsVideoLevel(prepared.video.Level),
			HighTier:    prepared.video.HighTier,
			BitDepth:    intPointerValue(prepared.video.BitDepth),
			PixelFormat: stringPointerValue(prepared.video.PixelFormat),
			Range:       hlsVideoRange(prepared.video),
			HDR10Plus:   boolPointerValue(prepared.video.Hdr10PlusPresentFlag) && prepared.job.RemoveDynamicHDR != encoding.RemoveHDR10Plus,
			DolbyVision: hlsDolbyVision(prepared.video, prepared.request, true),
		}
	} else {
		profile, level := prepared.job.OutputVideoProfile, prepared.job.OutputVideoLevel
		if profile == "" {
			profile = "high"
		}
		if level == 0 {
			level = 41
		}
		variant.Video = hls.MasterVideo{Codec: prepared.job.OutputVideoCodec, Profile: profile, Level: level, BitDepth: 8, Range: hls.VideoRangeSDR}
	}
	variant.VideoBitrate = prepared.job.OutputVideoBitrate
	variant.Width = prepared.job.OutputWidth
	variant.Height = prepared.job.OutputHeight
	if prepared.job.OutputFrameRate > 0 {
		variant.FrameRate = prepared.job.OutputFrameRate
	} else {
		variant.FrameRate = hlsFrameRate(prepared.video.ReferenceFrameRate())
	}
	return variant
}

func hlsOutputDimensions(video *MediaStream, request HLSRequest) (int, int) {
	return progressiveOutputDimensions(video, request.ProgressiveRequest)
}

func hlsSDRCompanion(prepared preparedHLS) *hls.MasterVideo {
	if prepared.request.AudioOnly || prepared.job.OutputVideoCodec != "copy" || prepared.video.VideoRange() != MediaVideoRangeHDR {
		return nil
	}
	return &hls.MasterVideo{Codec: "h264", Profile: "high", Level: 41, BitDepth: 8, Range: hls.VideoRangeSDR}
}

func hlsMainSegmentLengthMilliseconds(prepared preparedHLS) int64 {
	requested := int64(prepared.request.SegmentLengthSeconds) * 1_000
	if prepared.request.AudioOnly || prepared.job.OutputVideoCodec == "copy" {
		return requested
	}
	frameRate := hlsFrameRate(prepared.video.ReferenceFrameRate())
	if frameRate <= 0 {
		return requested
	}
	frames := math.Ceil(float64(requested) * frameRate / 1_000)
	return int64(math.Ceil(frames * 1_000 / frameRate))
}

func hlsMasterAudioCodec(outputCodec, sourceCodec string) string {
	if outputCodec == "copy" {
		return sourceCodec
	}
	return outputCodec
}

func boolPointerValue(value *bool) bool {
	return value != nil && *value
}

func hlsMainURI(rawQuery string) string { return "main.m3u8" + hlsQuery(rawQuery) }

func hlsQuery(rawQuery string) string {
	if rawQuery == "" || rawQuery[0] == '?' {
		return rawQuery
	}
	return "?" + rawQuery
}

func hlsSegmentExtension(container string) string {
	if container == "fmp4" {
		return ".mp4"
	}
	return ".ts"
}

func hlsSegmentContentType(container string) string {
	if container == "fmp4" {
		return hlsMP4ContentType
	}
	return hlsTSContentType
}

func hlsVideoLevel(level *float64) int {
	if level == nil || *level <= 0 {
		return 0
	}
	return int(*level)
}

func hlsFrameRate(frameRate *float32) float64 {
	if frameRate == nil || *frameRate <= 0 {
		return 0
	}
	return float64(*frameRate)
}

func hlsVideoRange(video *MediaStream) hls.VideoRange {
	switch video.VideoRangeType() {
	case VideoRangeHLG, VideoRangeDOVIWithHLG:
		return hls.VideoRangeHLG
	}
	if video.VideoRange() == MediaVideoRangeHDR {
		return hls.VideoRangeHDR
	}
	return hls.VideoRangeSDR
}

func hlsDolbyVision(video *MediaStream, request HLSRequest, copyVideo bool) *hls.DolbyVision {
	if !copyVideo || video.DvProfile == nil || video.DvLevel == nil {
		return nil
	}
	dolby := &hls.DolbyVision{Profile: *video.DvProfile, Level: *video.DvLevel}
	switch video.VideoRangeType() {
	case VideoRangeDOVI:
		dolby.Direct = hlsDOVIRequested(video, request)
	case VideoRangeDOVIWithHDR10, VideoRangeDOVIWithHDR10Plus:
		dolby.BaseLayer = hls.VideoRangeHDR
	case VideoRangeDOVIWithHLG:
		dolby.BaseLayer = hls.VideoRangeHLG
	default:
		return nil
	}
	return dolby
}

func hlsDOVIRequested(video *MediaStream, request HLSRequest) bool {
	rangeTypes := request.StreamOptions.Get(strings.ToLower(video.Codec) + "-rangetype")
	for _, candidate := range strings.FieldsFunc(rangeTypes, func(r rune) bool { return r == ',' || r == '|' }) {
		parsed, ok := ParseVideoRangeType(strings.TrimSpace(candidate))
		if ok && parsed == VideoRangeDOVI {
			return true
		}
	}
	return false
}

func hlsDOVISource(video *MediaStream) bool {
	switch video.VideoRangeType() {
	case VideoRangeDOVI, VideoRangeDOVIWithEL, VideoRangeDOVIWithHLG, VideoRangeDOVIWithSDR, VideoRangeDOVIWithHDR10, VideoRangeDOVIWithHDR10Plus, VideoRangeDOVIWithELHDR10Plus:
		return true
	default:
		return false
	}
}

// hlsDOVIEnhancementLayer reports a dual-layer (profile 7) source. Its copy
// is tagged hvc1 with no Dolby Vision configuration box, so the init segment
// matches the hvc1 master variant and the client plays the HDR10 base layer.
// No AVPlayer decodes a profile 7 configuration (D-65).
func hlsDOVIEnhancementLayer(video *MediaStream) bool {
	switch video.VideoRangeType() {
	case VideoRangeDOVIWithEL, VideoRangeDOVIWithELHDR10Plus:
		return true
	default:
		return false
	}
}

func hlsLiveIdentity(playlist, fallback string) string {
	const prefix = `#EXT-X-MAP:URI="`
	index := strings.Index(playlist, prefix)
	if index < 0 {
		return fallback
	}
	uri := playlist[index+len(prefix):]
	if end := strings.IndexByte(uri, '"'); end >= 0 {
		uri = uri[:end]
	}
	uri = filepath.Base(uri)
	return strings.TrimSuffix(uri, "-1.mp4")
}

func int64PointerValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
