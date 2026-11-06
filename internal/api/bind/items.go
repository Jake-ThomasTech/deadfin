// SPDX-License-Identifier: GPL-2.0-only

package bind

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

// ItemsQueryRequest normalizes an HTTP request query into the store contract
// and supplies the request-time fact required by the deterministic unaired
// predicate.
func ItemsQueryRequest(r *http.Request, clock clock.Clock) (store.ItemQuery, error) {
	query, err := ItemsQuery(r.URL.Query())
	if err != nil {
		return store.ItemQuery{}, err
	}
	if query.IsUnaired != nil {
		now := clock.Now().UTC()
		query.ReferenceTime = &now
	}
	return query, nil
}

// ItemsQuery normalizes public Items query parameters into the store contract.
// Route and current-user precedence belong to handlers; this function performs
// no authentication or route-specific policy.
// DefaultItemQueryDTOOptions contains the source route defaults shared by
// Items-derived endpoints. DtoOptions starts with every image type enabled and
// an unbounded per-type limit; an empty EnableImageTypes query keeps those
// defaults rather than disabling artwork. AddCurrentProgram is on, as in
// new DtoOptions().
func DefaultItemQueryDTOOptions() store.ItemQueryDTOOptions {
	return store.ItemQueryDTOOptions{
		EnableImages: true, EnableUserData: true, AddCurrentProgram: true, ImageTypeLimit: int(^uint(0) >> 1),
		ImageTypes: []item.ImageType{item.ImageTypePrimary, item.ImageTypeArt, item.ImageTypeBackdrop, item.ImageTypeBanner, item.ImageTypeLogo, item.ImageTypeThumb, item.ImageTypeDisc, item.ImageTypeBox, item.ImageTypeScreenshot, item.ImageTypeMenu, item.ImageTypeChapter, item.ImageTypeBoxRear, item.ImageTypeProfile},
	}
}

func ItemsQuery(values url.Values) (store.ItemQuery, error) {
	query := store.ItemQuery{EnableTotalRecordCount: true, DtoOptions: DefaultItemQueryDTOOptions()}
	request := itemQueryRequest{}
	if err := Query(itemQueryValues(values), &request); err != nil {
		return store.ItemQuery{}, err
	}

	query.ItemIDs = request.IDs
	query.ExcludeItemIDs = request.ExcludeItemIDs
	query.ParentID = request.ParentID
	query.AdjacentTo = request.AdjacentTo
	query.IncludeItemTypes = DelimitedEnums("IncludeItemTypes", request.IncludeItemTypes, ParseKind)
	query.ExcludeItemTypes = DelimitedEnums("ExcludeItemTypes", request.ExcludeItemTypes, ParseKind)
	query.Genres = request.Genres
	query.Tags = request.Tags
	query.Years = request.Years
	query.Person = request.Person
	query.PersonIDs = request.PersonIDs
	query.PersonTypes = request.PersonTypes
	query.GenreIDs = request.GenreIDs
	query.StudioIDs = request.StudioIDs
	query.Studios = request.Studios
	query.ArtistIDs = request.ArtistIDs
	query.ExcludeArtistIDs = request.ExcludeArtistIDs
	query.AlbumArtistIDs = request.AlbumArtistIDs
	query.ContributingArtistIDs = request.ContributingArtistIDs
	query.StartIndex = request.StartIndex
	query.Limit = request.Limit
	query.Recursive = request.Recursive
	query.IsFavorite = request.IsFavorite
	query.IsPlayed = request.IsPlayed
	query.IsMissing = request.IsMissing
	query.IsPlaceHolder = request.IsPlaceHolder
	query.IsUnaired = request.IsUnaired
	query.HasIMDbID = request.HasIMDbID
	query.HasTMDbID = request.HasTMDbID
	query.HasSubtitles = request.HasSubtitles
	query.CollapseBoxSetItems = request.CollapseBoxSetItems
	query.MinPremiereDate = request.MinPremiereDate.StdPtr()
	query.MaxPremiereDate = request.MaxPremiereDate.StdPtr()
	query.MinDateLastSavedForUser = request.MinDateLastSavedForUser.StdPtr()
	query.MinCommunityRating = request.MinCommunityRating
	query.NameStartsWith = request.NameStartsWith
	query.NameStartsWithOrGreater = request.NameStartsWithOrGreater
	query.NameLessThan = request.NameLessThan
	query.AudioLanguages = request.AudioLanguages
	query.SubtitleLanguages = request.SubtitleLanguages
	query.DtoOptions.Fields = ItemFields("Fields", request.Fields)
	if hasItemQueryKey(values, "ImageTypeLimit") {
		query.DtoOptions.ImageTypeLimit = request.ImageTypeLimit
	}
	if request.EnableUserData != nil {
		query.DtoOptions.EnableUserData = *request.EnableUserData
	}
	if request.EnableImages != nil {
		query.DtoOptions.EnableImages = *request.EnableImages
	}
	query.SearchTerm = request.SearchTerm
	if request.EnableTotalRecordCount != nil {
		query.EnableTotalRecordCount = *request.EnableTotalRecordCount
	}
	if err := applyItemQueryFilters(&query, request.Filters); err != nil {
		return store.ItemQuery{}, err
	}

	if query.StartIndex != nil && *query.StartIndex < 0 {
		return store.ItemQuery{}, fmt.Errorf("start index must not be negative")
	}
	if query.Limit != nil && *query.Limit < 0 {
		return store.ItemQuery{}, fmt.Errorf("limit must not be negative")
	}
	query.MediaTypes = DelimitedEnums("MediaTypes", request.MediaTypes, itemQueryMediaType)
	query.SortBy = DelimitedEnums("SortBy", request.SortBy, ParseText[store.ItemSortBy])
	query.SortOrder = DelimitedEnums("SortOrder", request.SortOrder, ParseText[store.SortOrder])
	query.VideoTypes = DelimitedEnums("VideoTypes", request.VideoTypes, itemQueryVideoType)
	// AddAdditionalDtoOptions keeps every image type when the parsed list is
	// empty, so a list of only unknown values keeps the default.
	if imageTypes := DelimitedEnums("EnableImageTypes", request.EnableImageTypes, ParseImageType); len(imageTypes) != 0 {
		query.DtoOptions.ImageTypes = imageTypes
	}
	return query, nil
}

