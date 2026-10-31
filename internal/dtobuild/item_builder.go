// SPDX-License-Identifier: GPL-2.0-only

package dtobuild

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf16"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/path"
	"github.com/deadfin-org/deadfin/internal/store"
)

// BatchEnricher computes supplemental projection facts for a complete result
// page. Builder invokes it at most once per Build call, preventing per-item
// projection I/O.
type BatchEnricher interface {
	Enrich(context.Context, []item.Item, store.ItemQueryDTOOptions, *dto.UUID) (map[dto.UUID]Enrichment, error)
}

// Enrichment contains optional batch-derived facts keyed by item ID. Builder
// only publishes each fact when its corresponding DTO field is requested.
type Enrichment struct {
	MediaSources       []dto.MediaSourceInfo
	MediaStreams       []dto.MediaStreamInfo
	HasSubtitles       bool
	People             []dto.BaseItemPerson
	ProviderIDs        map[string]string
	UserData           *dto.UserItemDataDto
	ChildCount         *int
	RecursiveItemCount *int
	SeriesName         string
	SeasonName         string
	SeriesID           *dto.UUID
	SeasonID           *dto.UUID
	AlbumID            *dto.UUID
	Album              string
	GenreItems         []dto.NameGuidPair
	ArtistItems        []dto.NameGuidPair
	AlbumArtists       []dto.NameGuidPair
	ImageTags          map[string]string
	BackdropImageTags  []string
	ImageBlurHashes    map[string]map[string]string
	InheritedImageTags map[string]string
}

// Builder projects persisted item facts into API DTOs. ItemStore is retained as
type Builder struct {
	items    *store.ItemStore
	enricher BatchEnricher
	serverID string
	paths    path.VirtualPathExpander
}

// NewBuilder creates a DTO builder using items for batch-backed projections.
// The zero-value builder expands nothing; the composition root calls WithPaths
// to inject a configured expander so item and image paths expand before they
// reach API responses.
func NewBuilder(items *store.ItemStore, enricher BatchEnricher, serverIDs ...string) *Builder {
	var serverID string
	if len(serverIDs) != 0 {
		serverID = serverIDs[0]
	}
	return &Builder{items: items, enricher: enricher, serverID: serverID}
}

// WithPaths sets the virtual-path expander on the builder and on its enricher
// when the enricher accepts one. Call this from the composition root with the
// expander constructed from config.Paths. A builder that does not receive a
// call leaves paths unchanged, which is correct for tests that do not test
// expansion.
func (b *Builder) WithPaths(paths path.VirtualPathExpander) *Builder {
	b.paths = paths
	if setter, ok := b.enricher.(pathExpanderSetter); ok {
		setter.setPaths(paths)
	}
	return b
}

// pathExpanderSetter is an optional interface that an enricher implements when
// it needs the virtual-path expander for media source and stream projections.
type pathExpanderSetter interface {
	setPaths(path.VirtualPathExpander)
}

// Build projects a page of already-loaded items. It preserves input order and
// invokes the optional enricher once for the entire page.
func (b *Builder) Build(ctx context.Context, values []item.Item, options store.ItemQueryDTOOptions, userID *dto.UUID) (dto.QueryResult[dto.BaseItemDto], error) {
	result := dto.NewQueryResult[dto.BaseItemDto]()
	enrichment := make(map[dto.UUID]Enrichment)
	if b.enricher != nil {
		var err error
		enrichment, err = b.enricher.Enrich(ctx, values, options, userID)
		if err != nil {
			return dto.QueryResult[dto.BaseItemDto]{}, err
		}
	}
	result.Items = make([]dto.BaseItemDto, 0, len(values))
	for _, value := range values {
		result.Items = append(result.Items, projectCore(value, enrichment[value.ID], options, userID, b.serverID, b.paths))
	}
	result.TotalRecordCount = len(result.Items)
	return result, nil
}

