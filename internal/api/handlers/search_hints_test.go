// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

type searchHintsFixture struct {
	router                                      *router.Router
	admin, restricted                           dto.UUID
	movies, shows                               dto.UUID
	exactMovie, wordMovie, series, season       dto.UUID
	episode, person, genre, anime, otherMovie   dto.UUID
	seriesThumb, seriesBackdrop, episodePrimary string
}

// newSearchHintsFixture stores a movies library and a shows library on a
// real SQLite store. The restricted user sees only the movies library.
func newSearchHintsFixture(t *testing.T) searchHintsFixture {
	t.Helper()
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	items := store.NewItemStore(stores)
	id := func(n int) dto.UUID { return itemNamesUUID(t, 900+n) }
	name := itemNamesString
	f := searchHintsFixture{
		admin: id(1), restricted: id(2), movies: id(11), shows: id(12),
		exactMovie: id(20), wordMovie: id(21), otherMovie: id(22), series: id(30), season: id(31), episode: id(32),
		person: id(40), genre: id(41), anime: id(50),
	}
	root, liveTv := id(10), id(60)
	modified := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// The DTO builder tags an image by its file name.
	f.seriesThumb, f.seriesBackdrop, f.episodePrimary = "series-thumb.jpg", "series-backdrop.jpg", "episode.jpg"
	one, year, ticks := 1, 2023, int64(15_000_000_000)
	ended := item.SeriesStatusEnded
	values := []item.Item{
		{ID: root, Kind: item.KindAggregateFolder, IsFolder: true, Name: name("root")},
		{ID: f.movies, Kind: item.KindCollectionFolder, IsFolder: true, ParentID: &root, Name: name("Frieren Movies")},
		{ID: f.shows, Kind: item.KindCollectionFolder, IsFolder: true, ParentID: &root, Name: name("Shows")},
		{ID: f.exactMovie, Kind: item.KindMovie, ParentID: &f.movies, Name: name("Frieren"), SortName: name("frieren"), ProductionYear: &year, RunTimeTicks: &ticks},
		{ID: f.wordMovie, Kind: item.KindMovie, ParentID: &f.movies, Name: name("Sousou no Frieren Special"), SortName: name("sousou no frieren special")},
		{ID: f.otherMovie, Kind: item.KindMovie, ParentID: &f.movies, Name: name("Unrelated"), SortName: name("unrelated")},
		{ID: id(23), Kind: item.KindMovie, ParentID: &f.movies, Name: name("Frieren Missing"), SortName: name("frieren missing"), IsVirtualItem: true},
		{ID: id(24), Kind: item.KindFolder, IsFolder: true, ParentID: &f.movies, Name: name("Frieren Folder"), SortName: name("frieren folder")},
		{ID: f.series, Kind: item.KindSeries, IsFolder: true, ParentID: &f.shows, Name: name("Frieren: Beyond Journey's End"), SortName: name("frieren beyond journeys end"), SeriesStatus: &ended,
			ImageInfos: []item.ImageInfo{{Type: item.ImageTypeThumb, Path: "/art/series-thumb.jpg", DateModified: &modified}, {Type: item.ImageTypeBackdrop, Path: "/art/series-backdrop.jpg", DateModified: &modified}}},
		{ID: f.season, Kind: item.KindSeason, IsFolder: true, ParentID: &f.series, Name: name("Season 1"), IndexNumber: &one, SeriesName: name("Frieren: Beyond Journey's End")},
		{ID: f.episode, Kind: item.KindEpisode, ParentID: &f.season, Name: name("The Journey of Frieren"), SortName: name("the journey of frieren"), IndexNumber: &one, ParentIndexNumber: &one, SeriesName: name("Frieren: Beyond Journey's End"),
			ImageInfos: []item.ImageInfo{{Type: item.ImageTypePrimary, Path: "/art/episode.jpg", DateModified: &modified, Width: 1920, Height: 1080}}},
		{ID: f.person, Kind: item.KindPerson, Name: name("Frieren Fan"), SortName: name("frieren fan")},
		{ID: f.genre, Kind: item.KindGenre, Name: name("Frierenesque"), SortName: name("frierenesque")},
		{ID: f.anime, Kind: item.KindSeries, IsFolder: true, ParentID: &f.shows, Name: name("Attack on Titan"), SortName: name("attack on titan"), ProviderIDs: anilistProviderIDs("16498")},
		{ID: id(60), Kind: item.KindUserView, IsFolder: true, SourceType: item.SourceTypeLiveTV, Name: name("Live TV")},
		{ID: id(61), Kind: item.KindLiveTvChannel, ParentID: &liveTv, SourceType: item.SourceTypeLiveTV, Name: name("FR | France 2"), SortName: name("fr france 2")},
	}
	for _, value := range values {
		if err := items.Upsert(t.Context(), value); err != nil {
			t.Fatalf("upsert %s: %v", value.ID, err)
		}
	}
	if err := store.NewAnimeStore(stores).SaveMedia(t.Context(), store.AnimeMedia{
		AniListID: 16498, Format: "TV", Title: "Attack on Titan", DateFetched: modified,
		TitleSets: []store.AnimeTitleSet{{Provider: "AniList", ExternalID: "16498", Titles: []store.AnimeTitle{{Title: "Shingeki no Kyojin", Language: "x-jat", Kind: "romaji"}}}},
	}); err != nil {
		t.Fatalf("save anime titles: %v", err)
	}
	if err := items.RepairDerived(t.Context()); err != nil {
		t.Fatalf("repair derived: %v", err)
	}

	users := itemsAccessUsers{values: map[dto.UUID]store.User{
		f.admin:      {ID: f.admin, Policy: []byte(`{"IsAdministrator":true,"EnableAllFolders":true}`)},
		f.restricted: {ID: f.restricted, Policy: []byte(`{"EnableAllFolders":false,"EnabledFolders":["` + f.movies.String() + `"]}`)},
	}}
	authorizer := middleware.NewAuthorizer(itemsAccessResolver{identities: map[string]auth.Identity{
		f.admin.String():      {Authenticated: true, UserID: f.admin},
		f.restricted.String(): {Authenticated: true, UserID: f.restricted},
	}}, itemsAccessPolicy{}, users)
	f.router = router.New(authorizer.Wrap)
	handlers.RegisterItems(f.router, items, dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, nil, nil)), clock.NewReal(), handlers.ItemServices{Users: users})
	return f
}

