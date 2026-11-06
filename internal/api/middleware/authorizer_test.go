// SPDX-License-Identifier: GPL-2.0-only

package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
)

var middlewareTestUserID = dto.UUID{0x5b, 0x15, 0xc1, 0x89, 0x2c, 0x4c, 0x4f, 0xd0, 0x9a, 0x3a, 0x8d, 0x4b, 0x2e, 0x9f, 0x0a, 0x67}

func TestAuthorizerEnforcesRouteDecisions(t *testing.T) {
	dependencyFailure := errors.New("dependency unavailable")
	policyFailure := errors.New("policy configuration unavailable")
	userFailure := errors.New("identity store unavailable")

	for _, test := range []struct {
		name         string
		route        router.Route
		identity     auth.Identity
		resolverErr  error
		decision     auth.Decision
		evaluatorErr error
		user         store.User
		userErr      error
		wantStatus   int
		wantHandler  bool
		wantIdentity auth.Identity
	}{
		{
			name:        "anonymous routes bypass failed authentication dependencies",
			route:       router.Route{Policy: "unregistered", Anonymous: true},
			resolverErr: dependencyFailure,
			wantStatus:  http.StatusNoContent,
			wantHandler: true,
		},
		{
			name:       "missing credentials are unauthorized",
			route:      router.Route{Policy: string(auth.PolicyDefaultAuthorization)},
			decision:   auth.DecisionUnauthenticated,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid credentials are unauthorized",
			route:      router.Route{Policy: string(auth.PolicyDefaultAuthorization)},
			identity:   auth.Identity{Token: "unknown-token"},
			decision:   auth.DecisionUnauthenticated,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "authenticated policy denial is forbidden",
			route:      router.Route{Policy: string(auth.PolicyDownload)},
			identity:   auth.Identity{Authenticated: true, UserID: middlewareTestUserID},
			decision:   auth.DecisionForbidden,
			user:       store.User{ID: middlewareTestUserID},
			wantStatus: http.StatusForbidden,
		},
		{
			name:        "resolver errors are internal failures",
			route:       router.Route{Policy: string(auth.PolicyDefaultAuthorization)},
			resolverErr: dependencyFailure,
			wantStatus:  http.StatusInternalServerError,
		},
		{
			name:       "user store errors are internal failures",
			route:      router.Route{Policy: string(auth.PolicyDefaultAuthorization)},
			identity:   auth.Identity{Authenticated: true, UserID: middlewareTestUserID},
			userErr:    userFailure,
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:         "evaluator errors are internal failures",
			route:        router.Route{Policy: string(auth.PolicyDefaultAuthorization)},
			identity:     auth.Identity{Authenticated: true, UserID: middlewareTestUserID},
			user:         store.User{ID: middlewareTestUserID},
			evaluatorErr: policyFailure,
			wantStatus:   http.StatusInternalServerError,
		},
		{
			name:       "configuration decisions are internal failures",
			route:      router.Route{Policy: "unregistered"},
			identity:   auth.Identity{Authenticated: true, UserID: middlewareTestUserID},
			decision:   auth.DecisionConfigurationError,
			user:       store.User{ID: middlewareTestUserID},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:         "allowed identities reach the handler with their context",
			route:        router.Route{Policy: string(auth.PolicyDefaultAuthorization)},
			identity:     auth.Identity{Authenticated: true, UserID: middlewareTestUserID, Token: "active-token"},
			decision:     auth.DecisionAllowed,
			user:         store.User{ID: middlewareTestUserID},
			wantStatus:   http.StatusNoContent,
			wantHandler:  true,
			wantIdentity: auth.Identity{Authenticated: true, UserID: middlewareTestUserID, Token: "active-token"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeResolver{identity: test.identity, err: test.resolverErr}
			evaluator := &fakeEvaluator{decision: test.decision, err: test.evaluatorErr}
			users := &fakeUsers{user: test.user, err: test.userErr}
			authorizer := middleware.NewAuthorizer(resolver, evaluator, users)

			handlerCalled := false
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				handlerCalled = true
				if test.wantIdentity.Authenticated {
					identity, found := middleware.IdentityFromContext(request.Context())
					if !found || identity != test.wantIdentity {
						t.Errorf("handler identity = (%+v, %t), want (%+v, true)", identity, found, test.wantIdentity)
					}
				}
				w.WriteHeader(http.StatusNoContent)
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/items", nil)
			authorizer.Wrap(test.route, handler).ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			if handlerCalled != test.wantHandler {
				t.Errorf("handler called = %t, want %t", handlerCalled, test.wantHandler)
			}
		})
	}
}