// BuildDetailed projects selected items using the source controller's default
// DtoOptions behavior. Unlike Build, this path always includes BaseItemDto's
// default detail facts while retaining every explicitly requested projection
// field. It keeps all enrichment batched in the single Build invocation.
func (b *Builder) BuildDetailed(ctx context.Context, values []item.Item, options store.ItemQueryDTOOptions, userID *dto.UUID) (dto.QueryResult[dto.BaseItemDto], error) {
	detailed := DetailedOptions(options)
	result, err := b.Build(ctx, values, detailed, userID)
	if err != nil {
		return dto.QueryResult[dto.BaseItemDto]{}, err
	}
	for index := range result.Items {
		projectDetailDefaults(&result.Items[index], values[index])
		projectDetailDimensions(&result.Items[index], values[index], detailed)
	}
	return result, nil
}

// DetailedOptions adds fields that DtoService writes without a Fields request.
// It deliberately leaves its input unchanged: list/page projection remains
// solely controlled by its caller's requested fields.
func DetailedOptions(options store.ItemQueryDTOOptions) store.ItemQueryDTOOptions {
	detailed := options
	detailed.Fields = append([]string(nil), detailDefaultFields...)
	for _, field := range options.Fields {
		if !hasField(detailed.Fields, field) {
			detailed.Fields = append(detailed.Fields, field)
		}
	}
	return detailed
}

var detailDefaultFields = []string{
	"AirTime", "CanDelete", "CanDownload", "ChannelInfo", "Chapters", "Trickplay", "ChildCount", "CumulativeRunTimeTicks", "CustomRating", "DateCreated", "DateLastMediaAdded", "DisplayPreferencesId", "Etag", "ExternalUrls", "Genres", "ItemCounts", "MediaSourceCount", "MediaSources", "OriginalTitle", "Overview", "ParentId", "Path", "People", "PlayAccess", "ProductionLocations", "ProviderIds", "PrimaryImageAspectRatio", "RecursiveItemCount", "Settings", "SeriesStudio", "SortName", "SpecialEpisodeNumbers", "Studios", "Taglines", "Tags", "RemoteTrailers", "MediaStreams", "DateLastRefreshed", "DateLastSaved", "ChannelImage", "EnableMediaSourceDisplay", "Width", "Height", "ExtraIds", "LocalTrailerCount", "IsHD", "SpecialFeatureCount",
}

func projectDetailDefaults(result *dto.BaseItemDto, value item.Item) {
	canManage := true
	canDownload := item.CanDownload(value)
	zero := 0
	fullAccess := dto.PlayAccess("Full")
	result.CanDelete = boolPointer(canManage)
	result.CanDownload = boolPointer(canDownload)
	result.EnableMediaSourceDisplay = boolPointer(true)
	result.PlayAccess = &fullAccess
	result.DisplayPreferencesID = detailDisplayPreferencesID(value)
	result.LockData = boolPointer(canManage)
	result.LockedFields = dtoMetadataFieldsOrEmpty(value.LockedFields)
	result.ExternalURLs = []dto.ExternalURL{}
	result.RemoteTrailers = []dto.MediaURL{}
	result.Chapters = []dto.ChapterInfo{}
	result.Trickplay = map[string]map[int]dto.TrickplayInfoDto{}
	result.Studios = dtoNamePairsOrEmpty(value.Values.Studios)
	result.ProductionLocations = stringsOrEmpty(value.Values.ProductionLocations)
	result.LocalTrailerCount = &zero
	result.SpecialFeatureCount = &zero
	if !item.IsFolder(value) {
		result.ChildCount = nil
		result.RecursiveItemCount = nil
	}
}

// projectDetailDimensions matches DtoService's detail-only fallback. Stored
// item dimensions take precedence; when the request explicitly projects top-
// level streams, the first video stream supplies only missing dimensions.
func projectDetailDimensions(result *dto.BaseItemDto, value item.Item, options store.ItemQueryDTOOptions) {
	if !hasField(options.Fields, "MediaStreams") || (value.Width != nil && value.Height != nil) {
		return
	}
	for _, stream := range result.MediaStreams {
		if stream.Type != dto.MediaStreamType("Video") {
			continue
		}
		if value.Width == nil {
			result.Width = stream.Width
		}
		if value.Height == nil {
			result.Height = stream.Height
		}
		return
	}
}

