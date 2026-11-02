// SPDX-License-Identifier: GPL-2.0-only

package identify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

func identifyTestInstant() time.Time {
	return time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
}

// fakeRefresher is a no-op Refresher that records the refresh call.
type fakeRefresher struct {
	called   bool
	received *item.Item
}

func (f *fakeRefresher) Refresh(ctx context.Context, rc library.RefreshContext, target *item.Item) error {
	f.called = true
	f.received = target
	return nil
}

// fakeSearcher returns canned results for search tests.
type fakeSearcher struct {
	name    string
	kind    item.Kind
	results []providers.RemoteSearchResult
	err     error
}

func (f fakeSearcher) Name() string                    { return f.name }
func (f fakeSearcher) SearchSupports(k item.Kind) bool { return k == f.kind }
func (f fakeSearcher) Search(_ context.Context, _ providers.Lookup) ([]providers.RemoteSearchResult, error) {
	return f.results, f.err
}

// fakeImageLister returns canned image results.
type fakeImageLister struct {
	name   string
	kind   item.Kind
	types  []item.ImageType
	images []providers.RemoteImageInfo
	err    error
}

func (f fakeImageLister) Name() string                            { return f.name }
func (f fakeImageLister) ImageSupports(k item.Kind) bool          { return k == f.kind }
func (f fakeImageLister) ImageTypes(k item.Kind) []item.ImageType { return f.types }
func (f fakeImageLister) Images(_ context.Context, _ providers.ImageLookup) ([]providers.RemoteImageInfo, error) {
	return f.images, f.err
}

// fakeDownloader returns canned bytes for the download path.
type fakeDownloader struct {
	body []byte
	err  error
}

func (f fakeDownloader) Fetch(_ context.Context, _ string) ([]byte, error) {
	return f.body, f.err
}

// fakeRemoteStore records Store calls and returns canned results.
type fakeRemoteStore struct {
	storeErr error
	stored   bool
}

func (f *fakeRemoteStore) Local(_ context.Context, _ string) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (f *fakeRemoteStore) Store(_ string, _ []byte) (string, time.Time, error) {
	if f.storeErr != nil {
		return "", time.Time{}, f.storeErr
	}
	f.stored = true
	return "/cache/img", identifyTestInstant(), nil
}

func ptr(s string) *string { return &s }
func intptr(i int) *int    { return &i }

func mustUUID(s string) dto.UUID {
	id, err := dto.ParseUUID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func newProviderIDs(kv ...string) item.ProviderIDs {
	var p item.ProviderIDs
	for i := 0; i+1 < len(kv); i += 2 {
		p.Set(kv[i], kv[i+1])
	}
	return p
}

func setupTestItems(t *testing.T) *store.ItemStore {
	t.Helper()
	items, _ := setupTestItemsAndStores(t)
	return items
}

// setupTestItemsAndStores is setupTestItems plus the underlying Stores, so a
// test can build a real library.Topology over the same database instead of
// leaving Service.topology nil.
func setupTestItemsAndStores(t *testing.T) (*store.ItemStore, store.Stores) {
	t.Helper()
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { stores.Close() })
	items := store.NewItemStore(stores)
	if err := items.BootstrapSystemFolders(t.Context(), t.TempDir()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return items, stores
}

// TestApplyReplacesProviderIDsAndRefreshes verifies Apply replaces provider IDs
// wholesale (removing stale IDs that are absent from the result), runs the
// refresher, restores locked fields, sets timestamps, and persists.
func TestApplyReplacesProviderIDsAndRefreshes(t *testing.T) {
	items := setupTestItems(t)
	fakeClock := clock.NewFake(identifyTestInstant())

	// A remote-metadata provider that overwrites the locked Name field during
	// Refresh. The real providers.Registry snapshots locked metadata before
	// each non-image provider runs and restores it afterward
	// (registry.go:89-99). This test proves that locked-field restoration works
	// through the real Registry, not a fake refresher.
	overwritingProvider := &remoteMetadataOverwriter{
		name:    "Overwriter",
		newName: ptr("Overwritten By Provider"),
	}
	registry := providers.NewRegistry(overwritingProvider)

	// Create a movie item with a stale provider ID (Imdb=stale) that the result
	// does not carry. Apply replaces ProviderIDs wholesale, so the stale ID must
	// be gone after Apply. The item also has a locked Name field so the
	// registry's locked-field restore can be verified.
	moviePath := filepath.Join(t.TempDir(), "movie.mp4")
	os.WriteFile(moviePath, []byte("data"), 0o644)
	lockedName := "Locked Original Title"
	movie := &item.Item{
		ID:           mustUUID("aabbccddeeff00112233445566778899"),
		Kind:         item.KindMovie,
		Name:         ptr(lockedName),
		Path:         &moviePath,
		ProviderIDs:  newProviderIDs("Tmdb", "999", "Imdb", "stale"),
		LockedFields: []item.MetadataField{item.MetadataFieldName},
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	svc := New(Options{
		Items:     items,
		Refresher: registry,
		Clock:     fakeClock,
	})

	result := providers.RemoteSearchResult{
		Name:           ptr("New Title"),
		ProviderIDs:    map[string]string{"Tmdb": "123"},
		ProductionYear: intptr(2024),
	}

	if err := svc.Apply(context.Background(), movie.ID, result, true); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Verify the item was persisted with replaced provider IDs.
	loaded, err := items.GetBatch(context.Background(), []dto.UUID{movie.ID})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("item not found after Apply")
	}
	got := loaded[0]
	if v, ok := got.ProviderIDs.Get("Tmdb"); !ok || v != "123" {
		t.Errorf("ProviderIDs[Tmdb] = %q, want 123 (replaced)", v)
	}
	// Stale ID removal: Imdb was on the item but not in the result, so it must
	// be gone after wholesale replacement (ItemLookupController.cs:266).
	if _, ok := got.ProviderIDs.Get("Imdb"); ok {
		t.Error("ProviderIDs[Imdb] still present, want removed (stale ID replaced wholesale)")
	}
	// Locked-field restoration: Apply seeds Name from the result before refresh.
	// The overwriting provider tries to overwrite it, but the locked-field
	// restore inside providers.Registry reverts the provider's overwrite to the
	// pre-provider value (the seeded value). This proves the locked-field
	// snapshot/restore mechanism works through the real Registry, not a fake.
	if got.Name == nil || *got.Name != "New Title" {
		t.Errorf("Name = %v, want %q (locked field restored after provider overwrite)", got.Name, "New Title")
	}
	if got.ProductionYear == nil || *got.ProductionYear != 2024 {
		t.Errorf("ProductionYear = %v, want 2024", got.ProductionYear)
	}
	if got.DateLastRefreshed == nil {
		t.Error("DateLastRefreshed not set")
	}
	// Verify the overwriting provider ran — the locked-field restore only
	// matters if the provider actually overwrote the field.
	if !overwritingProvider.called {
		t.Error("overwriting provider was not called by the registry")
	}
}

// remoteMetadataOverwriter is a remote-metadata provider that overwrites the
// item Name during Refresh. It proves the providers.Registry locked-field
// snapshot/restore mechanism works through Apply.
type remoteMetadataOverwriter struct {
	name    string
	newName *string
	called  bool
}

func (p *remoteMetadataOverwriter) Name() string { return p.name }
func (p *remoteMetadataOverwriter) Stage() providers.ProviderStage {
	return providers.ProviderStageRemoteMetadata
}
func (p *remoteMetadataOverwriter) Refresh(_ context.Context, _ library.RefreshContext, target *item.Item) (bool, error) {
	p.called = true
	target.Name = p.newName
	return false, nil
}

// TestApplyItemNotFound verifies Apply returns ErrItemNotFound for a missing item.
func TestApplyItemNotFound(t *testing.T) {
	items := setupTestItems(t)
	svc := New(Options{
		Items:     items,
		Refresher: &fakeRefresher{},
		Clock:     clock.NewFake(identifyTestInstant()),
	})
	err := svc.Apply(context.Background(), mustUUID("aabbccddeeff00112233445566778899"), providers.RemoteSearchResult{}, true)
	if !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("Apply(missing) err = %v, want ErrItemNotFound", err)
	}
}

