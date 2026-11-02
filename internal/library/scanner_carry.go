// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

// carryStoredProviderIDs and carryStoredImages keep the stored state that a
// rescan cannot re-derive. Upstream refreshes the stored BaseItem, so its
// ProviderIds and ImageInfos survive a scan: remote providers look the item
// up by the stored IDs (MetadataService.cs:757-764), and a remote image is
// fetched only for a type the item lacks unless the refresh replaces images
// (ItemImageProvider.RefreshImages). The scanner instead builds each
// candidate from the resolver and local metadata, so without this carry an
// identify fix, a downloaded remote image, or an uploaded image is lost on
// the next scan.

// carryStoredProviderIDs runs before the refresher, so remote providers
// resolve the item by the stored IDs. A stored provider ID carries when the
// candidate lacks the key, so a local NFO value still wins.
func carryStoredProviderIDs(candidate *item.Item, previous item.Item) {
	for _, providerID := range previous.ProviderIDs.Snapshot() {
		if _, found := candidate.ProviderIDs.Get(providerID.Name); !found {
			candidate.ProviderIDs.Set(providerID.Name, providerID.Value)
		}
	}
}

// carryStoredImages runs after the refresher. A stored image carries when it
// is a remote URL or a file outside the library root, such as an upload in
// the internal metadata directory. Artwork inside the library root belongs to
// the local image scan, which re-derives it or drops it when the file is
// gone. When the candidate already holds an image at the same type and
// index, a stored file replaces a provider URL, because upstream does not
// fetch a remote image for a type the item already has. A provider URL
// replaces a stored URL, because both name the provider's current artwork.
// A local library image wins; a carried backdrop then moves to the next free
// index.
//
// A refresh that replaces all images replaces an image only with one that a
// provider supplies (ItemImageProvider.RefreshImages). A stored image then
// carries only for a type the candidate lacks, and the dynamic image pass may
// replace it. When the refresh also replaces all metadata, the images in the
// internal metadata directory of the item do not carry
// (ItemImageProvider.RemoveImages).
func carryStoredImages(candidate *item.Item, previous item.Item, refresh RefreshOptions, libraryRoot string) {
	replacing := refresh.ReplaceAllImages && refresh.Image == RefreshModeFullRefresh
	var supplied map[item.ImageType]bool
	if replacing {
		supplied = make(map[item.ImageType]bool, len(candidate.ImageInfos))
		for _, image := range candidate.ImageInfos {
			supplied[image.Type] = true
		}
	}
	for _, image := range previous.ImageInfos {
		if !carriesStoredImage(image.Path, libraryRoot) || containsImagePath(candidate.ImageInfos, image) {
			continue
		}
		if replacing && (supplied[image.Type] || refresh.ReplaceAllMetadata && inInternalMetadataDir(image.Path, previous.ID)) {
			continue
		}
		position := imageAt(candidate.ImageInfos, image.Type, image.Index)
		switch {
		case position < 0:
			candidate.ImageInfos = append(candidate.ImageInfos, image)
		case isRemoteImageURL(candidate.ImageInfos[position].Path):
			if !isRemoteImageURL(image.Path) {
				candidate.ImageInfos[position] = image
			}
		case image.Type == item.ImageTypeBackdrop:
			image.Index = nextImageIndex(candidate.ImageInfos, image.Type)
			candidate.ImageInfos = append(candidate.ImageInfos, image)
		}
	}
}

// metadataPathToken is the stored prefix of a file under the metadata root.
const metadataPathToken = "%MetadataPath%"

// inInternalMetadataDir reports an image path in the internal metadata
// directory of the item (BaseItem.GetInternalMetadataPath).
func inInternalMetadataDir(path string, id dto.UUID) bool {
	directory := filepath.ToSlash(InternalMetadataDir(metadataPathToken, id)) + "/"
	return strings.HasPrefix(filepath.ToSlash(path), directory)
}