func dtoMetadataFieldsOrEmpty(values []item.MetadataField) []dto.MetadataField {
	result := make([]dto.MetadataField, len(values))
	for index, value := range values {
		result[index] = dto.MetadataField(value.String())
	}
	return result
}

func dtoNamePairsOrEmpty(values []string) []dto.NameGuidPair {
	result := make([]dto.NameGuidPair, len(values))
	for index, value := range values {
		result[index] = dto.NameGuidPair{Name: value}
	}
	return result
}

func detailDisplayPreferencesID(value item.Item) string {
	// BaseItem uses a folder's concrete item identity for its display
	// preferences; non-folder media uses its CLR-type EntityId.
	if item.IsFolder(value) {
		return value.ID.String()
	}
	return dotNetEntityID(detailCLRType(value.Kind)).String()
}

func detailCLRType(kind item.Kind) string {
	switch kind {
	case item.KindMovie:
		return "MediaBrowser.Controller.Entities.Movies.Movie"
	case item.KindBoxSet:
		return "MediaBrowser.Controller.Entities.Movies.BoxSet"
	case item.KindSeries:
		return "MediaBrowser.Controller.Entities.TV.Series"
	case item.KindSeason:
		return "MediaBrowser.Controller.Entities.TV.Season"
	case item.KindEpisode:
		return "MediaBrowser.Controller.Entities.TV.Episode"
	case item.KindAudio:
		return "MediaBrowser.Controller.Entities.Audio.Audio"
	case item.KindAudioBook:
		return "MediaBrowser.Controller.Entities.AudioBook"
	case item.KindMusicArtist:
		return "MediaBrowser.Controller.Entities.Audio.MusicArtist"
	case item.KindMusicAlbum:
		return "MediaBrowser.Controller.Entities.Audio.MusicAlbum"
	case item.KindBook:
		return "MediaBrowser.Controller.Entities.Book"
	case item.KindPlaylist:
		return "MediaBrowser.Controller.Playlists.Playlist"
	default:
		return "MediaBrowser.Controller.Entities." + kind.String()
	}
}

// dotNetEntityID reproduces the source EntityId hash: UTF-16LE MD5 followed
// by Guid's mixed-endian byte representation in N format.
func dotNetEntityID(typeName string) dto.UUID {
	parsed, err := dto.ParseUUID(dotNetMD5GUID(typeName))
	if err != nil {
		panic("dtobuild: deterministic detail entity ID")
	}
	return parsed
}

// dotNetMD5GUID formats an MD5 UTF-16LE digest as Guid.ToString("N").
func dotNetMD5GUID(value string) string {
	runes := utf16.Encode([]rune(value))
	encoded := make([]byte, len(runes)*2)
	for index, value := range runes {
		binary.LittleEndian.PutUint16(encoded[index*2:], value)
	}
	digest := md5.Sum(encoded)
	var text [32]byte
	hex.Encode(text[:], []byte{digest[3], digest[2], digest[1], digest[0], digest[5], digest[4], digest[7], digest[6], digest[8], digest[9], digest[10], digest[11], digest[12], digest[13], digest[14], digest[15]})
	return string(text[:])
}

const dotNetUnixEpochTicks int64 = 621355968000000000

func baseItemEtag(value item.Item) string {
	saved := time.Time{}
	if value.DateLastSaved != nil {
		saved = *value.DateLastSaved
	}
	utc := saved.UTC()
	ticks := utc.Unix()*10_000_000 + int64(utc.Nanosecond())/100 + dotNetUnixEpochTicks
	return dotNetMD5GUID(strconv.FormatInt(ticks, 10))
}