// TestDownloadBackdropAssignsNextFreeIndex verifies a Backdrop download
// appends at the next free index, not index 0.
func TestDownloadBackdropAssignsNextFreeIndex(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:   mustUUID("aabbccddeeff00112233445566778899"),
		Kind: item.KindMovie,
		Name: ptr("Movie"),
		ImageInfos: []item.ImageInfo{
			{Type: item.ImageTypeBackdrop, Index: 0, Path: "https://cdn.example.com/backdrop0.jpg"},
		},
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// The lister returns the new backdrop URL as a provider-listed image.
	lister := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary, item.ImageTypeBackdrop},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/backdrop0.jpg", Type: item.ImageTypeBackdrop},
			{URL: "https://cdn.example.com/backdrop1.jpg", Type: item.ImageTypeBackdrop},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister})

	svc := New(Options{
		Items:      items,
		Lookups:    lookups,
		Downloader: fakeDownloader{body: []byte("img")},
		Remote:     &fakeRemoteStore{},
		Clock:      clock.NewFake(identifyTestInstant()),
	})

	err := svc.DownloadImage(context.Background(), movie.ID, item.ImageTypeBackdrop, "https://cdn.example.com/backdrop1.jpg")
	if err != nil {
		t.Fatalf("DownloadImage: %v", err)
	}

	loaded, err := items.GetBatch(context.Background(), []dto.UUID{movie.ID})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	got := loaded[0]
	var backdropIndices []int
	for _, info := range got.ImageInfos {
		if info.Type == item.ImageTypeBackdrop {
			backdropIndices = append(backdropIndices, info.Index)
		}
	}
	if len(backdropIndices) != 2 {
		t.Fatalf("backdrop count = %d, want 2", len(backdropIndices))
	}
	// The new backdrop should be at index 1, not 0.
	found1 := false
	for _, idx := range backdropIndices {
		if idx == 1 {
			found1 = true
		}
	}
	if !found1 {
		t.Errorf("new backdrop index = %v, want 1 (next free)", backdropIndices)
	}
}

// TestDownloadAcceptsUnlistedURL verifies a URL that no provider listed for
// the item is downloaded and set, as in stock (#809). The item has no
// provider IDs and the registry has no listers.
func TestDownloadAcceptsUnlistedURL(t *testing.T) {
	items := setupTestItems(t)
	series := &item.Item{
		ID:   mustUUID("aabbccddeeff00112233445566778899"),
		Kind: item.KindSeries,
		Name: ptr("TableTop"),
	}
	if err := items.Upsert(context.Background(), *series); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	remoteStore := &fakeRemoteStore{}
	svc := New(Options{
		Items:      items,
		Lookups:    providers.NewLookupRegistry(nil, nil),
		Downloader: fakeDownloader{body: []byte("img")},
		Remote:     remoteStore,
		Clock:      clock.NewFake(identifyTestInstant()),
	})

	const imageURL = "http://artworks.example.com/banners/posters/257942-1.jpg"
	if err := svc.DownloadImage(context.Background(), series.ID, item.ImageTypePrimary, imageURL); err != nil {
		t.Fatalf("DownloadImage(unlisted) err = %v, want nil", err)
	}
	if !remoteStore.stored {
		t.Error("remote store was not written")
	}
	loaded, err := items.GetBatch(context.Background(), []dto.UUID{series.ID})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	var primary string
	for _, info := range loaded[0].ImageInfos {
		if info.Type == item.ImageTypePrimary {
			primary = info.Path
		}
	}
	if primary != imageURL {
		t.Errorf("Primary path = %q, want %q", primary, imageURL)
	}
}

