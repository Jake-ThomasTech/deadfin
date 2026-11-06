// SPDX-License-Identifier: GPL-2.0-only

package dtobuild

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/path"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/store"
)

// StoreUserReader supplies persisted target-user facts for access-aware DTO
// projections.
type StoreUserReader interface {
	Get(context.Context, dto.UUID) (store.User, error)
}

// NewStoreEnricher adapts persisted item satellites into DTO projections. Query
// has already batch-loaded item satellites; this adapter reads only projections
// that cannot live on item.Item itself.
func NewStoreEnricher(items *store.ItemStore, users StoreUserReader, languages playback.LanguageNormalizer) BatchEnricher {
	return &storeEnricher{items: items, users: users, languages: languages}
}

// setPaths injects the virtual-path expander for media source and stream
// projections. Builder calls this through the pathExpanderSetter interface when
// the composition root calls WithPaths.
func (e *storeEnricher) setPaths(paths path.VirtualPathExpander) {
	e.paths = paths
}

type storeEnricher struct {
	items     *store.ItemStore
	users     StoreUserReader
	languages playback.LanguageNormalizer
	paths     path.VirtualPathExpander
}

func (e storeEnricher) Enrich(ctx context.Context, values []item.Item, options store.ItemQueryDTOOptions, userID *dto.UUID) (map[dto.UUID]Enrichment, error) {
	result := make(map[dto.UUID]Enrichment, len(values))
	if e.items == nil {
		return result, nil
	}
	var user *store.User
	if userID != nil && e.users != nil {
		resolved, err := e.users.Get(ctx, *userID)
		if err != nil {
			return nil, err
		}
		user = &resolved
	}
	configuration := dto.DefaultUserConfiguration()
	if user != nil && len(user.Configuration) != 0 {
		if err := json.Unmarshal(user.Configuration, &configuration); err != nil {
			return nil, err
		}
	}
	ids := make([]dto.UUID, len(values))
	for index := range values {
		ids[index] = values[index].ID
	}
	wantSources := hasField(options.Fields, "MediaSources")
	wantStreams := hasField(options.Fields, "MediaStreams")
	wantSubtitleCheck := false
	for _, value := range values {
		if item.MediaTypeOf(value) == item.MediaTypeVideo {
			wantSubtitleCheck = true
			break
		}
	}
	var originalLanguages map[dto.UUID]string
	if wantSources && user != nil && wantSubtitleCheck {
		resolved, resolveErr := e.items.InheritedOriginalLanguages(ctx, values)
		if resolveErr != nil {
			return nil, resolveErr
		}
		originalLanguages = resolved
	}
	wantChild, wantRecursive := hasField(options.Fields, "ChildCount"), hasField(options.Fields, "RecursiveItemCount")
	graph, err := e.relatedItemGraph(ctx, values)
	if err != nil {
		return nil, err
	}
	entities, err := e.namedEntities(ctx, values, options)
	if err != nil {
		return nil, err
	}
	personImages, err := e.personImages(ctx, values, options)
	if err != nil {
		return nil, err
	}
	var sourceRecords map[dto.UUID][]playback.MediaSourceRecord
	if wantSources {
		sourceRecords, err = e.items.MediaSourceRecordsPage(ctx, ids)
		if err != nil {
			return nil, err
		}
	}
	var versionUserData map[dto.UUID]store.UserData
	if wantSources && user != nil {
		recordIDs := make([]dto.UUID, 0)
		for _, records := range sourceRecords {
			for _, record := range records {
				recordIDs = append(recordIDs, record.ID)
			}
		}
		versionUserData, err = e.items.VersionUserData(ctx, *userID, recordIDs)
		if err != nil {
			return nil, err
		}
	}
	var seriesSelections map[dto.UUID]playback.SeriesStreamSelection
	if wantSources && user != nil {
		seriesIDs := make([]dto.UUID, 0)
		for _, value := range values {
			if seriesID, found := store.PlaybackSelectionSeriesID(value, configuration); found {
				seriesIDs = append(seriesIDs, seriesID)
			}
		}
		seriesSelections, err = e.items.SeriesStreamSelections(ctx, *userID, seriesIDs)
		if err != nil {
			return nil, err
		}
	}
	wantUserDataRows := userID != nil && options.EnableUserData
	// Only folders that derive UserData from their children need the played
	// aggregate. Library views (CollectionFolder, UserView) do not, so a
	// UserViews page reads no subtree.
	folderCounts := make([]dto.UUID, 0)
	for _, value := range values {
		if supportsFolderUserDataFromChildren(value) {
			folderCounts = append(folderCounts, value.ID)
		}
	}
	batch, err := e.items.DTOProjections(ctx, ids, userID, wantStreams || wantSubtitleCheck, wantUserDataRows, wantChild, wantRecursive, folderCounts, user)
	if err != nil {
		return nil, err
	}
	collectionCounts := map[dto.UUID]int{}
	if wantChild {
		collectionCounts, err = e.collectionChildCounts(ctx, values)
		if err != nil {
			return nil, err
		}
	}
	for _, value := range values {
		enrichment := imagesEnrichment(value, options)
		streams := batch.Streams[value.ID]
		if wantSources && itemHasMediaSources(value) {
			records := sourceRecords[value.ID]
			sources := playback.MediaSourcesFromRecords(records)
			if value.Kind == item.KindLiveTvChannel {
				channelPlaceholderSources(sources)
			}
			switch item.MediaTypeOf(value) {
			case item.MediaTypeAudio:
				playback.ApplyFirstAudioStreamDefaults(sources)
			case item.MediaTypeVideo:
				if user != nil {
					originalLanguage := originalLanguages[value.ID]
					selection := playback.SourceSelection{
						Configuration:    configuration,
						MediaKind:        playback.SourceMediaVideo,
						OriginalLanguage: originalLanguage,
						PrimaryItem:      value.PrimaryVersionID == nil,
						VersionUserData:  make(map[string]playback.SourceUserData, len(records)),
					}
					if data, found := versionUserData[value.ID]; found {
						itemData := playbackSourceUserData(data)
						selection.ItemUserData = &itemData
					}
					if seriesID, found := store.PlaybackSelectionSeriesID(value, configuration); found {
						if previous, found := seriesSelections[seriesID]; found {
							selection.SeriesSelection = &previous
						}
					}
					for _, record := range records {
						if data, found := versionUserData[record.ID]; found {
							selection.VersionUserData[record.ID.String()] = playbackSourceUserData(data)
						}
					}
					sources = playback.ApplyUserSourceSelection(sources, selection, e.languages)
				}
			}
			enrichment.MediaSources = dtoMediaSources(expandSources(sources, e.paths))
		}
		if wantStreams && itemHasMediaSources(value) {
			enrichment.MediaStreams = dtoMediaStreams(expandStreams(streams, e.paths))
		}
		if hasSubtitleStream(streams) {
			enrichment.HasSubtitles = true
		}
		matched := matchedUserData{}
		if wantUserDataRows {
			matched = matchingUserData(value, store.UserDataInputsFor(value, graph.series), batch.UserData[value.ID])
		}
		if options.EnableUserData && userID != nil {
			enrichment.UserData = dtoUserData(value.ID, matched)
			applyFolderUserData(enrichment.UserData, value, batch.FolderUserDataCounts[value.ID])
			applyLeafUserData(enrichment.UserData, value)
		}
		if wantChild {
			count := batch.ChildCounts[value.ID]
			if value.Kind == item.KindCollectionFolder {
				count = collectionCounts[value.ID]
			}
			enrichment.ChildCount = &count
		}
		if wantRecursive {
			count := batch.RecursiveCounts[value.ID]
			enrichment.RecursiveItemCount = &count
		}
		applyRelatedProjection(&enrichment, value, graph, entities)
		if personImages != nil {
			enrichment.People = dtoPeople(value.People, personImages)
		}
		result[value.ID] = enrichment
	}
	return result, nil
}

