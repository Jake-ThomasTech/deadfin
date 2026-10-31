// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/providers/remote"
	"github.com/deadfin-org/deadfin/internal/providers/remote/remotetest"
	"github.com/deadfin-org/deadfin/internal/providers/tmdb"
	"github.com/deadfin-org/deadfin/internal/store"
)

// backfillCassetteRemotePatch loads the recorded TMDb the-matrix cassette and
// runs the real TMDb provider through it to produce a remote item patch. The
// cassette rejects every unmatched request, so the test makes no live HTTP
// request and fails loudly on any unexpected call. The target carries the
// stored item identity so the provider resolves the TMDb id from it and maps
// the cassette movie onto the target in place. The caller passes the result
// to MetadataBackfill as the desired item.
func backfillCassetteRemotePatch(t *testing.T, target item.Item) item.Item {
	t.Helper()
	cassette, err := remotetest.Load(filepath.Join("..", "..", "providers", "tmdb", "testdata", "the-matrix.json"))
	if err != nil {
		t.Fatalf("load TMDb cassette: %v", err)
	}
	config := tmdb.DefaultConfig()
	config.APIKey = "test-key"
	provider := tmdb.NewProvider(tmdb.Options{
		HTTP:   remote.NewClient(remote.Config{Doer: cassette, CacheEntries: 32}),
		Config: config,
	})
	refresh := library.RefreshContext{
		Options: library.LibraryOptions{
			EnableInternetProviders:   true,
			PreferredMetadataLanguage: "en",
			MetadataCountryCode:       "US",
		},
	}
	patch := target
	handled, err := provider.Refresh(t.Context(), refresh, &patch)
	if err != nil {
		t.Fatalf("refresh from cassette: %v", err)
	}
	if !handled {
		t.Fatal("TMDb provider did not claim the movie despite a matching cassette")
	}
	return patch
}

// backfillCassetteRemotePatchOmittingVoteAverage builds an in-memory TMDb
// cassette whose movie response omits vote_average. The real TMDb provider
// runs through the cassette, so the provider itself assigns CommunityRating
// from the omitted field: VoteAverage defaults to zero, and the provider
// sets CommunityRating to a pointer to zero. The test does not mutate the
// adapter output. The cassette rejects every unmatched request, so the test
// makes no live HTTP request.
func backfillCassetteRemotePatchOmittingVoteAverage(t *testing.T, target item.Item) item.Item {
	t.Helper()
	cassette := remotetest.NewCassette(
		remotetest.Interaction{
			Method: "GET",
			URL:    "https://api.themoviedb.org/3/configuration",
			Status: 200,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body: json.RawMessage(`{
  "images": {
    "secure_base_url": "https://image.tmdb.org/t/p/",
    "poster_sizes": ["w92", "w154", "w185", "w342", "w500", "w780", "original"],
    "backdrop_sizes": ["w300", "w780", "w1280", "original"]
  }
}`),
		},
		remotetest.Interaction{
			Method: "GET",
			URL:    "https://api.themoviedb.org/3/movie/603?language=en&include_image_language=en,null&append_to_response=credits,release_dates,images,videos,keywords",
			Status: 200,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body: json.RawMessage(`{
  "id": 603,
  "title": "The Matrix",
  "original_title": "The Matrix",
  "original_language": "en",
  "overview": "A computer hacker learns from mysterious rebels.\n\nHe joins the fight.",
  "tagline": "Free your mind.",
  "poster_path": "/matrix-poster.jpg",
  "release_date": "1999-03-31",
  "imdb_id": "tt0133093",
  "belongs_to_collection": {"id": 2344, "name": "The Matrix Collection"},
  "genres": [{"name": "Action"}, {"name": "Science Fiction"}],
  "production_companies": [{"name": "Warner Bros. Pictures"}, {"name": "Village Roadshow Pictures"}],
  "production_countries": [{"iso_3166_1": "US", "name": "United States of America"}],
  "credits": {
    "cast": [
      {"id": 6384, "name": "Keanu Reeves", "character": "Neo", "order": 0, "profile_path": "/keanu.jpg"},
      {"id": 2975, "name": "Laurence Fishburne", "character": "Morpheus", "order": 1, "profile_path": "/laurence.jpg"},
      {"id": 530, "name": "Carrie-Anne Moss", "character": "Trinity", "order": 2, "profile_path": ""}
    ],
    "crew": [
      {"id": 9339, "name": "Lana Wachowski", "job": "Director", "department": "Directing", "profile_path": "/lana.jpg"},
      {"id": 9340, "name": "Lilly Wachowski", "job": "Writer", "department": "Writing", "profile_path": "/lilly.jpg"},
      {"id": 1, "name": "Some Grip", "job": "Grip", "department": "Camera", "profile_path": ""}
    ]
  },
  "keywords": {"keywords": [{"name": "artificial reality"}, {"name": "dystopia"}]},
  "release_dates": {
    "results": [
      {"iso_3166_1": "US", "release_dates": [{"certification": "R"}]},
      {"iso_3166_1": "GB", "release_dates": [{"certification": "15"}]}
    ]
  },
  "videos": {
    "results": [
      {"key": "vKQi3bBA1y8", "name": "The Matrix Trailer", "site": "YouTube", "type": "Trailer"},
      {"key": "abc123", "name": "Featurette", "site": "YouTube", "type": "Featurette"}
    ]
  },
  "images": {
    "posters": [{"file_path": "/best-poster.jpg", "iso_639_1": "en", "width": 500, "height": 750, "vote_average": 5.6, "vote_count": 12}],
    "backdrops": [
      {"file_path": "/plain-backdrop.jpg", "iso_639_1": null, "width": 1280, "height": 720},
      {"file_path": "/text-backdrop.jpg", "iso_639_1": "en", "width": 1280, "height": 720}
    ],
    "logos": [{"file_path": "/logo.png", "iso_639_1": "en"}]
  }
}`),
		},
	)
	config := tmdb.DefaultConfig()
	config.APIKey = "test-key"
	provider := tmdb.NewProvider(tmdb.Options{
		HTTP:   remote.NewClient(remote.Config{Doer: cassette, CacheEntries: 32}),
		Config: config,
	})
	refresh := library.RefreshContext{
		Options: library.LibraryOptions{
			EnableInternetProviders:   true,
			PreferredMetadataLanguage: "en",
			MetadataCountryCode:       "US",
		},
	}
	patch := target
	handled, err := provider.Refresh(t.Context(), refresh, &patch)
	if err != nil {
		t.Fatalf("refresh from cassette without vote_average: %v", err)
	}
	if !handled {
		t.Fatal("TMDb provider did not claim the movie despite a matching cassette")
	}
	return patch
}

// TestItemStoreMetadataBackfillSQLite runs the metadata-backfill store test
// against SQLite.
func TestItemStoreMetadataBackfillSQLite(t *testing.T) {
	runItemStoreMetadataBackfill(t, openItemStore)
}

// TestItemStoreMetadataBackfillPostgres runs the metadata-backfill store test
// against PostgreSQL.
func TestItemStoreMetadataBackfillPostgres(t *testing.T) {
	runItemStoreMetadataBackfill(t, openPostgresItemStore)
}

