// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"encoding/json"
	"maps"
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
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/events"
)

// Sources: upstream/jellyfin/Jellyfin.Api/Controllers/ConfigurationController.cs
// (UpdateConfiguration :62-68, UpdateNamedConfiguration :92-106,
// UpdateBrandingConfiguration :122-137) and the jellyfin-web dashboard pages
// that call them. Each page reads the current document, changes the members
// it shows, and posts the whole document back:
//   - General (settings): getConfiguration -> ServerName, UICulture,
//     CachePath, MetadataPath, QuickConnectAvailable,
//     LibraryScanFanoutConcurrency, ParallelImageEncodingLimit ->
//     updateConfiguration.
//   - Metadata (libraries-metadata): getConfiguration ->
//     PreferredMetadataLanguage, MetadataCountryCode, DummyChapterDuration,
//     ChapterImageResolution -> updateConfiguration.
//   - Display (libraries-display): updateNamedConfiguration("metadata").
//   - Playback/Transcoding: getNamedConfiguration("encoding") -> form ->
//     updateNamedConfiguration("encoding").
//   - Networking: getNamedConfiguration("network") -> form ->
//     updateNamedConfiguration("network").
//   - Branding: getNamedConfiguration("branding") -> JSON.stringify({...,
//     LoginDisclaimer, CustomCss, SplashscreenEnabled}) ->
//     updateNamedConfiguration("branding").

func adminConfigUser() auth.Identity {
	identity := authenticatedConfigUser()
	identity.Administrator = true
	return identity
}

func newConfigurationWriteRouter(t *testing.T, identity auth.Identity) (*router.Router, *config.Manager, *events.Bus[config.Change]) {
	t.Helper()
	_, manager, changes := newManagedRouter(t)
	evaluator := auth.NewEvaluator(manager, startupUserLocalNetwork{}, startupUserSchedules{}, func() time.Time { return time.Unix(0, 0).UTC() })
	authorizer := middleware.NewAuthorizer(configurationResolver{identity: identity}, evaluator, configurationUsers{})
	rt := router.New(authorizer.Wrap)
	handlers.RegisterConfiguration(rt, manager)
	return rt, manager, changes
}

func subscribeChanges(t *testing.T, changes *events.Bus[config.Change]) *events.Subscription[config.Change] {
	t.Helper()
	subscription := changes.Subscribe(events.SubscriptionOptions{Buffer: 8})
	t.Cleanup(func() {
		if err := subscription.Close(context.Background()); err != nil {
			t.Errorf("close configuration subscription: %v", err)
		}
	})
	return subscription
}

// configurationSections reads every dashboard-visible section through the
// GET routes.
func configurationSections(t *testing.T, rt http.Handler) map[string]map[string]any {
	t.Helper()
	sections := map[string]string{
		"system":   "/System/Configuration",
		"encoding": "/System/Configuration/encoding",
		"network":  "/System/Configuration/network",
		"metadata": "/System/Configuration/metadata",
		"branding": "/System/Configuration/branding",
	}
	result := make(map[string]map[string]any, len(sections))
	for name, path := range sections {
		result[name] = getJSONDocument(t, rt, path)
	}
	return result
}

func getJSONDocument(t *testing.T, rt http.Handler, path string) map[string]any {
	t.Helper()
	rec := request(rt, http.MethodGet, path, "", "", "")
	assertStatus(t, rec, http.StatusOK)
	var document map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatalf("GET %s is not a JSON object: %v; body=%q", path, err, rec.Body.String())
	}
	return document
}

func postJSONDocument(t *testing.T, rt http.Handler, path string, document any) {
	t.Helper()
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal %s body: %v", path, err)
	}
	rec := request(rt, http.MethodPost, path, string(body), "application/json", "")
	assertStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Fatalf("POST %s body = %q, want empty", path, rec.Body.String())
	}
}

