// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/playback"
)

// RegisterDirectVideoStreams registers Jellyfin's static direct-file video
// surface and, when supplied, its local progressive copy/remux path. The
// optional progressive dependency preserves the static-only registration seam.
// Pinned VideosController serves these routes anonymously; Deadfin requires
// a credential, either a token or a PlaySessionId that an authenticated
// PlaybackInfo issued for the item, and scopes the item to its user
// (divergence D-54).
func RegisterDirectVideoStreams(rt *router.Router, service playback.DirectFileSourceService, progressive ...playback.ProgressiveVideoStreamer) {
	h := directFileStreamHandler{service: service}
	if len(progressive) != 0 {
		h.progressive = progressive[0]
	}
	for _, route := range []struct {
		method, pattern, operationID string
	}{
		{http.MethodGet, "/Videos/{itemId}/stream", "GetVideoStream"},
		{http.MethodHead, "/Videos/{itemId}/stream", "HeadVideoStream"},
		{http.MethodGet, "/Videos/{itemId}/stream.{container}", "GetVideoStreamByContainer"},
		{http.MethodHead, "/Videos/{itemId}/stream.{container}", "HeadVideoStreamByContainer"},
	} {
		rt.Handle(route.method, route.pattern, router.Route{OperationID: route.operationID, Policy: string(auth.PolicyDefaultAuthorization), PlaySessionCredential: true}, h.video)
	}
}

// RegisterDirectAudioStreams registers the audio stream routes and the
// universal-audio route. Without static, the stream routes transcode
// progressively. Pinned AudioController serves the stream routes
// anonymously; Deadfin requires a credential, either a token or a
// PlaySessionId that an authenticated PlaybackInfo issued for the item, and
// scopes the item to its user (divergence D-54). Optional dependencies
// preserve the two-argument registration seam used by isolated route tests:
// an ItemUserResolver, a playback.UniversalAudioDecisionService, a
// playback.ProgressiveAudioStreamer, and a playback.HLSVideoStreamer. Without
// the decision service, universal audio serves only the direct file.
func RegisterDirectAudioStreams(rt *router.Router, service playback.DirectFileSourceService, dependencies ...any) {
	h := directFileStreamHandler{service: service}
	for _, dependency := range dependencies {
		if value, ok := dependency.(ItemUserResolver); ok {
			h.users = value
		}
		if value, ok := dependency.(playback.UniversalAudioDecisionService); ok {
			h.universal = value
		}
		if value, ok := dependency.(playback.ProgressiveAudioStreamer); ok {
			h.progressiveAudioStreamer = value
		}
		if value, ok := dependency.(playback.HLSVideoStreamer); ok {
			h.hls = value
		}
	}
	for _, route := range []struct {
		method, pattern, operationID string
	}{
		{http.MethodGet, "/Audio/{itemId}/stream", "GetAudioStream"},
		{http.MethodHead, "/Audio/{itemId}/stream", "HeadAudioStream"},
		{http.MethodGet, "/Audio/{itemId}/stream.{container}", "GetAudioStreamByContainer"},
		{http.MethodHead, "/Audio/{itemId}/stream.{container}", "HeadAudioStreamByContainer"},
	} {
		rt.Handle(route.method, route.pattern, router.Route{OperationID: route.operationID, Policy: string(auth.PolicyDefaultAuthorization), PlaySessionCredential: true}, h.audio)
	}
	for _, route := range []struct{ method, operationID string }{
		{http.MethodGet, "GetUniversalAudioStream"},
		{http.MethodHead, "HeadUniversalAudioStream"},
	} {
		rt.Handle(route.method, "/Audio/{itemId}/universal", router.Route{OperationID: route.operationID, Policy: string(auth.PolicyDefaultAuthorization)}, h.universalAudio)
	}
}

