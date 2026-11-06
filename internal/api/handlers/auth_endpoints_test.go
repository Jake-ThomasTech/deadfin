// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	"github.com/deadfin-org/deadfin/internal/user"
)

// Sources: upstream/jellyfin/Jellyfin.Api/Controllers/UserController.cs:84-142,
// 178-340, 467-509, 581-639; SessionController.cs:52-69, 377-391.
// The hidden legacy routes are independent ledger entries because old web clients
// still build their URLs directly; see docs/03-api-surface.md:178-189.
func TestAuthEndpointRouteLedgerPinsMethodsPathsOperationsAndPolicies(t *testing.T) {
	h := newP104Harness(t)

	for _, want := range []router.Route{
		{Method: http.MethodPost, Pattern: "/Users/AuthenticateByName", OperationID: "AuthenticateUserByName", Anonymous: true},
		{Method: http.MethodPost, Pattern: "/Users/{userId}/Authenticate", OperationID: "SHADOW:AuthenticateUser", Anonymous: true},
		{Method: http.MethodGet, Pattern: "/Users/Me", OperationID: "GetCurrentUser", Policy: string(auth.PolicyDefaultAuthorization)},
		{Method: http.MethodGet, Pattern: "/Users/Public", OperationID: "GetPublicUsers", Anonymous: true},
		{Method: http.MethodGet, Pattern: "/Users/{userId}", OperationID: "GetUserById", Policy: string(auth.PolicyIgnoreParentalControl)},
		{Method: http.MethodGet, Pattern: "/Users", OperationID: "GetUsers", Policy: string(auth.PolicyDefaultAuthorization)},
		{Method: http.MethodPost, Pattern: "/Users/Configuration", OperationID: "UpdateUserConfiguration", Policy: string(auth.PolicyDefaultAuthorization)},
		{Method: http.MethodPost, Pattern: "/Users/{userId}/Configuration", OperationID: "SHADOW:UpdateUserConfigurationLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		{Method: http.MethodPost, Pattern: "/Users/Password", OperationID: "UpdateUserPassword", Policy: string(auth.PolicyDefaultAuthorization)},
		{Method: http.MethodPost, Pattern: "/Users/{userId}/Password", OperationID: "SHADOW:UpdateUserPasswordLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		{Method: http.MethodGet, Pattern: "/Sessions", OperationID: "GetSessions", Policy: string(auth.PolicyDefaultAuthorization)},
		{Method: http.MethodPost, Pattern: "/Sessions/Capabilities/Full", OperationID: "PostFullCapabilities", Policy: string(auth.PolicyDefaultAuthorization)},
		{Method: http.MethodPost, Pattern: "/Sessions/Capabilities", OperationID: "PostCapabilities", Policy: string(auth.PolicyDefaultAuthorization)},
	} {
		t.Run(want.Method+" "+want.Pattern, func(t *testing.T) {
			got, found := routeByOperation(h.rt.Routes(), want.OperationID)
			if !found {
				t.Fatalf("operation %q is not registered", want.OperationID)
			}
			if got.Method != want.Method || got.Pattern != want.Pattern || got.Policy != want.Policy || got.Anonymous != want.Anonymous {
				t.Errorf("route %q = %#v, want %#v", want.OperationID, got, want)
			}
		})
	}
}

// Sources: UserController.cs:178-340; auth/login.go:120-200; and
// docs/03-api-surface.md:130-137, 195-213. Login distinguishes a malformed
// request (400), invalid credentials (401), and a verified policy denial (403)
// because web selects a different recovery path for each status.
func TestAuthenticateByNameValidatesCredentialsAndProjectsAuthenticationResult(t *testing.T) {
	h := newP104Harness(t)
	policy := json.RawMessage(`{"IsAdministrator":true,"AccessSchedules":[{"MapKey":"MustNotChangeCase"}]}`)
	configuration := json.RawMessage(`{"AudioLanguagePreference":"en","GroupedFolders":[]}`)
	member := h.createUser(t, "Ada", "correct password", policy, configuration)

	for _, tc := range []struct {
		name     string
		body     string
		metadata string
		deny     bool
		want     int
	}{
		{name: "malformed JSON", body: `{"Username":"Ada","Pw":`, metadata: p104MediaBrowserMetadata, want: http.StatusBadRequest},
		{name: "missing password", body: `{"Username":"Ada"}`, metadata: p104MediaBrowserMetadata, want: http.StatusBadRequest},
		{name: "missing device metadata", body: `{"Username":"Ada","Pw":"correct password"}`, metadata: `MediaBrowser Client="web", Device="browser", Version="1.0"`, want: http.StatusBadRequest},
		{name: "bad credentials", body: `{"Username":"Ada","Pw":"wrong password"}`, metadata: p104MediaBrowserMetadata, want: http.StatusUnauthorized},
		{name: "verified policy denial", body: `{"Username":"Ada","Pw":"correct password"}`, metadata: p104MediaBrowserMetadata, deny: true, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.loginPolicy.err = nil
			if tc.deny {
				h.loginPolicy.err = auth.ErrLoginDenied
			}
			rec := p104Call(h.rt, http.MethodPost, "/Users/AuthenticateByName", tc.body, "application/json", "", "", tc.metadata)
			assertStatus(t, rec, tc.want)
		})
	}
	h.loginPolicy.err = nil

	for _, tc := range []struct {
		name       string
		accept     string
		resultKey  string
		userKey    string
		sessionKey string
	}{
		{name: "Pascal profile", resultKey: "User", userKey: "Id", sessionKey: "SessionInfo"},
		{name: "CamelCase profile", accept: `application/json; profile="CamelCase"`, resultKey: "user", userKey: "id", sessionKey: "sessionInfo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := p104CallFrom(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"Ada","Pw":"correct password"}`, "application/json", tc.accept, "", p104MediaBrowserMetadata, "172.18.0.1:4567")
			assertStatus(t, rec, http.StatusOK)
			result := p104Object(t, rec.Body.Bytes())
			userResult := p104RawObject(t, result[tc.resultKey])
			if got := p104String(t, userResult[tc.userKey]); got != member.ID.String() {
				t.Fatalf("authentication user ID = %q, want N-format %q", got, member.ID.String())
			}
			if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(p104String(t, userResult[tc.userKey])) {
				t.Fatalf("authentication user ID = %s, want 32 lowercase hexadecimal digits", userResult[tc.userKey])
			}
			policyResult := p104RawObject(t, userResult[p104Key(tc.name, "Policy", "policy")])
			schedules := p104RawArray(t, policyResult[p104Key(tc.name, "AccessSchedules", "accessSchedules")])
			if got := p104String(t, p104RawObject(t, schedules[0])["MapKey"]); got != "MustNotChangeCase" {
				t.Fatalf("policy map key was changed by casing negotiation: %q", got)
			}
			sessionResult := p104RawObject(t, result[tc.sessionKey])
			if got := p104String(t, sessionResult[p104Key(tc.name, "DeviceId", "deviceId")]); got != "browser-id" {
				t.Fatalf("session device ID = %q, want browser-id from authentication metadata", got)
			}
			if got := p104String(t, sessionResult[p104Key(tc.name, "RemoteEndPoint", "remoteEndPoint")]); got != "172.18.0.1" {
				t.Fatalf("session remote endpoint = %q, want stock remote host %q", got, "172.18.0.1")
			}
			if got := string(sessionResult[p104Key(tc.name, "AdditionalUsers", "additionalUsers")]); got != "[]" {
				t.Fatalf("empty additional-users session shape = %s, want []", got)
			}
			playState := p104RawObject(t, sessionResult[p104Key(tc.name, "PlayState", "playState")])
			if got := len(playState); got != 5 {
				t.Fatalf("default PlayState fields = %#v, want exact five-field source shape", playState)
			}
			assertJSONFields(t, playState, map[string]any{
				p104Key(tc.name, "CanSeek", "canSeek"): false, p104Key(tc.name, "IsPaused", "isPaused"): false, p104Key(tc.name, "IsMuted", "isMuted"): false, p104Key(tc.name, "RepeatMode", "repeatMode"): "RepeatNone", p104Key(tc.name, "PlaybackOrder", "playbackOrder"): "Default",
			})
			if got := p104String(t, result[p104Key(tc.name, "ServerId", "serverId")]); got != h.identity.ID.String() {
				t.Fatalf("authentication server ID = %q, want %q", got, h.identity.ID.String())
			}
			if token := p104String(t, result[p104Key(tc.name, "AccessToken", "accessToken")]); token == "" {
				t.Fatal("authentication result did not contain an access token")
			}
		})
	}

	legacy := p104Call(h.rt, http.MethodPost, "/Users/"+member.ID.Dashed()+"/Authenticate?pw=correct%20password", "", "", "", "", p104MediaBrowserMetadata)
	assertStatus(t, legacy, http.StatusOK)
	legacyResult := p104Object(t, legacy.Body.Bytes())
	if got := p104String(t, p104RawObject(t, legacyResult["User"])["Id"]); got != member.ID.String() {
		t.Fatalf("legacy authentication user ID = %q, want %q", got, member.ID.String())
	}
	missingLegacy := p104Call(h.rt, http.MethodPost, "/Users/12345678-1234-1234-1234-123456789abc/Authenticate?pw=correct%20password", "", "", "", "", p104MediaBrowserMetadata)
	assertStatus(t, missingLegacy, http.StatusNotFound)
}

func TestPasswordLoginReadsLiveLegacyAuthorizationForMetadataCarriers(t *testing.T) {
	h := newP104Harness(t)
	h.createUser(t, "Live legacy password", "correct password", json.RawMessage(`{"IsHidden":false}`), json.RawMessage(`{}`))
	metadata := `Client="Legacy Password Test", Device="Legacy Password Device", DeviceId="legacy-password-device", Version="1.0"`
	modern := http.Header{"Authorization": {`MediaBrowser ` + metadata}}
	assertStatus(t, p104PasswordLoginWithHeaders(h.rt, modern), http.StatusOK)

	legacyCarriers := []struct {
		name    string
		headers http.Header
	}{
		{name: "Emby authorization scheme", headers: http.Header{"Authorization": {`Emby ` + metadata}}},
		{name: "X-Emby-Authorization", headers: http.Header{"X-Emby-Authorization": {`MediaBrowser ` + metadata}}},
	}
	for _, carrier := range legacyCarriers {
		t.Run("disabled rejects "+carrier.name, func(t *testing.T) {
			assertStatus(t, p104PasswordLoginWithHeaders(h.rt, carrier.headers), http.StatusBadRequest)
		})
	}

	h.legacyAuthorization.Store(true)
	assertStatus(t, p104PasswordLoginWithHeaders(h.rt, modern), http.StatusOK)
	for _, carrier := range legacyCarriers {
		t.Run("enabled accepts "+carrier.name, func(t *testing.T) {
			assertStatus(t, p104PasswordLoginWithHeaders(h.rt, carrier.headers), http.StatusOK)
		})
	}
}