func projectCore(value item.Item, enrichment Enrichment, options store.ItemQueryDTOOptions, userID *dto.UUID, serverID string, paths path.VirtualPathExpander) dto.BaseItemDto {
	result := dto.BaseItemDto{
		ID: value.ID, Name: dereference(value.Name), ServerID: serverID,
		Container: dereference(value.Container), EndDate: value.EndDate, CriticRating: value.CriticRating,
		OfficialRating: dereference(value.OfficialRating),
		ProductionYear: value.ProductionYear, RunTimeTicks: value.RunTimeTicks,
		IndexNumber: value.IndexNumber, ParentIndexNumber: value.ParentIndexNumber,
		ChannelID: value.ChannelID, Type: dtoItemKind(value.Kind),
		OriginalLanguage: dereference(value.OriginalLanguage), ImageBlurHashes: make(map[string]map[string]string),
	}
	if value.PremiereDate != nil {
		result.PremiereDate = dto.NewItemPremiereDate(*value.PremiereDate, false)
	}
	mediaType := item.MediaTypeOf(value)
	result.MediaType = dto.MediaType(mediaType.String())
	if value.SourceType == item.SourceTypeChannel {
		result.SourceType = value.SourceType.String()
	}
	// Upstream writes IsFolder for folders and IHasMediaSources. Photo has a
	// concrete Photo media type but is neither, so its nullable DTO field must
	// remain absent rather than serializing false.
	if item.IsFolder(value) || itemHasMediaSources(value) {
		result.IsFolder = boolPointer(item.IsFolder(value))
	}
	if value.Kind != item.KindLiveTvProgram && value.Kind != item.KindTvProgram {
		locationType := dto.LocationType("FileSystem")
		// BaseItem.LocationType is Virtual for an empty path. A virtual
		// season (SeriesMetadataService.CreateSeasonAsync) has no path.
		if value.Kind == item.KindSeason && value.Path == nil {
			locationType = dto.LocationType("Virtual")
		}
		result.LocationType = &locationType
	}
	if hasField(options.Fields, "DateCreated") {
		result.DateCreated = value.DateCreated
	}
	if hasField(options.Fields, "DateLastMediaAdded") && item.IsFolder(value) {
		dateLastMediaAdded := time.Time{}
		if value.DateLastMediaAdded != nil {
			dateLastMediaAdded = *value.DateLastMediaAdded
		}
		projectedDateLastMediaAdded := dto.NewTime(dateLastMediaAdded)
		result.DateLastMediaAdded = &projectedDateLastMediaAdded
	}
	if hasField(options.Fields, "Etag") {
		result.Etag = baseItemEtag(value)
	}
	if hasField(options.Fields, "Settings") {
		result.ForcedSortName = dereference(value.ForcedSortName)
		result.PreferredMetadataLanguage = dereference(value.PreferredMetadataLanguage)
		result.PreferredMetadataCountryCode = dereference(value.PreferredMetadataCountryCode)
	}
	if hasField(options.Fields, "OriginalTitle") {
		result.OriginalTitle = dereference(value.OriginalTitle)
	}
	if hasField(options.Fields, "Path") {
		result.Path = paths.Expand(dereference(value.Path))
	}
	if hasField(options.Fields, "SortName") {
		result.SortName = dereference(value.SortName)
	}
	if hasField(options.Fields, "CustomRating") {
		result.CustomRating = dereference(value.CustomRating)
	}
	if hasField(options.Fields, "Overview") {
		result.Overview = dereference(value.Overview)
	}
	if hasField(options.Fields, "Genres") {
		result.Genres = stringsOrEmpty(value.Values.Genres)
		result.GenreItems = enrichment.GenreItems
		if result.GenreItems == nil {
			result.GenreItems = []dto.NameGuidPair{}
		}
	}
	if hasField(options.Fields, "Tags") {
		result.Tags = stringsOrEmpty(value.Values.Tags)
	}
	if hasField(options.Fields, "ProductionLocations") && (len(value.Values.ProductionLocations) != 0 || value.Kind == item.KindMovie) {
		result.ProductionLocations = value.Values.ProductionLocations
		if result.ProductionLocations == nil {
			result.ProductionLocations = []string{}
		}
	}
	if hasField(options.Fields, "Taglines") {
		result.Taglines = []string{}
		if value.Tagline != nil && *value.Tagline != "" {
			result.Taglines = []string{*value.Tagline}
		}
	}
	if hasField(options.Fields, "Width") && value.Width != nil {
		result.Width = value.Width
	}
	if hasField(options.Fields, "Height") && value.Height != nil {
		result.Height = value.Height
	}
	if hasField(options.Fields, "PrimaryImageAspectRatio") {
		result.PrimaryImageAspectRatio = primaryImageAspectRatio(value.ImageInfos)
	}
	result.ExtraType = dtoExtraType(value.ExtraType)
	result.Video3DFormat = dtoVideo3DFormat(value.Video3DFormat)
	if mediaType != item.MediaTypeAudio {
		result.VideoType = dtoVideoType(value.VideoType)
	}
	result.IsoType = dtoIsoType(value.IsoType)
	if value.IsPlaceHolder {
		result.IsPlaceHolder = boolPointer(true)
	}
	if enrichment.HasSubtitles {
		result.HasSubtitles = boolPointer(true)
	}
	if value.Extra.Episode != nil {
		result.IndexNumberEnd = value.Extra.Episode.IndexNumberEnd
	}
	if value.Extra.Series != nil {
		result.AirTime = dereference(value.Extra.Series.AirTime)
		result.DisplayOrder = dereference(value.Extra.Series.DisplayOrder)
		if value.Extra.Series.AirDays != nil {
			result.AirDays = make([]time.Weekday, len(value.Extra.Series.AirDays))
			for index, day := range value.Extra.Series.AirDays {
				result.AirDays[index] = time.Weekday(day)
			}
		}
	}
	if value.Extra.BoxSet != nil {
		result.DisplayOrder = dereference(value.Extra.BoxSet.DisplayOrder)
	}
	if value.Extra.Video != nil {
		result.AspectRatio = dereference(value.Extra.Video.AspectRatio)
	}
	// PlaylistsFolder overrides CollectionType with playlists.
	if value.Kind == item.KindPlaylistsFolder {
		collectionType := dto.CollectionType(item.CollectionTypePlaylists.String())
		result.CollectionType = &collectionType
	}
	// A mixed library has upstream's null CollectionType.
	if value.Extra.CollectionFolder != nil && value.Extra.CollectionFolder.CollectionType != nil && *value.Extra.CollectionFolder.CollectionType != item.CollectionTypeUnknown {
		collectionType := dto.CollectionType(value.Extra.CollectionFolder.CollectionType.String())
		result.CollectionType = &collectionType
	}
	if value.Extra.UserView != nil && value.Extra.UserView.ViewType != nil {
		collectionType := dto.CollectionType(value.Extra.UserView.ViewType.String())
		result.CollectionType = &collectionType
	}
	if value.Extra.Photo != nil {
		result.CameraMake = dereference(value.Extra.Photo.CameraMake)
		result.CameraModel = dereference(value.Extra.Photo.CameraModel)
		result.Software = dereference(value.Extra.Photo.Software)
		result.ExposureTime = value.Extra.Photo.ExposureTime
		result.FocalLength = value.Extra.Photo.FocalLength
		result.Aperture = value.Extra.Photo.Aperture
		result.ShutterSpeed = value.Extra.Photo.ShutterSpeed
		result.Latitude = value.Extra.Photo.Latitude
		result.Longitude = value.Extra.Photo.Longitude
		result.Altitude = value.Extra.Photo.Altitude
		result.IsoSpeedRating = value.Extra.Photo.IsoSpeedRating
		result.ImageOrientation = dtoImageOrientation(value.Extra.Photo.Orientation)
	}
	if hasField(options.Fields, "ParentId") {
		result.ParentID = value.ParentID
	}
	if hasField(options.Fields, "MediaSources") && itemHasMediaSources(value) {
		result.MediaSources = enrichment.MediaSources
		if result.MediaSources == nil {
			result.MediaSources = []dto.MediaSourceInfo{}
		}
	}
	if hasField(options.Fields, "MediaStreams") && itemHasMediaSources(value) {
		result.MediaStreams = enrichment.MediaStreams
		if result.MediaStreams == nil {
			result.MediaStreams = []dto.MediaStreamInfo{}
		}
	}
	if hasField(options.Fields, "People") {
		result.People = enrichment.People
		if result.People == nil {
			result.People = dtoPeopleOrEmpty(value.People)
		}
	}
	if hasField(options.Fields, "ProviderIds") {
		result.ProviderIDs = enrichment.ProviderIDs
		if result.ProviderIDs == nil {
			result.ProviderIDs = dtoProviderIDsOrEmpty(value.ProviderIDs)
		}
	}
	if options.EnableUserData && userID != nil {
		result.UserData = enrichment.UserData
	}
	if hasField(options.Fields, "ChildCount") {
		result.ChildCount = enrichment.ChildCount
	}
	if hasField(options.Fields, "RecursiveItemCount") {
		result.RecursiveItemCount = enrichment.RecursiveItemCount
	}
	result.SeriesName = dereference(value.SeriesName)
	if enrichment.SeriesName != "" {
		result.SeriesName = enrichment.SeriesName
	}
	result.SeasonName = dereference(value.SeasonName)
	if enrichment.SeasonName != "" {
		result.SeasonName = enrichment.SeasonName
	}
	switch value.Kind {
	case item.KindEpisode:
		result.SeriesID = enrichment.SeriesID
		result.SeasonID = enrichment.SeasonID
	case item.KindSeason:
		result.SeriesID = value.SeriesID
	}
	if hasArtistValues(value.Kind) {
		result.Artists = stringsOrEmpty(value.Values.Artists)
		result.ArtistItems = enrichment.ArtistItems
		if result.ArtistItems == nil {
			result.ArtistItems = []dto.NameGuidPair{}
		}
		result.AlbumArtists = enrichment.AlbumArtists
		if result.AlbumArtists == nil {
			result.AlbumArtists = []dto.NameGuidPair{}
		}
		if len(value.Values.AlbumArtists) != 0 {
			result.AlbumArtist = value.Values.AlbumArtists[0]
		}
	}
	if value.Kind == item.KindAudio {
		result.HasLyrics = boolPointer(false)
		result.Album = dereference(value.Album)
		if enrichment.Album != "" {
			result.Album = enrichment.Album
		}
		result.AlbumID = enrichment.AlbumID
		if result.PremiereDate == nil {
			result.PremiereDate = dto.NewItemPremiereDate(time.Time{}, true)
		}
	}
	if options.EnableImages {
		projectImages(&result, enrichment, options)
	}
	return result
}

