// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/events"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

func TestTranscodeWorkerRoutesAreOnTheMainRouterAndRefuseUserTokens(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))

	want := map[string]bool{
		http.MethodGet + " " + transcodeworker.PathConnect: true,
		http.MethodGet + " " + transcodeworker.PathSource:  true,
		http.MethodPut + " " + transcodeworker.PathFile:    true,
	}
	admin := 0
	for _, route := range app.Router().Routes() {
		key := route.Method + " " + route.Pattern
		if strings.HasPrefix(route.Pattern, "/deadfin/v1/transcode-workers") {
			admin++
			if route.Worker || route.Anonymous || route.Policy != string(auth.PolicyRequiresElevation) {
				t.Errorf("%s = %+v, want an administrator route", key, route)
			}
			continue
		}
		if !want[key] {
			continue
		}
		if !route.Worker || route.Anonymous || route.Policy != "" {
			t.Errorf("%s = %+v, want a worker credential route", key, route)
		}
		delete(want, key)
	}
	if len(want) != 0 || admin != 13 {
		t.Fatalf("worker routes missing: %v; admin routes = %d, want 12", want, admin)
	}

	// Before the first join token the connect route does not exist. The request comes from loopback so the first-run setup
	// gate, which covers these routes too, lets it reach the hub.
	connect := httptest.NewRequest(http.MethodGet, transcodeworker.PathConnect, nil)
	connect.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()
	app.Router().ServeHTTP(recorder, connect)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("connect without credentials = %d, want 404", recorder.Code)
	}

	// A user credential is not a job credential.
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/deadfin/v1/workers/jobs/job/source", nil),
		httptest.NewRequest(http.MethodPut, "/deadfin/v1/workers/jobs/job/files/abc0.ts", strings.NewReader("x")),
	} {
		request.RemoteAddr = "127.0.0.1:40000"
		request.Header.Set("Authorization", `MediaBrowser Client="test", Device="test", DeviceId="test", Version="1", Token="user-token"`)
		recorder := httptest.NewRecorder()
		app.Router().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a user token = %d, want 401", request.Method, request.URL.Path, recorder.Code)
		}
	}
}