// assertOnlySectionChanged proves a save of one dashboard page leaves every
// other section unchanged.
func assertOnlySectionChanged(t *testing.T, before, after map[string]map[string]any, changed string, want map[string]any) {
	t.Helper()
	for name, document := range after {
		if name == changed {
			if !reflect.DeepEqual(document, want) {
				t.Errorf("%s after save = %#v\nwant %#v", name, document, want)
			}
			continue
		}
		if !reflect.DeepEqual(document, before[name]) {
			t.Errorf("%s changed by a %s save:\n got %#v\nwant %#v", name, changed, document, before[name])
		}
	}
}

func TestConfigurationWriteRoutesRequireElevation(t *testing.T) {
	rt, _, _ := newConfigurationWriteRouter(t, adminConfigUser())
	want := map[string]string{
		"POST /System/Configuration":          "UpdateConfiguration",
		"POST /System/Configuration/{key}":    "UpdateNamedConfiguration",
		"POST /System/Configuration/Branding": "UpdateBrandingConfiguration",
	}
	got := map[string]router.Route{}
	for _, route := range rt.Routes() {
		got[route.Method+" "+route.Pattern] = route
	}
	for key, operation := range want {
		route, ok := got[key]
		if !ok {
			t.Errorf("route %s not registered", key)
			continue
		}
		if route.OperationID != operation || route.Policy != string(auth.PolicyRequiresElevation) || route.Anonymous {
			t.Errorf("%s = %+v, want %s with RequiresElevation", key, route, operation)
		}
	}
}

func TestConfigurationWritesRejectNonAdministrators(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity auth.Identity
		status   int
	}{
		{name: "unauthenticated", identity: auth.Identity{}, status: http.StatusUnauthorized},
		{name: "ordinary user", identity: authenticatedConfigUser(), status: http.StatusForbidden},
	} {
		for _, path := range []string{"/System/Configuration", "/System/Configuration/encoding", "/System/Configuration/Branding", "/System/Configuration/branding"} {
			t.Run(tc.name+path, func(t *testing.T) {
				rt, _, changes := newConfigurationWriteRouter(t, tc.identity)
				subscription := subscribeChanges(t, changes)
				rec := request(rt, http.MethodPost, path, `{}`, "application/json", "")
				assertStatus(t, rec, tc.status)
				assertNoChange(t, subscription.Events())
			})
		}
	}
}

func TestConfigurationGeneralPageRoundTripLeavesOtherSectionsUnchanged(t *testing.T) {
	rt, manager, changes := newConfigurationWriteRouter(t, adminConfigUser())
	subscription := subscribeChanges(t, changes)
	before := configurationSections(t, rt)

	document := maps.Clone(before["system"])
	document["ServerName"] = "Living Room"
	document["UICulture"] = "de"
	document["CachePath"] = ""
	document["MetadataPath"] = ""
	document["QuickConnectAvailable"] = false
	document["LibraryScanFanoutConcurrency"] = 4
	document["ParallelImageEncodingLimit"] = 2
	postJSONDocument(t, rt, "/System/Configuration", document)

	assertChange(t, subscription.Events(), config.Change{File: config.SystemFile})
	assertNoChange(t, subscription.Events())
	want := maps.Clone(before["system"])
	want["ServerName"] = "Living Room"
	want["UICulture"] = "de"
	want["QuickConnectAvailable"] = false
	want["LibraryScanFanoutConcurrency"] = float64(4)
	want["ParallelImageEncodingLimit"] = float64(2)
	// An empty CachePath is stored as the empty string; jellyfin-web sends
	// the empty form value and reads it back the same way.
	want["CachePath"] = ""
	assertOnlySectionChanged(t, before, configurationSections(t, rt), "system", want)
	if got := manager.System().ServerName; got != "Living Room" {
		t.Fatalf("System().ServerName = %q, want live value", got)
	}
	assertConfigFileContains(t, manager, config.SystemFile, "<ServerName>Living Room</ServerName>", "<QuickConnectAvailable>false</QuickConnectAvailable>")
}

