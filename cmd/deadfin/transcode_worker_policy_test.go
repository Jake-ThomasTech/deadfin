// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

func TestTranscodeWorkerPolicyAPI(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	handler := app.Router()
	configureComposedAdministrator(t, handler)
	adminToken := loginComposedAdministrator(t, handler)
	const path = "/deadfin/v1/transcode-workers/policy"

	// Administrator only.
	assertAuthenticatedStatus(t, handler, http.MethodGet, path, nil, "", http.StatusUnauthorized)
	assertAuthenticatedStatus(t, handler, http.MethodPost, "/Users/New", []byte(`{"Name":"viewer","Password":"viewer password"}`), adminToken, http.StatusOK)
	login := authenticatedCompositionRequest(handler, http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"viewer","Pw":"viewer password"}`), "")
	if login.Code != http.StatusOK {
		t.Fatalf("viewer login = %d: %s", login.Code, login.Body.String())
	}
	userToken := compositionAccessToken(t, login)
	assertAuthenticatedStatus(t, handler, http.MethodGet, path, nil, userToken, http.StatusForbidden)
	assertAuthenticatedStatus(t, handler, http.MethodPut, path, []byte(`{"Mode":"workers-only"}`), userToken, http.StatusForbidden)

	// The default policy runs every job locally.
	var policy dto.TranscodeWorkerPolicy
	getCompositionJSON(t, handler, path, adminToken, &policy)
	if policy.Mode != "local" || policy.Strategy != "pack" || len(policy.Rules) != 0 {
		t.Fatalf("default policy = %+v", policy)
	}

	for _, body := range []string{`{"Mode":"cloud"}`, `{"Mode":"workers-only","QueueTimeoutSeconds":3600}`, `{"Rules":[{"Local":true,"Pools":["gpu"]}]}`, `{"Rules":[{"Match":{"Hdr":["hdr11"]}}]}`} {
		assertAuthenticatedStatus(t, handler, http.MethodPut, path, []byte(body), adminToken, http.StatusBadRequest)
	}

	body := `{"Mode":"workers-only","QueueTimeoutSeconds":5,"CpuFallback":false,"Strategy":"spread","DefaultPools":["GPU"],"WarmSlots":{"GPU":2},` +
		`"Rules":[{"Name":"dv","Match":{"Hdr":["DoVi"],"BitDepthMin":10},"Pools":["nvidia"]}]}`
	recorder := authenticatedCompositionRequest(handler, http.MethodPut, path, []byte(body), adminToken)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT policy = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `no worker carries pool \"gpu\"`) {
		t.Fatalf("PUT policy warnings: %s", recorder.Body.String())
	}
	getCompositionJSON(t, handler, path, adminToken, &policy)
	if policy.Mode != "workers-only" || policy.Strategy != "spread" || policy.QueueTimeoutSeconds == nil || *policy.QueueTimeoutSeconds != 5 ||
		policy.CPUFallback == nil || *policy.CPUFallback || len(policy.DefaultPools) != 1 || policy.DefaultPools[0] != "gpu" ||
		policy.WarmSlots["gpu"] != 2 || len(policy.Rules) != 1 || policy.Rules[0].Match.HDR[0] != "dovi" || policy.Rules[0].Match.BitDepthMin != 10 || policy.Warnings != nil {
		t.Fatalf("stored policy = %+v", policy)
	}
	if live := app.transcodeWorkers.Policy(); live.Mode != transcodeworker.ModeWorkersOnly || live.Strategy != transcodeworker.StrategySpread {
		t.Fatalf("live hub policy = %+v", live)
	}
}

func TestTranscodeWorkerPolicyAtStartup(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	workers := store.NewTranscodeWorkerStore(stores)
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}

	policy, err := transcodeWorkerPolicy(t.Context(), workers, env(nil))
	if err != nil || policy.Mode != transcodeworker.ModeLocal {
		t.Fatalf("no stored policy = %+v, %v; want local", policy, err)
	}
	policy, err = transcodeWorkerPolicy(t.Context(), workers, env(map[string]string{envWorkerMode: "workers-first"}))
	if err != nil || policy.Mode != transcodeworker.ModeWorkersFirst {
		t.Fatalf("env mode = %+v, %v", policy, err)
	}
	if err := workers.SetPolicy(t.Context(), []byte(`{"mode":"local-first","localLimit":2,"rules":[{"name":"hevc","match":{"sourceCodec":["hevc"]},"local":true}]}`), time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	policy, err = transcodeWorkerPolicy(t.Context(), workers, env(map[string]string{envWorkerMode: "workers-only"}))
	if err != nil || policy.Mode != transcodeworker.ModeWorkersOnly || policy.LocalLimit != 2 || len(policy.Rules) != 1 {
		t.Fatalf("stored policy with env mode = %+v, %v", policy, err)
	}
	if _, err := transcodeWorkerPolicy(t.Context(), workers, env(map[string]string{envWorkerMode: "cloud"})); err == nil || !strings.Contains(err.Error(), envWorkerMode) {
		t.Fatalf("bad env mode: err = %v, want an error naming the variable", err)
	}
}
