// SPDX-License-Identifier: GPL-2.0-only

package providers

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/metadata/local"
	"github.com/deadfin-org/deadfin/internal/metadata/nfo"
)

// NewLocalImageProvider constructs the frozen-fact local artwork provider.
func NewLocalImageProvider() Provider { return localImageProvider{} }

type localImageProvider struct{}

func (localImageProvider) Name() string         { return "LocalImageProvider" }
func (localImageProvider) Stage() ProviderStage { return ProviderStageLocalImages }

func (localImageProvider) Refresh(ctx context.Context, refresh library.RefreshContext, target *item.Item) (bool, error) {
	if target == nil {
		return false, errors.New("local image provider: item is required")
	}
	if target.Path == nil || refresh.Directory.Path == "" {
		return false, nil
	}

	facts := newImageFacts(refresh)
	name := ""
	if target.Name != nil {
		name = *target.Name
	}
	images, err := local.DiscoverImages(ctx, local.ImageContext{
		Directory:         refresh.Directory.Path,
		MetadataDirectory: refresh.Directory.Path,
		ParentDirectory:   filepath.Dir(refresh.Directory.Path),
		ParentName:        filepath.Base(refresh.Directory.Path),
		ItemPath:          *target.Path,
		Name:              name,
		SeasonNumber:      target.IndexNumber,
		Kind:              target.Kind,
		IsMixedFolder:     target.IsInMixedFolder,
		ReadDir:           facts.readDir,
		Stat:              facts.stat,
	})
	if err != nil {
		return false, err
	}
	if len(images) == 0 {
		return false, nil
	}
	target.ImageInfos = mergeProviderImages(target.ImageInfos, images)
	if err := enrichLocalImageMetadata(ctx, target); err != nil {
		return false, err
	}
	return true, nil
}

type imageFacts struct {
	entries map[string][]library.Entry
	stats   map[string]local.ImageStat
}

func newImageFacts(refresh library.RefreshContext) imageFacts {
	facts := imageFacts{entries: make(map[string][]library.Entry, len(refresh.ChildEntries)+1), stats: make(map[string]local.ImageStat)}
	facts.add(refresh.Directory.Path, refresh.Entries)
	for path, entries := range refresh.ChildEntries {
		facts.add(path, entries)
	}
	return facts
}

func (facts imageFacts) add(directory string, entries []library.Entry) {
	facts.entries[filepath.Clean(directory)] = append([]library.Entry(nil), entries...)
	for _, entry := range entries {
		facts.stats[filepath.Clean(entry.Path)] = local.ImageStat{Size: entry.Size, Modified: entry.Modified}
	}
}

func (facts imageFacts) readDir(ctx context.Context, directory string) ([]local.ImageEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, found := facts.entries[filepath.Clean(directory)]
	if !found {
		return nil, fs.ErrNotExist
	}
	out := make([]local.ImageEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, local.ImageEntry{Name: filepath.Base(entry.Path)})
	}
	return out, nil
}

func (facts imageFacts) stat(ctx context.Context, path string) (local.ImageStat, error) {
	if err := ctx.Err(); err != nil {
		return local.ImageStat{}, err
	}
	stat, found := facts.stats[filepath.Clean(path)]
	if !found {
		return local.ImageStat{}, fs.ErrNotExist
	}
	return stat, nil
}

// NFOProviderOptions supplies the sole I/O seam used to read a known NFO file.
type NFOProviderOptions struct {
	Open func(context.Context, string) (io.ReadCloser, error)
}

// NewNFOProvider constructs a local metadata reader for sidecar NFO files.
func NewNFOProvider(options NFOProviderOptions) Provider { return nfoProvider{open: options.Open} }

type nfoProvider struct {
	open func(context.Context, string) (io.ReadCloser, error)
}

// Name is BaseNfoSaver.SaverName, the name jellyfin-web saves in
// LocalMetadataReaderOrder.
func (nfoProvider) Name() string         { return "Nfo" }
func (nfoProvider) Stage() ProviderStage { return ProviderStageLocalMetadata }

// SupportsKind lists the kinds of the pinned Xbmc NFO providers (Album,
// Artist, Episode, Movie, MusicVideo, Season, Series, and Video).
func (nfoProvider) SupportsKind(kind item.Kind) bool {
	switch kind {
	case item.KindMusicAlbum, item.KindMusicArtist, item.KindEpisode, item.KindMovie,
		item.KindMusicVideo, item.KindSeason, item.KindSeries, item.KindVideo:
		return true
	default:
		return false
	}
}