func (f searchHintsFixture) get(t *testing.T, actor dto.UUID, path string, want int) *httptest.ResponseRecorder {
	t.Helper()
	record := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("X-Item-Access-Actor", actor.String())
	f.router.ServeHTTP(record, request)
	if record.Code != want {
		t.Fatalf("GET %s = %d, want %d: %s", path, record.Code, want, record.Body.String())
	}
	return record
}

func (f searchHintsFixture) hints(t *testing.T, actor dto.UUID, path string) dto.SearchHintResult {
	t.Helper()
	var result dto.SearchHintResult
	if err := json.Unmarshal(f.get(t, actor, path, http.StatusOK).Body.Bytes(), &result); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return result
}

func searchHintIDs(result dto.SearchHintResult) []dto.UUID {
	ids := make([]dto.UUID, len(result.SearchHints))
	for index, hint := range result.SearchHints {
		ids[index] = hint.ID
	}
	return ids
}

// TestSearchHintsOrdersByScoreThenSortName pins the SqlSearchProvider tiers
// (exact 100, prefix 80, word prefix 75, contains 50) and the stable sort
// over the sort-name item order. Folders, virtual items, and library roots
// are excluded; people and genres are included by default.
func TestSearchHintsOrdersByScoreThenSortName(t *testing.T) {
	f := newSearchHintsFixture(t)
	got := f.hints(t, f.admin, "/Search/Hints?searchTerm=Frieren")
	want := []dto.UUID{f.exactMovie, f.series, f.person, f.genre, f.wordMovie, f.episode}
	if !reflect.DeepEqual(searchHintIDs(got), want) || got.TotalRecordCount != len(want) {
		t.Fatalf("hints = %v (total %d), want %v", searchHintIDs(got), got.TotalRecordCount, want)
	}

	// The anime title search of the Items path also selects candidates.
	anime := f.hints(t, f.admin, "/Search/Hints?searchTerm=shingeki")
	if !reflect.DeepEqual(searchHintIDs(anime), []dto.UUID{f.anime}) {
		t.Fatalf("anime title hints = %v, want %v", searchHintIDs(anime), f.anime)
	}
}