func TestConfigurationMetadataPageRoundTripLeavesOtherSectionsUnchanged(t *testing.T) {
	rt, manager, changes := newConfigurationWriteRouter(t, adminConfigUser())
	subscription := subscribeChanges(t, changes)
	before := configurationSections(t, rt)

	document := maps.Clone(before["system"])
	document["PreferredMetadataLanguage"] = "de"
	document["MetadataCountryCode"] = "DE"
	document["DummyChapterDuration"] = 300
	document["ChapterImageResolution"] = "P720"
	postJSONDocument(t, rt, "/System/Configuration", document)
	assertChange(t, subscription.Events(), config.Change{File: config.SystemFile})

	want := maps.Clone(before["system"])
	want["PreferredMetadataLanguage"] = "de"
	want["MetadataCountryCode"] = "DE"
	want["DummyChapterDuration"] = float64(300)
	want["ChapterImageResolution"] = "P720"
	assertOnlySectionChanged(t, before, configurationSections(t, rt), "system", want)
	if got := manager.System(); got.PreferredMetadataLanguage != "de" || got.MetadataCountryCode != "DE" {
		t.Fatalf("live metadata defaults = %q/%q, want de/DE", got.PreferredMetadataLanguage, got.MetadataCountryCode)
	}

	// The Display page then saves the metadata named configuration.
	before = configurationSections(t, rt)
	metadata := maps.Clone(before["metadata"])
	metadata["UseFileCreationTimeForDateAdded"] = false
	postJSONDocument(t, rt, "/System/Configuration/metadata", metadata)
	assertChange(t, subscription.Events(), config.Change{File: config.MetadataFile})
	assertNoChange(t, subscription.Events())
	assertOnlySectionChanged(t, before, configurationSections(t, rt), "metadata", metadata)
	assertConfigFileContains(t, manager, config.MetadataFile, "<MetadataConfiguration>", "<UseFileCreationTimeForDateAdded>false</UseFileCreationTimeForDateAdded>")
}

func TestConfigurationTranscodingPageRoundTripLeavesOtherSectionsUnchanged(t *testing.T) {
	rt, manager, changes := newConfigurationWriteRouter(t, adminConfigUser())
	subscription := subscribeChanges(t, changes)
	before := configurationSections(t, rt)

	document := maps.Clone(before["encoding"])
	document["HardwareAccelerationType"] = "qsv"
	document["QsvDevice"] = "/dev/dri/renderD129"
	document["EnableHardwareEncoding"] = true
	document["EnableIntelLowPowerH264HwEncoder"] = true
	document["HardwareDecodingCodecs"] = []any{"h264", "hevc", "av1"}
	document["EnableTonemapping"] = true
	document["TonemappingAlgorithm"] = "hable"
	document["H264Crf"] = 21
	document["AllowOnDemandMetadataBasedKeyframeExtractionForExtensions"] = []any{}
	postJSONDocument(t, rt, "/System/Configuration/encoding", document)

	assertChange(t, subscription.Events(), config.Change{File: config.EncodingFile})
	assertNoChange(t, subscription.Events())
	want := maps.Clone(document)
	want["H264Crf"] = float64(21)
	assertOnlySectionChanged(t, before, configurationSections(t, rt), "encoding", want)
	live := manager.Encoding()
	if live.HardwareAccelerationType != "qsv" || live.QsvDevice != "/dev/dri/renderD129" || !reflect.DeepEqual(live.HardwareDecodingCodecs, []string{"h264", "hevc", "av1"}) {
		t.Fatalf("live encoding options = %+v", live)
	}
	assertConfigFileContains(t, manager, config.EncodingFile,
		"<EncodingOptions>",
		"<HardwareAccelerationType>qsv</HardwareAccelerationType>",
		"<HardwareDecodingCodecs>\n    <string>h264</string>",
		"<AllowOnDemandMetadataBasedKeyframeExtractionForExtensions></AllowOnDemandMetadataBasedKeyframeExtractionForExtensions>",
	)
	reopened := reopenManager(t, manager)
	if got := reopened.Encoding(); !reflect.DeepEqual(got, live) {
		t.Fatalf("encoding after reload = %+v\nwant %+v", got, live)
	}
}

