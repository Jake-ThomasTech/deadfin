// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	dfpath "github.com/deadfin-org/deadfin/internal/path"
	"github.com/deadfin-org/deadfin/internal/store"
)

// CollectionsLibraryName is the name of the boxsets library that holds
// collections (the "Collections" localization string).
const CollectionsLibraryName = "Collections"

// ErrNoCollectionsPath reports a scanner configured without a collections
// path.
var ErrNoCollectionsPath = errors.New("library: collections path is not configured")

// CreateCollection ports CollectionManager.CreateCollectionAsync: it ensures
// the Collections library (EnsureLibraryFolder), creates the
// "<name> [boxset]" directory in it, and stores the BoxSet under the same
// identity, parent, and top parent a scan gives that directory. An existing
// BoxSet at that path is returned unchanged, and no collection.xml is
// written (divergence D-39).
func (s *Scanner) CreateCollection(ctx context.Context, name string, locked bool) (item.Item, error) {
	folder, err := s.collectionsFolder(ctx, true)
	if err != nil {
		return item.Item{}, err
	}
	root, collection, err := s.materializeCollectionsRoot(ctx, folder)
	if err != nil {
		return item.Item{}, err
	}
	directory, err := collectionDirectory(*root.Path, dfpath.ValidFilename(name)+" [boxset]")
	if err != nil {
		return item.Item{}, err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return item.Item{}, fmt.Errorf("create collection directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return item.Item{}, fmt.Errorf("stat collection directory: %w", err)
	}
	entry := Entry{Path: normalizedEntryPath(directory), IsDir: true, Created: info.ModTime().UTC(), Modified: info.ModTime().UTC()}
	resolved, err := s.chain.Resolve(ctx, ResolveContext{
		Entry:          entry,
		Parent:         &root,
		CollectionType: collection,
		Options:        folder.Options,
		LibraryRoot:    *root.Path,
	})
	if err != nil {
		return item.Item{}, fmt.Errorf("resolve collection directory: %w", err)
	}
	if resolved == nil || resolved.Kind != item.KindBoxSet {
		return item.Item{}, fmt.Errorf("resolve collection directory %q: not a BoxSet", directory)
	}
	if existing, err := s.items.Get(ctx, resolved.ID); err == nil {
		return existing, nil
	} else if !store.IsNotFound(err) {
		return item.Item{}, err
	}
	value := *resolved
	value.Name = &name
	value.IsLocked = locked
	created, modified := entry.Created, entry.Modified
	value.DateCreated, value.DateModified = &created, &modified
	materializeScannedSortName(&value, item.DefaultSortOptions())
	values := []item.Item{value}
	materializePresentationKeys(values, root, nil)
	if err := s.storeWrite(ctx, func() error { return s.items.Upsert(ctx, values[0]) }); err != nil {
		return item.Item{}, fmt.Errorf("store collection: %w", err)
	}
	return values[0], nil
}

// collectionDirectory returns the path of the collection directory base in
// root. Item IDs hash the lowercased path, so a directory whose name differs
// from base only in case has the same item ID. When root has such a
// directory, collectionDirectory returns it instead of a new path, so the
// two directories never collide in a scan (divergence D-115).
func collectionDirectory(root, base string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read collections directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == base {
			return filepath.Join(root, base), nil
		}
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.EqualFold(entry.Name(), base) {
			return filepath.Join(root, entry.Name()), nil
		}
	}
	return filepath.Join(root, base), nil
}

