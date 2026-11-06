// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/edge"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/store"
)

// RegisterPlaybackInfo registers Jellyfin's playback-info and live-stream
// transport surface. Route wrappers enforce the declared policy.
//
// When grants is supplied, every PlaySessionId that PlaybackInfo issues for a
// user becomes a direct-stream credential for that item (D-54).
func RegisterPlaybackInfo(rt *router.Router, service playback.PlaybackInfoService, users ItemUserResolver, grants ...PlaySessionIssuer) {
	var issuer PlaySessionIssuer
	if len(grants) != 0 {
		issuer = grants[0]
	}
	RegisterPlaybackInfoWithOptions(rt, service, users, PlaybackInfoOptions{Grants: issuer})
}

// RegisterPlaybackInfoWithEdge is RegisterPlaybackInfo with the stream edge
// (#621): when edge is not nil, a successful PlaybackInfo response carries
// the direct grants of its sources in the edge.DirectHeader header.
func RegisterPlaybackInfoWithEdge(rt *router.Router, service playback.PlaybackInfoService, users ItemUserResolver, grants PlaySessionIssuer, edge PlaybackInfoEdge) {
	RegisterPlaybackInfoWithOptions(rt, service, users, PlaybackInfoOptions{Grants: grants, Edge: edge})
}

// PlaybackInfoOptions are the optional collaborators of the PlaybackInfo
// routes. Each nil field is off.
type PlaybackInfoOptions struct {
	Grants PlaySessionIssuer
	Edge   PlaybackInfoEdge
	// Warmup learns of each response that announces a transcode (#623).
	Warmup PlaybackInfoWarmup
}

// PlaybackInfoWarmup is told the PlaySessionId of a PlaybackInfo response
// with a TranscodingUrl, so that an outside scaler can start a worker before
// the client asks for the stream (#623).
type PlaybackInfoWarmup interface {
	Anticipate(playSessionID string)
}

// RegisterPlaybackInfoWithOptions is RegisterPlaybackInfo with all optional
// collaborators.
func RegisterPlaybackInfoWithOptions(rt *router.Router, service playback.PlaybackInfoService, users ItemUserResolver, options PlaybackInfoOptions) {
	h := playbackInfoHandler{service: service, users: users, grants: options.Grants, edge: options.Edge, warmup: options.Warmup}
	rt.Handle(http.MethodGet, "/Items/{itemId}/PlaybackInfo", router.Route{OperationID: "GetPlaybackInfo", Policy: string(auth.PolicyDefaultAuthorization)}, h.getPlaybackInfo)
	rt.Handle(http.MethodPost, "/Items/{itemId}/PlaybackInfo", router.Route{OperationID: "GetPostedPlaybackInfo", Policy: string(auth.PolicyDefaultAuthorization)}, h.postPlaybackInfo)
	rt.Handle(http.MethodPost, "/LiveStreams/Open", router.Route{OperationID: "OpenLiveStream", Policy: string(auth.PolicyDefaultAuthorization)}, h.openLiveStream)
	rt.Handle(http.MethodPost, "/LiveStreams/Close", router.Route{OperationID: "CloseLiveStream", Policy: string(auth.PolicyDefaultAuthorization)}, h.closeLiveStream)
	rt.Handle(http.MethodGet, "/Playback/BitrateTest", router.Route{OperationID: "GetBitrateTestBytes", Policy: string(auth.PolicyDefaultAuthorization)}, h.getBitrateTest)
}

type playbackInfoHandler struct {
	service playback.PlaybackInfoService
	users   ItemUserResolver
	grants  PlaySessionIssuer
	edge    PlaybackInfoEdge
	warmup  PlaybackInfoWarmup
}

// PlaybackInfoEdge signs the stream edge grants of the static sources of a
// PlaybackInfo response (#621). playSessionID is empty when the caller has
// no user; such grants are not PlaySessionId credentials.
type PlaybackInfoEdge interface {
	DirectGrants(itemID dto.UUID, playSessionID string, sources []playback.MediaSourceInfo) []string
}

// PlaySessionIssuer records a PlaySessionId grant (D-54).
type PlaySessionIssuer interface {
	Issue(playSessionID string, userID, itemID dto.UUID, mediaSourceIDs []string)
}