// TestDownloadRejectsInvalidURL verifies a missing, non-http(s), or hostless
// imageUrl fails with ErrInvalidImageURL and a reason, before any fetch.
func TestDownloadRejectsInvalidURL(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:   mustUUID("aabbccddeeff00112233445566778899"),
		Kind: item.KindMovie,
		Name: ptr("Movie"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	remoteStore := &fakeRemoteStore{}
	svc := New(Options{
		Items:      items,
		Downloader: fakeDownloader{body: []byte("img")},
		Remote:     remoteStore,
		Clock:      clock.NewFake(identifyTestInstant()),
	})

	for raw, reason := range map[string]string{
		"":                       "ImageUrl is required",
		"file:///etc/passwd":     "must be an http or https URL",
		"poster.jpg":             "must be an http or https URL",
		"https:///no-host.jpg":   "has no host",
		"ftp://example.com/x.jp": "must be an http or https URL",
	} {
		err := svc.DownloadImage(context.Background(), movie.ID, item.ImageTypePrimary, raw)
		if !errors.Is(err, ErrInvalidImageURL) || !strings.Contains(err.Error(), reason) {
			t.Errorf("DownloadImage(%q) err = %v, want ErrInvalidImageURL with %q", raw, err, reason)
		}
	}
	if remoteStore.stored {
		t.Error("remote store was written for a rejected URL")
	}
}

// TestDownloadRefusedKeepsReason verifies a downloader refusal keeps the
// ErrImageRefused identity through DownloadImage, and nothing is stored.
func TestDownloadRefusedKeepsReason(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:   mustUUID("aabbccddeeff00112233445566778899"),
		Kind: item.KindMovie,
		Name: ptr("Movie"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	remoteStore := &fakeRemoteStore{}
	refusal := fmt.Errorf("%w: special-purpose address 10.0.0.1", ErrImageRefused)
	svc := New(Options{
		Items:      items,
		Downloader: fakeDownloader{err: refusal},
		Remote:     remoteStore,
		Clock:      clock.NewFake(identifyTestInstant()),
	})
	err := svc.DownloadImage(context.Background(), movie.ID, item.ImageTypePrimary, "https://internal.example/x.jpg")
	if !errors.Is(err, ErrImageRefused) || !strings.Contains(err.Error(), "special-purpose address") {
		t.Fatalf("DownloadImage err = %v, want ErrImageRefused with reason", err)
	}
	if remoteStore.stored {
		t.Error("remote store was written for a refused download")
	}
}

// TestRemoteImagesPrimaryFilter verifies the Primary type filter works —
// ImageTypePrimary == 0, so it must be distinguishable from "no filter".
func TestRemoteImagesPrimaryFilter(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	primaryType := item.ImageTypePrimary
	lister := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary, item.ImageTypeBackdrop},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/poster.jpg", Type: item.ImageTypePrimary},
			{URL: "https://cdn.example.com/backdrop.jpg", Type: item.ImageTypeBackdrop},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister})

	svc := New(Options{
		Items:   items,
		Lookups: lookups,
		Clock:   clock.NewFake(identifyTestInstant()),
	})

	result, err := svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID: movie.ID,
		Type:   &primaryType,
	})
	if err != nil {
		t.Fatalf("RemoteImages: %v", err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("images = %d, want 1 (Primary filter)", len(result.Images))
	}
	if result.Images[0].Type != item.ImageTypePrimary {
		t.Errorf("image type = %v, want Primary", result.Images[0].Type)
	}
}

// TestRemoteImagesNoTypeFilterReturnsAll verifies no type filter returns all images.
func TestRemoteImagesNoTypeFilterReturnsAll(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	lister := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary, item.ImageTypeBackdrop},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/poster.jpg", Type: item.ImageTypePrimary},
			{URL: "https://cdn.example.com/backdrop.jpg", Type: item.ImageTypeBackdrop},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister})

	svc := New(Options{
		Items:   items,
		Lookups: lookups,
		Clock:   clock.NewFake(identifyTestInstant()),
	})

	result, err := svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID: movie.ID,
		Type:   nil, // no filter
	})
	if err != nil {
		t.Fatalf("RemoteImages: %v", err)
	}
	if len(result.Images) != 2 {
		t.Fatalf("images = %d, want 2 (no filter)", len(result.Images))
	}
}

// TestRemoteImagesProviderFilterDoesNotAffectProvidersList verifies the
// providerName filter affects only images, not the Providers list.
func TestRemoteImagesProviderFilterDoesNotAffectProvidersList(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	lister1 := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/tmdb.jpg", Type: item.ImageTypePrimary},
		},
	}
	lister2 := fakeImageLister{
		name:  "OMDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/omdb.jpg", Type: item.ImageTypePrimary},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister1, lister2})

	svc := New(Options{
		Items:   items,
		Lookups: lookups,
		Clock:   clock.NewFake(identifyTestInstant()),
	})

	result, err := svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID:       movie.ID,
		ProviderName: "TheMovieDb",
	})
	if err != nil {
		t.Fatalf("RemoteImages: %v", err)
	}
	// Providers should list BOTH providers (not filtered by providerName).
	if len(result.Providers) != 2 {
		t.Fatalf("Providers = %v, want 2 (providerName does not filter Providers list)", result.Providers)
	}
	// But images should only be from TheMovieDb.
	if len(result.Images) != 1 {
		t.Fatalf("images = %d, want 1 (providerName filters images)", len(result.Images))
	}
}

// TestRemoteImagesIncludeAllLanguages verifies the includeAllLanguages flag
// disables the language filter.
func TestRemoteImagesIncludeAllLanguages(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	lister := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/en.jpg", Type: item.ImageTypePrimary, Language: "en"},
			{URL: "https://cdn.example.com/ja.jpg", Type: item.ImageTypePrimary, Language: "ja"},
			{URL: "https://cdn.example.com/null.jpg", Type: item.ImageTypePrimary, Language: ""},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister})

	svc := New(Options{
		Items:   items,
		Lookups: lookups,
		Clock:   clock.NewFake(identifyTestInstant()),
	})

	// Without includeAllLanguages: only en and languageless images.
	result, err := svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID:   movie.ID,
		Language: "en",
	})
	if err != nil {
		t.Fatalf("RemoteImages: %v", err)
	}
	if len(result.Images) != 2 {
		t.Fatalf("without includeAllLanguages: images = %d, want 2 (en + null)", len(result.Images))
	}

	// With includeAllLanguages: all three images.
	result, err = svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID:              movie.ID,
		Language:            "en",
		IncludeAllLanguages: true,
	})
	if err != nil {
		t.Fatalf("RemoteImages: %v", err)
	}
	if len(result.Images) != 3 {
		t.Fatalf("with includeAllLanguages: images = %d, want 3", len(result.Images))
	}
}