// collectionsFolder ports EnsureLibraryFolder: it finds the library whose
// media path is the collections path and, with create, adds it as a boxsets
// library.
func (s *Scanner) collectionsFolder(ctx context.Context, create bool) (VirtualFolder, error) {
	path := s.options.CollectionsPath
	if path == "" {
		return VirtualFolder{}, ErrNoCollectionsPath
	}
	path = normalizedEntryPath(path)
	folders, err := s.topology.VirtualFolders(ctx)
	if err != nil {
		return VirtualFolder{}, err
	}
	for _, folder := range folders {
		for _, mediaPath := range folder.MediaPaths {
			if normalizedEntryPath(mediaPath) == path {
				return folder, nil
			}
		}
	}
	if !create {
		return VirtualFolder{}, store.ErrNotFound
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return VirtualFolder{}, fmt.Errorf("create collections directory: %w", err)
	}
	options := DefaultLibraryOptions()
	options.PathInfos = []MediaPathInfo{{Path: path}}
	options.EnableRealtimeMonitor = false
	options.SaveLocalMetadata = true
	folder, err := s.topology.AddVirtualFolder(ctx, CollectionsLibraryName, item.CollectionTypeBoxSets.String(), options)
	if err != nil {
		return VirtualFolder{}, fmt.Errorf("add collections library: %w", err)
	}
	return folder, nil
}

// materializeCollectionsRoot stores the AggregateFolder, CollectionFolder,
// and media-path Folder of the collections library when a scan has not
// stored them yet. It resolves them the way prepareRoot does, so the next
// scan finds the same rows. It returns the media-path Folder.
func (s *Scanner) materializeCollectionsRoot(ctx context.Context, folder VirtualFolder) (item.Item, item.CollectionType, error) {
	collection, ok := collectionType(folder.CollectionType)
	if !ok {
		return item.Item{}, collection, fmt.Errorf("unsupported collection type %q", folder.CollectionType)
	}
	folder.Path = normalizedEntryPath(folder.Path)
	aggregate := aggregateFolder(normalizedEntryPath(s.topology.root), s.topology.anchor)
	virtual, err := s.chain.Resolve(ctx, ResolveContext{
		Entry:           Entry{Path: folder.Path, IsDir: true},
		Parent:          &aggregate,
		CollectionType:  collection,
		Options:         folder.Options,
		LibraryRoot:     folder.Path,
		IsVirtualFolder: true,
	})
	if err != nil || virtual == nil {
		return item.Item{}, collection, fmt.Errorf("resolve collections library: %w", errors.Join(err, errNilResolve(virtual)))
	}
	virtualPath := folder.Path
	virtual.Path = &virtualPath
	mediaPath := normalizedEntryPath(s.options.CollectionsPath)
	root, err := s.chain.Resolve(ctx, ResolveContext{
		Entry:                 Entry{Path: mediaPath, IsDir: true},
		Parent:                virtual,
		IsPhysicalLibraryRoot: true,
		CollectionType:        collection,
		Options:               folder.Options,
		LibraryRoot:           mediaPath,
	})
	if err != nil || root == nil {
		return item.Item{}, collection, fmt.Errorf("resolve collections path: %w", errors.Join(err, errNilResolve(root)))
	}
	if virtual.Extra.CollectionFolder == nil {
		virtual.Extra.CollectionFolder = &item.CollectionFolderExtra{}
	}
	virtual.Extra.CollectionFolder.CollectionType = &collection
	physical := []dto.UUID{root.ID}
	if _, err := s.items.Get(ctx, virtual.ID); err == nil {
		// A scan stored the library already; keep its physical folders.
		physical = nil
	}
	if physical != nil {
		virtual.Extra.CollectionFolder.PhysicalFolderIDs = physical
	}
	for _, value := range []*item.Item{&aggregate, virtual, root} {
		if _, err := s.items.Get(ctx, value.ID); err == nil {
			continue
		} else if !store.IsNotFound(err) {
			return item.Item{}, collection, err
		}
		materializeScannedSortName(value, item.DefaultSortOptions())
		materializeVirtualFolderLifecycle(value, nil, s.options.Now)
		if err := s.storeWrite(ctx, func() error { return s.items.Upsert(ctx, *value) }); err != nil {
			return item.Item{}, collection, fmt.Errorf("store collections library folder %s: %w", value.ID, err)
		}
	}
	return *root, collection, nil
}

func errNilResolve(value *item.Item) error {
	if value == nil {
		return errors.New("resolver returned no item")
	}
	return nil
}

// autoCollectionPageSize is the movie page size of CollectionPostScanTask.
const autoCollectionPageSize = 1000