// AuthenticateByName is anonymous at the router layer, so the handler must
// carry the peer identity into Login rather than letting an invalid zero-value
// address accidentally admit a public request.
func TestAuthenticateByNamePassesRemotePeerToRemoteAccessPolicy(t *testing.T) {
	h := newP104HarnessWithLoginNetwork(t, p104LoginNetwork{classifyPeer: true})

	for _, tc := range []struct {
		name              string
		remoteAddr        string
		remoteAccess      bool
		wantStatus        int
		wantRemoteIP      string
		wantValidRemoteIP bool
	}{
		{name: "loopback", remoteAddr: "127.0.0.1:8096", wantStatus: http.StatusOK, wantRemoteIP: "127.0.0.1", wantValidRemoteIP: true},
		{name: "LAN", remoteAddr: "192.168.10.20:8096", wantStatus: http.StatusOK, wantRemoteIP: "192.168.10.20", wantValidRemoteIP: true},
		{name: "public remote allowed", remoteAddr: "198.51.100.20:8096", remoteAccess: true, wantStatus: http.StatusOK, wantRemoteIP: "198.51.100.20", wantValidRemoteIP: true},
		{name: "public remote denied", remoteAddr: "198.51.100.21:8096", wantStatus: http.StatusForbidden, wantRemoteIP: "198.51.100.21", wantValidRemoteIP: true},
		{name: "malformed remote address", remoteAddr: "not-a-network-address", wantStatus: http.StatusOK, wantValidRemoteIP: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "remote-policy-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-")
			h.createUser(t, name, "correct password", json.RawMessage(`{"EnableRemoteAccess":`+strconv.FormatBool(tc.remoteAccess)+`}`), json.RawMessage(`{}`))
			h.loginPolicy.authorizations = nil

			rec := p104CallFrom(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"`+name+`","Pw":"correct password"}`, "application/json", "", "", p104MediaBrowserMetadata, tc.remoteAddr)
			assertStatus(t, rec, tc.wantStatus)
			if len(h.loginPolicy.authorizations) != 1 {
				t.Fatalf("remote policy authorizations = %#v, want one", h.loginPolicy.authorizations)
			}
			got := h.loginPolicy.authorizations[0]
			if got.remoteIP.IsValid() != tc.wantValidRemoteIP {
				t.Fatalf("LoginRequest.RemoteIP valid = %t, want %t for RemoteAddr %q", got.remoteIP.IsValid(), tc.wantValidRemoteIP, tc.remoteAddr)
			}
			if tc.wantValidRemoteIP && got.remoteIP.String() != tc.wantRemoteIP {
				t.Fatalf("LoginRequest.RemoteIP = %q, want host %q (without port) from RemoteAddr %q", got.remoteIP, tc.wantRemoteIP, tc.remoteAddr)
			}
		})
	}
}

func TestLoginAndLegacyAuthenticationProjectSourceCompatibleDefaults(t *testing.T) {
	h := newP104Harness(t)
	member := h.createUser(t, "Seeded", "correct password", json.RawMessage(`{"IsAdministrator":true}`), json.RawMessage(`{}`))

	for _, tc := range []struct {
		name   string
		path   string
		body   string
		accept string
		result string
	}{
		{name: "password login Pascal", path: "/Users/AuthenticateByName", body: `{"Username":"Seeded","Pw":"correct password"}`, result: "User"},
		{name: "password login Camel", path: "/Users/AuthenticateByName", body: `{"Username":"Seeded","Pw":"correct password"}`, accept: `application/json; profile="CamelCase"`, result: "user"},
		{name: "legacy login Pascal", path: "/Users/" + member.ID.Dashed() + "/Authenticate?pw=correct%20password", result: "User"},
		{name: "legacy login Camel", path: "/Users/" + member.ID.Dashed() + "/Authenticate?pw=correct%20password", accept: `application/json; profile="CamelCase"`, result: "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := p104Call(h.rt, http.MethodPost, tc.path, tc.body, "application/json", tc.accept, "", p104MediaBrowserMetadata)
			assertStatus(t, rec, http.StatusOK)
			result := p104Object(t, rec.Body.Bytes())
			userResult := p104RawObject(t, result[tc.result])
			configKey, policyKey := "Configuration", "Policy"
			if tc.accept != "" {
				configKey, policyKey = "configuration", "policy"
			}
			configuration := p104RawObject(t, userResult[configKey])
			policy := p104RawObject(t, userResult[policyKey])
			for _, key := range []string{"PlayDefaultAudioTrack", "HidePlayedInLatest", "RememberAudioSelections", "RememberSubtitleSelections", "EnableNextEpisodeAutoPlay"} {
				if tc.accept != "" {
					key = strings.ToLower(key[:1]) + key[1:]
				}
				if got := string(configuration[key]); got != "true" {
					t.Errorf("configuration %s = %s, want source-compatible true", key, got)
				}
			}
			for _, key := range []string{"GroupedFolders", "OrderedViews", "LatestItemsExcludes", "MyMediaExcludes"} {
				if tc.accept != "" {
					key = strings.ToLower(key[:1]) + key[1:]
				}
				if got := string(configuration[key]); got != "[]" {
					t.Errorf("configuration %s = %s, want non-null empty array", key, got)
				}
			}
			remoteKey := "EnableRemoteAccess"
			if tc.accept != "" {
				remoteKey = "enableRemoteAccess"
			}
			if got := string(policy[remoteKey]); got != "true" {
				t.Errorf("policy %s = %s, want source-compatible true", remoteKey, got)
			}
			camel := tc.accept != ""
			sessionResult := p104RawObject(t, result[p104ProfileKey(camel, "SessionInfo", "sessionInfo")])
			assertNonNull := func(projected map[string]json.RawMessage, pascal, camelCase string) {
				t.Helper()
				key := p104ProfileKey(camel, pascal, camelCase)
				if value, found := projected[key]; !found || string(value) == "null" {
					t.Errorf("%s must include non-null %q: %s", tc.name, key, rec.Body.String())
				}
			}
			assertNonNull(sessionResult, "LastActivityDate", "lastActivityDate")
			assertNonNull(userResult, "LastLoginDate", "lastLoginDate")
			assertNonNull(userResult, "LastActivityDate", "lastActivityDate")
			playState := p104RawObject(t, sessionResult[p104ProfileKey(camel, "PlayState", "playState")])
			assertJSONFields(t, playState, map[string]any{
				p104ProfileKey(camel, "CanSeek", "canSeek"): false, p104ProfileKey(camel, "IsMuted", "isMuted"): false, p104ProfileKey(camel, "IsPaused", "isPaused"): false, p104ProfileKey(camel, "PlaybackOrder", "playbackOrder"): "Default", p104ProfileKey(camel, "RepeatMode", "repeatMode"): "RepeatNone",
			})
		})
	}
}

// A fresh session has no optional playback/device state to serialize. Source
// SessionInfoDto omits nil optional values, retains its required empty queues,
// and formats the zero check-in with seven fractional digits.
func TestAuthenticateByNameProjectsCleanSessionOmissionsAndZeroCheckIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		accept string
		camel  bool
	}{
		{name: "Pascal"},
		{name: "CamelCase", accept: `application/json; profile="CamelCase"`, camel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newP104Harness(t)
			h.createUser(t, "Clean Session", "correct password", json.RawMessage(`{"IsAdministrator":true}`), json.RawMessage(`{}`))

			rec := p104Call(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"Clean Session","Pw":"correct password"}`, "application/json", tc.accept, "", p104MediaBrowserMetadata)
			assertStatus(t, rec, http.StatusOK)
			result := p104Object(t, rec.Body.Bytes())
			sessionInfo := p104RawObject(t, result[p104ProfileKey(tc.camel, "SessionInfo", "sessionInfo")])

			assertJSONFields(t, sessionInfo, map[string]any{
				p104ProfileKey(tc.camel, "AdditionalUsers", "additionalUsers"):         []any{},
				p104ProfileKey(tc.camel, "NowPlayingQueue", "nowPlayingQueue"):         []any{},
				p104ProfileKey(tc.camel, "LastPlaybackCheckIn", "lastPlaybackCheckIn"): "0001-01-01T00:00:00.0000000Z",
			})
			capabilities := p104RawObject(t, sessionInfo[p104ProfileKey(tc.camel, "Capabilities", "capabilities")])
			for _, field := range [][2]string{{"AppStoreUrl", "appStoreUrl"}, {"DeviceProfile", "deviceProfile"}, {"IconUrl", "iconUrl"}} {
				key := p104ProfileKey(tc.camel, field[0], field[1])
				if value, found := capabilities[key]; found {
					t.Errorf("clean SessionInfo capabilities must omit nil %q, got %s", key, value)
				}
			}
			for _, field := range [][2]string{
				{"DeviceType", "deviceType"},
				{"LastPausedDate", "lastPausedDate"},
				{"NowPlayingItem", "nowPlayingItem"},
				{"NowViewingItem", "nowViewingItem"},
				{"PlaylistItemId", "playlistItemId"},
				{"TranscodingInfo", "transcodingInfo"},
				{"UserPrimaryImageTag", "userPrimaryImageTag"},
			} {
				key := p104ProfileKey(tc.camel, field[0], field[1])
				if value, found := sessionInfo[key]; found {
					t.Errorf("clean SessionInfo must omit nil %q, got %s", key, value)
				}
			}
		})
	}
}

// A successful user-session authentication records both account timestamps
// before the handler projects its response. The fake clock makes the exact
// source date representation deterministic without relying on stale fixtures.
func TestAuthenticateByNameProjectsPersistedLoginAndActivityTimes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		accept string
		camel  bool
	}{
		{name: "Pascal"},
		{name: "CamelCase", accept: `application/json; profile="CamelCase"`, camel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newP104Harness(t)
			h.createUser(t, "Timestamped Login", "correct password", json.RawMessage(`{"IsAdministrator":true}`), json.RawMessage(`{}`))

			rec := p104Call(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"Timestamped Login","Pw":"correct password"}`, "application/json", tc.accept, "", p104MediaBrowserMetadata)
			assertStatus(t, rec, http.StatusOK)
			result := p104Object(t, rec.Body.Bytes())
			loggedInUser := p104RawObject(t, result[p104ProfileKey(tc.camel, "User", "user")])
			assertJSONFields(t, loggedInUser, map[string]any{
				p104ProfileKey(tc.camel, "LastLoginDate", "lastLoginDate"):       "2026-07-26T12:00:00.0000000Z",
				p104ProfileKey(tc.camel, "LastActivityDate", "lastActivityDate"): "2026-07-26T12:00:00.0000000Z",
			})
		})
	}
}

// Reporting capabilities changes the active session. A subsequent public
// session read must retain the caller-supplied media and command ordering in
// both the nested capabilities object and the compatibility top-level fields.
func TestSessionCapabilityReportPersistsOrderedPublicProjection(t *testing.T) {
	h := newP104Harness(t)
	owner := h.createUser(t, "Capability Owner", "correct password", json.RawMessage(`{"IsHidden":false}`), json.RawMessage(`{}`))

	login := p104Call(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"Capability Owner","Pw":"correct password"}`, "application/json", `application/json; profile="CamelCase"`, "", p104MediaBrowserMetadata)
	assertStatus(t, login, http.StatusOK)
	h.identities["owner"] = auth.Identity{Authenticated: true, Token: "owner-token", UserID: owner.ID, Client: "web", DeviceID: "browser-id"}

	report := p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities/Full", `{"PlayableMediaTypes":["Video","Audio"],"SupportedCommands":["MoveUp","Play"],"SupportsMediaControl":true,"SupportsPersistentIdentifier":true}`, "application/json", "", "owner", "")
	assertStatus(t, report, http.StatusNoContent)
	p104AssertEmptyBody(t, report)

	listed := p104Call(h.rt, http.MethodGet, "/Sessions?DeviceId=browser-id", "", "", `application/json; profile="CamelCase"`, "owner", "")
	assertStatus(t, listed, http.StatusOK)
	sessions := p104RawArray(t, listed.Body.Bytes())
	if len(sessions) != 1 {
		t.Fatalf("sessions after capability report = %s, want exactly the authenticated session", listed.Body.Bytes())
	}
	projected := p104RawObject(t, sessions[0])
	assertJSONFields(t, p104RawObject(t, projected["capabilities"]), map[string]any{
		"playableMediaTypes":           []string{"Video", "Audio"},
		"supportedCommands":            []string{"MoveUp", "Play"},
		"supportsMediaControl":         true,
		"supportsPersistentIdentifier": true,
	})
	assertJSONFields(t, projected, map[string]any{
		"playableMediaTypes":    []string{"Video", "Audio"},
		"supportedCommands":     []string{"MoveUp", "Play"},
		"supportsRemoteControl": false,
	})
}