// itemHasMediaSources mirrors the upstream IHasMediaSources implementations:
// Video (including its derived kinds and TV channels) and Audio. Other
// file-backed items such as Photo and Book expose Path but not MediaSources.
func itemHasMediaSources(value item.Item) bool {
	switch item.MediaTypeOf(value) {
	case item.MediaTypeAudio, item.MediaTypeVideo:
		return true
	default:
		return false
	}
}

func boolPointer(value bool) *bool { return &value }

// dtoItemKind is BaseItem.GetBaseItemKind: the BaseItemKind named by
// GetClientTypeName, which PlaylistsFolder overrides as ManualPlaylistsFolder.
func dtoItemKind(kind item.Kind) dto.Kind {
	if kind == item.KindPlaylistsFolder {
		return dto.Kind(item.KindManualPlaylistsFolder.String())
	}
	return dto.Kind(kind.String())
}

func primaryImageAspectRatio(images []item.ImageInfo) *float64 {
	for _, image := range images {
		if image.Type != item.ImageTypePrimary || image.Index != 0 || image.Width <= 0 || image.Height <= 0 {
			continue
		}
		ratio := float64(image.Width) / float64(image.Height)
		return &ratio
	}
	return nil
}

func dtoExtraType(value *item.ExtraType) *dto.ExtraType {
	if value == nil {
		return nil
	}
	result := dto.ExtraType(value.String())
	return &result
}