type itemQueryRequest struct {
	IDs                     []dto.UUID `query:"Ids"`
	ExcludeItemIDs          []dto.UUID `query:"ExcludeItemIds"`
	ParentID                *dto.UUID  `query:"ParentId"`
	AdjacentTo              *dto.UUID  `query:"AdjacentTo"`
	IncludeItemTypes        []string   `query:"IncludeItemTypes"`
	ExcludeItemTypes        []string   `query:"ExcludeItemTypes"`
	MediaTypes              []string   `query:"MediaTypes"`
	Genres                  []string   `query:"Genres"`
	Tags                    []string   `query:"Tags"`
	Years                   []int      `query:"Years"`
	Person                  string     `query:"Person"`
	PersonIDs               []dto.UUID `query:"PersonIds"`
	PersonTypes             []string   `query:"PersonTypes"`
	GenreIDs                []dto.UUID `query:"GenreIds"`
	StudioIDs               []dto.UUID `query:"StudioIds"`
	Studios                 []string   `query:"Studios"`
	ArtistIDs               []dto.UUID `query:"ArtistIds"`
	ExcludeArtistIDs        []dto.UUID `query:"ExcludeArtistIds"`
	AlbumArtistIDs          []dto.UUID `query:"AlbumArtistIds"`
	ContributingArtistIDs   []dto.UUID `query:"ContributingArtistIds"`
	SortBy                  []string   `query:"SortBy"`
	SortOrder               []string   `query:"SortOrder"`
	StartIndex              *int       `query:"StartIndex"`
	Limit                   *int       `query:"Limit"`
	Recursive               bool       `query:"Recursive"`
	IsPlayed                *bool      `query:"IsPlayed"`
	Filters                 []string   `query:"Filters"`
	IsFavorite              *bool      `query:"IsFavorite"`
	CollapseBoxSetItems     *bool      `query:"CollapseBoxSetItems"`
	IsMissing               *bool      `query:"IsMissing"`
	IsPlaceHolder           *bool      `query:"IsPlaceHolder"`
	IsUnaired               *bool      `query:"IsUnaired"`
	MinPremiereDate         *dto.Time  `query:"MinPremiereDate"`
	MinCommunityRating      *float32   `query:"MinCommunityRating"`
	HasIMDbID               *bool      `query:"HasImdbId"`
	HasTMDbID               *bool      `query:"HasTmdbId"`
	HasSubtitles            *bool      `query:"HasSubtitles"`
	VideoTypes              []string   `query:"VideoTypes"`
	EnableTotalRecordCount  *bool      `query:"EnableTotalRecordCount"`
	EnableUserData          *bool      `query:"EnableUserData"`
	MaxPremiereDate         *dto.Time  `query:"MaxPremiereDate"`
	MinDateLastSavedForUser *dto.Time  `query:"MinDateLastSavedForUser"`
	EnableImages            *bool      `query:"EnableImages"`
	ImageTypeLimit          int        `query:"ImageTypeLimit"`
	EnableImageTypes        []string   `query:"EnableImageTypes"`
	AudioLanguages          []string   `query:"AudioLanguages"`
	SubtitleLanguages       []string   `query:"SubtitleLanguages"`
	Fields                  []string   `query:"Fields"`
	SearchTerm              string     `query:"SearchTerm"`
	NameStartsWith          string     `query:"NameStartsWith"`
	NameStartsWithOrGreater string     `query:"NameStartsWithOrGreater"`
	NameLessThan            string     `query:"NameLessThan"`
}

