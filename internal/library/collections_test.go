// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/naming"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

type collectionsHarness struct {
	root     string
	data     string
	topology *library.Topology
	items    *store.ItemStore
	scanner  *library.Scanner
	movies   map[string]item.Item
}

// newCollectionsHarness scans a movies library with four movies through the
// default resolver chain, with the collections path set.
func newCollectionsHarness(t *testing.T, autoCollections bool) *collectionsHarness {
	t.Helper()
	return newCollectionsHarnessWith(t, autoCollections, nil)
}

func newCollectionsHarnessWith(t *testing.T, autoCollections bool, refresher library.Refresher) *collectionsHarness {
	t.Helper()
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open stores: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	root := t.TempDir()
	topology, err := library.NewTopology(root, store.NewLibraryStore(stores))
	if err != nil {
		t.Fatalf("open topology: %v", err)
	}
	t.Cleanup(func() { _ = topology.Close() })
	media := filepath.Join(t.TempDir(), "movies")
	for _, name := range []string{"Alpha (1999)", "Bravo (2003)", "Charlie (2003)", "Delta (2010)"} {
		if err := os.MkdirAll(filepath.Join(media, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(media, name, name+".mkv"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	options := library.DefaultLibraryOptions()
	options.PathInfos = []library.MediaPathInfo{{Path: media}}
	options.AutomaticallyAddToCollection = autoCollections
	if _, err := topology.AddVirtualFolder(t.Context(), "Movies", "movies", options); err != nil {
		t.Fatalf("add library: %v", err)
	}
	items := store.NewItemStore(stores)
	if err := items.BootstrapSystemFolders(t.Context(), root); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	data := filepath.Join(root, "data")
	scanner, err := library.NewScanner(topology, items, library.NewDefaultChain(naming.DefaultOptions()), library.ScannerOptions{CollectionsPath: filepath.Join(data, "collections"), Refresher: refresher})
	if err != nil {
		t.Fatalf("new scanner: %v", err)
	}
	t.Cleanup(func() { _ = scanner.Close() })
	h := &collectionsHarness{root: root, data: data, topology: topology, items: items, scanner: scanner, movies: map[string]item.Item{}}
	h.scan(t)
	page, err := items.Query(t.Context(), store.ItemQuery{IncludeItemTypes: []item.Kind{item.KindMovie}, Recursive: true})
	if err != nil {
		t.Fatalf("list movies: %v", err)
	}
	// FindMovie names a self-contained movie after its directory, so
	// "Alpha (1999)" is keyed as "Alpha".
	for _, movie := range page.Items {
		name, _, _ := strings.Cut(*movie.Name, " (")
		h.movies[name] = movie
	}
	if len(h.movies) != 4 {
		t.Fatalf("scanned movies = %d, want 4", len(h.movies))
	}
	return h
}

func (h *collectionsHarness) scan(t *testing.T) {
	t.Helper()
	result, err := h.scanner.Scan(t.Context())
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("scan: %v %v", err, result.Errors)
	}
}

func (h *collectionsHarness) boxSets(t *testing.T) map[string]item.Item {
	t.Helper()
	page, err := h.items.Query(t.Context(), store.ItemQuery{IncludeItemTypes: []item.Kind{item.KindBoxSet}, Recursive: true})
	if err != nil {
		t.Fatalf("list collections: %v", err)
	}
	out := map[string]item.Item{}
	for _, value := range page.Items {
		out[*value.Name] = value
	}
	return out
}

func (h *collectionsHarness) members(t *testing.T, id dto.UUID) []dto.UUID {
	t.Helper()
	page, err := h.items.Query(t.Context(), store.ItemQuery{ParentID: &id})
	if err != nil {
		t.Fatalf("list collection members: %v", err)
	}
	out := make([]dto.UUID, 0, len(page.Items))
	for _, value := range page.Items {
		out = append(out, value.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func sortedIDs(values ...item.Item) []dto.UUID {
	out := make([]dto.UUID, 0, len(values))
	for _, value := range values {
		out = append(out, value.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// TestCreateCollectionSurvivesRescan ports CreateCollectionAsync and proves
// a scan finds the same Collections library rows and BoxSet, and keeps the
// BoxSet's members.
func TestCreateCollectionSurvivesRescan(t *testing.T) {
	h := newCollectionsHarness(t, false)
	created, err := h.scanner.CreateCollection(t.Context(), "Heist: Films", true)
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	directory := filepath.Join(h.data, "collections", "Heist  Films [boxset]")
	if created.Kind != item.KindBoxSet || created.Path == nil || *created.Path != directory || !created.IsLocked {
		t.Fatalf("created = %+v", created)
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("collection directory: %v", err)
	}
	folders, err := h.topology.VirtualFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var collections *library.VirtualFolder
	for index := range folders {
		if folders[index].Name == library.CollectionsLibraryName {
			collections = &folders[index]
		}
	}
	if collections == nil || collections.CollectionType != "boxsets" || !reflect.DeepEqual(collections.MediaPaths, []string{filepath.Join(h.data, "collections")}) {
		t.Fatalf("collections library = %+v", collections)
	}
	again, err := h.scanner.CreateCollection(t.Context(), "Heist: Films", false)
	if err != nil || again.ID != created.ID {
		t.Fatalf("second create = %s, %v; want the existing %s", again.ID, err, created.ID)
	}

	if err := h.items.AddToCollection(t.Context(), created.ID, []dto.UUID{h.movies["Alpha"].ID, h.movies["Delta"].ID}); err != nil {
		t.Fatalf("add to collection: %v", err)
	}
	h.scan(t)
	h.scan(t)
	boxSets := h.boxSets(t)
	if len(boxSets) != 1 {
		t.Fatalf("collections after rescan = %v", boxSets)
	}
	after := boxSets["Heist: Films"]
	if after.ID != created.ID || after.ParentID == nil || created.ParentID == nil || *after.ParentID != *created.ParentID || *after.TopParentID != *created.TopParentID {
		t.Fatalf("rescanned collection = %+v, created %+v", after, created)
	}
	if got, want := h.members(t, created.ID), sortedIDs(h.movies["Alpha"], h.movies["Delta"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("members after rescan = %v, want %v", got, want)
	}

	if err := h.items.RemoveFromCollection(t.Context(), created.ID, []dto.UUID{h.movies["Alpha"].ID, h.movies["Bravo"].ID}); err != nil {
		t.Fatalf("remove from collection: %v", err)
	}
	h.scan(t)
	if got, want := h.members(t, created.ID), sortedIDs(h.movies["Delta"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("members after remove = %v, want %v", got, want)
	}
}

// TestAddMoviesToCollections ports CollectionPostScanTask: a TMDb collection
// shared by two movies creates a BoxSet, a single movie joins only an
// existing BoxSet, and alternate versions are skipped.
func TestAddMoviesToCollections(t *testing.T) {
	h := newCollectionsHarness(t, true)
	setCollection := func(name, collection string, primary *dto.UUID) {
		movie, err := h.items.Get(t.Context(), h.movies[name].ID)
		if err != nil {
			t.Fatal(err)
		}
		if movie.Extra.Movie == nil {
			movie.Extra.Movie = &item.MovieExtra{}
		}
		movie.Extra.Movie.TmdbCollectionName = &collection
		movie.PrimaryVersionID = primary
		if err := h.items.Upsert(t.Context(), movie); err != nil {
			t.Fatal(err)
		}
	}
	alpha := h.movies["Alpha"].ID
	setCollection("Alpha", "Pair Collection", nil)
	setCollection("Bravo", "Pair Collection", nil)
	setCollection("Charlie", "Solo Collection", nil)
	setCollection("Delta", "Pair Collection", &alpha)

	if err := h.scanner.AddMoviesToCollections(t.Context(), nil); err != nil {
		t.Fatalf("auto collections: %v", err)
	}
	boxSets := h.boxSets(t)
	if len(boxSets) != 1 {
		t.Fatalf("collections = %v, want only Pair Collection", boxSets)
	}
	pair := boxSets["Pair Collection"]
	if got, want := h.members(t, pair.ID), sortedIDs(h.movies["Alpha"], h.movies["Bravo"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("pair members = %v, want %v", got, want)
	}

	solo, err := h.scanner.CreateCollection(t.Context(), "Solo Collection", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.scanner.AddMoviesToCollections(t.Context(), nil); err != nil {
		t.Fatalf("auto collections rerun: %v", err)
	}
	if got, want := h.members(t, solo.ID), sortedIDs(h.movies["Charlie"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("solo members = %v, want %v", got, want)
	}
	if got := len(h.members(t, pair.ID)); got != 2 {
		t.Fatalf("pair members after rerun = %d, want 2", got)
	}
}

// TestCreateCollectionReusesCaseVariantDirectory proves that a collection
// name that differs from an existing collection directory only in case
// returns the existing BoxSet and creates no second directory. Both paths
// have the same item ID (deadfin#805, divergence D-115).
func TestCreateCollectionReusesCaseVariantDirectory(t *testing.T) {
	h := newCollectionsHarness(t, false)
	existing, err := h.scanner.CreateCollection(t.Context(), "Men In Black Collection", false)
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	again, err := h.scanner.CreateCollection(t.Context(), "Men in Black Collection", false)
	if err != nil || again.ID != existing.ID {
		t.Fatalf("case-variant create = %s, %v; want the existing %s", again.ID, err, existing.ID)
	}
	entries, err := os.ReadDir(filepath.Join(h.data, "collections"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "Men In Black Collection [boxset]" {
		t.Fatalf("collection directories = %v, want only the existing one", entries)
	}
	h.scan(t)
}

// TestAddMoviesToCollectionsMatchesExistingCollection proves that the
// auto-collection task adds movies to an existing BoxSet whose name differs
// only in case, or whose Tmdb provider ID is the movies' TMDb collection ID,
// and creates no duplicate (deadfin#805, divergence D-115). The next scan
// has no errors.
func TestAddMoviesToCollectionsMatchesExistingCollection(t *testing.T) {
	h := newCollectionsHarness(t, true)
	setCollection := func(name, collection, tmdbID string) {
		movie, err := h.items.Get(t.Context(), h.movies[name].ID)
		if err != nil {
			t.Fatal(err)
		}
		if movie.Extra.Movie == nil {
			movie.Extra.Movie = &item.MovieExtra{}
		}
		movie.Extra.Movie.TmdbCollectionName = &collection
		if tmdbID != "" {
			movie.ProviderIDs.Set("TmdbCollection", tmdbID)
		}
		if err := h.items.Upsert(t.Context(), movie); err != nil {
			t.Fatal(err)
		}
	}
	mib, err := h.scanner.CreateCollection(t.Context(), "Men In Black Collection", false)
	if err != nil {
		t.Fatal(err)
	}
	downton, err := h.scanner.CreateCollection(t.Context(), "Downton Abbey Collection", false)
	if err != nil {
		t.Fatal(err)
	}
	downton.ProviderIDs.Set("Tmdb", "435259")
	if err := h.items.Upsert(t.Context(), downton); err != nil {
		t.Fatal(err)
	}
	setCollection("Alpha", "Men in Black Collection", "86055")
	setCollection("Bravo", "Men in Black Collection", "86055")
	setCollection("Charlie", "Downton Abbey (Films) Collection", "435259")
	setCollection("Delta", "Downton Abbey (Films) Collection", "435259")

	if err := h.scanner.AddMoviesToCollections(t.Context(), nil); err != nil {
		t.Fatalf("auto collections: %v", err)
	}
	if boxSets := h.boxSets(t); len(boxSets) != 2 {
		t.Fatalf("collections = %v, want only the two existing ones", boxSets)
	}
	if got, want := h.members(t, mib.ID), sortedIDs(h.movies["Alpha"], h.movies["Bravo"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("Men In Black members = %v, want %v", got, want)
	}
	if got, want := h.members(t, downton.ID), sortedIDs(h.movies["Charlie"], h.movies["Delta"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("Downton Abbey members = %v, want %v", got, want)
	}
	entries, err := os.ReadDir(filepath.Join(h.data, "collections"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("collection directories = %v, want 2", entries)
	}
	h.scan(t)
}

// boxSetTmdbRecorder records the Tmdb provider ID each BoxSet carries when
// the refresher sees it.
type boxSetTmdbRecorder struct{ seen map[string]string }

func (r *boxSetTmdbRecorder) Refresh(_ context.Context, _ library.RefreshContext, target *item.Item) error {
	if target.Kind == item.KindBoxSet && target.Name != nil {
		id, _ := target.ProviderIDs.Get("Tmdb")
		r.seen[*target.Name] = id
	}
	return nil
}

// TestScanResolvesBoxSetTmdbIDFromMembers proves that a scan gives a BoxSet
// without a Tmdb provider ID the TMDb collection ID most of its movies carry,
// before the providers run, and stores it (deadfin#808, divergence D-116). A
// BoxSet with a Tmdb ID keeps it.
func TestScanResolvesBoxSetTmdbIDFromMembers(t *testing.T) {
	recorder := &boxSetTmdbRecorder{seen: map[string]string{}}
	h := newCollectionsHarnessWith(t, false, recorder)
	setCollectionID := func(name, id string) {
		movie, err := h.items.Get(t.Context(), h.movies[name].ID)
		if err != nil {
			t.Fatal(err)
		}
		movie.ProviderIDs.Set("TmdbCollection", id)
		if err := h.items.Upsert(t.Context(), movie); err != nil {
			t.Fatal(err)
		}
	}
	setCollectionID("Alpha", "87118")
	setCollectionID("Bravo", "87118")
	setCollectionID("Charlie", "10")
	cars, err := h.scanner.CreateCollection(t.Context(), "Cars Collection", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.items.AddToCollection(t.Context(), cars.ID, []dto.UUID{h.movies["Alpha"].ID, h.movies["Bravo"].ID, h.movies["Charlie"].ID}); err != nil {
		t.Fatal(err)
	}
	kept, err := h.scanner.CreateCollection(t.Context(), "Kept Collection", false)
	if err != nil {
		t.Fatal(err)
	}
	kept.ProviderIDs.Set("Tmdb", "555")
	if err := h.items.Upsert(t.Context(), kept); err != nil {
		t.Fatal(err)
	}
	if err := h.items.AddToCollection(t.Context(), kept.ID, []dto.UUID{h.movies["Alpha"].ID}); err != nil {
		t.Fatal(err)
	}

	h.scan(t)
	if got := recorder.seen["Cars Collection"]; got != "87118" {
		t.Errorf("refresher saw Cars Collection Tmdb id %q, want 87118 from its members", got)
	}
	if got := recorder.seen["Kept Collection"]; got != "555" {
		t.Errorf("refresher saw Kept Collection Tmdb id %q, want the stored 555", got)
	}
	stored, err := h.items.Get(t.Context(), cars.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := stored.ProviderIDs.Get("Tmdb"); got != "87118" {
		t.Errorf("stored Tmdb id = %q, want 87118", got)
	}
}

// TestScanReplacesBoxSetMovieTmdbID proves that a scan corrects a BoxSet
// whose Tmdb ID is a member's movie ID (deadfin#846). With members that
// carry a TmdbCollection ID, the BoxSet takes it; without one, the movie ID
// is removed, so the provider searches by name. A member TmdbCollection ID
// that is the member's own movie ID is not used, and a BoxSet Tmdb ID that
// is no member's movie ID stays.
func TestScanReplacesBoxSetMovieTmdbID(t *testing.T) {
	recorder := &boxSetTmdbRecorder{seen: map[string]string{}}
	h := newCollectionsHarnessWith(t, false, recorder)
	setIDs := func(name, movieID, collectionID string) {
		movie, err := h.items.Get(t.Context(), h.movies[name].ID)
		if err != nil {
			t.Fatal(err)
		}
		movie.ProviderIDs.Set("Tmdb", movieID)
		if collectionID != "" {
			movie.ProviderIDs.Set("TmdbCollection", collectionID)
		}
		if err := h.items.Upsert(t.Context(), movie); err != nil {
			t.Fatal(err)
		}
	}
	setIDs("Alpha", "24", "2883")
	setIDs("Bravo", "393", "2883")
	setIDs("Charlie", "170", "170")
	collection := func(name, tmdbID string, members ...string) dto.UUID {
		created, err := h.scanner.CreateCollection(t.Context(), name, false)
		if err != nil {
			t.Fatal(err)
		}
		created.ProviderIDs.Set("Tmdb", tmdbID)
		if err := h.items.Upsert(t.Context(), created); err != nil {
			t.Fatal(err)
		}
		ids := make([]dto.UUID, 0, len(members))
		for _, member := range members {
			ids = append(ids, h.movies[member].ID)
		}
		if err := h.items.AddToCollection(t.Context(), created.ID, ids); err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	killBill := collection("Kill Bill Collection", "24", "Alpha", "Bravo")
	days := collection("28 Days Later Collection", "170", "Charlie")
	kept := collection("Kept Collection", "1565", "Charlie")

	h.scan(t)
	for name, want := range map[string]string{"Kill Bill Collection": "2883", "28 Days Later Collection": "", "Kept Collection": "1565"} {
		if got := recorder.seen[name]; got != want {
			t.Errorf("refresher saw %s Tmdb id %q, want %q", name, got, want)
		}
	}
	for id, want := range map[dto.UUID]string{killBill: "2883", kept: "1565"} {
		stored, err := h.items.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := stored.ProviderIDs.Get("Tmdb"); got != want {
			t.Errorf("stored Tmdb id of %s = %q, want %q", id, got, want)
		}
	}
	stored, err := h.items.Get(t.Context(), days)
	if err != nil {
		t.Fatal(err)
	}
	if got, found := stored.ProviderIDs.Get("Tmdb"); found {
		t.Errorf("stored Tmdb id of 28 Days Later Collection = %q, want none", got)
	}
}
