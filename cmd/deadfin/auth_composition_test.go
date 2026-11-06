// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
)

func TestApplicationComposesPersistentLoginSessionsAndDisplayPreferences(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)
	configureComposedAdministrator(t, app.Router())

	token := loginComposedAdministrator(t, app.Router())
	assertAuthenticatedStatus(t, app.Router(), http.MethodPost, "/Sessions/Capabilities/Full", []byte(`{"PlayableMediaTypes":["Video"],"SupportedCommands":["Play"],"SupportsMediaControl":true}`), token, http.StatusNoContent)
	assertSessionCount(t, app.Router(), token, 1)

	const preferencesPath = "/DisplayPreferences/usersettings?client=Web"
	assertAuthenticatedStatus(t, app.Router(), http.MethodPost, preferencesPath, []byte(`{"SortBy":"DateCreated","RememberSorting":true}`), token, http.StatusNoContent)
	assertDisplayPreferences(t, app.Router(), preferencesPath, token)

	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("close composed application: %v", err)
	}
	app = newCompositionApplication(t, root)

	// Play sessions live in memory by default (D-06); the reload is off.
	assertAuthenticatedStatus(t, app.Router(), http.MethodGet, "/Users/Me", nil, token, http.StatusOK)
	assertSessionCount(t, app.Router(), token, 0)
	assertDisplayPreferences(t, app.Router(), preferencesPath, token)
}