func TestSearchHintsFiltersAndPaging(t *testing.T) {
	f := newSearchHintsFixture(t)
	for _, tc := range []struct {
		name  string
		actor dto.UUID
		path  string
		want  []dto.UUID
		total int
	}{
		{"include types", f.admin, "/Search/Hints?searchTerm=frieren&includeItemTypes=Movie,Episode", []dto.UUID{f.exactMovie, f.wordMovie, f.episode}, 3},
		{"exclude types", f.admin, "/Search/Hints?searchTerm=frieren&excludeItemTypes=Movie", []dto.UUID{f.series, f.person, f.genre, f.episode}, 4},
		{"people and genres off", f.admin, "/Search/Hints?searchTerm=frieren&includePeople=false&includeGenres=false", []dto.UUID{f.exactMovie, f.series, f.wordMovie, f.episode}, 4},
		// BuildIncludeItemTypes adds the genre kinds first; the list is then
		// not empty and does not contain Person, so people stay out.
		{"media off keeps genres only", f.admin, "/Search/Hints?searchTerm=frieren&includeMedia=false", []dto.UUID{f.genre}, 1},
		{"media off with people", f.admin, "/Search/Hints?searchTerm=frieren&includeMedia=false&includeItemTypes=Person", []dto.UUID{f.person}, 1},
		{"parent scope", f.admin, "/Search/Hints?searchTerm=frieren&parentId=" + f.shows.String(), []dto.UUID{f.series, f.episode}, 2},
		// IsSeries is the Live TV program flag, not the item kind.
		{"is series", f.admin, "/Search/Hints?searchTerm=frieren&includeItemTypes=Series,Movie&isSeries=true", []dto.UUID{}, 0},
		{"is not series", f.admin, "/Search/Hints?searchTerm=frieren&includeItemTypes=Series,Movie&isSeries=false", []dto.UUID{f.exactMovie, f.series, f.wordMovie}, 3},
		{"restricted user", f.restricted, "/Search/Hints?searchTerm=frieren&includeItemTypes=Movie,Series,Episode", []dto.UUID{f.exactMovie, f.wordMovie}, 2},
		// The candidates stop at the limit before paging, as upstream.
		{"limit caps candidates", f.admin, "/Search/Hints?searchTerm=frieren&limit=2", []dto.UUID{f.exactMovie, f.series}, 2},
		{"start index pages within the limit", f.admin, "/Search/Hints?searchTerm=frieren&startIndex=1&limit=3", []dto.UUID{f.series, f.person}, 3},
		{"no match", f.admin, "/Search/Hints?searchTerm=gundam", []dto.UUID{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.hints(t, tc.actor, tc.path)
			if !reflect.DeepEqual(searchHintIDs(got), tc.want) || got.TotalRecordCount != tc.total {
				t.Fatalf("GET %s = %v (total %d), want %v (total %d)", tc.path, searchHintIDs(got), got.TotalRecordCount, tc.want, tc.total)
			}
		})
	}
	f.get(t, f.admin, "/Search/Hints", http.StatusBadRequest)
	f.get(t, f.admin, "/Search/Hints?searchTerm=%20", http.StatusBadRequest)
}

