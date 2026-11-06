// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/edge"
	"github.com/deadfin-org/deadfin/internal/playback"
)

type recordingEdge struct {
	itemID        dto.UUID
	playSessionID string
	sources       int
}

func (e *recordingEdge) DirectGrants(itemID dto.UUID, playSessionID string, sources []playback.MediaSourceInfo) []string {
	e.itemID, e.playSessionID, e.sources = itemID, playSessionID, len(sources)
	return []string{"grant-a", "grant-b"}
}

// TestPlaybackInfoCarriesEdgeDirectGrants covers #621: PlaybackInfo puts the
// stream edge's direct grants in a header, with the PlaySessionId only for a
// user.
func TestPlaybackInfoCarriesEdgeDirectGrants(t *testing.T) {
	userID := playbackTestUUID(t, "00000000-0000-0000-0000-000000000901")
	itemID := playbackTestUUID(t, "00000000-0000-0000-0000-000000000902")
	const playSessionID = "5e1d4d5f6a7b4c8d9e0f1a2b3c4d5e6f"
	service := grantPlaybackInfoService{playSessionID: playSessionID, sourceID: itemID.String()}
	for _, test := range []struct {
		name        string
		userID      dto.UUID
		wantSession string
	}{
		{"user", userID, playSessionID},
		{"API key without a user", dto.NilUUID, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := &recordingEdge{}
			authorizer := middleware.NewAuthorizer(tokenResolver{userID: test.userID}, authenticatedEvaluator{}, playbackUsers{})
			routes := router.New(authorizer.Wrap)
			handlers.RegisterPlaybackInfoWithEdge(routes, service, nil, nil, recorder)
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				r := httptest.NewRequest(method, "/Items/"+itemID.Dashed()+"/PlaybackInfo", strings.NewReader(`{}`))
				r.Header.Set("Authorization", `MediaBrowser Client="Infuse", Token="user-token"`)
				r.Header.Set("Content-Type", "application/json")
				record := httptest.NewRecorder()
				routes.ServeHTTP(record, r)
				if record.Code != http.StatusOK {
					t.Fatalf("%s status = %d", method, record.Code)
				}
				if got := record.Header().Values(edge.DirectHeader); strings.Join(got, " ") != "grant-a grant-b" {
					t.Fatalf("%s %s = %q", method, edge.DirectHeader, got)
				}
				if recorder.itemID != itemID || recorder.playSessionID != test.wantSession || recorder.sources != 1 {
					t.Fatalf("%s DirectGrants(%v, %q, %d sources)", method, recorder.itemID, recorder.playSessionID, recorder.sources)
				}
			}
		})
	}
}

type transcodePlaybackInfoService struct {
	grantPlaybackInfoService
	transcodingURL string
}

func (service transcodePlaybackInfoService) PlaybackInfo(ctx context.Context, request playback.PlaybackInfoRequest) (playback.PlaybackInfoResponse, error) {
	response, err := service.grantPlaybackInfoService.PlaybackInfo(ctx, request)
	response.MediaSources[0].TranscodingURL = service.transcodingURL
	return response, err
}

type recordingWarmup struct{ sessions []string }

func (w *recordingWarmup) Anticipate(playSessionID string) {
	w.sessions = append(w.sessions, playSessionID)
}

// TestPlaybackInfoAnnouncesTranscodes covers #623: a response with a
// TranscodingUrl tells the warm-up its PlaySessionId; a direct play response
// does not.
func TestPlaybackInfoAnnouncesTranscodes(t *testing.T) {
	userID := playbackTestUUID(t, "00000000-0000-0000-0000-000000000903")
	itemID := playbackTestUUID(t, "00000000-0000-0000-0000-000000000904")
	const playSessionID = "6f2e5e6a7b8c4d9e0f1a2b3c4d5e6f70"
	for _, test := range []struct {
		name           string
		transcodingURL string
		want           string
	}{
		{"transcode", "/videos/x/master.m3u8", playSessionID},
		{"direct play", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := transcodePlaybackInfoService{grantPlaybackInfoService{playSessionID: playSessionID, sourceID: itemID.String()}, test.transcodingURL}
			warmup := &recordingWarmup{}
			authorizer := middleware.NewAuthorizer(tokenResolver{userID: userID}, authenticatedEvaluator{}, playbackUsers{})
			routes := router.New(authorizer.Wrap)
			handlers.RegisterPlaybackInfoWithOptions(routes, service, nil, handlers.PlaybackInfoOptions{Warmup: warmup})
			r := httptest.NewRequest(http.MethodPost, "/Items/"+itemID.Dashed()+"/PlaybackInfo", strings.NewReader(`{}`))
			r.Header.Set("Authorization", `MediaBrowser Client="Infuse", Token="user-token"`)
			r.Header.Set("Content-Type", "application/json")
			record := httptest.NewRecorder()
			routes.ServeHTTP(record, r)
			if record.Code != http.StatusOK {
				t.Fatalf("status = %d", record.Code)
			}
			if got := strings.Join(warmup.sessions, ","); got != test.want {
				t.Fatalf("Anticipate calls = %q, want %q", got, test.want)
			}
		})
	}
}
