// SPDX-License-Identifier: GPL-2.0-only

package playback

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
)

// VirtualPathExpander replaces virtual path placeholders in stored paths
// before they reach API responses or filesystem operations. The playback
// package defines this interface so it does not depend on the path package.
// A nil expander leaves paths unchanged.
type VirtualPathExpander interface {
	Expand(string) string
}

var (
	// ErrItemNotFound maps an unresolved playback item to the stock 404 response.
	ErrItemNotFound = errors.New("playback item not found")
	// ErrMediaSourceNotFound identifies an unknown source on an otherwise
	// accessible playback item. It wraps ErrItemNotFound to preserve the
	// selector's historical non-disclosure contract for callers that do not
	// need the distinction.
	ErrMediaSourceNotFound = fmt.Errorf("%w: media source not found", ErrItemNotFound)
	// ErrDownloadNotAllowed identifies a visible item that cannot be delivered
	// through the physical-file download endpoint.
	ErrDownloadNotAllowed = errors.New("download not allowed")
	// ErrLiveStreamUnavailable identifies Live TV operations without a composed provider.
	ErrLiveStreamUnavailable = errors.New("live stream provider unavailable")
	// ErrLiveStreamBusy identifies a Live TV open refused because every
	// tuner that carries the channel is in use.
	ErrLiveStreamBusy = errors.New("live stream: all tuners are in use")
	// ErrInvalidStoredDeviceProfile identifies malformed persisted device capability JSON.
	ErrInvalidStoredDeviceProfile = errors.New("invalid stored device profile")
	// ErrPlaySessionIDUnavailable identifies unavailable session entropy.
	ErrPlaySessionIDUnavailable = errors.New("play session ID unavailable")
)

// PlaybackInfoService is the application seam used by the PlaybackInfo HTTP
// surface. Implementations own source lookup, profile application, and live
// stream lifecycle; handlers only bind stock transport parameters.
type PlaybackInfoService interface {
	PlaybackInfo(context.Context, PlaybackInfoRequest) (PlaybackInfoResponse, error)
	OpenLiveStream(context.Context, OpenLiveStreamRequest) (LiveStreamResponse, error)
	CloseLiveStream(context.Context, string) error
}

// DirectFileSourceService resolves an optionally access-filtered selected
// source for a static local-file response. It intentionally excludes device
// negotiation and play-session creation: direct-file transport only needs the
// existing ordered source graph.
type DirectFileSourceService interface {
	StaticSource(context.Context, dto.UUID, dto.UUID, string) (MediaSourceInfo, error)
}

// DownloadFileSourceService resolves the canonical physical source eligible
// for an item download. It is intentionally narrower than StaticSource.
type DownloadFileSourceService interface {
	DownloadSource(context.Context, dto.UUID, dto.UUID) (MediaSourceInfo, error)
}

// DeviceProfileReader provides the current stored capabilities for an exact
// client/device session. It is separate from the request so transport adapters
// can keep the request profile strongly typed while preserving body-over-stored
// precedence.
type DeviceProfileReader interface {
	DeviceProfile(context.Context, string, string) (*DeviceProfile, error)
}

// PlaybackPermissionReader supplies the current user's playback policy facts.
type PlaybackPermissionReader interface {
	PlaybackPermissions(context.Context, dto.UUID) (PlaybackPermissions, error)
}

// PlaybackItemAccessReader resolves item visibility with the authenticated
// user's complete library policy. The playback service owns when this check is
// applied; store adapters own policy evaluation through their access-aware
// item-query seam.
type PlaybackItemAccessReader interface {
	AccessiblePlaybackItemIDs(context.Context, dto.UUID, []dto.UUID) ([]dto.UUID, error)
}

// PlaybackNetworkClassifier identifies whether a client address is local to
// the server for WAN policy bitrate clamping.
type PlaybackNetworkClassifier interface {
	IsLAN(netip.Addr) bool
}

// LiveStreamProvider is the deferred Live TV adapter. A nil provider means
// that LiveStreams/Open is unavailable while Close remains idempotent.
type LiveStreamProvider interface {
	OpenLiveStream(context.Context, OpenLiveStreamRequest) (LiveStreamResponse, error)
	CloseLiveStream(context.Context, string) error
}