// TestSearchHintsResponseShape compares hint bodies with the fields
// SearchController.GetSearchHintResult writes: ItemId mirrors Id, an empty
// channel id is written as null, Artists is always a list, the series name
// and inherited thumb and backdrop come from the parents.
func TestSearchHintsResponseShape(t *testing.T) {
	f := newSearchHintsFixture(t)
	record := f.get(t, f.admin, "/Search/Hints?searchTerm=journey&includeItemTypes=Episode,Series", http.StatusOK)
	var raw struct {
		SearchHints      []map[string]json.RawMessage
		TotalRecordCount int
	}
	if err := json.Unmarshal(record.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.SearchHints) != 2 {
		t.Fatalf("hints = %s, want the series and the episode", record.Body.String())
	}
	str := func(value string) string { return `"` + value + `"` }
	episode := raw.SearchHints[1]
	wantEpisode := map[string]string{
		"ItemId": str(f.episode.String()), "Id": str(f.episode.String()), "Name": str("The Journey of Frieren"),
		"IndexNumber": "1", "ParentIndexNumber": "1", "Type": str("Episode"), "MediaType": str("Video"),
		"PrimaryImageTag": str(f.episodePrimary), "PrimaryImageAspectRatio": "1.7777777777777777",
		"ThumbImageTag": str(f.seriesThumb), "ThumbImageItemId": str(f.series.String()),
		"BackdropImageTag": str(f.seriesBackdrop), "BackdropImageItemId": str(f.series.String()),
		"Series": str("Frieren: Beyond Journey's End"), "Artists": "[]", "ChannelId": "null",
	}
	if !reflect.DeepEqual(rawStrings(episode), wantEpisode) {
		t.Fatalf("episode hint = %v,\nwant %v", rawStrings(episode), wantEpisode)
	}
	series := raw.SearchHints[0]
	wantSeries := map[string]string{
		"ItemId": str(f.series.String()), "Id": str(f.series.String()), "Name": str("Frieren: Beyond Journey's End"),
		"Type": str("Series"), "IsFolder": "true", "MediaType": str("Unknown"), "Status": str("Ended"),
		"ThumbImageTag": str(f.seriesThumb), "ThumbImageItemId": str(f.series.String()),
		"BackdropImageTag": str(f.seriesBackdrop), "BackdropImageItemId": str(f.series.String()),
		"Artists": "[]", "ChannelId": "null",
	}
	if !reflect.DeepEqual(rawStrings(series), wantSeries) {
		t.Fatalf("series hint = %v,\nwant %v", rawStrings(series), wantSeries)
	}
	if !strings.HasPrefix(record.Body.String(), `{"SearchHints":[{"ItemId":`) {
		t.Fatalf("body = %s, want SearchHints first and ItemId first", record.Body.String())
	}
}

func anilistProviderIDs(id string) item.ProviderIDs {
	var providers item.ProviderIDs
	providers.Set("AniList", id)
	return providers
}

func rawStrings(values map[string]json.RawMessage) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = string(value)
	}
	return out
}

// A Live TV channel is found by the client kind TvChannel, as SharkParty's
// search asks, and its hint carries Type TvChannel
// (SearchController.GetSearchHintResult: item.GetBaseItemKind()).
func TestSearchHintsLiveTvChannelKind(t *testing.T) {
	f := newSearchHintsFixture(t)
	channel := itemNamesUUID(t, 961)
	for _, path := range []string{
		"/Search/Hints?searchTerm=france%202",
		"/Search/Hints?searchTerm=france%202&includeItemTypes=LiveTvProgram,TvChannel,Movie",
	} {
		result := f.hints(t, f.admin, path)
		if len(result.SearchHints) != 1 || result.SearchHints[0].ID != channel || result.SearchHints[0].Type != "TvChannel" {
			t.Fatalf("%s = %+v, want the channel with Type TvChannel", path, result.SearchHints)
		}
	}
	if result := f.hints(t, f.admin, "/Search/Hints?searchTerm=france%202&excludeItemTypes=TvChannel"); len(result.SearchHints) != 0 {
		t.Fatalf("excludeItemTypes=TvChannel = %+v, want none", result.SearchHints)
	}
	var page struct {
		Items []struct {
			ID   dto.UUID `json:"Id"`
			Type string
		}
		TotalRecordCount int
	}
	body := f.get(t, f.admin, "/Items?userId="+f.admin.String()+"&includeItemTypes=TvChannel&recursive=true", http.StatusOK).Body.Bytes()
	if err := json.Unmarshal(body, &page); err != nil || page.TotalRecordCount != 1 || len(page.Items) != 1 || page.Items[0].ID != channel || page.Items[0].Type != "TvChannel" {
		t.Fatalf("/Items includeItemTypes=TvChannel = %s (%v)", body, err)
	}
}