// runItemStoreMetadataBackfill proves the MetadataBackfill store operation
// preserves stored fields the remote patch omits, accepts non-empty provider
// values for unlocked fields, merges provider IDs and images per key, and
// preserves satellite families the backfill does not produce.
func runItemStoreMetadataBackfill(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 11900, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert backfill parent: %v", err)
	}

	// Seed a complete item with local metadata, locked fields, provider IDs,
	// local artwork, remote artwork, people, genres, studios, tags, runtime,
	// and an untouched satellite family (trailer types, linked children,
	// additional parts). The seed sets every structural field so the assertion
	// compares the complete state.
	seed := reconciliationChild(t, 11901, item.KindMovie, parent.ID, "seed movie")
	seed.Overview = stringPtr("A probe-sourced overview")
	seed.RunTimeTicks = int64Ptr(98765432100)
	seed.Values.Genres = []string{"Action", "Sci-Fi"}
	seed.Values.Studios = []string{"Warner Bros"}
	seed.Values.Tags = []string{"4k", "hdr"}
	seed.People = []item.PersonCredit{
		{PersonID: contractUUID(t, 11910), Name: "Lana Wachowski", Type: item.PersonKindDirector},
		{PersonID: contractUUID(t, 11911), Name: "Keanu Reeves", Type: item.PersonKindActor, Role: stringPtr("Neo")},
	}
	// The seed carries Tmdb=603 so the cassette provider resolves the movie
	// by id, plus an unrelated Tvdb id that must survive the per-key merge.
	seed.ProviderIDs = providerIDs("Tmdb", "603", "Tvdb", "12345")
	seed.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 0, Path: "/movies/the-matrix.jpg"},
		{Type: item.ImageTypeBackdrop, Index: 0, Path: "https://image.tmdb.org/t/p/original/old-backdrop.jpg"},
		{Type: item.ImageTypeBanner, Index: 0, Path: "/movies/the-matrix-banner.jpg"},
	}
	seed.LockedFields = []item.MetadataField{item.MetadataFieldName}
	seed.TrailerTypes = []item.TrailerType{item.TrailerTypeLocalTrailer}
	seed.LinkedChildren = []item.LinkedChildRef{
		{ItemID: contractUUID(t, 11920), Type: item.LinkedChildTypeManual},
	}
	seed.AdditionalParts = []string{"/movies/the-matrix-part2.mkv"}
	seed.ProductionYear = intPtr(1999)
	seed.CommunityRating = float32Ptr(8.7)
	seed.IsLocked = false
	// Set the structural fields the assertion compares. The backfill must
	// preserve every structural field from the stored row. PlaylistOwnerUserID
	// and OpenAccess are valid only for Playlist items, so they stay zero on
	// a Movie and the assertion compares those zero values.
	primaryVersionID := contractUUID(t, 11940)
	seed.PrimaryVersionID = &primaryVersionID
	seed.IsInMixedFolder = true
	seed.SourceType = item.SourceTypeLibrary
	movieMediaType := item.MediaTypeVideo
	seed.MediaType = &movieMediaType
	// Set an unrelated Extra field the cassette does not produce. It must
	// survive the merge.
	seed.Extra.Video = &item.VideoExtra{AspectRatio: stringPtr("16:9")}
	seedSaved := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}
	// Upsert derives the hierarchy columns; the backfill must keep them.
	stored, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("load seeded movie: %v", err)
	}

	// Seed user data so the test proves the backfill preserves it. The seed
	// sets every UserData field so the assertion compares the complete state.
	userID := contractUUID(t, 11930)
	seedPositionTicks := int64(98765432)
	seedLastPlayed := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	seedRating := float32(4.5)
	seedLikes := 7
	seedAudioIndex := 2
	seedSubtitleIndex := 5
	if _, err := items.MutateUserDataForItem(t.Context(), userID, seed.ID, func(data *store.UserData) error {
		data.Played = true
		data.PlayCount = 3
		data.IsFavorite = true
		data.Rating = &seedRating
		data.Likes = &seedLikes
		data.PositionTicks = seedPositionTicks
		data.LastPlayed = &seedLastPlayed
		data.AudioStreamIndex = &seedAudioIndex
		data.SubtitleStreamIndex = &seedSubtitleIndex
		return nil
	}); err != nil {
		t.Fatalf("seed user data: %v", err)
	}

	// Apply a cassette-backed remote patch. The the-matrix cassette runs the
	// real TMDb provider through a recorded HTTP interaction, so the patch
	// carries provider-supplied values: a new overview, a non-zero rating, new
	// genres, studios, tags, people, images, and Extra fields. All probe and
	// sidecar fields the cassette omits must stay from the stored row.
	remote := backfillCassetteRemotePatch(t, item.Item{
		ID:          seed.ID,
		Kind:        seed.Kind,
		ParentID:    seed.ParentID,
		Path:        seed.Path,
		Name:        seed.Name,
		ProviderIDs: seed.ProviderIDs,
	})
	backfillSaved := seedSaved.Add(time.Hour)
	result, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, backfillSaved, store.MetadataBackfillOptions{ForceRefresh: true})
	if err != nil {
		t.Fatalf("metadata backfill: %v", err)
	}
	assertReconcileResult(t, result, nil, []dto.UUID{seed.ID}, nil)

	loaded, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get backfilled movie: %v", err)
	}

	// Assert structural identity is preserved exactly, including every field
	// the remote stage cannot change.
	if loaded.ID != seed.ID {
		t.Fatalf("ID = %s, want %s", loaded.ID, seed.ID)
	}
	if loaded.Kind != seed.Kind {
		t.Fatalf("Kind = %s, want %s", loaded.Kind, seed.Kind)
	}
	if !reflect.DeepEqual(loaded.ParentID, seed.ParentID) {
		t.Fatalf("ParentID = %v, want %v", loaded.ParentID, seed.ParentID)
	}
	if !reflect.DeepEqual(loaded.Path, seed.Path) {
		t.Fatalf("Path = %v, want %v", loaded.Path, seed.Path)
	}
	if !reflect.DeepEqual(loaded.TopParentID, stored.TopParentID) {
		t.Fatalf("TopParentID = %v, want the stored %v", loaded.TopParentID, stored.TopParentID)
	}
	if !reflect.DeepEqual(loaded.SeriesID, stored.SeriesID) {
		t.Fatalf("SeriesID = %v, want the stored %v", loaded.SeriesID, stored.SeriesID)
	}
	if !reflect.DeepEqual(loaded.SeasonID, stored.SeasonID) {
		t.Fatalf("SeasonID = %v, want the stored %v", loaded.SeasonID, stored.SeasonID)
	}
	if !reflect.DeepEqual(loaded.ChannelID, seed.ChannelID) {
		t.Fatalf("ChannelID = %v, want %v", loaded.ChannelID, seed.ChannelID)
	}
	if !reflect.DeepEqual(loaded.OwnerID, seed.OwnerID) {
		t.Fatalf("OwnerID = %v, want %v", loaded.OwnerID, seed.OwnerID)
	}
	if !reflect.DeepEqual(loaded.PrimaryVersionID, seed.PrimaryVersionID) {
		t.Fatalf("PrimaryVersionID = %v, want %v", loaded.PrimaryVersionID, seed.PrimaryVersionID)
	}
	if loaded.PlaylistOwnerUserID != seed.PlaylistOwnerUserID {
		t.Fatalf("PlaylistOwnerUserID = %v, want %v", loaded.PlaylistOwnerUserID, seed.PlaylistOwnerUserID)
	}
	if loaded.IsFolder != seed.IsFolder {
		t.Fatalf("IsFolder = %v, want %v", loaded.IsFolder, seed.IsFolder)
	}
	if loaded.IsVirtualItem != seed.IsVirtualItem {
		t.Fatalf("IsVirtualItem = %v, want %v", loaded.IsVirtualItem, seed.IsVirtualItem)
	}
	if loaded.IsInMixedFolder != seed.IsInMixedFolder {
		t.Fatalf("IsInMixedFolder = %v, want %v", loaded.IsInMixedFolder, seed.IsInMixedFolder)
	}
	if loaded.SourceType != seed.SourceType {
		t.Fatalf("SourceType = %v, want %v", loaded.SourceType, seed.SourceType)
	}
	if !reflect.DeepEqual(loaded.MediaType, seed.MediaType) {
		t.Fatalf("MediaType = %v, want %v", loaded.MediaType, seed.MediaType)
	}
	if loaded.OpenAccess != seed.OpenAccess {
		t.Fatalf("OpenAccess = %v, want %v", loaded.OpenAccess, seed.OpenAccess)
	}

	// Assert the cassette overview replaced the stored value.
	wantOverview := "A computer hacker learns from mysterious rebels.\nHe joins the fight."
	if loaded.Overview == nil || *loaded.Overview != wantOverview {
		t.Fatalf("Overview = %v, want the cassette value", loaded.Overview)
	}

	// Assert the cassette OriginalLanguage enriched the stored row.
	if loaded.OriginalLanguage == nil || *loaded.OriginalLanguage != "en" {
		t.Fatalf("OriginalLanguage = %v, want en — the cassette value must enrich the stored row", loaded.OriginalLanguage)
	}

	// Assert probe-derived fields are preserved (the cassette omits them).
	if loaded.RunTimeTicks == nil || *loaded.RunTimeTicks != 98765432100 {
		t.Fatalf("RunTimeTicks = %v, want 98765432100 — the backfill must preserve an omitted probe field", loaded.RunTimeTicks)
	}

	// Assert the cassette non-zero rating replaced the stored value.
	if loaded.CommunityRating == nil || *loaded.CommunityRating != 8.2 {
		t.Fatalf("CommunityRating = %v, want 8.2 — the cassette non-zero value must replace the stored rating", loaded.CommunityRating)
	}

	// Assert the cassette taxonomy values replaced the stored values.
	if !reflect.DeepEqual(loaded.Values.Genres, []string{"Action", "Science Fiction"}) {
		t.Fatalf("Genres = %v, want [Action, Science Fiction] — the cassette values must replace the stored values", loaded.Values.Genres)
	}
	if !reflect.DeepEqual(loaded.Values.Studios, []string{"Warner Bros. Pictures", "Village Roadshow Pictures"}) {
		t.Fatalf("Studios = %v, want [Warner Bros. Pictures, Village Roadshow Pictures] — the cassette values must replace the stored values", loaded.Values.Studios)
	}
	if !reflect.DeepEqual(loaded.Values.Tags, []string{"artificial reality", "dystopia"}) {
		t.Fatalf("Tags = %v, want [artificial reality, dystopia] — the cassette values must replace the stored values", loaded.Values.Tags)
	}

	// Assert the cassette people replaced the stored people with complete
	// slice equality, including order and every field.
	neoRole := "Neo"
	morpheusRole := "Morpheus"
	trinityRole := "Trinity"
	directorRole := "Director"
	writerRole := "Writer"
	zero := 0
	one := 1
	two := 2
	wantPeople := []item.PersonCredit{
		{PersonID: library.PersonEntity("Keanu Reeves").ID, Name: "Keanu Reeves", Role: &neoRole, Type: item.PersonKindActor, SortOrder: &zero},
		{PersonID: library.PersonEntity("Laurence Fishburne").ID, Name: "Laurence Fishburne", Role: &morpheusRole, Type: item.PersonKindActor, SortOrder: &one},
		{PersonID: library.PersonEntity("Carrie-Anne Moss").ID, Name: "Carrie-Anne Moss", Role: &trinityRole, Type: item.PersonKindActor, SortOrder: &two},
		{PersonID: library.PersonEntity("Lana Wachowski").ID, Name: "Lana Wachowski", Role: &directorRole, Type: item.PersonKindDirector},
		{PersonID: library.PersonEntity("Lilly Wachowski").ID, Name: "Lilly Wachowski", Role: &writerRole, Type: item.PersonKindWriter},
	}
	if !reflect.DeepEqual(loaded.People, wantPeople) {
		t.Fatalf("People = %#v, want exact slice %#v — the cassette people must replace the stored people with complete equality", loaded.People, wantPeople)
	}

	// Assert locked fields are preserved, including the global lock flag.
	if loaded.IsLocked != seed.IsLocked {
		t.Fatalf("IsLocked = %v, want %v", loaded.IsLocked, seed.IsLocked)
	}
	if !reflect.DeepEqual(loaded.LockedFields, seed.LockedFields) {
		t.Fatalf("LockedFields = %v, want %v", loaded.LockedFields, seed.LockedFields)
	}

	// Assert the locked Name stayed from the stored row despite the cassette
	// title.
	if loaded.Name == nil || *loaded.Name != "seed movie" {
		t.Fatalf("Name = %v, want the locked seed value", loaded.Name)
	}

	// Assert provider IDs merge per key with an exact snapshot. The TMDb id
	// stays from the cassette, the IMDB id is added, the TmdbCollection id is
	// added, and the unrelated Tvdb id survives.
	wantProviderIDs := providerIDs("Tmdb", "603", "Imdb", "tt0133093", "TmdbCollection", "2344", "Tvdb", "12345")
	if !providerIDsEqual(loaded.ProviderIDs, wantProviderIDs) {
		t.Fatalf("ProviderIDs = %#v, want exact set %#v — a non-empty remote value must replace the same key and an unrelated provider ID must survive", loaded.ProviderIDs.Snapshot(), wantProviderIDs.Snapshot())
	}

	// Assert images merge by (Type, Index) with an exact set. The cassette
	// replaces the Primary and Backdrop, adds the Thumb and Logo, and the
	// unrelated Banner survives.
	wantImages := []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 0, Path: "https://image.tmdb.org/t/p/original/best-poster.jpg"},
		{Type: item.ImageTypeBackdrop, Index: 0, Path: "https://image.tmdb.org/t/p/original/plain-backdrop.jpg"},
		{Type: item.ImageTypeThumb, Index: 0, Path: "https://image.tmdb.org/t/p/original/text-backdrop.jpg"},
		{Type: item.ImageTypeLogo, Index: 0, Path: "https://image.tmdb.org/t/p/original/logo.png"},
		{Type: item.ImageTypeBanner, Index: 0, Path: "/movies/the-matrix-banner.jpg"},
	}
	if !imageSetEqual(loaded.ImageInfos, wantImages) {
		t.Fatalf("ImageInfos = %#v, want exact set %#v — the backfill must replace only the same-key image and preserve every unrelated image", loaded.ImageInfos, wantImages)
	}

	// Assert the untouched satellite family (trailer types) is preserved.
	if !reflect.DeepEqual(loaded.TrailerTypes, seed.TrailerTypes) {
		t.Fatalf("TrailerTypes = %v, want %v — an untouched satellite family must survive the backfill", loaded.TrailerTypes, seed.TrailerTypes)
	}

	// Assert linked children are preserved.
	if !reflect.DeepEqual(loaded.LinkedChildren, seed.LinkedChildren) {
		t.Fatalf("LinkedChildren = %v, want %v", loaded.LinkedChildren, seed.LinkedChildren)
	}

	// Assert additional parts are preserved.
	if !reflect.DeepEqual(loaded.AdditionalParts, seed.AdditionalParts) {
		t.Fatalf("AdditionalParts = %v, want %v", loaded.AdditionalParts, seed.AdditionalParts)
	}

	// Assert the cassette Extra fields replaced or enriched the stored Extra.
	if loaded.Extra.Movie == nil || loaded.Extra.Movie.TmdbCollectionName == nil || *loaded.Extra.Movie.TmdbCollectionName != "The Matrix Collection" {
		t.Fatalf("TmdbCollectionName = %v, want \"The Matrix Collection\" — the cassette value must enrich the stored row", loaded.Extra.Movie)
	}
	if len(loaded.Extra.RemoteTrailers) != 1 || loaded.Extra.RemoteTrailers[0].URL == nil || *loaded.Extra.RemoteTrailers[0].URL != "https://www.youtube.com/watch?v=vKQi3bBA1y8" {
		t.Fatalf("RemoteTrailers = %#v, want one YouTube trailer from the cassette", loaded.Extra.RemoteTrailers)
	}
	// Assert the unrelated stored Extra field survived the merge.
	if loaded.Extra.Video == nil || loaded.Extra.Video.AspectRatio == nil || *loaded.Extra.Video.AspectRatio != "16:9" {
		t.Fatalf("Video.AspectRatio = %v, want \"16:9\" — an unrelated stored Extra field must survive the backfill", loaded.Extra.Video)
	}

	// Assert user data is preserved with complete equality — the backfill
	// must not clear any UserData field, including the identity keys.
	var userData store.UserData
	if _, err := items.MutateUserDataForItem(t.Context(), userID, seed.ID, func(data *store.UserData) error {
		userData = *data
		return nil
	}); err != nil {
		t.Fatalf("read user data after backfill: %v", err)
	}
	if userData.UserID != userID {
		t.Fatalf("user data UserID = %v, want %v — the backfill must preserve the user data identity", userData.UserID, userID)
	}
	if userData.ItemID != seed.ID {
		t.Fatalf("user data ItemID = %v, want %v — the backfill must preserve the user data identity", userData.ItemID, seed.ID)
	}
	// The backfill preserves the user data under its seeded canonical key. The
	// key is the Tmdb id "603" because the item carried that provider id at
	// seed time and the backfill does not add new user data key rows.
	if userData.CustomKey != "603" {
		t.Fatalf("user data CustomKey = %q, want 603 — the backfill must preserve the seeded canonical key", userData.CustomKey)
	}
	if !userData.Played || userData.PlayCount != 3 || !userData.IsFavorite {
		t.Fatalf("user data = %#v, want Played=true, PlayCount=3, IsFavorite=true — the backfill must preserve user data", userData)
	}
	if userData.Rating == nil || *userData.Rating != seedRating {
		t.Fatalf("user data Rating = %v, want %v — the backfill must preserve the complete user data", userData.Rating, seedRating)
	}
	if userData.Likes == nil || *userData.Likes != seedLikes {
		t.Fatalf("user data Likes = %v, want %v — the backfill must preserve the complete user data", userData.Likes, seedLikes)
	}
	if userData.PositionTicks != seedPositionTicks {
		t.Fatalf("user data PositionTicks = %d, want %d — the backfill must preserve the complete user data", userData.PositionTicks, seedPositionTicks)
	}
	if userData.LastPlayed == nil || !userData.LastPlayed.Equal(seedLastPlayed) {
		t.Fatalf("user data LastPlayed = %v, want %v — the backfill must preserve the complete user data", userData.LastPlayed, seedLastPlayed)
	}
	if userData.AudioStreamIndex == nil || *userData.AudioStreamIndex != seedAudioIndex {
		t.Fatalf("user data AudioStreamIndex = %v, want %v — the backfill must preserve the complete user data", userData.AudioStreamIndex, seedAudioIndex)
	}
	if userData.SubtitleStreamIndex == nil || *userData.SubtitleStreamIndex != seedSubtitleIndex {
		t.Fatalf("user data SubtitleStreamIndex = %v, want %v — the backfill must preserve the complete user data", userData.SubtitleStreamIndex, seedSubtitleIndex)
	}

	// Assert DateLastSaved advanced.
	if loaded.DateLastSaved == nil || !loaded.DateLastSaved.Equal(backfillSaved) {
		t.Fatalf("DateLastSaved = %v, want %v", loaded.DateLastSaved, backfillSaved)
	}
}