// collectionChildCounts projects a CollectionFolder through its physical source
// Folder children. The sources themselves are scanner implementation details and
// must not appear as the collection's visible children, while direct non-folder
// children remain visible. One ancestor-scoped query obtains every requested
// collection subtree without per-collection reads.
func (e storeEnricher) collectionChildCounts(ctx context.Context, values []item.Item) (map[dto.UUID]int, error) {
	collectionIDs := make([]dto.UUID, 0, len(values))
	for _, value := range values {
		if value.Kind == item.KindCollectionFolder {
			collectionIDs = append(collectionIDs, value.ID)
		}
	}
	// Counted in SQL: loading every descendant to count direct children took
	// most of /UserViews on a ~13k-item library.
	return e.items.CollectionChildCounts(ctx, collectionIDs)
}

// personImages reads the Primary image of every credited Person entity once
// for the page. It returns nil when the People field is not requested.
func (e storeEnricher) personImages(ctx context.Context, values []item.Item, options store.ItemQueryDTOOptions) (map[dto.UUID]item.ImageInfo, error) {
	if !hasField(options.Fields, "People") {
		return nil, nil
	}
	ids := make([]dto.UUID, 0)
	seen := make(map[dto.UUID]struct{})
	for _, value := range values {
		for _, person := range value.People {
			if person.PersonID.IsNil() {
				continue
			}
			if _, found := seen[person.PersonID]; !found {
				seen[person.PersonID] = struct{}{}
				ids = append(ids, person.PersonID)
			}
		}
	}
	return e.items.PrimaryImages(ctx, ids)
}