// RegisterItemDownloads registers the direct physical-file download route.
// It remains with the Items registration seam because policy metadata and the
// item ID are part of that controller surface.
func RegisterItemDownloads(rt *router.Router, service playback.DownloadFileSourceService) {
	h := directFileStreamHandler{downloadService: service}
	rt.Handle(http.MethodGet, "/Items/{itemId}/Download", router.Route{OperationID: "GetDownload", Policy: string(auth.PolicyDownload)}, h.download)
}

type directFileStreamHandler struct {
	service                  playback.DirectFileSourceService
	downloadService          playback.DownloadFileSourceService
	progressive              playback.ProgressiveVideoStreamer
	progressiveAudioStreamer playback.ProgressiveAudioStreamer
	universal                playback.UniversalAudioDecisionService
	hls                      playback.HLSVideoStreamer
	users                    ItemUserResolver
}

type directFileStreamQuery struct {
	Static        *bool   `query:"static"`
	MediaSourceID *string `query:"mediaSourceId"`
	Container     *string `query:"container"`
	Params        *string `query:"params"`
}

// progressiveVideoStreamQuery is deliberately separate from the physical-file
// DTO: this route forwards the transcoding facts, rather than file transport
// options such as Params or Range.
type progressiveVideoStreamQuery struct {
	Container                   *string  `query:"container"`
	Params                      *string  `query:"params"`
	MediaSourceID               *string  `query:"mediaSourceId"`
	VideoCodec                  *string  `query:"videoCodec"`
	AudioCodec                  *string  `query:"audioCodec"`
	VideoStreamIndex            *int     `query:"videoStreamIndex"`
	AudioStreamIndex            *int     `query:"audioStreamIndex"`
	SubtitleStreamIndex         *int     `query:"subtitleStreamIndex"`
	SubtitleMethod              *string  `query:"subtitleMethod"`
	StartTimeTicks              *int64   `query:"startTimeTicks"`
	EnableAutoStreamCopy        *bool    `query:"enableAutoStreamCopy"`
	AllowVideoStreamCopy        *bool    `query:"allowVideoStreamCopy"`
	AllowAudioStreamCopy        *bool    `query:"allowAudioStreamCopy"`
	RequireAvc                  *bool    `query:"requireAvc"`
	RequireNonAnamorphic        *bool    `query:"requireNonAnamorphic"`
	DeInterlace                 *bool    `query:"deInterlace"`
	Width                       *int     `query:"width"`
	Height                      *int     `query:"height"`
	MaxWidth                    *int     `query:"maxWidth"`
	MaxHeight                   *int     `query:"maxHeight"`
	Framerate                   *float32 `query:"framerate"`
	MaxFramerate                *float32 `query:"maxFramerate"`
	VideoBitrate                *int     `query:"videoBitRate"`
	AudioBitrate                *int     `query:"audioBitRate"`
	Profile                     *string  `query:"profile"`
	Level                       *string  `query:"level"`
	MaxRefFrames                *int     `query:"maxRefFrames"`
	MaxVideoBitDepth            *int     `query:"maxVideoBitDepth"`
	Rotation                    *int     `query:"rotation"`
	MaxAudioChannels            *int     `query:"maxAudioChannels"`
	AudioSampleRate             *int     `query:"audioSampleRate"`
	AudioBitDepth               *int     `query:"maxAudioBitDepth"`
	AudioChannels               *int     `query:"audioChannels"`
	TranscodingMaxAudioChannels *int     `query:"transcodingMaxAudioChannels"`
	CopyTimestamps              *bool    `query:"copyTimestamps"`
	H264Profile                 *string  `query:"h264-profile"`
	H264Level                   *string  `query:"h264-level"`
	H264MaxRefFrames            *int     `query:"h264-maxrefframes"`
	H264VideoBitDepth           *int     `query:"h264-videobitdepth"`
	H264Rotation                *int     `query:"h264-rotation"`
	H264DeInterlace             *bool    `query:"h264-deinterlace"`
	H264RangeType               *string  `query:"h264-rangetype"`
	AC3AudioChannels            *int     `query:"ac3-audiochannels"`
	AC3AudioBitDepth            *int     `query:"ac3-audiobitdepth"`
	DeviceID                    *string  `query:"deviceId"`
	PlaySessionID               *string  `query:"playSessionId"`
}