// LiveTvSources supplies the media sources of Live TV channels (upstream
// LiveTvMediaSourceProvider) and the sources of opened live streams
// (MediaSourceManager.GetLiveStream).
type LiveTvSources interface {
	// ChannelSources returns the sources of a channel; ok is false when the
	// item is not a Live TV channel.
	ChannelSources(ctx context.Context, itemID dto.UUID) (sources []MediaSourceInfo, ok bool, err error)
	// LiveStream returns the source of an open live stream.
	LiveStream(ctx context.Context, liveStreamID string) (MediaSourceInfo, bool)
}

// LiveStartup times the first open of a live stream, for the live HLS
// startup trace. A zero time is a step that did not happen.
type LiveStartup struct {
	// Received is when PlaybackInfo asked to open the stream.
	Received time.Time
	// Connected is when the upstream answered, and FirstByte is when its
	// first data arrived.
	Connected time.Time
	FirstByte time.Time
	// Probed is when the media probe of the opened stream ended.
	Probed time.Time
	// Delivered counts the upstream bytes received so far.
	Delivered int64
}

// LiveStartupSource is a LiveTvSources that reports the startup times of an
// open live stream.
type LiveStartupSource interface {
	LiveStartup(liveStreamID string) (LiveStartup, bool)
}

// PlaybackInfoServiceConfig supplies the concrete orchestration dependencies.
// Every dependency is typed so HTTP, store, session, user, and network adapters
// map their own models exactly once at the application seam.
type PlaybackInfoServiceConfig struct {
	Sources          MediaSourceGraphReader
	Access           PlaybackItemAccessReader
	DeviceProfiles   DeviceProfileReader
	Permissions      PlaybackPermissionReader
	Network          PlaybackNetworkClassifier
	NewPlaySessionID func() string
	LiveStreams      LiveStreamProvider
	LiveTv           LiveTvSources
	Selection        SourceSelectionReader
	Languages        LanguageNormalizer
	Paths            VirtualPathExpander
	// Encoding decides whether a non-SDR source can transcode.
	Encoding TranscodeEncoding
	// Logger receives Live TV open failures; nil discards them.
	Logger *slog.Logger
}

// PlaybackInfoApplication is the concrete PlaybackInfoService implementation.
// Its methods deliberately remain the only place where source assembly,
type PlaybackInfoApplication struct {
	sources          MediaSourceGraphReader
	access           PlaybackItemAccessReader
	deviceProfiles   DeviceProfileReader
	permissions      PlaybackPermissionReader
	network          PlaybackNetworkClassifier
	newPlaySessionID func() string
	liveStreams      LiveStreamProvider
	liveTv           LiveTvSources
	selection        SourceSelectionReader
	languages        LanguageNormalizer
	paths            VirtualPathExpander
	encoding         TranscodeEncoding
	logger           *slog.Logger
}

// NewPlaybackInfoService constructs the concrete playback orchestration
// service. Nil optional adapters retain their stock no-provider behavior.
func NewPlaybackInfoService(config PlaybackInfoServiceConfig) *PlaybackInfoApplication {
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	return &PlaybackInfoApplication{
		sources:          config.Sources,
		access:           config.Access,
		deviceProfiles:   config.DeviceProfiles,
		permissions:      config.Permissions,
		network:          config.Network,
		newPlaySessionID: config.NewPlaySessionID,
		liveStreams:      config.LiveStreams,
		liveTv:           config.LiveTv,
		selection:        config.Selection,
		languages:        config.Languages,
		paths:            config.Paths,
		encoding:         config.Encoding,
		logger:           config.Logger,
	}
}

// expandSource applies virtual path expansion to a single media source. When
// the expander is nil, the source is returned unchanged.
func (service *PlaybackInfoApplication) expandSource(source MediaSourceInfo) MediaSourceInfo {
	if service.paths == nil {
		return source
	}
	expandMediaSourcePaths(&source, service.paths)
	return source
}

