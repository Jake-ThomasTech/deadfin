// SPDX-License-Identifier: GPL-2.0-only

package config_test

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/config"
)

// upstreamLiveTvXML is the shape XmlSerializer writes for LiveTvOptions
// (MediaBrowser.Model/LiveTv/LiveTvOptions.cs, TunerHostInfo.cs,
// ListingsProviderInfo.cs). Omitted members must keep their defaults.
const upstreamLiveTvXML = `<?xml version="1.0" encoding="utf-8"?>
<LiveTvOptions xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <GuideDays xsi:nil="true" />
  <EnableRecordingSubfolders>false</EnableRecordingSubfolders>
  <TunerHosts>
    <TunerHostInfo>
      <Id>0123456789abcdef0123456789abcdef</Id>
      <Url>http://127.0.0.1:5004/playlist.m3u</Url>
      <Type>m3u</Type>
      <FriendlyName>fake</FriendlyName>
      <AllowStreamSharing>false</AllowStreamSharing>
      <TunerCount>2</TunerCount>
    </TunerHostInfo>
  </TunerHosts>
  <ListingProviders>
    <ListingsProviderInfo>
      <Id>fedcba9876543210fedcba9876543210</Id>
      <Type>xmltv</Type>
      <Path>http://127.0.0.1:5004/xmltv.xml</Path>
      <EnabledTuners />
      <EnableAllTuners>true</EnableAllTuners>
      <MovieCategories>
        <string>movie</string>
        <string>film</string>
      </MovieCategories>
      <ChannelMappings>
        <NameValuePair>
          <Name>4.1</Name>
          <Value>wxyz.us</Value>
        </NameValuePair>
      </ChannelMappings>
    </ListingsProviderInfo>
  </ListingProviders>
  <PrePaddingSeconds>60</PrePaddingSeconds>
  <MediaLocationsCreated />
  <SaveRecordingNFO>true</SaveRecordingNFO>
</LiveTvOptions>
`

func TestManagerLoadsJellyfinLiveTvXML(t *testing.T) {
	paths := mustPaths(t)
	if err := os.MkdirAll(paths.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Config, config.LiveTvFile), []byte(upstreamLiveTvXML), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := openManager(t, paths)
	got := manager.LiveTv()

	want := config.DefaultLiveTvOptions()
	want.PrePaddingSeconds = 60
	host := config.DefaultTunerHostInfo()
	host.Id = "0123456789abcdef0123456789abcdef"
	host.Url = "http://127.0.0.1:5004/playlist.m3u"
	host.Type = "m3u"
	host.FriendlyName = "fake"
	host.AllowStreamSharing = false
	host.TunerCount = 2
	want.TunerHosts = []config.TunerHostInfo{host}
	provider := config.DefaultListingsProviderInfo()
	provider.Id = "fedcba9876543210fedcba9876543210"
	provider.Type = "xmltv"
	provider.Path = "http://127.0.0.1:5004/xmltv.xml"
	provider.MovieCategories = []string{"movie", "film"}
	provider.ChannelMappings = []config.NameValuePair{{Name: "4.1", Value: "wxyz.us"}}
	want.ListingProviders = []config.ListingsProviderInfo{provider}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LiveTv() =\n%#v\nwant\n%#v", got, want)
	}
}

func TestManagerMissingLiveTvXMLUsesDefaultsWithoutCreatingFile(t *testing.T) {
	paths := mustPaths(t)
	if err := os.MkdirAll(paths.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := openManager(t, paths)
	if got := manager.LiveTv(); !reflect.DeepEqual(got, config.DefaultLiveTvOptions()) {
		t.Fatalf("LiveTv() = %#v", got)
	}
	if _, err := os.Stat(filepath.Join(paths.Config, config.LiveTvFile)); !os.IsNotExist(err) {
		t.Fatalf("livetv.xml was created: %v", err)
	}
}

func TestLiveTvOptionsXMLRoundTrip(t *testing.T) {
	options := config.DefaultLiveTvOptions()
	days := 14
	options.GuideDays = &days
	options.TunerHosts = []config.TunerHostInfo{config.DefaultTunerHostInfo()}
	options.ListingProviders = []config.ListingsProviderInfo{config.DefaultListingsProviderInfo()}
	encoded, err := xml.Marshal(options)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, fragment := range []string{
		"<GuideDays>14</GuideDays>",
		"<TunerHosts><TunerHostInfo>",
		"<ListingProviders><ListingsProviderInfo>",
		"<MovieCategories><string>movie</string></MovieCategories>",
	} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("encoded document lacks %q: %s", fragment, encoded)
		}
	}
	if strings.Contains(string(encoded), "<Url>") {
		t.Fatalf("an empty string was written as an element: %s", encoded)
	}
	decoded := config.DefaultLiveTvOptions()
	if err := xml.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, options) {
		t.Fatalf("round trip =\n%#v\nwant\n%#v", decoded, options)
	}
}

