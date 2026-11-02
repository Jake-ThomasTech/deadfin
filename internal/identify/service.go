// SPDX-License-Identifier: GPL-2.0-only

// Package identify implements the synchronous single-item identify/lookup
// service for the ItemLookup API surface (WP-P8-05). It is a direct service, not
// a scanner extension: upstream's identify flow (ItemLookupController.cs:253-280)
// is a synchronous single-item refresh, and a rescan rebuilds items from
// resolvers carrying only lifecycle dates and locks forward, so provider IDs
// applied before a rescan would be dropped, not honored.
package identify

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/imageserve"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/store"
)

// ErrItemNotFound is returned when the target item does not exist.
var ErrItemNotFound = errors.New("identify: item not found")

// itemUpdatePublisher is the narrow event seam Apply publishes through. It
// holds only the item-update mutation so tests can intercept the call at the
// service boundary while the production ChangeBus satisfies it.
type itemUpdatePublisher interface {
	PublishItemUpdated(parentID, collectionFolderID dto.UUID, ids []dto.UUID)
}

// Service is the direct identify/lookup service. It depends on existing seams:
// *store.ItemStore, *library.Topology, the refresh *providers.Registry, the new
// *providers.LookupRegistry, the remote-image cache + hardened downloader,
// *config.Manager, clock.Clock, and the library *library.ChangeBus.
type Service struct {
	items      *store.ItemStore
	topology   *library.Topology
	refresher  library.Refresher
	lookups    *providers.LookupRegistry
	remote     remoteImageStore
	downloader Downloader
	config     *config.Manager
	clock      clock.Clock
	publisher  itemUpdatePublisher
	logger     Logger
}

// remoteImageStore is the narrow seam the download path needs: Local serves a
// cached remote image (serving path) and Store persists validated bytes the
// caller already fetched. *imageserve.RemoteCache satisfies it.
type remoteImageStore interface {
	Local(ctx context.Context, url string) (string, time.Time, error)
	Store(url string, body []byte) (string, time.Time, error)
}

// Logger records credential-safe identify observations. Nil selects a no-op.
type Logger interface {
	LogAttrs(ctx context.Context, level Level, msg string, attrs ...Attr)
}

// Level aliases slog.Level so this package does not import log/slog.
type Level int

// Attr aliases slog.Attr.
type Attr struct {
	Key   string
	Value any
}

// NoopLogger discards all observations.
type NoopLogger struct{}

func (NoopLogger) LogAttrs(context.Context, Level, string, ...Attr) {}

// Options constructs a Service.
type Options struct {
	Items      *store.ItemStore
	Topology   *library.Topology
	Refresher  library.Refresher
	Lookups    *providers.LookupRegistry
	Remote     remoteImageStore
	Downloader Downloader
	Config     *config.Manager
	Clock      clock.Clock
	Changes    *library.ChangeBus
	Logger     Logger
}

// New constructs the identify service. A nil clock selects real. A nil Changes
// leaves the publisher seam unset, so Apply publishes nothing (nil behavior).
func New(options Options) *Service {
	clk := options.Clock
	if clk == nil {
		clk = clock.NewReal()
	}
	logger := options.Logger
	if logger == nil {
		logger = NoopLogger{}
	}
	svc := &Service{
		items:      options.Items,
		topology:   options.Topology,
		refresher:  options.Refresher,
		lookups:    options.Lookups,
		remote:     options.Remote,
		downloader: options.Downloader,
		config:     options.Config,
		clock:      clk,
		logger:     logger,
	}
	if options.Changes != nil {
		svc.publisher = options.Changes
	}
	return svc
}

// RemoteSearchQuery carries the typed search request from the handler.
type RemoteSearchQuery struct {
	Kind                     item.Kind
	Name                     string
	Year                     *int
	ProviderIDs              map[string]string
	Language                 string
	Country                  string
	AlbumArtists             []string
	SongInfos                []providers.SongInfo
	SeriesName               string
	ArtistIDs                map[string]string
	SearchProviderName       string
	IncludeDisabledProviders bool
	ItemID                   dto.UUID
}