// issueGrant lets the PlaySessionId of a successful user PlaybackInfo stream
// the returned sources of itemID. An API key without a selected user issues
// no grant; such a caller sends its key on the stream URL.
// edgeGrants adds the stream edge grants of response to w.
func (h playbackInfoHandler) edgeGrants(w http.ResponseWriter, itemID dto.UUID, request playback.PlaybackInfoRequest, response playback.PlaybackInfoResponse) {
	if h.edge == nil || response.ErrorCode != "" {
		return
	}
	playSessionID := response.PlaySessionID
	if request.UserID.IsNil() {
		playSessionID = ""
	}
	for _, token := range h.edge.DirectGrants(itemID, playSessionID, response.MediaSources) {
		w.Header().Add(edge.DirectHeader, token)
	}
}

// announce tells the warm-up of a response that announces a transcode.
func (h playbackInfoHandler) announce(response playback.PlaybackInfoResponse) {
	if h.warmup == nil || response.ErrorCode != "" || response.PlaySessionID == "" {
		return
	}
	for _, source := range response.MediaSources {
		if source.TranscodingURL != "" {
			h.warmup.Anticipate(response.PlaySessionID)
			return
		}
	}
}

func (h playbackInfoHandler) issueGrant(itemID dto.UUID, request playback.PlaybackInfoRequest, response playback.PlaybackInfoResponse) {
	if h.grants == nil || response.ErrorCode != "" || response.PlaySessionID == "" || request.UserID.IsNil() {
		return
	}
	sources := make([]string, 0, len(response.MediaSources))
	for _, source := range response.MediaSources {
		sources = append(sources, source.ID)
	}
	h.grants.Issue(response.PlaySessionID, request.UserID, itemID, sources)
}

type playbackInfoQuery struct {
	UserID               *dto.UUID `query:"userId"`
	MaxStreamingBitrate  *int      `query:"maxStreamingBitrate"`
	StartTimeTicks       *int64    `query:"startTimeTicks"`
	AudioStreamIndex     *int      `query:"audioStreamIndex"`
	SubtitleStreamIndex  *int      `query:"subtitleStreamIndex"`
	MaxAudioChannels     *int      `query:"maxAudioChannels"`
	MediaSourceID        *string   `query:"mediaSourceId"`
	LiveStreamID         *string   `query:"liveStreamId"`
	AutoOpenLiveStream   *bool     `query:"autoOpenLiveStream"`
	EnableDirectPlay     *bool     `query:"enableDirectPlay"`
	EnableDirectStream   *bool     `query:"enableDirectStream"`
	EnableTranscoding    *bool     `query:"enableTranscoding"`
	AllowVideoStreamCopy *bool     `query:"allowVideoStreamCopy"`
	AllowAudioStreamCopy *bool     `query:"allowAudioStreamCopy"`
	DeviceID             *string   `query:"deviceId"`
}

type playbackInfoBody struct {
	UserID                              *dto.UUID               `json:"UserId"`
	MaxStreamingBitrate                 *int                    `json:"MaxStreamingBitrate"`
	StartTimeTicks                      *int64                  `json:"StartTimeTicks"`
	AudioStreamIndex                    *int                    `json:"AudioStreamIndex"`
	SubtitleStreamIndex                 *int                    `json:"SubtitleStreamIndex"`
	MaxAudioChannels                    *int                    `json:"MaxAudioChannels"`
	MediaSourceID                       *string                 `json:"MediaSourceId"`
	LiveStreamID                        *string                 `json:"LiveStreamId"`
	AutoOpenLiveStream                  *bool                   `json:"AutoOpenLiveStream"`
	EnableDirectPlay                    *bool                   `json:"EnableDirectPlay"`
	EnableDirectStream                  *bool                   `json:"EnableDirectStream"`
	EnableTranscoding                   *bool                   `json:"EnableTranscoding"`
	AllowVideoStreamCopy                *bool                   `json:"AllowVideoStreamCopy"`
	AllowAudioStreamCopy                *bool                   `json:"AllowAudioStreamCopy"`
	AlwaysBurnInSubtitleWhenTranscoding *bool                   `json:"AlwaysBurnInSubtitleWhenTranscoding"`
	DeviceProfile                       *playback.DeviceProfile `json:"DeviceProfile"`
	DeviceID                            *string                 `json:"DeviceId"`
}

type bitrateTestSize struct {
	value   int
	raw     string
	present bool
}

// BindQueryEmpty asks bind.QueryRequest to validate an explicitly empty size.
func (bitrateTestSize) BindQueryEmpty() {}

func (s *bitrateTestSize) UnmarshalText(text []byte) error {
	s.raw = string(text)
	s.present = true
	value, err := strconv.ParseInt(s.raw, 10, 32)
	if err != nil {
		return err
	}
	s.value = int(value)
	return nil
}

type bitrateTestQuery struct {
	Size bitrateTestSize `query:"size"`
}

