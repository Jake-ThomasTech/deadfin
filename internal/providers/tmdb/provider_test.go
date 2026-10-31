// SPDX-License-Identifier: GPL-2.0-only

package tmdb_test

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/remote"
	"github.com/deadfin-org/deadfin/internal/providers/remote/remotetest"
	"github.com/deadfin-org/deadfin/internal/providers/tmdb"
)

func newCassetteProvider(t *testing.T, cassettePath string) *tmdb.Provider {
	t.Helper()
	cassette, err := remotetest.Load(cassettePath)
	if err != nil {
		t.Fatalf("load cassette: %v", err)
	}
	config := tmdb.DefaultConfig()
	config.APIKey = "test-key"
	return tmdb.NewProvider(tmdb.Options{
		HTTP:   remote.NewClient(remote.Config{Doer: cassette, CacheEntries: 32}),
		Config: config,
	})
}

func movieRefreshContext() library.RefreshContext {
	return library.RefreshContext{
		Options: library.LibraryOptions{
			EnableInternetProviders:   true,
			PreferredMetadataLanguage: "en",
			MetadataCountryCode:       "US",
		},
	}
}

func stringPtr(value string) *string { return &value }

// TestProviderMapsMovieMetadataFromCassette exercises the full ported movie
// path — name+year search, movie fetch, and field mapping — against a recorded
// TMDb cassette (no live API).
func TestProviderMapsMovieMetadataFromCassette(t *testing.T) {
	provider := newCassetteProvider(t, "testdata/the-matrix.json")
	target := item.Item{Kind: item.KindMovie, Name: stringPtr("The Matrix"), ProductionYear: intPtr(1999)}

	handled, err := provider.Refresh(t.Context(), movieRefreshContext(), &target)
	if err != nil {
		t.Fatalf("refresh movie: %v", err)
	}
	if !handled {
		t.Fatal("provider did not claim the movie despite a matching cassette")
	}

	if target.Name == nil || *target.Name != "The Matrix" {
		t.Errorf("Name = %v, want The Matrix", target.Name)
	}
	if target.Overview == nil || *target.Overview != "A computer hacker learns from mysterious rebels.\nHe joins the fight." {
		t.Errorf("Overview = %v, want the paragraph-collapsed text", target.Overview)
	}
	if target.Tagline == nil || *target.Tagline != "Free your mind." {
		t.Errorf("Tagline = %v, want Free your mind.", target.Tagline)
	}
	if target.CommunityRating == nil || *target.CommunityRating != 8.2 {
		t.Errorf("CommunityRating = %v, want 8.2", target.CommunityRating)
	}
	if target.OfficialRating == nil || *target.OfficialRating != "R" {
		t.Errorf("OfficialRating = %v, want R (US certification)", target.OfficialRating)
	}
	if target.ProductionYear == nil || *target.ProductionYear != 1999 {
		t.Errorf("ProductionYear = %v, want 1999", target.ProductionYear)
	}
	if target.PremiereDate == nil || target.PremiereDate.Format("2006-01-02") != "1999-03-31" {
		t.Errorf("PremiereDate = %v, want 1999-03-31", target.PremiereDate)
	}
	if target.OriginalLanguage == nil || *target.OriginalLanguage != "en" {
		t.Errorf("OriginalLanguage = %v, want en", target.OriginalLanguage)
	}
	if !reflect.DeepEqual(target.Values.Genres, []string{"Action", "Science Fiction"}) {
		t.Errorf("Genres = %v", target.Values.Genres)
	}
	if !reflect.DeepEqual(target.Values.Studios, []string{"Warner Bros. Pictures", "Village Roadshow Pictures"}) {
		t.Errorf("Studios = %v", target.Values.Studios)
	}
	if !reflect.DeepEqual(target.Values.Tags, []string{"artificial reality", "dystopia"}) {
		t.Errorf("Tags = %v", target.Values.Tags)
	}
	if !reflect.DeepEqual(target.Values.ProductionLocations, []string{"United States of America"}) {
		t.Errorf("ProductionLocations = %v", target.Values.ProductionLocations)
	}

	if id, ok := target.ProviderIDs.Get("Tmdb"); !ok || id != "603" {
		t.Errorf("Tmdb id = %q, %t; want 603", id, ok)
	}
	if id, ok := target.ProviderIDs.Get("Imdb"); !ok || id != "tt0133093" {
		t.Errorf("Imdb id = %q, %t; want tt0133093", id, ok)
	}
	if id, ok := target.ProviderIDs.Get("TmdbCollection"); !ok || id != "2344" {
		t.Errorf("TmdbCollection id = %q, %t; want 2344", id, ok)
	}
	if target.Extra.Movie == nil || target.Extra.Movie.TmdbCollectionName == nil || *target.Extra.Movie.TmdbCollectionName != "The Matrix Collection" {
		t.Errorf("collection name = %v, want The Matrix Collection", target.Extra.Movie)
	}

	// Cast is ordered and bounded; the third actor has no profile image but is
	// kept because HideMissingCastMembers is off by default.
	if len(target.People) != 5 {
		t.Fatalf("People count = %d, want 3 cast + 2 wanted crew (grip excluded)", len(target.People))
	}
	if target.People[0].Name != "Keanu Reeves" || target.People[0].Type != item.PersonKindActor || target.People[0].Role == nil || *target.People[0].Role != "Neo" {
		t.Errorf("first person = %#v, want Keanu Reeves as Neo", target.People[0])
	}
	if target.People[3].Name != "Lana Wachowski" || target.People[3].Type != item.PersonKindDirector {
		t.Errorf("crew[0] = %#v, want Lana Wachowski director", target.People[3])
	}
	if target.People[4].Name != "Lilly Wachowski" || target.People[4].Type != item.PersonKindWriter {
		t.Errorf("crew[1] = %#v, want Lilly Wachowski writer", target.People[4])
	}

	// Each credit carries its TMDb profile image (PersonInfo.ImageUrl) at the
	// configured size; a credit without profile_path has none.
	for index, want := range []string{"https://image.tmdb.org/t/p/original/keanu.jpg", "https://image.tmdb.org/t/p/original/laurence.jpg", "", "https://image.tmdb.org/t/p/original/lana.jpg", "https://image.tmdb.org/t/p/original/lilly.jpg"} {
		if got := target.People[index].ImageURL; got != want {
			t.Errorf("credit %q ImageURL = %q, want %q", target.People[index].Name, got, want)
		}
	}

	// Each credit carries the TMDb person id (PersonInfo.ProviderIds), which
	// the scanner applies to the Person entity.
	for index, want := range []string{"6384", "2975", "530", "9339", "9340"} {
		if got, _ := target.People[index].ProviderIDs.Get("Tmdb"); got != want {
			t.Errorf("credit %q Tmdb id = %q, want %q", target.People[index].Name, got, want)
		}
	}

	// Each credit carries a durable PersonID derived from its name through
	// library.PersonEntity. The identity matches the one the scanner and the
	// identify Apply path use, so the provider output persists without a
	// test-only adapter.
	for _, credit := range target.People {
		if credit.PersonID.IsNil() {
			t.Errorf("credit %q has nil PersonID, want the durable identity", credit.Name)
		}
		if want := library.PersonEntity(credit.Name).ID; credit.PersonID != want {
			t.Errorf("credit %q PersonID = %s, want %s", credit.Name, credit.PersonID, want)
		}
	}

	// Trailers: only the YouTube trailer, not the featurette.
	if len(target.Extra.RemoteTrailers) != 1 || target.Extra.RemoteTrailers[0].URL == nil || *target.Extra.RemoteTrailers[0].URL != "https://www.youtube.com/watch?v=vKQi3bBA1y8" {
		t.Errorf("RemoteTrailers = %#v, want the single YouTube trailer", target.Extra.RemoteTrailers)
	}

	// Images: primary poster (from images.posters), a plain backdrop, the
	// text backdrop demoted to Thumb, and the logo — all as TMDb CDN URLs.
	assertImage(t, target.ImageInfos, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/best-poster.jpg")
	assertImage(t, target.ImageInfos, item.ImageTypeBackdrop, "https://image.tmdb.org/t/p/original/plain-backdrop.jpg")
	assertImage(t, target.ImageInfos, item.ImageTypeThumb, "https://image.tmdb.org/t/p/original/text-backdrop.jpg")
	assertImage(t, target.ImageInfos, item.ImageTypeLogo, "https://image.tmdb.org/t/p/original/logo.png")
}

// TestProviderInertWithoutAPIKey confirms a keyless provider never touches the
// network and never fails a scan (D-11).
func TestProviderInertWithoutAPIKey(t *testing.T) {
	provider := tmdb.NewProvider(tmdb.Options{
		HTTP:   remote.NewClient(remote.Config{Doer: remotetest.NewCassette()}),
		Config: tmdb.DefaultConfig(), // no API key
	})
	target := item.Item{Kind: item.KindMovie, Name: stringPtr("The Matrix")}
	handled, err := provider.Refresh(t.Context(), movieRefreshContext(), &target)
	if handled || err != nil {
		t.Fatalf("keyless provider = handled %t err %v, want inert (false, nil)", handled, err)
	}
	if target.ProviderIDs.Snapshot() != nil {
		t.Fatalf("keyless provider mutated the item: %#v", target)
	}
}

func TestProviderNilLoggerUsesSlogDefault(t *testing.T) {
	previousDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousDefault) })
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	provider := tmdb.NewProvider(tmdb.Options{})
	target := item.Item{Kind: item.KindMovie}
	handled, err := provider.Refresh(t.Context(), movieRefreshContext(), &target)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if handled {
		t.Fatal("inert provider reported handled")
	}
	if !strings.Contains(logs.String(), "tmdb refresh") {
		t.Errorf("nil logger did not use slog default handler: %q", logs.String())
	}
}