// TestRemoteImagesPreferredItemLanguage verifies the service uses the owning
// library's preferred metadata language when query.Language is empty, matching
// pinned stock (ProviderManager.cs:313-315). French must rank first; Japanese
// must appear only when all languages are requested.
func TestRemoteImagesPreferredItemLanguage(t *testing.T) {
	items, stores := setupTestItemsAndStores(t)

	mediaRoot := t.TempDir()
	moviePath := filepath.Join(mediaRoot, "movie.mp4")
	os.WriteFile(moviePath, []byte("data"), 0o644)

	topology, err := library.NewTopology(t.TempDir(), store.NewLibraryStore(stores))
	if err != nil {
		t.Fatalf("new topology: %v", err)
	}
	t.Cleanup(func() { topology.Close() })
	owningOptions := library.LibraryOptions{
		PreferredMetadataLanguage: "fr",
		MetadataCountryCode:       "FR",
	}
	virtual, err := topology.AddVirtualFolder(context.Background(), "Movies", "movies", owningOptions)
	if err != nil {
		t.Fatalf("add virtual folder: %v", err)
	}
	if err := topology.AddMediaPath(context.Background(), virtual.ID, mediaRoot); err != nil {
		t.Fatalf("add media path: %v", err)
	}

	collectionFolder := &item.Item{
		ID:       virtual.CollectionFolderID,
		Kind:     item.KindCollectionFolder,
		Name:     ptr("Movies"),
		Path:     &mediaRoot,
		IsFolder: true,
	}
	if err := items.Upsert(context.Background(), *collectionFolder); err != nil {
		t.Fatalf("upsert collection folder: %v", err)
	}

	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		Path:        &moviePath,
		ParentID:    &virtual.CollectionFolderID,
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	lister := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/en.jpg", Type: item.ImageTypePrimary, Language: "en"},
			{URL: "https://cdn.example.com/fr.jpg", Type: item.ImageTypePrimary, Language: "fr"},
			{URL: "https://cdn.example.com/ja.jpg", Type: item.ImageTypePrimary, Language: "ja"},
			{URL: "https://cdn.example.com/null.jpg", Type: item.ImageTypePrimary, Language: ""},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister})

	svc := New(Options{
		Items:    items,
		Topology: topology,
		Lookups:  lookups,
		Clock:    clock.NewFake(identifyTestInstant()),
	})

	// Language is empty, so the service resolves it from the owning library's
	// PreferredMetadataLanguage ("fr"), not the server system language ("en").
	result, err := svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID: movie.ID,
	})
	if err != nil {
		t.Fatalf("RemoteImages: %v", err)
	}
	// Without includeAllLanguages: only fr, en, and languageless images.
	// Japanese is filtered out because it is neither the preferred language nor "en".
	if len(result.Images) != 3 {
		t.Fatalf("images = %d, want 3 (fr + en + languageless; ja filtered)", len(result.Images))
	}
	// French must rank first (score 4), English second (score 3), languageless third (score 2).
	if result.Images[0].Language != "fr" {
		t.Errorf("first image language = %q, want fr (preferred language ranks first)", result.Images[0].Language)
	}
	if result.Images[1].Language != "en" {
		t.Errorf("second image language = %q, want en", result.Images[1].Language)
	}
	if result.Images[2].Language != "" {
		t.Errorf("third image language = %q, want empty (languageless ranks last)", result.Images[2].Language)
	}

	// With includeAllLanguages: all four images, French still first.
	result, err = svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID:              movie.ID,
		IncludeAllLanguages: true,
	})
	if err != nil {
		t.Fatalf("RemoteImages (includeAllLanguages): %v", err)
	}
	if len(result.Images) != 4 {
		t.Fatalf("with includeAllLanguages: images = %d, want 4", len(result.Images))
	}
	if result.Images[0].Language != "fr" {
		t.Errorf("with includeAllLanguages: first image language = %q, want fr", result.Images[0].Language)
	}
}

// TestRemoteImagesAbsentAndZeroLimit verifies absent limit returns all images
// while an explicit limit=0 returns an empty Images array with TotalRecordCount
// equal to the full filtered count.
func TestRemoteImagesAbsentAndZeroLimit(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	lister := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/a.jpg", Type: item.ImageTypePrimary, Language: "en"},
			{URL: "https://cdn.example.com/b.jpg", Type: item.ImageTypePrimary, Language: "en"},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister})

	svc := New(Options{
		Items:   items,
		Lookups: lookups,
		Clock:   clock.NewFake(identifyTestInstant()),
	})

	// Absent limit: all images.
	result, err := svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID: movie.ID,
	})
	if err != nil {
		t.Fatalf("RemoteImages (absent limit): %v", err)
	}
	if len(result.Images) != 2 {
		t.Fatalf("absent limit: images = %d, want 2", len(result.Images))
	}
	if result.TotalRecordCount != 2 {
		t.Errorf("absent limit: TotalRecordCount = %d, want 2", result.TotalRecordCount)
	}
	if len(result.Providers) != 1 {
		t.Errorf("absent limit: Providers = %d, want 1", len(result.Providers))
	}

	// Explicit limit=0: empty Images array, but TotalRecordCount and Providers
	// are the pre-slice values.
	zero := 0
	result, err = svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID: movie.ID,
		Limit:  &zero,
	})
	if err != nil {
		t.Fatalf("RemoteImages (limit=0): %v", err)
	}
	if len(result.Images) != 0 {
		t.Errorf("limit=0: images = %d, want 0", len(result.Images))
	}
	if result.TotalRecordCount != 2 {
		t.Errorf("limit=0: TotalRecordCount = %d, want 2 (pre-slice)", result.TotalRecordCount)
	}
	if len(result.Providers) != 1 {
		t.Errorf("limit=0: Providers = %d, want 1 (pre-slice)", len(result.Providers))
	}
}