func (h playbackInfoHandler) getPlaybackInfo(w http.ResponseWriter, r *http.Request) {
	itemID, ok := routeUUID(w, r, "itemId")
	if !ok {
		return
	}
	var query playbackInfoQuery
	if err := bind.QueryRequest(r, &query); err != nil {
		badRequest(w)
		return
	}
	request := playback.PlaybackInfoRequest{ItemID: itemID, AssemblyOnly: true}
	if user, selected := h.resolveUser(w, r, query.UserID); !selected {
		return
	} else if h.users != nil {
		request.UserID = user.ID
	}
	applyPlaybackIdentity(r, h.users != nil || query.UserID != nil, false, &request)
	response, err := h.service.PlaybackInfo(r.Context(), request)
	if err != nil {
		playbackError(w, err)
		return
	}
	h.issueGrant(itemID, request, response)
	h.edgeGrants(w, itemID, request, response)
	h.announce(response)
	_ = render.JSON(w, r, http.StatusOK, response)
}

func (h playbackInfoHandler) postPlaybackInfo(w http.ResponseWriter, r *http.Request) {
	itemID, ok := routeUUID(w, r, "itemId")
	if !ok {
		return
	}
	var query playbackInfoQuery
	if err := bind.QueryRequest(r, &query); err != nil {
		badRequest(w)
		return
	}
	var body playbackInfoBody
	if err := bind.JSONBody(r, &body); err != nil {
		badRequest(w)
		return
	}
	request := mergePlaybackInfo(itemID, query, body)
	if user, selected := h.resolveUser(w, r, first(query.UserID, body.UserID)); !selected {
		return
	} else if h.users != nil {
		request.UserID = user.ID
	}
	applyPlaybackIdentity(r, h.users != nil || query.UserID != nil || body.UserID != nil, query.DeviceID != nil || body.DeviceID != nil, &request)
	response, err := h.service.PlaybackInfo(r.Context(), request)
	if err != nil {
		playbackError(w, err)
		return
	}
	h.issueGrant(itemID, request, response)
	h.edgeGrants(w, itemID, request, response)
	h.announce(response)
	_ = render.JSON(w, r, http.StatusOK, response)
}

func (h playbackInfoHandler) openLiveStream(w http.ResponseWriter, r *http.Request) {
	var body openLiveStreamBody
	if err := bind.JSONBody(r, &body); err != nil {
		badRequest(w)
		return
	}
	request := body.request()
	if user, selected := h.resolveUser(w, r, body.UserID); !selected {
		return
	} else if h.users != nil {
		request.UserID = user.ID
	}
	if identity, ok := middleware.IdentityFromContext(r.Context()); ok {
		request.DeviceID = identity.DeviceID
	}
	response, err := h.service.OpenLiveStream(r.Context(), request)
	if err != nil {
		if errors.Is(err, playback.ErrLiveStreamUnavailable) {
			liveStreamUnavailable(w)
		} else if errors.Is(err, playback.ErrItemNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		} else {
			internalError(w)
		}
		return
	}
	_ = render.JSON(w, r, http.StatusOK, response)
}