type universalAudioStreamQuery struct {
	Container           []string  `query:"container"`
	MediaSourceID       *string   `query:"mediaSourceId"`
	UserID              *dto.UUID `query:"userId"`
	MaxStreamingBitrate *int      `query:"maxStreamingBitrate"`
	MaxAudioChannels    *int      `query:"maxAudioChannels"`
	MaxAudioSampleRate  *int      `query:"maxAudioSampleRate"`
	MaxAudioBitDepth    *int      `query:"maxAudioBitDepth"`
}

var (
	directFileRangeSyntax  = regexp.MustCompile(`^bytes=\s*(?:[0-9]+-[0-9]*|-[0-9]+)\s*$`)
	directContainerSyntax  = regexp.MustCompile(`^[a-zA-Z0-9\-\._,|]{0,40}$`)
	progressiveLevelSyntax = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)
)

func (h directFileStreamHandler) video(w http.ResponseWriter, r *http.Request) {
	var mode directFileStreamQuery
	if err := bind.QueryRequest(r, &mode); err != nil {
		badRequest(w)
		return
	}
	if mode.Params == nil {
		mode.Params = packedParamsFromRawQuery(r.URL.RawQuery)
	}
	applyPackedDirectAudioParams(&mode)
	if mode.Static != nil && *mode.Static {
		h.staticVideo(w, r)
		return
	}
	h.progressiveVideo(w, r)
}

// audio serves the static file, or a progressive transcode when the request
// is not static and the transcode path is composed.
func (h directFileStreamHandler) audio(w http.ResponseWriter, r *http.Request) {
	var mode directFileStreamQuery
	if err := bind.QueryRequest(r, &mode); err != nil {
		badRequest(w)
		return
	}
	if mode.Params == nil {
		mode.Params = packedParamsFromRawQuery(r.URL.RawQuery)
	}
	applyPackedDirectAudioParams(&mode)
	if (mode.Static == nil || !*mode.Static) && h.progressiveAudioStreamer != nil {
		h.progressiveAudio(w, r)
		return
	}
	h.staticAudio(w, r)
}

func (h directFileStreamHandler) staticVideo(w http.ResponseWriter, r *http.Request) {
	h.static(w, r, streamUserID(r), true)
}

func (h directFileStreamHandler) staticAudio(w http.ResponseWriter, r *http.Request) {
	h.static(w, r, streamUserID(r), false)
}

// streamUserID selects the user whose library access scopes a stream (D-54).
// A user token selects its user. An API key has no user and keeps the
// unscoped server-wide lookup, as on the other item routes
// (isGlobalAPIKeyItemRequest). The authorizer has already refused a request
// without a credential.
func streamUserID(r *http.Request) dto.UUID {
	identity, ok := middleware.IdentityFromContext(r.Context())
	if !ok || !identity.Authenticated || identity.APIKey {
		return dto.NilUUID
	}
	return identity.UserID
}