// TestSearchMergeResultsTwoSearchers verifies two searchers with an
// overlapping provider ID produce one merged result with SearchProviderName
// from the first.
func TestSearchMergeResultsTwoSearchers(t *testing.T) {
	items := setupTestItems(t)
	searcher1 := fakeSearcher{
		name: "TheMovieDb",
		kind: item.KindMovie,
		results: []providers.RemoteSearchResult{
			{Name: ptr("Movie A"), ProviderIDs: map[string]string{"Tmdb": "111"}, SearchProviderName: "TheMovieDb"},
		},
	}
	searcher2 := fakeSearcher{
		name: "IMDb",
		kind: item.KindMovie,
		results: []providers.RemoteSearchResult{
			{Name: ptr("Movie A"), ProviderIDs: map[string]string{"Tmdb": "111", "Imdb": "tt111"}, SearchProviderName: "IMDb"},
		},
	}
	lookups := providers.NewLookupRegistry([]providers.Searcher{searcher1, searcher2}, nil)

	svc := New(Options{
		Items:   items,
		Lookups: lookups,
		Clock:   clock.NewFake(identifyTestInstant()),
	})

	results, err := svc.Search(context.Background(), RemoteSearchQuery{
		Kind: item.KindMovie,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (merged by Tmdb:111)", len(results))
	}
	if results[0].SearchProviderName != "TheMovieDb" {
		t.Errorf("SearchProviderName = %q, want TheMovieDb (first provider)", results[0].SearchProviderName)
	}
	if results[0].ProviderIDs["Imdb"] != "tt111" {
		t.Errorf("Imdb not merged: %v", results[0].ProviderIDs)
	}
}

// TestRemoteImagesRatingOrder verifies images are ordered by CommunityRating
// (rounded to 1 decimal) descending, then VoteCount descending.
func TestRemoteImagesRatingOrder(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	lister := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/low.jpg", Type: item.ImageTypePrimary, CommunityRating: 5.0, VoteCount: 100, Language: "en"},
			{URL: "https://cdn.example.com/high.jpg", Type: item.ImageTypePrimary, CommunityRating: 8.5, VoteCount: 200, Language: "en"},
			{URL: "https://cdn.example.com/mid.jpg", Type: item.ImageTypePrimary, CommunityRating: 8.5, VoteCount: 50, Language: "en"},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister})

	svc := New(Options{
		Items:   items,
		Lookups: lookups,
		Clock:   clock.NewFake(identifyTestInstant()),
	})

	result, err := svc.RemoteImages(context.Background(), RemoteImagesQuery{
		ItemID:   movie.ID,
		Language: "en",
	})
	if err != nil {
		t.Fatalf("RemoteImages: %v", err)
	}
	if len(result.Images) != 3 {
		t.Fatalf("images = %d, want 3", len(result.Images))
	}
	if result.Images[0].CommunityRating != 8.5 {
		t.Errorf("first image rating = %v, want 8.5", result.Images[0].CommunityRating)
	}
	if result.Images[0].VoteCount != 200 {
		t.Errorf("first image vote = %v, want 200 (higher vote wins)", result.Images[0].VoteCount)
	}
	if result.Images[1].VoteCount != 50 {
		t.Errorf("second image vote = %v, want 50", result.Images[1].VoteCount)
	}
	if result.Images[2].CommunityRating != 5.0 {
		t.Errorf("third image rating = %v, want 5.0 (lower rating last)", result.Images[2].CommunityRating)
	}
}

// TestSearchItemlessUsesZeroLibraryOptions verifies that a search without
// ItemID uses zero-value LibraryOptions, so all providers are enabled.
func TestSearchItemlessUsesZeroLibraryOptions(t *testing.T) {
	items := setupTestItems(t)
	searcher := fakeSearcher{
		name: "TheMovieDb",
		kind: item.KindMovie,
		results: []providers.RemoteSearchResult{
			{Name: ptr("Movie"), ProviderIDs: map[string]string{"Tmdb": "1"}},
		},
	}
	lookups := providers.NewLookupRegistry([]providers.Searcher{searcher}, nil)

	svc := New(Options{
		Items:   items,
		Lookups: lookups,
		Clock:   clock.NewFake(identifyTestInstant()),
	})

	results, err := svc.Search(context.Background(), RemoteSearchQuery{
		Kind: item.KindMovie,
		Name: "Movie",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("itemless search results = %d, want 1 (all providers enabled)", len(results))
	}
}

// TestApplyRefreshContextFolderItem verifies Apply builds a scanner-compatible
// RefreshContext for a folder item: Directory is the folder itself, not its
// parent.
func TestApplyRefreshContextFolderItem(t *testing.T) {
	items := setupTestItems(t)
	fakeClock := clock.NewFake(identifyTestInstant())
	refresher := &folderRefreshCapturer{}

	folderPath := t.TempDir()
	os.MkdirAll(folderPath, 0o755)
	folder := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie Folder"),
		Path:        &folderPath,
		ProviderIDs: newProviderIDs("Tmdb", "999"),
	}
	if err := items.Upsert(context.Background(), *folder); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	svc := New(Options{
		Items:     items,
		Refresher: refresher,
		Clock:     fakeClock,
	})

	result := providers.RemoteSearchResult{
		Name:           ptr("New Title"),
		ProviderIDs:    map[string]string{"Tmdb": "123"},
		ProductionYear: intptr(2024),
	}

	if err := svc.Apply(context.Background(), folder.ID, result, true); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !refresher.called {
		t.Fatal("refresher not called")
	}
	if refresher.ctx.Directory.Path != folderPath {
		t.Errorf("Directory.Path = %q, want %q (folder itself, not parent)", refresher.ctx.Directory.Path, folderPath)
	}
	if !refresher.ctx.Directory.IsDir {
		t.Error("Directory.IsDir = false, want true")
	}
}

// TestApplyRefreshContextFileItem verifies Apply builds a scanner-compatible
// RefreshContext for a file item: Directory is the parent directory.
func TestApplyRefreshContextFileItem(t *testing.T) {
	items := setupTestItems(t)
	fakeClock := clock.NewFake(identifyTestInstant())
	refresher := &folderRefreshCapturer{}

	parentDir := t.TempDir()
	filePath := filepath.Join(parentDir, "movie.mp4")
	os.WriteFile(filePath, []byte("data"), 0o644)
	folder := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie File"),
		Path:        &filePath,
		ProviderIDs: newProviderIDs("Tmdb", "999"),
	}
	if err := items.Upsert(context.Background(), *folder); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	svc := New(Options{
		Items:     items,
		Refresher: refresher,
		Clock:     fakeClock,
	})

	result := providers.RemoteSearchResult{
		Name:           ptr("New Title"),
		ProviderIDs:    map[string]string{"Tmdb": "123"},
		ProductionYear: intptr(2024),
	}

	if err := svc.Apply(context.Background(), folder.ID, result, true); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !refresher.called {
		t.Fatal("refresher not called")
	}
	if refresher.ctx.Directory.Path != parentDir {
		t.Errorf("Directory.Path = %q, want %q (parent directory)", refresher.ctx.Directory.Path, parentDir)
	}
	if !refresher.ctx.Directory.IsDir {
		t.Error("Directory.IsDir = false, want true")
	}
}

// TestRemoteImagesProvenanceAllowsListedURL verifies a provider-listed URL
// passes the provenance check and the image is downloaded and cached.
func TestRemoteImagesProvenanceAllowsListedURL(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	lister := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/poster.jpg", Type: item.ImageTypePrimary},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister})
	remoteStore := &fakeRemoteStore{}

	svc := New(Options{
		Items:      items,
		Lookups:    lookups,
		Downloader: fakeDownloader{body: []byte("img")},
		Remote:     remoteStore,
		Clock:      clock.NewFake(identifyTestInstant()),
	})

	err := svc.DownloadImage(context.Background(), movie.ID, item.ImageTypePrimary, "https://cdn.example.com/poster.jpg")
	if err != nil {
		t.Fatalf("DownloadImage(listed) err = %v, want nil", err)
	}
	if !remoteStore.stored {
		t.Error("remote store not written for a listed URL")
	}
}

