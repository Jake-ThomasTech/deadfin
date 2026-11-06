// SPDX-License-Identifier: GPL-2.0-only

package guide_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

const upstreamData = "testdata/upstream"

// listingServer serves files of testdata/upstream by the last path segment,
// as the mocked HttpMessageHandler of XmlTvListingsProviderTests does.
type listingServer struct {
	*httptest.Server
	requests atomic.Int32
}

func newListingServer(t *testing.T) *listingServer {
	t.Helper()
	s := &listingServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		switch {
		case r.URL.Path == "/redirect":
			http.Redirect(w, r, "/guide.xml.gz?token=1", http.StatusFound)
		case r.URL.Path == "/guide.xml.gz":
			var buffer bytes.Buffer
			zip := gzip.NewWriter(&buffer)
			data, _ := os.ReadFile(filepath.Join(upstreamData, "notitle.xml"))
			_, _ = zip.Write(data)
			_ = zip.Close()
			_, _ = w.Write(buffer.Bytes())
		case r.URL.Path == "/empty.xml":
		default:
			http.ServeFile(w, r, filepath.Join(upstreamData, path.Base(r.URL.Path)))
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func newProvider(t *testing.T) (*guide.XMLTV, *clock.Fake) {
	t.Helper()
	fake := clock.NewFake(time.Date(2022, 11, 4, 0, 0, 0, 0, time.UTC))
	client, err := fetch.New(fetch.Options{HTTP: &http.Client{}, Clock: fake, UserAgent: "Deadfin/test"})
	if err != nil {
		t.Fatal(err)
	}
	return guide.NewXMLTV(client, fake, t.TempDir()), fake
}

func providerInfo(id, location string) config.ListingsProviderInfo {
	info := config.DefaultListingsProviderInfo()
	info.Id = id
	info.Type = "xmltv"
	info.Path = location
	return info
}

func programs(t *testing.T, provider *guide.XMLTV, info config.ListingsProviderInfo, channelID string) []guide.Program {
	t.Helper()
	start := time.Date(2022, 11, 4, 0, 0, 0, 0, time.UTC)
	var result []guide.Program
	err := provider.Programs(context.Background(), info, "en", []string{channelID}, start, start.AddDate(0, 0, 1), func(p guide.Program) error {
		result = append(result, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func single(t *testing.T, provider *guide.XMLTV, info config.ListingsProviderInfo) guide.Program {
	t.Helper()
	result := programs(t, provider, info, "3297")
	if len(result) != 1 {
		t.Fatalf("%s: %d programs, want 1", info.Path, len(result))
	}
	return result[0]
}

func assertEtag(t *testing.T, program guide.Program) {
	t.Helper()
	if !strings.HasPrefix(program.Etag, "xmltv-sha256-v1:") {
		t.Errorf("etag %q", program.Etag)
	}
}

// TestUpstreamNoTitle ports XmlTvListingsProviderTests.GetProgramsAsync_NoTitle_Success.
func TestUpstreamNoTitle(t *testing.T) {
	server := newListingServer(t)
	for _, location := range []string{filepath.Join(upstreamData, "notitle.xml"), server.URL + "/notitle.xml"} {
		provider, _ := newProvider(t)
		program := single(t, provider, providerInfo("notitle", location))
		if program.Name != nil || program.SeriesID != nil || program.EpisodeTitle != nil {
			t.Errorf("%s: name %v series %v episode title %v", location, program.Name, program.SeriesID, program.EpisodeTitle)
		}
		if !program.IsSports || !program.HasImage || !reflect.DeepEqual(program.ImageURL, strptr("https://domain.tld/image.png")) {
			t.Errorf("%s: sports %v image %v %v", location, program.IsSports, program.HasImage, program.ImageURL)
		}
		if program.ChannelID != "3297" {
			t.Errorf("%s: channel %q", location, program.ChannelID)
		}
		assertEtag(t, program)
	}
}

// TestUpstreamEmptyCategories ports
// XmlTvListingsProviderTests.GetProgramsAsync_EmptyCategories_Success.
func TestUpstreamEmptyCategories(t *testing.T) {
	server := newListingServer(t)
	for _, location := range []string{filepath.Join(upstreamData, "emptycategory.xml"), server.URL + "/emptycategory.xml"} {
		provider, _ := newProvider(t)
		program := single(t, provider, providerInfo("emptycategory", location))
		for _, genre := range program.Genres {
			if genre == "" {
				t.Errorf("%s: empty genre in %q", location, program.Genres)
			}
		}
		if program.ChannelID != "3297" {
			t.Errorf("%s: channel %q", location, program.ChannelID)
		}
		assertEtag(t, program)
	}
}

// TestUpstreamEtags ports the three etag tests of XmlTvListingsProviderTests.
func TestUpstreamEtags(t *testing.T) {
	provider, _ := newProvider(t)
	etag := func(name string) string {
		return single(t, provider, providerInfo(name, filepath.Join(upstreamData, name+".xml"))).Etag
	}
	base := etag("etag-base")
	if again := etag("etag-base"); again != base {
		t.Errorf("same content: %s != %s", again, base)
	}
	for _, name := range []string{"etag-title-change", "etag-description-change", "etag-icon-change", "etag-category-change", "etag-progid-change"} {
		if etag(name) == base {
			t.Errorf("%s: etag did not change", name)
		}
	}
	for _, name := range []string{"etag-reordered", "etag-unknown-field"} {
		if got := etag(name); got != base {
			t.Errorf("%s: etag %s, want %s", name, got, base)
		}
	}
}

// TestMapProgramme pins GetProgramInfo (XmlTvListingsProvider.cs:201-279)
// for etag-base.xml. The ids are computed by hand from upstream's formulas.
func TestMapProgramme(t *testing.T) {
	provider, _ := newProvider(t)
	program := single(t, provider, providerInfo("base", filepath.Join(upstreamData, "etag-base.xml")))
	want := guide.Program{
		ID:             "3297_2022-11-04T13:00:00.0000000+00:00",
		ChannelID:      "3297",
		Name:           strptr("Base Program"),
		OfficialRating: strptr("TV-G"),
		Overview:       strptr("Base description."),
		StartDate:      time.Date(2022, 11, 4, 13, 0, 0, 0, time.UTC),
		EndDate:        time.Date(2022, 11, 4, 14, 0, 0, 0, time.UTC),
		Genres:         []string{"series"},
		EpisodeTitle:   strptr("Base Episode"),
		ImageURL:       strptr("https://domain.tld/base.png"),
		HasImage:       true,
		IsSeries:       true,
		SeriesID:       strptr(tuner.DotNetMD5("Base Program")),
		ShowID:         strptr("EP123456789012"),
		SeasonNumber:   intptr(1),
		EpisodeNumber:  intptr(2),
	}
	etag, err := guide.Etag(want)
	if err != nil {
		t.Fatal(err)
	}
	want.Etag = etag
	if !reflect.DeepEqual(program, want) {
		t.Errorf("program\n got %+v\nwant %+v", program, want)
	}
}

func TestMapProgrammeShowID(t *testing.T) {
	info := config.DefaultListingsProviderInfo()
	start := time.Date(2026, 3, 8, 2, 0, 0, 0, time.FixedZone("", -5*3600))
	base := guide.XMLTVProgramme{
		ChannelID: "c",
		StartDate: start,
		EndDate:   start.Add(time.Hour),
		Title:     strptr("Show"),
	}

	t.Run("id keeps the file offset", func(t *testing.T) {
		got := guide.MapProgramme(base, info)
		if got.ID != "c_2026-03-08T02:00:00.0000000-05:00" || !got.StartDate.Equal(start) || got.StartDate.Location() != time.UTC {
			t.Errorf("id %q start %v", got.ID, got.StartDate)
		}
	})
	t.Run("title and episode title", func(t *testing.T) {
		p := base
		p.EpisodeTitle = strptr("Pilot")
		if got := guide.MapProgramme(p, info); *got.ShowID != tuner.DotNetMD5("ShowPilot") {
			t.Errorf("show id %q", *got.ShowID)
		}
	})
	t.Run("numbers are appended (D-74)", func(t *testing.T) {
		p := base
		p.Series, p.Episode = intptr(2), intptr(5)
		p.IsPreviouslyShown = true
		got := guide.MapProgramme(p, info)
		if *got.ShowID != tuner.DotNetMD5("Show-2-5") || *got.SeriesID != tuner.DotNetMD5("Show") {
			t.Errorf("show id %q series id %q", *got.ShowID, *got.SeriesID)
		}
	})
	t.Run("two shows with one episode number differ (D-74)", func(t *testing.T) {
		a, b := base, base
		a.Episode, b.Episode = intptr(5), intptr(5)
		b.Title = strptr("Other")
		if guide.MapProgramme(a, info).ShowID == nil || *guide.MapProgramme(a, info).ShowID == *guide.MapProgramme(b, info).ShowID {
			t.Error("show ids collide")
		}
	})
	t.Run("new episode zero gets start ticks", func(t *testing.T) {
		p := base
		p.Episode = intptr(0)
		got := guide.MapProgramme(p, info)
		// 2026-03-08T07:00:00Z in .NET ticks.
		if want := tuner.DotNetMD5("Show-0") + "639085500000000000"; *got.ShowID != want {
			t.Errorf("show id %q, want %q", *got.ShowID, want)
		}
	})
	t.Run("movie clears series fields", func(t *testing.T) {
		p := base
		p.Episode = intptr(3)
		p.EpisodeTitle = strptr("E")
		p.Categories = []string{"Movie", " "}
		got := guide.MapProgramme(p, info)
		if !got.IsMovie || got.IsSeries || got.EpisodeNumber != nil || got.EpisodeTitle != nil || got.SeriesID == nil {
			t.Errorf("%+v", got)
		}
		if !reflect.DeepEqual(got.Genres, []string{"Movie"}) {
			t.Errorf("genres %q", got.Genres)
		}
	})
	t.Run("repeat and premiere", func(t *testing.T) {
		p := base
		p.IsPreviouslyShown, p.IsNew, p.Premiere = true, true, true
		got := guide.MapProgramme(p, info)
		if got.IsRepeat || !got.IsPremiere {
			t.Errorf("repeat %v premiere %v", got.IsRepeat, got.IsPremiere)
		}
	})
}

func TestChannelsList(t *testing.T) {
	provider, _ := newProvider(t)
	document := filepath.Join(t.TempDir(), "guide.xml")
	if err := os.WriteFile(document, []byte(`<tv>
<channel id="a.fake"><display-name>A</display-name><icon src="https://a/a.png"/></channel>
<channel id="b.fake"><display-name>B</display-name><display-name>7</display-name></channel>
<channel id="a.fake"><display-name>A again</display-name><display-name>2.1</display-name></channel>
</tv>`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := provider.Channels(context.Background(), providerInfo("p", document), "en")
	if err != nil {
		t.Fatal(err)
	}
	want := []guide.Channel{
		{ID: "a.fake", Name: "A", Number: "a.fake", ImageURL: "https://a/a.png"},
		{ID: "b.fake", Name: "B", Number: "7"},
		{ID: "a.fake", Name: "A again", Number: "2.1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("channels\n got %+v\nwant %+v", got, want)
	}
	// A duplicate id: the later channel wins the index (EpgChannelData).
	index := guide.NewChannels(got)
	match, ok := index.Match(nil, tuner.Channel{TunerChannelID: "A.FAKE"})
	if !ok || match.Name != "A again" {
		t.Errorf("match %+v %v", match, ok)
	}
}

func TestCache(t *testing.T) {
	server := newListingServer(t)
	provider, fake := newProvider(t)
	info := providerInfo("cached", server.URL+"/notitle.xml")
	ctx := context.Background()

	first, err := provider.CachePath(ctx, info)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CachePath(ctx, info); err != nil {
		t.Fatal(err)
	}
	fake.Advance(guide.XMLTVCacheMaxAge - time.Second)
	if _, err := provider.CachePath(ctx, info); err != nil {
		t.Fatal(err)
	}
	if n := server.requests.Load(); n != 1 {
		t.Errorf("%d requests inside the cache age, want 1", n)
	}
	fake.Advance(2 * time.Second)
	second, err := provider.CachePath(ctx, info)
	if err != nil {
		t.Fatal(err)
	}
	if n := server.requests.Load(); n != 2 {
		t.Errorf("%d requests after the cache age, want 2", n)
	}
	if first != second || filepath.Base(first) != "cached.xml" || filepath.Base(filepath.Dir(first)) != "xmltv" {
		t.Errorf("cache path %s then %s", first, second)
	}
	entries, err := os.ReadDir(filepath.Dir(first))
	if err != nil || len(entries) != 1 {
		t.Errorf("cache directory %v %v, want one file", entries, err)
	}
}

func TestCacheGzipByFinalURL(t *testing.T) {
	server := newListingServer(t)
	provider, _ := newProvider(t)
	program := single(t, provider, providerInfo("gz", server.URL+"/redirect"))
	if !reflect.DeepEqual(program.ImageURL, strptr("https://domain.tld/image.png")) {
		t.Errorf("image %v", program.ImageURL)
	}
}

func TestCacheErrors(t *testing.T) {
	server := newListingServer(t)
	provider, _ := newProvider(t)
	ctx := context.Background()

	if _, err := provider.CachePath(ctx, providerInfo("empty", server.URL+"/empty.xml")); !errors.Is(err, guide.ErrEmptyXMLTV) {
		t.Errorf("empty download: %v", err)
	}
	if _, err := provider.CachePath(ctx, providerInfo("missing", server.URL+"/missing.xml")); err == nil {
		t.Error("404 download: no error")
	}
	for _, id := range []string{"", "..", "../x", "a/b"} {
		if _, err := provider.CachePath(ctx, providerInfo(id, filepath.Join(upstreamData, "notitle.xml"))); err == nil {
			t.Errorf("id %q: no error", id)
		}
	}
	secretURL := server.URL + "/xmltv.php?username=u&password=SECRETpw-91"
	if _, err := provider.CachePath(ctx, providerInfo("secret", secretURL)); err == nil || strings.Contains(err.Error(), "SECRETpw-91") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestValidate(t *testing.T) {
	provider, _ := newProvider(t)
	if err := provider.Validate(providerInfo("v", "https://example.invalid/guide.xml")); err != nil {
		t.Errorf("url: %v", err)
	}
	if err := provider.Validate(providerInfo("v", filepath.Join(upstreamData, "notitle.xml"))); err != nil {
		t.Errorf("existing file: %v", err)
	}
	if err := provider.Validate(providerInfo("v", filepath.Join(t.TempDir(), "missing.xml"))); err == nil {
		t.Error("missing file: no error")
	}
}

func TestLanguage(t *testing.T) {
	info := config.DefaultListingsProviderInfo()
	if got := guide.Language(info, "en"); got != "en" {
		t.Errorf("fallback %q", got)
	}
	info.PreferredLanguage = "de"
	if got := guide.Language(info, "en"); got != "de" {
		t.Errorf("preferred %q", got)
	}
}