// An anonymous route with OptionalIdentity installs a valid credential's
// identity without a policy check, and stays anonymous without one.
func TestAuthorizerOptionalIdentityOnAnonymousRoutes(t *testing.T) {
	user := auth.Identity{Authenticated: true, UserID: middlewareTestUserID, Token: "user-token"}
	for _, test := range []struct {
		name         string
		route        router.Route
		identity     auth.Identity
		resolverErr  error
		wantStatus   int
		wantIdentity bool
	}{
		{name: "plain anonymous route ignores the credential", route: router.Route{Anonymous: true}, identity: user, wantStatus: http.StatusNoContent},
		{name: "valid credential installs its identity", route: router.Route{Anonymous: true, OptionalIdentity: true}, identity: user, wantStatus: http.StatusNoContent, wantIdentity: true},
		{name: "invalid credential stays anonymous", route: router.Route{Anonymous: true, OptionalIdentity: true}, identity: auth.Identity{Token: "unknown-token"}, wantStatus: http.StatusNoContent},
		{name: "resolver errors are internal failures", route: router.Route{Anonymous: true, OptionalIdentity: true}, resolverErr: errors.New("device store unavailable"), wantStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluator := &fakeEvaluator{decision: auth.DecisionForbidden}
			authorizer := middleware.NewAuthorizer(&fakeResolver{identity: test.identity, err: test.resolverErr}, evaluator, &fakeUsers{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				identity, found := middleware.IdentityFromContext(request.Context())
				if found != test.wantIdentity || (found && identity != user) {
					t.Errorf("handler identity = (%+v, %t), want identity %t", identity, found, test.wantIdentity)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			recorder := httptest.NewRecorder()
			authorizer.Wrap(test.route, handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/items", nil))
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
		})
	}
}

// A transcode-worker route never resolves a user credential: the worker
// route authenticates itself, so a user token installs no identity there and
// reaches no policy (#614).
func TestAuthorizerLeavesWorkerRoutesToTheirOwnCredential(t *testing.T) {
	resolver := &fakeResolver{identity: auth.Identity{Authenticated: true, UserID: middlewareTestUserID, Token: "user-token"}}
	evaluator := &fakeEvaluator{decision: auth.DecisionAllowed}
	authorizer := middleware.NewAuthorizer(resolver, evaluator, &fakeUsers{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if identity, found := middleware.IdentityFromContext(request.Context()); found {
			t.Errorf("worker route received identity %+v", identity)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/deadfin/v1/workers/connect", nil)
	request.Header.Set("Authorization", `MediaBrowser Token="user-token"`)
	recorder := httptest.NewRecorder()
	authorizer.Wrap(router.Route{Worker: true}, handler).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || resolver.calls != 0 || evaluator.calls != 0 {
		t.Fatalf("status %d, resolver calls %d, evaluator calls %d; want 204, 0, 0", recorder.Code, resolver.calls, evaluator.calls)
	}
}

// Authorized browsing refreshes the existing session selected by the full
// Client+DeviceId identity. A physical device may have another active client,
// whose activity and request metadata must remain untouched.
func TestSessionActivityWrapperRefreshesOnlyAuthorizedExactClientDeviceSession(t *testing.T) {
	fakeClock := clock.NewFake(time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC))
	sessions := session.New(fakeClock)
	t.Cleanup(func() {
		if err := sessions.Close(); err != nil {
			t.Errorf("close sessions: %v", err)
		}
	})

	browser, err := sessions.LogActivity(t.Context(), session.Activity{
		Client: "Browser", Version: "1.0", DeviceID: "shared-device", DeviceName: "Old Browser", UserID: middlewareTestUserID, RemoteEndPoint: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("create browser session: %v", err)
	}
	fakeClock.Advance(time.Second)
	television, err := sessions.LogActivity(t.Context(), session.Activity{
		Client: "Television", Version: "5.0", DeviceID: "shared-device", DeviceName: "Living Room TV", UserID: middlewareTestUserID, RemoteEndPoint: "192.0.2.11",
	})
	if err != nil {
		t.Fatalf("create television session: %v", err)
	}

	resolver := &fakeResolver{identity: auth.Identity{
		Authenticated: true, Token: "browser-token", UserID: middlewareTestUserID,
		Client: "BROWSER", Device: "Renamed Browser", DeviceID: "shared-device", Version: "2.0",
	}}
	authorizer := middleware.NewAuthorizer(resolver, &fakeEvaluator{decision: auth.DecisionAllowed}, &fakeUsers{user: store.User{ID: middlewareTestUserID}})
	routes := router.New(authorizer.Wrap, middleware.NewSessionActivity(sessions).Wrap)
	routes.Handle(http.MethodGet, "/Items", router.Route{Policy: string(auth.PolicyDefaultAuthorization)}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	fakeClock.Advance(5 * time.Second)
	request := httptest.NewRequest(http.MethodGet, "/Items", nil)
	request.RemoteAddr = "198.51.100.44:8096"
	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("authorized browse status = %d, want %d", recorder.Code, http.StatusNoContent)
	}

	var refreshed, untouched session.Snapshot
	for _, snapshot := range sessions.Sessions(session.Query{}) {
		switch snapshot.ID {
		case browser.ID:
			refreshed = snapshot
		case television.ID:
			untouched = snapshot
		}
	}
	if !refreshed.LastActivity.Equal(fakeClock.Now()) || refreshed.RemoteEndPoint != "198.51.100.44" || refreshed.Client != "BROWSER" || refreshed.Version != "2.0" || refreshed.DeviceName != "Renamed Browser" {
		t.Fatalf("browser activity after authorized browse = %#v, want refreshed request metadata at %s", refreshed, fakeClock.Now())
	}
	if !untouched.LastActivity.Equal(television.LastActivity) || untouched.RemoteEndPoint != television.RemoteEndPoint || untouched.Client != television.Client || untouched.Version != television.Version || untouched.DeviceName != television.DeviceName {
		t.Fatalf("same-device television session changed after browser browse: got %#v, want %#v", untouched, television)
	}
}

func TestAuthorizerUnauthorizedResponsesAreEmpty(t *testing.T) {
	for _, test := range []struct {
		name     string
		identity auth.Identity
	}{
		{name: "missing credentials"},
		{name: "invalid credentials", identity: auth.Identity{Token: "unknown-token"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			authorizer := middleware.NewAuthorizer(
				&fakeResolver{identity: test.identity},
				&fakeEvaluator{decision: auth.DecisionUnauthenticated},
				&fakeUsers{},
			)
			recorder := httptest.NewRecorder()
			authorizer.Wrap(router.Route{Policy: string(auth.PolicyDefaultAuthorization)}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("unauthorized request invoked handler")
			})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/Startup/Configuration", nil))

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
			}
			if body := recorder.Body.String(); body != "" {
				t.Errorf("body = %q, want empty", body)
			}
			if contentType := recorder.Header().Get("Content-Type"); contentType != "" {
				t.Errorf("Content-Type = %q, want absent", contentType)
			}
		})
	}
}

func TestAuthorizerRejectsDisabledIdentity(t *testing.T) {
	resolver := &fakeResolver{identity: auth.Identity{Authenticated: true, UserID: middlewareTestUserID}}
	users := &fakeUsers{user: store.User{ID: middlewareTestUserID, Policy: []byte(`{"IsDisabled":true}`)}}
	evaluator := auth.NewEvaluator(fixedStartup{completed: true}, localNetwork{}, noSchedules{}, func() time.Time { return time.Time{} })
	authorizer := middleware.NewAuthorizer(resolver, evaluator, users)

	handlerCalled := false
	recorder := httptest.NewRecorder()
	authorizer.Wrap(router.Route{Policy: string(auth.PolicyDefaultAuthorization)}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerCalled = true
	})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/items", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if handlerCalled {
		t.Error("disabled identity invoked handler")
	}
}

func TestAuthorizerDistinguishesRemoteAnonymousLANAuthentication(t *testing.T) {
	for _, test := range []struct {
		name       string
		identity   auth.Identity
		decision   auth.Decision
		wantStatus int
	}{
		{name: "missing identity is unauthorized", decision: auth.DecisionForbidden, wantStatus: http.StatusUnauthorized},
		{name: "invalid identity is unauthorized", identity: auth.Identity{Token: "unknown-token"}, decision: auth.DecisionForbidden, wantStatus: http.StatusUnauthorized},
		{name: "authenticated identity denied by LAN policy is forbidden", identity: auth.Identity{Authenticated: true, UserID: middlewareTestUserID}, decision: auth.DecisionForbidden, wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeResolver{identity: test.identity}
			evaluator := &fakeEvaluator{decision: test.decision}
			users := &fakeUsers{user: store.User{ID: middlewareTestUserID}}
			authorizer := middleware.NewAuthorizer(resolver, evaluator, users)

			handlerCalled := false
			request := httptest.NewRequest(http.MethodGet, "/System/Ping", nil)
			request.RemoteAddr = "203.0.113.7:8096"
			recorder := httptest.NewRecorder()
			authorizer.Wrap(router.Route{Policy: string(auth.PolicyAnonymousLANAccess)}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				handlerCalled = true
			})).ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			if handlerCalled {
				t.Error("denied remote request invoked handler")
			}
			if evaluator.calls == 1 && evaluator.requests[0].RemoteIP.String() != "203.0.113.7" {
				t.Errorf("policy remote IP = %s, want 203.0.113.7", evaluator.requests[0].RemoteIP)
			}
		})
	}
}

