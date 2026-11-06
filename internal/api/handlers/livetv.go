// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"net/http"

	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
)

// RegisterLiveTv registers the LiveTv surface. admin serves the tuner,
// listing provider and channel mapping administration; reads serve the
// channel and programme routes. The recording and timer routes report that
// Deadfin has no DVR until recording lands (#541, #714).
func RegisterLiveTv(rt *router.Router, admin LiveTvAdmin, reads LiveTvReads) {
	h := liveTvStub{}
	access := string(auth.PolicyLiveTVAccess)
	manage := string(auth.PolicyLiveTVManagement)
	elevated := string(auth.PolicyRequiresElevation)

	for _, route := range []struct {
		method      string
		pattern     string
		operationID string
	}{
		{http.MethodGet, "/LiveTv/Recordings", "GetRecordings"},
		{http.MethodGet, "/LiveTv/Recordings/Folders", "GetRecordingFolders"},
		{http.MethodGet, "/LiveTv/SeriesTimers", "GetSeriesTimers"},
		{http.MethodGet, "/LiveTv/Timers", "GetTimers"},
	} {
		rt.Handle(route.method, route.pattern, router.Route{OperationID: route.operationID, Policy: access}, h.emptyQueryResult)
	}

	for _, route := range []struct {
		pattern     string
		operationID string
		policy      string
	}{
		{"/LiveTv/Tuners/Discover", "DiscoverTuners", elevated},
		{"/LiveTv/Tuners/Discvover", "DiscvoverTuners", elevated},
	} {
		rt.Handle(http.MethodGet, route.pattern, router.Route{OperationID: route.operationID, Policy: route.policy}, h.emptyArray)
	}

	rt.Handle(http.MethodGet, "/LiveTv/ListingProviders/Default", router.Route{OperationID: "GetDefaultListingProvider", Policy: access}, h.defaultListingProvider)
	rt.Handle(http.MethodGet, "/LiveTv/Timers/{timerId}", router.Route{OperationID: "GetTimer", Policy: access}, h.emptyObject)
	rt.Handle(http.MethodGet, "/LiveTv/Timers/Defaults", router.Route{OperationID: "GetDefaultTimer", Policy: access}, h.emptyObject)
	rt.Handle(http.MethodGet, "/LiveTv/ListingProviders/SchedulesDirect/Countries", router.Route{OperationID: "GetSchedulesDirectCountries", Policy: elevated}, h.emptyObject)

	registerLiveTvAdmin(rt, admin, access, elevated)
	registerLiveTvReads(rt, reads, access)

	// A 404 is observable on these OpenAPI operations. Report the deferred
	// no-tuner state instead of pretending that an item or stream exists.
	for _, route := range []struct {
		method      string
		pattern     string
		operationID string
		policy      string
		anonymous   bool
	}{
		{http.MethodGet, "/LiveTv/Recordings/{recordingId}", "GetRecording", access, false},
		{http.MethodDelete, "/LiveTv/Recordings/{recordingId}", "DeleteRecording", manage, false},
		{http.MethodGet, "/LiveTv/SeriesTimers/{timerId}", "GetSeriesTimer", access, false},
		{http.MethodGet, "/LiveTv/LiveRecordings/{recordingId}/stream", "GetLiveRecordingFile", "", true},
	} {
		rt.Handle(route.method, route.pattern, router.Route{
			OperationID: route.operationID,
			Policy:      route.policy,
			Anonymous:   route.anonymous,
		}, h.notConfigured)
	}

	// Deadfin has no DVR, so no timer exists and none can be stored (D-122).
	// Upstream reports an unknown timer with ResourceNotFoundException, a 404,
	// and a refused recording with ArgumentException, a 400. Report the same
	// errors instead of a 204 that tells a client a recording is scheduled.
	for _, route := range []struct {
		method      string
		pattern     string
		operationID string
		handler     http.HandlerFunc
	}{
		{http.MethodPost, "/LiveTv/SeriesTimers", "CreateSeriesTimer", h.noDvr},
		{http.MethodDelete, "/LiveTv/SeriesTimers/{timerId}", "CancelSeriesTimer", h.timerNotFound},
		{http.MethodPost, "/LiveTv/SeriesTimers/{timerId}", "UpdateSeriesTimer", h.timerNotFound},
		{http.MethodPost, "/LiveTv/Timers", "CreateTimer", h.noDvr},
		{http.MethodDelete, "/LiveTv/Timers/{timerId}", "CancelTimer", h.timerNotFound},
		{http.MethodPost, "/LiveTv/Timers/{timerId}", "UpdateTimer", h.timerNotFound},
	} {
		rt.Handle(route.method, route.pattern, router.Route{OperationID: route.operationID, Policy: manage}, route.handler)
	}

	rt.Handle(http.MethodPost, "/LiveTv/Tuners/{tunerId}/Reset", router.Route{OperationID: "ResetTuner", Policy: elevated}, h.noContent)
}

type liveTvStub struct{}

type liveTvInfo struct {
	Services     []struct{} `json:"Services"`
	IsEnabled    bool       `json:"IsEnabled"`
	EnabledUsers []string   `json:"EnabledUsers"`
}

type deferredProblem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func (liveTvStub) info(w http.ResponseWriter, r *http.Request) {
	_ = render.JSON(w, r, http.StatusOK, liveTvInfo{
		Services:     []struct{}{},
		EnabledUsers: []string{},
	})
}

func (liveTvStub) emptyQueryResult(w http.ResponseWriter, r *http.Request) {
	_ = render.JSON(w, r, http.StatusOK, dto.NewQueryResult[struct{}]())
}

func (liveTvStub) emptyArray(w http.ResponseWriter, r *http.Request) {
	_ = render.JSON(w, r, http.StatusOK, []struct{}{})
}

func (liveTvStub) emptyObject(w http.ResponseWriter, r *http.Request) {
	_ = render.JSON(w, r, http.StatusOK, struct{}{})
}

// defaultListingProvider ports GetDefaultListingProvider: a new
// ListingsProviderInfo with its constructor defaults.
func (liveTvStub) defaultListingProvider(w http.ResponseWriter, r *http.Request) {
	_ = render.JSON(w, r, http.StatusOK, config.DefaultListingsProviderInfo())
}

func (liveTvStub) notConfigured(w http.ResponseWriter, r *http.Request) {
	_ = render.JSON(w, r, http.StatusNotFound, deferredProblem{
		Title:  http.StatusText(http.StatusNotFound),
		Status: http.StatusNotFound,
		Detail: "no tuners configured",
	})
}

// noDvr refuses a new timer as upstream's exception middleware reports an
// ArgumentException: 400 with its production text body.
func (liveTvStub) noDvr(w http.ResponseWriter, _ *http.Request) {
	writeLiveTvTimerError(w, http.StatusBadRequest)
}

// timerNotFound reports an unknown timer as upstream's exception middleware
// reports a ResourceNotFoundException: 404 with its production text body.
func (liveTvStub) timerNotFound(w http.ResponseWriter, _ *http.Request) {
	writeLiveTvTimerError(w, http.StatusNotFound)
}

func writeLiveTvTimerError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("Error processing request."))
}

func (liveTvStub) noContent(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