// TestItemStoreMetadataBackfillSkipsUnchangedSQLite proves the metadata-backfill
// operation skips an item whose merged form is identical to the stored row
// when ForceRefresh is false.
func TestItemStoreMetadataBackfillSkipsUnchangedSQLite(t *testing.T) {
	runItemStoreMetadataBackfillSkipsUnchanged(t, openItemStore)
}

// TestItemStoreMetadataBackfillSkipsUnchangedPostgres proves the
// metadata-backfill operation skips an item whose merged form is identical to
// the stored row when ForceRefresh is false.
func TestItemStoreMetadataBackfillSkipsUnchangedPostgres(t *testing.T) {
	runItemStoreMetadataBackfillSkipsUnchanged(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillSkipsUnchanged(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 11950, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	seed := reconciliationChild(t, 11951, item.KindMovie, parent.ID, "unchanged movie")
	seed.Overview = stringPtr("A stored overview")
	seedSaved := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// A remote patch that omits all fields. The merged form is identical to
	// the stored row, so the backfill must skip it.
	remote := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: seed.ParentID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	backfillSaved := seedSaved.Add(time.Hour)
	result, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, backfillSaved, store.MetadataBackfillOptions{ForceRefresh: false})
	if err != nil {
		t.Fatalf("metadata backfill: %v", err)
	}
	assertReconcileResult(t, result, nil, nil, nil)

	loaded, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	if loaded.DateLastSaved == nil || !loaded.DateLastSaved.Equal(seedSaved) {
		t.Fatalf("DateLastSaved = %v, want unchanged %v — a no-op backfill must not advance DateLastSaved", loaded.DateLastSaved, seedSaved)
	}
}

// TestItemStoreMetadataBackfillRejectsAbsentIDSQLite proves the
// metadata-backfill store operation rejects an absent item ID with a
// structural error and makes no stored change.
func TestItemStoreMetadataBackfillRejectsAbsentIDSQLite(t *testing.T) {
	runItemStoreMetadataBackfillRejectsAbsentID(t, openItemStore)
}

// TestItemStoreMetadataBackfillRejectsAbsentIDPostgres proves the
// metadata-backfill store operation rejects an absent item ID with a
// structural error and makes no stored change.
func TestItemStoreMetadataBackfillRejectsAbsentIDPostgres(t *testing.T) {
	runItemStoreMetadataBackfillRejectsAbsentID(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillRejectsAbsentID(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 12000, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	absent := reconciliationChild(t, 12001, item.KindMovie, parent.ID, "absent movie")
	if err := items.Upsert(t.Context(), absent); err != nil {
		t.Fatalf("seed absent movie so it exists in the store: %v", err)
	}

	// Delete the item so it is absent from the store.
	if err := items.Delete(t.Context(), absent.ID); err != nil {
		t.Fatalf("delete movie to make it absent: %v", err)
	}

	remote := item.Item{
		ID:       absent.ID,
		Kind:     absent.Kind,
		ParentID: absent.ParentID,
		Path:     absent.Path,
		Name:     absent.Name,
	}
	savedAt := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	_, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, savedAt, store.MetadataBackfillOptions{ForceRefresh: true})
	if err == nil {
		t.Fatalf("metadata backfill with an absent ID returned nil; want a structural error — the backfill is enrichment-only")
	}

	// The rejected backfill must not insert the absent item. Load the absent
	// id and require a not-found error.
	if _, err := items.Get(t.Context(), absent.ID); !store.IsNotFound(err) {
		t.Fatalf("Get absent id after rejected backfill = %v, want store.IsNotFound — the backfill must not insert an absent item", err)
	}
}

// TestItemStoreMetadataBackfillRejectsStructuralChangeSQLite proves the
// metadata-backfill store operation rejects a mismatched Path, ParentID, or
// Kind with a structural error and makes no stored change.
func TestItemStoreMetadataBackfillRejectsStructuralChangeSQLite(t *testing.T) {
	runItemStoreMetadataBackfillRejectsStructuralChange(t, openItemStore)
}

// TestItemStoreMetadataBackfillRejectsStructuralChangePostgres proves the
// metadata-backfill store operation rejects a mismatched Path, ParentID, or
// Kind with a structural error and makes no stored change.
func TestItemStoreMetadataBackfillRejectsStructuralChangePostgres(t *testing.T) {
	runItemStoreMetadataBackfillRejectsStructuralChange(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillRejectsStructuralChange(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 12100, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	seed := reconciliationChild(t, 12101, item.KindMovie, parent.ID, "structural movie")
	seed.Path = stringPtr("/movies/structural.mkv")
	seed.Overview = stringPtr("A stored overview")
	seedSaved := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// Seed a Person virtual with stored metadata. The structural rejection
	// must leave this virtual row unchanged — the pre-write validation pass
	// returns the error before backfillVirtuals opens its transaction.
	person := hierarchyItem(t, 12110, item.KindPerson, nil)
	person.Name = stringPtr("Lana Wachowski")
	person.Overview = stringPtr("A director biography")
	person.ProviderIDs = providerIDs("Imdb", "nm0001")
	if err := items.Upsert(t.Context(), person); err != nil {
		t.Fatalf("seed Person virtual: %v", err)
	}

	// Seed a later item with a distinct ID. The later-item mismatch case
	// submits a valid earlier item and a structurally mismatched later
	// item, so the later item needs its own stored row. Capture the seed
	// snapshots for the earlier item, the later item, and the virtual row
	// after the store populates structural fields, so the later-item
	// rejection comparison is complete, not a selected-field check.
	later := reconciliationChild(t, 12120, item.KindMovie, parent.ID, "later movie")
	later.Path = stringPtr("/movies/later.mkv")
	later.Overview = stringPtr("A later stored overview")
	laterSaved := time.Date(2026, time.August, 27, 11, 0, 0, 0, time.UTC)
	later.DateLastSaved = &laterSaved
	if err := items.Upsert(t.Context(), later); err != nil {
		t.Fatalf("seed later movie: %v", err)
	}
	earlierSeedSnapshot, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("snapshot earlier item: %v", err)
	}
	laterSeedSnapshot, err := items.Get(t.Context(), later.ID)
	if err != nil {
		t.Fatalf("snapshot later item: %v", err)
	}
	virtualSeedSnapshot, err := items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("snapshot Person virtual: %v", err)
	}

	// The virtual candidate carries a changed Overview. A faulty ordering
	// that writes the virtual row before the item validation persists this
	// change, so the candidate proves that the rejection leaves no virtual
	// data committed.
	sparsePerson := item.Item{
		ID:   person.ID,
		Kind: person.Kind,
		Path: person.Path,
		Name: person.Name,
	}
	sparsePerson.Overview = stringPtr("A changed virtual overview")

	savedAt := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)

	// Mismatched Path.
	mismatchedPath := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: seed.ParentID,
		Path:     stringPtr("/movies/different-path.mkv"),
		Name:     seed.Name,
	}
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{mismatchedPath}, []item.Item{sparsePerson}, savedAt, store.MetadataBackfillOptions{ForceRefresh: true}); err == nil {
		t.Fatalf("metadata backfill with mismatched Path returned nil; want a structural error")
	}
	assertVirtualUnchanged(t, items, person, "mismatched Path rejection")

	// Mismatched Kind.
	mismatchedKind := item.Item{
		ID:       seed.ID,
		Kind:     item.KindSeries,
		ParentID: seed.ParentID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{mismatchedKind}, []item.Item{sparsePerson}, savedAt, store.MetadataBackfillOptions{ForceRefresh: true}); err == nil {
		t.Fatalf("metadata backfill with mismatched Kind returned nil; want a structural error")
	}
	assertVirtualUnchanged(t, items, person, "mismatched Kind rejection")

	// Mismatched ParentID.
	otherParent := hierarchyItem(t, 12102, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), otherParent); err != nil {
		t.Fatalf("upsert other parent: %v", err)
	}
	mismatchedParent := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: &otherParent.ID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{mismatchedParent}, []item.Item{sparsePerson}, savedAt, store.MetadataBackfillOptions{ForceRefresh: true}); err == nil {
		t.Fatalf("metadata backfill with mismatched ParentID returned nil; want a structural error")
	}
	assertVirtualUnchanged(t, items, person, "mismatched ParentID rejection")

	// A structural mismatch on a later item must not leave earlier virtual
	// rows or earlier item changes committed. Submit a valid earlier item
	// followed by a later item with a structural mismatch. The earlier and
	// later items have distinct IDs. The pre-write validation pass must
	// reject the batch before any write. The earlier item must keep every
	// seed field, and the virtual row must keep every seed field. No item or
	// virtual data persists.
	validFirst := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: seed.ParentID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	validFirst.Overview = stringPtr("A valid remote overview")
	mismatchedLater := item.Item{
		ID:       later.ID,
		Kind:     later.Kind,
		ParentID: later.ParentID,
		Path:     stringPtr("/movies/different-later-path.mkv"),
		Name:     later.Name,
	}
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{validFirst, mismatchedLater}, []item.Item{sparsePerson}, savedAt, store.MetadataBackfillOptions{ForceRefresh: true}); err == nil {
		t.Fatalf("metadata backfill with a mismatched later item returned nil; want a structural error")
	}

	// Assert the complete earlier item matches its seed snapshot. The valid
	// first item carried a changed Overview, but the pre-write validation
	// pass rejected the batch before any item transaction opened, so every
	// stored field must stay from the seed.
	loadedEarlier, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get earlier item after later-item rejection: %v", err)
	}
	if !reflect.DeepEqual(loadedEarlier, earlierSeedSnapshot) {
		t.Fatalf("earlier item = %#v after later-item rejection, want the complete seed snapshot %#v — a later structural rejection must leave the earlier item unchanged", loadedEarlier, earlierSeedSnapshot)
	}

	// Assert the complete later item matches its seed snapshot. The later
	// item carried a mismatched Path, so the rejection must leave it
	// unchanged too.
	loadedLater, err := items.Get(t.Context(), later.ID)
	if err != nil {
		t.Fatalf("get later item after later-item rejection: %v", err)
	}
	if !reflect.DeepEqual(loadedLater, laterSeedSnapshot) {
		t.Fatalf("later item = %#v after later-item rejection, want the complete seed snapshot %#v — a later structural rejection must leave the later item unchanged", loadedLater, laterSeedSnapshot)
	}

	// Assert the complete virtual row matches its seed snapshot. The
	// virtual candidate carried a changed Overview, but the pre-write
	// validation pass rejected the batch before backfillVirtuals opened its
	// transaction, so every stored field must stay from the seed.
	loadedVirtual, err := items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("get Person virtual after later-item rejection: %v", err)
	}
	if !reflect.DeepEqual(loadedVirtual, virtualSeedSnapshot) {
		t.Fatalf("Person virtual = %#v after later-item rejection, want the complete seed snapshot %#v — a later structural rejection must leave the virtual row unchanged", loadedVirtual, virtualSeedSnapshot)
	}
}

// assertVirtualUnchanged loads the Person virtual and asserts its stored
// metadata, provider ID, and identity survived a structural rejection.
func assertVirtualUnchanged(t *testing.T, items *store.ItemStore, person item.Item, label string) {
	t.Helper()
	loaded, err := items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("get Person virtual after %s: %v", label, err)
	}
	if loaded.Overview == nil || *loaded.Overview != "A director biography" {
		t.Fatalf("Person virtual Overview = %v after %s, want the stored value — a structural rejection must leave virtual rows unchanged", loaded.Overview, label)
	}
	if id, ok := loaded.ProviderIDs.Get("Imdb"); !ok || id != "nm0001" {
		t.Fatalf("Person virtual Imdb id = %q,%t after %s, want nm0001,true — a structural rejection must leave virtual rows unchanged", id, ok, label)
	}
	if !reflect.DeepEqual(loaded.Name, person.Name) {
		t.Fatalf("Person virtual Name = %v after %s, want %v", loaded.Name, label, person.Name)
	}
}