func TestAuthorizerResolvesJellyfinTokenFormsOnceAndExposesIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		target     string
		headerName string
		header     string
		token      string
	}{
		{name: "MediaBrowser authorization token", target: "/items", headerName: "Authorization", header: `MediaBrowser Token="media-token"`, token: "media-token"},
		{name: "ApiKey query token", target: "/items?ApiKey=query-token", token: "query-token"},
		{name: "legacy header token", target: "/items", headerName: "X-Emby-Token", header: "header-token", token: "header-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			devices := &fakeDeviceTokens{devices: map[string]store.Device{
				test.token: {UserID: middlewareTestUserID, AccessToken: test.token},
			}}
			resolver := auth.NewResolver(auth.ResolverConfig{Devices: devices, EnableLegacyAuthorization: true})
			evaluator := &fakeEvaluator{decision: auth.DecisionAllowed}
			users := &fakeUsers{user: store.User{ID: middlewareTestUserID}}
			authorizer := middleware.NewAuthorizer(resolver, evaluator, users)

			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				identity, found := middleware.IdentityFromContext(request.Context())
				if !found || !identity.Authenticated || identity.Token != test.token || identity.UserID != middlewareTestUserID {
					t.Errorf("context identity = (%+v, %t), want authenticated %q", identity, found, test.token)
				}
				if _, err := resolver.Resolve(request.Context(), request); err != nil {
					t.Errorf("downstream resolution: %v", err)
				}
				w.WriteHeader(http.StatusNoContent)
			})

			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			if test.headerName != "" {
				request.Header.Set(test.headerName, test.header)
			}
			recorder := httptest.NewRecorder()
			authorizer.Wrap(router.Route{Policy: string(auth.PolicyDefaultAuthorization)}, handler).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
			}
			if devices.tokenCalls != 1 || devices.refreshCalls != 1 {
				t.Errorf("resolver lookups = (%d token, %d refresh), want one each", devices.tokenCalls, devices.refreshCalls)
			}
		})
	}
}

