// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
)

// configurationResolver returns a fixed identity for test auth.
type configurationResolver struct{ identity auth.Identity }

func (r configurationResolver) Resolve(_ context.Context, _ *http.Request) (auth.Identity, error) {
	return r.identity, nil
}

// configurationUsers satisfies the middleware Users dependency for tests.
type configurationUsers struct{}

func (configurationUsers) Get(_ context.Context, id dto.UUID) (store.User, error) {
	return store.User{ID: id, Policy: []byte(`{}`)}, nil
}

func authenticatedConfigUser() auth.Identity {
	id, _ := dto.ParseUUID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	return auth.Identity{Authenticated: true, UserID: id}
}

// newConfigurationRouter builds a router with the auth middleware so that
// PolicyDefaultAuthorization routes return 401 for unauthenticated callers and
// 200 for authenticated ordinary users.
func newConfigurationRouter(t *testing.T, identity auth.Identity) (*router.Router, *config.Manager) {
	t.Helper()
	_, manager, _ := newManagedRouter(t)
	evaluator := auth.NewEvaluator(manager, startupUserLocalNetwork{}, startupUserSchedules{}, func() time.Time { return time.Unix(0, 0).UTC() })
	authorizer := middleware.NewAuthorizer(configurationResolver{identity: identity}, evaluator, configurationUsers{})
	rt := router.New(authorizer.Wrap)
	handlers.RegisterConfiguration(rt, manager)
	return rt, manager
}

func TestConfigurationRoutesRegisteredWithDefaultAuthorizationPolicy(t *testing.T) {
	rt, _ := newConfigurationRouter(t, authenticatedConfigUser())
	want := map[string]router.Route{
		"GET /System/Configuration":       {OperationID: "GetConfiguration", Policy: string(auth.PolicyDefaultAuthorization)},
		"GET /System/Configuration/{key}": {OperationID: "GetNamedConfiguration", Policy: string(auth.PolicyDefaultAuthorization)},
	}
	got := make(map[string]router.Route)
	for _, route := range rt.Routes() {
		got[route.Method+" "+route.Pattern] = router.Route{OperationID: route.OperationID, Policy: route.Policy}
	}
	for key, wantRoute := range want {
		gotRoute, ok := got[key]
		if !ok {
			t.Errorf("route %s not registered", key)
			continue
		}
		if gotRoute.OperationID != wantRoute.OperationID {
			t.Errorf("%s OperationID = %q, want %q", key, gotRoute.OperationID, wantRoute.OperationID)
		}
		if gotRoute.Policy != wantRoute.Policy {
			t.Errorf("%s Policy = %q, want %q", key, gotRoute.Policy, wantRoute.Policy)
		}
		if route, exists := got[key]; exists && route.Anonymous {
			t.Errorf("%s must not be anonymous", key)
		}
	}
}

func TestConfigurationGetConfigurationReturns401ForUnauthenticated(t *testing.T) {
	rt, _ := newConfigurationRouter(t, auth.Identity{})
	rec := request(rt, http.MethodGet, "/System/Configuration", "", "", "")
	assertStatus(t, rec, http.StatusUnauthorized)
}

func TestConfigurationGetNamedConfigurationMetadataReturns401ForUnauthenticated(t *testing.T) {
	rt, _ := newConfigurationRouter(t, auth.Identity{})
	rec := request(rt, http.MethodGet, "/System/Configuration/metadata", "", "", "")
	assertStatus(t, rec, http.StatusUnauthorized)
}

func TestConfigurationGetConfigurationReturns200ForAuthenticatedUser(t *testing.T) {
	rt, _ := newConfigurationRouter(t, authenticatedConfigUser())
	rec := request(rt, http.MethodGet, "/System/Configuration", "", "", "")
	assertStatus(t, rec, http.StatusOK)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v; body=%q", err, rec.Body.String())
	}
	if len(got) == 0 {
		t.Fatalf("response body is empty")
	}
}

func TestConfigurationGetNamedConfigurationMetadataReturns200ForAuthenticatedUser(t *testing.T) {
	rt, _ := newConfigurationRouter(t, authenticatedConfigUser())
	rec := request(rt, http.MethodGet, "/System/Configuration/metadata", "", "", "")
	assertStatus(t, rec, http.StatusOK)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v; body=%q", err, rec.Body.String())
	}
	if got["UseFileCreationTimeForDateAdded"] != true {
		t.Fatalf("UseFileCreationTimeForDateAdded = %v, want true", got["UseFileCreationTimeForDateAdded"])
	}
}

