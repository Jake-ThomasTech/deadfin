// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

// BenchmarkSearchScoresAtLibraryScale measures the GET /Search/Hints
// candidate search on the library of BenchmarkReconcileAtLibraryScale
// (about 47,000 items, 31,000 of them people). Search hints filter no kind,
// so the search visits every item. The rows carry the path, overview, and
// extra facts of a live library, and people, episodes, and tracks carry
// provider ids, as they do after a metadata refresh. The search must read
// the provider ids of the anime titles that match, not of every item
// (deadfin#826). It runs on file-backed SQLite and, when
// DEADFIN_TEST_POSTGRES_DSN is set, on PostgreSQL.
//
//	go test -run '^$' -bench SearchScoresAtLibraryScale ./internal/store/storetest/
func BenchmarkSearchScoresAtLibraryScale(b *testing.B) {
	forEachScaleBackend(b, benchmarkSearchScoresAtLibraryScale)
}

func benchmarkSearchScoresAtLibraryScale(b *testing.B, stores store.Stores) {
	ctx := b.Context()
	items := store.NewItemStore(stores)
	writeScaleFixture(b, ctx, stores)
	if err := items.RepairDerived(ctx); err != nil {
		b.Fatalf("initial global repair: %v", err)
	}
	q := scaleSQL{stores.Dialect()}
	text := "hex(id)"
	if q.postgres() {
		text = "CAST(id AS TEXT)"
	}
	overview := "'" + strings.Repeat("A long overview of the plot that runs on. ", 15) + "'"
	extra := `'{"tags":["a","b"],"studios":["Studio One","Studio Two"],"remoteTrailers":[{"url":"https://www.youtube.com/watch?v=0123456789a"}],"productionLocations":["United States of America"]}'`
	for _, statement := range []string{
		// The fixture names are ASCII, so LOWER is item.CleanName here.
		"UPDATE items SET clean_name = LOWER(name)",
		"UPDATE items SET path = '/media/library/' || name || '/' || name || ' (2001) [Bluray-1080p].mkv', overview = " + overview + ", extra = " + extra + " WHERE kind <> 'Person' OR id IN (SELECT person_id FROM people_map)",
		"INSERT INTO provider_ids (item_id, provider, value) SELECT id, 'Tmdb', " + text + " FROM items WHERE kind IN ('Person', 'Episode', 'Audio')",
		"INSERT INTO provider_ids (item_id, provider, value) SELECT id, 'Imdb', " + text + " FROM items WHERE kind IN ('Person', 'Episode')",
		"INSERT INTO provider_ids (item_id, provider, value) SELECT id, 'Tvdb', " + text + " FROM items WHERE kind = 'Episode'",
		// Every series is an AniList anime with two titles.
		"INSERT INTO provider_ids (item_id, provider, value) SELECT id, 'AniList', " + text + " FROM items WHERE kind = 'Series'",
		"INSERT INTO anime_titles (provider, external_id, ordinal, title, clean_title, fold_title) SELECT provider, value, 0, 'Anime ' || value, 'anime ' || LOWER(value), 'anime' || LOWER(value) FROM provider_ids WHERE provider = 'AniList'",
		"INSERT INTO anime_titles (provider, external_id, ordinal, title, clean_title, fold_title) SELECT provider, value, 1, 'Other ' || value, 'other ' || LOWER(value), 'other' || LOWER(value) FROM provider_ids WHERE provider = 'AniList'",
	} {
		if _, err := stores.Writer().ExecContext(ctx, statement); err != nil {
			b.Fatalf("prepare the search fixture: %v", err)
		}
	}
	analyzeScaleFixture(b, ctx, stores)
	admin := store.User{ID: scaleUUID(990_001), Policy: []byte(`{"IsAdministrator":true}`)}
	viewer := store.User{ID: scaleUUID(990_002), Policy: []byte(`{"EnableAllFolders":true}`)}
	// The candidate query of the search hints handler.
	notVirtual, noCollapse := false, false
	candidates := func(term string, user *store.User) store.ItemQuery {
		return store.ItemQuery{SearchTerm: term, IsVirtualItem: &notVirtual, IncludeOwnedItems: true, CollapseBoxSetItems: &noCollapse,
			ExcludeItemTypes: []item.Kind{item.KindYear, item.KindFolder, item.KindCollectionFolder}, User: user, UserID: &user.ID}
	}
	for _, scenario := range []struct {
		name    string
		query   store.ItemQuery
		matches int
	}{
		{"one match", candidates("Movie 512", &admin), 1},
		{"one match, not an administrator", candidates("Movie 512", &viewer), 1},
		{"many matches", candidates("Movie", &admin), 100},
		{"anime title", candidates("Anime", &admin), 100},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			for range b.N {
				scores, err := items.SearchScores(ctx, scenario.query, 100)
				if err != nil || len(scores) != scenario.matches {
					b.Fatalf("search scores: %d, %v; want %d", len(scores), err, scenario.matches)
				}
			}
		})
	}
}