// TestItemStoreMetadataBackfillRejectsVirtualStructuralChangeSQLite proves
// the metadata-backfill operation rejects a virtual structural mismatch before
// any write. No virtual row and no item row is committed.
func TestItemStoreMetadataBackfillRejectsVirtualStructuralChangeSQLite(t *testing.T) {
	runItemStoreMetadataBackfillRejectsVirtualStructuralChange(t, openItemStore)
}

// TestItemStoreMetadataBackfillRejectsVirtualStructuralChangePostgres proves
// the same virtual structural rejection contract against PostgreSQL.
func TestItemStoreMetadataBackfillRejectsVirtualStructuralChangePostgres(t *testing.T) {
	runItemStoreMetadataBackfillRejectsVirtualStructuralChange(t, openPostgresItemStore)
}

// runItemStoreMetadataBackfillRejectsVirtualStructuralChange seeds a Person
// virtual and a movie item. It runs MetadataBackfill with a virtual that has a
// mismatched Kind and a valid item. The pre-write validation pass must reject
// the virtual structural change before backfillVirtuals opens its
// transaction. The Person virtual and the movie item must stay unchanged.
func runItemStoreMetadataBackfillRejectsVirtualStructuralChange(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 12200, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	movie := reconciliationChild(t, 12201, item.KindMovie, parent.ID, "structural movie")
	movie.Overview = stringPtr("A stored overview")
	movieSaved := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	movie.DateLastSaved = &movieSaved
	if err := items.Upsert(t.Context(), movie); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	person := hierarchyItem(t, 12210, item.KindPerson, nil)
	person.Name = stringPtr("Lana Wachowski")
	person.Overview = stringPtr("A director biography")
	person.ProviderIDs = providerIDs("Imdb", "nm0001")
	if err := items.Upsert(t.Context(), person); err != nil {
		t.Fatalf("seed Person virtual: %v", err)
	}

	// A valid remote item that would update the movie overview.
	remoteMovie := item.Item{
		ID:       movie.ID,
		Kind:     movie.Kind,
		ParentID: movie.ParentID,
		Path:     movie.Path,
		Name:     movie.Name,
	}
	remoteMovie.Overview = stringPtr("A fresh remote overview")

	// A virtual with a mismatched Kind. The pre-write validation pass must
	// reject this before backfillVirtuals commits.
	mismatchedVirtual := item.Item{
		ID:   person.ID,
		Kind: item.KindFolder,
		Path: person.Path,
		Name: person.Name,
	}

	savedAt := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	_, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remoteMovie}, []item.Item{mismatchedVirtual}, savedAt, store.MetadataBackfillOptions{ForceRefresh: true})
	if err == nil {
		t.Fatalf("metadata backfill with a virtual structural mismatch returned nil; want a structural error")
	}

	// The Person virtual must stay unchanged.
	personLoaded, err := items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("get Person virtual: %v", err)
	}
	if personLoaded.Kind != item.KindPerson {
		t.Fatalf("Person virtual Kind = %s, want Person — a virtual structural rejection must leave the row unchanged", personLoaded.Kind)
	}
	if personLoaded.Overview == nil || *personLoaded.Overview != "A director biography" {
		t.Fatalf("Person virtual Overview = %v, want the stored value — a virtual structural rejection must leave the row unchanged", personLoaded.Overview)
	}
	if id, ok := personLoaded.ProviderIDs.Get("Imdb"); !ok || id != "nm0001" {
		t.Fatalf("Person virtual Imdb id = %q,%t, want nm0001,true — a virtual structural rejection must leave the row unchanged", id, ok)
	}

	// The movie item must also stay unchanged because the validation pass
	// returned the error before any item transaction opened.
	movieLoaded, err := items.Get(t.Context(), movie.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	if movieLoaded.Overview == nil || *movieLoaded.Overview != "A stored overview" {
		t.Fatalf("movie Overview = %v, want the stored value — a virtual structural rejection must leave all rows unchanged", movieLoaded.Overview)
	}
}

// TestItemStoreMetadataBackfillPreservesEmptyMetadataSQLite proves the
// metadata-backfill store operation preserves stored taxonomy values when the
// remote patch supplies empty (non-nil) slices for Genres, Studios, Tags, and
// People.
func TestItemStoreMetadataBackfillPreservesEmptyMetadataSQLite(t *testing.T) {
	runItemStoreMetadataBackfillPreservesEmptyMetadata(t, openItemStore)
}

// TestItemStoreMetadataBackfillPreservesEmptyMetadataPostgres proves the
// metadata-backfill store operation preserves stored taxonomy values when the
// remote patch supplies empty (non-nil) slices for Genres, Studios, Tags, and
// People.
func TestItemStoreMetadataBackfillPreservesEmptyMetadataPostgres(t *testing.T) {
	runItemStoreMetadataBackfillPreservesEmptyMetadata(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillPreservesEmptyMetadata(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 12200, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	seed := reconciliationChild(t, 12201, item.KindMovie, parent.ID, "empty-metadata movie")
	seed.Overview = stringPtr("A stored overview")
	seed.Values.Genres = []string{"Action", "Sci-Fi"}
	seed.Values.Studios = []string{"Warner Bros"}
	seed.Values.Tags = []string{"4k", "hdr"}
	seed.People = []item.PersonCredit{
		{PersonID: contractUUID(t, 12210), Name: "Lana Wachowski", Type: item.PersonKindDirector},
	}
	seedSaved := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// Apply a remote patch with empty (non-nil) slices for Genres, Studios,
	// Tags, and People. The stored values must survive.
	remote := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: seed.ParentID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	remote.Values.Genres = []string{}
	remote.Values.Studios = []string{}
	remote.Values.Tags = []string{}
	remote.People = []item.PersonCredit{}
	backfillSaved := seedSaved.Add(time.Hour)
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, backfillSaved, store.MetadataBackfillOptions{ForceRefresh: true}); err != nil {
		t.Fatalf("metadata backfill with empty metadata: %v", err)
	}

	loaded, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	if !reflect.DeepEqual(loaded.Values.Genres, []string{"Action", "Sci-Fi"}) {
		t.Fatalf("Genres = %v, want [Action, Sci-Fi] — an empty remote slice must not replace stored values", loaded.Values.Genres)
	}
	if !reflect.DeepEqual(loaded.Values.Studios, []string{"Warner Bros"}) {
		t.Fatalf("Studios = %v, want [Warner Bros] — an empty remote slice must not replace stored values", loaded.Values.Studios)
	}
	if !reflect.DeepEqual(loaded.Values.Tags, []string{"4k", "hdr"}) {
		t.Fatalf("Tags = %v, want [4k, hdr] — an empty remote slice must not replace stored values", loaded.Values.Tags)
	}
	if !reflect.DeepEqual(loaded.People, seed.People) {
		t.Fatalf("People = %#v, want %#v — an empty remote slice must not replace stored people", loaded.People, seed.People)
	}
}

// TestItemStoreMetadataBackfillGlobalLockProtectsTaxonomySQLite proves the
// metadata-backfill store operation preserves taxonomy values when IsLocked is
// true on the stored item, even if LockedFields does not list the field.
func TestItemStoreMetadataBackfillGlobalLockProtectsTaxonomySQLite(t *testing.T) {
	runItemStoreMetadataBackfillGlobalLockProtectsTaxonomy(t, openItemStore)
}

// TestItemStoreMetadataBackfillGlobalLockProtectsTaxonomyPostgres proves the
// metadata-backfill store operation preserves taxonomy values when IsLocked is
// true on the stored item, even if LockedFields does not list the field.
func TestItemStoreMetadataBackfillGlobalLockProtectsTaxonomyPostgres(t *testing.T) {
	runItemStoreMetadataBackfillGlobalLockProtectsTaxonomy(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillGlobalLockProtectsTaxonomy(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 12300, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	seed := reconciliationChild(t, 12301, item.KindMovie, parent.ID, "globally-locked movie")
	seed.Overview = stringPtr("A locked overview")
	seed.Values.Genres = []string{"Action", "Sci-Fi"}
	seed.Values.Studios = []string{"Warner Bros"}
	seed.Values.Tags = []string{"4k", "hdr"}
	seed.People = []item.PersonCredit{
		{PersonID: contractUUID(t, 12310), Name: "Lana Wachowski", Type: item.PersonKindDirector},
	}
	seed.IsLocked = true
	seed.LockedFields = nil
	seedSaved := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// Apply a remote patch with different taxonomy values. The globally
	// locked item must preserve every locked value.
	remote := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: seed.ParentID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	remote.Overview = stringPtr("A remote overview that must not replace the locked value")
	remote.Values.Genres = []string{"Comedy"}
	remote.Values.Studios = []string{"MGM"}
	remote.Values.Tags = []string{"sd"}
	remote.People = []item.PersonCredit{
		{PersonID: contractUUID(t, 12311), Name: "Someone Else", Type: item.PersonKindDirector},
	}
	backfillSaved := seedSaved.Add(time.Hour)
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, backfillSaved, store.MetadataBackfillOptions{ForceRefresh: true}); err != nil {
		t.Fatalf("metadata backfill with global lock: %v", err)
	}

	loaded, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	if loaded.Overview == nil || *loaded.Overview != "A locked overview" {
		t.Fatalf("Overview = %v, want the locked value — IsLocked must protect taxonomy", loaded.Overview)
	}
	if !reflect.DeepEqual(loaded.Values.Genres, []string{"Action", "Sci-Fi"}) {
		t.Fatalf("Genres = %v, want [Action, Sci-Fi] — IsLocked must protect taxonomy", loaded.Values.Genres)
	}
	if !reflect.DeepEqual(loaded.Values.Studios, []string{"Warner Bros"}) {
		t.Fatalf("Studios = %v, want [Warner Bros] — IsLocked must protect taxonomy", loaded.Values.Studios)
	}
	if !reflect.DeepEqual(loaded.Values.Tags, []string{"4k", "hdr"}) {
		t.Fatalf("Tags = %v, want [4k, hdr] — IsLocked must protect taxonomy", loaded.Values.Tags)
	}
	if !reflect.DeepEqual(loaded.People, seed.People) {
		t.Fatalf("People = %#v, want %#v — IsLocked must protect the complete people slice", loaded.People, seed.People)
	}
}

// TestItemStoreMetadataBackfillGlobalLockPreservesProviderFieldsSQLite proves
// the global IsLocked flag preserves provider-owned fields that have no
// individual MetadataField lock entry: ratings, dates, indexes, provider IDs,
// images, Extra, OriginalLanguage, and SeriesStatus. When ForceRefresh is
// false the backfill must write no update at all, because the merged item
// equals the stored row.
func TestItemStoreMetadataBackfillGlobalLockPreservesProviderFieldsSQLite(t *testing.T) {
	runItemStoreMetadataBackfillGlobalLockPreservesProviderFields(t, openItemStore)
}

// TestItemStoreMetadataBackfillGlobalLockPreservesProviderFieldsPostgres proves
// the global IsLocked flag preserves provider-owned fields that have no
// individual MetadataField lock entry: ratings, dates, indexes, provider IDs,
// images, Extra, OriginalLanguage, and SeriesStatus.
func TestItemStoreMetadataBackfillGlobalLockPreservesProviderFieldsPostgres(t *testing.T) {
	runItemStoreMetadataBackfillGlobalLockPreservesProviderFields(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillGlobalLockPreservesProviderFields(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 12400, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	seed := reconciliationChild(t, 12401, item.KindMovie, parent.ID, "globally-locked movie")
	seed.ProductionYear = intPtr(1999)
	seed.IndexNumber = intPtr(1)
	seed.ParentIndexNumber = intPtr(0)
	seed.CommunityRating = float32Ptr(8.7)
	seed.CriticRating = float32Ptr(9.0)
	premiere := time.Date(1999, time.March, 31, 0, 0, 0, 0, time.UTC)
	seed.PremiereDate = &premiere
	startDate := time.Date(1999, time.January, 1, 0, 0, 0, 0, time.UTC)
	seed.StartDate = &startDate
	seed.ProviderIDs = providerIDs("Tmdb", "603", "Imdb", "tt0133093")
	seed.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 0, Path: "https://image.tmdb.org/t/p/original/locked-poster.jpg"},
		{Type: item.ImageTypeBackdrop, Index: 0, Path: "https://image.tmdb.org/t/p/original/locked-backdrop.jpg"},
	}
	seed.Extra.Movie = &item.MovieExtra{TmdbCollectionName: stringPtr("The Matrix Collection")}
	// Seed provider-owned fields the remote stage writes: OriginalLanguage
	// (provider.go:174), SeriesStatus (provider_series.go:125), and Album
	// (TheAudioDb). The global IsLocked flag must protect all three.
	seed.OriginalLanguage = stringPtr("en")
	seed.Album = stringPtr("The Matrix OST")
	seedSeriesStatus := item.SeriesStatusEnded
	seed.SeriesStatus = &seedSeriesStatus
	seed.Extra.RemoteTrailers = []item.MediaURL{
		{URL: stringPtr("https://example.com/locked-trailer.mp4"), Name: stringPtr("Locked Trailer")},
	}
	seed.IsLocked = true
	seed.LockedFields = nil
	seedSaved := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// Seed user data so the test proves the backfill preserves it.
	userID := contractUUID(t, 12430)
	if _, err := items.MutateUserDataForItem(t.Context(), userID, seed.ID, func(data *store.UserData) error {
		data.Played = true
		data.PlayCount = 3
		data.IsFavorite = true
		data.Rating = float32Ptr(5.0)
		data.PositionTicks = 12345678
		return nil
	}); err != nil {
		t.Fatalf("seed user data: %v", err)
	}

	// Apply a remote patch with different provider-owned values. The globally
	// locked item must preserve every stored value and write no update.
	remote := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: seed.ParentID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	remote.ProductionYear = intPtr(2001)
	remote.IndexNumber = intPtr(2)
	remote.CommunityRating = float32Ptr(5.0)
	remote.CriticRating = float32Ptr(6.0)
	remotePremiere := time.Date(2001, time.March, 16, 0, 0, 0, 0, time.UTC)
	remote.PremiereDate = &remotePremiere
	remoteStart := time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
	remote.StartDate = &remoteStart
	remote.ProviderIDs = providerIDs("Tmdb", "999", "Tvdb", "12345")
	remote.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 0, Path: "https://image.tmdb.org/t/p/original/new-poster.jpg"},
	}
	remote.Extra.Movie = &item.MovieExtra{TmdbCollectionName: stringPtr("A Different Collection")}
	// Supply different remote values for the provider-owned fields. The
	remote.Album = stringPtr("A Different Album")
	remote.OriginalLanguage = stringPtr("ja")
	remoteSeriesStatus := item.SeriesStatusContinuing
	remote.SeriesStatus = &remoteSeriesStatus
	remote.Extra.RemoteTrailers = []item.MediaURL{
		{URL: stringPtr("https://example.com/new-trailer.mp4"), Name: stringPtr("New Trailer")},
	}
	backfillSaved := seedSaved.Add(time.Hour)
	result, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, backfillSaved, store.MetadataBackfillOptions{ForceRefresh: false})
	if err != nil {
		t.Fatalf("metadata backfill with global lock: %v", err)
	}

	// The globally locked item must produce no update when ForceRefresh is
	// false, because the merged item equals the stored row.
	assertReconcileResult(t, result, nil, nil, nil)

	loaded, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}

	// Assert every provider-owned field is preserved with exact equality.
	if loaded.ProductionYear == nil || *loaded.ProductionYear != 1999 {
		t.Fatalf("ProductionYear = %v, want 1999 — IsLocked must protect it", loaded.ProductionYear)
	}
	if loaded.IndexNumber == nil || *loaded.IndexNumber != 1 {
		t.Fatalf("IndexNumber = %v, want 1 — IsLocked must protect it", loaded.IndexNumber)
	}
	if loaded.ParentIndexNumber == nil || *loaded.ParentIndexNumber != 0 {
		t.Fatalf("ParentIndexNumber = %v, want 0 — IsLocked must protect it", loaded.ParentIndexNumber)
	}
	if loaded.CommunityRating == nil || *loaded.CommunityRating != 8.7 {
		t.Fatalf("CommunityRating = %v, want 8.7 — IsLocked must protect it", loaded.CommunityRating)
	}
	if loaded.CriticRating == nil || *loaded.CriticRating != 9.0 {
		t.Fatalf("CriticRating = %v, want 9.0 — IsLocked must protect it", loaded.CriticRating)
	}
	if loaded.PremiereDate == nil || !loaded.PremiereDate.Equal(premiere) {
		t.Fatalf("PremiereDate = %v, want %v — IsLocked must protect it", loaded.PremiereDate, premiere)
	}
	if loaded.StartDate == nil || !loaded.StartDate.Equal(startDate) {
		t.Fatalf("StartDate = %v, want %v — IsLocked must protect it", loaded.StartDate, startDate)
	}

	// Assert the locked OriginalLanguage survived the remote patch.
	if loaded.OriginalLanguage == nil || *loaded.OriginalLanguage != "en" {
		t.Fatalf("OriginalLanguage = %v, want \"en\" — IsLocked must protect the provider-owned field", loaded.OriginalLanguage)
	}
	// Assert the locked Album survived the remote patch.
	if loaded.Album == nil || *loaded.Album != "The Matrix OST" {
		t.Fatalf("Album = %v, want \"The Matrix OST\" — IsLocked must protect the provider-owned field", loaded.Album)
	}
	// Assert the locked SeriesStatus survived the remote patch.
	if loaded.SeriesStatus == nil || *loaded.SeriesStatus != item.SeriesStatusEnded {
		t.Fatalf("SeriesStatus = %v, want Ended — IsLocked must protect the provider-owned field", loaded.SeriesStatus)
	}
	if !providerIDsEqual(loaded.ProviderIDs, seed.ProviderIDs) {
		t.Fatalf("ProviderIDs = %#v, want exact seed set %#v — IsLocked must protect provider IDs", loaded.ProviderIDs.Snapshot(), seed.ProviderIDs.Snapshot())
	}
	if !imageSetEqual(loaded.ImageInfos, seed.ImageInfos) {
		t.Fatalf("ImageInfos = %#v, want exact seed set %#v — IsLocked must protect images", loaded.ImageInfos, seed.ImageInfos)
	}
	if loaded.Extra.Movie == nil || loaded.Extra.Movie.TmdbCollectionName == nil || *loaded.Extra.Movie.TmdbCollectionName != "The Matrix Collection" {
		t.Fatalf("TmdbCollectionName = %v, want \"The Matrix Collection\" — IsLocked must protect Extra", loaded.Extra.Movie)
	}
	if len(loaded.Extra.RemoteTrailers) != 1 || loaded.Extra.RemoteTrailers[0].URL == nil || *loaded.Extra.RemoteTrailers[0].URL != "https://example.com/locked-trailer.mp4" {
		t.Fatalf("RemoteTrailers = %#v, want the locked trailer — IsLocked must protect Extra", loaded.Extra.RemoteTrailers)
	}

	// Assert the stored DateLastSaved is unchanged (no write occurred).
	if loaded.DateLastSaved == nil || !loaded.DateLastSaved.Equal(seedSaved) {
		t.Fatalf("DateLastSaved = %v, want %v — no write must occur for a locked item with ForceRefresh false", loaded.DateLastSaved, seedSaved)
	}

	// Assert user data is preserved with exact equality.
	var userData store.UserData
	if _, err := items.MutateUserDataForItem(t.Context(), userID, seed.ID, func(data *store.UserData) error {
		userData = *data
		return nil
	}); err != nil {
		t.Fatalf("read user data after backfill: %v", err)
	}
	if !userData.Played || userData.PlayCount != 3 || !userData.IsFavorite || userData.Rating == nil || *userData.Rating != 5.0 || userData.PositionTicks != 12345678 {
		t.Fatalf("user data = %#v, want {Played: true, PlayCount: 3, IsFavorite: true, Rating: 5.0, PositionTicks: 12345678} — the backfill must preserve complete user data", userData)
	}
}