// The seeded login replay lists sessions by latest activity while retaining the
// concrete client/device identity and negotiated nested field casing. This is
// deliberately an unfiltered administrator read: the public list, rather than
// a manager snapshot, is the compatibility boundary.
func TestSessionsListOrdersSeededIdentitiesAndRecasesNestedPlayState(t *testing.T) {
	h := newP104Harness(t)
	administrator := h.createUser(t, "Replay Administrator", "correct password", json.RawMessage(`{"IsAdministrator":true,"IsHidden":false}`), json.RawMessage(`{}`))
	h.identities["administrator"] = auth.Identity{Authenticated: true, Administrator: true, Token: "administrator-token", UserID: administrator.ID, Client: "web", DeviceID: "administrator-device"}

	preflight, err := h.sessions.LogActivity(t.Context(), session.Activity{Client: "Goldenrec preflight", Version: "1.0.0", DeviceID: "goldenrec-preflight-v1", DeviceName: "Goldenrec preflight", UserID: administrator.ID})
	if err != nil {
		t.Fatalf("seed preflight session: %v", err)
	}
	h.clock.Advance(2 * time.Second)
	login, err := h.sessions.LogActivity(t.Context(), session.Activity{Client: "Goldenrec login corpus", Version: "1.0.0", DeviceID: "goldenrec-login-corpus", DeviceName: "Goldenrec login corpus", UserID: administrator.ID})
	if err != nil {
		t.Fatalf("seed login session: %v", err)
	}
	if err := h.sessions.ReportCapabilities(t.Context(), session.CapabilityReport{ID: &login.ID, CurrentID: login.ID, Capabilities: session.Capabilities{PlayableMediaTypes: []string{"Audio", "Video"}, SupportedCommands: []string{"Play", "MoveUp"}, SupportsMediaControl: true}}); err != nil {
		t.Fatalf("seed login capabilities: %v", err)
	}

	listed := p104Call(h.rt, http.MethodGet, "/Sessions", "", "", `application/json; profile="CamelCase"`, "administrator", "")
	assertStatus(t, listed, http.StatusOK)
	sessions := p104RawArray(t, listed.Body.Bytes())
	if len(sessions) != 2 {
		t.Fatalf("seeded sessions = %s, want login and preflight sessions", listed.Body.Bytes())
	}
	for index, want := range []struct {
		id, client, deviceID, activity string
	}{
		{login.ID, "Goldenrec login corpus", "goldenrec-login-corpus", "2026-07-26T12:00:02.0000000Z"},
		{preflight.ID, "Goldenrec preflight", "goldenrec-preflight-v1", "2026-07-26T12:00:00.0000000Z"},
	} {
		projected := p104RawObject(t, sessions[index])
		assertJSONFields(t, projected, map[string]any{
			"id":               want.id,
			"client":           want.client,
			"deviceId":         want.deviceID,
			"userId":           administrator.ID.String(),
			"lastActivityDate": want.activity,
		})
	}
	loginProjection := p104RawObject(t, sessions[0])
	assertJSONFields(t, p104RawObject(t, loginProjection["capabilities"]), map[string]any{
		"playableMediaTypes":   []string{"Audio", "Video"},
		"supportedCommands":    []string{"Play", "MoveUp"},
		"supportsMediaControl": true,
	})
	assertJSONFields(t, p104RawObject(t, loginProjection["playState"]), map[string]any{
		"canSeek": false, "isMuted": false, "isPaused": false, "playbackOrder": "Default", "repeatMode": "RepeatNone",
	})
}

// Additional session participants come from the authenticated session snapshot,
// but their public identities must be resolved from persisted users. The response
// preserves the client's participant order in either negotiated casing profile.
func TestSessionsListProjectsOrderedPersistedAdditionalUsers(t *testing.T) {
	h := newP104Harness(t)
	primary := h.createUser(t, "Primary Viewer", "correct password", json.RawMessage(`{"IsAdministrator":true,"IsHidden":false}`), json.RawMessage(`{}`))
	firstAdditional := h.createUser(t, "First Participant", "correct password", json.RawMessage(`{"IsHidden":false}`), json.RawMessage(`{}`))
	secondAdditional := h.createUser(t, "Second Participant", "correct password", json.RawMessage(`{"IsHidden":false}`), json.RawMessage(`{}`))
	h.identities["primary"] = auth.Identity{Authenticated: true, Administrator: true, Token: "primary-token", UserID: primary.ID, Client: "web", DeviceID: "primary-device"}

	participants, err := h.sessions.LogActivity(t.Context(), session.Activity{
		Client: "Participant Client", Version: "1.0", DeviceID: "participants-device", DeviceName: "Participant Device", UserID: primary.ID,
		AdditionalUserIDs: []dto.UUID{secondAdditional.ID, firstAdditional.ID},
	})
	if err != nil {
		t.Fatalf("seed participant session: %v", err)
	}
	empty, err := h.sessions.LogActivity(t.Context(), session.Activity{
		Client: "Empty Client", Version: "1.0", DeviceID: "empty-device", DeviceName: "Empty Device", UserID: primary.ID,
	})
	if err != nil {
		t.Fatalf("seed empty-participant session: %v", err)
	}

	for _, tc := range []struct {
		name, accept, idKey, additionalUsersKey, userIDKey, userNameKey string
	}{
		{name: "PascalCase", idKey: "Id", additionalUsersKey: "AdditionalUsers", userIDKey: "UserId", userNameKey: "UserName"},
		{name: "CamelCase", accept: `application/json; profile="CamelCase"`, idKey: "id", additionalUsersKey: "additionalUsers", userIDKey: "userId", userNameKey: "userName"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listed := p104Call(h.rt, http.MethodGet, "/Sessions", "", "", tc.accept, "primary", "")
			assertStatus(t, listed, http.StatusOK)
			values := p104RawArray(t, listed.Body.Bytes())
			if len(values) != 2 {
				t.Fatalf("session count = %d, want participant and empty sessions: %s", len(values), listed.Body.Bytes())
			}

			byID := make(map[string]map[string]json.RawMessage, len(values))
			for _, value := range values {
				projected := p104RawObject(t, value)
				byID[p104String(t, projected[tc.idKey])] = projected
			}
			participantProjection, found := byID[participants.ID]
			if !found {
				t.Fatalf("participant session %q absent from %s", participants.ID, listed.Body.Bytes())
			}
			additionalUsers := p104RawArray(t, participantProjection[tc.additionalUsersKey])
			want := []struct{ id, name string }{
				{secondAdditional.ID.String(), secondAdditional.Name},
				{firstAdditional.ID.String(), firstAdditional.Name},
			}
			if len(additionalUsers) != len(want) {
				t.Fatalf("additional users = %s, want %#v (and no primary user)", participantProjection[tc.additionalUsersKey], want)
			}
			for index, expected := range want {
				actual := p104RawObject(t, additionalUsers[index])
				if got := p104String(t, actual[tc.userIDKey]); got != expected.id {
					t.Fatalf("additional user %d ID = %q, want %q", index, got, expected.id)
				}
				if got := p104String(t, actual[tc.userNameKey]); got != expected.name {
					t.Fatalf("additional user %d name = %q, want %q", index, got, expected.name)
				}
			}

			emptyProjection, found := byID[empty.ID]
			if !found {
				t.Fatalf("empty participant session %q absent from %s", empty.ID, listed.Body.Bytes())
			}
			if got := string(emptyProjection[tc.additionalUsersKey]); got != "[]" {
				t.Fatalf("empty session additional users = %s, want []", got)
			}
		})
	}
}