// Search runs a remote search across enabled providers, merging results per
// upstream's ProviderManager semantics (ProviderManager.cs:944-996).
func (s *Service) Search(ctx context.Context, query RemoteSearchQuery) ([]providers.RemoteSearchResult, error) {
	if s.lookups == nil {
		return nil, nil
	}
	// Resolve library options for the enabled/order filter. When ItemID is
	// set, use that item's library options (ProviderManager.cs:949-957);
	// otherwise use zero-value LibraryOptions, mirroring upstream's dummy
	// new LibraryOptions() (ProviderManager.cs:920-933).
	var opts library.LibraryOptions
	if query.ItemID != (dto.UUID{}) {
		loaded, err := s.items.GetBatch(ctx, []dto.UUID{query.ItemID})
		if err != nil {
			return nil, err
		}
		if len(loaded) > 0 {
			opts, err = s.resolveLibraryOptions(ctx, &loaded[0])
			if err != nil {
				return nil, err
			}
		}
	} else {
		// Zero-value LibraryOptions has empty TypeOptions, so all providers
		// are enabled.
		opts = library.LibraryOptions{}
	}
	language, country := s.resolvedLanguageCountry(query.Language, query.Country)

	order, enabled := providerOrderAndEnabled(opts, query.Kind)

	searchers := providers.FilteredSearchers(
		s.lookups.Searchers(),
		query.Kind,
		order, enabled,
		query.IncludeDisabledProviders,
	)

	lookup := providers.Lookup{
		Kind:         query.Kind,
		Name:         query.Name,
		Year:         query.Year,
		ProviderIDs:  query.ProviderIDs,
		Language:     language,
		Country:      country,
		AlbumArtists: query.AlbumArtists,
		SongInfos:    query.SongInfos,
		SeriesName:   query.SeriesName,
		ArtistIDs:    query.ArtistIDs,
	}

	var perProvider [][]providers.RemoteSearchResult
	for _, searcher := range searchers {
		if query.SearchProviderName != "" && !strings.EqualFold(searcher.Name(), query.SearchProviderName) {
			continue
		}
		results, err := searcher.Search(ctx, lookup)
		if err != nil {
			// Per-provider errors are logged and swallowed
			// (ProviderManager.cs:990-996).
			s.logger.LogAttrs(ctx, Level(4), "identify: search provider error",
				Attr{Key: "provider", Value: searcher.Name()},
				Attr{Key: "error", Value: err.Error()})
			continue
		}
		// Tag results with SearchProviderName.
		for i := range results {
			results[i].SearchProviderName = searcher.Name()
		}
		perProvider = append(perProvider, results)
	}

	merged := providers.MergeSearchResults(perProvider)
	return merged, nil
}