type relatedItemGraph struct {
	albums  map[dto.UUID]item.Item
	seasons map[dto.UUID]item.Item
	series  map[dto.UUID]item.Item
	// discFolders maps the ID of a Folder that is the parent of an Audio
	// item to the Folder's own parent ID.
	discFolders map[dto.UUID]dto.UUID
}

func (e storeEnricher) relatedItemGraph(ctx context.Context, values []item.Item) (relatedItemGraph, error) {
	graph := relatedItemGraph{albums: map[dto.UUID]item.Item{}, seasons: map[dto.UUID]item.Item{}, series: map[dto.UUID]item.Item{}, discFolders: map[dto.UUID]dto.UUID{}}
	ids := make([]dto.UUID, 0, len(values)*3)
	seen := make(map[dto.UUID]struct{}, len(values)*3)
	add := func(id *dto.UUID) {
		if id != nil && !id.IsNil() {
			if _, found := seen[*id]; !found {
				seen[*id] = struct{}{}
				ids = append(ids, *id)
			}
		}
	}
	for _, value := range values {
		if value.Kind == item.KindAudio {
			add(value.ParentID)
		}
		if value.Kind == item.KindEpisode {
			add(value.SeasonID)
			add(value.SeriesID)
		}
	}
	if len(ids) == 0 {
		return graph, nil
	}
	result, err := e.items.Query(ctx, store.ItemQuery{ItemIDs: ids, IncludeItemTypes: []item.Kind{item.KindMusicAlbum, item.KindSeason, item.KindSeries, item.KindFolder}, EnableTotalRecordCount: false})
	if err != nil {
		return relatedItemGraph{}, err
	}
	folderIDs := make([]dto.UUID, 0)
	for _, value := range result.Items {
		switch value.Kind {
		case item.KindMusicAlbum:
			graph.albums[value.ID] = value
		case item.KindSeason:
			graph.seasons[value.ID] = value
		case item.KindSeries:
			graph.series[value.ID] = value
		case item.KindFolder:
			if value.ParentID != nil && !value.ParentID.IsNil() {
				graph.discFolders[value.ID] = *value.ParentID
				folderIDs = append(folderIDs, *value.ParentID)
			}
		}
	}
	if len(folderIDs) == 0 {
		return graph, nil
	}
	// A track of a multi-disc album is a child of a disc folder ("CD 01")
	// below the album, so its album is the folder's parent.
	albums, err := e.items.Query(ctx, store.ItemQuery{ItemIDs: folderIDs, IncludeItemTypes: []item.Kind{item.KindMusicAlbum}, EnableTotalRecordCount: false})
	if err != nil {
		return relatedItemGraph{}, err
	}
	for _, value := range albums.Items {
		graph.albums[value.ID] = value
	}
	return graph, nil
}

// album returns the MusicAlbum of an Audio item: its parent, or the parent
// of its disc folder (Audio.AlbumEntity is FindParent<MusicAlbum>()).
func (graph relatedItemGraph) album(parentID dto.UUID) (item.Item, bool) {
	if related, found := graph.albums[parentID]; found {
		return related, true
	}
	if albumID, found := graph.discFolders[parentID]; found {
		related, found := graph.albums[albumID]
		return related, found
	}
	return item.Item{}, false
}

type namedEntityGraph struct {
	genres      map[string]dto.UUID
	musicGenres map[string]dto.UUID
	artists     map[string]dto.UUID
}