func TestConfigurationNetworkingPageRoundTripLeavesOtherSectionsUnchanged(t *testing.T) {
	rt, manager, changes := newConfigurationWriteRouter(t, adminConfigUser())
	subscription := subscribeChanges(t, changes)
	before := configurationSections(t, rt)
	if got := before["network"]["InternalHttpPort"]; got != float64(8096) {
		t.Fatalf("default network InternalHttpPort = %v, want 8096", got)
	}

	document := maps.Clone(before["network"])
	document["BaseUrl"] = "jellyfin/"
	document["LocalNetworkSubnets"] = []any{"10.10.0.0/16", "!10.10.9.0/24"}
	document["KnownProxies"] = []any{"10.10.0.1"}
	document["EnableIPv6"] = true
	document["VirtualInterfaceNames"] = []any{}
	document["PublishedServerUriBySubnet"] = []any{"all=https://media.example.test"}
	document["RemoteIPFilter"] = []any{"203.0.113.0/24"}
	document["IsRemoteIPFilterBlacklist"] = true
	postJSONDocument(t, rt, "/System/Configuration/network", document)

	assertChange(t, subscription.Events(), config.Change{File: config.NetworkFile})
	assertNoChange(t, subscription.Events())
	want := maps.Clone(document)
	want["BaseUrl"] = "/jellyfin"
	assertOnlySectionChanged(t, before, configurationSections(t, rt), "network", want)
	assertConfigFileContains(t, manager, config.NetworkFile,
		"<BaseUrl>/jellyfin</BaseUrl>",
		"<LocalNetworkSubnets>\n    <string>10.10.0.0/16</string>\n    <string>!10.10.9.0/24</string>\n  </LocalNetworkSubnets>",
		"<VirtualInterfaceNames></VirtualInterfaceNames>",
	)
	reopened := reopenManager(t, manager)
	if got := reopened.Network(); !reflect.DeepEqual(got, manager.Network()) {
		t.Fatalf("network after reload = %+v\nwant %+v", got, manager.Network())
	}
}

func TestConfigurationBrandingPageRoundTripKeepsSplashscreenLocation(t *testing.T) {
	rt, manager, changes := newConfigurationWriteRouter(t, adminConfigUser())
	if err := manager.UpdateBranding(t.Context(), func(branding *config.BrandingOptions) error {
		branding.SplashscreenLocation = "/config/splashscreen-upload.png"
		return nil
	}); err != nil {
		t.Fatalf("seed branding: %v", err)
	}
	subscription := subscribeChanges(t, changes)
	before := configurationSections(t, rt)
	if got := before["branding"]["SplashscreenLocation"]; got != "/config/splashscreen-upload.png" {
		t.Fatalf("GET branding SplashscreenLocation = %v", got)
	}

	// The page spreads the current document; a client-supplied
	// SplashscreenLocation never replaces the stored one.
	document := maps.Clone(before["branding"])
	document["LoginDisclaimer"] = "Family only."
	document["CustomCss"] = "body { color: teal; }"
	document["SplashscreenEnabled"] = true
	document["SplashscreenLocation"] = "/etc/passwd"
	postJSONDocument(t, rt, "/System/Configuration/branding", document)
	assertChange(t, subscription.Events(), config.Change{File: config.BrandingFile})
	assertNoChange(t, subscription.Events())

	want := maps.Clone(document)
	want["SplashscreenLocation"] = "/config/splashscreen-upload.png"
	assertOnlySectionChanged(t, before, configurationSections(t, rt), "branding", want)

	// UpdateBrandingConfiguration with the DTO shape.
	before = configurationSections(t, rt)
	postJSONDocument(t, rt, "/System/Configuration/Branding", map[string]any{"LoginDisclaimer": "Updated.", "SplashscreenEnabled": false})
	assertChange(t, subscription.Events(), config.Change{File: config.BrandingFile})
	want = map[string]any{"LoginDisclaimer": "Updated.", "SplashscreenEnabled": false, "SplashscreenLocation": "/config/splashscreen-upload.png"}
	assertOnlySectionChanged(t, before, configurationSections(t, rt), "branding", want)
	assertConfigFileContains(t, manager, config.BrandingFile, "<BrandingOptions>", "<LoginDisclaimer>Updated.</LoginDisclaimer>", "<SplashscreenLocation>/config/splashscreen-upload.png</SplashscreenLocation>")
}