func TestUsersMeProjectsSourceKnownDTOFacts(t *testing.T) {
	h := newP104Harness(t)
	member := h.createUser(t, "Stock Facts", "correct password", json.RawMessage(`{"IsHidden":false}`), json.RawMessage(`{"CastReceiverId":"F007D354"}`))
	login := p104Call(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"Stock Facts","Pw":"correct password"}`, "application/json", "", "", p104MediaBrowserMetadata)
	assertStatus(t, login, http.StatusOK)
	result := p104Object(t, login.Body.Bytes())
	h.identities["member"] = auth.Identity{Authenticated: true, Token: p104String(t, result["AccessToken"]), UserID: member.ID, DeviceID: "browser-id", Client: "web"}

	for _, tc := range []struct {
		name   string
		accept string
		camel  bool
	}{
		{name: "Pascal"},
		{name: "CamelCase", accept: `application/json; profile="CamelCase"`, camel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := p104Call(h.rt, http.MethodGet, "/Users/Me", "", "", tc.accept, "member", "")
			assertStatus(t, rec, http.StatusOK)
			body := p104Object(t, rec.Body.Bytes())
			configuration := p104RawObject(t, body[p104ProfileKey(tc.camel, "Configuration", "configuration")])
			assertJSONFields(t, configuration, map[string]any{p104ProfileKey(tc.camel, "CastReceiverId", "castReceiverId"): "F007D354"})

			assertJSONFields(t, body, map[string]any{
				p104ProfileKey(tc.camel, "Id", "id"):                             member.ID.String(),
				p104ProfileKey(tc.camel, "LastActivityDate", "lastActivityDate"): "2026-07-26T12:00:00.0000000Z",
				p104ProfileKey(tc.camel, "LastLoginDate", "lastLoginDate"):       "2026-07-26T12:00:00.0000000Z",
			})

			policy := p104RawObject(t, body[p104ProfileKey(tc.camel, "Policy", "policy")])
			for _, field := range [][2]string{{"MaxParentalRating", "maxParentalRating"}, {"MaxParentalSubRating", "maxParentalSubRating"}} {
				key := p104ProfileKey(tc.camel, field[0], field[1])
				if _, found := policy[key]; found {
					t.Errorf("stock /Users/Me policy omits %q, not null: %s", key, rec.Body.String())
				}
			}
			for _, field := range [][2]string{{"PrimaryImageAspectRatio", "primaryImageAspectRatio"}, {"PrimaryImageTag", "primaryImageTag"}, {"ServerName", "serverName"}} {
				key := p104ProfileKey(tc.camel, field[0], field[1])
				if _, found := body[key]; found {
					t.Errorf("stock /Users/Me omits %q, not null or empty: %s", key, rec.Body.String())
				}
			}
		})
	}
}

func p104ProfileKey(camel bool, pascal, camelCase string) string {
	if camel {
		return camelCase
	}
	return pascal
}

// Sources: UserController.cs:84-142 and 467-509. User DTOs are public outputs:
// IDs serialize as N-format GUIDs, profile casing applies only to DTO fields, and
// a missing current user is a BadRequest rather than a fabricated empty user.
func TestUserReadRoutesProjectUsersAndMapMissingAndPolicyOutcomes(t *testing.T) {
	h := newP104Harness(t)
	ada := h.createUser(t, "Ada", "ada password", json.RawMessage(`{"IsAdministrator":true,"IsHidden":false}`), json.RawMessage(`{"GroupedFolders":[]}`))
	ben := h.createUser(t, "Ben", "ben password", json.RawMessage(`{"IsHidden":false,"EnableUserPreferenceAccess":true}`), json.RawMessage(`{"AudioLanguagePreference":"fr"}`))
	h.identities["ada"] = auth.Identity{Authenticated: true, Administrator: true, Token: "ada-token", UserID: ada.ID, DeviceID: "ada-device", Client: "web"}
	h.identities["ben"] = auth.Identity{Authenticated: true, Token: "ben-token", UserID: ben.ID, DeviceID: "ben-device", Client: "web"}
	h.identities["deny"] = auth.Identity{Authenticated: true, Token: "deny", UserID: ben.ID, DeviceID: "ben-device", Client: "web"}
	h.identities["missing"] = auth.Identity{Authenticated: true, Token: "missing", UserID: p104UUID(t, "b56d517e-2df1-4e9c-a331-502a5f10a899")}

	for _, tc := range []struct {
		name   string
		path   string
		actor  string
		accept string
		want   int
	}{
		{name: "anonymous public users", path: "/Users/Public", want: http.StatusOK},
		{name: "current user", path: "/Users/Me", actor: "ben", want: http.StatusOK},
		{name: "administrator list", path: "/Users", actor: "ada", accept: `application/json; profile="CamelCase"`, want: http.StatusOK},
		{name: "get user by dashed route ID", path: "/Users/" + ben.ID.Dashed(), actor: "ben", want: http.StatusOK},
		{name: "malformed route ID", path: "/Users/not-a-guid", actor: "ben", want: http.StatusBadRequest},
		{name: "absent route ID", path: "/Users/12345678-1234-1234-1234-123456789abc", actor: "ben", want: http.StatusNotFound},
		{name: "stale current identity", path: "/Users/Me", actor: "missing", want: http.StatusBadRequest},
		{name: "no identity", path: "/Users", want: http.StatusUnauthorized},
		{name: "policy denial", path: "/Users/Me", actor: "deny", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := p104Call(h.rt, http.MethodGet, tc.path, "", "", tc.accept, tc.actor, "")
			assertStatus(t, rec, tc.want)
		})
	}

	public := p104RawArray(t, p104Call(h.rt, http.MethodGet, "/Users/Public", "", "", "", "", "").Body.Bytes())
	if len(public) != 2 {
		t.Fatalf("public users = %s, want both visible users", public)
	}
	me := p104Object(t, p104Call(h.rt, http.MethodGet, "/Users/Me", "", "", "", "ben", "").Body.Bytes())
	if got := p104String(t, me["Id"]); got != ben.ID.String() {
		t.Fatalf("GET /Users/Me ID = %q, want N-format %q", got, ben.ID.String())
	}
	listed := p104RawArray(t, p104Call(h.rt, http.MethodGet, "/Users", "", "", `application/json; profile="CamelCase"`, "ada", "").Body.Bytes())
	if got := p104String(t, p104RawObject(t, listed[0])["name"]); got != "Ada" {
		t.Fatalf("camel-cased user list first name = %q, want alphabetical Ada", got)
	}
}

// Public users are selected at the anonymous HTTP boundary with the peer and
// MediaBrowser device facts of the actual request. A host:port peer must be
// normalized before locality is evaluated, and DeviceId is case-insensitive.
func TestPublicUsersApplyPeerAndMediaBrowserDeviceVisibility(t *testing.T) {
	h := newP104Harness(t)
	h.createUser(t, "Remote disabled", "remote-disabled-password", json.RawMessage(`{"IsHidden":false,"IsDisabled":false,"EnableRemoteAccess":false,"EnableAllDevices":true}`), json.RawMessage(`{}`))
	h.createUser(t, "Device restricted", "device-restricted-password", json.RawMessage(`{"IsHidden":false,"IsDisabled":false,"EnableRemoteAccess":true,"EnableAllDevices":false,"EnabledDevices":["living-room-device"]}`), json.RawMessage(`{}`))

	for _, tc := range []struct {
		name       string
		remoteAddr string
		metadata   string
		want       map[string]struct{}
	}{
		{
			name:       "normalized LAN peer may select remote-disabled users",
			remoteAddr: "192.168.10.20:8096",
			metadata:   `MediaBrowser Client="web", Device="browser", DeviceId="other-device", Version="1.0"`,
			want:       map[string]struct{}{"Remote disabled": {}},
		},
		{
			name:       "remote matching MediaBrowser DeviceId may select device-restricted users",
			remoteAddr: "203.0.113.24:8096",
			metadata:   `MediaBrowser Client="web", Device="browser", DeviceId="LIVING-ROOM-DEVICE", Version="1.0"`,
			want:       map[string]struct{}{"Device restricted": {}},
		},
		{
			name:       "remote unmatched MediaBrowser DeviceId cannot select device-restricted users",
			remoteAddr: "203.0.113.24:8096",
			metadata:   `MediaBrowser Client="web", Device="browser", DeviceId="other-device", Version="1.0"`,
			want:       map[string]struct{}{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := p104CallFrom(h.rt, http.MethodGet, "/Users/Public", "", "", "", "", tc.metadata, tc.remoteAddr)
			assertStatus(t, rec, http.StatusOK)

			users := p104RawArray(t, rec.Body.Bytes())
			got := make(map[string]struct{}, len(users))
			for _, value := range users {
				got[p104String(t, p104RawObject(t, value)["Name"])] = struct{}{}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("public users = %#v, want %#v: %s", got, tc.want, rec.Body.String())
			}
			for name := range tc.want {
				if _, found := got[name]; !found {
					t.Errorf("public users = %#v, want %q", got, name)
				}
			}
		})
	}
}

// Local-only accounts must be discoverable and admit password login from the
// same peer classes. This exercises the independent public-user and login
// paths so a future locality change cannot widen one without the other.
func TestPublicUsersAndLoginAgreeOnLocalPeerClasses(t *testing.T) {
	h := newP104HarnessWithLoginNetwork(t, p104LoginNetwork{classifyPeer: true})

	for _, tc := range []struct {
		name       string
		remoteAddr string
		local      bool
	}{
		{name: "IPv4 loopback", remoteAddr: "127.0.0.1:8096", local: true},
		{name: "IPv6 loopback", remoteAddr: "[::1]:8096", local: true},
		{name: "RFC1918 IPv4", remoteAddr: "192.168.10.20:8096", local: true},
		{name: "ULA IPv6", remoteAddr: "[fd00::20]:8096", local: true},
		{name: "IPv6 link-local", remoteAddr: "[fe80::20]:8096", local: true},
		{name: "IPv4 link-local", remoteAddr: "169.254.20.20:8096", local: true},
		{name: "public IPv4", remoteAddr: "198.51.100.20:8096", local: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "locality-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-")
			h.createUser(t, name, "correct password", json.RawMessage(`{"IsHidden":false,"IsDisabled":false,"EnableRemoteAccess":false,"EnableAllDevices":true}`), json.RawMessage(`{}`))

			public := p104CallFrom(h.rt, http.MethodGet, "/Users/Public", "", "", "", "", p104MediaBrowserMetadata, tc.remoteAddr)
			assertStatus(t, public, http.StatusOK)
			listed := false
			for _, value := range p104RawArray(t, public.Body.Bytes()) {
				if p104String(t, p104RawObject(t, value)["Name"]) == name {
					listed = true
					break
				}
			}
			if listed != tc.local {
				t.Fatalf("GET /Users/Public listed local-only user = %t for peer %q, want %t; body: %s", listed, tc.remoteAddr, tc.local, public.Body.String())
			}

			login := p104CallFrom(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"`+name+`","Pw":"correct password"}`, "application/json", "", "", p104MediaBrowserMetadata, tc.remoteAddr)
			wantStatus := http.StatusForbidden
			if tc.local {
				wantStatus = http.StatusOK
			}
			assertStatus(t, login, wantStatus)
			if admitted := login.Code == http.StatusOK; admitted != listed {
				t.Fatalf("login admission = %t but public visibility = %t for peer %q", admitted, listed, tc.remoteAddr)
			}
		})
	}
}

// Nullable isHidden and isDisabled queries select policy combinations, and
// response casing must not change which persisted users are selected.
func TestUsersListBindsNullableVisibilityFiltersAcrossProfiles(t *testing.T) {
	h := newP104Harness(t)
	hiddenEnabled := h.createUser(t, "Hidden enabled", "hidden-enabled-password", json.RawMessage(`{"IsHidden":true,"IsDisabled":false}`), json.RawMessage(`{}`))
	visibleDisabled := h.createUser(t, "Visible disabled", "visible-disabled-password", json.RawMessage(`{"IsHidden":false,"IsDisabled":true}`), json.RawMessage(`{}`))
	administrator := h.createUser(t, "Administrator", "administrator-password", json.RawMessage(`{"IsAdministrator":true,"IsHidden":true,"IsDisabled":true}`), json.RawMessage(`{}`))
	h.identities["administrator"] = auth.Identity{Authenticated: true, Administrator: true, Token: "administrator-token", UserID: administrator.ID, DeviceID: "administrator-device", Client: "web"}

	for _, profile := range []struct {
		name    string
		accept  string
		nameKey string
	}{
		{name: "Pascal", nameKey: "Name"},
		{name: "CamelCase", accept: `application/json; profile="CamelCase"`, nameKey: "name"},
	} {
		for _, tc := range []struct {
			name  string
			query string
			want  store.User
		}{
			{name: "hidden enabled", query: "?isHidden=true&isDisabled=false", want: hiddenEnabled},
			{name: "visible disabled", query: "?isHidden=false&isDisabled=true", want: visibleDisabled},
		} {
			t.Run(profile.name+"/"+tc.name, func(t *testing.T) {
				rec := p104Call(h.rt, http.MethodGet, "/Users"+tc.query, "", "", profile.accept, "administrator", "")
				assertStatus(t, rec, http.StatusOK)
				listed := p104RawArray(t, rec.Body.Bytes())
				if len(listed) != 1 {
					t.Fatalf("GET /Users%s returned %d users, want only %q: %s", tc.query, len(listed), tc.want.Name, rec.Body.String())
				}
				if got := p104String(t, p104RawObject(t, listed[0])[profile.nameKey]); got != tc.want.Name {
					t.Errorf("GET /Users%s first name = %q, want %q", tc.query, got, tc.want.Name)
				}
			})
		}
	}
}