func TestConfigurationMetadataModelMatchesUpstreamDefault(t *testing.T) {
	rt, _ := newConfigurationRouter(t, authenticatedConfigUser())
	rec := request(rt, http.MethodGet, "/System/Configuration/metadata", "", "", "")
	assertStatus(t, rec, http.StatusOK)
	want := map[string]any{"UseFileCreationTimeForDateAdded": true}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata response = %#v, want %#v", got, want)
	}
}

func TestConfigurationEncodingModelMatchesUpstreamDefault(t *testing.T) {
	rt, _ := newConfigurationRouter(t, authenticatedConfigUser())
	rec := request(rt, http.MethodGet, "/System/Configuration/encoding", "", "", "")
	assertStatus(t, rec, http.StatusOK)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]any{
		"HardwareAccelerationType": "none",
		"EnableHardwareEncoding":   true,
		"EnableTonemapping":        false,
		"EnableVppTonemapping":     false,
		"TonemappingAlgorithm":     "bt2390",
		"TonemappingPeak":          float64(100),
		"VaapiDevice":              "/dev/dri/renderD128",
		"HardwareDecodingCodecs":   []any{"h264", "vc1"},
	} {
		if !reflect.DeepEqual(got[key], want) {
			t.Errorf("%s = %#v, want %#v", key, got[key], want)
		}
	}
}

func TestConfigurationUnsupportedNamedKeyReturns404(t *testing.T) {
	rt, _ := newConfigurationRouter(t, authenticatedConfigUser())
	rec := request(rt, http.MethodGet, "/System/Configuration/unknown", "", "", "")
	assertStatus(t, rec, http.StatusNotFound)
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json; charset=utf-8")
	}
	var problem map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("unmarshal problem: %v; body=%q", err, rec.Body.String())
	}
	if problem["title"] != "Not Found" {
		t.Errorf("problem title = %v, want %q", problem["title"], "Not Found")
	}
	if problem["status"] != float64(http.StatusNotFound) {
		t.Errorf("problem status = %v, want %d", problem["status"], http.StatusNotFound)
	}
	if problem["detail"] != "Named configuration not found." {
		t.Errorf("problem detail = %v, want %q", problem["detail"], "Named configuration not found.")
	}
}

func TestConfigurationGetConfigurationMatchesStockFixture(t *testing.T) {
	rt, manager := newConfigurationRouter(t, authenticatedConfigUser())

	// Seed a deterministic configuration matching the stock fixture. CacheSize
	// is machine-dependent upstream (Environment.ProcessorCount * 100), so the
	// test fixes it to 1600. The four empty-array fields (ContentTypes,
	// CodecsUsed, PluginRepositories, PathSubstitutions) are stock-compatible
	// empty slices in DefaultServerConfiguration, so a fresh manager emits []
	// for these without test-side repair.
	seeded := config.DefaultServerConfiguration()
	seeded.CacheSize = 1600
	if err := manager.UpdateSystem(context.Background(), func(system *config.ServerConfiguration) error {
		*system = seeded
		return nil
	}); err != nil {
		t.Fatalf("seed system: %v", err)
	}

	rec := request(rt, http.MethodGet, "/System/Configuration", "", "", "")
	assertStatus(t, rec, http.StatusOK)

	stock := loadConfigurationStockFixture(t, "GetConfiguration")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal deadfin response: %v; body=%q", err, rec.Body.String())
	}

	if !reflect.DeepEqual(stock, got) {
		t.Fatalf("GetConfiguration mismatch:\nstock=%#v\ngot=%#v", stock, got)
	}
}

// TestConfigurationGetConfigurationEmitsEmptyArraysNotNull proves that a fresh
// manager response emits [] for the four top-level array fields that stock
// Jellyfin initializes with Array.Empty<T>(): ContentTypes, CodecsUsed,
// PluginRepositories, and PathSubstitutions. The test uses a fresh manager
// with no seeding so the assertion covers the production default path.
func TestConfigurationGetConfigurationEmitsEmptyArraysNotNull(t *testing.T) {
	rt, _ := newConfigurationRouter(t, authenticatedConfigUser())
	rec := request(rt, http.MethodGet, "/System/Configuration", "", "", "")
	assertStatus(t, rec, http.StatusOK)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal deadfin response: %v; body=%q", err, rec.Body.String())
	}

	for _, field := range []string{"ContentTypes", "CodecsUsed", "PluginRepositories", "PathSubstitutions"} {
		value, ok := got[field]
		if !ok {
			t.Errorf("%s missing from response", field)
			continue
		}
		arr, isArr := value.([]any)
		if !isArr {
			t.Errorf("%s = %v (%T), want a JSON array []; a nil slice would serialize as null", field, value, value)
			continue
		}
		if len(arr) != 0 {
			t.Errorf("%s has %d elements, want 0", field, len(arr))
		}
	}
}