// System.Text.Json constructs each array element, so a posted tuner or
// provider keeps the constructor defaults of the members it omits.
func TestLiveTvOptionsJSONElementsDecodeOverDefaults(t *testing.T) {
	var options config.LiveTvOptions
	body := `{"GuideDays":3,"TunerHosts":[{"Type":"m3u","Url":"http://x.test/a.m3u"}],"ListingProviders":[{"Type":"xmltv","Path":"/tmp/g.xml"}]}`
	if err := json.Unmarshal([]byte(body), &options); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if options.GuideDays == nil || *options.GuideDays != 3 {
		t.Fatalf("GuideDays = %v", options.GuideDays)
	}
	host := options.TunerHosts[0]
	if !host.AllowHWTranscoding || !host.AllowStreamSharing || !host.IgnoreDts || host.FallbackMaxStreamingBitrate != 30000000 {
		t.Fatalf("tuner lost its defaults: %#v", host)
	}
	provider := options.ListingProviders[0]
	if !provider.EnableAllTuners || len(provider.KidsCategories) != 5 || provider.ChannelMappings == nil {
		t.Fatalf("provider lost its defaults: %#v", provider)
	}
	if !options.SaveRecordingNFO || options.RecordingPostProcessorArguments != `"{path}"` {
		t.Fatalf("options lost their defaults: %#v", options)
	}
}

func TestSaveNamedConfigurationLiveTvPersists(t *testing.T) {
	paths := mustPaths(t)
	if err := os.MkdirAll(paths.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := openManager(t, paths)
	body := []byte(`{"TunerHosts":[{"Id":"0123456789abcdef0123456789abcdef","Type":"m3u","Url":"http://x.test/a.m3u"}]}`)
	if err := manager.SaveNamedConfiguration(t.Context(), "LiveTV", body); err != nil {
		t.Fatalf("SaveNamedConfiguration: %v", err)
	}
	reopened := openManager(t, paths)
	if got := reopened.LiveTv().TunerHosts; len(got) != 1 || got[0].Url != "http://x.test/a.m3u" || !got[0].AllowStreamSharing {
		t.Fatalf("persisted tuners = %#v", got)
	}
}

func TestLiveTvOptionsRedactedMasksCredentials(t *testing.T) {
	const marker = "MARKERpw-7b1e"
	options := config.DefaultLiveTvOptions()
	host := config.DefaultTunerHostInfo()
	host.Url = "http://alice:" + marker + "@panel.test/get.php?username=alice&password=" + marker
	options.TunerHosts = []config.TunerHostInfo{host}
	provider := config.DefaultListingsProviderInfo()
	provider.Path = "http://panel.test/xmltv.php?username=alice&password=" + marker
	provider.Username, provider.Password = "alice", marker
	options.ListingProviders = []config.ListingsProviderInfo{provider}

	redacted := options.Redacted()
	encoded, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), marker) || strings.Contains(string(encoded), "alice") {
		t.Fatalf("Redacted() leaks a credential: %s", encoded)
	}
	if options.ListingProviders[0].Password != marker {
		t.Fatal("Redacted() changed the receiver")
	}
}