func (provider nfoProvider) Refresh(ctx context.Context, refresh library.RefreshContext, target *item.Item) (handled bool, err error) {
	if target == nil {
		return false, errors.New("NFO provider: item is required")
	}
	if target.Path == nil {
		return false, nil
	}
	nfoPath, found := nfoPath(refresh.Entries, *target.Path, target.Kind, target.IsInMixedFolder)
	if !found {
		return false, nil
	}
	if provider.open == nil {
		return false, errors.New("NFO provider: open is required")
	}
	reader, err := provider.open(ctx, nfoPath)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	result, err := nfo.Parse(ctx, reader, nfo.ParseOptions{Kind: target.Kind})
	if err != nil {
		return false, err
	}
	mergeNFOResult(target, result)
	return true, nil
}

// HasChanged mirrors BaseNfoProvider.HasChanged: the NFO changed when its
// write time is more than one minute after the item was last saved. The
// tolerance keeps upstream's guard against its own NFO writes. A stored item
// without DateLastSaved counts as changed.
func (nfoProvider) HasChanged(refresh library.RefreshContext, stored item.Item) bool {
	if stored.Path == nil {
		return false
	}
	path, found := nfoPath(refresh.Entries, *stored.Path, stored.Kind, stored.IsInMixedFolder)
	if !found {
		return false
	}
	for _, entry := range refresh.Entries {
		if filepath.Clean(entry.Path) != path {
			continue
		}
		if stored.DateLastSaved == nil {
			return true
		}
		return entry.Modified.Sub(*stored.DateLastSaved) > time.Minute
	}
	return false
}

func containsEntry(entries []library.Entry, want string) bool {
	want = filepath.Clean(want)
	for _, entry := range entries {
		if filepath.Clean(entry.Path) == want {
			return true
		}
	}
	return false
}

// nfoPath selects the first actual sidecar using the same naming order as the
// pinned Xbmc metadata providers. Directory kinds have fixed names; movies
// prefer movie.nfo unless they belong to a mixed folder, then use a stem match.
func nfoPath(entries []library.Entry, itemPath string, kind item.Kind, mixedFolder bool) (string, bool) {
	paths := make([]string, 0, 2)
	switch kind {
	case item.KindSeries:
		paths = append(paths, filepath.Join(itemPath, "tvshow.nfo"))
	case item.KindSeason:
		paths = append(paths, filepath.Join(itemPath, "season.nfo"))
	case item.KindMusicArtist:
		paths = append(paths, filepath.Join(itemPath, "artist.nfo"))
	case item.KindMusicAlbum:
		paths = append(paths, filepath.Join(itemPath, "album.nfo"))
	case item.KindMovie:
		// A directory-backed movie keeps movie.nfo beside its media, whereas
		// a file-backed movie keeps it in the parent directory. Check the
		// directory convention first; it cannot match a regular file path.
		paths = append(paths, filepath.Join(itemPath, "movie.nfo"))
		if !mixedFolder {
			paths = append(paths, filepath.Join(filepath.Dir(itemPath), "movie.nfo"))
		}
	}
	paths = append(paths, strings.TrimSuffix(itemPath, filepath.Ext(itemPath))+".nfo")
	for _, path := range paths {
		if containsEntry(entries, path) {
			return path, true
		}
	}
	return "", false
}

