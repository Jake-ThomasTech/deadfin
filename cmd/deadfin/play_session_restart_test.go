// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/edge"
)

// TestApplicationPlaySessionSurvivesRestart (#620): with the reload on, a
// client starts
// playback, the application restarts on the same store, and the client goes
// on with its old token and PlaySessionId. The session and its now-playing
// item are there before any report, the PlaySessionId still authorizes the
// direct stream, and a progress report saves the position.
func TestApplicationPlaySessionSurvivesRestart(t *testing.T) {
	t.Setenv(envPlaySessionRestore, "true")
	const (
		runtimeTicks  int64 = 20 * 60 * 10_000_000
		startTicks    int64 = runtimeTicks / 4
		positionTicks int64 = runtimeTicks / 2
	)
	programData := filepath.Join(t.TempDir(), "program-data")
	var app *application
	t.Cleanup(func() {
		if app != nil {
			if err := app.Close(context.Background()); err != nil {
				t.Errorf("close application: %v", err)
			}
		}
	})

	app = openPlaystateTracerApplication(t, programData)
	configureComposedAdministrator(t, app.Router())
	token, userID := loginPlaystateTracerAdministrator(t, app)
	movieID := seedPlaystateTracerMovie(t, app, runtimeTicks)
	playSessionID := mintPlaystateTracerSession(t, app, token, movieID)
	start := fmt.Sprintf(`{"ItemId":%q,"CanSeek":true,"IsPaused":true,"PositionTicks":%d,"PlayMethod":"DirectPlay","PlaySessionId":%q}`, movieID.Dashed(), startTicks, playSessionID)
	if record := authenticatedCompositionRequest(app.Router(), http.MethodPost, "/Sessions/Playing", []byte(start), token); record.Code != http.StatusNoContent {
		t.Fatalf("start status = %d; body: %s", record.Code, record.Body.String())
	}
	before := playSessionSnapshot(t, app, token)
	streamPath := fmt.Sprintf("/Videos/%s/stream?static=true&PlaySessionId=%s", movieID.Dashed(), playSessionID)
	beforeStream := grantRequest(app, streamPath).Code
	if beforeStream == http.StatusUnauthorized {
		t.Fatalf("the PlaySessionId does not authorize the stream before the restart")
	}

	realClock := clock.NewReal()
	stopped := realClock.Now()
	if err := app.Close(context.Background()); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	app = nil
	app = openPlaystateTracerApplication(t, programData)
	t.Logf("in-process restart gap (close and reopen on SQLite): %s", realClock.Now().Sub(stopped).Round(time.Millisecond))

	restored := playSessionSnapshot(t, app, token)
	if restored != before || restored.UserID != userID.String() || restored.NowPlayingItem.ID != movieID.String() ||
		restored.PlayState.PositionTicks != startTicks || !restored.PlayState.IsPaused {
		t.Fatalf("restored session = %+v, want %+v", restored, before)
	}
	if code := grantRequest(app, streamPath).Code; code != beforeStream {
		t.Fatalf("stream with the old PlaySessionId after the restart = %d, want %d as before", code, beforeStream)
	}
	if code := grantRequest(app, fmt.Sprintf("/Videos/%s/stream?static=true&PlaySessionId=unknown", movieID.Dashed())).Code; code != http.StatusUnauthorized {
		t.Fatalf("stream with an unknown PlaySessionId = %d, want 401", code)
	}

	progress := fmt.Sprintf(`{"ItemId":%q,"CanSeek":true,"IsPaused":false,"PositionTicks":%d,"PlayMethod":"DirectPlay","PlaySessionId":%q}`, movieID.Dashed(), positionTicks, playSessionID)
	if record := authenticatedCompositionRequest(app.Router(), http.MethodPost, "/Sessions/Playing/Progress", []byte(progress), token); record.Code != http.StatusNoContent {
		t.Fatalf("progress after restart status = %d; body: %s", record.Code, record.Body.String())
	}
	if got := playSessionSnapshot(t, app, token); got.ID != before.ID || got.PlayState.PositionTicks != positionTicks || got.PlayState.IsPaused {
		t.Fatalf("session after progress = %+v", got)
	}
	itemPath := fmt.Sprintf("/Items/%s?userId=%s&EnableUserData=true", movieID.Dashed(), userID.Dashed())
	record := authenticatedCompositionRequest(app.Router(), http.MethodGet, itemPath, nil, token)
	if record.Code != http.StatusOK {
		t.Fatalf("GET item status = %d; body: %s", record.Code, record.Body.String())
	}
	assertPlaystateTracerUserData(t, record.Body.Bytes(), positionTicks)
}

type restartSessionDTO struct {
	ID             string `json:"Id"`
	UserID         string `json:"UserId"`
	DeviceID       string `json:"DeviceId"`
	NowPlayingItem struct {
		ID string `json:"Id"`
	} `json:"NowPlayingItem"`
	PlayState struct {
		PositionTicks int64 `json:"PositionTicks"`
		IsPaused      bool  `json:"IsPaused"`
	} `json:"PlayState"`
}

func playSessionSnapshot(t *testing.T, app *application, token string) restartSessionDTO {
	t.Helper()
	record := authenticatedCompositionRequest(app.Router(), http.MethodGet, "/Sessions?deviceId=deadfin-test-device", nil, token)
	if record.Code != http.StatusOK {
		t.Fatalf("GET /Sessions status = %d; body: %s", record.Code, record.Body.String())
	}
	var sessions []restartSessionDTO
	if err := json.Unmarshal(record.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode sessions: %v; body: %s", err, record.Body.String())
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %s, want one", record.Body.String())
	}
	return sessions[0]
}

// grantRequest sends a request with no credential but its PlaySessionId, as
// AVPlayer does.
func grantRequest(app *application, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = "127.0.0.1:8096"
	record := httptest.NewRecorder()
	app.Router().ServeHTTP(record, request)
	return record
}

// TestPlaySessionRestoreIsOffByDefault: with no setting the reload is off,
// as upstream keeps sessions in memory only. The environment variable, the
// stream edge and server replicas turn it on.
func TestPlaySessionRestoreIsOffByDefault(t *testing.T) {
	env := func(value string) func(string) string {
		return func(name string) string {
			if name == envPlaySessionRestore {
				return value
			}
			return ""
		}
	}
	signer, err := edge.NewSigner("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		value    string
		replicas *replicaMode
		edge     serverEdge
		want     bool
	}{
		{name: "unset", want: false},
		{name: "false", value: "false", want: false},
		{name: "true", value: "true", want: true},
		{name: "edge", edge: serverEdge{signer: signer}, want: true},
		{name: "replicas", value: "false", replicas: &replicaMode{}, want: true},
	} {
		got, err := playSessionRestore(env(test.value), test.replicas, test.edge)
		if err != nil || got != test.want {
			t.Errorf("%s: playSessionRestore = %v, %v; want %v", test.name, got, err, test.want)
		}
	}
	if _, err := playSessionRestore(env("sometimes"), nil, serverEdge{}); err == nil {
		t.Error("a value that is not a boolean is accepted")
	}
}