// folderRefreshCapturer records the RefreshContext for inspection.
type folderRefreshCapturer struct {
	called bool
	ctx    library.RefreshContext
}

func (f *folderRefreshCapturer) Refresh(_ context.Context, rc library.RefreshContext, _ *item.Item) error {
	f.called = true
	f.ctx = rc
	return nil
}

// dateCreatedMutatingRefresher simulates the NFO adapter overwriting DateCreated
// during refresh (adapters.go:216). It records the value before refresh and
// replaces DateCreated with a different timestamp.
type dateCreatedMutatingRefresher struct {
	called      bool
	original    *time.Time
	overwritten time.Time
}

func (f *dateCreatedMutatingRefresher) Refresh(_ context.Context, _ library.RefreshContext, target *item.Item) error {
	f.called = true
	f.original = target.DateCreated
	// Simulate the NFO adapter overwriting DateCreated from dateadded.
	overwritten := f.overwritten
	target.DateCreated = &overwritten
	return nil
}

// TestApplyPreservesDateCreatedAcrossRefresh verifies Apply snapshots DateCreated
// before the refresh and restores it after, so the NFO adapter's dateadded
// overwrite does not persist.
func TestApplyPreservesDateCreatedAcrossRefresh(t *testing.T) {
	items := setupTestItems(t)
	fakeClock := clock.NewFake(identifyTestInstant())
	original := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	overwritten := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	refresher := &dateCreatedMutatingRefresher{overwritten: overwritten}

	moviePath := filepath.Join(t.TempDir(), "movie.mp4")
	os.WriteFile(moviePath, []byte("data"), 0o644)
	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		Path:        &moviePath,
		ProviderIDs: newProviderIDs("Tmdb", "999"),
		DateCreated: &original,
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	svc := New(Options{
		Items:     items,
		Refresher: refresher,
		Clock:     fakeClock,
	})

	result := providers.RemoteSearchResult{
		Name:        ptr("New Title"),
		ProviderIDs: map[string]string{"Tmdb": "123"},
	}

	if err := svc.Apply(context.Background(), movie.ID, result, true); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !refresher.called {
		t.Fatal("refresher not called")
	}
	if refresher.original == nil || !refresher.original.Equal(original) {
		t.Errorf("refresher saw DateCreated = %v, want %v (snapshot before refresh)", refresher.original, original)
	}

	// The persisted item must retain the original DateCreated.
	loaded, err := items.GetBatch(context.Background(), []dto.UUID{movie.ID})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("item not found after Apply")
	}
	got := loaded[0]
	if got.DateCreated == nil || !got.DateCreated.Equal(original) {
		t.Errorf("DateCreated = %v, want %v (preserved across refresh)", got.DateCreated, original)
	}
	// DateLastRefreshed must advance.
	if got.DateLastRefreshed == nil {
		t.Error("DateLastRefreshed not set")
	}
}

// TestDownloadImageReplacesNonBackdropIndexZero verifies non-Backdrop download
// replaces only the index-0 entry of that type, not a higher-index row that
// happens to precede index 0.
func TestDownloadImageReplacesNonBackdropIndexZero(t *testing.T) {
	items := setupTestItems(t)
	movie := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie"),
		ProviderIDs: newProviderIDs("Tmdb", "1"),
	}
	// Insert a Primary at index 1 BEFORE index 0, simulating a higher-index
	// row that precedes index 0 in the slice.
	primary1 := "https://cdn.example.com/primary1.jpg"
	primary0 := "https://cdn.example.com/primary0.jpg"
	movie.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypePrimary, Index: 1, Path: primary1},
		{Type: item.ImageTypePrimary, Index: 0, Path: primary0},
	}
	if err := items.Upsert(context.Background(), *movie); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	lister := fakeImageLister{
		name:  "TheMovieDb",
		kind:  item.KindMovie,
		types: []item.ImageType{item.ImageTypePrimary},
		images: []providers.RemoteImageInfo{
			{URL: "https://cdn.example.com/new-poster.jpg", Type: item.ImageTypePrimary},
		},
	}
	lookups := providers.NewLookupRegistry(nil, []providers.ImageLister{lister})
	remoteStore := &fakeRemoteStore{}
	newURL := "https://cdn.example.com/new-poster.jpg"

	svc := New(Options{
		Items:      items,
		Lookups:    lookups,
		Downloader: fakeDownloader{body: []byte("img")},
		Remote:     remoteStore,
		Clock:      clock.NewFake(identifyTestInstant()),
	})

	if err := svc.DownloadImage(context.Background(), movie.ID, item.ImageTypePrimary, newURL); err != nil {
		t.Fatalf("DownloadImage: %v", err)
	}

	loaded, err := items.GetBatch(context.Background(), []dto.UUID{movie.ID})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	got := loaded[0]

	// The index-0 entry must be replaced; the index-1 entry must be unchanged.
	var idx0, idx1 *item.ImageInfo
	for i := range got.ImageInfos {
		if got.ImageInfos[i].Type == item.ImageTypePrimary && got.ImageInfos[i].Index == 0 {
			idx0 = &got.ImageInfos[i]
		}
		if got.ImageInfos[i].Type == item.ImageTypePrimary && got.ImageInfos[i].Index == 1 {
			idx1 = &got.ImageInfos[i]
		}
	}
	if idx0 == nil {
		t.Fatal("index-0 Primary not found after download")
	}
	if idx0.Path != newURL {
		t.Errorf("index-0 Path = %q, want %q (replaced)", idx0.Path, newURL)
	}
	if idx1 == nil {
		t.Fatal("index-1 Primary lost after download")
	}
	if idx1.Path != primary1 {
		t.Errorf("index-1 Path = %q, want %q (unchanged)", idx1.Path, primary1)
	}
}

