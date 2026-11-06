// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

func bearerRequest(handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = "127.0.0.1:8096"
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestScalingTokenReadsOnlyTheMetricsAndTheScalingSignals(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	handler := app.Router()
	configureComposedAdministrator(t, handler)
	adminToken := loginComposedAdministrator(t, handler)
	const metrics, scaling = "/deadfin/v1/metrics", "/deadfin/v1/workers/scaling"

	for _, path := range []string{metrics, scaling} {
		assertAuthenticatedStatus(t, handler, http.MethodGet, path, nil, "", http.StatusUnauthorized)
		assertAuthenticatedStatus(t, handler, http.MethodGet, path, nil, adminToken, http.StatusOK)
		if got := bearerRequest(handler, http.MethodGet, path, "dfs1.not-a-real-scaling-token-value").Code; got != http.StatusUnauthorized {
			t.Fatalf("GET %s with an unknown scaling token = %d, want 401", path, got)
		}
	}

	created := authenticatedCompositionRequest(handler, http.MethodPost, "/deadfin/v1/transcode-workers/scaling-token", nil, adminToken)
	if created.Code != http.StatusOK {
		t.Fatalf("create scaling token = %d: %s", created.Code, created.Body.String())
	}
	var token dto.TranscodeWorkerScalingToken
	if err := json.Unmarshal(created.Body.Bytes(), &token); err != nil || !strings.HasPrefix(token.Token, "dfs1.") {
		t.Fatalf("scaling token = %+v, %v", token, err)
	}

	response := bearerRequest(handler, http.MethodGet, metrics, token.Token)
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/plain; version=0.0.4") ||
		!strings.Contains(response.Body.String(), `deadfin_transcode_pool_slots_wanted{pool="*"} 1`) {
		t.Fatalf("metrics with the scaling token = %d %q:\n%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	response = bearerRequest(handler, http.MethodGet, scaling, token.Token)
	var signals transcodeworker.Scaling
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &signals) != nil || signals.Pools["*"].SlotsWanted != 1 {
		t.Fatalf("scaling with the scaling token = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"slotsWanted":1`) {
		t.Fatalf("scaling JSON is not camelCase: %s", response.Body.String())
	}
	// The scaling token reads nothing else.
	for _, path := range []string{"/deadfin/v1/transcode-workers", "/deadfin/v1/transcode-workers/policy", "/System/Info", "/Users"} {
		if got := bearerRequest(handler, http.MethodGet, path, token.Token).Code; got != http.StatusUnauthorized {
			t.Fatalf("GET %s with the scaling token = %d, want 401", path, got)
		}
	}

	assertAuthenticatedStatus(t, handler, http.MethodDelete, "/deadfin/v1/transcode-workers/scaling-token", nil, adminToken, http.StatusNoContent)
	if got := bearerRequest(handler, http.MethodGet, metrics, token.Token).Code; got != http.StatusUnauthorized {
		t.Fatalf("metrics with a deleted scaling token = %d, want 401", got)
	}
}

func TestTranscodeScalingTokensFromTheEnvironment(t *testing.T) {
	token, err := transcodeworker.NewScalingToken()
	if err != nil {
		t.Fatal(err)
	}
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == envScalingTokens {
				return value
			}
			return ""
		}
	}
	if tokens, err := transcodeScalingTokens(env(" , " + token + " ,")); err != nil || len(tokens) != 1 || tokens[0] != token {
		t.Fatalf("tokens = %v, %v", tokens, err)
	}
	if _, err := transcodeScalingTokens(env("dfw1.worker-token")); err == nil || !strings.Contains(err.Error(), envScalingTokens) {
		t.Fatalf("a non-scaling token: err = %v", err)
	}
	if ok, err := (transcodeworker.ScalingTokens{Static: []string{token}}).Check(bearer(token)); err != nil || !ok {
		t.Fatalf("static token check = %v, %v", ok, err)
	}
	if ok, _ := (transcodeworker.ScalingTokens{Static: []string{token}}).Check(bearer(token + "x")); ok {
		t.Fatal("a wrong token passed")
	}
}

func bearer(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/deadfin/v1/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func TestDrainOnSignalStopsAfterTheGrace(t *testing.T) {
	agent, err := transcodeworker.NewAgent(transcodeworker.AgentConfig{
		ServerURL: "https://deadfin.example", JoinToken: mustWorkerToken(t), StateDir: t.TempDir(), WorkDir: t.TempDir(), Clock: clock.NewFake(time.Unix(1_800_000_000, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := clock.NewFake(time.Unix(1_800_000_000, 0))
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan struct{})
	go func() {
		drainOnSignal(signals, agent, 25*time.Second, fake, cancel, done)
		close(returned)
	}()
	signals <- os.Interrupt
	for !agent.Draining() {
		runtime.Gosched()
	}
	advanced := time.Duration(0)
	for ctx.Err() == nil {
		if advanced >= 10*time.Minute {
			t.Fatal("the grace ended without a stop")
		}
		fake.Advance(time.Second)
		advanced += time.Second
		for range 50 {
			runtime.Gosched()
		}
	}
	if advanced < 25*time.Second {
		t.Fatalf("stopped after %s, before the 25 s grace", advanced)
	}
	<-returned
	close(done)
}

func mustWorkerToken(t *testing.T) transcodeworker.Token {
	t.Helper()
	value, err := transcodeworker.NewToken("")
	if err != nil {
		t.Fatal(err)
	}
	token, err := transcodeworker.ParseToken(value)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