func TestApplicationFreshHiddenAdministratorPolicyAndDisplayPreferencesMatchStockAcrossReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)
	configureComposedAdministrator(t, app.Router())

	login := authenticatedCompositionRequest(app.Router(), http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"captain","Pw":"correct horse battery staple"}`), "")
	assertCompositionLoginStatus(t, login, http.StatusOK)
	token := compositionAccessToken(t, login)
	var loginBody map[string]json.RawMessage
	if err := json.Unmarshal(login.Body.Bytes(), &loginBody); err != nil {
		t.Fatalf("decode fresh login result: %v; body: %s", err, login.Body.String())
	}
	var loginUser, policy map[string]json.RawMessage
	if err := json.Unmarshal(loginBody["User"], &loginUser); err != nil {
		t.Fatalf("decode fresh login user: %v; body: %s", err, login.Body.String())
	}
	if err := json.Unmarshal(loginUser["Policy"], &policy); err != nil {
		t.Fatalf("decode fresh login policy: %v; body: %s", err, login.Body.String())
	}
	if got := string(policy["PasswordResetProviderId"]); got != `"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider"` {
		t.Fatalf("fresh login PasswordResetProviderId = %s, want stock provider ID", got)
	}

	const preferencesPath = "/DisplayPreferences/usersettings?client=Web"
	fresh := composedFreshDisplayPreferenceFields()
	assertComposedDisplayPreferenceFields(t, app.Router(), preferencesPath, token, fresh)

	saved := composedFreshDisplayPreferenceFields()
	saved["SortBy"] = "DateCreated"
	saved["RememberSorting"] = true
	saved["CustomPrefs"] = map[string]any{
		"chromecastVersion":          "NIGHTLY",
		"skipForwardLength":          "00030000",
		"skipBackLength":             "010000",
		"enableNextVideoInfoOverlay": "true",
		"tvhome":                     "",
		"dashboardTheme":             nil,
		"userCasing":                 "MiXeD Value",
	}
	body, err := json.Marshal(saved)
	if err != nil {
		t.Fatalf("marshal saved display preferences: %v", err)
	}
	assertAuthenticatedStatus(t, app.Router(), http.MethodPost, preferencesPath, body, token, http.StatusNoContent)
	assertComposedDisplayPreferenceFields(t, app.Router(), preferencesPath, token, saved)

	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("close composed application: %v", err)
	}
	app = newCompositionApplication(t, root)
	assertComposedDisplayPreferenceFields(t, app.Router(), preferencesPath, token, saved)
}

func composedFreshDisplayPreferenceFields() map[string]any {
	return map[string]any{
		"Client": "Web",
		"CustomPrefs": map[string]any{
			"chromecastVersion":          "stable",
			"skipForwardLength":          "30000",
			"skipBackLength":             "10000",
			"enableNextVideoInfoOverlay": "False",
			"tvhome":                     nil,
			"dashboardTheme":             nil,
		},
		"Id":                 "3ce5b65d-e116-d731-65d1-efc4a30ec35c",
		"PrimaryImageHeight": 250,
		"PrimaryImageWidth":  250,
		"RememberIndexing":   false,
		"RememberSorting":    false,
		"ScrollDirection":    "Horizontal",
		"ShowBackdrop":       true,
		"ShowSidebar":        false,
		"SortBy":             "SortName",
		"SortOrder":          "Ascending",
	}
}

func assertComposedDisplayPreferenceFields(t *testing.T, handler http.Handler, path, token string, want map[string]any) {
	t.Helper()
	rec := authenticatedCompositionRequest(handler, http.MethodGet, path, nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d; body: %s", path, rec.Code, http.StatusOK, rec.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode display preferences: %v; body: %s", err, rec.Body.String())
	}
	if len(got) != len(want) {
		t.Fatalf("display preference field count = %d, want exact DTO shape %d: %s", len(got), len(want), rec.Body.String())
	}
	for key, expected := range want {
		encoded, err := json.Marshal(expected)
		if err != nil {
			t.Fatalf("marshal expected %q: %v", key, err)
		}
		if value, ok := got[key]; !ok {
			t.Errorf("display preferences missing %q: %s", key, rec.Body.String())
		} else if string(value) != string(encoded) {
			t.Errorf("display preferences %q = %s, want %s", key, value, encoded)
		}
	}
}

// Source: upstream Jellyfin SessionManager.AuthenticateNewSessionInternal.
// Device admission occurs before device-token replacement and session creation.
// A restricted user's listed device is admitted, while an unlisted device is a
// verified-login denial (403) that leaves its existing token and session state
// untouched. This uses the application graph and SQLite-backed user service so
// the policy is read from the same persisted state as production login.
func TestApplicationLoginEnforcesDevicePolicyBeforeMintingTokenOrSession(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	member, err := app.UserService().Create(t.Context(), "device-restricted", "correct password")
	if err != nil {
		t.Fatalf("create restricted user: %v", err)
	}
	member.Policy = []byte(`{"EnableAllDevices":false,"EnabledDevices":["allowed-device"]}`)
	if err := app.UserService().Update(t.Context(), member); err != nil {
		t.Fatalf("persist restricted device policy: %v", err)
	}

	// A token on the denied device must survive: IssueToken atomically replaces
	// device tokens, therefore its survival proves the denial minted no token.
	blocked, err := app.UserService().IssueToken(t.Context(), store.Device{
		UserID: member.ID, AppName: "seed", AppVersion: "1.0", DeviceName: "Blocked", DeviceID: "blocked-device",
	})
	if err != nil {
		t.Fatalf("seed blocked-device token: %v", err)
	}

	denied := composedLogin(t, app, member.Name, "correct password", "blocked-device")
	assertCompositionLoginStatus(t, denied, http.StatusForbidden)
	if _, err := app.UserService().Token(t.Context(), blocked.AccessToken); err != nil {
		t.Fatalf("denied-device login replaced existing token: %v", err)
	}
	if sessions := app.sessions.Sessions(session.Query{}); len(sessions) != 0 {
		t.Fatalf("denied-device login sessions = %#v, want none", sessions)
	}

	allowed := composedLogin(t, app, member.Name, "correct password", "allowed-device")
	assertCompositionLoginStatus(t, allowed, http.StatusOK)
	allowedToken := compositionAccessToken(t, allowed)
	if allowedToken == "" {
		t.Fatal("allowed-device login returned no access token")
	}
	if _, err := app.UserService().Token(t.Context(), allowedToken); err != nil {
		t.Fatalf("allowed-device login did not persist token: %v", err)
	}
	if sessions := app.sessions.Sessions(session.Query{}); len(sessions) != 1 || sessions[0].DeviceID != "allowed-device" || sessions[0].UserID != member.ID {
		t.Fatalf("allowed-device login sessions = %#v, want one allowed-device session for %s", sessions, member.ID)
	}
}

// Source: upstream Jellyfin SessionManager.AuthenticateNewSessionInternal.
// MaxActiveSessions rejects a distinct active client/device at 403 before it
// can replace that device's token, but a second login from the already active
// client/device replaces its token and refreshes the same session identity.
func TestApplicationLoginEnforcesMaxActiveSessionsAndRefreshesSameDevice(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	member, err := app.UserService().Create(t.Context(), "session-limited", "correct password")
	if err != nil {
		t.Fatalf("create session-limited user: %v", err)
	}
	member.Policy = []byte(`{"EnableAllDevices":true,"MaxActiveSessions":1}`)
	if err := app.UserService().Update(t.Context(), member); err != nil {
		t.Fatalf("persist max-active-sessions policy: %v", err)
	}

	// The preexisting second-device token detects a forbidden IssueToken call.
	blocked, err := app.UserService().IssueToken(t.Context(), store.Device{
		UserID: member.ID, AppName: "seed", AppVersion: "1.0", DeviceName: "Second", DeviceID: "second-device",
	})
	if err != nil {
		t.Fatalf("seed second-device token: %v", err)
	}

	first := composedLogin(t, app, member.Name, "correct password", "first-device")
	assertCompositionLoginStatus(t, first, http.StatusOK)
	firstToken := compositionAccessToken(t, first)
	firstSessions := app.sessions.Sessions(session.Query{})
	if len(firstSessions) != 1 || firstSessions[0].DeviceID != "first-device" {
		t.Fatalf("first login sessions = %#v, want one first-device session", firstSessions)
	}
	firstSessionID := firstSessions[0].ID

	denied := composedLogin(t, app, member.Name, "correct password", "second-device")
	assertCompositionLoginStatus(t, denied, http.StatusForbidden)
	if _, err := app.UserService().Token(t.Context(), blocked.AccessToken); err != nil {
		t.Fatalf("session-limit denial replaced second-device token: %v", err)
	}
	if sessions := app.sessions.Sessions(session.Query{}); len(sessions) != 1 || sessions[0].ID != firstSessionID {
		t.Fatalf("session-limit denial sessions = %#v, want unchanged first session %q", sessions, firstSessionID)
	}

	replacement := composedLogin(t, app, member.Name, "correct password", "first-device")
	assertCompositionLoginStatus(t, replacement, http.StatusOK)
	replacementToken := compositionAccessToken(t, replacement)
	if replacementToken == firstToken {
		t.Fatalf("same-device login token = %q, want replacement of %q", replacementToken, firstToken)
	}
	if _, err := app.UserService().Token(t.Context(), firstToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("same-device login retained old token: %v, want ErrNotFound", err)
	}
	if _, err := app.UserService().Token(t.Context(), replacementToken); err != nil {
		t.Fatalf("same-device replacement token is not persisted: %v", err)
	}
	if sessions := app.sessions.Sessions(session.Query{}); len(sessions) != 1 || sessions[0].ID != firstSessionID || sessions[0].DeviceID != "first-device" {
		t.Fatalf("same-device login sessions = %#v, want refreshed first session %q", sessions, firstSessionID)
	}
}

// Legacy carriers are an operator-controlled compatibility switch. Exercise the
// router's actual middleware stack against one running application so a config
// change cannot accidentally require process reconstruction to take effect.
func TestApplicationReadsPersistedLegacyAuthorizationForEachRequest(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	configureComposedAdministrator(t, app.Router())
	token := loginComposedAdministrator(t, app.Router())
	router := app.Router()

	modern := composedLegacyAuthorizationRequest(router, map[string]string{
		"Authorization": `MediaBrowser Token="` + token + `"`,
	}, nil)
	assertCompositionAuthorizationStatus(t, modern, http.StatusOK)

	carriers := []struct {
		name    string
		headers map[string]string
		query   url.Values
	}{
		{name: "X-Emby-Token", headers: map[string]string{"X-Emby-Token": token}},
		{name: "X-MediaBrowser-Token", headers: map[string]string{"X-MediaBrowser-Token": token}},
		{name: "Emby authorization scheme", headers: map[string]string{"Authorization": `Emby Token="` + token + `"`}},
		{name: "X-Emby-Authorization", headers: map[string]string{"X-Emby-Authorization": `MediaBrowser Token="` + token + `"`}},
		{name: "api_key query", query: url.Values{"api_key": {token}}},
	}
	for _, carrier := range carriers {
		t.Run("disabled rejects "+carrier.name, func(t *testing.T) {
			record := composedLegacyAuthorizationRequest(router, carrier.headers, carrier.query)
			assertCompositionAuthorizationStatus(t, record, http.StatusUnauthorized)
		})
	}

	if err := app.ConfigManager().UpdateSystem(t.Context(), func(system *config.ServerConfiguration) error {
		system.EnableLegacyAuthorization = true
		return nil
	}); err != nil {
		t.Fatalf("persist EnableLegacyAuthorization: %v", err)
	}
	if !app.ConfigManager().System().EnableLegacyAuthorization {
		t.Fatal("persisted EnableLegacyAuthorization = false, want true")
	}

	modern = composedLegacyAuthorizationRequest(router, map[string]string{
		"Authorization": `MediaBrowser Token="` + token + `"`,
	}, nil)
	assertCompositionAuthorizationStatus(t, modern, http.StatusOK)

	for _, carrier := range carriers {
		t.Run("enabled accepts "+carrier.name, func(t *testing.T) {
			record := composedLegacyAuthorizationRequest(router, carrier.headers, carrier.query)
			assertCompositionAuthorizationStatus(t, record, http.StatusOK)
		})
	}
}

// Password login derives required device metadata from the same live legacy
// authorization switch as protected middleware requests. Only dictionary
// carriers can supply that metadata; scalar token/query carriers cannot form a
// valid password-login device request on their own.
func TestApplicationPasswordLoginReadsPersistedLegacyAuthorizationForEachRequest(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	configureComposedAdministrator(t, app.Router())
	router := app.Router()
	metadata := `Client="Legacy Password Test", Device="Legacy Password Device", DeviceId="legacy-password-device", Version="1.0"`

	modern := http.Header{"Authorization": {`MediaBrowser ` + metadata}}
	assertCompositionLoginStatus(t, composedPasswordLoginWithHeaders(router, modern), http.StatusOK)

	legacyCarriers := []struct {
		name    string
		headers http.Header
	}{
		{name: "Emby authorization scheme", headers: http.Header{"Authorization": {`Emby ` + metadata}}},
		{name: "X-Emby-Authorization", headers: http.Header{"X-Emby-Authorization": {`MediaBrowser ` + metadata}}},
	}
	for _, carrier := range legacyCarriers {
		t.Run("disabled rejects "+carrier.name, func(t *testing.T) {
			assertCompositionLoginStatus(t, composedPasswordLoginWithHeaders(router, carrier.headers), http.StatusBadRequest)
		})
	}

	if err := app.ConfigManager().UpdateSystem(t.Context(), func(system *config.ServerConfiguration) error {
		system.EnableLegacyAuthorization = true
		return nil
	}); err != nil {
		t.Fatalf("persist EnableLegacyAuthorization: %v", err)
	}

	assertCompositionLoginStatus(t, composedPasswordLoginWithHeaders(router, modern), http.StatusOK)
	for _, carrier := range legacyCarriers {
		t.Run("enabled accepts "+carrier.name, func(t *testing.T) {
			assertCompositionLoginStatus(t, composedPasswordLoginWithHeaders(router, carrier.headers), http.StatusOK)
		})
	}
}

func composedPasswordLoginWithHeaders(handler http.Handler, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", bytes.NewReader([]byte(`{"Username":"captain","Pw":"correct horse battery staple"}`)))
	req.RemoteAddr = "127.0.0.1:8096"
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

func composedLegacyAuthorizationRequest(handler http.Handler, headers map[string]string, query url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	request.RemoteAddr = "127.0.0.1:8096"
	request.URL.RawQuery = query.Encode()
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func assertCompositionAuthorizationStatus(t *testing.T, record *httptest.ResponseRecorder, want int) {
	t.Helper()
	if record.Code != want {
		t.Fatalf("authorization status = %d, want %d; body: %s", record.Code, want, record.Body.String())
	}
}

// The composed graph gives /Users/Public and password login the same network
// classifier. Local-only accounts remain selectable from LAN peers, including
// IPv6 and link-local addresses, but never from a public peer.
func TestApplicationPublicUsersAndLoginShareLocalityClassification(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	configureComposedAdministrator(t, app.Router())

	member, err := app.UserService().Create(t.Context(), "local-only", "correct password")
	if err != nil {
		t.Fatalf("create local-only user: %v", err)
	}
	member.Policy = []byte(`{"IsHidden":false,"IsDisabled":false,"EnableRemoteAccess":false,"EnableAllDevices":true}`)
	if err := app.UserService().Update(t.Context(), member); err != nil {
		t.Fatalf("persist local-only policy: %v", err)
	}

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
			public := compositionRequestFrom(app.Router(), http.MethodGet, "/Users/Public", nil, tc.remoteAddr, "")
			if public.Code != http.StatusOK {
				t.Fatalf("GET /Users/Public status = %d, want %d; body: %s", public.Code, http.StatusOK, public.Body.String())
			}
			var users []struct{ Name string }
			if err := json.Unmarshal(public.Body.Bytes(), &users); err != nil {
				t.Fatalf("decode public users: %v; body: %s", err, public.Body.String())
			}
			listed := false
			for _, user := range users {
				if user.Name == member.Name {
					listed = true
					break
				}
			}
			if listed != tc.local {
				t.Fatalf("GET /Users/Public listed local-only user = %t for peer %q, want %t; body: %s", listed, tc.remoteAddr, tc.local, public.Body.String())
			}

			login := compositionRequestFrom(app.Router(), http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"local-only","Pw":"correct password"}`), tc.remoteAddr, "")
			wantStatus := http.StatusForbidden
			if tc.local {
				wantStatus = http.StatusOK
			}
			assertCompositionLoginStatus(t, login, wantStatus)
			if admitted := login.Code == http.StatusOK; admitted != listed {
				t.Fatalf("login admission = %t but public visibility = %t for peer %q", admitted, listed, tc.remoteAddr)
			}
		})
	}
}