func (h directFileStreamHandler) progressiveVideo(w http.ResponseWriter, r *http.Request) {
	itemID, ok := directFileItemID(w, r)
	if !ok {
		return
	}
	if h.progressive == nil {
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}
	var query progressiveVideoStreamQuery
	if err := bind.QueryRequest(r, &query); err != nil {
		badRequest(w)
		return
	}
	if query.Params == nil {
		query.Params = packedParamsFromRawQuery(r.URL.RawQuery)
	}
	subtitleMethod, ok := progressiveSubtitleMethod(w, query.SubtitleMethod)
	if !ok {
		return
	}
	params := progressiveExplicitStreamParams(query, subtitleMethod)
	if query.Params != nil {
		var err error
		params, err = DecodeStreamParams(*query.Params, true, params)
		if err != nil {
			badRequest(w)
			return
		}
	}
	if params.Level != nil && !progressiveLevelSyntax.MatchString(*params.Level) {
		badRequest(w)
		return
	}
	if !directContainerSyntax.MatchString(stringValue(params.VideoCodec)) || !directContainerSyntax.MatchString(stringValue(params.AudioCodec)) {
		badRequest(w)
		return
	}
	container, ok := progressiveVideoContainer(w, r, query.Container)
	if !ok {
		return
	}

	videoCodec, audioCodec := stringValue(params.VideoCodec), stringValue(params.AudioCodec)
	response, err := h.progressive.OpenProgressive(r.Context(), playback.ProgressiveRequest{
		UserID:                      streamUserID(r),
		ItemID:                      itemID,
		MediaSourceID:               stringValue(params.MediaSourceID),
		Container:                   container,
		DeviceID:                    stringValue(params.DeviceID),
		PlaySessionID:               stringValue(params.PlaySessionID),
		UserAgent:                   r.UserAgent(),
		VideoCodec:                  videoCodec,
		AudioCodec:                  audioCodec,
		VideoCodecs:                 progressiveCodecList(videoCodec),
		AudioCodecs:                 progressiveCodecList(audioCodec),
		VideoStreamIndex:            query.VideoStreamIndex,
		AudioStreamIndex:            params.AudioStreamIndex,
		SubtitleStreamIndex:         params.SubtitleStreamIndex,
		SubtitleDeliveryMethod:      params.SubtitleMethod,
		StartTimeTicks:              int64Value(params.StartTimeTicks),
		AllowVideoStreamCopy:        boolDefault(query.AllowVideoStreamCopy, true),
		AllowAudioStreamCopy:        boolDefault(query.AllowAudioStreamCopy, true),
		EnableAutoStreamCopy:        boolDefault(query.EnableAutoStreamCopy, true),
		RequireAvc:                  boolValue(params.RequireAVC),
		RequireNonAnamorphic:        boolValue(params.RequireNonAnamorphic),
		DeInterlace:                 boolValue(params.Deinterlace),
		Width:                       query.Width,
		Height:                      query.Height,
		MaxWidth:                    params.MaxWidth,
		MaxHeight:                   params.MaxHeight,
		MaxFramerate:                params.MaxFramerate,
		Framerate:                   query.Framerate,
		VideoBitrate:                params.VideoBitrate,
		Profile:                     params.Profile,
		Level:                       params.Level,
		MaxRefFrames:                params.MaxRefFrames,
		MaxVideoBitDepth:            params.MaxVideoBitDepth,
		Rotation:                    query.Rotation,
		AudioBitrate:                params.AudioBitrate,
		MaxAudioChannels:            params.MaxAudioChannels,
		AudioChannels:               query.AudioChannels,
		TranscodingMaxAudioChannels: params.TranscodingMaxAudioChannels,
		AudioSampleRate:             query.AudioSampleRate,
		MaxAudioBitDepth:            query.AudioBitDepth,
		CopyTimestamps:              boolValue(params.CopyTimestamps),
		StreamOptions:               progressiveStreamOptions(query),
		Head:                        r.Method == http.MethodHead,
	})
	if err != nil {
		progressiveVideoError(w, err)
		return
	}
	w.Header().Set("Content-Type", response.ContentType)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if response.Body == nil {
		internalError(w)
		return
	}
	defer func() { _ = response.Body.Close() }()
	_ = http.NewResponseController(w).Flush()
	_, _ = io.Copy(w, response.Body)
}