// Apply applies a chosen remote-search result to an item: replaces provider IDs
// wholesale, re-fetches metadata via the existing Registry.Refresh, sets
// timestamps, persists via ItemStore.Upsert, and publishes an update event
// (ItemLookupController.cs:253-280, MetadataService.cs:270-275).
func (s *Service) Apply(ctx context.Context, itemID dto.UUID, result providers.RemoteSearchResult, replaceAllImages bool) error {
	// 1. Load the full item with satellites.
	loaded, err := s.items.GetBatch(ctx, []dto.UUID{itemID})
	if err != nil {
		return err
	}
	if len(loaded) == 0 {
		return ErrItemNotFound
	}
	target := &loaded[0]

	// 2. Replace ProviderIDs wholesale (ItemLookupController.cs:266).
	target.ProviderIDs = providers.ItemFromResultIDs(result.ProviderIDs)

	// 3. Seed Name/ProductionYear from the result (MetadataService.cs:270-275).
	if result.Name != nil && *result.Name != "" {
		target.Name = result.Name
	}
	if result.ProductionYear != nil {
		target.ProductionYear = result.ProductionYear
	}

	// 4. Resolve library options.
	opts, err := s.resolveLibraryOptions(ctx, target)
	if err != nil {
		return err
	}

	// 5. Build a RefreshContext from the item's on-disk location.
	refreshCtx, err := s.buildRefreshContext(ctx, target, opts, replaceAllImages)
	if err != nil {
		return err
	}

	// 6. Snapshot DateCreated before the refresh — the local NFO adapter can
	// overwrite it from dateadded (adapters.go:216), but the identify flow
	// must keep the original creation timestamp.
	originalDateCreated := target.DateCreated

	// 7. Run the existing Registry.Refresh on the work copy.
	// EnableInternetProviders is forced true for this context copy only.
	refreshCtx.Options.EnableInternetProviders = true
	if err := s.refresher.Refresh(ctx, refreshCtx, target); err != nil {
		return fmt.Errorf("identify: refresh: %w", err)
	}

	// 8. Restore DateCreated (the NFO adapter may have overwritten it).
	target.DateCreated = originalDateCreated

	// 9. Collapse duplicate credits and values, as the scanner does before
	// it stores a plan. A remote result can credit one person twice with
	// the same kind (two cast entries with one name, or Writer and
	// Screenplay), which the people_map key rejects (#803). Upstream
	// PeopleHelper.AddPerson merges such credits.
	library.NormalizeItemLists(target)

	// 10. Set DateLastRefreshed/DateLastSaved = now, keep DateCreated.
	now := s.clock.Now()
	target.DateLastRefreshed = &now
	target.DateLastSaved = &now

	// 11. Persist with ItemStore.Upsert.
	if err := s.items.Upsert(ctx, *target); err != nil {
		return fmt.Errorf("identify: persist: %w", err)
	}

	// 12. Publish an item-updated event.
	s.publishUpdate(target)
	return nil
}

// RemoteImagesQuery carries the remote-image listing request.
type RemoteImagesQuery struct {
	ItemID              dto.UUID
	Type                *item.ImageType
	ProviderName        string
	IncludeAllLanguages bool
	StartIndex          int
	Limit               *int
	Language            string
}

// RemoteImagesResult is the listing response.
type RemoteImagesResult struct {
	Images           []providers.RemoteImageInfo
	TotalRecordCount int
	Providers        []string
}