// expandSources applies virtual path expansion to a slice of media sources.
// When the expander is nil, the slice is returned unchanged.
func (service *PlaybackInfoApplication) expandSources(sources []MediaSourceInfo) []MediaSourceInfo {
	if service.paths == nil {
		return sources
	}
	for index := range sources {
		expandMediaSourcePaths(&sources[index], service.paths)
	}
	return sources
}

// PlaybackInfo assembles one source snapshot, resolves the effective current
// user/device facts, mints exactly one response session ID, then applies the
// pure negotiation resolver to every candidate source.
func (service *PlaybackInfoApplication) PlaybackInfo(ctx context.Context, request PlaybackInfoRequest) (PlaybackInfoResponse, error) {
	if service.sources == nil {
		return PlaybackInfoResponse{}, ErrItemNotFound
	}
	if service.access != nil {
		accessible, err := service.access.AccessiblePlaybackItemIDs(ctx, request.UserID, []dto.UUID{request.ItemID})
		if err != nil {
			return PlaybackInfoResponse{}, err
		}
		if !containsPlaybackItemID(accessible, request.ItemID) {
			return PlaybackInfoResponse{}, ErrItemNotFound
		}
	}
	if service.liveTv != nil {
		sources, isChannel, err := service.liveTv.ChannelSources(ctx, request.ItemID)
		if err != nil {
			return PlaybackInfoResponse{}, err
		}
		if isChannel {
			return service.liveTvPlaybackInfo(ctx, request, sources)
		}
	}
	records, err := assembleMediaSourceRecords(ctx, service.sources, request.ItemID)
	if err != nil {
		return PlaybackInfoResponse{}, err
	}
	if service.access != nil {
		accessible, err := service.access.AccessiblePlaybackItemIDs(ctx, request.UserID, playbackRecordIDs(records))
		if err != nil {
			return PlaybackInfoResponse{}, err
		}
		records = filterAccessiblePlaybackRecords(records, accessible)
	}
	var selection *SourceSelection
	if service.selection != nil && !request.UserID.IsNil() {
		resolved, found, err := service.selection.SourceSelection(ctx, request.UserID, request.ItemID, playbackRecordIDs(records))
		if err != nil {
			return PlaybackInfoResponse{}, err
		}
		if found {
			selection = &resolved
		}
	}
	rawSourceProjection, err := service.prepareRequest(ctx, &request)
	if err != nil {
		return PlaybackInfoResponse{}, err
	}
	playSessionID := request.Session.ID
	if request.AssemblyOnly {
		sources := MediaSourcesFromRecords(records)
		if rawSourceProjection {
			sources = rawMediaSourcesFromRecords(records)
		}
		if selection != nil {
			sources = ApplyUserSourceSelection(sources, *selection, service.languages)
		}
		return PlaybackInfoResponse{MediaSources: service.expandSources(sources), PlaySessionID: playSessionID}, nil
	}
	sources := MediaSourcesFromRecords(records)
	if selection != nil {
		sources = ApplyUserSourceSelection(sources, *selection, service.languages)
	}
	sources = service.expandSources(sources)
	request.MediaType = sourceMediaType(sources)
	request.CanTranscodeVideo = service.encoding.CanTranscodeVideo
	request.CanExtractSubtitles = service.encoding.CanExtractSubtitles
	response, err := ResolvePlaybackInfo(request, sources)
	if err != nil {
		return PlaybackInfoResponse{}, err
	}
	if selection != nil {
		response.MediaSources = reorderResolvedPlaybackSources(response.MediaSources, *selection)
	}
	return response, nil
}