func openManager(t *testing.T, paths config.Paths) *config.Manager {
	t.Helper()
	manager, err := config.Open(t.Context(), paths, newChangeBus(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return manager
}

// The Deadfin-only tuner fields (D-78, D-91, D-93) round-trip through
// livetv.xml and survive a named-configuration save from a stock client
// that omits them.
func TestLiveTvDeadfinTunerFieldsKeptAcrossStockSave(t *testing.T) {
	paths := mustPaths(t)
	if err := os.MkdirAll(paths.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := openManager(t, paths)
	body := []byte(`{"TunerHosts":[{"Id":"0123456789abcdef0123456789abcdef","Type":"m3u","Url":"http://x.test/a.m3u","SkipVodEntries":true,"PlaylistRefreshMinutes":60,"StreamOpenIntervalMs":0,"IncludeGroups":[],"ExcludeGroups":["US | Adult","XXX"]}]}`)
	if err := manager.SaveNamedConfiguration(t.Context(), "livetv", body); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(paths.Config, config.LiveTvFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "<SkipVodEntries>true</SkipVodEntries>") || !strings.Contains(string(encoded), "<PlaylistRefreshMinutes>60</PlaylistRefreshMinutes>") {
		t.Fatalf("livetv.xml lacks the Deadfin fields: %s", encoded)
	}
	// A restart reads every field back from livetv.xml.
	loaded := openManager(t, paths).LiveTv().TunerHosts[0]
	if loaded.SkipVodEntries == nil || !*loaded.SkipVodEntries || loaded.PlaylistRefreshMinutes == nil || *loaded.PlaylistRefreshMinutes != 60 ||
		loaded.StreamOpenIntervalMs == nil || *loaded.StreamOpenIntervalMs != 0 ||
		loaded.IncludeGroups == nil || len(loaded.IncludeGroups) != 0 ||
		!slices.Equal(loaded.ExcludeGroups, []string{"US | Adult", "XXX"}) {
		t.Fatalf("reload lost Deadfin fields: %#v", loaded)
	}
	stock := []byte(`{"GuideDays":3,"TunerHosts":[{"Id":"0123456789ABCDEF0123456789ABCDEF","Type":"m3u","Url":"http://x.test/b.m3u"}]}`)
	if err := manager.SaveNamedConfiguration(t.Context(), "livetv", stock); err != nil {
		t.Fatal(err)
	}
	host := openManager(t, paths).LiveTv().TunerHosts[0]
	if host.Url != "http://x.test/b.m3u" || host.SkipVodEntries == nil || !*host.SkipVodEntries || host.PlaylistRefreshMinutes == nil || *host.PlaylistRefreshMinutes != 60 ||
		host.StreamOpenIntervalMs == nil || *host.StreamOpenIntervalMs != 0 || host.IncludeGroups == nil ||
		!slices.Equal(host.ExcludeGroups, []string{"US | Adult", "XXX"}) {
		t.Fatalf("stock save lost the Deadfin fields: %#v", host)
	}
	// A plain upstream document has no Deadfin elements.
	plain, err := xml.Marshal(config.DefaultTunerHostInfo())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "SkipVodEntries") || strings.Contains(string(plain), "PlaylistRefreshMinutes") ||
		strings.Contains(string(plain), "StreamOpenIntervalMs") || strings.Contains(string(plain), "Groups") {
		t.Fatalf("unset Deadfin fields were written: %s", plain)
	}
}

// TestLiveTvListingProviderDescriptionEpisodesKept covers D-123: livetv.xml
// keeps ParseDescriptionEpisodes, and a stock save that omits it keeps it.
func TestLiveTvListingProviderDescriptionEpisodesKept(t *testing.T) {
	paths := mustPaths(t)
	if err := os.MkdirAll(paths.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := openManager(t, paths)
	if !config.DefaultListingsProviderInfo().DescriptionEpisodes() {
		t.Fatal("DescriptionEpisodes is off by default, want on")
	}
	body := []byte(`{"ListingProviders":[{"Id":"0123456789abcdef0123456789abcdef","Type":"xmltv","Path":"http://x.test/a.xml","ParseDescriptionEpisodes":false}]}`)
	if err := manager.SaveNamedConfiguration(t.Context(), "livetv", body); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(paths.Config, config.LiveTvFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "<ParseDescriptionEpisodes>false</ParseDescriptionEpisodes>") {
		t.Fatalf("livetv.xml lacks ParseDescriptionEpisodes: %s", encoded)
	}
	if loaded := openManager(t, paths).LiveTv().ListingProviders[0]; loaded.DescriptionEpisodes() {
		t.Fatalf("reload lost ParseDescriptionEpisodes: %#v", loaded)
	}
	stock := []byte(`{"ListingProviders":[{"Id":"0123456789ABCDEF0123456789ABCDEF","Type":"xmltv","Path":"http://x.test/b.xml"}]}`)
	if err := manager.SaveNamedConfiguration(t.Context(), "livetv", stock); err != nil {
		t.Fatal(err)
	}
	if provider := openManager(t, paths).LiveTv().ListingProviders[0]; provider.Path != "http://x.test/b.xml" || provider.DescriptionEpisodes() {
		t.Fatalf("stock save lost ParseDescriptionEpisodes: %#v", provider)
	}
	plain, err := xml.Marshal(config.DefaultListingsProviderInfo())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "ParseDescriptionEpisodes") {
		t.Fatalf("unset ParseDescriptionEpisodes was written: %s", plain)
	}
}