func (h playbackInfoHandler) closeLiveStream(w http.ResponseWriter, r *http.Request) {
	var query struct {
		LiveStreamID string `query:"liveStreamId"`
	}
	if err := bind.QueryRequest(r, &query); err != nil || query.LiveStreamID == "" {
		badRequest(w)
		return
	}
	if err := h.service.CloseLiveStream(r.Context(), query.LiveStreamID); err != nil {
		internalError(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func routeUUID(w http.ResponseWriter, r *http.Request, name string) (dto.UUID, bool) {
	id, err := dto.ParseUUID(chi.URLParam(r, name))
	if err != nil {
		badRequest(w)
		return dto.NilUUID, false
	}
	return id, true
}

func badRequest(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
}

func internalError(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

func playbackError(w http.ResponseWriter, err error) {
	if errors.Is(err, playback.ErrItemNotFound) {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	internalError(w)
}

func liveStreamUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte("Error processing request."))
}

func (h playbackInfoHandler) resolveUser(w http.ResponseWriter, r *http.Request, requested *dto.UUID) (store.User, bool) {
	if h.users == nil {
		return store.User{}, true
	}
	return resolveSelectedUser(w, r, requested, h.users)
}

func applyPlaybackIdentity(r *http.Request, hasUser, hasDevice bool, request *playback.PlaybackInfoRequest) {
	request.ClientAddress = middleware.ClientAddressFromContext(r.Context())
	if !request.ClientAddress.IsValid() {
		request.ClientAddress = middleware.NormalizeClientAddress(r.RemoteAddr)
	}
	identity, ok := middleware.IdentityFromContext(r.Context())
	if !ok {
		return
	}
	request.Client = identity.Client
	request.Session.DeviceID = identity.DeviceID
	request.Session.AccessToken = identity.Token
	if !hasUser {
		request.UserID = identity.UserID
	}
	if !hasDevice {
		request.DeviceID = identity.DeviceID
	}
}

func mergePlaybackInfo(itemID dto.UUID, q playbackInfoQuery, b playbackInfoBody) playback.PlaybackInfoRequest {
	request := playback.PlaybackInfoRequest{ItemID: itemID, MaxStreamingBitrate: first(q.MaxStreamingBitrate, b.MaxStreamingBitrate), StartTimeTicks: first(q.StartTimeTicks, b.StartTimeTicks), AudioStreamIndex: first(q.AudioStreamIndex, b.AudioStreamIndex), SubtitleStreamIndex: first(q.SubtitleStreamIndex, b.SubtitleStreamIndex), MaxAudioChannels: first(q.MaxAudioChannels, b.MaxAudioChannels), MediaSourceID: firstString(q.MediaSourceID, b.MediaSourceID), LiveStreamID: firstString(q.LiveStreamID, b.LiveStreamID), AutoOpenLiveStream: firstBool(q.AutoOpenLiveStream, b.AutoOpenLiveStream, false), EnableDirectPlay: firstBool(q.EnableDirectPlay, b.EnableDirectPlay, true), EnableDirectStream: firstBool(q.EnableDirectStream, b.EnableDirectStream, true), EnableTranscoding: firstBool(q.EnableTranscoding, b.EnableTranscoding, true), AllowVideoStreamCopy: firstBool(q.AllowVideoStreamCopy, b.AllowVideoStreamCopy, true), AllowAudioStreamCopy: firstBool(q.AllowAudioStreamCopy, b.AllowAudioStreamCopy, true), AlwaysBurnInSubtitleWhenTranscoding: firstBool(nil, b.AlwaysBurnInSubtitleWhenTranscoding, false), DeviceProfile: b.DeviceProfile, DeviceID: firstString(q.DeviceID, b.DeviceID)}
	if user := first(q.UserID, b.UserID); user != nil {
		request.UserID = *user
	}
	return request
}

func first[T any](left, right *T) *T {
	if left != nil {
		return left
	}
	return right
}

func firstString(left, right *string) string {
	if value := first(left, right); value != nil {
		return *value
	}
	return ""
}

func firstBool(left, right *bool, fallback bool) bool {
	if value := first(left, right); value != nil {
		return *value
	}
	return fallback
}

type openLiveStreamBody struct {
	OpenToken                           *string                 `json:"OpenToken"`
	UserID                              *dto.UUID               `json:"UserId"`
	PlaySessionID                       *string                 `json:"PlaySessionId"`
	MaxStreamingBitrate                 *int                    `json:"MaxStreamingBitrate"`
	StartTimeTicks                      *int64                  `json:"StartTimeTicks"`
	AudioStreamIndex                    *int                    `json:"AudioStreamIndex"`
	SubtitleStreamIndex                 *int                    `json:"SubtitleStreamIndex"`
	MaxAudioChannels                    *int                    `json:"MaxAudioChannels"`
	ItemID                              *dto.UUID               `json:"ItemId"`
	DeviceProfile                       *playback.DeviceProfile `json:"DeviceProfile"`
	EnableDirectPlay                    *bool                   `json:"EnableDirectPlay"`
	EnableDirectStream                  *bool                   `json:"EnableDirectStream"`
	AlwaysBurnInSubtitleWhenTranscoding *bool                   `json:"AlwaysBurnInSubtitleWhenTranscoding"`
}

func (b openLiveStreamBody) request() playback.OpenLiveStreamRequest {
	r := playback.OpenLiveStreamRequest{OpenToken: firstString(b.OpenToken, nil), PlaySessionID: firstString(b.PlaySessionID, nil), MaxStreamingBitrate: b.MaxStreamingBitrate, StartTimeTicks: b.StartTimeTicks, AudioStreamIndex: b.AudioStreamIndex, SubtitleStreamIndex: b.SubtitleStreamIndex, MaxAudioChannels: b.MaxAudioChannels, DeviceProfile: b.DeviceProfile, EnableDirectPlay: firstBool(b.EnableDirectPlay, nil, true), EnableDirectStream: firstBool(b.EnableDirectStream, nil, true), AlwaysBurnInSubtitleWhenTranscoding: firstBool(b.AlwaysBurnInSubtitleWhenTranscoding, nil, false)}
	if b.UserID != nil {
		r.UserID = *b.UserID
	}
	if b.ItemID != nil {
		r.ItemID = *b.ItemID
	}
	return r
}

const (
	bitrateDefaultSize      = 102400
	bitrateMinimumSize      = 1
	bitrateMaximumSize      = 100_000_000
	bitrateStreamBufferSize = 64 * 1024
	bitrateMinPayloadSize   = 16
)

// getBitrateTest serves GET /Playback/BitrateTest (GetBitrateTestBytes).
// It binds the optional size query parameter, validates the inclusive range,
// and streams a random byte body. The validation bodies remain byte-controlled
// ASP.NET ProblemDetails templates because the corpus preserves their bytes.
func (h playbackInfoHandler) getBitrateTest(w http.ResponseWriter, r *http.Request) {
	query := bitrateTestQuery{}
	if err := bind.QueryRequest(r, &query); err != nil {
		raw := query.Size.raw
		writeBitrateValidationProblem(w, bitrateUnparseableMessage(raw))
		return
	}
	size := bitrateDefaultSize
	if query.Size.present {
		size = query.Size.value
	}
	if size < bitrateMinimumSize || size > bitrateMaximumSize {
		writeBitrateValidationProblem(w, bitrateOutOfRangeMessage)
		return
	}

	length := bitratePayloadLength(size)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(length))
	w.WriteHeader(http.StatusOK)

	var buffer [bitrateStreamBufferSize]byte
	remaining := length
	for remaining > 0 {
		chunk := len(buffer)
		if chunk > remaining {
			chunk = remaining
		}
		rand.Read(buffer[:chunk])
		if _, err := w.Write(buffer[:chunk]); err != nil {
			return
		}
		remaining -= chunk
	}
}

// bitratePayloadLength returns the response body length for a requested
// size. The rule max(16, next power of two >= size) reproduces every
// captured pair (102400->131072, 1->16, 1024->1024, 1000->1024,
// 100000000->134217728) and mirrors ArrayPool<byte>.Shared.Rent bucket
// sizing. Only the captured pairs are contractual; a contradicting
// capture wins.
func bitratePayloadLength(size int) int {
	if size < bitrateMinPayloadSize {
		return bitrateMinPayloadSize
	}
	power := bitrateMinPayloadSize
	for power < size {
		power *= 2
	}
	return power
}

const bitrateOutOfRangeMessage = "The requested size must be greater than or equal to 1 and less than or equal to 100000000"

func bitrateUnparseableMessage(raw string) string {
	return "The value '" + raw + "' is not valid."
}

// bitrateProblemString returns message as a complete JSON string literal,
// quotes included. json.Marshal escapes '"', '\\', control characters and
// <>&; the apostrophe becomes \u0027 like the System.Text.Json default
// encoder.
func bitrateProblemString(message string) string {
	escaped, err := json.Marshal(message)
	if err != nil {
		return `""`
	}
	return strings.ReplaceAll(string(escaped), "'", `\u0027`)
}

// writeBitrateValidationProblem emits the exact ASP.NET ProblemDetails
// byte template with two substitutions: the JSON-encoded message and a
// per-request W3C traceparent traceId. The traceId is a fixed 55-byte
// form (00-<32 hex>-<16 hex>-00) so the preserved Content-Length compares
// equal to stock. Do not marshal through render.JSON: casing negotiation
// would recase the fixed lowercase keys and the charset/escaping would
// drift. The names are distinct from the plugins.go problemDetails and
// renderProblem so merges stay order-independent.
func writeBitrateValidationProblem(w http.ResponseWriter, message string) {
	traceID := generateTraceID()
	body := []byte(`{"type":"https://tools.ietf.org/html/rfc9110#section-15.5.1","title":"One or more validation errors occurred.","status":400,"errors":{"size":[` +
		bitrateProblemString(message) +
		`]},"traceId":"` +
		traceID +
		`"}`)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(body)
}

// generateTraceID returns a W3C traceparent-form identifier:
// 00-<32 lowercase hex>-<16 lowercase hex>-00 (55 bytes), matching stock's
// per-request Activity ID length so the preserved Content-Length
// compares equal.
func generateTraceID() string {
	var trace [24]byte
	rand.Read(trace[:])
	return "00-" +
		hex.EncodeToString(trace[:16]) +
		"-" +
		hex.EncodeToString(trace[16:]) +
		"-00"
}