func carriesStoredImage(path, libraryRoot string) bool {
	if path == "" {
		return false
	}
	if isRemoteImageURL(path) {
		return true
	}
	if libraryRoot == "" {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(libraryRoot), filepath.Clean(path))
	if err != nil {
		return true
	}
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func containsImagePath(images []item.ImageInfo, image item.ImageInfo) bool {
	for _, current := range images {
		if current.Type == image.Type && current.Path == image.Path {
			return true
		}
	}
	return false
}

func imageAt(images []item.ImageInfo, imageType item.ImageType, index int) int {
	for position, current := range images {
		if current.Type == imageType && current.Index == index {
			return position
		}
	}
	return -1
}

func nextImageIndex(images []item.ImageInfo, imageType item.ImageType) int {
	next := 0
	for _, current := range images {
		if current.Type == imageType && current.Index >= next {
			next = current.Index + 1
		}
	}
	return next
}

// storedMetadataMerge selects how a rebuilt candidate takes the stored
// metadata of the item it replaces. The scanner rebuilds every candidate from
// the resolver and the local sources, but upstream refreshes the stored
// BaseItem in place, so the merges below port the two MergeData calls at the
// end of MetadataService.RefreshWithProviders:
//
//	MergeData(stored, fresh, [], replaceData: false)   // stored fills gaps
//	MergeData(fresh, stored, LockedFields, shouldReplace)
//
// shouldReplace is true for a Default refresh and for ReplaceAllMetadata, and
// false for FullRefresh without ReplaceAllMetadata.
type storedMetadataMerge uint8

const (
	// mergeNone keeps the candidate as built: a new item, or
	// ReplaceAllMetadata.
	mergeNone storedMetadataMerge = iota
	// mergeKeepStored is the result when no metadata provider would run:
	// the stored value wins and the candidate fills only an empty field.
	mergeKeepStored
	// mergeFillMissing is FullRefresh without ReplaceAllMetadata
	// (shouldReplace false): the stored value wins, the refreshed value fills
	// an empty field, and the additive lists take the union.
	mergeFillMissing
	// mergePreferRefreshed is a Default refresh that ran providers
	// (shouldReplace true): a value the providers set wins, the stored value
	// fills an empty field, and the additive lists take the union.
	mergePreferRefreshed
)

// metadataRefreshPlan is the provider decision for one candidate.
type metadataRefreshPlan struct {
	// skipRemote suppresses the remote metadata stage.
	skipRemote bool
	// merge selects how the candidate takes the stored metadata.
	merge storedMetadataMerge
	// refreshed reports that every provider ran, so DateLastRefreshed
	// advances when no remote provider failed.
	refreshed bool
}

// planMetadataRefresh ports the provider selection of
// MetadataService.RefreshMetadata and GetProviders. stored is the item at the
// candidate path, or nil for a new item. localChanged reports whether a local
// metadata source changed after stored was last saved (IHasItemChangeMonitor);
// the plan calls it only when the answer matters.
//
// Every provider runs, remote included, when the refresh replaces all
// metadata, when the mode is FullRefresh, or when the mode is at least Default
// and the item is on its first refresh (no DateLastRefreshed) or requires a
// refresh (the automatic refresh interval elapsed, or the item's file or
// directory changed: BaseItem.RequiresRefresh). Otherwise the local readers
// run only when their source changed, and remote providers do not run.
//
// Folder.RequiresRefresh also refreshes a MusicArtist, MusicAlbum, or
// Playlist without RunTimeTicks. Deadfin does not compute a cumulative
// runtime, so that rule would refresh every album on every scan; it is not
// ported (D-49).
func planMetadataRefresh(refresh RefreshOptions, options LibraryOptions, candidate item.Item, stored *item.Item, localChanged func() bool, now func() time.Time) metadataRefreshPlan {
	mode := refresh.Metadata
	if stored == nil {
		return metadataRefreshPlan{refreshed: mode != RefreshModeNone}
	}
	first := stored.DateLastRefreshed == nil
	requires := options.AutomaticRefreshIntervalDays > 0 && (first || now().Sub(*stored.DateLastRefreshed) >= time.Duration(options.AutomaticRefreshIntervalDays)*24*time.Hour)
	if !requires && mode != RefreshModeNone {
		requires = storedFileChanged(*stored, candidate)
	}
	runAll := refresh.ReplaceAllMetadata || mode == RefreshModeFullRefresh || (first || requires) && mode >= RefreshModeDefault
	plan := metadataRefreshPlan{skipRemote: !runAll, refreshed: runAll}
	switch {
	case refresh.ReplaceAllMetadata && mode > RefreshModeValidationOnly:
		plan.merge = mergeNone
	case refresh.ReplaceAllMetadata || mode == RefreshModeFullRefresh:
		plan.merge = mergeFillMissing
	case runAll:
		plan.merge = mergePreferRefreshed
	case mode != RefreshModeNone && localChanged != nil && localChanged():
		// Only the local readers run. DateLastRefreshed keeps its stored
		// value: it records the last refresh that ran every provider, which
		// is what the first-refresh and interval rules read.
		plan.merge = mergePreferRefreshed
	default:
		plan.merge = mergeKeepStored
	}
	return plan
}

// storedFileChanged ports BaseItem.RequiresRefresh and
// BaseItemExtensions.HasChanged: the item's file or directory write time
// differs from the stored DateModified by more than one second. A stored
// item without DateModified never requires a refresh.
func storedFileChanged(stored, candidate item.Item) bool {
	if stored.DateModified == nil || candidate.DateModified == nil {
		return false
	}
	delta := stored.DateModified.Sub(*candidate.DateModified)
	if delta < 0 {
		delta = -delta
	}
	return delta > time.Second
}

// refreshForCandidate narrows a targeted refresh to the target item and its
// descendants. Upstream ProviderManager.RefreshItem refreshes only the target
// subtree with the requested intent. The scanner scans the whole library
// that holds a target, so every other item takes a Default refresh.
func refreshForCandidate(refresh RefreshOptions, candidate item.Item, parent item.Item, ancestors []*item.Item, collectionFolderID dto.UUID) RefreshOptions {
	if len(refresh.Targets) == 0 {
		return refresh
	}
	for _, target := range refresh.Targets {
		if target.ItemID == collectionFolderID || target.ItemID == candidate.ID || target.ItemID == parent.ID {
			return refresh
		}
		for _, ancestor := range ancestors {
			if ancestor != nil && ancestor.ID == target.ItemID {
				return refresh
			}
		}
		if target.Path != "" && candidate.Path != nil && pathWithin(*candidate.Path, target.Path) {
			return refresh
		}
	}
	scoped := DefaultRefreshOptions()
	scoped.Targets = refresh.Targets
	return scoped
}

// refreshNameSnapshot holds the Name-family values the resolver set before
// the providers ran. A provider that leaves them unchanged did not set them,
// so the stored value wins, as it does upstream where the resolver name is
// never part of the provider result.
type refreshNameSnapshot struct {
	name, sortName *string
}

func snapshotRefreshNames(value item.Item) refreshNameSnapshot {
	return refreshNameSnapshot{name: copyRefreshString(value.Name), sortName: copyRefreshString(value.SortName)}
}

// mergeStoredMetadata applies merge to candidate. It covers the fields of
// MetadataService.MergeBaseItemData and the per-kind MergeData overrides.
// Provider IDs and images carry separately (carryStoredProviderIDs,
// carryStoredImages). Locked fields carry afterward
// (rehydrateRefreshLocks). Probe-owned fields (RunTimeTicks of playable
// media, Video3DFormat) stay with the probe, which reuses the stored facts for
// an unchanged file.
func mergeStoredMetadata(candidate *item.Item, stored item.Item, merge storedMetadataMerge, resolved refreshNameSnapshot) {
	if merge == mergeNone || stored.IsLocked {
		return
	}
	preferStored := merge != mergePreferRefreshed
	union := merge != mergeKeepStored

	providerSetName := !sameRefreshString(candidate.Name, resolved.name) || !sameRefreshString(candidate.SortName, resolved.sortName)
	if nonEmptyString(stored.Name) && (preferStored || !providerSetName) {
		candidate.Name = copyRefreshString(stored.Name)
		candidate.SortName = copyRefreshString(stored.SortName)
	}
	mergeString(&candidate.ForcedSortName, stored.ForcedSortName, preferStored)
	mergeString(&candidate.OriginalTitle, stored.OriginalTitle, preferStored)
	mergeString(&candidate.OriginalLanguage, stored.OriginalLanguage, preferStored)
	mergeString(&candidate.Overview, stored.Overview, preferStored)
	mergeString(&candidate.Tagline, stored.Tagline, preferStored)
	mergeString(&candidate.OfficialRating, stored.OfficialRating, preferStored)
	mergeString(&candidate.CustomRating, stored.CustomRating, preferStored)
	mergeString(&candidate.PreferredMetadataLanguage, stored.PreferredMetadataLanguage, preferStored)
	mergeString(&candidate.PreferredMetadataCountryCode, stored.PreferredMetadataCountryCode, preferStored)
	if candidate.Kind == item.KindMusicVideo {
		mergeString(&candidate.Album, stored.Album, preferStored)
	}
	mergeValue(&candidate.CommunityRating, stored.CommunityRating, preferStored)
	mergeValue(&candidate.CriticRating, stored.CriticRating, preferStored)
	mergeValue(&candidate.EndDate, stored.EndDate, preferStored)
	mergeValue(&candidate.PremiereDate, stored.PremiereDate, preferStored)
	mergeValue(&candidate.ProductionYear, stored.ProductionYear, preferStored)
	mergeValue(&candidate.IndexNumber, stored.IndexNumber, preferStored)
	mergeValue(&candidate.ParentIndexNumber, stored.ParentIndexNumber, preferStored)
	mergeValue(&candidate.AirsBeforeSeasonNumber, stored.AirsBeforeSeasonNumber, preferStored)
	mergeValue(&candidate.AirsAfterSeasonNumber, stored.AirsAfterSeasonNumber, preferStored)
	mergeValue(&candidate.AirsBeforeEpisodeNumber, stored.AirsBeforeEpisodeNumber, preferStored)
	mergeValue(&candidate.SeriesStatus, stored.SeriesStatus, preferStored)
	if !isPlayableMedia(*candidate) && candidate.Kind != item.KindBook {
		mergeValue(&candidate.RunTimeTicks, stored.RunTimeTicks, preferStored)
	}

	// Genres and Artists replace as a whole (MergeBaseItemData Genres,
	// AudioMetadataService and AlbumMetadataService Artists).
	candidate.Values.Genres = mergeWholeList(candidate.Values.Genres, stored.Values.Genres, preferStored)
	candidate.Values.Artists = mergeWholeList(candidate.Values.Artists, stored.Values.Artists, preferStored)
	// Studios, Tags, ProductionLocations, and AlbumArtists take the
	// case-insensitive union when both sides hold values.
	candidate.Values.Studios = mergeUnionList(candidate.Values.Studios, stored.Values.Studios, preferStored, union)
	candidate.Values.Tags = mergeUnionList(candidate.Values.Tags, stored.Values.Tags, preferStored, union)
	candidate.Values.ProductionLocations = mergeUnionList(candidate.Values.ProductionLocations, stored.Values.ProductionLocations, preferStored, union)
	candidate.Values.AlbumArtists = mergeUnionList(candidate.Values.AlbumArtists, stored.Values.AlbumArtists, preferStored, union)
	candidate.People = mergePeople(candidate.People, stored.People, preferStored, union)
	candidate.Extra.RemoteTrailers = mergeRemoteTrailers(candidate.Extra.RemoteTrailers, stored.Extra.RemoteTrailers, preferStored, union)
	mergeExtra(candidate, stored, preferStored)
}

func nonEmptyString(value *string) bool { return value != nil && *value != "" }

// mergeString applies one MergeData string rule. preferStored keeps a
// non-empty stored value; otherwise the stored value fills an empty
// candidate.
func mergeString(target **string, stored *string, preferStored bool) {
	if !nonEmptyString(stored) {
		return
	}
	if preferStored || !nonEmptyString(*target) {
		*target = copyRefreshString(stored)
	}
}

func mergeValue[T any](target **T, stored *T, preferStored bool) {
	if stored == nil {
		return
	}
	if preferStored || *target == nil {
		value := *stored
		*target = &value
	}
}

func mergeWholeList(candidate, stored []string, preferStored bool) []string {
	if len(stored) == 0 {
		return candidate
	}
	if preferStored || len(candidate) == 0 {
		return append([]string(nil), stored...)
	}
	return candidate
}

// mergeUnionList keeps the winning side first and appends the other side's
// values that it lacks, compared case-insensitively
// (Distinct(StringComparer.OrdinalIgnoreCase)).
func mergeUnionList(candidate, stored []string, preferStored, union bool) []string {
	if len(stored) == 0 {
		return candidate
	}
	if len(candidate) == 0 {
		return append([]string(nil), stored...)
	}
	if !union {
		if preferStored {
			return append([]string(nil), stored...)
		}
		return candidate
	}
	first, second := candidate, stored
	if preferStored {
		first, second = stored, candidate
	}
	merged := make([]string, 0, len(first)+len(second))
	for _, value := range append(append([]string(nil), first...), second...) {
		if !containsFoldString(merged, value) {
			merged = append(merged, value)
		}
	}
	return merged
}

func containsFoldString(values []string, value string) bool {
	for _, current := range values {
		if strings.EqualFold(current, value) {
			return true
		}
	}
	return false
}

// mergePeople ports the Cast rule of MergeBaseItemData. The winning side
// keeps its credits; when union holds and both sides have credits, the other
// side only fills a missing Role or SortOrder of a credit with the same
// person (MergePeople). It never adds a credit.
func mergePeople(candidate, stored []item.PersonCredit, preferStored, union bool) []item.PersonCredit {
	if len(stored) == 0 {
		return candidate
	}
	if len(candidate) == 0 {
		return append([]item.PersonCredit(nil), stored...)
	}
	winner, source := candidate, stored
	if preferStored {
		winner, source = append([]item.PersonCredit(nil), stored...), candidate
	}
	if !union && preferStored {
		return winner
	}
	byName := make(map[string][]item.PersonCredit, len(source))
	for _, credit := range source {
		key := item.CleanName(credit.Name)
		byName[key] = append(byName[key], credit)
	}
	seen := make(map[string]int, len(winner))
	for index := range winner {
		key := item.CleanName(winner[index].Name)
		matches := byName[key]
		position := seen[key]
		seen[key]++
		if len(matches) == 0 {
			continue
		}
		match := matches[0]
		if position < len(matches) {
			match = matches[position]
		}
		if (winner[index].Role == nil || strings.TrimSpace(*winner[index].Role) == "") && match.Role != nil && strings.TrimSpace(*match.Role) != "" {
			winner[index].Role = copyRefreshString(match.Role)
		}
		if winner[index].SortOrder == nil && match.SortOrder != nil {
			order := *match.SortOrder
			winner[index].SortOrder = &order
		}
		if winner[index].ImageURL == "" {
			winner[index].ImageURL = match.ImageURL
		}
		// MetadataService merges the provider IDs of the source person with
		// ProviderIds.TryAdd.
		for _, id := range match.ProviderIDs.Snapshot() {
			if _, found := winner[index].ProviderIDs.Get(id.Name); !found {
				winner[index].ProviderIDs.Set(id.Name, id.Value)
			}
		}
	}
	return winner
}

// mergeRemoteTrailers ports the RemoteTrailers rule: the union by URL.
func mergeRemoteTrailers(candidate, stored []item.MediaURL, preferStored, union bool) []item.MediaURL {
	if len(stored) == 0 {
		return candidate
	}
	if len(candidate) == 0 || preferStored && !union {
		return append([]item.MediaURL(nil), stored...)
	}
	if !union {
		return candidate
	}
	first, second := candidate, stored
	if preferStored {
		first, second = stored, candidate
	}
	merged := make([]item.MediaURL, 0, len(first)+len(second))
	seen := make(map[string]struct{}, len(first)+len(second))
	for _, trailer := range append(append([]item.MediaURL(nil), first...), second...) {
		key := ""
		if trailer.URL != nil {
			key = *trailer.URL
		}
		if _, found := seen[key]; found {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, trailer)
	}
	return merged
}

// mergeExtra ports SeriesMetadataService (AirTime, AirDays, DisplayOrder
// through MergeDisplayOrder) and MovieMetadataService (CollectionName).
func mergeExtra(candidate *item.Item, stored item.Item, preferStored bool) {
	if previous := stored.Extra.Series; previous != nil {
		if candidate.Extra.Series == nil {
			candidate.Extra.Series = &item.SeriesExtra{}
		}
		current := candidate.Extra.Series
		mergeString(&current.AirTime, previous.AirTime, preferStored)
		mergeString(&current.DisplayOrder, previous.DisplayOrder, preferStored)
		if len(previous.AirDays) != 0 && (preferStored || len(current.AirDays) == 0) {
			current.AirDays = append([]item.DayOfWeek(nil), previous.AirDays...)
		}
		if current.AirTime == nil && current.DisplayOrder == nil && len(current.AirDays) == 0 {
			candidate.Extra.Series = nil
		}
	}
	if previous := stored.Extra.Movie; previous != nil && nonEmptyString(previous.TmdbCollectionName) {
		if candidate.Extra.Movie == nil {
			candidate.Extra.Movie = &item.MovieExtra{}
		}
		mergeString(&candidate.Extra.Movie.TmdbCollectionName, previous.TmdbCollectionName, preferStored)
	}
}

// refreshWithOutcome runs refresher and returns its outcome when it reports
// one.
func refreshWithOutcome(ctx context.Context, refresher Refresher, refresh RefreshContext, target *item.Item) (RefreshOutcome, error) {
	if reporter, ok := refresher.(OutcomeRefresher); ok {
		return reporter.RefreshWithOutcome(ctx, refresh, target)
	}
	return RefreshOutcome{}, refresher.Refresh(ctx, refresh, target)
}

// carryRefreshed reports whether candidate takes a new DateLastRefreshed. A
// refresh that ran every provider without a remote failure does
// (MetadataService.RefreshMetadata); otherwise the stored value carries, so
// an unchanged item stays unchanged and a failed first refresh retries on the
// next scan.
func carryRefreshed(candidate *item.Item, stored *item.Item, plan metadataRefreshPlan, outcome RefreshOutcome) bool {
	if plan.refreshed && outcome.RemoteFailures == 0 {
		return true
	}
	if stored != nil {
		candidate.DateLastRefreshed = copyScannedPointer(stored.DateLastRefreshed)
	}
	return false
}