// TestItemStoreMetadataBackfillPerItemTransactionSQLite proves the
// metadata-backfill store operation uses per-item transactions. When the
// second of two items fails, the first item's update commits and the second
// item stays unchanged.
func TestItemStoreMetadataBackfillPerItemTransactionSQLite(t *testing.T) {
	runItemStoreMetadataBackfillPerItemTransaction(t, openItemStore)
}

// TestItemStoreMetadataBackfillPerItemTransactionPostgres proves the
// metadata-backfill store operation uses per-item transactions. When the
// second of two items fails, the first item's update commits and the second
// item stays unchanged.
func TestItemStoreMetadataBackfillPerItemTransactionPostgres(t *testing.T) {
	runItemStoreMetadataBackfillPerItemTransaction(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillPerItemTransaction(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 12400, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	// Seed two items. The first will succeed. The second will fail because
	// a concurrent writer changes its stored Path between the two item
	// transactions, so the in-transaction structural check rejects the
	// mismatch. The pre-write validation pass passes both items because
	// the stored Path still matches at validation time.
	first := reconciliationChild(t, 12401, item.KindMovie, parent.ID, "first movie")
	first.Overview = stringPtr("First stored overview")
	firstSaved := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	first.DateLastSaved = &firstSaved
	if err := items.Upsert(t.Context(), first); err != nil {
		t.Fatalf("seed first movie: %v", err)
	}

	second := reconciliationChild(t, 12402, item.KindMovie, parent.ID, "second movie")
	second.Overview = stringPtr("Second stored overview")
	second.DateLastSaved = &firstSaved
	if err := items.Upsert(t.Context(), second); err != nil {
		t.Fatalf("seed second movie: %v", err)
	}

	// Both remote patches match the stored rows at validation time. The
	// first gets a new overview. The second keeps the same Path so the
	// pre-pass accepts it.
	firstRemote := item.Item{
		ID:       first.ID,
		Kind:     first.Kind,
		ParentID: first.ParentID,
		Path:     first.Path,
		Name:     first.Name,
	}
	firstRemote.Overview = stringPtr("First fresh remote overview")

	secondRemote := item.Item{
		ID:       second.ID,
		Kind:     second.Kind,
		ParentID: second.ParentID,
		Path:     second.Path,
		Name:     second.Name,
	}

	// The BetweenItems hook runs after the first item commits and before
	// the second item's transaction loads its baseline. It changes the
	// second item's stored Path, so the in-transaction structural check
	// rejects the mismatch. This proves the per-item transaction boundary:
	// the first item stays committed and the second item rolls back.
	hook := func() error {
		current, err := items.Get(t.Context(), second.ID)
		if err != nil {
			return err
		}
		current.Path = stringPtr("/moved/by/concurrent-writer.mkv")
		return items.Upsert(t.Context(), current)
	}

	savedAt := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	_, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{firstRemote, secondRemote}, nil, savedAt, store.MetadataBackfillOptions{ForceRefresh: true, BetweenItems: hook})
	if err == nil {
		t.Fatalf("metadata backfill with a structural error in the second item returned nil; want an error")
	}

	// The first item must commit despite the second item's failure.
	firstLoaded, err := items.Get(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("get first movie: %v", err)
	}
	if firstLoaded.Overview == nil || *firstLoaded.Overview != "First fresh remote overview" {
		t.Fatalf("first Overview = %v, want the remote value — a per-item transaction must commit the first item before the second fails", firstLoaded.Overview)
	}

	// The second item must carry the concurrent writer's Path, not the
	// backfill's merge. The backfill rolled back.
	secondLoaded, err := items.Get(t.Context(), second.ID)
	if err != nil {
		t.Fatalf("get second movie: %v", err)
	}
	if secondLoaded.Overview == nil || *secondLoaded.Overview != "Second stored overview" {
		t.Fatalf("second Overview = %v, want the seed value — a per-item transaction must roll back the failed item", secondLoaded.Overview)
	}
	if secondLoaded.Path == nil || *secondLoaded.Path != "/moved/by/concurrent-writer.mkv" {
		t.Fatalf("second Path = %v, want the concurrent writer's path — the backfill must roll back and leave the concurrent change", secondLoaded.Path)
	}
}

// TestItemStoreMetadataBackfillPreservesDerivedMediaSQLite proves the
// metadata-backfill store operation does not delete keyframes or trickplay
// rows. The test seeds an item through ReconcileScannedPlan so the source
// revision is set, seeds keyframes and trickplay, then runs a backfill. The
// derived media must survive.
func TestItemStoreMetadataBackfillPreservesDerivedMediaSQLite(t *testing.T) {
	runItemStoreMetadataBackfillPreservesDerivedMedia(t, openItemStore)
}

// TestItemStoreMetadataBackfillPreservesDerivedMediaPostgres proves the
// metadata-backfill store operation does not delete keyframes or trickplay
// rows.
func TestItemStoreMetadataBackfillPreservesDerivedMediaPostgres(t *testing.T) {
	runItemStoreMetadataBackfillPreservesDerivedMedia(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillPreservesDerivedMedia(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent, source, media := revisionSourceFixture(t, 12500)
	reconcileRevisionSource(t, items, parent, source, media)

	// Seed keyframes and trickplay so the test proves the backfill does not
	// delete them.
	seedDerivedMetadata(t, items, source.ID, *source.RunTimeTicks)

	// Apply a remote backfill patch.
	remote := item.Item{
		ID:       source.ID,
		Kind:     source.Kind,
		ParentID: source.ParentID,
		Path:     source.Path,
		Name:     source.Name,
	}
	remote.Overview = stringPtr("A remote overview")
	remote.ProviderIDs = providerIDs("Tmdb", "603")
	savedAt := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, savedAt, store.MetadataBackfillOptions{ForceRefresh: true}); err != nil {
		t.Fatalf("metadata backfill: %v", err)
	}

	// Assert keyframes survived.
	if got, err := items.Keyframes(t.Context(), source.ID); err != nil {
		t.Fatalf("keyframes after backfill: %v — the backfill must not delete derived media", err)
	} else if got.TotalDurationTicks != *source.RunTimeTicks {
		t.Fatalf("keyframe total duration = %d, want %d", got.TotalDurationTicks, *source.RunTimeTicks)
	}

	// Assert trickplay survived.
	if got, err := items.Trickplays(t.Context(), source.ID); err != nil || len(got) == 0 {
		t.Fatalf("trickplay after backfill = %#v, %v — the backfill must not delete derived media", got, err)
	}
}

// int64Ptr returns a pointer to value.
func int64Ptr(value int64) *int64 { return &value }

// intPtr returns a pointer to value.
func intPtr(value int) *int { return &value }

// float32Ptr returns a pointer to value.
func float32Ptr(value float32) *float32 { return &value }

// TestItemStoreMetadataBackfillConcurrentUpdateSurvivesSQLite proves the
// metadata-backfill operation loads each item's baseline inside its own
// transaction. A BetweenItems hook applies a concurrent local update to the
// second item after the first item commits. The second item's merge must
// observe that concurrent update and keep it; a preloaded baseline would
// clobber it.
func TestItemStoreMetadataBackfillConcurrentUpdateSurvivesSQLite(t *testing.T) {
	runItemStoreMetadataBackfillConcurrentUpdateSurvives(t, openItemStore)
}

// TestItemStoreMetadataBackfillConcurrentUpdateSurvivesPostgres proves the
// same in-transaction baseline contract against PostgreSQL.
func TestItemStoreMetadataBackfillConcurrentUpdateSurvivesPostgres(t *testing.T) {
	runItemStoreMetadataBackfillConcurrentUpdateSurvives(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillConcurrentUpdateSurvives(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 12500, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}
	first := reconciliationChild(t, 12501, item.KindMovie, parent.ID, "first movie")
	first.Overview = stringPtr("First stored overview")
	firstSaved := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	first.DateLastSaved = &firstSaved
	if err := items.Upsert(t.Context(), first); err != nil {
		t.Fatalf("seed first movie: %v", err)
	}
	second := reconciliationChild(t, 12502, item.KindMovie, parent.ID, "second movie")
	second.Overview = stringPtr("Second stored overview")
	second.DateLastSaved = &firstSaved
	if err := items.Upsert(t.Context(), second); err != nil {
		t.Fatalf("seed second movie: %v", err)
	}

	firstRemote := item.Item{
		ID:       first.ID,
		Kind:     first.Kind,
		ParentID: first.ParentID,
		Path:     first.Path,
		Name:     first.Name,
	}
	firstRemote.Overview = stringPtr("First fresh remote overview")
	secondRemote := item.Item{
		ID:       second.ID,
		Kind:     second.Kind,
		ParentID: second.ParentID,
		Path:     second.Path,
		Name:     second.Name,
	}
	secondRemote.Values.Genres = []string{"Drama"}

	var hookCalls int
	concurrentOverview := "A concurrent local overview update"
	hook := func() error {
		hookCalls++
		current, err := items.Get(t.Context(), second.ID)
		if err != nil {
			return err
		}
		current.Overview = &concurrentOverview
		return items.Upsert(t.Context(), current)
	}

	savedAt := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	result, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{firstRemote, secondRemote}, nil, savedAt, store.MetadataBackfillOptions{ForceRefresh: true, BetweenItems: hook})
	if err != nil {
		t.Fatalf("metadata backfill: %v", err)
	}
	if hookCalls != 1 {
		t.Fatalf("BetweenItems hook calls = %d, want 1 — the hook runs once between the two item transactions", hookCalls)
	}
	if !reflect.DeepEqual(result.Updated, []dto.UUID{first.ID, second.ID}) {
		t.Fatalf("backfill updated = %v, want both items", result.Updated)
	}

	firstLoaded, err := items.Get(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("get first movie: %v", err)
	}
	if firstLoaded.Overview == nil || *firstLoaded.Overview != "First fresh remote overview" {
		t.Fatalf("first Overview = %v, want the remote value", firstLoaded.Overview)
	}
	if firstLoaded.DateLastSaved == nil || !firstLoaded.DateLastSaved.Equal(savedAt) {
		t.Fatalf("first DateLastSaved = %v, want %v", firstLoaded.DateLastSaved, savedAt)
	}

	secondLoaded, err := items.Get(t.Context(), second.ID)
	if err != nil {
		t.Fatalf("get second movie: %v", err)
	}
	if secondLoaded.Overview == nil || *secondLoaded.Overview != concurrentOverview {
		t.Fatalf("second Overview = %v, want the concurrent local update — the per-item baseline must observe writers that commit between items", secondLoaded.Overview)
	}
	foundGenre := false
	for _, genre := range secondLoaded.Values.Genres {
		if genre == "Drama" {
			foundGenre = true
		}
	}
	if !foundGenre {
		t.Fatalf("second Genres = %v, want Drama — the remote genre patch must persist alongside the concurrent update", secondLoaded.Values.Genres)
	}
	if secondLoaded.DateLastSaved == nil || !secondLoaded.DateLastSaved.Equal(savedAt) {
		t.Fatalf("second DateLastSaved = %v, want %v", secondLoaded.DateLastSaved, savedAt)
	}
}

// TestItemStoreMetadataBackfillPreservesVirtualMetadataSQLite proves the
// metadata-backfill operation preserves stored metadata, structural identity,
// and satellite families on an existing Person virtual when the remote
// candidate omits them.
func TestItemStoreMetadataBackfillPreservesVirtualMetadataSQLite(t *testing.T) {
	runItemStoreMetadataBackfillPreservesVirtualMetadata(t, openItemStore)
}

// TestItemStoreMetadataBackfillPreservesVirtualMetadataPostgres proves the same
// virtual-preservation contract against PostgreSQL.
func TestItemStoreMetadataBackfillPreservesVirtualMetadataPostgres(t *testing.T) {
	runItemStoreMetadataBackfillPreservesVirtualMetadata(t, openPostgresItemStore)
}

// runItemStoreMetadataBackfillPreservesVirtualMetadata seeds a Person virtual
// with metadata, a provider ID, an image, and a satellite family. It then runs
// MetadataBackfill with a sparse Person virtual that omits all of them. The
// stored Person must keep its metadata, identity, and satellites. An absent
// Person virtual must insert through the ordinary upsert path.
func runItemStoreMetadataBackfillPreservesVirtualMetadata(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 13000, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	// Seed an existing movie child so the mixed plan has an existing item.
	movie := reconciliationChild(t, 13001, item.KindMovie, parent.ID, "seed movie")
	movie.Overview = stringPtr("A probe-sourced overview")
	movie.ProviderIDs = providerIDs("Tmdb", "603")
	if err := items.Upsert(t.Context(), movie); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// Seed a Person virtual with metadata, a provider ID, an image, and a
	// satellite family. The backfill must preserve all of these because the
	// sparse remote candidate omits them.
	person := hierarchyItem(t, 13010, item.KindPerson, nil)
	person.Name = stringPtr("Lana Wachowski")
	person.Overview = stringPtr("A director biography from a sidecar")
	person.RunTimeTicks = int64Ptr(999999)
	person.Values.Genres = []string{"Film Noir"}
	person.Values.Studios = []string{"Studio Ghibli"}
	person.Values.Tags = []string{"tag-person"}
	person.ProviderIDs = providerIDs("Imdb", "nm0001")
	person.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 0, Path: "https://image.tmdb.org/t/p/original/person.jpg"},
	}
	person.TrailerTypes = []item.TrailerType{item.TrailerTypeLocalTrailer}
	person.AdditionalParts = []string{"/people/lana-part2.mkv"}
	if err := items.Upsert(t.Context(), person); err != nil {
		t.Fatalf("seed Person virtual: %v", err)
	}

	// Build a sparse Person virtual that carries only the structural identity.
	// This simulates the nfoPeople output that builds a Person from only a
	// PersonCredit name.
	sparsePerson := item.Item{
		ID:   person.ID,
		Kind: person.Kind,
		Path: person.Path,
		Name: person.Name,
	}

	// Build a sparse remote movie that carries only the provider enrichment.
	remoteMovie := item.Item{
		ID:       movie.ID,
		Kind:     movie.Kind,
		ParentID: movie.ParentID,
		Path:     movie.Path,
		Name:     movie.Name,
	}
	remoteMovie.ProviderIDs = providerIDs("Tmdb", "603")
	remoteMovie.Overview = stringPtr("A fresh remote overview")

	savedAt := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	result, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remoteMovie}, []item.Item{sparsePerson}, savedAt, store.MetadataBackfillOptions{ForceRefresh: true})
	if err != nil {
		t.Fatalf("metadata backfill with virtuals: %v", err)
	}
	assertReconcileResult(t, result, nil, []dto.UUID{movie.ID}, nil)

	// Assert the Person virtual keeps its stored metadata, structural
	// identity, and satellite families. The sparse candidate omitted all of
	// these.
	loaded, err := items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("get Person virtual after backfill: %v", err)
	}
	if loaded.ID != person.ID {
		t.Fatalf("Person virtual ID = %s, want %s — the backfill must preserve the stored identity", loaded.ID, person.ID)
	}
	if loaded.Kind != item.KindPerson {
		t.Fatalf("Person virtual Kind = %s, want Person", loaded.Kind)
	}
	if !reflect.DeepEqual(loaded.Path, person.Path) {
		t.Fatalf("Person virtual Path = %v, want %v", loaded.Path, person.Path)
	}
	if !reflect.DeepEqual(loaded.Name, person.Name) {
		t.Fatalf("Person virtual Name = %v, want %v", loaded.Name, person.Name)
	}
	if loaded.Overview == nil || *loaded.Overview != "A director biography from a sidecar" {
		t.Fatalf("Person virtual Overview = %v, want the stored value — the backfill must preserve omitted metadata", loaded.Overview)
	}
	if loaded.RunTimeTicks == nil || *loaded.RunTimeTicks != 999999 {
		t.Fatalf("Person virtual RunTimeTicks = %v, want 999999", loaded.RunTimeTicks)
	}
	if !reflect.DeepEqual(loaded.Values.Genres, []string{"Film Noir"}) {
		t.Fatalf("Person virtual Genres = %v, want [Film Noir]", loaded.Values.Genres)
	}
	if !reflect.DeepEqual(loaded.Values.Studios, []string{"Studio Ghibli"}) {
		t.Fatalf("Person virtual Studios = %v, want [Studio Ghibli]", loaded.Values.Studios)
	}
	if !reflect.DeepEqual(loaded.Values.Tags, []string{"tag-person"}) {
		t.Fatalf("Person virtual Tags = %v, want [tag-person]", loaded.Values.Tags)
	}
	if id, ok := loaded.ProviderIDs.Get("Imdb"); !ok || id != "nm0001" {
		t.Fatalf("Person virtual Imdb id = %q,%t, want nm0001,true — the backfill must preserve an unrelated provider ID", id, ok)
	}
	var hasPersonImage bool
	for _, image := range loaded.ImageInfos {
		if image.Type == item.ImageTypePrimary && image.Path == "https://image.tmdb.org/t/p/original/person.jpg" {
			hasPersonImage = true
		}
	}
	if !hasPersonImage {
		t.Fatalf("Person virtual lost its stored Primary image; ImageInfos = %#v", loaded.ImageInfos)
	}
	if !reflect.DeepEqual(loaded.TrailerTypes, person.TrailerTypes) {
		t.Fatalf("Person virtual TrailerTypes = %v, want %v — the backfill must preserve an untouched satellite family", loaded.TrailerTypes, person.TrailerTypes)
	}
	if !reflect.DeepEqual(loaded.AdditionalParts, person.AdditionalParts) {
		t.Fatalf("Person virtual AdditionalParts = %v, want %v — the backfill must preserve an untouched satellite family", loaded.AdditionalParts, person.AdditionalParts)
	}

	// Assert the movie kept its stored fields and gained the remote overview.
	movieLoaded, err := items.Get(t.Context(), movie.ID)
	if err != nil {
		t.Fatalf("get movie after backfill: %v", err)
	}
	if movieLoaded.Overview == nil || *movieLoaded.Overview != "A fresh remote overview" {
		t.Fatalf("movie Overview = %v, want the remote value", movieLoaded.Overview)
	}
}

