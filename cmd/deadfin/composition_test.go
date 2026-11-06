// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/events"
	"github.com/deadfin-org/deadfin/internal/user"
)

func TestApplicationComposesPersistentStartupAndAuthorization(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)

	assertCompositionRoutes(t, app.Router().Routes())
	assertHTTPStatus(t, app.Router(), http.MethodGet, "/startup/configuration", nil, http.StatusOK)
	assertHTTPStatus(t, app.Router(), http.MethodGet, "/localization/countries", nil, http.StatusOK)
	assertHTTPStatus(t, app.Router(), http.MethodGet, "/branding/css", nil, http.StatusOK)

	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Configuration", []byte(`{"ServerName":"Persistent Deadfin","UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}`), http.StatusNoContent)
	assertStartupServerName(t, app.Router(), "Persistent Deadfin")
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/User", []byte(`{"Name":"captain","Password":"correct horse battery staple"}`), http.StatusNoContent)

	assertPublicCompletion(t, app.Router(), false)
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Complete", nil, http.StatusNoContent)
	assertPublicCompletion(t, app.Router(), true)
	assertHTTPStatus(t, app.Router(), http.MethodGet, "/Startup/Configuration", nil, http.StatusUnauthorized)
	for _, path := range []string{
		"/Localization/Countries",
		"/Localization/Cultures",
		"/Localization/ParentalRatings",
		"/Localization/Options",
	} {
		assertHTTPStatus(t, app.Router(), http.MethodGet, path, nil, http.StatusUnauthorized)
	}

	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("close first application: %v", err)
	}
	app = nil

	assertFile(t, filepath.Join(root, "config", "system.xml"))
	assertFile(t, filepath.Join(root, "data", "deadfin.db"))

	app = newCompositionApplication(t, root)

	user, err := app.UserService().GetByName(t.Context(), "captain")
	if err != nil {
		t.Fatalf("load persisted startup user: %v", err)
	}
	verified, err := app.UserService().VerifyPassword(t.Context(), user.ID, "correct horse battery staple")
	if err != nil {
		t.Fatalf("verify persisted startup credential: %v", err)
	}
	if !verified {
		t.Fatal("persisted startup credential was not accepted after restart")
	}
	assertPublicCompletion(t, app.Router(), true)
	assertPublicServerName(t, app.Router(), "Persistent Deadfin")

	persistedConfiguration, err := os.ReadFile(filepath.Join(root, "config", "system.xml"))
	if err != nil {
		t.Fatalf("read persisted system configuration: %v", err)
	}
	if !bytes.Contains(persistedConfiguration, []byte("Persistent Deadfin")) {
		t.Fatalf("persisted system configuration does not contain updated server name: %s", persistedConfiguration)
	}
}

// Public discovery remains anonymous while the setup gate changes. It must
// therefore read the same live, persisted manager state as the startup
// endpoints rather than an identity snapshot captured during composition.
func TestApplicationPublicSystemInfoReadsPersistedStartupCompletionAcrossRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)

	assertComposedPublicStartupCompletion(t, app.Router(), false)
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Complete", nil, http.StatusNoContent)
	assertComposedPublicStartupCompletion(t, app.Router(), true)

	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("close completed application: %v", err)
	}
	app = newCompositionApplication(t, root)
	assertComposedPublicStartupCompletion(t, app.Router(), true)
}

func TestFreshApplicationKeepsStartupIncompleteAndAllowsAnonymousStartupRoutes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh-program-data")
	app := newCompositionApplication(t, root)

	systemConfiguration, err := os.ReadFile(filepath.Join(root, "config", "system.xml"))
	if err != nil {
		t.Fatalf("read fresh system configuration: %v", err)
	}
	if !bytes.Contains(systemConfiguration, []byte("<IsStartupWizardCompleted>false</IsStartupWizardCompleted>")) {
		t.Fatalf("fresh system configuration = %s, want persisted IsStartupWizardCompleted=false", systemConfiguration)
	}
	assertPublicCompletion(t, app.Router(), false)
	assertHTTPStatus(t, app.Router(), http.MethodGet, "/Startup/Configuration", nil, http.StatusOK)
	assertHTTPStatus(t, app.Router(), http.MethodGet, "/Startup/User", nil, http.StatusOK)
}

func TestWizardRemoteAccessConfigurationBlocksRemoteLoginAndAuthorizedRequests(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))

	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/RemoteAccess", []byte(`{"EnableRemoteAccess":false}`), http.StatusNoContent)
	configureComposedAdministrator(t, app.Router())
	captain, err := app.UserService().GetByName(t.Context(), "captain")
	if err != nil {
		t.Fatalf("load startup administrator: %v", err)
	}
	var policy struct{ EnableRemoteAccess bool }
	if err := json.Unmarshal(captain.Policy, &policy); err != nil {
		t.Fatalf("decode startup administrator policy: %v", err)
	}
	if !policy.EnableRemoteAccess {
		t.Fatal("startup administrator policy must allow remote access so the global setting is the denial source")
	}

	remoteLogin := compositionRequestFrom(app.Router(), http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"captain","Pw":"correct horse battery staple"}`), "198.51.100.80:8096", "")
	if remoteLogin.Code != http.StatusForbidden {
		t.Fatalf("remote password login after wizard disabled global remote access = %d, want %d; body: %s", remoteLogin.Code, http.StatusForbidden, remoteLogin.Body.String())
	}

	lanLogin := compositionRequestFrom(app.Router(), http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"captain","Pw":"correct horse battery staple"}`), "127.0.0.1:8096", "")
	if lanLogin.Code != http.StatusOK {
		t.Fatalf("LAN password login after wizard disabled global remote access = %d, want %d; body: %s", lanLogin.Code, http.StatusOK, lanLogin.Body.String())
	}
	token := compositionAccessToken(t, lanLogin)

	remoteProtected := compositionRequestFrom(app.Router(), http.MethodGet, "/System/Info", nil, "198.51.100.80:8096", token)
	if remoteProtected.Code != http.StatusForbidden {
		t.Fatalf("remote authenticated SystemInfo after wizard disabled global remote access = %d, want %d; body: %s", remoteProtected.Code, http.StatusForbidden, remoteProtected.Body.String())
	}
	lanProtected := compositionRequestFrom(app.Router(), http.MethodGet, "/System/Info", nil, "127.0.0.1:8096", token)
	if lanProtected.Code != http.StatusOK {
		t.Fatalf("LAN authenticated SystemInfo after wizard disabled global remote access = %d, want %d; body: %s", lanProtected.Code, http.StatusOK, lanProtected.Body.String())
	}
}

func TestDefaultApplicationSystemInfoPublishesRequestReachableAddressInsteadOfWildcard(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	const requestHost = "reachable.deadfin.test:8096"
	req := httptest.NewRequest(http.MethodGet, "http://"+requestHost+"/System/Info/Public", nil)
	req.Host = requestHost
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /System/Info/Public status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct{ LocalAddress string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode public system info: %v; body: %s", err, rec.Body.String())
	}
	if want := "http://" + requestHost; body.LocalAddress != want {
		t.Fatalf("default composition LocalAddress = %q, want request-reachable %q", body.LocalAddress, want)
	}
}

func TestApplicationHidesStartupAdministratorFromAnonymousPublicUsers(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))

	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Configuration", []byte(`{"ServerName":"Startup Visibility","UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}`), http.StatusNoContent)
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/User", []byte(`{"Name":"startup-admin","Password":"correct horse battery staple"}`), http.StatusNoContent)
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Complete", nil, http.StatusNoContent)

	rec := requestComposition(t, app.Router(), http.MethodGet, "/Users/Public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous public users status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]" {
		t.Fatalf("anonymous public users body = %s, want []", got)
	}
}

func TestPublicSystemInfoUsesUpdatedStartupServerNameImmediately(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))

	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Configuration", []byte(`{"ServerName":"Live Deadfin","UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}`), http.StatusNoContent)
	assertPublicServerName(t, app.Router(), "Live Deadfin")
}

func TestApplicationSystemInfoRefreshesConfiguredIdentityForPublicAndAuthenticatedResponses(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"))
	configureComposedAdministrator(t, app.Router())
	token := loginComposedAdministrator(t, app.Router())

	updateComposedServerName(t, app.Router(), token, "Before System Info Refresh")
	assertSystemInfoServerName(t, app.Router(), "", "Before System Info Refresh")
	assertSystemInfoServerName(t, app.Router(), token, "Before System Info Refresh")

	updateComposedServerName(t, app.Router(), token, "After System Info Refresh")
	assertSystemInfoServerName(t, app.Router(), "", "After System Info Refresh")
	assertSystemInfoServerName(t, app.Router(), token, "After System Info Refresh")
}

func TestApplicationSystemInfoUsesConfiguredItemsByNamePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom-program-data")
	app := newCompositionApplication(t, root)
	configureComposedAdministrator(t, app.Router())
	token := loginComposedAdministrator(t, app.Router())

	rec := authenticatedCompositionRequest(app.Router(), http.MethodGet, "/System/Info", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated system info status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		ItemsByNamePath string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode authenticated system info: %v; body: %s", err, rec.Body.String())
	}
	if want := filepath.Join(root, "metadata"); body.ItemsByNamePath != want {
		t.Fatalf("ItemsByNamePath = %q, want configured metadata path %q", body.ItemsByNamePath, want)
	}
}

func TestRouteCompositionProjectsConfiguredSystemInfoPathsAndPersistedCastReceivers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "configured-program-data")
	paths, err := config.NewPaths(root)
	if err != nil {
		t.Fatalf("derive configured paths: %v", err)
	}
	changes := events.NewBus[config.Change](clock.NewReal())
	manager, err := config.Open(t.Context(), paths, changes)
	if err != nil {
		t.Fatalf("open persisted system configuration: %v", err)
	}
	receivers := []config.CastReceiverApplication{
		{ID: "F007D354", Name: "Stable"},
		{ID: "6F511C87", Name: "Unstable"},
	}
	if err := manager.UpdateSystem(t.Context(), func(configuration *config.ServerConfiguration) error {
		configuration.CastReceiverApplications = receivers
		return nil
	}); err != nil {
		t.Fatalf("persist cast receiver applications: %v", err)
	}
	if err := changes.Close(t.Context()); err != nil {
		t.Fatalf("close configuration change bus: %v", err)
	}

	webPath := filepath.Join(t.TempDir(), "runtime-web-assets")
	rt := router.New()
	registerRoutes(rt, routeServices{
		identity: config.DefaultServerIdentity(),
		paths:    paths,
		webPath:  webPath,
		startup:  manager,
		manager:  manager,
		clock:    clock.NewReal(),
		users:    &user.Service{},
	})

	for _, tc := range []struct {
		name   string
		accept string
		camel  bool
	}{
		{name: "Pascal"},
		{name: "CamelCase", accept: `application/json; profile="CamelCase"`, camel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/System/Info", nil)
			req.Header.Set("Accept", tc.accept)
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /System/Info status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode GET /System/Info response: %v; body: %s", err, rec.Body.String())
			}
			assertComposedSystemInfoJSON(t, body, map[string]any{
				composedSystemInfoKey("ProgramDataPath", tc.camel):          paths.Root,
				composedSystemInfoKey("ItemsByNamePath", tc.camel):          paths.Metadata,
				composedSystemInfoKey("InternalMetadataPath", tc.camel):     paths.Metadata,
				composedSystemInfoKey("CachePath", tc.camel):                paths.Cache,
				composedSystemInfoKey("LogPath", tc.camel):                  paths.Log,
				composedSystemInfoKey("TranscodingTempPath", tc.camel):      paths.Transcodes,
				composedSystemInfoKey("WebPath", tc.camel):                  webPath,
				composedSystemInfoKey("CastReceiverApplications", tc.camel): composedReceiverJSON(receivers, tc.camel),
			})
		})
	}
}

func composedSystemInfoKey(field string, camel bool) string {
	if !camel {
		return field
	}
	return strings.ToLower(field[:1]) + field[1:]
}

func composedReceiverJSON(receivers []config.CastReceiverApplication, camel bool) []map[string]string {
	result := make([]map[string]string, len(receivers))
	for index, receiver := range receivers {
		result[index] = map[string]string{
			composedSystemInfoKey("Id", camel):   receiver.ID,
			composedSystemInfoKey("Name", camel): receiver.Name,
		}
	}
	return result
}

func assertComposedSystemInfoJSON(t *testing.T, body map[string]json.RawMessage, want map[string]any) {
	t.Helper()
	for key, expected := range want {
		expectedJSON, err := json.Marshal(expected)
		if err != nil {
			t.Fatalf("marshal expected %q value: %v", key, err)
		}
		if got, found := body[key]; !found {
			t.Errorf("GET /System/Info response missing %q", key)
		} else if string(got) != string(expectedJSON) {
			t.Errorf("GET /System/Info response %q = %s, want %s", key, got, expectedJSON)
		}
	}
}

func TestApplicationSystemInfoUsesConfiguredWebSocketPort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listen string
		want   int
	}{
		{name: "nonstandard port", listen: "127.0.0.1:19196", want: 19196},
		{name: "ephemeral port", listen: "127.0.0.1:0", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newCompositionApplicationWithListen(t, filepath.Join(t.TempDir(), "program-data"), tc.listen)
			configureComposedAdministrator(t, app.Router())
			token := loginComposedAdministrator(t, app.Router())

			rec := authenticatedCompositionRequest(app.Router(), http.MethodGet, "/System/Info", nil, token)
			if rec.Code != http.StatusOK {
				t.Fatalf("authenticated system info status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			var body struct {
				WebSocketPortNumber int
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode authenticated system info: %v; body: %s", err, rec.Body.String())
			}
			if body.WebSocketPortNumber != tc.want {
				t.Fatalf("WebSocketPortNumber = %d, want configured listen port %d", body.WebSocketPortNumber, tc.want)
			}
		})
	}
}

// Published request URIs are opt-in. Without that policy, discovery must use
// the configured bind address rather than client-controlled Host or forwarding
// headers; enabling it admits only the direct request URI.
func TestApplicationSystemInfoLocalAddressTrustsRequestURIOnlyWhenEnabled(t *testing.T) {
	const (
		listen      = "127.0.0.1:19196"
		configured  = "http://127.0.0.1:19196"
		requestHost = "published-request.example:9443"
	)

	for _, tc := range []struct {
		name             string
		enableRequestURI bool
		requestURL       string
		wantLocalAddress string
	}{
		{
			name:             "configured bind address ignores untrusted request metadata",
			requestURL:       "http://" + requestHost + "/System/Info/Public",
			wantLocalAddress: configured,
		},
		{
			name:             "enabled policy uses direct request URI but ignores forwarding headers",
			enableRequestURI: true,
			requestURL:       "https://" + requestHost + "/System/Info/Public",
			wantLocalAddress: "https://" + requestHost,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "program-data")
			if tc.enableRequestURI {
				configPath := filepath.Join(root, "config")
				if err := os.MkdirAll(configPath, 0o700); err != nil {
					t.Fatalf("create network configuration directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(configPath, "network.xml"), []byte("<NetworkConfiguration><EnablePublishedServerUriByRequest>true</EnablePublishedServerUriByRequest></NetworkConfiguration>"), 0o600); err != nil {
					t.Fatalf("write published request URI configuration: %v", err)
				}
			}

			app := newCompositionApplicationWithListen(t, root, listen)
			req := httptest.NewRequest(http.MethodGet, tc.requestURL, nil)
			req.Host = requestHost
			req.Header.Set("Forwarded", "for=198.51.100.73;proto=http;host=forwarded-attacker.invalid:6553")
			req.Header.Set("X-Forwarded-Host", "forwarded-attacker.invalid:6553")
			req.Header.Set("X-Forwarded-Proto", "http")
			rec := httptest.NewRecorder()
			app.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /System/Info/Public status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			var body struct {
				LocalAddress string
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode public system info: %v; body: %s", err, rec.Body.String())
			}
			if body.LocalAddress != tc.wantLocalAddress {
				t.Fatalf("LocalAddress = %q, want %q", body.LocalAddress, tc.wantLocalAddress)
			}
		})
	}
}

func TestDockerRuntimeProvidesWritableAbsoluteProgramDataForNonrootProcess(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	for _, want := range [][]byte{
		[]byte("ENV JELLYFIN_DATA_DIR=/var/lib/deadfin"),
		[]byte("mkdir -p /var/lib/deadfin"),
		[]byte("chown -R 65532:65532 /var/lib/deadfin"),
		[]byte("USER 65532:65532"),
	} {
		if !bytes.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile does not establish nonroot writable program-data contract %q", want)
		}
	}
	if bytes.Contains(dockerfile, []byte("ENV DEADFIN_DATA_DIR")) {
		t.Fatal("Dockerfile sets DEADFIN_DATA_DIR, which would outrank a deployment's JELLYFIN_DATA_DIR")
	}
	image := map[string]string{"JELLYFIN_DATA_DIR": "/var/lib/deadfin"}
	if got := resolveProgramData("", fakeEnv(image)); got != "/var/lib/deadfin" {
		t.Fatalf("container program-data environment resolves to %q, want /var/lib/deadfin", got)
	}
	// A deployment that overrides the image's JELLYFIN_DATA_DIR keeps its root.
	deployed := map[string]string{"JELLYFIN_DATA_DIR": "/var/lib/deadfin/data-old"}
	if got := resolveProgramData("", fakeEnv(deployed)); got != "/var/lib/deadfin/data-old" {
		t.Fatalf("deployment JELLYFIN_DATA_DIR resolves to %q, want /var/lib/deadfin/data-old", got)
	}
	deployed["DEADFIN_DATA_DIR"] = "/srv/deadfin"
	if got := resolveProgramData("", fakeEnv(deployed)); got != "/srv/deadfin" {
		t.Fatalf("deployment DEADFIN_DATA_DIR resolves to %q, want /srv/deadfin", got)
	}
}

func TestRouteLedgerRegistersExactRuntimeOperationsWithoutPersistentIO(t *testing.T) {
	workingDirectory := t.TempDir()
	t.Chdir(workingDirectory)

	routes := routeLedger().Routes()
	assertCompositionRoutes(t, routes)

	entries, err := os.ReadDir(workingDirectory)
	if err != nil {
		t.Fatalf("read route-ledger working directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("route ledger created persistent entries = %v, want none", entryNames(entries))
	}
}

func TestApplicationConstructionFailureReleasesPriorResources(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create program-data root: %v", err)
	}
	dataPath := filepath.Join(root, "data")
	if err := os.WriteFile(dataPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create blocking data path: %v", err)
	}

	if app, err := newApplication(t.Context(), root); err == nil {
		if app != nil {
			_ = app.Close(context.Background())
		}
		t.Fatal("newApplication with a non-directory data path succeeded")
	}

	if err := os.Remove(dataPath); err != nil {
		t.Fatalf("remove blocking data path after construction failure: %v", err)
	}
	app := newCompositionApplication(t, root)
	t.Cleanup(func() {
		if err := app.Close(context.Background()); err != nil {
			t.Errorf("close retried application: %v", err)
		}
	})
}

func TestResolveProgramDataPrioritizesFlagThenEnvironment(t *testing.T) {
	flagRoot := filepath.Join(t.TempDir(), "flag")
	deadfinRoot := filepath.Join(t.TempDir(), "deadfin")
	jellyfinRoot := filepath.Join(t.TempDir(), "jellyfin")

	for _, tc := range []struct {
		name string
		flag string
		env  map[string]string
		want string
	}{
		{"flag over both variables", "  " + flagRoot + "  ", map[string]string{"DEADFIN_DATA_DIR": deadfinRoot, "JELLYFIN_DATA_DIR": jellyfinRoot}, flagRoot},
		{"DEADFIN_DATA_DIR over JELLYFIN_DATA_DIR", " \t", map[string]string{"DEADFIN_DATA_DIR": "  " + deadfinRoot + "\n", "JELLYFIN_DATA_DIR": jellyfinRoot}, deadfinRoot},
		{"DEADFIN_DATA_DIR alone", "", map[string]string{"DEADFIN_DATA_DIR": deadfinRoot}, deadfinRoot},
		{"JELLYFIN_DATA_DIR alone", "", map[string]string{"JELLYFIN_DATA_DIR": jellyfinRoot}, jellyfinRoot},
		{"blank DEADFIN_DATA_DIR falls back", "", map[string]string{"DEADFIN_DATA_DIR": " ", "JELLYFIN_DATA_DIR": jellyfinRoot}, jellyfinRoot},
		{"default", "", nil, defaultProgramData()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveProgramData(tc.flag, fakeEnv(tc.env)); got != tc.want {
				t.Fatalf("resolveProgramData(%q, %v) = %q, want %q", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

func TestWarnUnusedJellyfinDirsNamesDataRootSubfolder(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	root := filepath.Join(t.TempDir(), "program-data")

	warnUnusedJellyfinDirs(t.Context(), logger, fakeEnv(map[string]string{
		"JELLYFIN_CONFIG_DIR": "/etc/jellyfin",
		"JELLYFIN_CACHE_DIR":  " ",
	}), root)

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("warnings = %d (%s), want 1", len(lines), buf.Bytes())
	}
	var record map[string]any
	if err := json.Unmarshal(lines[0], &record); err != nil {
		t.Fatalf("decode warning: %v", err)
	}
	if record["level"] != "WARN" || record["variable"] != "JELLYFIN_CONFIG_DIR" || record["value"] != "/etc/jellyfin" || record["path"] != filepath.Join(root, "config") {
		t.Fatalf("warning = %v, want WARN for JELLYFIN_CONFIG_DIR naming %s", record, filepath.Join(root, "config"))
	}
}

func fakeEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestInstallationIdentityCreatesPersistentDeviceFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)

	devicePath := filepath.Join(root, "data", "device.txt")
	assertFile(t, devicePath)
	info, err := os.Stat(devicePath)
	if err != nil {
		t.Fatalf("stat device.txt: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("device.txt mode = %o, want 0600", info.Mode().Perm())
	}

	id := composedPublicServerID(t, app.Router())
	if id == "" || id == dto.NilUUID.String() {
		t.Fatalf("public system info returned invalid installation ID %q", id)
	}

	raw, err := os.ReadFile(devicePath)
	if err != nil {
		t.Fatalf("read device.txt: %v", err)
	}
	if string(raw) != id {
		t.Fatalf("device.txt = %q, want public ID %q", string(raw), id)
	}
}

func TestInstallationIdentityIsStableAcrossRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)
	firstID := composedPublicServerID(t, app.Router())
	if firstID == "" {
		t.Fatal("first composition returned empty installation ID")
	}

	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("close first application: %v", err)
	}

	app = newCompositionApplication(t, root)
	secondID := composedPublicServerID(t, app.Router())
	if secondID != firstID {
		t.Fatalf("installation ID changed across restart: first=%s second=%s", firstID, secondID)
	}
}

func TestInstallationIdentityDiffersBetweenRoots(t *testing.T) {
	firstRoot := filepath.Join(t.TempDir(), "program-data-a")
	firstApp := newCompositionApplication(t, firstRoot)
	firstID := composedPublicServerID(t, firstApp.Router())

	secondRoot := filepath.Join(t.TempDir(), "program-data-b")
	secondApp := newCompositionApplication(t, secondRoot)
	secondID := composedPublicServerID(t, secondApp.Router())

	if firstID == secondID {
		t.Fatalf("two independent roots produced the same installation ID: %s", firstID)
	}
}

func TestInstallationIdentityIsNotCompiledConstant(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)
	id := composedPublicServerID(t, app.Router())

	compiled := config.DefaultServerIdentity()
	if id == compiled.ID.String() {
		t.Fatalf("installation ID %s matches the compiled constant %s; it must be random per install", id, compiled.ID.String())
	}
}

func TestInstallationIdentityNameUpdatePreservesID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)

	configureComposedAdministrator(t, app.Router())
	token := loginComposedAdministrator(t, app.Router())

	beforeID := composedPublicServerID(t, app.Router())
	if beforeID == "" {
		t.Fatal("public system info returned empty installation ID before name update")
	}

	updateComposedServerName(t, app.Router(), token, "Renamed Installation")
	assertPublicServerName(t, app.Router(), "Renamed Installation")

	afterID := composedPublicServerID(t, app.Router())
	if afterID != beforeID {
		t.Fatalf("installation ID changed after name update: before=%s after=%s", beforeID, afterID)
	}
}

func TestInstallationIdentityDefaultsToHostname(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	assertPublicServerName(t, app.Router(), hostname)
}

func TestInstallationIdentityClearedServerNameFallsBackToHostname(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	app := newCompositionApplication(t, root)

	configureComposedAdministrator(t, app.Router())
	token := loginComposedAdministrator(t, app.Router())

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}

	beforeID := composedPublicServerID(t, app.Router())
	if beforeID == "" {
		t.Fatal("public system info returned empty installation ID before name clear")
	}

	updateComposedServerName(t, app.Router(), token, "Named Installation")
	assertPublicServerName(t, app.Router(), "Named Installation")

	updateComposedServerName(t, app.Router(), token, "")
	assertPublicServerName(t, app.Router(), hostname)

	afterID := composedPublicServerID(t, app.Router())
	if afterID != beforeID {
		t.Fatalf("installation ID changed after name clear: before=%s after=%s", beforeID, afterID)
	}
}

func TestInstallationIdentityUnreadableDeviceFileFailsApplicationConstruction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program-data")
	if err := os.MkdirAll(filepath.Join(root, "data", "device.txt"), 0o700); err != nil {
		t.Fatalf("create device.txt as directory: %v", err)
	}

	_, err := newApplication(t.Context(), root)
	if err == nil {
		t.Fatal("expected application construction to fail for an unreadable identity file, got nil")
	}
	if !strings.Contains(err.Error(), "load installation identity") {
		t.Fatalf("error = %q, want one about loading installation identity", err.Error())
	}
}

func TestInstallationIdentityUnwritableDataDirFailsApplicationConstruction(t *testing.T) {
	skipIfRoot(t, "root bypasses directory permission checks, so the write cannot be forced to fail")
	root := filepath.Join(t.TempDir(), "program-data")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatalf("chmod data dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o700) })

	_, err := newApplication(t.Context(), root)
	if err == nil {
		t.Fatal("expected application construction to fail for an unwritable identity file, got nil")
	}
	// A read-only data directory makes construction fail. The SQLite open
	// or the identity write reports the failure before construction can
	// fall back to the compiled constant identity.
	if !strings.Contains(err.Error(), "program-data directory") &&
		!strings.Contains(err.Error(), "application database") &&
		!strings.Contains(err.Error(), "load installation identity") {
		t.Fatalf("error = %q, want one about the data directory, database, or installation identity", err.Error())
	}
}

func newCompositionApplication(t *testing.T, root string, options ...applicationOption) *application {
	t.Helper()

	app, err := newApplicationWithListen(t.Context(), root, defaultListenAddress, options...)
	if err != nil {
		t.Fatalf("newApplicationWithListen(%q): %v", root, err)
	}
	t.Cleanup(func() {
		if app != nil {
			if err := app.Close(context.Background()); err != nil {
				t.Errorf("close application: %v", err)
			}
		}
	})
	return app
}

func newCompositionApplicationWithListen(t *testing.T, root, listen string) *application {
	t.Helper()

	app, err := newApplicationWithListen(t.Context(), root, listen)
	if err != nil {
		t.Fatalf("newApplicationWithListen(%q, %q): %v", root, listen, err)
	}
	t.Cleanup(func() {
		if app != nil {
			if err := app.Close(context.Background()); err != nil {
				t.Errorf("close application: %v", err)
			}
		}
	})
	return app
}

func assertCompositionRoutes(t *testing.T, routes []router.Route) {
	t.Helper()

	want := map[string]router.Route{
		"GET /deadfin/v1/build":                                                 {OperationID: "NATIVE:GetBuild", Anonymous: true},
		"GET /deadfin/v1/setup":                                                 {OperationID: "NATIVE:UnlockSetup", Anonymous: true},
		"GET /deadfin/v1/workers/connect":                                       {OperationID: "NATIVE:TranscodeWorkerConnect", Worker: true},
		"GET /deadfin/v1/workers/jobs/{id}/source":                              {OperationID: "NATIVE:TranscodeWorkerSource", Worker: true},
		"PUT /deadfin/v1/workers/jobs/{id}/files/{name}":                        {OperationID: "NATIVE:TranscodeWorkerFile", Worker: true},
		"GET /deadfin/v1/workers/jobs/{id}/assets/{name}":                       {OperationID: "NATIVE:TranscodeWorkerAsset", Worker: true},
		"POST /deadfin/v1/workers/jobs/{id}/output":                             {OperationID: "NATIVE:TranscodeWorkerOutput", Worker: true},
		"GET /deadfin/v1/transcode-workers":                                     {OperationID: "NATIVE:GetTranscodeWorkers", Policy: string(auth.PolicyRequiresElevation)},
		"GET /deadfin/v1/transcode-workers/join-tokens":                         {OperationID: "NATIVE:GetTranscodeWorkerJoinTokens", Policy: string(auth.PolicyRequiresElevation)},
		"POST /deadfin/v1/transcode-workers/join-tokens":                        {OperationID: "NATIVE:CreateTranscodeWorkerJoinToken", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /deadfin/v1/transcode-workers/join-tokens/{tokenId}":            {OperationID: "NATIVE:RevokeTranscodeWorkerJoinToken", Policy: string(auth.PolicyRequiresElevation)},
		"GET /deadfin/v1/transcode-workers/pins":                                {OperationID: "NATIVE:GetTranscodeWorkerPins", Policy: string(auth.PolicyRequiresElevation)},
		"PUT /deadfin/v1/transcode-workers/pins":                                {OperationID: "NATIVE:SetTranscodeWorkerPins", Policy: string(auth.PolicyRequiresElevation)},
		"GET /deadfin/v1/transcode-workers/policy":                              {OperationID: "NATIVE:GetTranscodeWorkerPolicy", Policy: string(auth.PolicyRequiresElevation)},
		"PUT /deadfin/v1/transcode-workers/policy":                              {OperationID: "NATIVE:SetTranscodeWorkerPolicy", Policy: string(auth.PolicyRequiresElevation)},
		"GET /deadfin/v1/transcode-workers/plain-http":                          {OperationID: "NATIVE:GetTranscodeWorkerPlainHttp", Policy: string(auth.PolicyRequiresElevation)},
		"POST /deadfin/v1/transcode-workers/scaling-token":                      {OperationID: "NATIVE:CreateTranscodeWorkerScalingToken", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /deadfin/v1/transcode-workers/scaling-token":                    {OperationID: "NATIVE:DeleteTranscodeWorkerScalingToken", Policy: string(auth.PolicyRequiresElevation)},
		"GET /deadfin/v1/metrics":                                               {OperationID: "NATIVE:GetMetrics", Policy: string(auth.PolicyRequiresElevation), ScalingCredential: true},
		"GET /deadfin/v1/workers/scaling":                                       {OperationID: "NATIVE:GetTranscodeWorkerScaling", Policy: string(auth.PolicyRequiresElevation), ScalingCredential: true},
		"POST /deadfin/v1/transcode-workers/{workerId}":                         {OperationID: "NATIVE:UpdateTranscodeWorker", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /deadfin/v1/transcode-workers/{workerId}":                       {OperationID: "NATIVE:RevokeTranscodeWorker", Policy: string(auth.PolicyRequiresElevation)},
		"GET /deadfin/v1/livetv/tunerhosts/{id}/groups":                         {OperationID: "NATIVE:GetTunerHostGroups", Policy: "RequiresElevation"},
		"GET /System/Info/Public":                                               {OperationID: "GetPublicSystemInfo", Anonymous: true},
		"GET /System/Info":                                                      {OperationID: "GetSystemInfo", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /System/Endpoint":                                                  {OperationID: "GetEndpointInfo", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /System/Ping":                                                      {OperationID: "GetPingSystem", Anonymous: true},
		"POST /System/Ping":                                                     {OperationID: "PostPingSystem", Anonymous: true},
		"GET /System/Configuration":                                             {OperationID: "GetConfiguration", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /System/Configuration/{key}":                                       {OperationID: "GetNamedConfiguration", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /System/Configuration":                                            {OperationID: "UpdateConfiguration", Policy: string(auth.PolicyRequiresElevation)},
		"POST /System/Configuration/Branding":                                   {OperationID: "UpdateBrandingConfiguration", Policy: string(auth.PolicyRequiresElevation)},
		"POST /System/Configuration/{key}":                                      {OperationID: "UpdateNamedConfiguration", Policy: string(auth.PolicyRequiresElevation)},
		"GET /System/ActivityLog/Entries":                                       {OperationID: "GetLogEntries", Policy: string(auth.PolicyRequiresElevation)},
		"GET /System/Logs":                                                      {OperationID: "GetServerLogs", Policy: string(auth.PolicyRequiresElevation)},
		"GET /System/Logs/Log":                                                  {OperationID: "GetLogFile", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Devices":                                                          {OperationID: "GetDevices", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /Devices":                                                       {OperationID: "DeleteDevice", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Devices/Info":                                                     {OperationID: "GetDeviceInfo", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Devices/Options":                                                  {OperationID: "GetDeviceOptions", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Devices/Options":                                                 {OperationID: "UpdateDeviceOptions", Policy: string(auth.PolicyRequiresElevation)},
		"GET /System/Info/Storage":                                              {OperationID: "GetSystemStorage", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Library/Refresh":                                                 {OperationID: "RefreshLibrary", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Items/{itemId}/Refresh":                                          {OperationID: "RefreshItem", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Items/{itemId}":                                                  {OperationID: "UpdateItem", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Items/{itemId}/MetadataEditor":                                    {OperationID: "GetMetadataEditorInfo", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Items/{itemId}/ContentType":                                      {OperationID: "UpdateItemContentType", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /Items/{itemId}/Images/{imageType}":                             {OperationID: "DeleteItemImage", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /Items/{itemId}/Images/{imageType}/{imageIndex}":                {OperationID: "DeleteItemImageByIndex", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Items/{itemId}/Images/{imageType}":                               {OperationID: "SetItemImage", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Items/{itemId}/Images/{imageType}/{imageIndex}":                  {OperationID: "SetItemImageByIndex", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Items/{itemId}/Images/{imageType}/{imageIndex}/Index":            {OperationID: "UpdateItemImageIndex", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Videos/{itemId}/Subtitles":                                       {OperationID: "UploadSubtitle", Policy: string(auth.PolicySubtitleManagement)},
		"DELETE /Videos/{itemId}/Subtitles/{index}":                             {OperationID: "DeleteSubtitle", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Startup/Configuration":                                            {OperationID: "GetStartupConfiguration", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"POST /Startup/Configuration":                                           {OperationID: "UpdateInitialConfiguration", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"POST /Startup/RemoteAccess":                                            {OperationID: "SetRemoteAccess", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"GET /Startup/User":                                                     {OperationID: "GetFirstUser", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"GET /Startup/FirstUser":                                                {OperationID: "GetFirstUser_2", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"POST /Startup/User":                                                    {OperationID: "UpdateStartupUser", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"POST /Startup/Complete":                                                {OperationID: "CompleteWizard", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"GET /Branding/Configuration":                                           {OperationID: "GetBrandingOptions", Anonymous: true},
		"GET /Branding/Css":                                                     {OperationID: "GetBrandingCss", Anonymous: true},
		"GET /Branding/Css.css":                                                 {OperationID: "GetBrandingCss_2", Anonymous: true},
		"GET /Branding/Splashscreen":                                            {OperationID: "GetSplashscreen", Anonymous: true},
		"POST /Branding/Splashscreen":                                           {OperationID: "UploadCustomSplashscreen", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /Branding/Splashscreen":                                         {OperationID: "DeleteCustomSplashscreen", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Collections":                                                     {OperationID: "CreateCollection", Policy: string(auth.PolicyCollectionManagement)},
		"POST /Collections/{collectionId}/Items":                                {OperationID: "AddToCollection", Policy: string(auth.PolicyCollectionManagement)},
		"DELETE /Collections/{collectionId}/Items":                              {OperationID: "RemoveFromCollection", Policy: string(auth.PolicyCollectionManagement)},
		"POST /Playlists":                                                       {OperationID: "CreatePlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Playlists/{playlistId}":                                          {OperationID: "UpdatePlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Playlists/{playlistId}":                                           {OperationID: "GetPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Playlists/{playlistId}/Users":                                     {OperationID: "GetPlaylistUsers", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Playlists/{playlistId}/Users/{userId}":                            {OperationID: "GetPlaylistUser", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Playlists/{playlistId}/Users/{userId}":                           {OperationID: "UpdatePlaylistUser", Policy: string(auth.PolicyDefaultAuthorization)},
		"DELETE /Playlists/{playlistId}/Users/{userId}":                         {OperationID: "RemoveUserFromPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Playlists/{playlistId}/Items":                                    {OperationID: "AddItemToPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Playlists/{playlistId}/Items/{itemId}/Move/{newIndex}":           {OperationID: "MoveItem", Policy: string(auth.PolicyDefaultAuthorization)},
		"DELETE /Playlists/{playlistId}/Items":                                  {OperationID: "RemoveItemFromPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Playlists/{playlistId}/Items":                                     {OperationID: "GetPlaylistItems", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Localization/Countries":                                           {OperationID: "GetCountries", Policy: string(auth.PolicyFirstTimeSetupOrDefault)},
		"GET /Localization/Cultures":                                            {OperationID: "GetCultures", Policy: string(auth.PolicyFirstTimeSetupOrDefault)},
		"GET /Localization/ParentalRatings":                                     {OperationID: "GetParentalRatings", Policy: string(auth.PolicyFirstTimeSetupOrDefault)},
		"GET /Localization/Options":                                             {OperationID: "GetLocalizationOptions", Policy: string(auth.PolicyFirstTimeSetupOrDefault)},
		"POST /Users/AuthenticateByName":                                        {OperationID: "AuthenticateUserByName", Anonymous: true},
		"POST /Users/{userId}/Authenticate":                                     {OperationID: "SHADOW:AuthenticateUser", Anonymous: true},
		"GET /Users/Me":                                                         {OperationID: "GetCurrentUser", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Users/Public":                                                     {OperationID: "GetPublicUsers", Anonymous: true},
		"GET /Users/{userId}":                                                   {OperationID: "GetUserById", Policy: string(auth.PolicyIgnoreParentalControl)},
		"GET /Users":                                                            {OperationID: "GetUsers", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Users/Configuration":                                             {OperationID: "UpdateUserConfiguration", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Users/{userId}/Configuration":                                    {OperationID: "SHADOW:UpdateUserConfigurationLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Users/Password":                                                  {OperationID: "UpdateUserPassword", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Users/{userId}/Password":                                         {OperationID: "SHADOW:UpdateUserPasswordLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Users/{userId}/Policy":                                           {OperationID: "UpdateUserPolicy", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Users/New":                                                       {OperationID: "CreateUserByName", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /Users/{userId}":                                                {OperationID: "DeleteUser", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Users":                                                           {OperationID: "UpdateUser", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Users/{userId}":                                                  {OperationID: "SHADOW:UpdateUserLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Library/Media/Updated":                                           {OperationID: "PostUpdatedMedia", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Library/Series/Updated":                                          {OperationID: "PostUpdatedSeries", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Library/Series/Added":                                            {OperationID: "PostAddedSeries", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Library/Movies/Updated":                                          {OperationID: "PostUpdatedMovies", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Library/Movies/Added":                                            {OperationID: "PostAddedMovies", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Auth/Keys":                                                        {OperationID: "GetKeys", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Auth/Keys":                                                       {OperationID: "CreateKey", Policy: string(auth.PolicyRequiresElevation)},
		"POST /ClientLog/Document":                                              {OperationID: "LogFile", Policy: string(auth.PolicyDefaultAuthorization)},
		"DELETE /Auth/Keys/{key}":                                               {OperationID: "RevokeKey", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Sessions":                                                         {OperationID: "GetSessions", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /socket":                                                           {OperationID: "OpenWebSocket", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/Capabilities/Full":                                      {OperationID: "PostFullCapabilities", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/Capabilities":                                           {OperationID: "PostCapabilities", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/Logout":                                                 {OperationID: "ReportSessionEnded", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/{sessionId}/Command/{command}":                          {OperationID: "SendGeneralCommand", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/{sessionId}/Message":                                    {OperationID: "SendMessageCommand", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/{sessionId}/Playing":                                    {OperationID: "Play", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/{sessionId}/Playing/{command}":                          {OperationID: "SendPlaystateCommand", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/{sessionId}/Viewing":                                    {OperationID: "DisplayContent", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/Playing":                                                {OperationID: "ReportPlaybackStart", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/Playing/Progress":                                       {OperationID: "ReportPlaybackProgress", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/Playing/Stopped":                                        {OperationID: "ReportPlaybackStopped", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Sessions/Playing/Ping":                                           {OperationID: "PingPlaybackSession", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /PlayingItems/{itemId}":                                           {OperationID: "SHADOW:OnPlaybackStart", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Users/{userId}/PlayingItems/{itemId}":                            {OperationID: "SHADOW:OnPlaybackStartLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /PlayingItems/{itemId}/Progress":                                  {OperationID: "SHADOW:OnPlaybackProgress", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Users/{userId}/PlayingItems/{itemId}/Progress":                   {OperationID: "SHADOW:OnPlaybackProgressLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"DELETE /PlayingItems/{itemId}":                                         {OperationID: "SHADOW:OnPlaybackStopped", Policy: string(auth.PolicyDefaultAuthorization)},
		"DELETE /Users/{userId}/PlayingItems/{itemId}":                          {OperationID: "SHADOW:OnPlaybackStoppedLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /DisplayPreferences/{displayPreferencesId}":                        {OperationID: "GetDisplayPreferences", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /DisplayPreferences/{displayPreferencesId}":                       {OperationID: "UpdateDisplayPreferences", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/PlaybackInfo":                                      {OperationID: "GetPlaybackInfo", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/{itemId}/PlaybackInfo":                                     {OperationID: "GetPostedPlaybackInfo", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /LiveStreams/Open":                                                {OperationID: "OpenLiveStream", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /LiveStreams/Close":                                               {OperationID: "CloseLiveStream", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Playback/BitrateTest":                                             {OperationID: "GetBitrateTestBytes", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{itemId}/stream":                                           {OperationID: "GetVideoStream", Policy: "DefaultAuthorization", PlaySessionCredential: true},
		"HEAD /Videos/{itemId}/stream":                                          {OperationID: "HeadVideoStream", Policy: "DefaultAuthorization", PlaySessionCredential: true},
		"GET /Videos/{itemId}/stream.{container}":                               {OperationID: "GetVideoStreamByContainer", Policy: "DefaultAuthorization", PlaySessionCredential: true},
		"HEAD /Videos/{itemId}/stream.{container}":                              {OperationID: "HeadVideoStreamByContainer", Policy: "DefaultAuthorization", PlaySessionCredential: true},
		"GET /Audio/{itemId}/stream":                                            {OperationID: "GetAudioStream", Policy: "DefaultAuthorization", PlaySessionCredential: true},
		"HEAD /Audio/{itemId}/stream":                                           {OperationID: "HeadAudioStream", Policy: "DefaultAuthorization", PlaySessionCredential: true},
		"GET /Audio/{itemId}/stream.{container}":                                {OperationID: "GetAudioStreamByContainer", Policy: "DefaultAuthorization", PlaySessionCredential: true},
		"HEAD /Audio/{itemId}/stream.{container}":                               {OperationID: "HeadAudioStreamByContainer", Policy: "DefaultAuthorization", PlaySessionCredential: true},
		"GET /Audio/{itemId}/universal":                                         {OperationID: "GetUniversalAudioStream", Policy: string(auth.PolicyDefaultAuthorization)},
		"HEAD /Audio/{itemId}/universal":                                        {OperationID: "HeadUniversalAudioStream", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/Download":                                          {OperationID: "GetDownload", Policy: string(auth.PolicyDownload)},
		"GET /Videos/{itemId}/live.m3u8":                                        {OperationID: "SHADOW:GetLiveHlsStream", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{itemId}/master.m3u8":                                      {OperationID: "SHADOW:GetMasterHlsVideoPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"HEAD /Videos/{itemId}/master.m3u8":                                     {OperationID: "SHADOW:HeadMasterHlsVideoPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Audio/{itemId}/master.m3u8":                                       {OperationID: "SHADOW:GetMasterHlsAudioPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"HEAD /Audio/{itemId}/master.m3u8":                                      {OperationID: "SHADOW:HeadMasterHlsAudioPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{itemId}/main.m3u8":                                        {OperationID: "SHADOW:GetVariantHlsVideoPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Audio/{itemId}/main.m3u8":                                         {OperationID: "SHADOW:GetVariantHlsAudioPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{itemId}/hls1/{playlistId}/{segmentId}.{container}":        {OperationID: "SHADOW:GetHlsVideoSegment", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Audio/{itemId}/hls1/{playlistId}/{segmentId}.{container}":         {OperationID: "SHADOW:GetHlsAudioSegment", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Audio/{itemId}/hls/{segmentId}/stream.mp3":                        {OperationID: "SHADOW:GetHlsAudioSegmentLegacyMp3", Anonymous: true},
		"GET /Audio/{itemId}/hls/{segmentId}/stream.aac":                        {OperationID: "SHADOW:GetHlsAudioSegmentLegacyAac", Anonymous: true},
		"GET /Videos/{itemId}/hls/{playlistId}/stream.m3u8":                     {OperationID: "SHADOW:GetHlsPlaylistLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{itemId}/hls/{playlistId}/{segmentId}.{segmentContainer}":  {OperationID: "SHADOW:GetHlsVideoSegmentLegacy", Anonymous: true},
		"DELETE /Videos/ActiveEncodings":                                        {OperationID: "SHADOW:StopEncodingProcess", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{itemId}/{mediaSourceId}/Subtitles/{index}/subtitles.m3u8": {OperationID: "GetSubtitlePlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{routeItemId}/{routeMediaSourceId}/Subtitles/{routeIndex}/Stream.{routeFormat}":                           {OperationID: "GetSubtitle", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{routeItemId}/{routeMediaSourceId}/Subtitles/{routeIndex}/{routeStartPositionTicks}/Stream.{routeFormat}": {OperationID: "GetSubtitleWithTicks", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{videoId}/{mediaSourceId}/Attachments/{index}":                                                            {OperationID: "GetAttachment", Anonymous: true, OptionalIdentity: true},
		"GET /Videos/{itemId}/Trickplay/{width}/tiles.m3u8":                                                                    {OperationID: "GetTrickplayHlsPlaylist", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{itemId}/Trickplay/{width}/{index}.jpg":                                                                   {OperationID: "GetTrickplayTileImage", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Library/MediaFolders":                                {OperationID: "GetMediaFolders", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Library/VirtualFolders":                              {OperationID: "GetVirtualFolders", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"POST /Library/VirtualFolders":                             {OperationID: "AddVirtualFolder", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"DELETE /Library/VirtualFolders":                           {OperationID: "RemoveVirtualFolder", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"POST /Library/VirtualFolders/Paths":                       {OperationID: "AddMediaPath", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"DELETE /Library/VirtualFolders/Paths":                     {OperationID: "RemoveMediaPath", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"POST /Library/VirtualFolders/LibraryOptions":              {OperationID: "UpdateLibraryOptions", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"GET /Libraries/AvailableOptions":                          {OperationID: "GetLibraryOptionsInfo", Policy: string(auth.PolicyFirstTimeSetupOrDefault)},
		"GET /Environment/DirectoryContents":                       {OperationID: "GetDirectoryContents", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"POST /Environment/ValidatePath":                           {OperationID: "ValidatePath", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"GET /Environment/Drives":                                  {OperationID: "GetDrives", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"GET /Environment/ParentPath":                              {OperationID: "GetParentPath", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"GET /Environment/DefaultDirectoryBrowser":                 {OperationID: "GetDefaultDirectoryBrowser", Policy: string(auth.PolicyFirstTimeSetupOrElevated)},
		"GET /Items/{itemId}/Images":                               {OperationID: "GetItemImageInfos", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/Images/{imageType}":                   {OperationID: "GetItemImage", Anonymous: true},
		"HEAD /Items/{itemId}/Images/{imageType}":                  {OperationID: "HeadItemImage", Anonymous: true},
		"GET /Persons/{name}/Images/{imageType}":                   {OperationID: "GetPersonImage", Anonymous: true},
		"GET /Persons/{name}/Images/{imageType}/{imageIndex}":      {OperationID: "GetPersonImageByIndex", Anonymous: true},
		"HEAD /Persons/{name}/Images/{imageType}":                  {OperationID: "HeadPersonImage", Anonymous: true},
		"HEAD /Persons/{name}/Images/{imageType}/{imageIndex}":     {OperationID: "HeadPersonImageByIndex", Anonymous: true},
		"GET /Genres/{name}/Images/{imageType}":                    {OperationID: "GetGenreImage", Anonymous: true},
		"GET /Genres/{name}/Images/{imageType}/{imageIndex}":       {OperationID: "GetGenreImageByIndex", Anonymous: true},
		"HEAD /Genres/{name}/Images/{imageType}":                   {OperationID: "HeadGenreImage", Anonymous: true},
		"HEAD /Genres/{name}/Images/{imageType}/{imageIndex}":      {OperationID: "HeadGenreImageByIndex", Anonymous: true},
		"GET /MusicGenres/{name}/Images/{imageType}":               {OperationID: "GetMusicGenreImage", Anonymous: true},
		"GET /MusicGenres/{name}/Images/{imageType}/{imageIndex}":  {OperationID: "GetMusicGenreImageByIndex", Anonymous: true},
		"HEAD /MusicGenres/{name}/Images/{imageType}":              {OperationID: "HeadMusicGenreImage", Anonymous: true},
		"HEAD /MusicGenres/{name}/Images/{imageType}/{imageIndex}": {OperationID: "HeadMusicGenreImageByIndex", Anonymous: true},
		"GET /Studios/{name}/Images/{imageType}":                   {OperationID: "GetStudioImage", Anonymous: true},
		"GET /Studios/{name}/Images/{imageType}/{imageIndex}":      {OperationID: "GetStudioImageByIndex", Anonymous: true},
		"HEAD /Studios/{name}/Images/{imageType}":                  {OperationID: "HeadStudioImage", Anonymous: true},
		"HEAD /Studios/{name}/Images/{imageType}/{imageIndex}":     {OperationID: "HeadStudioImageByIndex", Anonymous: true},
		"GET /Artists/{name}/Images/{imageType}/{imageIndex}":      {OperationID: "GetArtistImage", Anonymous: true},
		"HEAD /Artists/{name}/Images/{imageType}/{imageIndex}":     {OperationID: "HeadArtistImage", Anonymous: true},
		"GET /Items/{itemId}/Images/{imageType}/{imageIndex}":      {OperationID: "GetItemImageByIndex", Anonymous: true},
		"HEAD /Items/{itemId}/Images/{imageType}/{imageIndex}":     {OperationID: "HeadItemImageByIndex", Anonymous: true},
		"GET /Items/{itemId}/Images/{imageType}/{imageIndex}/{tag}/{format}/{maxWidth}/{maxHeight}/{percentPlayed}/{unplayedCount}":  {OperationID: "GetItemImage2", Anonymous: true},
		"HEAD /Items/{itemId}/Images/{imageType}/{imageIndex}/{tag}/{format}/{maxWidth}/{maxHeight}/{percentPlayed}/{unplayedCount}": {OperationID: "HeadItemImage2", Anonymous: true},
		"GET /UserImage":                                     {OperationID: "GetUserImage", Anonymous: true},
		"HEAD /UserImage":                                    {OperationID: "HeadUserImage", Anonymous: true},
		"GET /Items":                                         {OperationID: "GetItems", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Users/{userId}/Items":                          {OperationID: "SHADOW:GetItemsLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/Counts":                                  {OperationID: "GetItemCounts", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}":                                {OperationID: "GetItem", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Users/{userId}/Items/{itemId}":                 {OperationID: "SHADOW:GetItemLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /UserItems/Resume":                              {OperationID: "GetResumeItems", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Users/{userId}/Items/Resume":                   {OperationID: "SHADOW:GetResumeItemsLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/Latest":                                  {OperationID: "GetLatestMedia", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Search/Hints":                                  {OperationID: "GetSearchHints", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Users/{userId}/Items/Latest":                   {OperationID: "SHADOW:GetLatestMediaLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /UserItems/{itemId}/UserData":                  {OperationID: "UpdateItemUserData", Policy: string(auth.PolicyDefaultAuthorization)},
		"DELETE /UserItems/{itemId}/UserData":                {OperationID: "SHADOW:DeleteItemUserData", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /UserPlayedItems/{itemId}":                     {OperationID: "MarkPlayedItem", Policy: string(auth.PolicyDefaultAuthorization)},
		"DELETE /UserPlayedItems/{itemId}":                   {OperationID: "MarkUnplayedItem", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /UserFavoriteItems/{itemId}":                   {OperationID: "MarkFavoriteItem", Policy: string(auth.PolicyDefaultAuthorization)},
		"DELETE /UserFavoriteItems/{itemId}":                 {OperationID: "UnmarkFavoriteItem", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/Similar":                        {OperationID: "GetSimilarItems", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Artists/{itemId}/Similar":                      {OperationID: "GetSimilarArtists", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Albums/{itemId}/Similar":                       {OperationID: "GetSimilarAlbums", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Shows/{itemId}/Similar":                        {OperationID: "GetSimilarShows", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Movies/{itemId}/Similar":                       {OperationID: "GetSimilarMovies", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Trailers/{itemId}/Similar":                     {OperationID: "GetSimilarTrailers", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/SpecialFeatures":                {OperationID: "GetSpecialFeatures", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Users/{userId}/Items/{itemId}/SpecialFeatures": {OperationID: "SHADOW:GetSpecialFeaturesLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/LocalTrailers":                  {OperationID: "GetLocalTrailers", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Users/{userId}/Items/{itemId}/LocalTrailers":   {OperationID: "SHADOW:GetLocalTrailersLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/Intros":                         {OperationID: "GetIntros", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Users/{userId}/Items/{itemId}/Intros":          {OperationID: "SHADOW:GetIntrosLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Videos/{itemId}/AdditionalParts":               {OperationID: "GetAdditionalPart", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/Ancestors":                      {OperationID: "GetAncestors", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/ThemeMedia":                     {OperationID: "GetThemeMedia", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /MediaSegments/{itemId}":                        {OperationID: "GetItemSegments", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Shows/NextUp":                                  {OperationID: "GetNextUp", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Shows/{seriesId}/Episodes":                     {OperationID: "GetEpisodes", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Shows/{seriesId}/Seasons":                      {OperationID: "GetSeasons", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Genres":                                        {OperationID: "GetGenres", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Persons":                                       {OperationID: "GetPersons", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Artists":                                       {OperationID: "GetArtists", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Studios":                                       {OperationID: "GetStudios", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Artists/AlbumArtists":                          {OperationID: "GetAlbumArtists", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Persons/{name}":                                {OperationID: "GetPerson", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Genres/{genreName}":                            {OperationID: "GetGenre", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /MusicGenres/{genreName}":                       {OperationID: "GetMusicGenre", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Studios/{name}":                                {OperationID: "GetStudio", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Artists/{name}":                                {OperationID: "GetArtistByName", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/Filters":                                 {OperationID: "GetQueryFiltersLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/Filters2":                                {OperationID: "GetQueryFilters", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /UserViews":                                     {OperationID: "GetUserViews", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Users/{userId}/Views":                          {OperationID: "SHADOW:GetUserViewsLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /UserViews/GroupingOptions":                     {OperationID: "GetGroupingOptions", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/Movie":                     {OperationID: "GetMovieRemoteSearchResults", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/Trailer":                   {OperationID: "GetTrailerRemoteSearchResults", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/MusicVideo":                {OperationID: "GetMusicVideoRemoteSearchResults", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/Series":                    {OperationID: "GetSeriesRemoteSearchResults", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/BoxSet":                    {OperationID: "GetBoxSetRemoteSearchResults", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/MusicArtist":               {OperationID: "GetMusicArtistRemoteSearchResults", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/MusicAlbum":                {OperationID: "GetMusicAlbumRemoteSearchResults", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/Person":                    {OperationID: "GetPersonRemoteSearchResults", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Items/RemoteSearch/Book":                      {OperationID: "GetBookRemoteSearchResults", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/RemoteSearch/Apply/{itemId}":            {OperationID: "ApplySearchCriteria", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Items/{itemId}/RemoteImages":                   {OperationID: "GetRemoteImages", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/RemoteImages/Providers":         {OperationID: "GetRemoteImageProviders", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Items/{itemId}/RemoteImages/Download":         {OperationID: "DownloadRemoteImage", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /Items/{itemId}":                             {OperationID: "DeleteItem", Policy: string(auth.PolicyDefaultAuthorization)},
		"DELETE /Items":                                      {OperationID: "DeleteItems", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /Items/{itemId}/ExternalIdInfos":                {OperationID: "GetExternalIdInfos", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Users/{userId}/GroupingOptions":                {OperationID: "SHADOW:GetGroupingOptionsLegacy", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /QuickConnect/Enabled":                          {OperationID: "GetQuickConnectEnabled", Anonymous: true},
		"POST /QuickConnect/Initiate":                        {OperationID: "InitiateQuickConnect", Anonymous: true},
		"GET /QuickConnect/Connect":                          {OperationID: "GetQuickConnectState", Anonymous: true},
		"POST /QuickConnect/Authorize":                       {OperationID: "AuthorizeQuickConnect", Policy: string(auth.PolicyDefaultAuthorization)},
		"POST /Users/AuthenticateWithQuickConnect":           {OperationID: "AuthenticateWithQuickConnect", Anonymous: true},
		"GET /ScheduledTasks":                                {OperationID: "GetTasks", Policy: string(auth.PolicyRequiresElevation)},
		"GET /ScheduledTasks/{taskId}":                       {OperationID: "GetTask", Policy: string(auth.PolicyRequiresElevation)},
		"POST /ScheduledTasks/{taskId}/Triggers":             {OperationID: "UpdateTask", Policy: string(auth.PolicyRequiresElevation)},
		"POST /ScheduledTasks/Running/{taskId}":              {OperationID: "StartTask", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /ScheduledTasks/Running/{taskId}":            {OperationID: "StopTask", Policy: string(auth.PolicyRequiresElevation)},
		// Deferred-feature stubs (WP-P9-03): LiveTv, SyncPlay, Plugin, Backup.
		"DELETE /LiveTv/ListingProviders":                           {OperationID: "DeleteListingProvider", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /LiveTv/Recordings/{recordingId}":                   {OperationID: "DeleteRecording", Policy: string(auth.PolicyLiveTVManagement)},
		"DELETE /LiveTv/SeriesTimers/{timerId}":                     {OperationID: "CancelSeriesTimer", Policy: string(auth.PolicyLiveTVManagement)},
		"DELETE /LiveTv/Timers/{timerId}":                           {OperationID: "CancelTimer", Policy: string(auth.PolicyLiveTVManagement)},
		"DELETE /LiveTv/TunerHosts":                                 {OperationID: "DeleteTunerHost", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /Packages/Installing/{packageId}":                   {OperationID: "CancelPackageInstallation", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /Plugins/{pluginId}":                                {OperationID: "UninstallPlugin", Policy: string(auth.PolicyRequiresElevation)},
		"DELETE /Plugins/{pluginId}/{version}":                      {OperationID: "UninstallPluginByVersion", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Backup":                                               {OperationID: "ListBackups", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Backup/Manifest":                                      {OperationID: "GetBackup", Policy: string(auth.PolicyRequiresElevation)},
		"GET /LiveTv/ChannelMappingOptions":                         {OperationID: "GetChannelMappingOptions", Policy: string(auth.PolicyRequiresElevation)},
		"GET /LiveTv/Channels":                                      {OperationID: "GetLiveTvChannels", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Channels/{channelId}":                          {OperationID: "GetChannel", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/GuideInfo":                                     {OperationID: "GetGuideInfo", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Info":                                          {OperationID: "GetLiveTvInfo", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/ListingProviders/Default":                      {OperationID: "GetDefaultListingProvider", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/ListingProviders/Lineups":                      {OperationID: "GetLineups", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/ListingProviders/SchedulesDirect/Countries":    {OperationID: "GetSchedulesDirectCountries", Policy: string(auth.PolicyRequiresElevation)},
		"GET /LiveTv/LiveRecordings/{recordingId}/stream":           {OperationID: "GetLiveRecordingFile", Anonymous: true},
		"GET /LiveTv/LiveStreamFiles/{streamId}/stream.{container}": {OperationID: "GetLiveStreamFile", Anonymous: true},
		"GET /LiveTv/Programs":                                      {OperationID: "GetLiveTvPrograms", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Programs/Recommended":                          {OperationID: "GetRecommendedPrograms", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Programs/{programId}":                          {OperationID: "GetProgram", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Recordings":                                    {OperationID: "GetRecordings", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Recordings/Folders":                            {OperationID: "GetRecordingFolders", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Recordings/{recordingId}":                      {OperationID: "GetRecording", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/SeriesTimers":                                  {OperationID: "GetSeriesTimers", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/SeriesTimers/{timerId}":                        {OperationID: "GetSeriesTimer", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Timers":                                        {OperationID: "GetTimers", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Timers/Defaults":                               {OperationID: "GetDefaultTimer", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Timers/{timerId}":                              {OperationID: "GetTimer", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/TunerHosts/Types":                              {OperationID: "GetTunerHostTypes", Policy: string(auth.PolicyLiveTVAccess)},
		"GET /LiveTv/Tuners/Discover":                               {OperationID: "DiscoverTuners", Policy: string(auth.PolicyRequiresElevation)},
		"GET /LiveTv/Tuners/Discvover":                              {OperationID: "DiscvoverTuners", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Packages":                                             {OperationID: "GetPackages", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Packages/{name}":                                      {OperationID: "GetPackageInfo", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Plugins":                                              {OperationID: "GetPlugins", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Plugins/{pluginId}/Configuration":                     {OperationID: "GetPluginConfiguration", Policy: string(auth.PolicyRequiresElevation)},
		"GET /Plugins/{pluginId}/{version}/Image":                   {OperationID: "GetPluginImage", Anonymous: true},
		"GET /Repositories":                                         {OperationID: "GetRepositories", Policy: string(auth.PolicyRequiresElevation)},
		"GET /SyncPlay/List":                                        {OperationID: "SyncPlayGetGroups", Policy: string(auth.PolicySyncPlayJoinGroup)},
		"GET /SyncPlay/{id}":                                        {OperationID: "SyncPlayGetGroup", Policy: string(auth.PolicySyncPlayJoinGroup)},
		"GET /web/ConfigurationPage":                                {OperationID: "GetDashboardConfigurationPage", Anonymous: true},
		"GET /web/ConfigurationPages":                               {OperationID: "GetConfigurationPages", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Backup/Create":                                       {OperationID: "CreateBackup", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Backup/Restore":                                      {OperationID: "StartRestoreBackup", Policy: string(auth.PolicyRequiresElevation)},
		"POST /LiveTv/ChannelMappings":                              {OperationID: "SetChannelMapping", Policy: string(auth.PolicyRequiresElevation)},
		"POST /LiveTv/ListingProviders":                             {OperationID: "AddListingProvider", Policy: string(auth.PolicyRequiresElevation)},
		"POST /LiveTv/Programs":                                     {OperationID: "GetPrograms", Policy: string(auth.PolicyLiveTVAccess)},
		"POST /LiveTv/SeriesTimers":                                 {OperationID: "CreateSeriesTimer", Policy: string(auth.PolicyLiveTVManagement)},
		"POST /LiveTv/SeriesTimers/{timerId}":                       {OperationID: "UpdateSeriesTimer", Policy: string(auth.PolicyLiveTVManagement)},
		"POST /LiveTv/Timers":                                       {OperationID: "CreateTimer", Policy: string(auth.PolicyLiveTVManagement)},
		"POST /LiveTv/Timers/{timerId}":                             {OperationID: "UpdateTimer", Policy: string(auth.PolicyLiveTVManagement)},
		"POST /LiveTv/TunerHosts":                                   {OperationID: "AddTunerHost", Policy: string(auth.PolicyRequiresElevation)},
		"POST /LiveTv/Tuners/{tunerId}/Reset":                       {OperationID: "ResetTuner", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Packages/Installed/{name}":                           {OperationID: "InstallPackage", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Plugins/{pluginId}/Configuration":                    {OperationID: "UpdatePluginConfiguration", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Plugins/{pluginId}/Manifest":                         {OperationID: "GetPluginManifest", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Plugins/{pluginId}/{version}/Disable":                {OperationID: "DisablePlugin", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Plugins/{pluginId}/{version}/Enable":                 {OperationID: "EnablePlugin", Policy: string(auth.PolicyRequiresElevation)},
		"POST /Repositories":                                        {OperationID: "SetRepositories", Policy: string(auth.PolicyRequiresElevation)},
		"POST /SyncPlay/Buffering":                                  {OperationID: "SyncPlayBuffering", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/Join":                                       {OperationID: "SyncPlayJoinGroup", Policy: string(auth.PolicySyncPlayJoinGroup)},
		"POST /SyncPlay/Leave":                                      {OperationID: "SyncPlayLeaveGroup", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/MovePlaylistItem":                           {OperationID: "SyncPlayMovePlaylistItem", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/New":                                        {OperationID: "SyncPlayCreateGroup", Policy: string(auth.PolicySyncPlayCreateGroup)},
		"POST /SyncPlay/NextItem":                                   {OperationID: "SyncPlayNextItem", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/Pause":                                      {OperationID: "SyncPlayPause", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/Ping":                                       {OperationID: "SyncPlayPing", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/PreviousItem":                               {OperationID: "SyncPlayPreviousItem", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/Queue":                                      {OperationID: "SyncPlayQueue", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/Ready":                                      {OperationID: "SyncPlayReady", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/RemoveFromPlaylist":                         {OperationID: "SyncPlayRemoveFromPlaylist", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/Seek":                                       {OperationID: "SyncPlaySeek", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/SetIgnoreWait":                              {OperationID: "SyncPlaySetIgnoreWait", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/SetNewQueue":                                {OperationID: "SyncPlaySetNewQueue", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/SetPlaylistItem":                            {OperationID: "SyncPlaySetPlaylistItem", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/SetRepeatMode":                              {OperationID: "SyncPlaySetRepeatMode", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/SetShuffleMode":                             {OperationID: "SyncPlaySetShuffleMode", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/Stop":                                       {OperationID: "SyncPlayStop", Policy: string(auth.PolicySyncPlayHasAccess)},
		"POST /SyncPlay/Unpause":                                    {OperationID: "SyncPlayUnpause", Policy: string(auth.PolicySyncPlayHasAccess)},
	}
	got := make(map[string]router.Route, len(routes))
	for _, route := range routes {
		key := route.Method + " " + route.Pattern
		got[key] = router.Route{OperationID: route.OperationID, Policy: route.Policy, Anonymous: route.Anonymous, PlaySessionCredential: route.PlaySessionCredential, OptionalIdentity: route.OptionalIdentity, Worker: route.Worker, ScalingCredential: route.ScalingCredential}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered route metadata = %#v, want %#v", got, want)
	}
}

func assertHTTPStatus(t *testing.T, handler http.Handler, method, path string, body []byte, want int) {
	t.Helper()

	rec := requestComposition(t, handler, method, path, body)
	if rec.Code != want {
		t.Fatalf("%s %s status = %d, want %d; body: %s", method, path, rec.Code, want, rec.Body.String())
	}
}

func assertStartupServerName(t *testing.T, handler http.Handler, want string) {
	t.Helper()

	rec := requestComposition(t, handler, http.MethodGet, "/Startup/Configuration", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("startup configuration status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		ServerName string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode startup configuration: %v", err)
	}
	if body.ServerName != want {
		t.Fatalf("startup configuration server name = %q, want %q", body.ServerName, want)
	}
}

func assertPublicServerName(t *testing.T, handler http.Handler, want string) {
	t.Helper()

	rec := requestComposition(t, handler, http.MethodGet, "/System/Info/Public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public system info status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		ServerName string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode public system info: %v", err)
	}
	if body.ServerName != want {
		t.Fatalf("public server name = %q, want %q", body.ServerName, want)
	}
}

func composedPublicServerID(t *testing.T, handler http.Handler) string {
	t.Helper()

	rec := requestComposition(t, handler, http.MethodGet, "/System/Info/Public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public system info status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Id string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode public system info: %v; body: %s", err, rec.Body.String())
	}
	return body.Id
}

func updateComposedServerName(t *testing.T, handler http.Handler, token, name string) {
	t.Helper()

	payload, err := json.Marshal(struct {
		ServerName string
	}{ServerName: name})
	if err != nil {
		t.Fatalf("marshal server-name configuration: %v", err)
	}
	rec := authenticatedCompositionRequest(handler, http.MethodPost, "/Startup/Configuration", payload, token)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update server-name configuration status = %d, want %d; body: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
}

func assertSystemInfoServerName(t *testing.T, handler http.Handler, token, want string) {
	t.Helper()

	path := "/System/Info/Public"
	rec := requestComposition(t, handler, http.MethodGet, path, nil)
	if token != "" {
		path = "/System/Info"
		rec = authenticatedCompositionRequest(handler, http.MethodGet, path, nil, token)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d; body: %s", path, rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		ServerName string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode GET %s response: %v; body: %s", path, err, rec.Body.String())
	}
	if body.ServerName != want {
		t.Fatalf("GET %s ServerName = %q, want configured server name %q", path, body.ServerName, want)
	}
}

func assertPublicCompletion(t *testing.T, handler http.Handler, want bool) {
	t.Helper()

	rec := requestComposition(t, handler, http.MethodGet, "/System/Info/Public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public system info status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		StartupWizardCompleted bool
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode public system info: %v", err)
	}
	if body.StartupWizardCompleted != want {
		t.Fatalf("public startup completion = %t, want %t", body.StartupWizardCompleted, want)
	}
}

func assertComposedPublicStartupCompletion(t *testing.T, handler http.Handler, want bool) {
	t.Helper()

	for _, tc := range []struct {
		name   string
		accept string
		field  string
	}{
		{name: "PascalCase", field: "StartupWizardCompleted"},
		{name: "CamelCase", accept: `application/json; profile="CamelCase"`, field: "startupWizardCompleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/System/Info/Public", nil)
			req.Header.Set("Accept", tc.accept)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /System/Info/Public status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}

			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode public system info: %v; body: %s", err, rec.Body.String())
			}
			raw, ok := body[tc.field]
			if !ok {
				t.Fatalf("public system info missing %q: %s", tc.field, rec.Body.String())
			}
			var got bool
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode public startup completion %q: %v; body: %s", tc.field, err, rec.Body.String())
			}
			if got != want {
				t.Fatalf("public startup completion %q = %t, want %t", tc.field, got, want)
			}
		})
	}
}

func requestComposition(t *testing.T, handler http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	// A loopback peer may run the first-run wizard (D-53).
	req.RemoteAddr = "127.0.0.1:8096"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func assertFile(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if info.IsDir() {
		t.Fatalf("%q is a directory, want persistent file", path)
	}
}

func entryNames(entries []fs.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func compositionRequestFrom(handler http.Handler, method, path string, body []byte, remoteAddress, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = remoteAddress
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

// skipIfRoot skips the test when the process runs as root (effective UID 0).
// Root bypasses Unix file-mode permission checks, so a test that depends on a
// read-only directory failing cannot produce a deterministic failure under
// root. The CI jail runs as root.
func skipIfRoot(t *testing.T, reason string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skipf("skipping permission probe under root: %s", reason)
	}
}