// prepareRequest fills the current device profile, permissions, network
// and a new play session ID into request. rawSourceProjection reports a
// request that falls back to assembly because no device profile exists.
func (service *PlaybackInfoApplication) prepareRequest(ctx context.Context, request *PlaybackInfoRequest) (rawSourceProjection bool, err error) {
	if !request.AssemblyOnly {
		if request.DeviceProfile == nil && service.deviceProfiles != nil {
			request.DeviceProfile, err = service.deviceProfiles.DeviceProfile(ctx, request.Client, request.DeviceID)
			if err != nil {
				return false, err
			}
		}
		if request.DeviceProfile == nil {
			request.AssemblyOnly = true
			rawSourceProjection = true
		}
		if service.permissions != nil {
			request.Permissions, err = service.permissions.PlaybackPermissions(ctx, request.UserID)
			if err != nil {
				return false, err
			}
		}
		if service.network != nil {
			request.Network = PlaybackNetwork{IsLAN: !request.ClientAddress.IsValid() || service.network.IsLAN(request.ClientAddress)}
		}
	}
	playSessionID, err := service.newSessionID()
	if err != nil {
		return false, err
	}
	request.Session.ID = playSessionID
	if request.Session.DeviceID == "" {
		request.Session.DeviceID = request.DeviceID
	}
	return rawSourceProjection, nil
}

// liveTvPlaybackInfo ports the Live TV half of MediaInfoController
// PostPlaybackInfo (MediaInfoController.cs:116-260) and
// MediaInfoHelper.GetPlaybackInfo (MediaInfoHelper.cs:111-116): a request
// with a LiveStreamId answers that open stream; AutoOpenLiveStream opens
// the channel and answers the opened source only. A busy tuner answers
// ErrorCode NoCompatibleStream with no source.
func (service *PlaybackInfoApplication) liveTvPlaybackInfo(ctx context.Context, request PlaybackInfoRequest, sources []MediaSourceInfo) (PlaybackInfoResponse, error) {
	if request.LiveStreamID != "" {
		source, found := service.liveTv.LiveStream(ctx, request.LiveStreamID)
		if !found {
			return PlaybackInfoResponse{}, ErrItemNotFound
		}
		sources = []MediaSourceInfo{source}
	}
	if _, err := service.prepareRequest(ctx, &request); err != nil {
		return PlaybackInfoResponse{}, err
	}
	playSessionID := request.Session.ID
	if request.LiveStreamID == "" && request.AutoOpenLiveStream && len(sources) > 0 && sources[0].RequiresOpening && sources[0].LiveStreamID == "" {
		if service.liveStreams == nil {
			return PlaybackInfoResponse{}, ErrLiveStreamUnavailable
		}
		opened, err := service.liveStreams.OpenLiveStream(ctx, OpenLiveStreamRequest{
			OpenToken:                           sources[0].OpenToken,
			UserID:                              request.UserID,
			PlaySessionID:                       playSessionID,
			DeviceID:                            request.Session.DeviceID,
			MaxStreamingBitrate:                 request.MaxStreamingBitrate,
			StartTimeTicks:                      request.StartTimeTicks,
			AudioStreamIndex:                    request.AudioStreamIndex,
			SubtitleStreamIndex:                 request.SubtitleStreamIndex,
			MaxAudioChannels:                    request.MaxAudioChannels,
			ItemID:                              request.ItemID,
			DeviceProfile:                       request.DeviceProfile,
			EnableDirectPlay:                    request.EnableDirectPlay,
			EnableDirectStream:                  request.EnableDirectStream,
			AlwaysBurnInSubtitleWhenTranscoding: request.AlwaysBurnInSubtitleWhenTranscoding,
		})
		if errors.Is(err, ErrLiveStreamBusy) {
			service.logger.Warn("live tv: open refused, all tuners in use", "itemId", request.ItemID.String(), "error", err)
			return PlaybackInfoResponse{MediaSources: []MediaSourceInfo{}, PlaySessionID: playSessionID, ErrorCode: PlaybackInfoNoCompatibleStream}, nil
		}
		if err != nil {
			service.logger.Warn("live tv: open failed", "itemId", request.ItemID.String(), "error", err)
			return PlaybackInfoResponse{}, err
		}
		sources = []MediaSourceInfo{opened.MediaSource}
		// Upstream negotiates the opened source by its own id
		// (MediaInfoHelper.OpenMediaSource).
		request.MediaSourceID = opened.MediaSource.ID
	}
	if request.AssemblyOnly {
		return PlaybackInfoResponse{MediaSources: sources, PlaySessionID: playSessionID}, nil
	}
	request.MediaType = sourceMediaType(sources)
	request.CanTranscodeVideo = service.encoding.CanTranscodeVideo
	request.CanExtractSubtitles = service.encoding.CanExtractSubtitles
	response, err := ResolvePlaybackInfo(request, sources)
	if err != nil || response.ErrorCode != PlaybackInfoNoCompatibleStream || len(sources) == 0 {
		return response, err
	}
	service.logger.Warn("live tv: no compatible stream for the channel source",
		append([]any{"itemId", request.ItemID.String(), "opened", sources[0].LiveStreamID != ""}, noCompatibleDetail(request, sources[0])...)...)
	if liveStreamID := sources[0].LiveStreamID; liveStreamID != "" && request.LiveStreamID == "" {
		// The client gets no source to close, so the open would hold the
		// tuner until the broker's idle timeout.
		if err := service.liveStreams.CloseLiveStream(ctx, liveStreamID); err != nil {
			service.logger.Warn("live tv: close of the unplayable stream failed", "liveStreamId", liveStreamID, "error", err)
		}
	}
	return response, nil
}