// RemoteImages lists available remote images for an item
// (RemoteImageController.cs:74-117). 404-check first; list via LookupRegistry;
// TotalRecordCount = filtered count before startIndex/limit slicing; Providers
// = names of all listers supporting the item (type-filtered when type given),
// distinct case-insensitive (RemoteImageController.cs:91-117). The Providers
// list is built from ALL listers, not the providerName-filtered set —
// providerName filters only the image results, not the provider listing.
func (s *Service) RemoteImages(ctx context.Context, query RemoteImagesQuery) (RemoteImagesResult, error) {
	// 404-check first.
	loaded, err := s.items.GetBatch(ctx, []dto.UUID{query.ItemID})
	if err != nil {
		return RemoteImagesResult{}, err
	}
	if len(loaded) == 0 {
		return RemoteImagesResult{}, ErrItemNotFound
	}
	target := loaded[0]

	if s.lookups == nil {
		return RemoteImagesResult{}, nil
	}

	allListers := providers.FilteredListers(s.lookups.Listers(), target.Kind)

	language := query.Language
	if language == "" {
		// Use the item's preferred metadata language, matching pinned stock
		// (ProviderManager.cs:313-315: item.GetPreferredMetadataLanguage()).
		// Fall back to the server system language when no owning library
		// exists or the option is unset (BaseItem.GetPreferredMetadataLanguage
		// falls through to the server configuration).
		opts, _ := s.resolveLibraryOptions(ctx, &target)
		if opts.PreferredMetadataLanguage != "" {
			language = opts.PreferredMetadataLanguage
		} else {
			language = s.systemLanguage()
		}
	}

	// Providers field: names of all listers supporting the item, type-filtered
	// when type is given, distinct case-insensitive (RemoteImageController.cs:91-117).
	// Built from ALL listers, not the providerName-filtered set.
	providerNames := make([]string, 0, len(allListers))
	seen := make(map[string]bool)
	for _, l := range allListers {
		if query.Type != nil {
			supported := false
			for _, t := range l.ImageTypes(target.Kind) {
				if t == *query.Type {
					supported = true
					break
				}
			}
			if !supported {
				continue
			}
		}
		name := l.Name()
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			providerNames = append(providerNames, name)
		}
	}

	// Apply the providerName filter to the listers used for image fetching only.
	listers := allListers
	if query.ProviderName != "" {
		listers = filterListersByName(listers, query.ProviderName)
	}

	// Fetch images from each provider, with type filter applied to the provider's
	// supported types when type is given.
	var allImages []providers.RemoteImageInfo
	for _, lister := range listers {
		if query.Type != nil {
			supported := false
			for _, t := range lister.ImageTypes(target.Kind) {
				if t == *query.Type {
					supported = true
					break
				}
			}
			if !supported {
				continue
			}
		}
		images, err := lister.Images(ctx, providers.ImageLookup{
			Kind:        target.Kind,
			Language:    language,
			ProviderIDs: providers.ItemToMap(target.ProviderIDs),
		})
		if err != nil {
			// Per-provider errors → empty list (ProviderManager.cs:358-366).
			s.logger.LogAttrs(ctx, Level(4), "identify: image provider error",
				Attr{Key: "provider", Value: lister.Name()},
				Attr{Key: "error", Value: err.Error()})
			continue
		}
		// Type filter on results.
		if query.Type != nil {
			images = providers.FilterImagesByType(images, *query.Type)
		}
		// Language filter.
		images = providers.FilterImagesByLanguage(images, language, query.IncludeAllLanguages)
		allImages = append(allImages, images...)
	}

	// Order per OrderByLanguageDescending (D-09).
	providers.SortRemoteImages(allImages, language)

	total := len(allImages)

	// Slice by startIndex/limit. Limit is *int so absent limit (nil) is
	// distinguishable from an explicit limit=0 (RemoteImageController.cs
	// uses nullable int? and Take(0)). A nil limit returns all images;
	// a non-nil limit returns at most max(*limit, 0) images.
	start := query.StartIndex
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := total
	if query.Limit != nil {
		limit := *query.Limit
		if limit < 0 {
			limit = 0
		}
		if start+limit < end {
			end = start + limit
		}
	}
	allImages = allImages[start:end]

	return RemoteImagesResult{
		Images:           allImages,
		TotalRecordCount: total,
		Providers:        providerNames,
	}, nil
}

// ImageProviderInfo is one entry in the RemoteImages/Providers response.
type ImageProviderInfo struct {
	Name            string
	SupportedImages []item.ImageType
}

// RemoteImageProviders lists image providers supporting the item
// (GetRemoteImageProviders).
func (s *Service) RemoteImageProviders(ctx context.Context, itemID dto.UUID) ([]ImageProviderInfo, error) {
	loaded, err := s.items.GetBatch(ctx, []dto.UUID{itemID})
	if err != nil {
		return nil, err
	}
	if len(loaded) == 0 {
		return nil, ErrItemNotFound
	}
	target := loaded[0]
	if s.lookups == nil {
		return nil, nil
	}
	listers := providers.FilteredListers(s.lookups.Listers(), target.Kind)
	out := make([]ImageProviderInfo, 0, len(listers))
	for _, l := range listers {
		out = append(out, ImageProviderInfo{
			Name:            l.Name(),
			SupportedImages: l.ImageTypes(target.Kind),
		})
	}
	return out, nil
}