// TestProviderStageAndName pins the pipeline contract.
func TestProviderStageAndName(t *testing.T) {
	provider := tmdb.NewProvider(tmdb.Options{})
	if provider.Name() != "TheMovieDb" {
		t.Errorf("Name = %q, want TheMovieDb", provider.Name())
	}
	if provider.Stage() != providers.ProviderStageRemoteMetadata {
		t.Errorf("Stage = %v, want remote metadata", provider.Stage())
	}
}

func assertImage(t *testing.T, images []item.ImageInfo, kind item.ImageType, path string) {
	t.Helper()
	for _, image := range images {
		if image.Type == kind && image.Path == path {
			return
		}
	}
	t.Errorf("missing %v image %q in %#v", kind, path, images)
}

// findImage returns the first image of kind, or nil when absent.
func findImage(images []item.ImageInfo, kind item.ImageType) *item.ImageInfo {
	for index := range images {
		if images[index].Type == kind {
			return &images[index]
		}
	}
	return nil
}

// assertImageDimensions checks that the image of kind carries the exact path,
// Width, and Height. Zero dimensions assert an unknown size.
func assertImageDimensions(t *testing.T, images []item.ImageInfo, kind item.ImageType, path string, width, height int) {
	t.Helper()
	image := findImage(images, kind)
	if image == nil {
		t.Errorf("missing %v image in %#v", kind, images)
		return
	}
	if image.Path != path || image.Width != width || image.Height != height {
		t.Errorf("%v image = {Path:%q Width:%d Height:%d}, want {Path:%q Width:%d Height:%d}", kind, image.Path, image.Width, image.Height, path, width, height)
	}
}