// noCompatibleDetail names what the resolver compared: the source, and the
// client's direct play and transcoding containers.
func noCompatibleDetail(request PlaybackInfoRequest, source MediaSourceInfo) []any {
	var video, audio string
	if stream := source.VideoStream(); stream != nil {
		video = stream.Codec
	}
	for _, stream := range source.MediaStreams {
		if stream.Type == StreamTypeAudio {
			audio = stream.Codec
			break
		}
	}
	var directPlay, transcoding []string
	if profile := request.DeviceProfile; profile != nil {
		for _, candidate := range profile.DirectPlayProfiles {
			directPlay = append(directPlay, cmp.Or(candidate.Container, "*"))
		}
		for _, candidate := range profile.TranscodingProfiles {
			transcoding = append(transcoding, cmp.Or(candidate.Container, "*"))
		}
	}
	return []any{
		"container", source.Container, "videoCodec", video, "audioCodec", audio,
		"supportsDirectPlay", source.SupportsDirectPlay, "supportsDirectStream", source.SupportsDirectStream,
		"supportsTranscoding", source.SupportsTranscoding, "mostCompatibleOnly", source.UseMostCompatibleTranscodingProfile,
		"directPlayProfiles", strings.Join(directPlay, ","), "transcodingProfiles", strings.Join(transcoding, ","),
		"enableTranscoding", request.Permissions.EnableVideoTranscoding, "enableRemuxing", request.Permissions.EnableRemuxing,
	}
}

// reorderResolvedPlaybackSources restores the primary item's resumable
// alternate preference without replaying source-selection stream defaults over
// the resolver's negotiated result.
func reorderResolvedPlaybackSources(sources []MediaSourceInfo, selection SourceSelection) []MediaSourceInfo {
	if !selection.PrimaryItem || len(sources) < 2 {
		return sources
	}

	best, found := 0, false
	var bestLastPlayed time.Time
	for index := range sources {
		data, exists := selection.VersionUserData[sources[index].ID]
		if !exists || data.PositionTicks <= 0 {
			continue
		}
		lastPlayed := time.Time{}
		if data.LastPlayed != nil {
			lastPlayed = *data.LastPlayed
		}
		if !found || lastPlayed.After(bestLastPlayed) {
			best, bestLastPlayed, found = index, lastPlayed, true
		}
	}
	if !found || best == 0 {
		return sources
	}

	selected := sources[best]
	copy(sources[1:best+1], sources[:best])
	sources[0] = selected
	return sources
}

