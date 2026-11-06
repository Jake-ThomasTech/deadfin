// SPDX-License-Identifier: GPL-2.0-only

package store_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

// TestTranslateItemQueryUsesNormalizedPlaylistVisibilitySQL pins divergence
// D-03 (docs/divergences.md). A playlist is visible to the current user only
// through its dedicated public, playlist-owner, or normalized share facts;
// generic owner_id is deliberately not a playlist-owner alias.
func TestTranslateItemQueryUsesNormalizedPlaylistVisibilitySQL(t *testing.T) {
	viewer := groupingUUID(t, 601)
	query := store.ItemQuery{
		User:             &store.User{ID: viewer},
		IncludeItemTypes: []item.Kind{item.KindPlaylist},
	}

	for _, dialect := range groupingDialects() {
		t.Run(dialect.name, func(t *testing.T) {
			translated, err := store.Translate(query, dialect.dialect)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			for _, sql := range []string{translated.SQL, translated.CountSQL} {
				for _, fragment := range []string{"kind IN", "open_access", "playlist_owner_user_id", "item_shares"} {
					if !strings.Contains(sql, fragment) {
						t.Fatalf("playlist visibility SQL = %s\nwant normalized visibility fragment %q", sql, fragment)
					}
				}
				if strings.Contains(sql, "owner_id =") {
					t.Fatalf("playlist visibility must not alias generic owner_id: %s", sql)
				}
				if strings.Contains(sql, viewer.String()) {
					t.Fatalf("playlist visibility SQL interpolated viewer %q: %s", viewer, sql)
				}
				if !strings.Contains(sql, dialect.placeholder) {
					t.Fatalf("playlist visibility SQL = %s, want %s placeholder", sql, dialect.placeholder)
				}
			}
			if len(translated.CountArgs) < 2 {
				t.Fatalf("playlist count args = %#v, want kind and bound viewer", translated.CountArgs)
			}
			if len(translated.Args) < len(translated.CountArgs) {
				t.Fatalf("page args = %#v, want count-argument prefix %#v", translated.Args, translated.CountArgs)
			}
		})
	}
}

// TestTranslateItemQueryUsesBoxSetLibraryFolderRelation pins the collection
// half of D-03: a BoxSet's library visibility comes from
// boxset_library_folders, not playlist facts or a serialized blob. The same
// relation must constrain the count before pagination.
func TestTranslateItemQueryUsesBoxSetLibraryFolderRelation(t *testing.T) {
	folder := groupingUUID(t, 602)
	query := store.ItemQuery{
		IncludeItemTypes:     []item.Kind{item.KindBoxSet},
		BoxSetLibraryFolders: []dto.UUID{folder},
	}
	for _, dialect := range groupingDialects() {
		t.Run(dialect.name, func(t *testing.T) {
			translated, err := store.Translate(query, dialect.dialect)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			for _, sql := range []string{translated.SQL, translated.CountSQL} {
				if !strings.Contains(sql, "boxset_library_folders") || !strings.Contains(sql, "library_folder_id") {
					t.Fatalf("BoxSet library relation SQL = %s", sql)
				}
				if strings.Contains(sql, folder.String()) {
					t.Fatalf("BoxSet library SQL interpolated folder %q: %s", folder, sql)
				}
				if !strings.Contains(sql, dialect.placeholder) {
					t.Fatalf("BoxSet library SQL = %s, want %s placeholder", sql, dialect.placeholder)
				}
			}
			if len(translated.CountArgs) != 2 {
				t.Fatalf("BoxSet count args = %#v, want kind and folder", translated.CountArgs)
			}
		})
	}
}

// TestTranslateItemQueryReadsVisiblePageDirectly pins the direct page shape.
// When visibility is the only transform, the page statement selects from items
// with the filters and the visibility predicate inline, so the planner can walk
// a start index in order and stop at the limit. Another transform keeps the
// id relation.
func TestTranslateItemQueryReadsVisiblePageDirectly(t *testing.T) {
	viewer := groupingUUID(t, 603)
	limit := 50
	direct := store.ItemQuery{
		User:             &store.User{ID: viewer, Policy: []byte(`{"LimitLiveTvToSharedLists":true}`)},
		IncludeItemTypes: []item.Kind{item.KindLiveTvProgram},
		Limit:            &limit,
		OrderBy:          []store.ItemOrder{{SortBy: store.ItemSortByStartDate, SortOrder: store.SortOrderAscending}},
	}
	adjacent := direct
	adjacentTo := groupingUUID(t, 604)
	adjacent.AdjacentTo = &adjacentTo

	for _, dialect := range groupingDialects() {
		t.Run(dialect.name, func(t *testing.T) {
			translated, err := store.Translate(direct, dialect.dialect)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			if !strings.HasPrefix(translated.SQL, "SELECT ") || strings.Contains(translated.SQL, "IN (SELECT id FROM") {
				t.Fatalf("visible page SQL = %s\nwant a direct select from items", translated.SQL)
			}
			for _, fragment := range []string{"FROM items WHERE", "channel_list_entry", "ORDER BY"} {
				if !strings.Contains(translated.SQL, fragment) {
					t.Fatalf("visible page SQL = %s\nwant %q", translated.SQL, fragment)
				}
			}
			if !strings.Contains(translated.CountSQL, "visible_ids") {
				t.Fatalf("visible count SQL = %s\nwant the id relation", translated.CountSQL)
			}
			assertPlaceholderArgs(t, dialect.placeholder, translated.SQL, translated.Args)

			translated, err = store.Translate(adjacent, dialect.dialect)
			if err != nil {
				t.Fatalf("Translate adjacent: %v", err)
			}
			if !strings.Contains(translated.SQL, "IN (SELECT id FROM") {
				t.Fatalf("adjacent page SQL = %s\nwant the id relation", translated.SQL)
			}
		})
	}
}

func assertPlaceholderArgs(t *testing.T, placeholder, sql string, args []any) {
	t.Helper()
	if placeholder == "?" {
		if got := strings.Count(sql, "?"); got != len(args) {
			t.Fatalf("SQL has %d placeholders, args = %d: %s", got, len(args), sql)
		}
		return
	}
	for index := range args {
		if !strings.Contains(sql, "$"+strconv.Itoa(index+1)) {
			t.Fatalf("SQL lacks $%d for %d args: %s", index+1, len(args), sql)
		}
	}
	if strings.Contains(sql, "$"+strconv.Itoa(len(args)+1)) {
		t.Fatalf("SQL uses $%d beyond %d args: %s", len(args)+1, len(args), sql)
	}
}