// progressiveExplicitStreamParams collects the query fields that legacy Params
// may replace before the request crosses the progressive-service boundary.
func progressiveExplicitStreamParams(query progressiveVideoStreamQuery, subtitleMethod *playback.SubtitleDeliveryMethod) StreamParams {
	return StreamParams{
		DeviceID:                    query.DeviceID,
		MediaSourceID:               query.MediaSourceID,
		VideoCodec:                  query.VideoCodec,
		AudioCodec:                  query.AudioCodec,
		AudioStreamIndex:            query.AudioStreamIndex,
		SubtitleStreamIndex:         query.SubtitleStreamIndex,
		VideoBitrate:                query.VideoBitrate,
		AudioBitrate:                query.AudioBitrate,
		MaxAudioChannels:            query.MaxAudioChannels,
		MaxFramerate:                query.MaxFramerate,
		MaxWidth:                    query.MaxWidth,
		MaxHeight:                   query.MaxHeight,
		StartTimeTicks:              query.StartTimeTicks,
		Level:                       query.Level,
		MaxRefFrames:                query.MaxRefFrames,
		MaxVideoBitDepth:            query.MaxVideoBitDepth,
		Profile:                     query.Profile,
		PlaySessionID:               query.PlaySessionID,
		CopyTimestamps:              query.CopyTimestamps,
		SubtitleMethod:              subtitleMethod,
		TranscodingMaxAudioChannels: query.TranscodingMaxAudioChannels,
		RequireAVC:                  query.RequireAvc,
		RequireNonAnamorphic:        query.RequireNonAnamorphic,
		Deinterlace:                 query.DeInterlace,
	}
}

func progressiveVideoContainer(w http.ResponseWriter, r *http.Request, queryContainer *string) (string, bool) {
	container := stringValue(queryContainer)
	if container == "" {
		container = chi.URLParam(r, "container")
	}
	container = strings.ToLower(strings.TrimSpace(container))
	if container != "ts" && container != "mp4" && container != "mkv" {
		badRequest(w)
		return "", false
	}
	return container, true
}

func progressiveSubtitleMethod(w http.ResponseWriter, value *string) (*playback.SubtitleDeliveryMethod, bool) {
	var method playback.SubtitleDeliveryMethod
	if value == nil {
		method = playback.SubtitleEncode
		return &method, true
	}
	switch strings.ToLower(strings.TrimSpace(*value)) {
	case "encode":
		method = playback.SubtitleEncode
	case "embed":
		method = playback.SubtitleEmbed
	case "external":
		method = playback.SubtitleExternal
	case "hls":
		method = playback.SubtitleHLS
	case "drop":
		method = playback.SubtitleDrop
	default:
		badRequest(w)
		return nil, false
	}
	return &method, true
}

func progressiveCodecList(codecs string) []string {
	if codecs == "" {
		return nil
	}
	values := strings.Split(codecs, ",")
	result := values[:0]
	for _, codec := range values {
		if codec = strings.TrimSpace(codec); codec != "" {
			result = append(result, codec)
		}
	}
	return result
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func boolValue(value *bool) bool {
	return value != nil && *value
}

func boolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func progressiveStreamOptions(query progressiveVideoStreamQuery) playback.StreamOptions {
	var options playback.StreamOptions
	setProgressiveStringOption(&options, "h264-profile", query.H264Profile)
	setProgressiveStringOption(&options, "h264-level", query.H264Level)
	setProgressiveIntOption(&options, "h264-maxrefframes", query.H264MaxRefFrames)
	setProgressiveIntOption(&options, "h264-videobitdepth", query.H264VideoBitDepth)
	setProgressiveIntOption(&options, "h264-rotation", query.H264Rotation)
	setProgressiveBoolOption(&options, "h264-deinterlace", query.H264DeInterlace)
	setProgressiveStringOption(&options, "h264-rangetype", query.H264RangeType)
	setProgressiveIntOption(&options, "ac3-audiochannels", query.AC3AudioChannels)
	setProgressiveIntOption(&options, "ac3-audiobitdepth", query.AC3AudioBitDepth)
	return options
}

func setProgressiveStringOption(options *playback.StreamOptions, name string, value *string) {
	if value != nil {
		options.Set(name, *value)
	}
}

func setProgressiveIntOption(options *playback.StreamOptions, name string, value *int) {
	if value != nil {
		options.Set(name, strconv.Itoa(*value))
	}
}

func setProgressiveBoolOption(options *playback.StreamOptions, name string, value *bool) {
	if value != nil {
		options.Set(name, strconv.FormatBool(*value))
	}
}

func firstFloat32(primary, fallback *float32) *float32 {
	if primary != nil {
		return primary
	}
	return fallback
}

func progressiveVideoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, playback.ErrMediaSourceNotFound):
		sourceMismatch(w)
	case errors.Is(err, playback.ErrItemNotFound):
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	case errors.Is(err, playback.ErrProgressiveInvalid):
		badRequest(w)
	case errors.Is(err, playback.ErrProgressiveUnsupported):
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
	default:
		internalError(w)
	}
}