// AddMoviesToCollections ports CollectionPostScanTask. For each library with
// AutomaticallyAddToCollection, it groups the movies that are not alternate
// versions by their TMDb collection name. A group joins the BoxSet with that
// name; when no such BoxSet exists, a group of two or more movies creates
// one. A failure on one collection is logged and the task continues.
//
// Upstream matches the name exactly. Deadfin then also matches a BoxSet
// whose Tmdb provider ID is the TMDb collection ID of a movie in the group,
// and last a BoxSet whose name differs only in case (divergence D-115). An
// imported collection that TMDb now spells or names differently thus gets
// the movies, and no duplicate collection is created beside it.
func (s *Scanner) AddMoviesToCollections(ctx context.Context, progress func(float64)) error {
	report := func(value float64) {
		if progress != nil {
			progress(value)
		}
	}
	folders, err := s.topology.VirtualFolders(ctx)
	if err != nil {
		return err
	}
	groups := map[string]map[dto.UUID]struct{}{}
	tmdbIDs := map[string]map[string]struct{}{}
	for _, folder := range folders {
		if !folder.Options.AutomaticallyAddToCollection {
			continue
		}
		parentID := folder.CollectionFolderID
		notVirtual := false
		for start := 0; ; start += autoCollectionPageSize {
			startIndex, limit := start, autoCollectionPageSize
			page, err := s.items.Query(ctx, store.ItemQuery{
				ParentID:         &parentID,
				Recursive:        true,
				IncludeItemTypes: []item.Kind{item.KindMovie},
				MediaTypes:       []item.MediaType{item.MediaTypeVideo},
				IsVirtualItem:    &notVirtual,
				SortBy:           []store.ItemSortBy{store.ItemSortBySortName},
				StartIndex:       &startIndex,
				Limit:            &limit,
			})
			if err != nil {
				return fmt.Errorf("auto collections: list movies of %q: %w", folder.Name, err)
			}
			for _, movie := range page.Items {
				if movie.PrimaryVersionID != nil || movie.Extra.Movie == nil || movie.Extra.Movie.TmdbCollectionName == nil || *movie.Extra.Movie.TmdbCollectionName == "" {
					continue
				}
				name := *movie.Extra.Movie.TmdbCollectionName
				if groups[name] == nil {
					groups[name] = map[dto.UUID]struct{}{}
				}
				groups[name][movie.ID] = struct{}{}
				if id, ok := movie.ProviderIDs.Get("TmdbCollection"); ok && id != "" {
					if tmdbIDs[name] == nil {
						tmdbIDs[name] = map[string]struct{}{}
					}
					tmdbIDs[name][id] = struct{}{}
				}
			}
			if len(page.Items) < autoCollectionPageSize {
				break
			}
		}
	}
	if len(groups) == 0 {
		report(100)
		return nil
	}
	boxSets, err := s.items.Query(ctx, store.ItemQuery{IncludeItemTypes: []item.Kind{item.KindBoxSet}, Recursive: true})
	if err != nil {
		return fmt.Errorf("auto collections: list collections: %w", err)
	}
	byName := map[string]dto.UUID{}
	byFoldedName := map[string]dto.UUID{}
	byTmdbID := map[string]dto.UUID{}
	for _, boxSet := range boxSets.Items {
		if boxSet.Name != nil {
			if _, seen := byName[*boxSet.Name]; !seen {
				byName[*boxSet.Name] = boxSet.ID
			}
			if _, seen := byFoldedName[strings.ToLower(*boxSet.Name)]; !seen {
				byFoldedName[strings.ToLower(*boxSet.Name)] = boxSet.ID
			}
		}
		if id, ok := boxSet.ProviderIDs.Get("Tmdb"); ok && id != "" {
			if _, seen := byTmdbID[id]; !seen {
				byTmdbID[id] = boxSet.ID
			}
		}
	}
	find := func(name string) (dto.UUID, bool) {
		if id, found := byName[name]; found {
			return id, true
		}
		ids := make([]string, 0, len(tmdbIDs[name]))
		for id := range tmdbIDs[name] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, tmdbID := range ids {
			if id, found := byTmdbID[tmdbID]; found {
				return id, true
			}
		}
		id, found := byFoldedName[strings.ToLower(name)]
		return id, found
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for index, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		movies := make([]dto.UUID, 0, len(groups[name]))
		for id := range groups[name] {
			movies = append(movies, id)
		}
		sort.Slice(movies, func(i, j int) bool { return movies[i].String() < movies[j].String() })
		boxSetID, found := find(name)
		if !found && len(movies) >= 2 {
			created, err := s.CreateCollection(ctx, name, false)
			if err != nil {
				s.logger.Error("auto collections: create collection", "collection", name, "error", err)
				continue
			}
			boxSetID, found = created.ID, true
			byName[name] = boxSetID
			byFoldedName[strings.ToLower(name)] = boxSetID
		}
		if found {
			if err := s.storeWrite(ctx, func() error { return s.items.AddToCollection(ctx, boxSetID, movies) }); err != nil {
				s.logger.Error("auto collections: add movies", "collection", name, "error", err)
			}
		}
		report(100 * float64(index+1) / float64(len(names)))
	}
	report(100)
	return nil
}