// UserDto's obsolete password fields are still public wire contract fields.
// Every endpoint that projects a user must emit the source-compatible values.
func TestUserDTOProjectsSourcePasswordFlagsAcrossReadAndLoginEndpoints(t *testing.T) {
	h := newP104Harness(t)
	member := h.createUser(t, "Password flags", "member-password", json.RawMessage(`{"IsHidden":false,"IsDisabled":false}`), json.RawMessage(`{}`))
	administrator := h.createUser(t, "Password administrator", "administrator-password", json.RawMessage(`{"IsAdministrator":true,"IsHidden":true,"IsDisabled":true}`), json.RawMessage(`{}`))
	h.identities["member"] = auth.Identity{Authenticated: true, Token: "member-token", UserID: member.ID, DeviceID: "member-device", Client: "web"}
	h.identities["administrator"] = auth.Identity{Authenticated: true, Administrator: true, Token: "administrator-token", UserID: administrator.ID, DeviceID: "administrator-device", Client: "web"}

	for _, profile := range []struct {
		name   string
		accept string
		camel  bool
	}{
		{name: "Pascal"},
		{name: "CamelCase", accept: `application/json; profile="CamelCase"`, camel: true},
	} {
		for _, endpoint := range []struct {
			name   string
			method string
			path   string
			body   string
			actor  string
			user   func(*testing.T, []byte, bool) map[string]json.RawMessage
		}{
			{
				name:   "public",
				method: http.MethodGet,
				path:   "/Users/Public",
				user: func(t *testing.T, body []byte, camel bool) map[string]json.RawMessage {
					t.Helper()
					for _, value := range p104RawArray(t, body) {
						projected := p104RawObject(t, value)
						if p104String(t, projected[p104ProfileKey(camel, "Id", "id")]) == member.ID.String() {
							return projected
						}
					}
					t.Fatalf("GET /Users/Public did not project user %q: %s", member.ID, body)
					return nil
				},
			},
			{
				name:   "me",
				method: http.MethodGet,
				path:   "/Users/Me",
				actor:  "member",
				user: func(t *testing.T, body []byte, _ bool) map[string]json.RawMessage {
					t.Helper()
					return p104Object(t, body)
				},
			},
			{
				name:   "list",
				method: http.MethodGet,
				path:   "/Users",
				actor:  "administrator",
				user: func(t *testing.T, body []byte, camel bool) map[string]json.RawMessage {
					t.Helper()
					for _, value := range p104RawArray(t, body) {
						projected := p104RawObject(t, value)
						if p104String(t, projected[p104ProfileKey(camel, "Id", "id")]) == member.ID.String() {
							return projected
						}
					}
					t.Fatalf("GET /Users did not project user %q: %s", member.ID, body)
					return nil
				},
			},
			{
				name:   "login",
				method: http.MethodPost,
				path:   "/Users/AuthenticateByName",
				body:   `{"Username":"Password flags","Pw":"member-password"}`,
				user: func(t *testing.T, body []byte, camel bool) map[string]json.RawMessage {
					t.Helper()
					return p104RawObject(t, p104Object(t, body)[p104ProfileKey(camel, "User", "user")])
				},
			},
		} {
			t.Run(profile.name+"/"+endpoint.name, func(t *testing.T) {
				rec := p104Call(h.rt, endpoint.method, endpoint.path, endpoint.body, "application/json", profile.accept, endpoint.actor, p104MediaBrowserMetadata)
				assertStatus(t, rec, http.StatusOK)
				projected := endpoint.user(t, rec.Body.Bytes(), profile.camel)
				assertJSONFields(t, projected, map[string]any{
					p104ProfileKey(profile.camel, "HasPassword", "hasPassword"):                             true,
					p104ProfileKey(profile.camel, "HasConfiguredPassword", "hasConfiguredPassword"):         true,
					p104ProfileKey(profile.camel, "HasConfiguredEasyPassword", "hasConfiguredEasyPassword"): false,
				})
			})
		}
	}
}

// Sources: UserController.cs:261-318 and 460-509; user/user.go:235-297.
// The unscoped operations select UserId from query or the authenticated caller;
// legacy scoped routes must execute the same authorization transition.
func TestUserConfigurationAndPasswordRoutesHonorTargetsCredentialsAndLegacyAliases(t *testing.T) {
	h := newP104Harness(t)
	admin := h.createUser(t, "Admin", "admin password", json.RawMessage(`{"IsAdministrator":true,"IsHidden":false}`), json.RawMessage(`{"GroupedFolders":[]}`))
	member := h.createUser(t, "Member", "old password", json.RawMessage(`{"IsHidden":false,"EnableUserPreferenceAccess":true}`), json.RawMessage(`{"GroupedFolders":[]}`))
	h.identities["admin"] = auth.Identity{Authenticated: true, Administrator: true, Token: "admin-token", UserID: admin.ID, DeviceID: "admin-device", Client: "web"}
	h.identities["member"] = auth.Identity{Authenticated: true, Token: "member-token", UserID: member.ID, DeviceID: "member-device", Client: "web"}

	unscopedConfiguration := p104Call(h.rt, http.MethodPost, "/Users/Configuration?userId="+member.ID.Dashed(), `{"AudioLanguagePreference":"ja","GroupedFolders":[]}`, "application/json", "", "admin", "")
	assertStatus(t, unscopedConfiguration, http.StatusNoContent)
	p104AssertEmptyBody(t, unscopedConfiguration)
	configured := p104Object(t, p104Call(h.rt, http.MethodGet, "/Users/"+member.ID.Dashed(), "", "", "", "member", "").Body.Bytes())
	if got := p104String(t, p104RawObject(t, configured["Configuration"])["AudioLanguagePreference"]); got != "ja" {
		t.Fatalf("unscoped configuration update visible through user projection = %q, want ja", got)
	}

	legacyConfiguration := p104Call(h.rt, http.MethodPost, "/Users/"+member.ID.Dashed()+"/Configuration", `{"AudioLanguagePreference":"de","GroupedFolders":[]}`, "application/json", "", "member", "")
	assertStatus(t, legacyConfiguration, http.StatusNoContent)
	p104AssertEmptyBody(t, legacyConfiguration)
	deniedConfiguration := p104Call(h.rt, http.MethodPost, "/Users/"+admin.ID.Dashed()+"/Configuration", `{"AudioLanguagePreference":"it"}`, "application/json", "", "member", "")
	assertStatus(t, deniedConfiguration, http.StatusForbidden)
	malformedConfiguration := p104Call(h.rt, http.MethodPost, "/Users/Configuration", `{"AudioLanguagePreference":`, "application/json", "", "member", "")
	assertStatus(t, malformedConfiguration, http.StatusBadRequest)
	unauthenticatedConfiguration := p104Call(h.rt, http.MethodPost, "/Users/Configuration", `{}`, "application/json", "", "", "")
	assertStatus(t, unauthenticatedConfiguration, http.StatusUnauthorized)

	changePassword := p104Call(h.rt, http.MethodPost, "/Users/Password", `{"CurrentPw":"old password","NewPw":"new password"}`, "application/json", "", "member", "")
	assertStatus(t, changePassword, http.StatusNoContent)
	p104AssertEmptyBody(t, changePassword)
	if verified, err := h.users.VerifyPassword(t.Context(), member.ID, "new password"); err != nil || !verified {
		t.Fatalf("unscoped password update did not establish the new credential: verified=%t err=%v", verified, err)
	}
	missingCurrent := p104Call(h.rt, http.MethodPost, "/Users/Password", `{"NewPw":"should not win"}`, "application/json", "", "member", "")
	assertStatus(t, missingCurrent, http.StatusForbidden)
	if verified, err := h.users.VerifyPassword(t.Context(), member.ID, "new password"); err != nil || !verified {
		t.Fatalf("missing-current-password request changed credential: verified=%t err=%v", verified, err)
	}

	resetPassword := p104Call(h.rt, http.MethodPost, "/Users/"+member.ID.Dashed()+"/Password", `{"ResetPassword":true}`, "application/json", "", "admin", "")
	assertStatus(t, resetPassword, http.StatusNoContent)
	p104AssertEmptyBody(t, resetPassword)
	if verified, err := h.users.VerifyPassword(t.Context(), member.ID, "new password"); err != nil || verified {
		t.Fatalf("legacy reset left the old credential valid: verified=%t err=%v", verified, err)
	}
	adminReset := p104Call(h.rt, http.MethodPost, "/Users/"+admin.ID.Dashed()+"/Password", `{"ResetPassword":true}`, "application/json", "", "admin", "")
	assertStatus(t, adminReset, http.StatusBadRequest)
}

// UserController binds named user enums at its HTTP boundary. Invalid values
// must surface as the same Bad Request response as other binding failures and
// must not replace a previously accepted configuration or policy document.
func TestUserConfigurationAndPolicyEnumBindingRejectsInvalidValuesWithoutPersistence(t *testing.T) {
	h := newP104Harness(t)
	administrator := h.createUser(t, "Administrator", "administrator password", json.RawMessage(`{"IsAdministrator":true,"IsHidden":false}`), json.RawMessage(`{}`))
	member := h.createUser(t, "Member", "member password", json.RawMessage(`{"IsHidden":false,"EnableUserPreferenceAccess":true,"SyncPlayAccess":"CreateAndJoinGroups"}`), json.RawMessage(`{"SubtitleMode":"Default"}`))
	h.identities["administrator"] = auth.Identity{Authenticated: true, Administrator: true, Token: "administrator-token", UserID: administrator.ID, DeviceID: "administrator-device", Client: "web"}
	h.identities["member"] = auth.Identity{Authenticated: true, Token: "member-token", UserID: member.ID, DeviceID: "member-device", Client: "web"}

	t.Run("every SubtitleMode member round-trips through self configuration", func(t *testing.T) {
		for _, mode := range []dto.SubtitleMode{
			dto.SubtitleModeDefault,
			dto.SubtitleModeAlways,
			dto.SubtitleModeOnlyForced,
			dto.SubtitleModeNone,
			dto.SubtitleModeSmart,
		} {
			t.Run(string(mode), func(t *testing.T) {
				rec := p104Call(h.rt, http.MethodPost, "/Users/Configuration", `{"SubtitleMode":"`+string(mode)+`"}`, "application/json", "", "member", "")
				assertStatus(t, rec, http.StatusNoContent)
				p104AssertEmptyBody(t, rec)

				userResponse := p104Call(h.rt, http.MethodGet, "/Users/"+member.ID.Dashed(), "", "", "", "member", "")
				assertStatus(t, userResponse, http.StatusOK)
				configuration := p104RawObject(t, p104Object(t, userResponse.Body.Bytes())["Configuration"])
				if got := p104String(t, configuration["SubtitleMode"]); got != string(mode) {
					t.Fatalf("persisted SubtitleMode = %q, want %q", got, mode)
				}
			})
		}
	})

	t.Run("invalid SubtitleMode returns binding error and retains accepted configuration", func(t *testing.T) {
		baseline := p104Call(h.rt, http.MethodPost, "/Users/Configuration", `{"SubtitleMode":"Always","AudioLanguagePreference":"en"}`, "application/json", "", "member", "")
		assertStatus(t, baseline, http.StatusNoContent)

		invalid := p104Call(h.rt, http.MethodPost, "/Users/Configuration", `{"SubtitleMode":"Unexpected","AudioLanguagePreference":"de"}`, "application/json", "", "member", "")
		assertStatus(t, invalid, http.StatusBadRequest)
		if got := invalid.Body.String(); got != "Bad Request\n" {
			t.Fatalf("invalid SubtitleMode response = %q, want source binding error", got)
		}

		userResponse := p104Call(h.rt, http.MethodGet, "/Users/"+member.ID.Dashed(), "", "", "", "member", "")
		assertStatus(t, userResponse, http.StatusOK)
		configuration := p104RawObject(t, p104Object(t, userResponse.Body.Bytes())["Configuration"])
		assertJSONFields(t, configuration, map[string]any{"SubtitleMode": "Always", "AudioLanguagePreference": "en"})
	})

	t.Run("every SyncPlayAccess member round-trips through administrator policy update", func(t *testing.T) {
		for _, access := range []dto.SyncPlayAccess{
			dto.SyncPlayAccessCreateAndJoinGroups,
			dto.SyncPlayAccessJoinGroups,
			dto.SyncPlayAccessNone,
		} {
			t.Run(string(access), func(t *testing.T) {
				rec := p104Call(h.rt, http.MethodPost, "/Users/"+member.ID.Dashed()+"/Policy", `{"SyncPlayAccess":"`+string(access)+`"}`, "application/json", "", "administrator", "")
				assertStatus(t, rec, http.StatusNoContent)
				p104AssertEmptyBody(t, rec)

				userResponse := p104Call(h.rt, http.MethodGet, "/Users/"+member.ID.Dashed(), "", "", "", "member", "")
				assertStatus(t, userResponse, http.StatusOK)
				policy := p104RawObject(t, p104Object(t, userResponse.Body.Bytes())["Policy"])
				if got := p104String(t, policy["SyncPlayAccess"]); got != string(access) {
					t.Fatalf("persisted SyncPlayAccess = %q, want %q", got, access)
				}
			})
		}
	})

	t.Run("invalid SyncPlayAccess returns binding error and retains accepted policy", func(t *testing.T) {
		baseline := p104Call(h.rt, http.MethodPost, "/Users/"+member.ID.Dashed()+"/Policy", `{"SyncPlayAccess":"JoinGroups"}`, "application/json", "", "administrator", "")
		assertStatus(t, baseline, http.StatusNoContent)

		invalid := p104Call(h.rt, http.MethodPost, "/Users/"+member.ID.Dashed()+"/Policy", `{"SyncPlayAccess":"Unexpected"}`, "application/json", "", "administrator", "")
		assertStatus(t, invalid, http.StatusBadRequest)
		if got := invalid.Body.String(); got != "Bad Request\n" {
			t.Fatalf("invalid SyncPlayAccess response = %q, want source binding error", got)
		}

		userResponse := p104Call(h.rt, http.MethodGet, "/Users/"+member.ID.Dashed(), "", "", "", "member", "")
		assertStatus(t, userResponse, http.StatusOK)
		policy := p104RawObject(t, p104Object(t, userResponse.Body.Bytes())["Policy"])
		if got := p104String(t, policy["SyncPlayAccess"]); got != "JoinGroups" {
			t.Fatalf("policy after invalid SyncPlayAccess = %q, want accepted JoinGroups value", got)
		}
	})
}