// StaticSource resolves one exact direct-file source from the ordered source
// graph without applying device negotiation or creating a play session. A
// non-zero user ID applies the existing access filter. Missing items and
// inaccessible items return ErrItemNotFound. An unknown source on an
// accessible graph returns ErrMediaSourceNotFound.
func (service *PlaybackInfoApplication) StaticSource(ctx context.Context, userID, itemID dto.UUID, mediaSourceID string) (MediaSourceInfo, error) {
	if service.sources == nil {
		return MediaSourceInfo{}, ErrItemNotFound
	}
	if userID != dto.NilUUID && service.access != nil {
		accessible, err := service.access.AccessiblePlaybackItemIDs(ctx, userID, []dto.UUID{itemID})
		if err != nil {
			return MediaSourceInfo{}, err
		}
		if !containsPlaybackItemID(accessible, itemID) {
			return MediaSourceInfo{}, ErrItemNotFound
		}
	}

	records, err := assembleMediaSourceRecords(ctx, service.sources, itemID)
	if err != nil {
		return MediaSourceInfo{}, err
	}
	if userID != dto.NilUUID && service.access != nil {
		accessible, err := service.access.AccessiblePlaybackItemIDs(ctx, userID, playbackRecordIDs(records))
		if err != nil {
			return MediaSourceInfo{}, err
		}
		records = filterAccessiblePlaybackRecords(records, accessible)
	}
	if len(records) == 0 {
		return MediaSourceInfo{}, ErrItemNotFound
	}

	if mediaSourceID == "" {
		return service.expandSource(MediaSourceFromRecord(records[0])), nil
	}
	for _, record := range records {
		if record.ID.String() == mediaSourceID {
			return service.expandSource(MediaSourceFromRecord(record)), nil
		}
	}
	if sourceID, err := dto.ParseUUID(mediaSourceID); err == nil && sourceID == itemID {
		return service.expandSource(MediaSourceFromRecord(records[0])), nil
	}
	return MediaSourceInfo{}, ErrMediaSourceNotFound
}

// UniversalAudioSource resolves the universal-audio source with the same
// access-filtered graph as StaticSource, but matches source IDs using the
// upstream OrdinalIgnoreCase semantics.
func (service *PlaybackInfoApplication) UniversalAudioSource(ctx context.Context, userID, itemID dto.UUID, mediaSourceID string) (MediaSourceInfo, error) {
	records, err := service.accessibleSourceRecords(ctx, userID, itemID)
	if err != nil {
		return MediaSourceInfo{}, err
	}
	if mediaSourceID == "" {
		return service.expandSource(MediaSourceFromRecord(records[0])), nil
	}
	for _, record := range records {
		if strings.EqualFold(record.ID.String(), mediaSourceID) {
			return service.expandSource(MediaSourceFromRecord(record)), nil
		}
	}
	if sourceID, parseErr := dto.ParseUUID(mediaSourceID); parseErr == nil && sourceID == itemID {
		return service.expandSource(MediaSourceFromRecord(records[0])), nil
	}
	return MediaSourceInfo{}, ErrMediaSourceNotFound
}

// DownloadSource resolves only the accessible canonical root source and
// verifies that its persisted item capability permits a physical download.
func (service *PlaybackInfoApplication) DownloadSource(ctx context.Context, userID, itemID dto.UUID) (MediaSourceInfo, error) {
	records, err := service.accessibleSourceRecords(ctx, userID, itemID)
	if err != nil {
		return MediaSourceInfo{}, err
	}
	for _, record := range records {
		if record.ID != itemID {
			continue
		}
		if !record.Downloadable {
			return MediaSourceInfo{}, ErrDownloadNotAllowed
		}
		return service.expandSource(MediaSourceFromRecord(record)), nil
	}
	return MediaSourceInfo{}, ErrItemNotFound
}

func (service *PlaybackInfoApplication) accessibleSourceRecords(ctx context.Context, userID, itemID dto.UUID) ([]MediaSourceRecord, error) {
	if service.sources == nil {
		return nil, ErrItemNotFound
	}
	if userID != dto.NilUUID && service.access != nil {
		accessible, err := service.access.AccessiblePlaybackItemIDs(ctx, userID, []dto.UUID{itemID})
		if err != nil {
			return nil, err
		}
		if !containsPlaybackItemID(accessible, itemID) {
			return nil, ErrItemNotFound
		}
	}
	records, err := assembleMediaSourceRecords(ctx, service.sources, itemID)
	if err != nil {
		return nil, err
	}
	if userID != dto.NilUUID && service.access != nil {
		accessible, err := service.access.AccessiblePlaybackItemIDs(ctx, userID, playbackRecordIDs(records))
		if err != nil {
			return nil, err
		}
		records = filterAccessiblePlaybackRecords(records, accessible)
	}
	if len(records) == 0 {
		return nil, ErrItemNotFound
	}
	return records, nil
}