func (h directFileStreamHandler) static(w http.ResponseWriter, r *http.Request, userID dto.UUID, rejectDisc bool) {
	itemID, query, ok := directFileRequest(w, r)
	if !ok {
		return
	}
	outputContainer, ok := directOutputContainer(w, r, query)
	if !ok {
		return
	}
	if query.Static == nil || !*query.Static {
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}
	source, ok := h.selectedSource(w, r, userID, itemID, stringValue(query.MediaSourceID))
	if !ok {
		return
	}
	if rejectDisc && isDiscVideo(source) {
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}
	h.serveSource(w, r, source, outputContainer, "")
}

func (h directFileStreamHandler) universalAudio(w http.ResponseWriter, r *http.Request) {
	itemID, ok := directFileItemID(w, r)
	if !ok {
		return
	}
	var query universalAudioStreamQuery
	if err := bind.QueryRequest(r, &query); err != nil {
		badRequest(w)
		return
	}
	if !universalAudioContainersPresent(query.Container) {
		internalError(w)
		return
	}
	requestedUserID := ""
	if query.UserID != nil {
		requestedUserID = query.UserID.Dashed()
	}
	userID := dto.NilUUID
	if !isGlobalAPIKeyItemRequest(r, requestedUserID) {
		user, ok := resolveSelectedUser(w, r, query.UserID, h.users)
		if !ok {
			return
		}
		userID = user.ID
	}
	if h.universal != nil {
		h.universalAudioTranscode(w, r, userID, itemID, query)
		return
	}
	// Without the transcode services, serve only the direct file.
	request := playback.UniversalAudioRequest{Containers: query.Container, MaxStreamingBitrate: query.MaxStreamingBitrate, MaxAudioChannels: query.MaxAudioChannels, MaxAudioSampleRate: query.MaxAudioSampleRate, MaxAudioBitDepth: query.MaxAudioBitDepth}
	var source playback.MediaSourceInfo
	var err error
	if universal, available := h.service.(playback.UniversalAudioSourceService); available {
		source, err = universal.UniversalAudioSource(r.Context(), userID, itemID, stringValue(query.MediaSourceID))
	} else {
		source, err = h.service.StaticSource(r.Context(), userID, itemID, stringValue(query.MediaSourceID))
	}
	if err != nil {
		h.sourceError(w, err)
		return
	}
	decision, err := playback.DecideUniversalAudio(request, source, itemID.Dashed(), nil)
	if err != nil {
		internalError(w)
		return
	}
	if !decision.Static {
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}
	h.serveSource(w, r, decision.Source, "", "")
}