// TestApplyRefreshContextFolderAndNestedFile verifies Apply builds a
// scanner-equivalent RefreshContext for both folder and file items: Directory,
// Entries, parent ChildEntries keyed by the parent path, owning VirtualFolder
// LibraryRoot, copied options with EnableInternetProviders forced true, full
// refresh modes, ReplaceAllMetadata, and query-controlled ReplaceAllImages.
func TestApplyRefreshContextFolderAndNestedFile(t *testing.T) {
	items, stores := setupTestItemsAndStores(t)
	fakeClock := clock.NewFake(identifyTestInstant())
	refresher := &folderRefreshCapturer{}

	// --- Folder item ---
	// mediaRoot is the library's media path; the movie folder sits under it, so
	// LibraryRoot (mediaRoot) is distinguishable from the item's own directory.
	mediaRoot := t.TempDir()
	folderPath := filepath.Join(mediaRoot, "Movie Folder (2024)")
	if err := os.MkdirAll(folderPath, 0o755); err != nil {
		t.Fatalf("mkdir folder: %v", err)
	}
	// Create a child file so the folder has a non-empty listing.
	os.WriteFile(filepath.Join(folderPath, "child.mp4"), []byte("data"), 0o644)
	// Create a sibling dir so the parent listing is non-empty.
	parentDir := filepath.Dir(folderPath)

	// A real Topology with an owning VirtualFolder whose MediaPaths contains the
	// item. Both assertions below depend on it: LibraryRoot is resolved from the
	// owning folder's media path (service.go buildRefreshContext), and Options are
	// copied from that folder (resolveLibraryOptions, matched by TopParentID).
	// The options are deliberately non-default so a copy is distinguishable from
	// the zero value — EnableInternetProviders is left false here precisely so the
	// forced-true assertion proves the override rather than the default.
	topology, err := library.NewTopology(t.TempDir(), store.NewLibraryStore(stores))
	if err != nil {
		t.Fatalf("new topology: %v", err)
	}
	t.Cleanup(func() { topology.Close() })
	owningOptions := library.LibraryOptions{
		EnableInternetProviders:   false,
		PreferredMetadataLanguage: "fr",
		MetadataCountryCode:       "FR",
		SaveLocalMetadata:         true,
	}
	virtual, err := topology.AddVirtualFolder(context.Background(), "Movies", "movies", owningOptions)
	if err != nil {
		t.Fatalf("add virtual folder: %v", err)
	}
	if err := topology.AddMediaPath(context.Background(), virtual.ID, mediaRoot); err != nil {
		t.Fatalf("add media path: %v", err)
	}

	// The scanned CollectionFolder item for this library. Its ID is the
	// deterministic path-derived identity the topology reports as
	// CollectionFolderID. It has to exist as a real ancestor: the store *derives*
	// top_parent_id from the ancestor chain (item_store_scan.go:240-246) and
	// overwrites whatever a caller passes, so an item with a hand-set TopParentID
	// and no parent would silently get its own ID back and prove nothing.
	collectionFolder := &item.Item{
		ID:       virtual.CollectionFolderID,
		Kind:     item.KindCollectionFolder,
		Name:     ptr("Movies"),
		Path:     &mediaRoot,
		IsFolder: true,
	}
	if err := items.Upsert(context.Background(), *collectionFolder); err != nil {
		t.Fatalf("upsert collection folder: %v", err)
	}

	folder := &item.Item{
		ID:          mustUUID("aabbccddeeff00112233445566778899"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie Folder"),
		Path:        &folderPath,
		IsFolder:    true,
		ParentID:    &virtual.CollectionFolderID,
		ProviderIDs: newProviderIDs("Tmdb", "999"),
	}
	if err := items.Upsert(context.Background(), *folder); err != nil {
		t.Fatalf("upsert folder: %v", err)
	}

	svc := New(Options{
		Items:     items,
		Refresher: refresher,
		Topology:  topology,
		Clock:     fakeClock,
	})

	result := providers.RemoteSearchResult{
		Name:           ptr("New Title"),
		ProviderIDs:    map[string]string{"Tmdb": "123"},
		ProductionYear: intptr(2024),
	}

	if err := svc.Apply(context.Background(), folder.ID, result, true); err != nil {
		t.Fatalf("Apply (folder): %v", err)
	}
	if !refresher.called {
		t.Fatal("refresher not called for folder")
	}
	rc := refresher.ctx
	if rc.Directory.Path != folderPath {
		t.Errorf("folder Directory.Path = %q, want %q", rc.Directory.Path, folderPath)
	}
	if !rc.Directory.IsDir {
		t.Error("folder Directory.IsDir = false, want true")
	}
	if len(rc.Entries) == 0 {
		t.Error("folder Entries empty, want the folder's own listing")
	}
	// ChildEntries must be keyed by the parent path with the parent listing.
	parentKey := filepath.Clean(parentDir)
	children, ok := rc.ChildEntries[parentKey]
	if !ok {
		t.Errorf("folder ChildEntries missing key %q; keys: %v", parentKey, rc.ChildEntries)
	} else if len(children) == 0 {
		t.Errorf("folder ChildEntries[%q] is empty, want the parent listing", parentKey)
	}
	if !rc.Options.EnableInternetProviders {
		t.Error("folder Options.EnableInternetProviders = false, want true (forced for identify)")
	}
	if rc.Mode != library.RefreshModeFullRefresh {
		t.Errorf("folder Mode = %v, want FullRefresh", rc.Mode)
	}
	if rc.Image != library.RefreshModeFullRefresh {
		t.Errorf("folder Image = %v, want FullRefresh", rc.Image)
	}
	if !rc.ReplaceAllMetadata {
		t.Error("folder ReplaceAllMetadata = false, want true")
	}
	if !rc.ReplaceAllImages {
		t.Error("folder ReplaceAllImages = false, want true (query default)")
	}
	// LibraryRoot and the copied options — the two things this test named in its
	// doc comment but never asserted (the old body discarded its opts with
	// `_ = opts`). Authorized rework, scope itemlookup/composed-acceptance@v1.
	if rc.LibraryRoot != mediaRoot {
		t.Errorf("folder LibraryRoot = %q, want %q (owning VirtualFolder media path)", rc.LibraryRoot, mediaRoot)
	}
	if rc.Options.PreferredMetadataLanguage != "fr" || rc.Options.MetadataCountryCode != "FR" || !rc.Options.SaveLocalMetadata {
		t.Errorf("folder Options not copied from the owning VirtualFolder: got lang=%q country=%q saveLocal=%v, want fr/FR/true",
			rc.Options.PreferredMetadataLanguage, rc.Options.MetadataCountryCode, rc.Options.SaveLocalMetadata)
	}

	// --- File item ---
	refresher.called = false
	refresher.ctx = library.RefreshContext{}
	fileDir := t.TempDir()
	filePath := filepath.Join(fileDir, "movie.mp4")
	os.WriteFile(filePath, []byte("data"), 0o644)
	file := &item.Item{
		ID:          mustUUID("bbccddee11223344556677889900aaff"),
		Kind:        item.KindMovie,
		Name:        ptr("Movie File"),
		Path:        &filePath,
		ProviderIDs: newProviderIDs("Tmdb", "888"),
	}
	if err := items.Upsert(context.Background(), *file); err != nil {
		t.Fatalf("upsert file: %v", err)
	}

	if err := svc.Apply(context.Background(), file.ID, result, false); err != nil {
		t.Fatalf("Apply (file): %v", err)
	}
	if !refresher.called {
		t.Fatal("refresher not called for file")
	}
	rc = refresher.ctx
	if rc.Directory.Path != fileDir {
		t.Errorf("file Directory.Path = %q, want %q (parent dir)", rc.Directory.Path, fileDir)
	}
	if !rc.Directory.IsDir {
		t.Error("file Directory.IsDir = false, want true")
	}
	if len(rc.Entries) == 0 {
		t.Error("file Entries empty, want the parent listing")
	}
	// ChildEntries keyed by the parent dir with the parent listing.
	fileParentKey := filepath.Clean(fileDir)
	if _, ok := rc.ChildEntries[fileParentKey]; !ok {
		t.Errorf("file ChildEntries missing key %q; keys: %v", fileParentKey, rc.ChildEntries)
	}
	if rc.ReplaceAllImages {
		t.Error("file ReplaceAllImages = true, want false (query-controlled)")
	}
}

// recordLogger captures LogAttrs calls for observability tests. It satisfies
// the Logger interface.
type recordLogger struct {
	records []logRecord
}

type logRecord struct {
	level Level
	msg   string
	attrs []Attr
}

func (r *recordLogger) LogAttrs(_ context.Context, level Level, msg string, attrs ...Attr) {
	r.records = append(r.records, logRecord{level: level, msg: msg, attrs: attrs})
}

// findAttr returns the Value for the first Attr with the given Key.
func findAttr(attrs []Attr, key string) (any, bool) {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return nil, false
}

// TestProviderErrorsAreLogged verifies that a provider error produces one
// Level(4) log record with the provider name and error text, and that the
// service continues and returns results from providers without errors.
func TestProviderErrorsAreLogged(t *testing.T) {
	t.Run("search", func(t *testing.T) {
		items := setupTestItems(t)
		rec := &recordLogger{}

		errProvider := fakeSearcher{
			name: "FaultySearch",
			kind: item.KindMovie,
			err:  errors.New("search backend offline"),
		}
		okProvider := fakeSearcher{
			name: "TheMovieDb",
			kind: item.KindMovie,
			results: []providers.RemoteSearchResult{
				{Name: ptr("Real Movie"), ProviderIDs: map[string]string{"Tmdb": "1"}},
			},
		}
		lookups := providers.NewLookupRegistry(
			[]providers.Searcher{errProvider, okProvider},
			nil,
		)

		svc := New(Options{
			Items:   items,
			Lookups: lookups,
			Clock:   clock.NewFake(identifyTestInstant()),
			Logger:  rec,
		})

		results, err := svc.Search(context.Background(), RemoteSearchQuery{
			Kind: item.KindMovie,
			Name: "Real Movie",
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}

		if len(rec.records) != 1 {
			t.Fatalf("log records = %d, want 1", len(rec.records))
		}
		got := rec.records[0]
		if got.level != Level(4) {
			t.Errorf("level = %v, want %v", got.level, Level(4))
		}
		if got.msg != "identify: search provider error" {
			t.Errorf("msg = %q, want %q", got.msg, "identify: search provider error")
		}
		provider, ok := findAttr(got.attrs, "provider")
		if !ok || provider != "FaultySearch" {
			t.Errorf("provider attr = %v, want FaultySearch", provider)
		}
		errText, ok := findAttr(got.attrs, "error")
		if !ok || errText != "search backend offline" {
			t.Errorf("error attr = %v, want search backend offline", errText)
		}

		if len(results) != 1 {
			t.Fatalf("results = %d, want 1 (from the provider without an error)", len(results))
		}
		if results[0].Name == nil || *results[0].Name != "Real Movie" {
			t.Errorf("result name = %v, want Real Movie", results[0].Name)
		}
	})

	t.Run("remote_images", func(t *testing.T) {
		items := setupTestItems(t)
		rec := &recordLogger{}

		movie := &item.Item{
			ID:   mustUUID("aabbccddeeff00112233445566778899"),
			Kind: item.KindMovie,
			Name: ptr("Movie"),
		}
		if err := items.Upsert(context.Background(), *movie); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		errLister := fakeImageLister{
			name:  "FaultyImages",
			kind:  item.KindMovie,
			types: []item.ImageType{item.ImageTypePrimary},
			err:   errors.New("image service unavailable"),
		}
		okLister := fakeImageLister{
			name:  "TheMovieDb",
			kind:  item.KindMovie,
			types: []item.ImageType{item.ImageTypePrimary},
			images: []providers.RemoteImageInfo{
				{URL: "https://cdn.example.com/primary.jpg", Type: item.ImageTypePrimary, Language: "en"},
			},
		}
		lookups := providers.NewLookupRegistry(
			nil,
			[]providers.ImageLister{errLister, okLister},
		)

		svc := New(Options{
			Items:   items,
			Lookups: lookups,
			Clock:   clock.NewFake(identifyTestInstant()),
			Logger:  rec,
		})

		result, err := svc.RemoteImages(context.Background(), RemoteImagesQuery{
			ItemID:   movie.ID,
			Language: "en",
		})
		if err != nil {
			t.Fatalf("RemoteImages: %v", err)
		}

		if len(rec.records) != 1 {
			t.Fatalf("log records = %d, want 1", len(rec.records))
		}
		got := rec.records[0]
		if got.level != Level(4) {
			t.Errorf("level = %v, want %v", got.level, Level(4))
		}
		if got.msg != "identify: image provider error" {
			t.Errorf("msg = %q, want %q", got.msg, "identify: image provider error")
		}
		provider, ok := findAttr(got.attrs, "provider")
		if !ok || provider != "FaultyImages" {
			t.Errorf("provider attr = %v, want FaultyImages", provider)
		}
		errText, ok := findAttr(got.attrs, "error")
		if !ok || errText != "image service unavailable" {
			t.Errorf("error attr = %v, want image service unavailable", errText)
		}

		if len(result.Images) != 1 {
			t.Fatalf("images = %d, want 1 (from the provider without an error)", len(result.Images))
		}
		if result.Images[0].URL != "https://cdn.example.com/primary.jpg" {
			t.Errorf("image URL = %q, want https://cdn.example.com/primary.jpg", result.Images[0].URL)
		}
	})
}