func (e storeEnricher) namedEntities(ctx context.Context, values []item.Item, options store.ItemQueryDTOOptions) (namedEntityGraph, error) {
	graph := namedEntityGraph{genres: map[string]dto.UUID{}, musicGenres: map[string]dto.UUID{}, artists: map[string]dto.UUID{}}
	wantGenres := hasField(options.Fields, "Genres")
	wantArtists := false
	for _, value := range values {
		if hasArtistValues(value.Kind) && (len(value.Values.Artists) != 0 || len(value.Values.AlbumArtists) != 0) {
			wantArtists = true
			break
		}
	}
	if !wantGenres && !wantArtists {
		return graph, nil
	}
	// Resolve only the names the page references. Loading every Genre,
	// MusicGenre, and MusicArtist cost a scan of all of them, with their
	// satellites, on each page; a library page or UserViews then took hundreds
	// of milliseconds whatever its size.
	keys := map[string]bool{}
	addKeys := func(names []string) {
		for _, name := range names {
			if key := item.CleanName(strings.TrimSpace(name)); key != "" {
				keys[key] = true
			}
		}
	}
	for _, value := range values {
		addKeys(value.Values.Genres)
		if hasArtistValues(value.Kind) {
			addKeys(value.Values.Artists)
			addKeys(value.Values.AlbumArtists)
		}
	}
	if len(keys) == 0 {
		return graph, nil
	}
	cleanNames := make([]string, 0, len(keys))
	for key := range keys {
		cleanNames = append(cleanNames, key)
	}
	kinds := []item.Kind{item.KindGenre, item.KindMusicGenre, item.KindMusicArtist}
	ids, err := e.items.ItemIDsByCleanName(ctx, kinds, cleanNames)
	if err != nil {
		return namedEntityGraph{}, err
	}
	if len(ids) == 0 {
		return graph, nil
	}
	// The ItemIds query keeps the default filters and order of the former
	// unrestricted query, so the first match for a name is the same item.
	result, err := e.items.Query(ctx, store.ItemQuery{ItemIDs: ids, IncludeItemTypes: kinds, EnableTotalRecordCount: false})
	if err != nil {
		return namedEntityGraph{}, err
	}
	for _, value := range result.Items {
		if value.Name == nil {
			continue
		}
		key := item.CleanName(strings.TrimSpace(*value.Name))
		if key == "" {
			continue
		}
		switch value.Kind {
		case item.KindGenre:
			if _, found := graph.genres[key]; !found {
				graph.genres[key] = value.ID
			}
		case item.KindMusicGenre:
			if _, found := graph.musicGenres[key]; !found {
				graph.musicGenres[key] = value.ID
			}
		case item.KindMusicArtist:
			if _, found := graph.artists[key]; !found {
				graph.artists[key] = value.ID
			}
		}
	}
	return graph, nil
}

func applyRelatedProjection(enrichment *Enrichment, value item.Item, graph relatedItemGraph, entities namedEntityGraph) {
	if value.Kind == item.KindEpisode {
		if value.SeriesID != nil {
			if related, found := graph.series[*value.SeriesID]; found {
				id := related.ID
				enrichment.SeriesID = &id
				enrichment.SeriesName = dereference(related.Name)
			}
		}
		if value.SeasonID != nil {
			if related, found := graph.seasons[*value.SeasonID]; found {
				id := related.ID
				enrichment.SeasonID = &id
				enrichment.SeasonName = dereference(related.Name)
			}
		}
	}
	if value.Kind == item.KindAudio {
		if value.ParentID != nil {
			if related, found := graph.album(*value.ParentID); found {
				id := related.ID
				enrichment.AlbumID = &id
				enrichment.Album = dereference(related.Name)
			}
		}
	}
	if hasArtistValues(value.Kind) {
		enrichment.ArtistItems = namedPairs(value.Values.Artists, entities.artists)
		enrichment.AlbumArtists = namedPairs(value.Values.AlbumArtists, entities.artists)
	}
	genreIDs := entities.genres
	if item.MediaTypeOf(value) == item.MediaTypeAudio {
		genreIDs = entities.musicGenres
	}
	enrichment.GenreItems = namedPairs(value.Values.Genres, genreIDs)
}

// hasArtistValues reports whether items of kind implement IHasArtist and
// IHasAlbumArtist (Audio and MusicAlbum), whose DTOs name artist items.
func hasArtistValues(kind item.Kind) bool {
	return kind == item.KindAudio || kind == item.KindMusicAlbum
}