func TestConfigurationPartialPayloadsKeepUpstreamDefaults(t *testing.T) {
	t.Run("system", func(t *testing.T) {
		rt, manager, _ := newConfigurationWriteRouter(t, adminConfigUser())
		rec := request(rt, http.MethodPost, "/System/Configuration", `{"ServerName":"Den","LibraryMonitorDelay":"45","TrickplayOptions":{"Interval":5000}}`, "application/json", "")
		assertStatus(t, rec, http.StatusNoContent)
		want := config.DefaultServerConfiguration()
		want.ServerName = "Den"
		want.LibraryMonitorDelay = 45
		want.TrickplayOptions.Interval = 5000
		got := manager.System()
		got.CacheSize = want.CacheSize
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("system after partial save = %+v\nwant %+v", got, want)
		}
	})
	t.Run("encoding", func(t *testing.T) {
		rt, manager, _ := newConfigurationWriteRouter(t, adminConfigUser())
		rec := request(rt, http.MethodPost, "/System/Configuration/encoding", `{"HardwareAccelerationType":"vaapi"}`, "application/json", "")
		assertStatus(t, rec, http.StatusNoContent)
		want := config.DefaultEncodingOptions()
		want.HardwareAccelerationType = "vaapi"
		if got := manager.Encoding(); !reflect.DeepEqual(got, want) {
			t.Fatalf("encoding after partial save = %+v\nwant %+v", got, want)
		}
	})
	t.Run("network", func(t *testing.T) {
		rt, manager, _ := newConfigurationWriteRouter(t, adminConfigUser())
		rec := request(rt, http.MethodPost, "/System/Configuration/network", `{"EnableIPv6":true}`, "application/json", "")
		assertStatus(t, rec, http.StatusNoContent)
		want := config.DefaultNetworkConfiguration()
		want.EnableIPv6 = true
		if got := manager.Network(); !reflect.DeepEqual(got, want) {
			t.Fatalf("network after partial save = %+v\nwant %+v", got, want)
		}
	})
	t.Run("metadata", func(t *testing.T) {
		rt, manager, _ := newConfigurationWriteRouter(t, adminConfigUser())
		rec := request(rt, http.MethodPost, "/System/Configuration/metadata", `{}`, "application/json", "")
		assertStatus(t, rec, http.StatusNoContent)
		if got := manager.Metadata(); got != config.DefaultMetadataConfiguration() {
			t.Fatalf("metadata after empty-object save = %+v, want upstream default", got)
		}
	})
}

func TestConfigurationWriteErrorsMatchUpstreamStatus(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name   string
		path   string
		body   string
		status int
	}{
		{name: "missing body", path: "/System/Configuration", body: "", status: http.StatusBadRequest},
		{name: "null body", path: "/System/Configuration/encoding", body: "null", status: http.StatusBadRequest},
		{name: "array body", path: "/System/Configuration/network", body: "[]", status: http.StatusBadRequest},
		{name: "wrong member type", path: "/System/Configuration", body: `{"MinResumePct":"many"}`, status: http.StatusBadRequest},
		{name: "unknown key", path: "/System/Configuration/notakey", body: `{}`, status: http.StatusNotFound},
		{name: "missing metadata path", path: "/System/Configuration", body: `{"MetadataPath":"` + missing + `"}`, status: http.StatusNotFound},
		{name: "missing cache path", path: "/System/Configuration", body: `{"CachePath":"` + missing + `"}`, status: http.StatusNotFound},
		{name: "missing transcode path", path: "/System/Configuration/encoding", body: `{"TranscodingTempPath":"` + missing + `"}`, status: http.StatusNotFound},
		{name: "encoder app path", path: "/System/Configuration/encoding", body: `{"EncoderAppPath":"/usr/bin/evil"}`, status: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, manager, changes := newConfigurationWriteRouter(t, adminConfigUser())
			subscription := subscribeChanges(t, changes)
			before := configurationSections(t, rt)
			rec := request(rt, http.MethodPost, tc.path, tc.body, "application/json", "")
			assertStatus(t, rec, tc.status)
			if tc.status != http.StatusBadRequest && rec.Body.String() != "Error processing request." {
				t.Errorf("body = %q, want upstream production message", rec.Body.String())
			}
			assertNoChange(t, subscription.Events())
			if after := configurationSections(t, rt); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected save changed configuration")
			}
			_ = manager
		})
	}
}