func hasItemQueryKey(values url.Values, want string) bool {
	for key := range values {
		if strings.EqualFold(key, want) {
			return true
		}
	}
	return false
}

func itemQueryValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for key, entries := range values {
		out[key] = make([]string, len(entries))
		for index, value := range entries {
			out[key][index] = strings.ReplaceAll(value, "|", ",")
		}
	}
	return out
}

func applyItemQueryFilters(query *store.ItemQuery, filters []string) error {
	for _, filter := range filters {
		switch {
		case strings.EqualFold(filter, "IsFolder"):
			if itemQueryHasFilter(filters, "IsNotFolder") {
				return fmt.Errorf("conflicting filters")
			}
			query.IsFolder = itemQueryBool(true)
		case strings.EqualFold(filter, "IsNotFolder"):
			if itemQueryHasFilter(filters, "IsFolder") {
				return fmt.Errorf("conflicting filters")
			}
			query.IsFolder = itemQueryBool(false)
		case strings.EqualFold(filter, "IsUnplayed"):
			if itemQueryHasFilter(filters, "IsPlayed") {
				return fmt.Errorf("conflicting filters")
			}
			query.IsPlayed = itemQueryBool(false)
		case strings.EqualFold(filter, "IsPlayed"):
			if itemQueryHasFilter(filters, "IsUnplayed") {
				return fmt.Errorf("conflicting filters")
			}
			query.IsPlayed = itemQueryBool(true)
		case strings.EqualFold(filter, "IsFavorite"):
			query.IsFavorite = itemQueryBool(true)
		case strings.EqualFold(filter, "IsResumable"):
			query.IsResumable = itemQueryBool(true)
		case strings.EqualFold(filter, "Likes"):
			if itemQueryHasFilter(filters, "Dislikes") {
				return fmt.Errorf("conflicting filters")
			}
			query.IsLiked = itemQueryBool(true)
		case strings.EqualFold(filter, "Dislikes"):
			if itemQueryHasFilter(filters, "Likes") {
				return fmt.Errorf("conflicting filters")
			}
			query.IsLiked = itemQueryBool(false)
		case strings.EqualFold(filter, "IsFavoriteOrLikes"):
			query.IsFavoriteOrLiked = itemQueryBool(true)
		default:
			dropDelimitedValue("Filters", filter)
		}
	}
	return nil
}

func itemQueryHasFilter(filters []string, want string) bool {
	for _, filter := range filters {
		if strings.EqualFold(filter, want) {
			return true
		}
	}
	return false
}

func itemQueryBool(value bool) *bool { return &value }

func itemQueryMediaType(value string) (item.MediaType, bool) {
	for _, candidate := range [...]item.MediaType{item.MediaTypeUnknown, item.MediaTypeVideo, item.MediaTypeAudio, item.MediaTypePhoto, item.MediaTypeBook} {
		if strings.EqualFold(value, candidate.String()) {
			return candidate, true
		}
	}
	return item.MediaTypeUnknown, false
}

func itemQueryVideoType(value string) (item.VideoType, bool) {
	for _, candidate := range [...]item.VideoType{item.VideoTypeVideoFile, item.VideoTypeIso, item.VideoTypeDvd, item.VideoTypeBluRay} {
		if strings.EqualFold(value, candidate.String()) {
			return candidate, true
		}
	}
	return 0, false
}