func namedPairs(names []string, ids map[string]dto.UUID) []dto.NameGuidPair {
	result := make([]dto.NameGuidPair, 0, len(names))
	for _, name := range names {
		if id, found := ids[item.CleanName(strings.TrimSpace(name))]; found {
			result = append(result, dto.NameGuidPair{Name: name, ID: id})
		}
	}
	return result
}

func hasSubtitleStream(streams []playback.MediaStream) bool {
	for _, stream := range streams {
		if stream.Type == playback.StreamTypeSubtitle {
			return true
		}
	}
	return false
}

// channelPlaceholderSources ports LiveTvChannel.GetMediaSources
// (LiveTvChannel.cs:125-140): the placeholder source of a channel is an
// infinite stream with the protocol of the channel URL. Every Deadfin
// channel plays through the HTTP broker and the item stores no URL (D-100),
// so the protocol is Http.
func channelPlaceholderSources(sources []playback.MediaSourceInfo) {
	for index := range sources {
		sources[index].Protocol = playback.MediaProtocolHTTP
		sources[index].IsInfiniteStream = sources[index].RunTimeTicks == nil
	}
}

func dtoMediaSources(sources []playback.MediaSourceInfo) []dto.MediaSourceInfo {
	if sources == nil {
		return nil
	}
	result := make([]dto.MediaSourceInfo, len(sources))
	for index := range sources {
		wire, err := json.Marshal(itemMediaSourceProjection(sources[index]))
		if err != nil {
			return nil
		}
		if err := json.Unmarshal(wire, &result[index]); err != nil {
			return nil
		}
	}
	return result
}

// applyFolderUserData ports Folder.FillUserDataDtoValues: eligible folders
// publish child-derived state while leaves preserve direct user data untouched.
func applyFolderUserData(data *dto.UserItemDataDto, value item.Item, counts store.FolderUserDataCounts) {
	if data == nil || !supportsFolderUserDataFromChildren(value) {
		return
	}
	unplayed := counts.Total - counts.Played
	data.UnplayedItemCount = &unplayed
	if counts.Total == 0 {
		data.Played = true
		return
	}
	percentage := float64(counts.Played) / float64(counts.Total) * 100
	data.PlayedPercentage = &percentage
	data.Played = counts.Played >= counts.Total
}

// applyLeafUserData ports BaseItem.FillUserDataDtoValues: a leaf with a
// runtime publishes PlayedPercentage from its playback position. The base
// method runs for items that do not override it (Folder overrides it for
// child-derived aggregation), so this path is leaf-only.
func applyLeafUserData(data *dto.UserItemDataDto, value item.Item) {
	if data == nil || item.IsFolder(value) {
		return
	}
	if value.RunTimeTicks == nil || *value.RunTimeTicks <= 0 {
		return
	}
	if data.PlaybackPositionTicks <= 0 {
		return
	}
	percentage := float64(data.PlaybackPositionTicks) / float64(*value.RunTimeTicks) * 100
	data.PlayedPercentage = &percentage
}

func supportsFolderUserDataFromChildren(value item.Item) bool {
	if !item.IsFolder(value) || !item.SupportsPlayedStatus(value) || value.SourceType != item.SourceTypeLibrary {
		return false
	}
	switch value.Kind {
	case item.KindCollectionFolder, item.KindUserRootFolder, item.KindUserView, item.KindChannel:
		return false
	default:
		return true
	}
}

func dtoMediaStreams(values []playback.MediaStream) []dto.MediaStreamInfo {
	if values == nil {
		return nil
	}
	wire, err := json.Marshal(itemMediaStreamProjection(values))
	if err != nil {
		return nil
	}
	var result []dto.MediaStreamInfo
	if err := json.Unmarshal(wire, &result); err != nil {
		return nil
	}
	return result
}

// itemMediaStreamProjection models the repository-backed /Items shape. The
// ffprobe range token is deliberately unavailable from Jellyfin's persisted
// stream entity; HLG transfer/primaries likewise remain absent in the attested
// repository projection while ColorSpace survives.
func itemMediaStreamProjection(values []playback.MediaStream) []playback.MediaStream {
	if values == nil {
		return nil
	}
	projected := make([]playback.MediaStream, len(values))
	copy(projected, values)
	for index := range projected {
		stream := &projected[index]
		stream.ColorRange = nil
		if stream.ColorTransfer != nil && strings.EqualFold(*stream.ColorTransfer, "arib-std-b67") {
			stream.ColorTransfer = nil
			stream.ColorPrimaries = nil
		}
	}
	return projected
}