func TestConfigurationGetNamedConfigurationMetadataMatchesStockFixture(t *testing.T) {
	rt, _ := newConfigurationRouter(t, authenticatedConfigUser())
	rec := request(rt, http.MethodGet, "/System/Configuration/metadata", "", "", "")
	assertStatus(t, rec, http.StatusOK)

	stock := loadConfigurationStockFixture(t, "GetNamedConfiguration_metadata")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal deadfin response: %v; body=%q", err, rec.Body.String())
	}
	if !reflect.DeepEqual(stock, got) {
		t.Fatalf("metadata mismatch:\nstock=%#v\ngot=%#v", stock, got)
	}
}

// loadConfigurationStockFixture reads the checked-in stock fixture and returns
// the sub-document for the named key.
func loadConfigurationStockFixture(t *testing.T, key string) map[string]any {
	t.Helper()
	path := filepath.Join("testdata", "configuration-stock.json")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stock fixture %s: %v", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(source, &doc); err != nil {
		t.Fatalf("unmarshal stock fixture: %v", err)
	}
	raw, ok := doc[key]
	if !ok {
		t.Fatalf("stock fixture missing key %q", key)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal stock %s: %v", key, err)
	}
	return result
}

// livetvMarker is a synthetic panel password; no non-admin response may
// carry it (D-69).
const livetvMarker = "MARKERpw-7b1e"

func saveLiveTvWithCredentials(t *testing.T, manager *config.Manager) {
	t.Helper()
	err := manager.UpdateLiveTv(t.Context(), func(options *config.LiveTvOptions) error {
		host := config.DefaultTunerHostInfo()
		host.Id, host.Type = "0123456789abcdef0123456789abcdef", "m3u"
		host.Url = "http://panel.test/get.php?username=alice&password=" + livetvMarker + "&type=m3u_plus"
		provider := config.DefaultListingsProviderInfo()
		provider.Id, provider.Type = "fedcba9876543210fedcba9876543210", "xmltv"
		provider.Path = "http://panel.test/xmltv.php?username=alice&password=" + livetvMarker
		provider.Username, provider.Password = "alice", livetvMarker
		options.TunerHosts = []config.TunerHostInfo{host}
		options.ListingProviders = []config.ListingsProviderInfo{provider}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateLiveTv: %v", err)
	}
}

func TestConfigurationGetNamedConfigurationLiveTvRedactsCredentialsForUsers(t *testing.T) {
	rt, manager := newConfigurationRouter(t, authenticatedConfigUser())
	saveLiveTvWithCredentials(t, manager)
	rec := request(rt, http.MethodGet, "/System/Configuration/livetv", "", "", "")
	assertStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	if strings.Contains(body, livetvMarker) || strings.Contains(body, "alice") {
		t.Fatalf("non-admin livetv configuration leaks a credential: %s", body)
	}
	if !strings.Contains(body, "panel.test/get.php?username=***") {
		t.Fatalf("tuner URL missing or not masked: %s", body)
	}
}

func TestConfigurationGetNamedConfigurationLiveTvKeepsCredentialsForAdministrators(t *testing.T) {
	admin := authenticatedConfigUser()
	admin.Administrator = true
	rt, manager := newConfigurationRouter(t, admin)
	saveLiveTvWithCredentials(t, manager)
	rec := request(rt, http.MethodGet, "/System/Configuration/livetv", "", "", "")
	assertStatus(t, rec, http.StatusOK)
	var got config.LiveTvOptions
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ListingProviders[0].Password != livetvMarker || !strings.Contains(got.TunerHosts[0].Url, livetvMarker) {
		t.Fatalf("admin configuration was masked: %s", rec.Body.String())
	}
}

func TestConfigurationGetNamedConfigurationLiveTvDefaults(t *testing.T) {
	rt, _ := newConfigurationRouter(t, authenticatedConfigUser())
	rec := request(rt, http.MethodGet, "/System/Configuration/livetv", "", "", "")
	assertStatus(t, rec, http.StatusOK)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"EnableRecordingSubfolders":                false,
		"EnableOriginalAudioWithEncodedRecordings": false,
		"TunerHosts":                      []any{},
		"ListingProviders":                []any{},
		"PrePaddingSeconds":               float64(0),
		"PostPaddingSeconds":              float64(0),
		"MediaLocationsCreated":           []any{},
		"RecordingPostProcessorArguments": `"{path}"`,
		"SaveRecordingNFO":                true,
		"SaveRecordingImages":             true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("livetv defaults = %#v, want %#v", got, want)
	}
}