// TestDefaultUserDTOReportsSyncPlayAccessNone verifies that a user created
// without an explicit SyncPlayAccess policy reports Policy.SyncPlayAccess=None
// in the projected user DTO. The default capability for SyncPlay is disabled.
func TestDefaultUserDTOReportsSyncPlayAccessNone(t *testing.T) {
	h := newP104Harness(t)
	member := h.createUser(t, "Default Member", "member password", json.RawMessage(`{"IsHidden":false}`), json.RawMessage(`{}`))
	h.identities["member"] = auth.Identity{Authenticated: true, Token: "member-token", UserID: member.ID, DeviceID: "member-device", Client: "web"}

	rec := p104Call(h.rt, http.MethodGet, "/Users/"+member.ID.Dashed(), "", "", "", "member", "")
	assertStatus(t, rec, http.StatusOK)
	policy := p104RawObject(t, p104Object(t, rec.Body.Bytes())["Policy"])
	if got := p104String(t, policy["SyncPlayAccess"]); got != "None" {
		t.Fatalf("default user DTO Policy.SyncPlayAccess = %q, want %q", got, "None")
	}
}

// A literal JSON null is syntactically valid, but it is not a UserPolicy
// document. Reject it before the persisted policy can lose its object shape or
// invalidate the account's existing authentication paths.
func TestUserPolicyUpdateRejectsLiteralNullWithoutMutatingLoginableAccount(t *testing.T) {
	h := newP104Harness(t)
	administrator := h.createUser(t, "Policy Administrator", "administrator password", json.RawMessage(`{"IsAdministrator":true,"IsHidden":false}`), json.RawMessage(`{}`))
	member := h.createUser(t, "Policy Member", "member password", json.RawMessage(`{"IsHidden":false,"EnableAllFolders":true}`), json.RawMessage(`{}`))
	h.identities["administrator"] = auth.Identity{Authenticated: true, Administrator: true, Token: "administrator-token", UserID: administrator.ID, DeviceID: "administrator-device", Client: "web"}

	issued := p104Call(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"Policy Member","Pw":"member password"}`, "application/json", "", "", p104MediaBrowserMetadata)
	assertStatus(t, issued, http.StatusOK)
	accessToken := p104String(t, p104Object(t, issued.Body.Bytes())["AccessToken"])
	h.identities[accessToken] = auth.Identity{Authenticated: true, Token: accessToken, UserID: member.ID, DeviceID: "browser-id", Client: "web"}

	acceptedPolicy := `{"IsHidden":false,"EnableAllFolders":false,"EnableRemoteAccess":true}`
	accepted := p104Call(h.rt, http.MethodPost, "/Users/"+member.ID.Dashed()+"/Policy", acceptedPolicy, "application/json", "", "administrator", "")
	assertStatus(t, accepted, http.StatusNoContent)
	p104AssertEmptyBody(t, accepted)
	assertPersistedPolicy := func(want string) {
		t.Helper()
		stored, err := h.users.Get(t.Context(), member.ID)
		if err != nil {
			t.Fatalf("read policy member: %v", err)
		}
		if got := string(stored.Policy); got != want {
			t.Fatalf("persisted policy bytes = %q, want %q", got, want)
		}
	}
	assertPersistedPolicy(acceptedPolicy)

	malformed := p104Call(h.rt, http.MethodPost, "/Users/"+member.ID.Dashed()+"/Policy", `{"IsHidden":`, "application/json", "", "administrator", "")
	assertStatus(t, malformed, http.StatusBadRequest)
	assertPersistedPolicy(acceptedPolicy)

	rejected := p104Call(h.rt, http.MethodPost, "/Users/"+member.ID.Dashed()+"/Policy", `null`, "application/json", "", "administrator", "")
	assertStatus(t, rejected, http.StatusBadRequest)
	assertPersistedPolicy(acceptedPolicy)

	// The pre-existing bearer-equivalent identity remains usable after both
	// rejected writes, and password authentication still resolves the account.
	tokenRead := p104Call(h.rt, http.MethodGet, "/Users/Me", "", "", "", accessToken, "")
	assertStatus(t, tokenRead, http.StatusOK)
	passwordLogin := p104Call(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"Policy Member","Pw":"member password"}`, "application/json", "", "", p104MediaBrowserMetadata)
	assertStatus(t, passwordLogin, http.StatusOK)
}

// HTTP policy updates must share the service's strict persisted-document
// decoding: null is not a boolean permission, while false is.
func TestUserPolicyUpdateRejectsEveryNestedBooleanNullBeforePersistence(t *testing.T) {
	h := newP104Harness(t)
	administrator := h.createUser(t, "Null policy administrator", "administrator password", json.RawMessage(`{"IsAdministrator":true}`), json.RawMessage(`{}`))
	member := h.createUser(t, "Null policy member", "member password", json.RawMessage(`{"IsHidden":true,"EnableRemoteAccess":true}`), json.RawMessage(`{}`))
	h.identities["administrator"] = auth.Identity{Authenticated: true, Administrator: true, Token: "administrator-token", UserID: administrator.ID, DeviceID: "administrator-device", Client: "web"}
	path := "/Users/" + member.ID.Dashed() + "/Policy"
	baseline := json.RawMessage(`{"IsHidden":true,"EnableRemoteAccess":true}`)

	assertStoredPolicy := func(want json.RawMessage) {
		t.Helper()
		stored, err := h.users.Get(t.Context(), member.ID)
		if err != nil {
			t.Fatalf("read policy member: %v", err)
		}
		if string(stored.Policy) != string(want) {
			t.Fatalf("persisted policy = %s, want %s", stored.Policy, want)
		}
	}

	policyType := reflect.TypeFor[dto.UserPolicy]()
	for fieldIndex := range policyType.NumField() {
		field := policyType.Field(fieldIndex)
		if field.Type.Kind() != reflect.Bool {
			continue
		}
		fieldName := strings.Split(field.Tag.Get("json"), ",")[0]
		if fieldName == "" || fieldName == "-" {
			t.Fatalf("boolean UserPolicy field %s has no JSON name", field.Name)
		}
		t.Run(fieldName, func(t *testing.T) {
			if err := h.users.UpdatePolicy(t.Context(), member.ID, baseline); err != nil {
				t.Fatalf("restore baseline policy: %v", err)
			}
			nullDocument, err := json.Marshal(map[string]any{fieldName: nil})
			if err != nil {
				t.Fatalf("marshal null %s policy: %v", fieldName, err)
			}
			rejected := p104Call(h.rt, http.MethodPost, path, string(nullDocument), "application/json", "", "administrator", "")
			assertStatus(t, rejected, http.StatusBadRequest)
			assertStoredPolicy(baseline)

			falseDocument, err := json.Marshal(map[string]any{fieldName: false})
			if err != nil {
				t.Fatalf("marshal false %s policy: %v", fieldName, err)
			}
			accepted := p104Call(h.rt, http.MethodPost, path, string(falseDocument), "application/json", "", "administrator", "")
			assertStatus(t, accepted, http.StatusNoContent)
			p104AssertEmptyBody(t, accepted)
			assertStoredPolicy(falseDocument)
		})
	}
}

// Sources: upstream/jellyfin/Jellyfin.Api/Controllers/SessionController.cs:52-69,
// 377-391; internal/session/manager.go:139-194. An omitted capability Id targets
// the caller's current session, while an explicit Id targets that exact session.
func TestSessionsFiltersAndCapabilityReportsSelectExplicitOrCurrentSession(t *testing.T) {
	h := newP104Harness(t)
	owner := h.createUser(t, "Owner", "owner password", json.RawMessage(`{"IsHidden":false}`), json.RawMessage(`{"GroupedFolders":[]}`))
	h.identities["owner"] = auth.Identity{Authenticated: true, Token: "owner-token", UserID: owner.ID, Client: "web", DeviceID: "current-device"}
	stale, err := h.sessions.LogActivity(t.Context(), session.Activity{Client: "tablet", Version: "1.0", DeviceID: "stale-device", DeviceName: "Tablet", UserID: owner.ID})
	if err != nil {
		t.Fatalf("seed stale session: %v", err)
	}
	h.clock.Advance(2 * time.Second)
	current, err := h.sessions.LogActivity(t.Context(), session.Activity{Client: "web", Version: "1.0", DeviceID: "current-device", DeviceName: "Browser", UserID: owner.ID})
	if err != nil {
		t.Fatalf("seed current session: %v", err)
	}
	other, err := h.sessions.LogActivity(t.Context(), session.Activity{Client: "tv", Version: "1.0", DeviceID: "other-device", DeviceName: "TV", UserID: owner.ID})
	if err != nil {
		t.Fatalf("seed other session: %v", err)
	}

	filtered := p104Call(h.rt, http.MethodGet, "/Sessions?DeviceId=CURRENT-DEVICE", "", "", `application/json; profile="CamelCase"`, "owner", "")
	assertStatus(t, filtered, http.StatusOK)
	filteredSessions := p104RawArray(t, filtered.Body.Bytes())
	if len(filteredSessions) != 1 {
		t.Fatalf("filtered sessions = %s, want only current session", filteredSessions)
	}
	filteredSession := p104RawObject(t, filteredSessions[0])
	if got := p104String(t, filteredSession["id"]); got != current.ID {
		t.Fatalf("filtered session ID = %q, want %q", got, current.ID)
	}
	if got := p104String(t, filteredSession["userId"]); got != owner.ID.String() {
		t.Fatalf("filtered session user ID = %q, want N-format %q", got, owner.ID.String())
	}
	playState := p104RawObject(t, filteredSession["playState"])
	if got := len(playState); got != 5 {
		t.Fatalf("GET /Sessions PlayState fields = %#v, want exact five-field source shape", playState)
	}
	assertJSONFields(t, playState, map[string]any{
		"canSeek": false, "isMuted": false, "isPaused": false, "playbackOrder": "Default", "repeatMode": "RepeatNone",
	})
	active := p104Call(h.rt, http.MethodGet, "/Sessions?ActiveWithinSeconds=1", "", "", "", "owner", "")
	assertStatus(t, active, http.StatusOK)
	activeSessions := p104RawArray(t, active.Body.Bytes())
	if len(activeSessions) != 2 {
		t.Fatalf("active-session filter = %s, want current and other but not stale", activeSessions)
	}
	for _, value := range activeSessions {
		if got := p104String(t, p104RawObject(t, value)["Id"]); got == stale.ID {
			t.Fatalf("active-session filter retained stale session %q", stale.ID)
		}
	}

	currentCapabilities := p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities/Full", `{"PlayableMediaTypes":"Audio,Video","SupportedCommands":["Play","Stop"],"SupportsMediaControl":true}`, "application/json", "", "owner", "")
	assertStatus(t, currentCapabilities, http.StatusNoContent)
	p104AssertEmptyBody(t, currentCapabilities)
	otherCapabilities := p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities/Full?Id="+other.ID, `{"PlayableMediaTypes":["Photo"],"SupportedCommands":"Display"}`, "application/json", "", "owner", "")
	assertStatus(t, otherCapabilities, http.StatusNoContent)
	p104AssertEmptyBody(t, otherCapabilities)

	snapshots := h.sessions.Sessions(session.Query{})
	if got := p104Session(t, snapshots, current.ID).Capabilities.PlayableMediaTypes; len(got) != 2 || got[0] != "Audio" || got[1] != "Video" {
		t.Fatalf("implicit current-session capabilities = %#v, want Audio and Video", got)
	}
	if got := p104Session(t, snapshots, other.ID).Capabilities.PlayableMediaTypes; len(got) != 1 || got[0] != "Photo" {
		t.Fatalf("explicit-ID capabilities = %#v, want Photo", got)
	}
	updated := p104Call(h.rt, http.MethodGet, "/Sessions?DeviceId=current-device", "", "", `application/json; profile="CamelCase"`, "owner", "")
	assertStatus(t, updated, http.StatusOK)
	updatedSessions := p104RawArray(t, updated.Body.Bytes())
	if len(updatedSessions) != 1 {
		t.Fatalf("updated current-device sessions = %s, want one session", updatedSessions)
	}
	updatedCapabilities := p104RawObject(t, p104RawObject(t, updatedSessions[0])["capabilities"])
	assertJSONFields(t, updatedCapabilities, map[string]any{
		"playableMediaTypes":   []string{"Audio", "Video"},
		"supportedCommands":    []string{"Play", "Stop"},
		"supportsMediaControl": true,
	})
	missing := p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities/Full?Id=missing-session", `{}`, "application/json", "", "owner", "")
	assertStatus(t, missing, http.StatusNotFound)
	empty := p104Call(h.rt, http.MethodGet, "/Sessions?DeviceId=none", "", "", "", "owner", "")
	assertStatus(t, empty, http.StatusOK)
	if got := p104RawArray(t, empty.Body.Bytes()); len(got) != 0 {
		t.Fatalf("empty session list = %s, want an empty JSON array", got)
	}
}