// TestProviderMovieImageDimensions proves a refresh carries the TMDb image
// collection width and height into each attached ImageInfo: Primary from
// images.posters, the plain Backdrop, the text backdrop mapped to Thumb, and
// the Logo.
func TestProviderMovieImageDimensions(t *testing.T) {
	provider := newCassetteProvider(t, "testdata/the-matrix.json")
	target := item.Item{Kind: item.KindMovie, Name: stringPtr("The Matrix"), ProductionYear: intPtr(1999)}

	handled, err := provider.Refresh(t.Context(), movieRefreshContext(), &target)
	if err != nil {
		t.Fatalf("refresh movie: %v", err)
	}
	if !handled {
		t.Fatal("provider did not claim the movie despite a matching cassette")
	}

	assertImageDimensions(t, target.ImageInfos, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/best-poster.jpg", 500, 750)
	assertImageDimensions(t, target.ImageInfos, item.ImageTypeBackdrop, "https://image.tmdb.org/t/p/original/plain-backdrop.jpg", 1280, 720)
	assertImageDimensions(t, target.ImageInfos, item.ImageTypeThumb, "https://image.tmdb.org/t/p/original/text-backdrop.jpg", 1280, 720)
	assertImageDimensions(t, target.ImageInfos, item.ImageTypeLogo, "https://image.tmdb.org/t/p/original/logo.png", 800, 310)
}

// TestProviderMovieImageDimensionsReplace proves a full refresh with
// ReplaceAllImages overwrites Path, Width, and Height together, and that a
// non-replacing refresh leaves an existing image of the same type untouched.
func TestProviderMovieImageDimensionsReplace(t *testing.T) {
	existing := item.ImageInfo{Type: item.ImageTypePrimary, Path: "/local/poster.jpg", Width: 10, Height: 20}

	provider := newCassetteProvider(t, "testdata/the-matrix.json")
	target := item.Item{Kind: item.KindMovie, Name: stringPtr("The Matrix"), ProductionYear: intPtr(1999), ImageInfos: []item.ImageInfo{existing}}
	if _, err := provider.Refresh(t.Context(), movieRefreshContext(), &target); err != nil {
		t.Fatalf("refresh movie: %v", err)
	}
	assertImageDimensions(t, target.ImageInfos, item.ImageTypePrimary, existing.Path, existing.Width, existing.Height)

	replacing := movieRefreshContext()
	replacing.Image = library.RefreshModeFullRefresh
	replacing.ReplaceAllImages = true
	target = item.Item{Kind: item.KindMovie, Name: stringPtr("The Matrix"), ProductionYear: intPtr(1999), ImageInfos: []item.ImageInfo{existing}}
	if _, err := provider.Refresh(t.Context(), replacing, &target); err != nil {
		t.Fatalf("refresh movie (replace): %v", err)
	}
	assertImageDimensions(t, target.ImageInfos, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/best-poster.jpg", 500, 750)
}

// TestProviderMovieUntaggedBackdrops proves a replacing refresh of a movie
// matched by TMDb id requests include_image_language=en,null and attaches the
// first untagged backdrop as Backdrop, after a tagged one that maps to Thumb
// (#783).
func TestProviderMovieUntaggedBackdrops(t *testing.T) {
	provider := newCassetteProvider(t, "testdata/untagged-backdrops.json")
	refresh := movieRefreshContext()
	refresh.Image = library.RefreshModeFullRefresh
	refresh.ReplaceAllImages = true
	target := item.Item{Kind: item.KindMovie, Name: stringPtr("2 Fast 2 Furious")}
	target.ProviderIDs.Set("Tmdb", "584")

	handled, err := provider.Refresh(t.Context(), refresh, &target)
	if err != nil {
		t.Fatalf("refresh movie: %v", err)
	}
	if !handled {
		t.Fatal("provider did not claim the movie despite a matching cassette")
	}
	assertImageDimensions(t, target.ImageInfos, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/2f2f-poster.jpg", 500, 750)
	assertImageDimensions(t, target.ImageInfos, item.ImageTypeBackdrop, "https://image.tmdb.org/t/p/original/2f2f-backdrop.jpg", 1920, 1080)
	assertImageDimensions(t, target.ImageInfos, item.ImageTypeThumb, "https://image.tmdb.org/t/p/original/2f2f-text-backdrop.jpg", 1280, 720)
	assertImageDimensions(t, target.ImageInfos, item.ImageTypeLogo, "https://image.tmdb.org/t/p/original/2f2f-logo.png", 800, 310)
}

// TestProviderMovieImageDimensionsPosterFallback proves that a movie with no
// usable image collection attaches poster_path as Primary with unknown (zero)
// dimensions, because poster_path carries none.
func TestProviderMovieImageDimensionsPosterFallback(t *testing.T) {
	provider := newCassetteProvider(t, "testdata/movie-poster-fallback.json")
	target := item.Item{Kind: item.KindMovie, Name: stringPtr("The Emperor's New Groove")}
	target.ProviderIDs.Set("Tmdb", "11688")

	handled, err := provider.Refresh(t.Context(), movieRefreshContext(), &target)
	if err != nil {
		t.Fatalf("refresh movie: %v", err)
	}
	if !handled {
		t.Fatal("provider did not claim the movie despite a matching cassette")
	}

	if len(target.ImageInfos) != 1 {
		t.Fatalf("ImageInfos = %#v, want only the poster_path Primary", target.ImageInfos)
	}
	assertImageDimensions(t, target.ImageInfos, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/emperor-poster.jpg", 0, 0)
}

func intPtr(value int) *int { return &value }