// DownloadImage downloads a remote image and stores it on the item
// (RemoteImageController.cs:130-160). As in stock, any http(s) imageUrl is
// accepted; the hardened downloader refuses special-purpose destinations and
// bounds and validates the response (D-27). Non-Backdrop replaces the existing
// index-0 entry of that type; Backdrop appends at the next free index.
func (s *Service) DownloadImage(ctx context.Context, itemID dto.UUID, imageType item.ImageType, imageURL string) error {
	// 404-check.
	loaded, err := s.items.GetBatch(ctx, []dto.UUID{itemID})
	if err != nil {
		return err
	}
	if len(loaded) == 0 {
		return ErrItemNotFound
	}
	target := &loaded[0]

	if err := checkImageURL(imageURL); err != nil {
		return err
	}

	// Fetch through the hardened downloader: http(s) only, dial-time rejection
	// of special-purpose addresses on every redirect hop, response bounded and
	// validated as image content. The validated bytes are then persisted via
	// Store so a failed write persists nothing; the item's ImageInfo.Path
	// stores the remote URL, which the image pipeline serves through the same
	// cache (imageserve/service.go:212, handlers/images.go:286).
	if s.downloader == nil {
		return imageserve.ErrRemoteUnavailable
	}
	body, err := s.downloader.Fetch(ctx, imageURL)
	if err != nil {
		return fmt.Errorf("identify: download image: %w", err)
	}
	if s.remote == nil {
		return imageserve.ErrRemoteUnavailable
	}
	if _, _, err := s.remote.Store(imageURL, body); err != nil {
		return fmt.Errorf("identify: store image: %w", err)
	}

	// Set the item's ImageInfo for type.
	setItemImage(target, imageType, imageURL)

	if err := s.items.Upsert(ctx, *target); err != nil {
		return fmt.Errorf("identify: persist image: %w", err)
	}

	s.publishUpdate(target)
	return nil
}

// ErrInvalidImageURL is returned when the imageUrl is not an absolute http(s)
// URL. The error text names the reason.
var ErrInvalidImageURL = errors.New("invalid ImageUrl")

// checkImageURL accepts an absolute http(s) URL with a host.
func checkImageURL(imageURL string) error {
	if imageURL == "" {
		return fmt.Errorf("%w: ImageUrl is required", ErrInvalidImageURL)
	}
	if !providers.IsAllowedImageURL(imageURL) {
		return fmt.Errorf("%w: ImageUrl must be an http or https URL", ErrInvalidImageURL)
	}
	parsed, err := url.Parse(imageURL)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("%w: ImageUrl has no host", ErrInvalidImageURL)
	}
	return nil
}

// ExternalIDs returns the external-id descriptors for an item's kind
// (ProviderManager.cs:1002-1016).
func (s *Service) ExternalIDs(ctx context.Context, itemID dto.UUID) ([]providers.ExternalIDDescriptor, error) {
	loaded, err := s.items.GetBatch(ctx, []dto.UUID{itemID})
	if err != nil {
		return nil, err
	}
	if len(loaded) == 0 {
		return nil, ErrItemNotFound
	}
	target := loaded[0]
	return providers.GetExternalIDInfos(target.Kind), nil
}

// --- helpers ---

func (s *Service) resolveLibraryOptions(ctx context.Context, target *item.Item) (library.LibraryOptions, error) {
	if s.topology == nil || target.TopParentID == nil {
		return library.LibraryOptions{}, nil
	}
	folders, err := s.topology.VirtualFolders(ctx)
	if err != nil {
		return library.LibraryOptions{}, err
	}
	for _, folder := range folders {
		// Match on CollectionFolderID, not the registry ID. An item's
		// TopParentID chains up to the topmost *item* — the scanned
		// CollectionFolder (resolver.go:383-390) — and CollectionFolderID is
		// that scanned identity. topology.go:110-113 states the registry ID is
		// independent of it, so comparing folder.ID here never matched and
		// every item silently fell through to zero LibraryOptions.
		if folder.CollectionFolderID == *target.TopParentID {
			return folder.Options, nil
		}
	}
	// No match (Person, standalone entities) → zero-value LibraryOptions,
	// mirroring upstream's dummy new LibraryOptions().
	return library.LibraryOptions{}, nil
}