func (h directFileStreamHandler) download(w http.ResponseWriter, r *http.Request) {
	itemID, ok := directFileItemID(w, r)
	if !ok {
		return
	}
	identity, authenticated := middleware.IdentityFromContext(r.Context())
	if !authenticated || !identity.Authenticated {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	if h.downloadService == nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	source, err := h.downloadService.DownloadSource(r.Context(), identity.UserID, itemID)
	if errors.Is(err, playback.ErrDownloadNotAllowed) {
		sourceMismatch(w)
		return
	}
	if err != nil {
		h.sourceError(w, err)
		return
	}
	h.serveSource(w, r, source, "", downloadContentDisposition(source.Path))
}

func directFileRequest(w http.ResponseWriter, r *http.Request) (dto.UUID, directFileStreamQuery, bool) {
	itemID, ok := directFileItemID(w, r)
	if !ok {
		return dto.NilUUID, directFileStreamQuery{}, false
	}
	var query directFileStreamQuery
	if err := bind.QueryRequest(r, &query); err != nil {
		badRequest(w)
		return dto.NilUUID, directFileStreamQuery{}, false
	}
	if query.Params == nil {
		query.Params = packedParamsFromRawQuery(r.URL.RawQuery)
	}
	applyPackedDirectAudioParams(&query)
	return itemID, query, true
}

func applyPackedDirectAudioParams(query *directFileStreamQuery) {
	if query.Params == nil {
		return
	}
	slots := strings.Split(*query.Params, ";")
	if len(slots) > 2 && strings.TrimSpace(slots[2]) != "" {
		query.MediaSourceID = &slots[2]
	}
	if len(slots) > 3 && strings.TrimSpace(slots[3]) != "" {
		value := strings.EqualFold(slots[3], "true")
		query.Static = &value
	}
}

func packedParamsFromRawQuery(raw string) *string {
	for _, part := range strings.Split(raw, "&") {
		key, value, found := strings.Cut(part, "=")
		if !found || !strings.EqualFold(key, "params") {
			continue
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			return nil
		}
		return &decoded
	}
	return nil
}

func directOutputContainer(w http.ResponseWriter, r *http.Request, query directFileStreamQuery) (string, bool) {
	container := chi.URLParam(r, "container")
	if container == "" && query.Container != nil {
		container = *query.Container
	}
	if !directContainerSyntax.MatchString(container) {
		badRequest(w)
		return "", false
	}
	return container, true
}

func directFileItemID(w http.ResponseWriter, r *http.Request) (dto.UUID, bool) {
	itemID, err := dto.ParseUUID(chi.URLParam(r, "itemId"))
	if err != nil {
		badRequest(w)
		return dto.NilUUID, false
	}
	return itemID, true
}

func (h directFileStreamHandler) selectedSource(w http.ResponseWriter, r *http.Request, userID, itemID dto.UUID, mediaSourceID string) (playback.MediaSourceInfo, bool) {
	if h.service == nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return playback.MediaSourceInfo{}, false
	}
	source, err := h.service.StaticSource(r.Context(), userID, itemID, mediaSourceID)
	if err != nil {
		h.sourceError(w, err)
		return playback.MediaSourceInfo{}, false
	}
	return source, true
}