// A DeviceId identifies the physical device, not the active Jellyfin session:
// clients sharing it must report capabilities to their own Client+DeviceId
// session when Id is omitted.
func TestSessionCapabilitiesWithoutIDTargetExactClientAndDevice(t *testing.T) {
	h := newP104Harness(t)
	owner := h.createUser(t, "Twin Device Owner", "correct password", json.RawMessage(`{}`), json.RawMessage(`{}`))

	login := func(client, endpoint string) {
		t.Helper()
		metadata := `MediaBrowser Client="` + client + `", Device="Shared Device", DeviceId="shared-device", Version="1.0"`
		rec := p104CallFrom(h.rt, http.MethodPost, "/Users/AuthenticateByName", `{"Username":"Twin Device Owner","Pw":"correct password"}`, "application/json", "", "", metadata, endpoint)
		assertStatus(t, rec, http.StatusOK)
	}
	login("Browser", "198.51.100.10:8096")
	h.clock.Advance(time.Second)
	login("Television", "198.51.100.11:8096")

	h.identities["browser"] = auth.Identity{Authenticated: true, Token: "browser-token", UserID: owner.ID, Client: "Browser", DeviceID: "shared-device", Version: "1.0"}
	updated := p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities/Full", `{"PlayableMediaTypes":["Audio"],"SupportedCommands":["Play"],"SupportsMediaControl":true}`, "application/json", "", "browser", "")
	assertStatus(t, updated, http.StatusNoContent)

	listed := p104Call(h.rt, http.MethodGet, "/Sessions?DeviceId=SHARED-DEVICE", "", "", "", "browser", "")
	assertStatus(t, listed, http.StatusOK)
	sessions := p104RawArray(t, listed.Body.Bytes())
	if len(sessions) != 2 {
		t.Fatalf("shared-device sessions = %s, want Browser and Television sessions", sessions)
	}
	for _, raw := range sessions {
		value := p104RawObject(t, raw)
		capabilities := p104RawObject(t, value["Capabilities"])
		switch client := p104String(t, value["Client"]); client {
		case "Browser":
			assertJSONFields(t, capabilities, map[string]any{
				"PlayableMediaTypes":   []string{"Audio"},
				"SupportedCommands":    []string{"Play"},
				"SupportsMediaControl": true,
			})
		case "Television":
			assertJSONFields(t, capabilities, map[string]any{
				"PlayableMediaTypes":   []string{},
				"SupportedCommands":    []string{},
				"SupportsMediaControl": false,
			})
		default:
			t.Fatalf("shared-device session client = %q, want Browser or Television", client)
		}
	}
}

// Source: upstream/jellyfin/Emby.Server.Implementations/Session/SessionManager.cs:1992-2091.
// GET /Sessions derives its visibility from persisted caller, device, and target-user
// policy facts; it never treats every authenticated caller as a global observer.
func TestSessionsListAuthorizesVisibilityAndControllableTargets(t *testing.T) {
	h := newP104Harness(t)
	admin := h.createUser(t, "Administrator", "admin password", json.RawMessage(`{"IsAdministrator":true,"EnableRemoteControlOfOtherUsers":true,"EnableAllDevices":true}`), json.RawMessage(`{}`))
	alice := h.createUser(t, "Alice", "alice password", json.RawMessage(`{"IsAdministrator":false,"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":true,"EnableAllDevices":false,"EnabledDevices":["alice-device","shared-device"]}`), json.RawMessage(`{}`))
	bob := h.createUser(t, "Bob", "bob password", json.RawMessage(`{"IsAdministrator":false,"EnableRemoteControlOfOtherUsers":false,"EnableSharedDeviceControl":false,"EnableAllDevices":true}`), json.RawMessage(`{}`))
	h.identities["admin"] = auth.Identity{Authenticated: true, Administrator: true, Token: "admin-token", UserID: admin.ID, DeviceID: "admin-device", Client: "web"}
	h.identities["alice"] = auth.Identity{Authenticated: true, Token: "alice-token", UserID: alice.ID, DeviceID: "alice-device", Client: "web"}
	h.identities["bob"] = auth.Identity{Authenticated: true, Token: "bob-token", UserID: bob.ID, DeviceID: "bob-device", Client: "web"}

	seed := func(client, device string, userID dto.UUID, additional ...dto.UUID) session.Snapshot {
		t.Helper()
		created, err := h.sessions.LogActivity(t.Context(), session.Activity{Client: client, Version: "1.0", DeviceID: device, DeviceName: device, UserID: userID, AdditionalUserIDs: additional})
		if err != nil {
			t.Fatalf("seed %s session: %v", device, err)
		}
		return created
	}
	enableRemoteControl := func(created session.Snapshot) {
		t.Helper()
		id := created.ID
		if err := h.sessions.ReportCapabilities(t.Context(), session.CapabilityReport{ID: &id, CurrentID: id, Capabilities: session.Capabilities{SupportsMediaControl: true}}); err != nil {
			t.Fatalf("enable remote control for %s: %v", created.ID, err)
		}
	}

	adminSession := seed("Admin Web", "admin-device", admin.ID)
	aliceSession := seed("Alice Web", "alice-device", alice.ID)
	sharedSession := seed("Shared TV", "shared-device", bob.ID, alice.ID)
	bobSession := seed("Bob Web", "bob-device", bob.ID)
	anonymousSession := seed("Anonymous TV", "anonymous-device", dto.NilUUID)
	nonControllableBobSession := seed("Legacy Bob", "legacy-bob-device", bob.ID)
	for _, created := range []session.Snapshot{adminSession, aliceSession, sharedSession, bobSession, anonymousSession} {
		enableRemoteControl(created)
	}

	for _, tc := range []struct {
		name   string
		actor  string
		accept string
		idKey  string
		want   []string
	}{
		{name: "non-administrator Pascal list exposes only own, shared, and anonymous sessions", actor: "alice", idKey: "Id", want: []string{aliceSession.ID, sharedSession.ID, anonymousSession.ID}},
		{name: "non-administrator CamelCase list exposes only own, shared, and anonymous sessions", actor: "alice", accept: `application/json; profile="CamelCase"`, idKey: "id", want: []string{aliceSession.ID, sharedSession.ID, anonymousSession.ID}},
		{name: "administrator list exposes every session", actor: "admin", idKey: "Id", want: []string{adminSession.ID, aliceSession.ID, sharedSession.ID, bobSession.ID, anonymousSession.ID, nonControllableBobSession.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := p104Call(h.rt, http.MethodGet, "/Sessions", "", "", tc.accept, tc.actor, "")
			assertStatus(t, rec, http.StatusOK)
			p104AssertSessionIDs(t, rec.Body.Bytes(), tc.idKey, tc.want)
		})
	}

	t.Run("controllable self list honors remote support, additional users, and caller device access", func(t *testing.T) {
		aliceControllable := p104Call(h.rt, http.MethodGet, "/Sessions?ControllableByUserId="+alice.ID.Dashed(), "", "", `application/json; profile="CamelCase"`, "alice", "")
		assertStatus(t, aliceControllable, http.StatusOK)
		p104AssertSessionIDs(t, aliceControllable.Body.Bytes(), "id", []string{aliceSession.ID, sharedSession.ID})
	})

	t.Run("administrator controllable list honors target shared-device policy", func(t *testing.T) {
		adminControllable := p104Call(h.rt, http.MethodGet, "/Sessions?ControllableByUserId="+bob.ID.Dashed(), "", "", "", "admin", "")
		assertStatus(t, adminControllable, http.StatusOK)
		p104AssertSessionIDs(t, adminControllable.Body.Bytes(), "Id", []string{adminSession.ID, aliceSession.ID, sharedSession.ID, bobSession.ID})
	})

	t.Run("missing controllable target returns source empty result", func(t *testing.T) {
		missing := p104UUID(t, "12345678-1234-1234-1234-123456789abc")
		missingTarget := p104Call(h.rt, http.MethodGet, "/Sessions?ControllableByUserId="+missing.Dashed(), "", "", "", "admin", "")
		assertStatus(t, missingTarget, http.StatusOK)
		p104AssertSessionIDs(t, missingTarget.Body.Bytes(), "Id", nil)
	})

	t.Run("non-administrator cannot select another controllable target", func(t *testing.T) {
		unauthorizedTarget := p104Call(h.rt, http.MethodGet, "/Sessions?ControllableByUserId="+bob.ID.Dashed(), "", "", "", "alice", "")
		assertStatus(t, unauthorizedTarget, http.StatusForbidden)
	})
}

const p104MediaBrowserMetadata = `MediaBrowser Client="web", Device="browser", DeviceId="browser-id", Version="1.0"`

type p104Harness struct {
	rt                  *router.Router
	users               *user.Service
	stores              store.Stores
	clock               *clock.Fake
	sessions            *session.Manager
	identities          map[string]auth.Identity
	loginPolicy         *p104LoginPolicy
	identity            config.ServerIdentity
	legacyAuthorization *atomic.Bool
}

func newP104Harness(t *testing.T) *p104Harness {
	return newP104HarnessWithLoginNetwork(t, p104LoginNetwork{})
}