func (s *Service) buildRefreshContext(ctx context.Context, target *item.Item, opts library.LibraryOptions, replaceAllImages bool) (library.RefreshContext, error) {
	// Build the Directory, Entries, and ChildEntries the same way the
	// scanner does (scanner.go:900-913): for a folder item, Directory is
	// the folder itself and Entries is its own listing; for a file item,
	// Directory is the parent directory and Entries is the parent's
	// listing. ChildEntries carries the parent listing keyed by the
	// parent path, so local providers can resolve parent conventions
	// (such as season artwork) without another filesystem traversal —
	// matching the scanner's frozen child-entry fact
	// (scanner.go:907-912, resolver.go:139).
	dir := ""
	var entries []library.Entry
	childEntries := map[string][]library.Entry{}
	if target.Path != nil {
		itemPath := *target.Path
		if info, err := os.Stat(itemPath); err == nil && info.IsDir() {
			// Folder item: Directory is the folder itself, Entries is
			// its own listing, and ChildEntries is the parent listing
			// keyed by the parent path (scanner.go:911-912).
			dir = itemPath
			entries = readDirEntries(itemPath)
			parentDir := filepath.Dir(itemPath)
			childEntries[filepath.Clean(parentDir)] = readDirEntries(parentDir)
		} else {
			// File item (or missing path): Directory is the parent,
			// Entries is the parent's listing. ChildEntries is the
			// parent listing keyed by the parent path — the same
			// directoryChildren map the scanner builds for the
			// parent folder (scanner.go:622-636).
			dir = filepath.Dir(itemPath)
			entries = readDirEntries(dir)
			childEntries[filepath.Clean(dir)] = entries
		}
	}

	// LibraryRoot is the owning VirtualFolder's media path that contains the
	// item, not the item's own directory. This matches the scanner's
	// RefreshContext construction, where LibraryRoot is the media path the
	// scanner walks for the owning library. Fall back to the item's directory
	// when no owning VirtualFolder is found (Person, standalone entities).
	libraryRoot := dir
	if target.Path != nil && s.topology != nil && target.TopParentID != nil {
		if folders, err := s.topology.VirtualFolders(ctx); err == nil {
			for _, folder := range folders {
				// CollectionFolderID, not the registry ID — see
				// resolveLibraryOptions for why.
				if folder.CollectionFolderID == *target.TopParentID {
					for _, mediaPath := range folder.MediaPaths {
						if strings.HasPrefix(*target.Path, mediaPath+string(filepath.Separator)) {
							libraryRoot = mediaPath
							break
						}
					}
					break
				}
			}
		}
	}

	return library.RefreshContext{
		Directory: library.Entry{
			Path:  dir,
			IsDir: true,
		},
		Entries:            entries,
		ChildEntries:       childEntries,
		Options:            opts,
		LibraryRoot:        libraryRoot,
		Mode:               library.RefreshModeFullRefresh,
		Image:              library.RefreshModeFullRefresh,
		ReplaceAllMetadata: true,
		ReplaceAllImages:   replaceAllImages,
		// Identify does not run local metadata providers
		// (MetadataService.cs:768-769), so a stale NFO does not win
		// over the chosen search result.
		SkipLocalMetadata: true,
	}, nil
}

// readDirEntries reads the directory listing and converts to library.Entry
// values, mirroring the scanner's entry construction.
func readDirEntries(dir string) []library.Entry {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var entries []library.Entry
	for _, entry := range list {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		entries = append(entries, library.Entry{
			Path:     filepath.Join(dir, entry.Name()),
			IsDir:    entry.IsDir(),
			Size:     info.Size(),
			Created:  creationTime(info),
			Modified: info.ModTime(),
		})
	}
	return entries
}