// OpenLiveStream delegates only when a provider was composed. Live TV is not
// available in the local-file implementation, so nil providers cannot invent
// a media source.
func (service *PlaybackInfoApplication) OpenLiveStream(ctx context.Context, request OpenLiveStreamRequest) (LiveStreamResponse, error) {
	if service.liveStreams == nil {
		return LiveStreamResponse{}, ErrLiveStreamUnavailable
	}
	return service.liveStreams.OpenLiveStream(ctx, request)
}

// CloseLiveStream is idempotent without a composed provider, matching an
// empty live-stream registry while avoiding a fake lifecycle implementation.
func (service *PlaybackInfoApplication) CloseLiveStream(ctx context.Context, liveStreamID string) error {
	if service.liveStreams == nil {
		return nil
	}
	return service.liveStreams.CloseLiveStream(ctx, liveStreamID)
}

func containsPlaybackItemID(ids []dto.UUID, id dto.UUID) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func playbackRecordIDs(records []MediaSourceRecord) []dto.UUID {
	ids := make([]dto.UUID, len(records))
	for index, record := range records {
		ids[index] = record.ID
	}
	return ids
}

func filterAccessiblePlaybackRecords(records []MediaSourceRecord, accessible []dto.UUID) []MediaSourceRecord {
	if len(records) == 0 || len(accessible) == 0 {
		return nil
	}
	allowed := make(map[dto.UUID]struct{}, len(accessible))
	for _, id := range accessible {
		allowed[id] = struct{}{}
	}
	filtered := records[:0]
	for _, record := range records {
		if _, ok := allowed[record.ID]; ok {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func (service *PlaybackInfoApplication) newSessionID() (string, error) {
	if service.newPlaySessionID != nil {
		if value := service.newPlaySessionID(); value != "" {
			return value, nil
		}
		return "", ErrPlaySessionIDUnavailable
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("%w: %v", ErrPlaySessionIDUnavailable, err)
	}
	return hex.EncodeToString(value[:]), nil
}

func sourceMediaType(sources []MediaSourceInfo) DlnaProfileType {
	for _, source := range sources {
		for _, stream := range source.MediaStreams {
			if stream.Type == StreamTypeVideo {
				return DlnaVideo
			}
		}
	}
	return DlnaAudio
}

// PlaybackInfoRequest is the pure media-negotiation input. Its identifiers and
// device profile use the same typed model as the application adapter.
type PlaybackInfoRequest struct {
	ItemID                              dto.UUID
	UserID                              dto.UUID
	Client                              string
	MaxStreamingBitrate                 *int
	StartTimeTicks                      *int64
	AudioStreamIndex                    *int
	SubtitleStreamIndex                 *int
	MaxAudioChannels                    *int
	MediaSourceID                       string
	LiveStreamID                        string
	AutoOpenLiveStream                  bool
	EnableDirectPlay                    bool
	EnableDirectStream                  bool
	EnableTranscoding                   bool
	AllowVideoStreamCopy                bool
	AllowAudioStreamCopy                bool
	AlwaysBurnInSubtitleWhenTranscoding bool
	DeviceProfile                       *DeviceProfile
	DeviceID                            string
	MediaType                           DlnaProfileType
	Session                             PlaybackSession
	Permissions                         PlaybackPermissions
	Network                             PlaybackNetwork
	ClientAddress                       netip.Addr
	AssemblyOnly                        bool
	ForceDirectStream                   bool
	// CanTranscodeVideo reports whether the encoder can transcode a video
	// stream. Nil allows only SDR sources (D-32).
	CanTranscodeVideo func(MediaStream) bool
	// CanExtractSubtitles reports whether an embedded subtitle stream can
	// be delivered External during a transcode. Nil reports false.
	CanExtractSubtitles func(string) bool
}

// PlaybackSession is caller-owned request identity shared by every generated
// source URL.
type PlaybackSession struct {
	ID          string
	DeviceID    string
	AccessToken string
}

// PlaybackPermissions controls the pure negotiation paths available to a user.
type PlaybackPermissions struct {
	EnableAudioTranscoding       bool
	EnableVideoTranscoding       bool
	EnableRemuxing               bool
	ForceRemoteSourceTranscoding bool
	RemoteBitrateLimit           *int
}

// PlaybackNetwork supplies the caller's locality classification.
type PlaybackNetwork struct{ IsLAN bool }

// PlaybackInfoError is the stock response error code.
type PlaybackInfoError string

const PlaybackInfoNoCompatibleStream PlaybackInfoError = "NoCompatibleStream"

type PlaybackInfoResponse struct {
	MediaSources  []MediaSourceInfo `json:"MediaSources"`
	PlaySessionID string            `json:"PlaySessionId,omitempty"`
	ErrorCode     PlaybackInfoError `json:"ErrorCode,omitempty"`
}

// MarshalJSON keeps PlaybackInfo's stream projection compatible with the pinned
// MediaInfo endpoint. ColorRange remains a persisted probe fact used by playback
// decisions and item projections, but the upstream PlaybackInfo response does
// not expose its ffprobe color-range token.
func (response PlaybackInfoResponse) MarshalJSON() ([]byte, error) {
	sources := make([]playbackInfoMediaSource, len(response.MediaSources))
	for index := range response.MediaSources {
		sources[index] = playbackInfoMediaSource(response.MediaSources[index])
	}
	return json.Marshal(struct {
		MediaSources  []playbackInfoMediaSource `json:"MediaSources"`
		PlaySessionID string                    `json:"PlaySessionId,omitempty"`
		ErrorCode     PlaybackInfoError         `json:"ErrorCode,omitempty"`
	}{sources, response.PlaySessionID, response.ErrorCode})
}

// playbackInfoMediaSource specializes only the PlaybackInfo wire projection;
// it deliberately leaves MediaSourceInfo's persistence and DTO serialization
// untouched.
type playbackInfoMediaSource MediaSourceInfo

func (source playbackInfoMediaSource) MarshalJSON() ([]byte, error) {
	type wire playbackInfoMediaSource

	var streams *[]MediaStream
	if source.MediaStreams != nil {
		projected := make([]MediaStream, len(source.MediaStreams))
		for index := range source.MediaStreams {
			projected[index] = source.MediaStreams[index]
			// PlaybackInfo omits the persisted ffprobe token but keeps the
			// normal MediaStream derived-wire projection.
			projected[index].ColorRange = nil
		}
		streams = &projected
	}
	var attachments *[]MediaAttachment
	var formats *[]string
	var headers *map[string]string
	var id *string
	if !source.idNull && (source.ID != "" || source.idSet) {
		id = &source.ID
	}
	if source.MediaAttachments != nil {
		attachments = &source.MediaAttachments
	}
	if source.Formats != nil {
		formats = &source.Formats
	}
	if source.RequiredHTTPHeaders != nil {
		headers = &source.RequiredHTTPHeaders
	}

	return json.Marshal(struct {
		wire
		ID                  *string            `json:"Id,omitempty"`
		MediaStreams        *[]MediaStream     `json:"MediaStreams,omitempty"`
		MediaAttachments    *[]MediaAttachment `json:"MediaAttachments,omitempty"`
		Formats             *[]string          `json:"Formats,omitempty"`
		RequiredHTTPHeaders *map[string]string `json:"RequiredHttpHeaders,omitempty"`
	}{wire(source), id, streams, attachments, formats, headers})
}

type OpenLiveStreamRequest struct {
	OpenToken     string
	UserID        dto.UUID
	PlaySessionID string
	// DeviceID is the requesting device. Its other live streams on the
	// same tuner are released before the open (D-92).
	DeviceID                            string
	MaxStreamingBitrate                 *int
	StartTimeTicks                      *int64
	AudioStreamIndex                    *int
	SubtitleStreamIndex                 *int
	MaxAudioChannels                    *int
	ItemID                              dto.UUID
	DeviceProfile                       *DeviceProfile
	EnableDirectPlay                    bool
	EnableDirectStream                  bool
	AlwaysBurnInSubtitleWhenTranscoding bool
}

type LiveStreamResponse struct {
	MediaSource MediaSourceInfo `json:"MediaSource"`
}