// TestItemStoreMetadataBackfillInsertsAbsentVirtualSQLite proves the
// metadata-backfill operation inserts an absent Person virtual through the
// ordinary upsert path with the full candidate.
func TestItemStoreMetadataBackfillInsertsAbsentVirtualSQLite(t *testing.T) {
	runItemStoreMetadataBackfillInsertsAbsentVirtual(t, openItemStore)
}

// TestItemStoreMetadataBackfillInsertsAbsentVirtualPostgres proves the same
// absent-virtual insertion contract against PostgreSQL.
func TestItemStoreMetadataBackfillInsertsAbsentVirtualPostgres(t *testing.T) {
	runItemStoreMetadataBackfillInsertsAbsentVirtual(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillInsertsAbsentVirtual(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 13100, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	absentPerson := hierarchyItem(t, 13101, item.KindPerson, nil)
	absentPerson.Name = stringPtr("New Person")
	absentPerson.Overview = stringPtr("A new person overview")
	absentPerson.ProviderIDs = providerIDs("Tmdb", "500")

	savedAt := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, nil, []item.Item{absentPerson}, savedAt, store.MetadataBackfillOptions{ForceRefresh: true}); err != nil {
		t.Fatalf("metadata backfill with absent virtual: %v", err)
	}

	loaded, err := items.Get(t.Context(), absentPerson.ID)
	if err != nil {
		t.Fatalf("get absent Person after backfill: %v", err)
	}
	if loaded.ID != absentPerson.ID {
		t.Fatalf("absent Person ID = %s, want %s", loaded.ID, absentPerson.ID)
	}
	if loaded.Kind != item.KindPerson {
		t.Fatalf("absent Person Kind = %s, want Person", loaded.Kind)
	}
	if loaded.Overview == nil || *loaded.Overview != "A new person overview" {
		t.Fatalf("absent Person Overview = %v, want the candidate value", loaded.Overview)
	}
	if id, ok := loaded.ProviderIDs.Get("Tmdb"); !ok || id != "500" {
		t.Fatalf("absent Person Tmdb id = %q,%t, want 500,true", id, ok)
	}
}

// TestItemStoreMetadataBackfillMixedVirtualPlanSQLite proves the
// metadata-backfill operation handles a mixed plan with an existing Person
// virtual, an existing child, and an absent child. The Person virtual must
// keep its stored metadata and satellites. The existing child must keep its
// stored fields. The absent child must insert through the ordinary reconcile
// path with its complete candidate. No existing child, virtual row, metadata
// field, or satellite row is deleted.
func TestItemStoreMetadataBackfillMixedVirtualPlanSQLite(t *testing.T) {
	runItemStoreMetadataBackfillMixedVirtualPlan(t, openItemStore)
}

// TestItemStoreMetadataBackfillMixedVirtualPlanPostgres proves the same
// mixed-plan contract against PostgreSQL.
func TestItemStoreMetadataBackfillMixedVirtualPlanPostgres(t *testing.T) {
	runItemStoreMetadataBackfillMixedVirtualPlan(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillMixedVirtualPlan(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 13200, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	// Seed an existing child with metadata.
	existingChild := reconciliationChild(t, 13201, item.KindMovie, parent.ID, "existing movie")
	existingChild.Overview = stringPtr("A stored overview")
	existingChild.RunTimeTicks = int64Ptr(12345678900)
	existingChild.Values.Genres = []string{"Action"}
	if err := items.Upsert(t.Context(), existingChild); err != nil {
		t.Fatalf("seed existing child: %v", err)
	}

	// Seed a Person virtual with metadata and a satellite family.
	person := hierarchyItem(t, 13210, item.KindPerson, nil)
	person.Name = stringPtr("Lana Wachowski")
	person.Overview = stringPtr("A director biography")
	person.Values.Genres = []string{"Film Noir"}
	person.TrailerTypes = []item.TrailerType{item.TrailerTypeLocalTrailer}
	person.AdditionalParts = []string{"/people/lana-part2.mkv"}
	if err := items.Upsert(t.Context(), person); err != nil {
		t.Fatalf("seed Person virtual: %v", err)
	}

	// Build a sparse remote candidate for the existing child that omits all
	// stored fields and adds only a provider ID.
	remoteChild := item.Item{
		ID:       existingChild.ID,
		Kind:     existingChild.Kind,
		ParentID: existingChild.ParentID,
		Path:     existingChild.Path,
		Name:     existingChild.Name,
	}
	remoteChild.ProviderIDs = providerIDs("Tmdb", "603")

	// Build a sparse Person virtual that omits the stored metadata and
	// satellite family.
	sparsePerson := item.Item{
		ID:   person.ID,
		Kind: person.Kind,
		Path: person.Path,
		Name: person.Name,
	}

	savedAt := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	result, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remoteChild}, []item.Item{sparsePerson}, savedAt, store.MetadataBackfillOptions{ForceRefresh: true})
	if err != nil {
		t.Fatalf("metadata backfill mixed plan: %v", err)
	}
	assertReconcileResult(t, result, nil, []dto.UUID{existingChild.ID}, nil)

	// The Person virtual keeps its stored metadata and satellite family.
	personLoaded, err := items.Get(t.Context(), person.ID)
	if err != nil {
		t.Fatalf("get Person virtual: %v", err)
	}
	if personLoaded.Overview == nil || *personLoaded.Overview != "A director biography" {
		t.Fatalf("Person virtual Overview = %v, want the stored value — the backfill must preserve omitted metadata", personLoaded.Overview)
	}
	if !reflect.DeepEqual(personLoaded.Values.Genres, []string{"Film Noir"}) {
		t.Fatalf("Person virtual Genres = %v, want [Film Noir]", personLoaded.Values.Genres)
	}
	if !reflect.DeepEqual(personLoaded.TrailerTypes, person.TrailerTypes) {
		t.Fatalf("Person virtual TrailerTypes = %v, want %v — the backfill must preserve the satellite family", personLoaded.TrailerTypes, person.TrailerTypes)
	}
	if !reflect.DeepEqual(personLoaded.AdditionalParts, person.AdditionalParts) {
		t.Fatalf("Person virtual AdditionalParts = %v, want %v — the backfill must preserve the satellite family", personLoaded.AdditionalParts, person.AdditionalParts)
	}

	// The existing child keeps its stored fields and gains the provider ID.
	childLoaded, err := items.Get(t.Context(), existingChild.ID)
	if err != nil {
		t.Fatalf("get existing child: %v", err)
	}
	if childLoaded.Overview == nil || *childLoaded.Overview != "A stored overview" {
		t.Fatalf("existing child Overview = %v, want the stored value", childLoaded.Overview)
	}
	if childLoaded.RunTimeTicks == nil || *childLoaded.RunTimeTicks != 12345678900 {
		t.Fatalf("existing child RunTimeTicks = %v, want 12345678900", childLoaded.RunTimeTicks)
	}
	if !reflect.DeepEqual(childLoaded.Values.Genres, []string{"Action"}) {
		t.Fatalf("existing child Genres = %v, want [Action]", childLoaded.Values.Genres)
	}
	if id, ok := childLoaded.ProviderIDs.Get("Tmdb"); !ok || id != "603" {
		t.Fatalf("existing child Tmdb id = %q,%t, want 603,true", id, ok)
	}

	// No existing child, virtual row, metadata field, or satellite row was
	// deleted.
	allItems, err := items.Query(t.Context(), store.ItemQuery{EnableTotalRecordCount: true})
	if err != nil {
		t.Fatalf("query all items: %v", err)
	}
	foundPerson := false
	foundChild := false
	for _, it := range allItems.Items {
		if it.ID == person.ID {
			foundPerson = true
		}
		if it.ID == existingChild.ID {
			foundChild = true
		}
	}
	if !foundPerson {
		t.Fatalf("Person virtual was deleted — the backfill must not delete an existing virtual row")
	}
	if !foundChild {
		t.Fatalf("existing child was deleted — the backfill must not delete an existing child")
	}
}

// TestItemStoreMetadataBackfillPreservesEmptyPathImageSQLite proves the
// metadata-backfill operation preserves a stored image when the remote patch
// supplies a same-key image with an empty Path. The stored image and an
// unrelated stored image must survive with exact equality.
func TestItemStoreMetadataBackfillPreservesEmptyPathImageSQLite(t *testing.T) {
	runItemStoreMetadataBackfillPreservesEmptyPathImage(t, openItemStore)
}

// TestItemStoreMetadataBackfillPreservesEmptyPathImagePostgres proves the same
// empty-path image preservation contract against PostgreSQL.
func TestItemStoreMetadataBackfillPreservesEmptyPathImagePostgres(t *testing.T) {
	runItemStoreMetadataBackfillPreservesEmptyPathImage(t, openPostgresItemStore)
}

// runItemStoreMetadataBackfillPreservesEmptyPathImage seeds a stored Backdrop
// image with a populated Path and an unrelated stored Logo image. It applies a
// remote patch that supplies a same-key Backdrop image with an empty Path. The
// stored Backdrop and the unrelated Logo must survive with exact equality.
func runItemStoreMetadataBackfillPreservesEmptyPathImage(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 14000, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	seed := reconciliationChild(t, 14001, item.KindMovie, parent.ID, "empty-path image movie")
	seed.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypeBackdrop, Index: 0, Path: "https://image.tmdb.org/t/p/original/stored-backdrop.jpg"},
		{Type: item.ImageTypeLogo, Index: 0, Path: "/logos/stored-logo.png"},
	}
	seedSaved := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// A remote patch that supplies a same-key Backdrop image with an empty
	// Path. The empty Path is an omission signal, so the stored Backdrop
	// must survive.
	remote := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: seed.ParentID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	remote.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypeBackdrop, Index: 0, Path: ""},
	}
	backfillSaved := seedSaved.Add(time.Hour)
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, backfillSaved, store.MetadataBackfillOptions{ForceRefresh: true}); err != nil {
		t.Fatalf("metadata backfill with empty-path image: %v", err)
	}

	loaded, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}

	// Assert the exact image set: the stored Backdrop survived the empty-path
	// remote image and the unrelated Logo survived with exact equality.
	want := []item.ImageInfo{
		{Type: item.ImageTypeBackdrop, Index: 0, Path: "https://image.tmdb.org/t/p/original/stored-backdrop.jpg"},
		{Type: item.ImageTypeLogo, Index: 0, Path: "/logos/stored-logo.png"},
	}
	if !imageSetEqual(loaded.ImageInfos, want) {
		t.Fatalf("ImageInfos = %#v, want exact set %#v — the empty-path remote image must not clear the stored Backdrop and the unrelated Logo must survive", loaded.ImageInfos, want)
	}
}