func TestAuthorizerReusesAlreadyResolvedRequestCache(t *testing.T) {
	devices := &fakeDeviceTokens{devices: map[string]store.Device{
		"cached-token": {UserID: middlewareTestUserID, AccessToken: "cached-token"},
	}}
	resolver := auth.NewResolver(auth.ResolverConfig{Devices: devices})
	request := httptest.NewRequest(http.MethodGet, "/items?ApiKey=cached-token", nil)
	request = request.WithContext(auth.NewRequestCache(request.Context()))
	resolved, err := resolver.Resolve(request.Context(), request)
	if err != nil {
		t.Fatalf("initial resolution: %v", err)
	}

	authorizer := middleware.NewAuthorizer(resolver, &fakeEvaluator{decision: auth.DecisionAllowed}, &fakeUsers{user: store.User{ID: middlewareTestUserID}})
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		identity, found := middleware.IdentityFromContext(request.Context())
		if !found || identity != resolved {
			t.Errorf("context identity = (%+v, %t), want (%+v, true)", identity, found, resolved)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	authorizer.Wrap(router.Route{Policy: string(auth.PolicyDefaultAuthorization)}, handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if devices.tokenCalls != 1 || devices.refreshCalls != 1 {
		t.Errorf("resolver lookups = (%d token, %d refresh), want one each", devices.tokenCalls, devices.refreshCalls)
	}
}

func TestAuthorizerValidatesUserBeforeAnonymousLANPolicy(t *testing.T) {
	for _, test := range []struct {
		name     string
		remote   string
		user     store.User
		userErr  error
		local    bool
		disabled bool
	}{
		{name: "local stale deleted user", remote: "192.0.2.10", userErr: store.ErrNotFound, local: true},
		{name: "remote stale deleted user", remote: "203.0.113.7", userErr: store.ErrNotFound},
		{name: "local disabled persisted user", remote: "192.0.2.10", user: store.User{ID: middlewareTestUserID, Policy: []byte(`{"IsDisabled":true}`)}, local: true, disabled: true},
		{name: "remote disabled persisted user", remote: "203.0.113.7", user: store.User{ID: middlewareTestUserID, Policy: []byte(`{"IsDisabled":true}`)}, disabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			address := netip.MustParseAddr(test.remote)
			evaluator := middleware.Evaluator(&fakeEvaluator{decision: auth.DecisionAllowed})
			if test.disabled {
				local := map[netip.Addr]bool{}
				if test.local {
					local[address] = true
				}
				evaluator = auth.NewEvaluator(fixedStartup{completed: true}, localNetwork{local: local}, noSchedules{}, func() time.Time { return time.Time{} })
			}
			authorizer := middleware.NewAuthorizer(
				&fakeResolver{identity: auth.Identity{Authenticated: true, UserID: middlewareTestUserID}},
				evaluator,
				&fakeUsers{user: test.user, err: test.userErr},
			)

			handlerCalled := false
			request := httptest.NewRequest(http.MethodGet, "/System/Ping", nil)
			request.RemoteAddr = test.remote + ":8096"
			recorder := httptest.NewRecorder()
			authorizer.Wrap(router.Route{Policy: string(auth.PolicyAnonymousLANAccess)}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				handlerCalled = true
			})).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
			}
			if handlerCalled {
				t.Error("invalid LAN identity invoked handler")
			}
		})
	}
}