func newP104HarnessWithLoginNetwork(t *testing.T, network p104LoginNetwork) *p104Harness {
	t.Helper()
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open identity store: %v", err)
	}
	t.Cleanup(func() {
		if err := stores.Close(); err != nil {
			t.Errorf("close identity store: %v", err)
		}
	})
	fakeClock := clock.NewFake(time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC))
	users := user.New(store.NewIdentity(stores), fakeClock, &p104Entropy{})
	identity := config.ServerIdentity{ServerName: "P104 Test Server", ID: p104UUID(t, "c72d78ad-52ca-4ce0-9073-b6e687e909ce"), Version: "12.0.0-p104"}
	sessions := session.New(fakeClock)
	items := store.NewItemStore(stores)
	itemBuilder := dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, users, nil), identity.ID.String())
	policy := &p104LoginPolicy{}
	legacyAuthorization := &atomic.Bool{}
	login := auth.NewLogin(auth.LoginConfig{
		Users:                      users,
		Policy:                     policy,
		Network:                    network,
		Sessions:                   p104LoginSessions{manager: sessions},
		Server:                     identity,
		LegacyAuthorizationEnabled: legacyAuthorization.Load,
	})
	identities := map[string]auth.Identity{}
	authorizer := middleware.NewAuthorizer(p104Resolver{identities: identities}, p104Evaluator{}, p104AuthorizerUsers{users: users})
	rt := router.New(authorizer.Wrap)
	handlers.RegisterAuth(rt, login, users, sessions, items, itemBuilder, identity, p104Startup{completed: true}, network, nil, nil)
	return &p104Harness{rt: rt, stores: stores, users: users, sessions: sessions, clock: fakeClock, identities: identities, loginPolicy: policy, identity: identity, legacyAuthorization: legacyAuthorization}
}

func (h *p104Harness) createUser(t *testing.T, name, password string, policy, configuration json.RawMessage) store.User {
	t.Helper()
	created, err := h.users.Create(t.Context(), name, password)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	created.Policy = policy
	created.Configuration = configuration
	if err := h.users.Update(t.Context(), created); err != nil {
		t.Fatalf("apply %s policy and configuration: %v", name, err)
	}
	return created
}

func p104Call(handler http.Handler, method, path, body, contentType, accept, actor, metadata string) *httptest.ResponseRecorder {
	return p104CallFrom(handler, method, path, body, contentType, accept, actor, metadata, "")
}

func p104CallFrom(handler http.Handler, method, path, body, contentType, accept, actor, metadata, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if actor != "" {
		req.Header.Set("X-P104-Actor", actor)
	}
	if metadata != "" {
		req.Header.Set("Authorization", metadata)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func p104PasswordLoginWithHeaders(handler http.Handler, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", strings.NewReader(`{"Username":"Live legacy password","Pw":"correct password"}`))
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

type p104Resolver struct{ identities map[string]auth.Identity }

func (resolver p104Resolver) Resolve(_ context.Context, request *http.Request) (auth.Identity, error) {
	return resolver.identities[request.Header.Get("X-P104-Actor")], nil
}

type p104Evaluator struct{}

func (p104Evaluator) Evaluate(_ context.Context, request auth.PolicyRequest) (auth.Decision, error) {
	if request.Identity.Token == "deny" {
		return auth.DecisionForbidden, nil
	}
	if !request.Identity.Authenticated {
		return auth.DecisionUnauthenticated, nil
	}
	return auth.DecisionAllowed, nil
}

type p104AuthorizerUsers struct{ users *user.Service }

func (users p104AuthorizerUsers) Get(ctx context.Context, id dto.UUID) (store.User, error) {
	value, err := users.users.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{ID: id}, nil
	}
	return value, err
}

type p104LoginAuthorization struct {
	remoteIP netip.Addr
	local    bool
}

type p104LoginPolicy struct {
	err            error
	authorizations []p104LoginAuthorization
}

func (policy *p104LoginPolicy) Authorize(_ context.Context, user store.User, remoteIP netip.Addr, local bool) error {
	policy.authorizations = append(policy.authorizations, p104LoginAuthorization{remoteIP: remoteIP, local: local})
	if policy.err != nil {
		return policy.err
	}
	var accountPolicy struct {
		IsDisabled         bool
		EnableRemoteAccess bool
	}
	if err := json.Unmarshal(user.Policy, &accountPolicy); err != nil {
		return err
	}
	if accountPolicy.IsDisabled || (!local && !accountPolicy.EnableRemoteAccess) {
		return auth.ErrLoginDenied
	}
	return nil
}

func (policy *p104LoginPolicy) AuthorizeDevice(context.Context, store.User, string) error {
	return policy.err
}

type p104LoginNetwork struct{ classifyPeer bool }

func (p104LoginNetwork) IsInLocalNetwork(address netip.Addr) bool {
	return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast()
}

func (network p104LoginNetwork) IsLocal(address netip.Addr) bool {
	return !network.classifyPeer || network.IsInLocalNetwork(address)
}

type p104LoginSessions struct{ manager *session.Manager }

func (sessions p104LoginSessions) CheckLimit(context.Context, dto.UUID) error { return nil }
func (sessions p104LoginSessions) LogActivity(ctx context.Context, activity session.Activity) (session.Snapshot, error) {
	return sessions.manager.LogActivity(ctx, activity)
}

type p104Startup struct{ completed bool }

func (startup p104Startup) StartupWizardCompleted() bool { return startup.completed }

type p104Entropy struct {
	mu   sync.Mutex
	next byte
}

func (entropy *p104Entropy) Read(data []byte) (int, error) {
	entropy.mu.Lock()
	defer entropy.mu.Unlock()
	for index := range data {
		entropy.next++
		if entropy.next == 0 {
			entropy.next++
		}
		data[index] = entropy.next
	}
	return len(data), nil
}

func p104UUID(t *testing.T, text string) dto.UUID {
	t.Helper()
	id, err := dto.ParseUUID(text)
	if err != nil {
		t.Fatalf("parse UUID %q: %v", text, err)
	}
	return id
}

func p104Object(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode JSON object %q: %v", data, err)
	}
	return value
}

func p104RawObject(t *testing.T, data json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	return p104Object(t, data)
}

func p104RawArray(t *testing.T, data []byte) []json.RawMessage {
	t.Helper()
	var value []json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode JSON array %q: %v", data, err)
	}
	return value
}

func p104String(t *testing.T, data json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode JSON string %q: %v", data, err)
	}
	return value
}

func p104Key(profile, pascal, camel string) string {
	if profile == "CamelCase profile" {
		return camel
	}
	return pascal
}

func p104AssertEmptyBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Body.Len(); got != 0 {
		t.Fatalf("response body length = %d, want an empty body: %q", got, rec.Body.String())
	}
}

func p104Session(t *testing.T, snapshots []session.Snapshot, id string) session.Snapshot {
	t.Helper()
	for _, snapshot := range snapshots {
		if snapshot.ID == id {
			return snapshot
		}
	}
	t.Fatalf("session %q was not found in %#v", id, snapshots)
	return session.Snapshot{}
}

func p104AssertSessionIDs(t *testing.T, body []byte, idKey string, want []string) {
	t.Helper()
	values := p104RawArray(t, body)
	if len(values) != len(want) {
		t.Fatalf("session count = %d, want %d: %s", len(values), len(want), body)
	}
	got := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := p104String(t, p104RawObject(t, value)[idKey])
		got[id] = struct{}{}
	}
	if len(got) != len(want) {
		t.Fatalf("session IDs = %#v, want %#v", got, want)
	}
	for _, id := range want {
		if _, found := got[id]; !found {
			t.Fatalf("session IDs = %#v, want session %q", got, id)
		}
	}
}

// POST /Sessions/Capabilities is the query-parameter variant of the
// capability report. Upstream binds the same fields from the query string
// (SessionController.cs:348-366). SupportsPersistentIdentifier defaults to
// true when absent (jellyfin-openapi-stable.json:34457-34464).
func TestPostCapabilities(t *testing.T) {
	h := newP104Harness(t)
	owner := h.createUser(t, "Owner", "owner password", json.RawMessage(`{}`), json.RawMessage(`{}`))
	h.identities["owner"] = auth.Identity{Authenticated: true, Token: "owner-token", UserID: owner.ID, Client: "web", DeviceID: "current-device"}
	current, err := h.sessions.LogActivity(t.Context(), session.Activity{Client: "web", Version: "1.0", DeviceID: "current-device", DeviceName: "Browser", UserID: owner.ID})
	if err != nil {
		t.Fatalf("seed current session: %v", err)
	}
	other, err := h.sessions.LogActivity(t.Context(), session.Activity{Client: "tv", Version: "1.0", DeviceID: "other-device", DeviceName: "TV", UserID: owner.ID})
	if err != nil {
		t.Fatalf("seed other session: %v", err)
	}

	// The full capture shape: query parameters persist with ordered values and
	// the default SupportsPersistentIdentifier=true.
	rec := p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities?playableMediaTypes=Video&supportedCommands=DisplayMessage&supportsMediaControl=false", "", "", "", "owner", "")
	assertStatus(t, rec, http.StatusNoContent)
	p104AssertEmptyBody(t, rec)
	snapshots := h.sessions.Sessions(session.Query{})
	cap := p104Session(t, snapshots, current.ID).Capabilities
	if len(cap.PlayableMediaTypes) != 1 || cap.PlayableMediaTypes[0] != "Video" {
		t.Fatalf("PlayableMediaTypes = %#v, want [Video]", cap.PlayableMediaTypes)
	}
	if len(cap.SupportedCommands) != 1 || cap.SupportedCommands[0] != "DisplayMessage" {
		t.Fatalf("SupportedCommands = %#v, want [DisplayMessage]", cap.SupportedCommands)
	}
	if cap.SupportsMediaControl {
		t.Fatalf("SupportsMediaControl = %v, want false", cap.SupportsMediaControl)
	}
	if !cap.SupportsPersistentIdentifier {
		t.Fatalf("SupportsPersistentIdentifier = %v, want true (default)", cap.SupportsPersistentIdentifier)
	}
	if cap.DeviceProfile != nil {
		t.Fatalf("DeviceProfile = %#v, want nil", cap.DeviceProfile)
	}

	// A CSV list stores both entries.
	rec = p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities?supportedCommands=DisplayMessage,Play", "", "", "", "owner", "")
	assertStatus(t, rec, http.StatusNoContent)
	snapshots = h.sessions.Sessions(session.Query{})
	commands := p104Session(t, snapshots, current.ID).Capabilities.SupportedCommands
	if len(commands) != 2 || commands[0] != "DisplayMessage" || commands[1] != "Play" {
		t.Fatalf("SupportedCommands = %#v, want [DisplayMessage Play]", commands)
	}

	// An explicit supportsPersistentIdentifier=false stores false.
	rec = p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities?supportsPersistentIdentifier=false", "", "", "", "owner", "")
	assertStatus(t, rec, http.StatusNoContent)
	snapshots = h.sessions.Sessions(session.Query{})
	if p104Session(t, snapshots, current.ID).Capabilities.SupportsPersistentIdentifier {
		t.Fatalf("SupportsPersistentIdentifier = true, want false")
	}

	// An explicit known id targets that session.
	rec = p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities?Id="+other.ID+"&supportsMediaControl=true", "", "", "", "owner", "")
	assertStatus(t, rec, http.StatusNoContent)
	snapshots = h.sessions.Sessions(session.Query{})
	if !p104Session(t, snapshots, other.ID).Capabilities.SupportsMediaControl {
		t.Fatalf("other session SupportsMediaControl = false, want true")
	}

	// An unknown id returns 404.
	rec = p104Call(h.rt, http.MethodPost, "/Sessions/Capabilities?Id=missing-session", "", "", "", "owner", "")
	assertStatus(t, rec, http.StatusNotFound)
}