func (s *Service) resolvedLanguageCountry(language, country string) (string, string) {
	if s.config != nil {
		system := s.config.System()
		if strings.TrimSpace(language) == "" {
			language = system.PreferredMetadataLanguage
		}
		if strings.TrimSpace(country) == "" {
			country = system.MetadataCountryCode
		}
	}
	return language, country
}

func (s *Service) systemLanguage() string {
	if s.config != nil {
		return s.config.System().PreferredMetadataLanguage
	}
	return "en"
}

func (s *Service) publishUpdate(target *item.Item) {
	if s.publisher == nil || target == nil {
		return
	}
	parentID := dto.NilUUID
	if target.ParentID != nil {
		parentID = *target.ParentID
	}
	collectionFolderID := dto.NilUUID
	if target.TopParentID != nil {
		collectionFolderID = *target.TopParentID
	}
	s.publisher.PublishItemUpdated(parentID, collectionFolderID, []dto.UUID{target.ID})
}

func setItemImage(target *item.Item, imageType item.ImageType, imageURL string) {
	// Non-Backdrop replaces the existing index-0 entry of that type.
	// Backdrop appends at the next free index (upstream null-index semantics).
	if imageType != item.ImageTypeBackdrop {
		for i := range target.ImageInfos {
			if target.ImageInfos[i].Type == imageType && target.ImageInfos[i].Index == 0 {
				target.ImageInfos[i].Path = imageURL
				return
			}
		}
		// No index-0 entry of this type; append at index 0.
		target.ImageInfos = append(target.ImageInfos, item.ImageInfo{
			Type:  imageType,
			Index: 0,
			Path:  imageURL,
		})
		return
	}
	// Backdrop: assign the next free index (upstream null-index semantics).
	usedIndices := make(map[int]bool)
	for _, info := range target.ImageInfos {
		if info.Type == item.ImageTypeBackdrop {
			usedIndices[info.Index] = true
		}
	}
	nextIndex := 0
	for usedIndices[nextIndex] {
		nextIndex++
	}
	target.ImageInfos = append(target.ImageInfos, item.ImageInfo{
		Type:  imageType,
		Index: nextIndex,
		Path:  imageURL,
	})
}

// providerOrderAndEnabled resolves the enabled-provider filter and ordering
// for a kind from the library's TypeOptions. When no TypeOptions exist for the
// kind, enabled is nil (all providers enabled, BaseItemManager.cs:46-47). When
// TypeOptions exist, enabled is MetadataFetchers — an empty (non-nil) slice
// means no providers are enabled, matching upstream's
// MetadataFetchers.Contains(name) on an empty collection. Order is
// MetadataFetcherOrder when present; nil (registration order) otherwise
// (ProviderManager.cs:597-607).
func providerOrderAndEnabled(opts library.LibraryOptions, kind item.Kind) (order, enabled []string) {
	for _, typeOpts := range opts.TypeOptions {
		if !strings.EqualFold(typeOpts.Type, kind.String()) {
			continue
		}
		// TypeOptions exist for this kind: MetadataFetchers drives the enabled
		// filter. Ensure it is non-nil so FilteredSearchers distinguishes
		// "explicit empty" (none enabled) from "absent" (all enabled).
		enabled = typeOpts.MetadataFetchers
		if enabled == nil {
			enabled = []string{}
		}
		if len(typeOpts.MetadataFetcherOrder) > 0 {
			return typeOpts.MetadataFetcherOrder, enabled
		}
		return nil, enabled
	}
	return nil, nil
}

func filterListersByName(listers []providers.ImageLister, name string) []providers.ImageLister {
	result := make([]providers.ImageLister, 0, len(listers))
	for _, l := range listers {
		if strings.EqualFold(l.Name(), name) {
			result = append(result, l)
		}
	}
	return result
}

func creationTime(info os.FileInfo) time.Time {
	return info.ModTime()
}