func itemMediaSourceProjection(source playback.MediaSourceInfo) playback.MediaSourceInfo {
	source.MediaStreams = itemMediaStreamProjection(source.MediaStreams)
	return source
}

func playbackSourceUserData(data store.UserData) playback.SourceUserData {
	result := playback.SourceUserData{PositionTicks: data.PositionTicks}
	if data.AudioStreamIndex != nil {
		value := *data.AudioStreamIndex
		result.AudioStreamIndex = &value
	}
	if data.SubtitleStreamIndex != nil {
		value := *data.SubtitleStreamIndex
		result.SubtitleStreamIndex = &value
	}
	if data.LastPlayed != nil {
		value := *data.LastPlayed
		result.LastPlayed = &value
	}
	return result
}

type matchedUserData struct {
	key   string
	data  store.UserData
	found bool
}

// matchingUserData returns the first persisted row in the item's deterministic
// user-data-key order, retaining the first key for an absent-row DTO.
func matchingUserData(value item.Item, inputs item.UserDataInputs, rows map[string]store.UserData) matchedUserData {
	key, data, found := store.MatchUserDataRow(value, inputs, rows)
	return matchedUserData{key: key, data: data, found: found}
}

func dtoUserData(itemID dto.UUID, matched matchedUserData) *dto.UserItemDataDto {
	result := &dto.UserItemDataDto{ItemID: itemID, Key: matched.key}
	if matched.found {
		applyUserItemData(result, matched.data)
	}
	return result
}

// UserItemData projects a committed user-data mutation through the same DTO
// mapping used by item pages.
func UserItemData(data store.UserData) dto.UserItemDataDto {
	result := dto.UserItemDataDto{ItemID: data.ItemID, Key: data.CustomKey}
	applyUserItemData(&result, data)
	return result
}

func applyUserItemData(result *dto.UserItemDataDto, data store.UserData) {
	result.Played, result.PlayCount, result.IsFavorite, result.PlaybackPositionTicks = data.Played, data.PlayCount, data.IsFavorite, data.PositionTicks
	result.Rating = data.Rating
	if data.Likes != nil {
		liked := *data.Likes != 0
		result.Likes = &liked
	}
	if data.LastPlayed != nil {
		converted := dto.NewTime(*data.LastPlayed)
		result.LastPlayedDate = &converted
	}
}

func imagesEnrichment(value item.Item, options store.ItemQueryDTOOptions) Enrichment {
	if !options.EnableImages {
		return Enrichment{}
	}
	result := Enrichment{ImageTags: make(map[string]string), ImageBlurHashes: make(map[string]map[string]string)}
	for _, image := range value.ImageInfos {
		tag := filepath.Base(image.Path)
		if tag == "" || tag == "." {
			continue
		}
		kind := image.Type.String()
		if image.Type == item.ImageTypeBackdrop {
			result.BackdropImageTags = append(result.BackdropImageTags, tag)
		} else if image.Index == 0 {
			result.ImageTags[kind] = tag
		}
		if image.BlurHash != nil {
			if result.ImageBlurHashes[kind] == nil {
				result.ImageBlurHashes[kind] = make(map[string]string)
			}
			result.ImageBlurHashes[kind][tag] = *image.BlurHash
		}
	}
	return result
}

// expandSources replaces virtual path placeholders in media source and stream
// paths before they reach the DTO. The playback package does not expand these
// paths when the enricher assembles sources directly from stored records.
func expandSources(sources []playback.MediaSourceInfo, expander path.VirtualPathExpander) []playback.MediaSourceInfo {
	for index := range sources {
		sources[index].Path = expander.Expand(sources[index].Path)
		for stream := range sources[index].MediaStreams {
			sources[index].MediaStreams[stream].Path = expander.Expand(sources[index].MediaStreams[stream].Path)
		}
	}
	return sources
}

// expandStreams replaces virtual path placeholders in stream paths before they
// reach the DTO. Streams that carry persisted item or subtitle paths expand
// the same tokens as item paths.
func expandStreams(streams []playback.MediaStream, expander path.VirtualPathExpander) []playback.MediaStream {
	for index := range streams {
		streams[index].Path = expander.Expand(streams[index].Path)
	}
	return streams
}