func (h directFileStreamHandler) sourceError(w http.ResponseWriter, err error) {
	if errors.Is(err, playback.ErrMediaSourceNotFound) {
		sourceMismatch(w)
		return
	}
	if errors.Is(err, playback.ErrItemNotFound) {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	internalError(w)
}

func sourceMismatch(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", "25")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, "Error processing request.")
}

func (h directFileStreamHandler) serveSource(w http.ResponseWriter, r *http.Request, source playback.MediaSourceInfo, outputContainer, disposition string) {
	if source.Protocol != playback.MediaProtocolFile || source.Path == "" {
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}
	file, err := os.Open(source.Path)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		internalError(w)
		return
	}
	if !info.Mode().IsRegular() {
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}

	stripMalformedOrMultipleRanges(r)
	contentType := directFileContentType(outputContainer, source.Container, source.Path)
	lastModified := ""
	if !info.ModTime().IsZero() {
		lastModified = info.ModTime().UTC().Format(http.TimeFormat)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", contentType)
	if disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	http.ServeContent(&directFileResponseWriter{ResponseWriter: w, contentType: contentType, lastModified: lastModified}, r, info.Name(), info.ModTime(), io.NewSectionReader(file, 0, info.Size()))
}

func universalAudioContainersPresent(containers []string) bool {
	for _, containers := range containers {
		if strings.TrimSpace(containers) != "" {
			return true
		}
	}
	return false
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func downloadContentDisposition(path string) string {
	name := safeDispositionFilename(filepath.Base(path))
	return `attachment; filename="` + asciiDispositionFilename(name) + `"; filename*=UTF-8''` + rfc5987Filename(name)
}

func safeDispositionFilename(name string) string {
	var output strings.Builder
	for _, character := range name {
		if character == '"' || character == '\r' || character == '\n' || character < 0x20 || character == 0x7f {
			continue
		}
		output.WriteRune(character)
	}
	if output.Len() == 0 {
		return "download"
	}
	return output.String()
}

func asciiDispositionFilename(name string) string {
	var output strings.Builder
	for _, character := range name {
		switch {
		case character == '\\':
			output.WriteString(`\\`)
		case character > 0x7e:
			output.WriteByte('?')
		default:
			output.WriteRune(character)
		}
	}
	return output.String()
}

func rfc5987Filename(name string) string {
	var output strings.Builder
	for _, value := range []byte(name) {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || strings.ContainsRune("!#$&+-.^_`|~", rune(value)) {
			output.WriteByte(value)
			continue
		}
		const hex = "0123456789ABCDEF"
		output.WriteByte('%')
		output.WriteByte(hex[value>>4])
		output.WriteByte(hex[value&0x0f])
	}
	return output.String()
}

// stripMalformedOrMultipleRanges preserves Go's standard handling of valid
// ranges while matching the upstream physical-file endpoint's forgiving
// treatment of malformed headers and repeated Range fields.
func stripMalformedOrMultipleRanges(r *http.Request) {
	ranges := r.Header.Values("Range")
	if len(ranges) != 1 || !directFileRangeSyntax.MatchString(ranges[0]) {
		r.Header.Del("Range")
	}
}

// directFileResponseWriter retains ServeContent's range parser and status
// choices while matching PhysicalFile's empty 416 response body.
type directFileResponseWriter struct {
	http.ResponseWriter
	contentType  string
	lastModified string
	discardBody  bool
}

func (w *directFileResponseWriter) WriteHeader(status int) {
	if status == http.StatusRequestedRangeNotSatisfiable {
		w.Header().Del("X-Content-Type-Options")
		w.Header().Set("Content-Type", w.contentType)
		if w.lastModified != "" {
			w.Header().Set("Last-Modified", w.lastModified)
		}
		w.Header().Set("Content-Length", "0")
		w.discardBody = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *directFileResponseWriter) Write(value []byte) (int, error) {
	if w.discardBody {
		return len(value), nil
	}
	return w.ResponseWriter.Write(value)
}

// ReadFrom preserves a downstream response writer's streaming fast path while
// consuming the body locally for an intentionally empty 416 response.
func (w *directFileResponseWriter) ReadFrom(source io.Reader) (int64, error) {
	if w.discardBody {
		return io.Copy(io.Discard, source)
	}
	if downstream, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return downstream.ReadFrom(source)
	}
	return io.Copy(w.ResponseWriter, source)
}

// Unwrap exposes the original response writer to net/http response controls.
func (w *directFileResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func isDiscVideo(source playback.MediaSourceInfo) bool {
	return source.VideoType != nil && (*source.VideoType == playback.VideoTypeDVD || *source.VideoType == playback.VideoTypeBluRay)
}

func directFileContentType(outputContainer, sourceContainer, path string) string {
	return directFileContentTypeWithLookup(outputContainer, sourceContainer, path, mime.TypeByExtension)
}

func directFileContentTypeWithLookup(outputContainer, sourceContainer, path string, typeByExtension func(string) string) string {
	container := strings.TrimSpace(outputContainer)
	if container == "" {
		container = strings.TrimSpace(sourceContainer)
	}
	if index := strings.IndexByte(container, ','); index >= 0 {
		container = container[:index]
	}
	container = strings.TrimPrefix(container, ".")
	if strings.EqualFold(container, "flac") {
		return "audio/flac"
	}
	if container != "" {
		if contentType := typeByExtension("." + container); contentType != "" {
			return contentType
		}
	}
	extension := filepath.Ext(path)
	if strings.EqualFold(extension, ".flac") {
		return "audio/flac"
	}
	if contentType := typeByExtension(extension); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}
