// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
)

func TestLiveTvRoutesMatchPinnedOpenAPI(t *testing.T) {
	rt := router.New()
	handlers.RegisterLiveTv(rt, &fakeLiveTvAdmin{}, handlers.LiveTvReads{})

	access := string(auth.PolicyLiveTVAccess)
	manage := string(auth.PolicyLiveTVManagement)
	elevated := string(auth.PolicyRequiresElevation)
	wantRoutes := []router.Route{
		{Method: http.MethodGet, Pattern: "/LiveTv/ChannelMappingOptions", OperationID: "GetChannelMappingOptions", Policy: elevated},
		{Method: http.MethodPost, Pattern: "/LiveTv/ChannelMappings", OperationID: "SetChannelMapping", Policy: elevated},
		{Method: http.MethodGet, Pattern: "/LiveTv/Channels", OperationID: "GetLiveTvChannels", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/Channels/{channelId}", OperationID: "GetChannel", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/GuideInfo", OperationID: "GetGuideInfo", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/Info", OperationID: "GetLiveTvInfo", Policy: access},
		{Method: http.MethodPost, Pattern: "/LiveTv/ListingProviders", OperationID: "AddListingProvider", Policy: elevated},
		{Method: http.MethodDelete, Pattern: "/LiveTv/ListingProviders", OperationID: "DeleteListingProvider", Policy: elevated},
		{Method: http.MethodGet, Pattern: "/LiveTv/ListingProviders/Default", OperationID: "GetDefaultListingProvider", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/ListingProviders/Lineups", OperationID: "GetLineups", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/ListingProviders/SchedulesDirect/Countries", OperationID: "GetSchedulesDirectCountries", Policy: elevated},
		{Method: http.MethodGet, Pattern: "/LiveTv/LiveRecordings/{recordingId}/stream", OperationID: "GetLiveRecordingFile", Anonymous: true},
		{Method: http.MethodGet, Pattern: "/LiveTv/LiveStreamFiles/{streamId}/stream.{container}", OperationID: "GetLiveStreamFile", Anonymous: true},
		{Method: http.MethodGet, Pattern: "/LiveTv/Programs", OperationID: "GetLiveTvPrograms", Policy: access},
		{Method: http.MethodPost, Pattern: "/LiveTv/Programs", OperationID: "GetPrograms", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/Programs/{programId}", OperationID: "GetProgram", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/Programs/Recommended", OperationID: "GetRecommendedPrograms", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/Recordings", OperationID: "GetRecordings", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/Recordings/{recordingId}", OperationID: "GetRecording", Policy: access},
		{Method: http.MethodDelete, Pattern: "/LiveTv/Recordings/{recordingId}", OperationID: "DeleteRecording", Policy: manage},
		{Method: http.MethodGet, Pattern: "/LiveTv/Recordings/Folders", OperationID: "GetRecordingFolders", Policy: access},
		{Method: http.MethodGet, Pattern: "/LiveTv/SeriesTimers", OperationID: "GetSeriesTimers", Policy: access},
		{Method: http.MethodPost, Pattern: "/LiveTv/SeriesTimers", OperationID: "CreateSeriesTimer", Policy: manage},
		{Method: http.MethodGet, Pattern: "/LiveTv/SeriesTimers/{timerId}", OperationID: "GetSeriesTimer", Policy: access},
		{Method: http.MethodDelete, Pattern: "/LiveTv/SeriesTimers/{timerId}", OperationID: "CancelSeriesTimer", Policy: manage},
		{Method: http.MethodPost, Pattern: "/LiveTv/SeriesTimers/{timerId}", OperationID: "UpdateSeriesTimer", Policy: manage},
		{Method: http.MethodGet, Pattern: "/LiveTv/Timers", OperationID: "GetTimers", Policy: access},
		{Method: http.MethodPost, Pattern: "/LiveTv/Timers", OperationID: "CreateTimer", Policy: manage},
		{Method: http.MethodGet, Pattern: "/LiveTv/Timers/{timerId}", OperationID: "GetTimer", Policy: access},
		{Method: http.MethodDelete, Pattern: "/LiveTv/Timers/{timerId}", OperationID: "CancelTimer", Policy: manage},
		{Method: http.MethodPost, Pattern: "/LiveTv/Timers/{timerId}", OperationID: "UpdateTimer", Policy: manage},
		{Method: http.MethodGet, Pattern: "/LiveTv/Timers/Defaults", OperationID: "GetDefaultTimer", Policy: access},
		{Method: http.MethodPost, Pattern: "/LiveTv/TunerHosts", OperationID: "AddTunerHost", Policy: elevated},
		{Method: http.MethodDelete, Pattern: "/LiveTv/TunerHosts", OperationID: "DeleteTunerHost", Policy: elevated},
		{Method: http.MethodGet, Pattern: "/LiveTv/TunerHosts/Types", OperationID: "GetTunerHostTypes", Policy: access},
		{Method: http.MethodPost, Pattern: "/LiveTv/Tuners/{tunerId}/Reset", OperationID: "ResetTuner", Policy: elevated},
		{Method: http.MethodGet, Pattern: "/LiveTv/Tuners/Discover", OperationID: "DiscoverTuners", Policy: elevated},
		{Method: http.MethodGet, Pattern: "/LiveTv/Tuners/Discvover", OperationID: "DiscvoverTuners", Policy: elevated},
		{Method: http.MethodGet, Pattern: "/deadfin/v1/livetv/tunerhosts/{id}/groups", OperationID: "NATIVE:GetTunerHostGroups", Policy: elevated},
	}

	if got := len(rt.Routes()); got != len(wantRoutes) {
		t.Fatalf("LiveTv route count = %d, want %d", got, len(wantRoutes))
	}
	for _, want := range wantRoutes {
		t.Run(want.OperationID, func(t *testing.T) {
			got, found := routeByOperation(rt.Routes(), want.OperationID)
			if !found || got != want {
				t.Fatalf("route %q = %#v, found %t, want %#v", want.OperationID, got, found, want)
			}
		})
	}
}

func TestLiveTvInfoReportsDisabledWithNoServicesOrUsers(t *testing.T) {
	rt := router.New()
	handlers.RegisterLiveTv(rt, &fakeLiveTvAdmin{}, handlers.LiveTvReads{})
	record := liveTvRequest(rt, http.MethodGet, "/LiveTv/Info", "")
	if record.Code != http.StatusOK {
		t.Fatalf("GET LiveTv Info status = %d, want 200; body=%s", record.Code, record.Body.String())
	}
	var body struct {
		Services     []json.RawMessage `json:"Services"`
		IsEnabled    bool              `json:"IsEnabled"`
		EnabledUsers []string          `json:"EnabledUsers"`
	}
	if err := json.Unmarshal(record.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode LiveTv Info: %v; body=%s", err, record.Body.String())
	}
	if body.IsEnabled || body.Services == nil || len(body.Services) != 0 || body.EnabledUsers == nil || len(body.EnabledUsers) != 0 {
		t.Fatalf("LiveTv Info = %#v, want disabled with empty services and users", body)
	}
}

func TestLiveTvCollectionEndpointsReturnEmptyQueryResults(t *testing.T) {
	rt := router.New()
	handlers.RegisterLiveTv(rt, &fakeLiveTvAdmin{}, handlers.LiveTvReads{})
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/LiveTv/Channels"},
		{http.MethodGet, "/LiveTv/Programs"},
		{http.MethodPost, "/LiveTv/Programs"},
		{http.MethodGet, "/LiveTv/Programs/Recommended"},
		{http.MethodGet, "/LiveTv/Recordings"},
		{http.MethodGet, "/LiveTv/Recordings/Folders"},
		{http.MethodGet, "/LiveTv/SeriesTimers"},
		{http.MethodGet, "/LiveTv/Timers"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			record := liveTvRequest(rt, tc.method, tc.path, `{}`)
			if record.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", record.Code, record.Body.String())
			}
			var body dto.QueryResult[json.RawMessage]
			if err := json.Unmarshal(record.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode QueryResult: %v; body=%s", err, record.Body.String())
			}
			if body.Items == nil || len(body.Items) != 0 || body.TotalRecordCount != 0 || body.StartIndex != 0 {
				t.Fatalf("QueryResult = %#v, want empty result at start index zero", body)
			}
		})
	}
}