type fixedStartup struct{ completed bool }

func (startup fixedStartup) StartupWizardCompleted() bool { return startup.completed }

type localNetwork struct{ local map[netip.Addr]bool }

func (network localNetwork) IsInLocalNetwork(address netip.Addr) bool { return network.local[address] }

type noSchedules struct{}

func (noSchedules) AccessSchedules(context.Context, dto.UUID) ([]store.AccessSchedule, error) {
	return nil, nil
}

type fakeResolver struct {
	identity auth.Identity
	err      error
	calls    int
}

func (resolver *fakeResolver) Resolve(context.Context, *http.Request) (auth.Identity, error) {
	resolver.calls++
	return resolver.identity, resolver.err
}

type fakeEvaluator struct {
	decision auth.Decision
	err      error
	calls    int
	requests []auth.PolicyRequest
}

func (evaluator *fakeEvaluator) Evaluate(_ context.Context, request auth.PolicyRequest) (auth.Decision, error) {
	evaluator.calls++
	evaluator.requests = append(evaluator.requests, request)
	return evaluator.decision, evaluator.err
}

type fakeUsers struct {
	user  store.User
	err   error
	calls int
}

func (users *fakeUsers) Get(context.Context, dto.UUID) (store.User, error) {
	users.calls++
	return users.user, users.err
}

type fakeDeviceTokens struct {
	devices      map[string]store.Device
	tokenCalls   int
	refreshCalls int
}

func (tokens *fakeDeviceTokens) Token(_ context.Context, token string) (store.Device, error) {
	tokens.tokenCalls++
	device, found := tokens.devices[token]
	if !found {
		return store.Device{}, store.ErrNotFound
	}
	return device, nil
}

func (tokens *fakeDeviceTokens) RefreshDeviceIfCurrent(_ context.Context, device store.Device) (store.Device, bool, error) {
	tokens.refreshCalls++
	return device, true, nil
}
