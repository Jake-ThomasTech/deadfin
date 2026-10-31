// SPDX-License-Identifier: GPL-2.0-only

package tmdb_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/remote"
	"github.com/deadfin-org/deadfin/internal/providers/remote/remotetest"
	"github.com/deadfin-org/deadfin/internal/providers/tmdb"
)

// newCassetteLookupProvider builds a TMDb LookupProvider backed by a cassette
// so the search path is exercised against a pinned fixture.
func newCassetteLookupProvider(t *testing.T, cassettePath string) *tmdb.LookupProvider {
	t.Helper()
	cassette, err := remotetest.Load(cassettePath)
	if err != nil {
		t.Fatalf("load cassette: %v", err)
	}
	config := tmdb.DefaultConfig()
	config.APIKey = "test-key"
	return tmdb.NewLookupProvider(tmdb.Options{
		HTTP:   remote.NewClient(remote.Config{Doer: cassette, CacheEntries: 32}),
		Config: config,
	})
}

// TestLookupProviderSearchMovieFromCassette exercises the TMDb lookup Search
// path against a recorded cassette (no live API): it searches by name+year and
// verifies the returned RemoteSearchResult carries the TMDb provider ID and
// the search-result name.
func TestLookupProviderSearchMovieFromCassette(t *testing.T) {
	provider := newCassetteLookupProvider(t, "testdata/the-matrix.json")
	if !provider.SearchSupports(item.KindMovie) {
		t.Fatal("SearchSupports(Movie) = false, want true")
	}
	results, err := provider.Search(context.Background(), providers.Lookup{
		Kind:     item.KindMovie,
		Name:     "The Matrix",
		Year:     intPtr(1999),
		Language: "en",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned no results")
	}
	// The first result should be The Matrix (TMDb id 603).
	r := results[0]
	if r.ProviderIDs["Tmdb"] != "603" {
		t.Errorf("ProviderIDs[Tmdb] = %q, want 603", r.ProviderIDs["Tmdb"])
	}
	if r.Name == nil || *r.Name != "The Matrix" {
		t.Errorf("Name = %v, want The Matrix", r.Name)
	}
}

// TestLookupProviderSearchByTmdbIDFromCassette verifies the TMDb lookup Search
// resolves by provider ID (Tmdb), fetching the movie detail directly.
func TestLookupProviderSearchByTmdbIDFromCassette(t *testing.T) {
	provider := newCassetteLookupProvider(t, "testdata/the-matrix.json")
	results, err := provider.Search(context.Background(), providers.Lookup{
		Kind:        item.KindMovie,
		ProviderIDs: map[string]string{"Tmdb": "603"},
		Language:    "en",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search by Tmdb ID returned no results")
	}
	r := results[0]
	if r.ProviderIDs["Tmdb"] != "603" {
		t.Errorf("ProviderIDs[Tmdb] = %q, want 603", r.ProviderIDs["Tmdb"])
	}
	if r.Name == nil || *r.Name != "The Matrix" {
		t.Errorf("Name = %v, want The Matrix", r.Name)
	}
}

// TestLookupProviderInertWithoutAPIKey verifies a keyless provider is inert:
// SearchSupports and ImageSupports return false (D-11).
func TestLookupProviderInertWithoutAPIKey(t *testing.T) {
	provider := tmdb.NewLookupProvider(tmdb.Options{
		HTTP:   remote.NewClient(remote.Config{}),
		Config: tmdb.DefaultConfig(),
	})
	if provider.SearchSupports(item.KindMovie) {
		t.Error("SearchSupports(Movie) = true, want false (inert without key)")
	}
	if provider.ImageSupports(item.KindMovie) {
		t.Error("ImageSupports(Movie) = true, want false (inert without key)")
	}
}

// TestRemoteImagesWireParity verifies the TMDb image provider does not
// synthesize ThumbnailUrl: the pinned stock ConvertToRemoteImageInfo
// (TmdbClientManager.cs:647-683) never sets ThumbnailUrl, so the wire
// shape must omit it. The test calls Images against a cassette, confirms
// every returned RemoteImageInfo has an empty ThumbnailURL, converts each
// to the dto.RemoteImageInfo wire shape (same logic as the handler's
// remoteImageInfoToDTO), and asserts the serialized JSON contains no
// "ThumbnailUrl" field.
func TestRemoteImagesWireParity(t *testing.T) {
	provider := newCassetteLookupProvider(t, "testdata/lookup-images.json")
	if !provider.ImageSupports(item.KindMovie) {
		t.Fatal("ImageSupports(Movie) = false, want true")
	}
	images, err := provider.Images(context.Background(), providers.ImageLookup{
		Kind:        item.KindMovie,
		ProviderIDs: map[string]string{"Tmdb": "603"},
	})
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(images) == 0 {
		t.Fatal("Images returned no results")
	}
	for i, img := range images {
		if img.ThumbnailURL != "" {
			t.Errorf("images[%d].ThumbnailURL = %q, want empty (stock never synthesizes it)", i, img.ThumbnailURL)
		}
	}
	// Convert to the wire DTO the same way the handler does and assert
	// ThumbnailUrl is absent from the serialized JSON.
	for i, img := range images {
		wire := dto.RemoteImageInfo{
			Type:       img.Type.String(),
			RatingType: img.RatingType,
		}
		if img.ProviderName != "" {
			wire.ProviderName = &img.ProviderName
		}
		if img.URL != "" {
			wire.URL = &img.URL
		}
		if img.ThumbnailURL != "" {
			wire.ThumbnailURL = &img.ThumbnailURL
		}
		if img.Height > 0 {
			wire.Height = &img.Height
		}
		if img.Width > 0 {
			wire.Width = &img.Width
		}
		if img.CommunityRating != 0 {
			wire.CommunityRating = &img.CommunityRating
		}
		if img.VoteCount > 0 {
			wire.VoteCount = &img.VoteCount
		}
		if img.Language != "" {
			wire.Language = &img.Language
		}
		raw, err := json.Marshal(wire)
		if err != nil {
			t.Fatalf("marshal images[%d]: %v", i, err)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal images[%d]: %v", i, err)
		}
		if _, ok := decoded["ThumbnailUrl"]; ok {
			t.Errorf("images[%d] wire JSON contains ThumbnailUrl, want it omitted:\n%s", i, raw)
		}
	}
}

// lookupImages fetches remote images for kind/tmdbID from a cassette and
// fails the test on error or an empty result.
func lookupImages(t *testing.T, cassettePath string, kind item.Kind, tmdbID string) []providers.RemoteImageInfo {
	t.Helper()
	provider := newCassetteLookupProvider(t, cassettePath)
	images, err := provider.Images(context.Background(), providers.ImageLookup{
		Kind:        kind,
		ProviderIDs: map[string]string{"Tmdb": tmdbID},
	})
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(images) == 0 {
		t.Fatal("Images returned no results")
	}
	return images
}

// assertRemoteImageDimensions checks that exactly one RemoteImageInfo has the
// given URL and that it carries imgType and the exact Width and Height.
func assertRemoteImageDimensions(t *testing.T, images []providers.RemoteImageInfo, imgType item.ImageType, url string, width, height int) {
	t.Helper()
	for _, img := range images {
		if img.URL != url {
			continue
		}
		if img.Type != imgType || img.Width != width || img.Height != height {
			t.Errorf("image %q = {Type:%v Width:%d Height:%d}, want {Type:%v Width:%d Height:%d}", url, img.Type, img.Width, img.Height, imgType, width, height)
		}
		return
	}
	t.Errorf("missing image %q in %#v", url, images)
}

// TestLookupMovieImageDimensions proves the movie Images constructor copies the
// exact TMDb width and height for Primary, Backdrop, Thumb, and Logo.
func TestLookupMovieImageDimensions(t *testing.T) {
	images := lookupImages(t, "testdata/lookup-images.json", item.KindMovie, "603")
	assertRemoteImageDimensions(t, images, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/best-poster.jpg", 500, 750)
	assertRemoteImageDimensions(t, images, item.ImageTypeBackdrop, "https://image.tmdb.org/t/p/original/plain-backdrop.jpg", 1280, 720)
	assertRemoteImageDimensions(t, images, item.ImageTypeThumb, "https://image.tmdb.org/t/p/original/text-backdrop.jpg", 1280, 720)
	assertRemoteImageDimensions(t, images, item.ImageTypeLogo, "https://image.tmdb.org/t/p/original/logo.png", 800, 310)
}

// TestLookupSeriesImageDimensions proves the series Images constructor copies
// the exact TMDb width and height for Primary, Backdrop, Thumb, and Logo.
func TestLookupSeriesImageDimensions(t *testing.T) {
	images := lookupImages(t, "testdata/breaking-bad.json", item.KindSeries, "1396")
	assertRemoteImageDimensions(t, images, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/bb-best-poster.jpg", 500, 750)
	assertRemoteImageDimensions(t, images, item.ImageTypeBackdrop, "https://image.tmdb.org/t/p/original/bb-backdrop.jpg", 1280, 720)
	assertRemoteImageDimensions(t, images, item.ImageTypeThumb, "https://image.tmdb.org/t/p/original/bb-text-backdrop.jpg", 1920, 1080)
	assertRemoteImageDimensions(t, images, item.ImageTypeLogo, "https://image.tmdb.org/t/p/original/bb-logo.png", 1000, 400)
}

// TestLookupMovieImageDimensionsPosterFallback proves the poster_path fallback
// RemoteImageInfo keeps zero (unknown) dimensions.
func TestLookupMovieImageDimensionsPosterFallback(t *testing.T) {
	images := lookupImages(t, "testdata/movie-poster-fallback.json", item.KindMovie, "11688")
	if len(images) != 1 {
		t.Fatalf("Images = %#v, want only the poster_path Primary", images)
	}
	assertRemoteImageDimensions(t, images, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/emperor-poster.jpg", 0, 0)
}

// TestLookupSeriesImageDimensionsPosterFallback proves the series poster_path
// fallback RemoteImageInfo keeps zero (unknown) dimensions.
func TestLookupSeriesImageDimensionsPosterFallback(t *testing.T) {
	images := lookupImages(t, "testdata/series-poster-fallback.json", item.KindSeries, "1396")
	if len(images) != 1 {
		t.Fatalf("Images = %#v, want only the poster_path Primary", images)
	}
	assertRemoteImageDimensions(t, images, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/bb-poster.jpg", 0, 0)
}

// TestLookupMovieImagesUntaggedBackdrops proves a RemoteImages lookup with a
// language set still lists TMDb's untagged backdrops as Backdrop. Like
// upstream's GetMovieAsync(id, null, null, null), the request carries no
// language: TMDb would otherwise return only the request-language images, and
// every listed backdrop would map to Thumb (#783).
func TestLookupMovieImagesUntaggedBackdrops(t *testing.T) {
	provider := newCassetteLookupProvider(t, "testdata/untagged-backdrops.json")
	images, err := provider.Images(context.Background(), providers.ImageLookup{
		Kind:        item.KindMovie,
		Language:    "en",
		ProviderIDs: map[string]string{"Tmdb": "584"},
	})
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	assertRemoteImageDimensions(t, images, item.ImageTypeBackdrop, "https://image.tmdb.org/t/p/original/2f2f-backdrop.jpg", 1920, 1080)
	assertRemoteImageDimensions(t, images, item.ImageTypeBackdrop, "https://image.tmdb.org/t/p/original/2f2f-backdrop-2.jpg", 1280, 720)
	assertRemoteImageDimensions(t, images, item.ImageTypeThumb, "https://image.tmdb.org/t/p/original/2f2f-text-backdrop.jpg", 1280, 720)
	assertRemoteImageDimensions(t, images, item.ImageTypeThumb, "https://image.tmdb.org/t/p/original/2f2f-text-backdrop-fr.jpg", 1280, 720)
	assertRemoteImageDimensions(t, images, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/2f2f-poster-fr.jpg", 500, 750)
}