func TestLiveTvDeferredRoutesKeepNotFoundShape(t *testing.T) {
	rt := router.New()
	handlers.RegisterLiveTv(rt, &fakeLiveTvAdmin{}, handlers.LiveTvReads{})

	missing := liveTvRequest(rt, http.MethodDelete, "/LiveTv/Recordings/missing", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing recording status = %d, want 404; body=%s", missing.Code, missing.Body.String())
	}
	var problem struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(missing.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode missing recording problem: %v; body=%s", err, missing.Body.String())
	}
	if problem.Title != http.StatusText(http.StatusNotFound) || problem.Status != http.StatusNotFound || problem.Detail != "no tuners configured" {
		t.Fatalf("missing recording problem = %#v", problem)
	}
}

// TestLiveTvTimerMutationsReportNoDvr covers D-122: no timer mutation
// reports success, because Deadfin stores no timer.
func TestLiveTvTimerMutationsReportNoDvr(t *testing.T) {
	rt := router.New()
	handlers.RegisterLiveTv(rt, &fakeLiveTvAdmin{}, handlers.LiveTvReads{})
	for _, tc := range []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{http.MethodPost, "/LiveTv/Timers", `{"ProgramId":"p1","ChannelId":"c1"}`, http.StatusBadRequest},
		{http.MethodPost, "/LiveTv/SeriesTimers", `{"ProgramId":"p1","RecordAnyChannel":true}`, http.StatusBadRequest},
		{http.MethodPost, "/LiveTv/Timers/t1", `{"Id":"t1","PrePaddingSeconds":60}`, http.StatusNotFound},
		{http.MethodPost, "/LiveTv/SeriesTimers/s1", `{"Id":"s1","KeepUpTo":3}`, http.StatusNotFound},
		{http.MethodDelete, "/LiveTv/Timers/t1", "", http.StatusNotFound},
		{http.MethodDelete, "/LiveTv/SeriesTimers/s1", "", http.StatusNotFound},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			record := liveTvRequest(rt, tc.method, tc.path, tc.body)
			if record.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", record.Code, tc.want, record.Body.String())
			}
			if got := record.Header().Get("Content-Type"); got != "text/plain" {
				t.Fatalf("Content-Type = %q, want text/plain", got)
			}
			if got := record.Body.String(); got != "Error processing request." {
				t.Fatalf("body = %q, want upstream production error text", got)
			}
		})
	}

	for _, path := range []string{"/LiveTv/Timers", "/LiveTv/SeriesTimers"} {
		record := liveTvRequest(rt, http.MethodGet, path, "")
		var body dto.QueryResult[json.RawMessage]
		if err := json.Unmarshal(record.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v; body=%s", path, err, record.Body.String())
		}
		if body.TotalRecordCount != 0 || len(body.Items) != 0 {
			t.Fatalf("GET %s after refused mutations = %#v, want no timers", path, body)
		}
	}
}

func liveTvRequest(rt *router.Router, method, path, body string) *httptest.ResponseRecorder {
	record := httptest.NewRecorder()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	rt.ServeHTTP(record, request)
	return record
}