// TestItemStoreMetadataBackfillPreservesUnrelatedImagesSQLite proves the
// metadata-backfill operation replaces only the same-key image and preserves
// every unrelated stored image with exact equality.
func TestItemStoreMetadataBackfillPreservesUnrelatedImagesSQLite(t *testing.T) {
	runItemStoreMetadataBackfillPreservesUnrelatedImages(t, openItemStore)
}

// TestItemStoreMetadataBackfillPreservesUnrelatedImagesPostgres proves the same
// unrelated-image preservation and same-key replacement contract against
// PostgreSQL.
func TestItemStoreMetadataBackfillPreservesUnrelatedImagesPostgres(t *testing.T) {
	runItemStoreMetadataBackfillPreservesUnrelatedImages(t, openPostgresItemStore)
}

// runItemStoreMetadataBackfillPreservesUnrelatedImages seeds three stored
// images: a local Primary, an old remote Backdrop, and a local Logo. It applies
// a remote patch that replaces only the Backdrop. The test asserts exact
// image equality: the new Backdrop replaces the old URL, and the Primary and
// Logo survive unchanged.
func runItemStoreMetadataBackfillPreservesUnrelatedImages(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 14100, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	seed := reconciliationChild(t, 14101, item.KindMovie, parent.ID, "unrelated image movie")
	seed.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 0, Path: "/movies/local-primary.jpg"},
		{Type: item.ImageTypeBackdrop, Index: 0, Path: "https://image.tmdb.org/t/p/original/old-backdrop.jpg"},
		{Type: item.ImageTypeLogo, Index: 0, Path: "/logos/local-logo.png"},
	}
	seedSaved := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// A remote patch that replaces only the Backdrop. The Primary and the
	// Logo are unrelated keys, so they must survive.
	remote := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: seed.ParentID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	remote.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypeBackdrop, Index: 0, Path: "https://image.tmdb.org/t/p/original/new-backdrop.jpg"},
	}
	backfillSaved := seedSaved.Add(time.Hour)
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, backfillSaved, store.MetadataBackfillOptions{ForceRefresh: true}); err != nil {
		t.Fatalf("metadata backfill with unrelated images: %v", err)
	}

	loaded, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}

	// Build the expected exact image set.
	want := []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 0, Path: "/movies/local-primary.jpg"},
		{Type: item.ImageTypeBackdrop, Index: 0, Path: "https://image.tmdb.org/t/p/original/new-backdrop.jpg"},
		{Type: item.ImageTypeLogo, Index: 0, Path: "/logos/local-logo.png"},
	}

	// Match images by (Type, Index) so the assertion is order-independent.
	if !imageSetEqual(loaded.ImageInfos, want) {
		t.Fatalf("ImageInfos = %#v, want exact set %#v — the backfill must replace only the same-key image and preserve every unrelated image", loaded.ImageInfos, want)
	}
}