func composedLogin(t *testing.T, app *application, username, password, deviceID string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(struct {
		Username string `json:"Username"`
		Password string `json:"Pw"`
	}{Username: username, Password: password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:8096"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", `MediaBrowser Client="Composition Test", Device="Composition Device", DeviceId="`+deviceID+`", Version="1.0"`)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	return rec
}

func assertCompositionLoginStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("login status = %d, want source status %d; body: %s", rec.Code, want, rec.Body.String())
	}
}

func compositionAccessToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var result struct{ AccessToken string }
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode authentication result: %v; body: %s", err, rec.Body.String())
	}
	return result.AccessToken
}

func configureComposedAdministrator(t *testing.T, handler http.Handler) {
	t.Helper()

	assertHTTPStatus(t, handler, http.MethodPost, "/Startup/User", []byte(`{"Name":"captain","Password":"correct horse battery staple"}`), http.StatusNoContent)
	assertHTTPStatus(t, handler, http.MethodPost, "/Startup/Complete", nil, http.StatusNoContent)
}

func loginComposedAdministrator(t *testing.T, handler http.Handler) string {
	t.Helper()

	rec := authenticatedCompositionRequest(handler, http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"captain","Pw":"correct horse battery staple"}`), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var result struct {
		AccessToken string
		SessionInfo json.RawMessage
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode authentication result: %v", err)
	}
	if result.AccessToken == "" {
		t.Fatal("authentication result has an empty access token")
	}
	if len(result.SessionInfo) == 0 || bytes.Equal(result.SessionInfo, []byte("null")) {
		t.Fatalf("authentication result session info = %s, want session object", result.SessionInfo)
	}
	return result.AccessToken
}

func assertSessionCount(t *testing.T, handler http.Handler, token string, want int) {
	t.Helper()

	rec := authenticatedCompositionRequest(handler, http.MethodGet, "/Sessions", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("sessions status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var sessions []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if len(sessions) != want {
		t.Fatalf("active sessions = %d, want %d; body: %s", len(sessions), want, rec.Body.String())
	}
}

func assertDisplayPreferences(t *testing.T, handler http.Handler, path, token string) {
	t.Helper()

	rec := authenticatedCompositionRequest(handler, http.MethodGet, path, nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("display preferences status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var preferences map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &preferences); err != nil {
		t.Fatalf("decode display preferences: %v", err)
	}
	var sortBy string
	if err := json.Unmarshal(preferences["SortBy"], &sortBy); err != nil {
		t.Fatalf("decode persisted SortBy: %v; body: %s", err, rec.Body.String())
	}
	if sortBy != "DateCreated" {
		t.Fatalf("persisted SortBy = %q, want DateCreated", sortBy)
	}
	var rememberSorting bool
	if err := json.Unmarshal(preferences["RememberSorting"], &rememberSorting); err != nil {
		t.Fatalf("decode persisted RememberSorting: %v; body: %s", err, rec.Body.String())
	}
	if !rememberSorting {
		t.Fatal("persisted RememberSorting = false, want true")
	}
}

func assertAuthenticatedStatus(t *testing.T, handler http.Handler, method, path string, body []byte, token string, want int) {
	t.Helper()

	rec := authenticatedCompositionRequest(handler, method, path, body, token)
	if rec.Code != want {
		t.Fatalf("%s %s status = %d, want %d; body: %s", method, path, rec.Code, want, rec.Body.String())
	}
}

func authenticatedCompositionRequest(handler http.Handler, method, path string, body []byte, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:8096"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	authorization := `MediaBrowser Client="Deadfin Test", Device="Deadfin Browser", DeviceId="deadfin-test-device", Version="1.0"`
	if token != "" {
		authorization += `, Token="` + token + `"`
	}
	req.Header.Set("Authorization", authorization)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
