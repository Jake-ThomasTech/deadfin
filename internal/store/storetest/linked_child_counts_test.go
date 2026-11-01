// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"fmt"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	storetestpkg "github.com/deadfin-org/deadfin/internal/store/storetest"
)

// TestItemStoreLinkedChildCountsSQLite proves the linked child counts
// against SQLite.
func TestItemStoreLinkedChildCountsSQLite(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	runItemStoreLinkedChildCounts(t, stores)
}

// TestItemStoreLinkedChildCountsPostgres runs the same cases against
// PostgreSQL when DEADFIN_TEST_POSTGRES_DSN is configured.
func TestItemStoreLinkedChildCountsPostgres(t *testing.T) {
	runItemStoreLinkedChildCounts(t, storetestpkg.OpenPostgres(t))
}

// runItemStoreLinkedChildCounts ports ItemCountService.GetChildCountBatch
// (jellyfin@b04614e ItemCountService.cs:303-337): a BoxSet's ChildCount is
// the count of its linked children. A folder without linked children keeps
// its hierarchical count, and a Movie's alternate-version links do not count.
// deadfin-web shows Play on a collection only when ChildCount > 0 (#794).
func runItemStoreLinkedChildCounts(t *testing.T, stores store.Stores) {
	t.Helper()
	items := store.NewItemStore(stores)
	id := func(value int) dto.UUID {
		t.Helper()
		parsed, err := dto.ParseUUID(fmt.Sprintf("%032x", value))
		if err != nil {
			t.Fatalf("parse ID %d: %v", value, err)
		}
		return parsed
	}
	named := func(value int, kind item.Kind, parent *dto.UUID, folder bool) item.Item {
		name := fmt.Sprintf("%s %d", kind, value)
		return item.Item{ID: id(value), Kind: kind, ParentID: parent, Name: &name, SortName: &name, IsFolder: folder}
	}
	collections := named(1, item.KindFolder, nil, true)
	library := named(2, item.KindCollectionFolder, nil, true)
	boxSet := named(10, item.KindBoxSet, &collections.ID, true)
	empty := named(11, item.KindBoxSet, &collections.ID, true)
	first := named(20, item.KindMovie, &library.ID, false)
	second := named(21, item.KindMovie, &library.ID, false)
	version := named(22, item.KindMovie, &library.ID, false)
	second.LinkedChildren = []item.LinkedChildRef{{ItemID: version.ID, Type: item.LinkedChildTypeLocalAlternateVersion}}
	for _, value := range []item.Item{collections, library, boxSet, empty, first, version, second} {
		if err := items.Upsert(t.Context(), value); err != nil {
			t.Fatalf("upsert %s: %v", value.ID, err)
		}
	}
	if err := items.AddToCollection(t.Context(), boxSet.ID, []dto.UUID{first.ID, second.ID}); err != nil {
		t.Fatalf("AddToCollection: %v", err)
	}

	projections, err := items.DTOProjections(t.Context(), []dto.UUID{boxSet.ID, empty.ID, collections.ID, second.ID}, nil, false, false, true, false, nil)
	if err != nil {
		t.Fatalf("projections: %v", err)
	}
	for _, tc := range []struct {
		name string
		id   dto.UUID
		want int
	}{
		{name: "BoxSet counts its linked children", id: boxSet.ID, want: 2},
		{name: "empty BoxSet counts 0", id: empty.ID, want: 0},
		{name: "folder keeps its hierarchical count", id: collections.ID, want: 2},
		{name: "Movie does not count its alternate versions", id: second.ID, want: 0},
	} {
		if got := projections.ChildCounts[tc.id]; got != tc.want {
			t.Errorf("%s: ChildCounts = %d, want %d", tc.name, got, tc.want)
		}
	}
}