// imageSetEqual reports whether two image slices contain the same images by
// (Type, Index, Path) without regard for order.
func imageSetEqual(got, want []item.ImageInfo) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g.Type == w.Type && g.Index == w.Index && g.Path == w.Path {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestItemStoreMetadataBackfillPreservesRatingFromZeroDefaultSQLite proves a
// zero remote CommunityRating does not overwrite a stored non-zero rating.
// TMDb assigns CommunityRating from VoteAverage, and VoteAverage defaults to
// zero when the response omits it, so a zero desired value is an omission.
func TestItemStoreMetadataBackfillPreservesRatingFromZeroDefaultSQLite(t *testing.T) {
	runItemStoreMetadataBackfillPreservesRatingFromZeroDefault(t, openItemStore)
}

// TestItemStoreMetadataBackfillPreservesRatingFromZeroDefaultPostgres proves a
// zero remote CommunityRating does not overwrite a stored non-zero rating.
func TestItemStoreMetadataBackfillPreservesRatingFromZeroDefaultPostgres(t *testing.T) {
	runItemStoreMetadataBackfillPreservesRatingFromZeroDefault(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillPreservesRatingFromZeroDefault(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 14300, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	seed := reconciliationChild(t, 14301, item.KindMovie, parent.ID, "rated movie")
	seed.CommunityRating = float32Ptr(8.7)
	seed.CriticRating = float32Ptr(7.5)
	seed.ProviderIDs = providerIDs("Tmdb", "603")
	seedSaved := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// Produce the remote patch from an in-memory TMDb cassette whose movie
	// response omits vote_average. The real provider adapter runs through
	// the cassette, so the provider itself assigns CommunityRating from the
	// omitted field: VoteAverage defaults to zero, and the provider sets
	// CommunityRating to a pointer to zero. The test does not mutate the
	// adapter output afterward. A zero desired value is an omission, so the
	// merge must preserve the stored non-zero CommunityRating. The provider
	// does not set CriticRating, so the adapter output leaves it nil, and the
	// merge preserves the stored non-zero CriticRating.
	remote := backfillCassetteRemotePatchOmittingVoteAverage(t, item.Item{
		ID:          seed.ID,
		Kind:        seed.Kind,
		ParentID:    seed.ParentID,
		Path:        seed.Path,
		Name:        seed.Name,
		ProviderIDs: seed.ProviderIDs,
	})
	backfillSaved := seedSaved.Add(time.Hour)
	result, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, backfillSaved, store.MetadataBackfillOptions{ForceRefresh: true})
	if err != nil {
		t.Fatalf("metadata backfill with zero rating: %v", err)
	}
	assertReconcileResult(t, result, nil, []dto.UUID{seed.ID}, nil)

	loaded, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	if loaded.CommunityRating == nil || *loaded.CommunityRating != 8.7 {
		t.Fatalf("CommunityRating = %v, want 8.7 — a zero remote rating must not overwrite the stored value", loaded.CommunityRating)
	}
	if loaded.CriticRating == nil || *loaded.CriticRating != 7.5 {
		t.Fatalf("CriticRating = %v, want 7.5 — a zero remote rating must not overwrite the stored value", loaded.CriticRating)
	}

	// A non-zero remote rating replaces the stored value. Produce the
	// remote patch from the cassette again. The cassette carries a non-zero
	// vote_average (8.2), so the provider-derived CommunityRating is
	// non-zero and must replace the stored value.
	remote2 := backfillCassetteRemotePatch(t, item.Item{
		ID:          seed.ID,
		Kind:        seed.Kind,
		ParentID:    seed.ParentID,
		Path:        seed.Path,
		Name:        seed.Name,
		ProviderIDs: seed.ProviderIDs,
	})
	backfillSaved2 := backfillSaved.Add(time.Hour)
	if _, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote2}, nil, backfillSaved2, store.MetadataBackfillOptions{ForceRefresh: true}); err != nil {
		t.Fatalf("metadata backfill with non-zero rating: %v", err)
	}
	loaded2, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get movie after replacement: %v", err)
	}
	if loaded2.CommunityRating == nil || *loaded2.CommunityRating != 8.2 {
		t.Fatalf("CommunityRating = %v, want 8.2 — the cassette non-zero value must replace the stored rating", loaded2.CommunityRating)
	}
}

// TestItemStoreMetadataBackfillExtraReplacementPersistsForceRefreshFalseSQLite
// proves a non-empty remote TmdbCollectionName persists when ForceRefresh is
// false. mergeBackfillExtra must not mutate the shared stored Movie pointer,
// or backfillOneItem compares the mutated baseline and skips the write.
func TestItemStoreMetadataBackfillExtraReplacementPersistsForceRefreshFalseSQLite(t *testing.T) {
	runItemStoreMetadataBackfillExtraReplacementPersistsForceRefreshFalse(t, openItemStore)
}

// TestItemStoreMetadataBackfillExtraReplacementPersistsForceRefreshFalsePostgres
// proves a non-empty remote TmdbCollectionName persists when ForceRefresh is
// false.
func TestItemStoreMetadataBackfillExtraReplacementPersistsForceRefreshFalsePostgres(t *testing.T) {
	runItemStoreMetadataBackfillExtraReplacementPersistsForceRefreshFalse(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillExtraReplacementPersistsForceRefreshFalse(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 14400, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	// Seed a stale collection name and an unrelated stored Extra field. The
	// remote stage does not produce the video aspect ratio, so the backfill
	// merge must preserve it.
	seed := reconciliationChild(t, 14401, item.KindMovie, parent.ID, "collection movie")
	seed.Extra.Movie = &item.MovieExtra{TmdbCollectionName: stringPtr("Stale Collection")}
	seed.Extra.Video = &item.VideoExtra{AspectRatio: stringPtr("16:9")}
	seed.Extra.RemoteTrailers = []item.MediaURL{{URL: stringPtr("https://example.com/old.mp4")}}
	seedSaved := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	seed.DateLastSaved = &seedSaved
	if err := items.Upsert(t.Context(), seed); err != nil {
		t.Fatalf("seed movie: %v", err)
	}

	// Apply a different non-empty collection name with ForceRefresh false.
	// The merge must detect the change and write it, even though the
	// no-op comparison compares the merged item against the stored baseline.
	newName := "The Matrix Collection"
	remote := item.Item{
		ID:       seed.ID,
		Kind:     seed.Kind,
		ParentID: seed.ParentID,
		Path:     seed.Path,
		Name:     seed.Name,
	}
	remote.Extra.Movie = &item.MovieExtra{TmdbCollectionName: &newName}
	backfillSaved := seedSaved.Add(time.Hour)
	result, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remote}, nil, backfillSaved, store.MetadataBackfillOptions{ForceRefresh: false})
	if err != nil {
		t.Fatalf("metadata backfill with extra replacement: %v", err)
	}
	assertReconcileResult(t, result, nil, []dto.UUID{seed.ID}, nil)

	loaded, err := items.Get(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	// The new collection name persisted.
	if loaded.Extra.Movie == nil || loaded.Extra.Movie.TmdbCollectionName == nil || *loaded.Extra.Movie.TmdbCollectionName != newName {
		t.Fatalf("TmdbCollectionName = %v, want %q — a non-empty remote collection name must persist with ForceRefresh false", loaded.Extra.Movie, newName)
	}
	// The unrelated stored Extra fields survived with exact equality.
	if loaded.Extra.Video == nil || loaded.Extra.Video.AspectRatio == nil || *loaded.Extra.Video.AspectRatio != "16:9" {
		t.Fatalf("Video.AspectRatio = %v, want \"16:9\" — an unrelated stored Extra field must survive the backfill", loaded.Extra.Video)
	}
	if len(loaded.Extra.RemoteTrailers) != 1 || loaded.Extra.RemoteTrailers[0].URL == nil || *loaded.Extra.RemoteTrailers[0].URL != "https://example.com/old.mp4" {
		t.Fatalf("RemoteTrailers = %#v, want one entry with the old URL — an unrelated stored Extra field must survive the backfill", loaded.Extra.RemoteTrailers)
	}
}

// providerIDsEqual reports whether two ProviderIDs contain the same entries
// by (Name, Value) without regard for order.
func providerIDsEqual(got, want item.ProviderIDs) bool {
	gotEntries := got.Snapshot()
	wantEntries := want.Snapshot()
	if len(gotEntries) != len(wantEntries) {
		return false
	}
	for _, w := range wantEntries {
		found := false
		for _, g := range gotEntries {
			if g.Name == w.Name && g.Value == w.Value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestItemStoreMetadataBackfillKeepsStoredVirtualPathSQLite proves a virtual
// candidate whose path differs only in case from the stored row merges onto
// that row and keeps the stored path (#748).
func TestItemStoreMetadataBackfillKeepsStoredVirtualPathSQLite(t *testing.T) {
	runItemStoreMetadataBackfillKeepsStoredVirtualPath(t, openItemStore)
}

// TestItemStoreMetadataBackfillKeepsStoredVirtualPathPostgres proves the
// same virtual path contract against PostgreSQL.
func TestItemStoreMetadataBackfillKeepsStoredVirtualPathPostgres(t *testing.T) {
	runItemStoreMetadataBackfillKeepsStoredVirtualPath(t, openPostgresItemStore)
}

func runItemStoreMetadataBackfillKeepsStoredVirtualPath(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()

	items := open(t)
	parent := hierarchyItem(t, 12300, item.KindFolder, nil)
	if err := items.Upsert(t.Context(), parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}
	movie := reconciliationChild(t, 12301, item.KindMovie, parent.ID, "virtual path movie")
	if err := items.Upsert(t.Context(), movie); err != nil {
		t.Fatalf("seed movie: %v", err)
	}
	studio := hierarchyItem(t, 12310, item.KindStudio, nil)
	studio.Name = stringPtr("Starz")
	studio.Path = stringPtr("%MetadataPath%/Studio/Starz")
	if err := items.Upsert(t.Context(), studio); err != nil {
		t.Fatalf("seed Studio: %v", err)
	}

	remoteMovie := item.Item{ID: movie.ID, Kind: movie.Kind, ParentID: movie.ParentID, Path: movie.Path, Name: movie.Name}
	remoteMovie.ProviderIDs = providerIDs("Tmdb", "146")
	variant := item.Item{ID: studio.ID, Kind: item.KindStudio, Path: stringPtr("%MetadataPath%/Studio/STARZ"), Name: stringPtr("STARZ")}

	savedAt := time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	result, err := items.MetadataBackfill(t.Context(), parent.ID, []item.Item{remoteMovie}, []item.Item{variant}, savedAt, store.MetadataBackfillOptions{})
	if err != nil {
		t.Fatalf("metadata backfill with a case-variant virtual path: %v", err)
	}
	if len(result.Updated) != 1 || result.Updated[0] != movie.ID {
		t.Fatalf("updated = %v, want [%s]", result.Updated, movie.ID)
	}
	loaded, err := items.Get(t.Context(), studio.ID)
	if err != nil {
		t.Fatalf("get Studio: %v", err)
	}
	if loaded.Path == nil || *loaded.Path != "%MetadataPath%/Studio/Starz" {
		t.Fatalf("Studio Path = %v, want the stored path", loaded.Path)
	}
	enriched, err := items.Get(t.Context(), movie.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	if id, ok := enriched.ProviderIDs.Get("Tmdb"); !ok || id != "146" {
		t.Fatalf("movie Tmdb id = %q,%t, want 146,true", id, ok)
	}
}