func mergeNFOResult(target *item.Item, result nfo.Result) {
	patch := result.Patch
	if nfoMergeAllowed(target, item.MetadataFieldName) {
		mergeNFOString(&target.Name, patch.Name)
		mergeNFOString(&target.SortName, patch.SortName)
		mergeNFOString(&target.ForcedSortName, patch.ForcedSortName)
		mergeNFOString(&target.OriginalTitle, patch.OriginalTitle)
	}
	if nfoMergeAllowed(target, item.MetadataFieldOverview) {
		mergeNFOString(&target.Overview, patch.Overview)
		mergeNFOString(&target.Tagline, patch.Tagline)
	}
	if nfoMergeAllowed(target, item.MetadataFieldOfficialRating) {
		mergeNFOString(&target.OfficialRating, patch.OfficialRating)
		mergeNFOString(&target.CustomRating, patch.CustomRating)
	}
	if nfoMergeAllowed(target) {
		mergeNFOString(&target.PreferredMetadataLanguage, patch.PreferredMetadataLanguage)
		mergeNFOString(&target.PreferredMetadataCountryCode, patch.PreferredMetadataCountryCode)
		mergeNFOString(&target.Album, patch.Album)
		mergeNFOString(&target.SeriesName, patch.SeriesName)
		mergeNFOValue(&target.DateCreated, patch.DateCreated)
		mergeNFOValue(&target.PremiereDate, patch.PremiereDate)
		mergeNFOValue(&target.EndDate, patch.EndDate)
		mergeNFOValue(&target.ProductionYear, patch.ProductionYear)
		mergeNFOValue(&target.CommunityRating, patch.CommunityRating)
		mergeNFOValue(&target.CriticRating, patch.CriticRating)
		mergeNFOValue(&target.ParentIndexNumber, patch.ParentIndexNumber)
		mergeNFOValue(&target.IndexNumber, patch.IndexNumber)
		mergeNFOValue(&target.AirsBeforeEpisodeNumber, patch.AirsBeforeEpisodeNumber)
		mergeNFOValue(&target.AirsAfterSeasonNumber, patch.AirsAfterSeasonNumber)
		mergeNFOValue(&target.AirsBeforeSeasonNumber, patch.AirsBeforeSeasonNumber)
		mergeNFOValue(&target.SeriesStatus, patch.SeriesStatus)
	}
	if nfoMergeAllowed(target, item.MetadataFieldRuntime) {
		mergeNFOValue(&target.RunTimeTicks, patch.RunTimeTicks)
	}
	if nfoMergeAllowed(target, item.MetadataFieldGenres) && len(result.Genres) != 0 {
		target.Values.Genres = append([]string(nil), result.Genres...)
	}
	if nfoMergeAllowed(target, item.MetadataFieldStudios) && len(result.Studios) != 0 {
		target.Values.Studios = append([]string(nil), result.Studios...)
	}
	if nfoMergeAllowed(target, item.MetadataFieldTags) && len(result.Tags) != 0 {
		target.Values.Tags = append([]string(nil), result.Tags...)
	}
	if nfoMergeAllowed(target, item.MetadataFieldCast) && len(result.People) != 0 {
		target.People = nfoPeople(result.People)
	}
	if nfoMergeAllowed(target) {
		for _, providerID := range result.ProviderIDs.Snapshot() {
			target.ProviderIDs.Set(providerID.Name, providerID.Value)
		}
		target.ImageInfos = mergeProviderImages(target.ImageInfos, patch.ImageInfos)
		if len(patch.Extra.RemoteTrailers) != 0 {
			target.Extra.RemoteTrailers = append([]item.MediaURL(nil), patch.Extra.RemoteTrailers...)
		}
		if patch.Extra.Episode != nil {
			target.Extra.Episode = patch.Extra.Episode
		}
		if patch.Extra.Series != nil {
			target.Extra.Series = patch.Extra.Series
		}
		if patch.IsLocked {
			target.IsLocked = true
		}
		if len(patch.LockedFields) != 0 {
			target.LockedFields = mergeLockedFields(target.LockedFields, patch.LockedFields)
		}
	}
}

func nfoMergeAllowed(target *item.Item, fields ...item.MetadataField) bool {
	if target.IsLocked {
		return false
	}
	for _, field := range fields {
		for _, locked := range target.LockedFields {
			if locked == field {
				return false
			}
		}
	}
	return true
}

func mergeLockedFields(current, patch []item.MetadataField) []item.MetadataField {
	merged := append([]item.MetadataField(nil), current...)
	for _, field := range patch {
		found := false
		for _, existing := range merged {
			if existing == field {
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, field)
		}
	}
	return merged
}

func mergeNFOString(target **string, patch *string) {
	if patch != nil {
		*target = patch
	}
}

func mergeNFOValue[T any](target **T, patch *T) {
	if patch != nil {
		*target = patch
	}
}

func nfoPeople(people []nfo.Person) []item.PersonCredit {
	credits := make([]item.PersonCredit, 0, len(people))
	seen := make(map[struct {
		id   dto.UUID
		kind item.PersonKind
	}]struct{}, len(people))
	for _, person := range people {
		if person.Name == "" {
			continue
		}
		id := nfoPersonID(person.Name)
		key := struct {
			id   dto.UUID
			kind item.PersonKind
		}{id, person.Kind}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		credits = append(credits, item.PersonCredit{PersonID: id, Name: person.Name, Role: person.Role, Type: person.Kind, SortOrder: person.Order})
	}
	return credits
}

func nfoPersonID(name string) dto.UUID {
	return library.PersonEntity(name).ID
}

func mergeProviderImages(current, patch []item.ImageInfo) []item.ImageInfo {
	if len(patch) == 0 {
		return current
	}
	merged := append([]item.ImageInfo(nil), current...)
	for _, image := range patch {
		replaced := false
		for index := range merged {
			if merged[index].Type == image.Type && merged[index].Index == image.Index {
				merged[index] = image
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, image)
		}
	}
	return merged
}