func TestConfigurationAcceptsExistingDirectoryPaths(t *testing.T) {
	rt, manager, _ := newConfigurationWriteRouter(t, adminConfigUser())
	metadataPath := t.TempDir()
	body, _ := json.Marshal(map[string]any{"MetadataPath": metadataPath})
	assertStatus(t, request(rt, http.MethodPost, "/System/Configuration", string(body), "application/json", ""), http.StatusNoContent)
	if got := manager.System().MetadataPath; got != metadataPath {
		t.Fatalf("MetadataPath = %q, want %q", got, metadataPath)
	}
	entries, err := os.ReadDir(metadataPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("write probe left entries %v (err %v)", entries, err)
	}
}

func TestConfigurationNamedKeyIsCaseInsensitive(t *testing.T) {
	rt, manager, _ := newConfigurationWriteRouter(t, adminConfigUser())
	assertStatus(t, request(rt, http.MethodPost, "/System/Configuration/Encoding", `{"H265Crf":30}`, "application/json", ""), http.StatusNoContent)
	if got := manager.Encoding().H265Crf; got != 30 {
		t.Fatalf("H265Crf = %d, want 30", got)
	}
	if got := getJSONDocument(t, rt, "/System/Configuration/ENCODING")["H265Crf"]; got != float64(30) {
		t.Fatalf("GET ENCODING H265Crf = %v, want 30", got)
	}
	if got := getJSONDocument(t, rt, "/System/Configuration/Branding"); got["SplashscreenEnabled"] != false {
		t.Fatalf("GET Branding = %v", got)
	}
}

func assertConfigFileContains(t *testing.T, manager *config.Manager, file string, fragments ...string) {
	t.Helper()
	path := filepath.Join(manager.Paths().Config, file)
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	for _, fragment := range fragments {
		if !strings.Contains(string(source), fragment) {
			t.Errorf("%s does not contain %q:\n%s", file, fragment, source)
		}
	}
}

func reopenManager(t *testing.T, manager *config.Manager) *config.Manager {
	t.Helper()
	changes := events.NewBus[config.Change](clock.NewFake(time.Unix(0, 0).UTC()))
	t.Cleanup(func() { _ = changes.Close(context.Background()) })
	reopened, err := config.Open(t.Context(), manager.Paths(), changes)
	if err != nil {
		t.Fatalf("reopen configuration: %v", err)
	}
	return reopened
}

func TestConfigurationSaveKeepsCompletedStartupWizard(t *testing.T) {
	rt, manager, _ := newConfigurationWriteRouter(t, adminConfigUser())
	seeded := manager.System()
	seeded.IsStartupWizardCompleted = true
	seedSystem(t, manager, seeded)
	for _, body := range []string{`{"ServerName":"Den"}`, `{"IsStartupWizardCompleted":false}`} {
		assertStatus(t, request(rt, http.MethodPost, "/System/Configuration", body, "application/json", ""), http.StatusNoContent)
		if !manager.StartupWizardCompleted() {
			t.Fatalf("save %s reopened the startup wizard", body)
		}
	}
}