// resolveBoxSetTmdbID gives a BoxSet without a Tmdb provider ID the TMDb
// collection ID that most of its movies carry as TmdbCollection; a tie takes
// the lowest ID. Upstream TmdbBoxSetProvider only searches TMDb by the
// collection name. The members' ID also finds a collection whose name
// differs from TMDb's (divergence D-116).
//
// A BoxSet Tmdb ID is a TMDb collection ID. A stored Tmdb ID that is the
// Tmdb ID of a member movie, and no member's TmdbCollection ID, is a movie
// ID: it is replaced by the members' collection ID, or removed so that the
// provider searches by name (deadfin#846). A member TmdbCollection ID that is
// the member's own Tmdb ID is not used.
func (s *Scanner) resolveBoxSetTmdbID(ctx context.Context, candidate *item.Item) error {
	if candidate.Kind != item.KindBoxSet || candidate.ID.IsNil() {
		return nil
	}
	parentID := candidate.ID
	members, err := s.items.Query(ctx, store.ItemQuery{ParentID: &parentID, IncludeItemTypes: []item.Kind{item.KindMovie}})
	if err != nil {
		return fmt.Errorf("list collection members of %s: %w", candidate.ID, err)
	}
	counts := map[string]int{}
	movieIDs := map[string]struct{}{}
	for _, member := range members.Items {
		movieID, _ := member.ProviderIDs.Get("Tmdb")
		if movieID = strings.TrimSpace(movieID); movieID != "" {
			movieIDs[movieID] = struct{}{}
		}
		if id, ok := member.ProviderIDs.Get("TmdbCollection"); ok {
			if id = strings.TrimSpace(id); id != "" && id != movieID {
				counts[id]++
			}
		}
	}
	if id, ok := candidate.ProviderIDs.Get("Tmdb"); ok && strings.TrimSpace(id) != "" {
		id = strings.TrimSpace(id)
		_, isMovie := movieIDs[id]
		if _, isCollection := counts[id]; !isMovie || isCollection {
			return nil
		}
		s.logger.Warn("collection has a movie TMDb id; replacing it", "collection", candidate.ID.String(), "tmdbId", id)
		candidate.ProviderIDs = withoutProviderID(candidate.ProviderIDs, "Tmdb")
	}
	best, bestCount, bestNumber := "", 0, 0
	for id, count := range counts {
		number, err := strconv.Atoi(id)
		if err != nil || number <= 0 {
			continue
		}
		if count > bestCount || (count == bestCount && number < bestNumber) {
			best, bestCount, bestNumber = id, count, number
		}
	}
	if best != "" {
		candidate.ProviderIDs.Set("Tmdb", best)
	}
	return nil
}

// withoutProviderID returns ids without the name key.
func withoutProviderID(ids item.ProviderIDs, name string) item.ProviderIDs {
	var out item.ProviderIDs
	for _, id := range ids.Snapshot() {
		if !strings.EqualFold(id.Name, name) {
			out.Set(id.Name, id.Value)
		}
	}
	return out
}