func dtoVideo3DFormat(value *item.Video3DFormat) *dto.Video3DFormat {
	if value == nil {
		return nil
	}
	result := dto.Video3DFormat(value.String())
	return &result
}

func dtoVideoType(value *item.VideoType) *dto.VideoType {
	if value == nil {
		return nil
	}
	result := dto.VideoType(value.String())
	return &result
}

func dtoIsoType(value *item.IsoType) *dto.IsoType {
	if value == nil {
		return nil
	}
	result := dto.IsoType(value.String())
	return &result
}

func dtoProviderIDsOrEmpty(values item.ProviderIDs) map[string]string {
	entries := values.Snapshot()
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		result[entry.Name] = entry.Value
	}
	return result
}

func dtoPeopleOrEmpty(values []item.PersonCredit) []dto.BaseItemPerson {
	return dtoPeople(values, nil)
}

// dtoPeople projects credits as DtoService.AttachPeople does: each person
// carries the tag and BlurHash of its Person entity's Primary image.
func dtoPeople(values []item.PersonCredit, primaryImages map[dto.UUID]item.ImageInfo) []dto.BaseItemPerson {
	result := make([]dto.BaseItemPerson, len(values))
	for index, value := range values {
		result[index] = dto.BaseItemPerson{ID: value.PersonID, Name: value.Name, Role: dereference(value.Role), Type: value.Type.String()}
		image, found := primaryImages[value.PersonID]
		if !found {
			continue
		}
		tag := filepath.Base(image.Path)
		if tag == "" || tag == "." {
			continue
		}
		result[index].PrimaryImageTag = &tag
		if image.BlurHash != nil {
			result[index].ImageBlurHashes = map[string]map[string]string{item.ImageTypePrimary.String(): {tag: *image.BlurHash}}
		}
	}
	return result
}

func stringsOrEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func dtoImageOrientation(value *item.ImageOrientation) *dto.ImageOrientation {
	if value == nil {
		return nil
	}
	mapped := dto.ImageOrientation(value.String())
	return &mapped
}

func projectImages(result *dto.BaseItemDto, enrichment Enrichment, options store.ItemQueryDTOOptions) {
	imageTypes := options.ImageTypes
	if len(imageTypes) == 0 {
		imageTypes = allImageTypes()
	}
	result.ImageTags = make(map[string]string)
	allowed := make(map[string]struct{}, len(imageTypes))
	for _, imageType := range imageTypes {
		allowed[imageType.String()] = struct{}{}
	}
	limit := options.ImageTypeLimit
	if limit <= 0 {
		return
	}
	selected := make(map[string][]string, len(allowed))
	if _, ok := allowed["Backdrop"]; ok {
		selected["Backdrop"] = []string{}
		for _, tag := range enrichment.BackdropImageTags {
			if len(selected["Backdrop"]) == limit {
				break
			}
			selected["Backdrop"] = append(selected["Backdrop"], tag)
		}
		result.BackdropImageTags = selected["Backdrop"]
	}
	for imageType, tag := range enrichment.ImageTags {
		if _, ok := allowed[imageType]; ok && len(selected[imageType]) < limit {
			selected[imageType] = append(selected[imageType], tag)
		}
	}
	for imageType, tag := range enrichment.InheritedImageTags {
		if _, ok := allowed[imageType]; ok && len(selected[imageType]) == 0 {
			selected[imageType] = append(selected[imageType], tag)
		}
	}
	for imageType, tags := range selected {
		if imageType == "Backdrop" {
			continue
		}
		if len(tags) != 0 {
			result.ImageTags[imageType] = tags[0]
		}
		for _, tag := range tags {
			if hash, ok := enrichment.ImageBlurHashes[imageType][tag]; ok {
				if result.ImageBlurHashes[imageType] == nil {
					result.ImageBlurHashes[imageType] = make(map[string]string)
				}
				result.ImageBlurHashes[imageType][tag] = hash
			}
		}
	}
	for _, tag := range selected["Backdrop"] {
		if hash, ok := enrichment.ImageBlurHashes["Backdrop"][tag]; ok {
			if result.ImageBlurHashes["Backdrop"] == nil {
				result.ImageBlurHashes["Backdrop"] = make(map[string]string)
			}
			result.ImageBlurHashes["Backdrop"][tag] = hash
		}
	}
}

func allImageTypes() []item.ImageType {
	return []item.ImageType{item.ImageTypePrimary, item.ImageTypeArt, item.ImageTypeBackdrop, item.ImageTypeBanner, item.ImageTypeLogo, item.ImageTypeThumb, item.ImageTypeDisc, item.ImageTypeBox, item.ImageTypeScreenshot, item.ImageTypeMenu, item.ImageTypeChapter, item.ImageTypeBoxRear, item.ImageTypeProfile}
}

func hasField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