// TestTranscodeWorkerAdminEnrollsListsAndRevokes drives the admin API and
// a real agent against the composed server (#615).
func TestTranscodeWorkerAdminEnrollsListsAndRevokes(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	handler := app.Router()
	configureComposedAdministrator(t, handler)
	adminToken := loginComposedAdministrator(t, handler)

	// Administrator only: anonymous is 401, a user is 403.
	assertAuthenticatedStatus(t, handler, http.MethodGet, "/deadfin/v1/transcode-workers", nil, "", http.StatusUnauthorized)
	assertAuthenticatedStatus(t, handler, http.MethodPost, "/Users/New", []byte(`{"Name":"viewer","Password":"viewer password"}`), adminToken, http.StatusOK)
	login := authenticatedCompositionRequest(handler, http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"viewer","Pw":"viewer password"}`), "")
	if login.Code != http.StatusOK {
		t.Fatalf("viewer login = %d: %s", login.Code, login.Body.String())
	}
	userToken := compositionAccessToken(t, login)
	for _, path := range []string{"/deadfin/v1/transcode-workers", "/deadfin/v1/transcode-workers/join-tokens", "/deadfin/v1/transcode-workers/pins"} {
		assertAuthenticatedStatus(t, handler, http.MethodGet, path, nil, userToken, http.StatusForbidden)
	}
	assertAuthenticatedStatus(t, handler, http.MethodPost, "/deadfin/v1/transcode-workers/join-tokens", []byte(`{"Name":"x"}`), userToken, http.StatusForbidden)

	// Validation.
	assertAuthenticatedStatus(t, handler, http.MethodPost, "/deadfin/v1/transcode-workers/join-tokens", []byte(`{"Fingerprint":"nope"}`), adminToken, http.StatusBadRequest)
	assertAuthenticatedStatus(t, handler, http.MethodPost, "/deadfin/v1/transcode-workers/join-tokens", []byte(`{"Uses":-1}`), adminToken, http.StatusBadRequest)
	assertAuthenticatedStatus(t, handler, http.MethodPut, "/deadfin/v1/transcode-workers/pins", []byte(`{"Fingerprints":["nope"]}`), adminToken, http.StatusBadRequest)
	assertAuthenticatedStatus(t, handler, http.MethodDelete, "/deadfin/v1/transcode-workers/join-tokens/missing", nil, adminToken, http.StatusNotFound)
	assertAuthenticatedStatus(t, handler, http.MethodDelete, "/deadfin/v1/transcode-workers/missing", nil, adminToken, http.StatusNotFound)

	created := authenticatedCompositionRequest(handler, http.MethodPost, "/deadfin/v1/transcode-workers/join-tokens", []byte(`{"Name":"gpu box","Uses":1,"ExpiresInSeconds":3600}`), adminToken)
	if created.Code != http.StatusOK {
		t.Fatalf("create join token = %d: %s", created.Code, created.Body.String())
	}
	var joinToken dto.CreatedTranscodeWorkerJoinToken
	if err := json.Unmarshal(created.Body.Bytes(), &joinToken); err != nil {
		t.Fatal(err)
	}
	token, err := transcodeworker.ParseToken(joinToken.Token)
	if err != nil || token.ID != joinToken.ID || joinToken.Name != "gpu box" || joinToken.UsesLeft == nil || *joinToken.UsesLeft != 1 || joinToken.Expires == nil || !joinToken.Usable {
		t.Fatalf("created join token = %+v, %v", joinToken, err)
	}
	var tokens []dto.TranscodeWorkerJoinToken
	getCompositionJSON(t, handler, "/deadfin/v1/transcode-workers/join-tokens", adminToken, &tokens)
	if len(tokens) != 1 || tokens[0].ID != token.ID || strings.Contains(fmt.Sprint(tokens), token.Secret) {
		t.Fatalf("join tokens = %+v", tokens)
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	agent, err := transcodeworker.NewAgent(transcodeworker.AgentConfig{
		ServerURL: server.URL, StateDir: t.TempDir(), JoinToken: token, Name: "gpu-a", WorkDir: t.TempDir(),
		Capabilities: transcodeworker.Capabilities{MaxJobs: 2, HWAccels: []string{"qsv"}, Pools: []string{"QSV"}},
		Clock:        clock.NewReal(), ReconnectDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	var workers []dto.TranscodeWorker
	waitFor(t, func() bool {
		getCompositionJSON(t, handler, "/deadfin/v1/transcode-workers", adminToken, &workers)
		return len(workers) == 1 && workers[0].Connected
	})
	worker := workers[0]
	if worker.ID != agent.WorkerID() || worker.Name != "gpu-a" || worker.JoinTokenID != token.ID || worker.InputMode != "auto" ||
		strings.Join(worker.Pools, ",") != "qsv" || worker.Capabilities == nil || worker.Capabilities.MaxJobs != 2 {
		t.Fatalf("worker = %+v", worker)
	}
	getCompositionJSON(t, handler, "/deadfin/v1/transcode-workers/join-tokens", adminToken, &tokens)
	if len(tokens) != 1 || *tokens[0].UsesLeft != 0 || tokens[0].Usable {
		t.Fatalf("spent join token = %+v", tokens)
	}

	assertAuthenticatedStatus(t, handler, http.MethodPost, "/deadfin/v1/transcode-workers/"+worker.ID, []byte(`{"Name":"gpu-a1","InputMode":"shared","Pools":["Big","nvenc"]}`), adminToken, http.StatusNoContent)
	assertAuthenticatedStatus(t, handler, http.MethodPost, "/deadfin/v1/transcode-workers/"+worker.ID, []byte(`{"InputMode":"magic"}`), adminToken, http.StatusBadRequest)
	getCompositionJSON(t, handler, "/deadfin/v1/transcode-workers", adminToken, &workers)
	if workers[0].Name != "gpu-a1" || workers[0].InputMode != "shared" || strings.Join(workers[0].Pools, ",") != "big,nvenc" {
		t.Fatalf("updated worker = %+v", workers[0])
	}
	if pools := app.transcodeWorkers.Workers()[0].Pools; strings.Join(pools, ",") != "big,nvenc" {
		t.Fatalf("live pools = %v", pools)
	}

	pin := transcodeworker.CertificateFingerprint(&x509.Certificate{RawSubjectPublicKeyInfo: []byte("next key")})
	assertAuthenticatedStatus(t, handler, http.MethodPut, "/deadfin/v1/transcode-workers/pins", []byte(`{"Fingerprints":["`+pin+`"]}`), adminToken, http.StatusNoContent)
	var pins dto.TranscodeWorkerPins
	getCompositionJSON(t, handler, "/deadfin/v1/transcode-workers/pins", adminToken, &pins)
	if len(pins.Fingerprints) != 1 || pins.Fingerprints[0] != pin {
		t.Fatalf("pins = %+v", pins)
	}

	// Revoking the join token keeps the worker; revoking the worker closes
	// its connection, and the agent stops when the server refuses it.
	assertAuthenticatedStatus(t, handler, http.MethodDelete, "/deadfin/v1/transcode-workers/join-tokens/"+token.ID, nil, adminToken, http.StatusNoContent)
	getCompositionJSON(t, handler, "/deadfin/v1/transcode-workers", adminToken, &workers)
	if len(workers) != 1 || workers[0].Revoked != nil {
		t.Fatalf("token revocation changed the worker: %+v", workers)
	}
	assertAuthenticatedStatus(t, handler, http.MethodDelete, "/deadfin/v1/transcode-workers/"+worker.ID, nil, adminToken, http.StatusNoContent)
	select {
	case err := <-done:
		done <- err
		if !errors.Is(err, transcodeworker.ErrRejected) {
			t.Fatalf("revoked agent Run = %v, want ErrRejected", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked worker kept running")
	}
	getCompositionJSON(t, handler, "/deadfin/v1/transcode-workers", adminToken, &workers)
	if len(workers) != 1 || workers[0].Connected || workers[0].Revoked == nil {
		t.Fatalf("revoked worker = %+v", workers)
	}
}

func getCompositionJSON(t *testing.T, handler http.Handler, path, token string, into any) {
	t.Helper()
	recorder := authenticatedCompositionRequest(handler, http.MethodGet, path, nil, token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

// waitFor polls a condition on the composed server, whose clock is real.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for !cond() {
		select {
		case <-deadline.C:
			t.Fatal("condition not reached")
		default:
		}
		runtime.Gosched()
	}
}

func TestWorkerFingerprintIsThePEMCertificateKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "deadfin.test"}, NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1<<32, 0)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	bundle := append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("ignored")}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	fingerprint, err := pemCertificateFingerprint(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != transcodeworker.CertificateFingerprint(certificate) {
		t.Fatalf("fingerprint = %q, want the leaf SPKI digest", fingerprint)
	}
	if _, err := pemCertificateFingerprint([]byte("not pem")); err == nil {
		t.Fatal("a bundle without a certificate was accepted")
	}
	path := filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := workerFingerprintCommand([]string{"-cert", path}); err != nil || got != fingerprint {
		t.Fatalf("worker fingerprint = %q, %v", got, err)
	}
	if _, err := workerFingerprintCommand(nil); err == nil {
		t.Fatal("worker fingerprint without -cert succeeded")
	}
}

func TestTranscodeWorkerEnvironment(t *testing.T) {
	token, err := transcodeworker.NewToken("")
	if err != nil {
		t.Fatal(err)
	}
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	tokens, allow, err := transcodeWorkerEnvironment(env(nil))
	if err != nil || len(tokens) != 0 || allow != nil {
		t.Fatalf("empty environment = %v, %v, %v", tokens, allow, err)
	}
	tokens, allow, err = transcodeWorkerEnvironment(env(map[string]string{envWorkerTokens: " , " + token + " ,", envWorkerAllowHTTP: "true"}))
	if err != nil || len(tokens) != 1 || allow == nil || !*allow {
		t.Fatalf("configured environment = %v, %v, %v", tokens, allow, err)
	}
	for key, value := range map[string]string{envWorkerTokens: "dfw1.not-a-token", envWorkerAllowHTTP: "maybe"} {
		if _, _, err := transcodeWorkerEnvironment(env(map[string]string{key: value})); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: err = %v, want an error naming the variable", key, value, err)
		}
	}
}

func TestTranscodeJobLeaseEnvironment(t *testing.T) {
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == envJobLease {
				return value
			}
			return ""
		}
	}
	for value, want := range map[string]time.Duration{"": transcodeworker.DefaultLease, "0": 0, "90s": 90 * time.Second, " 2m ": 2 * time.Minute} {
		if got, err := transcodeJobLease(env(value)); err != nil || got != want {
			t.Errorf("%s=%q = %s, %v; want %s", envJobLease, value, got, err, want)
		}
	}
	for _, value := range []string{"soon", "-5s"} {
		if _, err := transcodeJobLease(env(value)); err == nil || !strings.Contains(err.Error(), envJobLease) {
			t.Errorf("%s=%q: err = %v, want an error naming the variable", envJobLease, value, err)
		}
	}
}

// TestStaticTranscodeWorkerTokenFromTheEnvironment is the GitOps setup: one
// server variable, and the server URL and token on the worker.
func TestStaticTranscodeWorkerTokenFromTheEnvironment(t *testing.T) {
	value, err := transcodeworker.NewToken("")
	if err != nil {
		t.Fatal(err)
	}
	token, err := transcodeworker.ParseToken(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWorkerTokens, value)
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	handler := app.Router()
	configureComposedAdministrator(t, handler)
	adminToken := loginComposedAdministrator(t, handler)

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	agent, err := transcodeworker.NewAgent(transcodeworker.AgentConfig{
		ServerURL: server.URL, JoinToken: token, Name: "gitops-0", WorkDir: t.TempDir(),
		Capabilities: transcodeworker.Capabilities{MaxJobs: 1}, Clock: clock.NewReal(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	var workers []dto.TranscodeWorker
	waitFor(t, func() bool {
		getCompositionJSON(t, handler, "/deadfin/v1/transcode-workers", adminToken, &workers)
		return len(workers) == 1 && workers[0].Connected
	})
	if !workers[0].Static || workers[0].Name != "gitops-0" || agent.WorkerID() != "" {
		t.Fatalf("static worker = %+v, agent id %q", workers[0], agent.WorkerID())
	}
	assertAuthenticatedStatus(t, handler, http.MethodDelete, "/deadfin/v1/transcode-workers/"+workers[0].ID, nil, adminToken, http.StatusNotFound)
}

func TestWorkerHardwareFlags(t *testing.T) {
	hardware, err := workerHardware("QSV", "/dev/dri/renderD129", []string{"hevc"})
	if err != nil || hardware.Type != encoding.HardwareAccelerationQSV || hardware.QsvDevice != "/dev/dri/renderD129" || hardware.VaapiDevice != "/dev/dri/renderD129" || len(hardware.DecodingCodecs) != 1 {
		t.Fatalf("workerHardware = %+v, %v", hardware, err)
	}
	if hardware, err := workerHardware("", "/dev/dri/renderD128", nil); err != nil || hardware.Type != "" {
		t.Fatalf("software = %+v, %v", hardware, err)
	}
	for _, bad := range [][2]string{{"amf", "/dev/dri/renderD128"}, {"vaapi", "renderD128"}} {
		if _, err := workerHardware(bad[0], bad[1], nil); err == nil {
			t.Fatalf("workerHardware(%q, %q) accepted", bad[0], bad[1])
		}
	}
	probed := []string{"cuda", "drm", "opencl", "qsv", "vaapi"}
	if got := strings.Join(hardwareFamilies("qsv", probed), ","); got != "qsv,vaapi,opencl" {
		t.Fatalf("qsv families = %s", got)
	}
	if got := strings.Join(hardwareFamilies("nvenc", probed), ","); got != "cuda,opencl" {
		t.Fatalf("nvenc families = %s", got)
	}
	if got := hardwareFamilies("", probed); len(got) != 0 {
		t.Fatalf("software families = %v", got)
	}
}

// TestTranscodeScaleHookEnvironment covers #623: the hook is off without a
// URL and, with a warning, without a secret; a bad URL or a short secret is
// an error.
func TestTranscodeScaleHookEnvironment(t *testing.T) {
	const secret = "0123456789abcdef"
	logger := slog.New(slog.DiscardHandler)
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	if hook, err := transcodeScaleHook(env(nil), logger); err != nil || hook.URL != "" {
		t.Fatalf("absent = %+v, %v", hook, err)
	}
	if hook, err := transcodeScaleHook(env(map[string]string{envScaleHookURL: "https://keda.example/scale"}), logger); err != nil || hook.URL != "" {
		t.Fatalf("no secret = %+v, %v", hook, err)
	}
	hook, err := transcodeScaleHook(env(map[string]string{envScaleHookURL: "https://keda.example/scale", envScaleHookSecret: secret}), logger)
	if err != nil || hook.URL != "https://keda.example/scale" || string(hook.Secret) != secret {
		t.Fatalf("set = %+v, %v", hook, err)
	}
	for _, bad := range []map[string]string{
		{envScaleHookURL: "ftp://keda.example/", envScaleHookSecret: secret},
		{envScaleHookURL: "https://keda.example/", envScaleHookSecret: "short"},
	} {
		if _, err := transcodeScaleHook(env(bad), logger); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	if !envBool("true") || !envBool(" 1 ") || envBool("") || envBool("yes") {
		t.Fatal("envBool")
	}
}

// TestTranscodeWorkerPlainHTTPToggle turns the plain-HTTP opt-in on and off
// through the encoding configuration (#664): the admin API reports it, and
// the worker routes admit or refuse a plain-HTTP request without a restart.
func TestTranscodeWorkerPlainHTTPToggle(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	handler := app.Router()
	configureComposedAdministrator(t, handler)
	adminToken := loginComposedAdministrator(t, handler)
	const path = "/deadfin/v1/transcode-workers/plain-http"
	assertAuthenticatedStatus(t, handler, http.MethodGet, path, nil, "", http.StatusUnauthorized)

	connect := func() int {
		request := httptest.NewRequest(http.MethodGet, transcodeworker.PathConnect, nil)
		request.RemoteAddr = "192.0.2.10:5000"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	check := func(want bool) {
		t.Helper()
		var setting dto.TranscodeWorkerPlainHTTP
		getCompositionJSON(t, handler, path, adminToken, &setting)
		if setting != (dto.TranscodeWorkerPlainHTTP{AllowPlainHttp: want, Configured: want}) {
			t.Fatalf("plain-http = %+v, want %v", setting, want)
		}
		// Past the TLS check, a hub with no credential answers 404.
		wantStatus := http.StatusForbidden
		if want {
			wantStatus = http.StatusNotFound
		}
		if got := connect(); got != wantStatus {
			t.Fatalf("plain-HTTP connect = %d, want %d", got, wantStatus)
		}
	}
	setEncoding := func(body string) {
		t.Helper()
		assertAuthenticatedStatus(t, handler, http.MethodPost, "/System/Configuration/encoding", []byte(body), adminToken, http.StatusNoContent)
	}
	check(false)
	setEncoding(`{"AllowPlainHttpTranscodeWorkers":true}`)
	check(true)
	recorder := authenticatedCompositionRequest(handler, http.MethodGet, "/System/Configuration/encoding", nil, adminToken)
	if !strings.Contains(recorder.Body.String(), `"AllowPlainHttpTranscodeWorkers":true`) {
		t.Fatalf("encoding configuration: %s", recorder.Body.String())
	}
	setEncoding(`{"AllowPlainHttpTranscodeWorkers":false}`)
	check(false)
}

func TestTranscodeWorkerPlainHTTPEnvironmentWins(t *testing.T) {
	paths, err := config.NewPaths(filepath.Join(t.TempDir(), "program-data"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := config.Open(t.Context(), paths, events.NewBus[config.Change](clock.NewReal()))
	if err != nil {
		t.Fatal(err)
	}
	on, off := true, false
	if err := manager.SaveNamedConfiguration(t.Context(), config.KeyEncoding, []byte(`{"AllowPlainHttpTranscodeWorkers":true}`)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		environment *bool
		want        dto.TranscodeWorkerPlainHTTP
	}{
		{nil, dto.TranscodeWorkerPlainHTTP{AllowPlainHttp: true, Configured: true}},
		{&off, dto.TranscodeWorkerPlainHTTP{Configured: true, SetByEnvironment: true}},
		{&on, dto.TranscodeWorkerPlainHTTP{AllowPlainHttp: true, Configured: true, SetByEnvironment: true}},
	} {
		setting := newTranscodeWorkerPlainHTTP(tc.environment, manager, slog.New(slog.DiscardHandler))
		if got := setting.setting(); got != tc.want || setting.allowed() != tc.want.AllowPlainHttp {
			t.Errorf("environment %v: setting = %+v, want %+v", tc.environment, got, tc.want)
		}
	}
}
