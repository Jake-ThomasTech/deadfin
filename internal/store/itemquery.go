// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

// ItemSortBy identifies an upstream item ordering.
//
// Its string identities and ItemSortByValues order are pinned to
// Jellyfin.Data.Enums.ItemSortBy. Unknown values are intentionally preserved
// as strings and rejected by the SQL translator until an implementation owns
// them.
type ItemSortBy string

const (
	ItemSortByDefault              ItemSortBy = "Default"
	ItemSortByAiredEpisodeOrder    ItemSortBy = "AiredEpisodeOrder"
	ItemSortByAlbum                ItemSortBy = "Album"
	ItemSortByAlbumArtist          ItemSortBy = "AlbumArtist"
	ItemSortByArtist               ItemSortBy = "Artist"
	ItemSortByDateCreated          ItemSortBy = "DateCreated"
	ItemSortByOfficialRating       ItemSortBy = "OfficialRating"
	ItemSortByDatePlayed           ItemSortBy = "DatePlayed"
	ItemSortByPremiereDate         ItemSortBy = "PremiereDate"
	ItemSortByStartDate            ItemSortBy = "StartDate"
	ItemSortBySortName             ItemSortBy = "SortName"
	ItemSortByName                 ItemSortBy = "Name"
	ItemSortByRandom               ItemSortBy = "Random"
	ItemSortByRuntime              ItemSortBy = "Runtime"
	ItemSortByCommunityRating      ItemSortBy = "CommunityRating"
	ItemSortByProductionYear       ItemSortBy = "ProductionYear"
	ItemSortByPlayCount            ItemSortBy = "PlayCount"
	ItemSortByCriticRating         ItemSortBy = "CriticRating"
	ItemSortByIsFolder             ItemSortBy = "IsFolder"
	ItemSortByIsUnplayed           ItemSortBy = "IsUnplayed"
	ItemSortByIsPlayed             ItemSortBy = "IsPlayed"
	ItemSortBySeriesSortName       ItemSortBy = "SeriesSortName"
	ItemSortByVideoBitRate         ItemSortBy = "VideoBitRate"
	ItemSortByAirTime              ItemSortBy = "AirTime"
	ItemSortByStudio               ItemSortBy = "Studio"
	ItemSortByIsFavoriteOrLiked    ItemSortBy = "IsFavoriteOrLiked"
	ItemSortByDateLastContentAdded ItemSortBy = "DateLastContentAdded"
	ItemSortBySeriesDatePlayed     ItemSortBy = "SeriesDatePlayed"
	ItemSortByParentIndexNumber    ItemSortBy = "ParentIndexNumber"
	ItemSortByIndexNumber          ItemSortBy = "IndexNumber"
)

var itemSortByValues = [...]ItemSortBy{
	ItemSortByDefault, ItemSortByAiredEpisodeOrder, ItemSortByAlbum, ItemSortByAlbumArtist,
	ItemSortByArtist, ItemSortByDateCreated, ItemSortByOfficialRating, ItemSortByDatePlayed,
	ItemSortByPremiereDate, ItemSortByStartDate, ItemSortBySortName, ItemSortByName,
	ItemSortByRandom, ItemSortByRuntime, ItemSortByCommunityRating, ItemSortByProductionYear,
	ItemSortByPlayCount, ItemSortByCriticRating, ItemSortByIsFolder, ItemSortByIsUnplayed,
	ItemSortByIsPlayed, ItemSortBySeriesSortName, ItemSortByVideoBitRate, ItemSortByAirTime,
	ItemSortByStudio, ItemSortByIsFavoriteOrLiked, ItemSortByDateLastContentAdded,
	ItemSortBySeriesDatePlayed, ItemSortByParentIndexNumber, ItemSortByIndexNumber,
}

// ItemSortByValues returns the complete pinned sort vocabulary in upstream enum
// order. The returned slice may be modified without changing the contract.
func ItemSortByValues() []ItemSortBy {
	return append([]ItemSortBy(nil), itemSortByValues[:]...)
}

// UnmarshalText binds a case-insensitive upstream item sort value.
func (s *ItemSortBy) UnmarshalText(text []byte) error {
	for _, candidate := range itemSortByValues {
		if strings.EqualFold(string(text), string(candidate)) {
			*s = candidate
			return nil
		}
	}
	return fmt.Errorf("invalid item sort %q", text)
}

// SortOrder controls the direction for the corresponding ItemQuery.SortBy entry.
type SortOrder string

const (
	// SortOrderAscending orders values from low to high.
	SortOrderAscending SortOrder = "Ascending"
	// SortOrderDescending orders values from high to low.
	SortOrderDescending SortOrder = "Descending"
)

// UnmarshalText binds a case-insensitive upstream sort direction.
func (s *SortOrder) UnmarshalText(text []byte) error {
	switch {
	case strings.EqualFold(string(text), string(SortOrderAscending)):
		*s = SortOrderAscending
	case strings.EqualFold(string(text), string(SortOrderDescending)):
		*s = SortOrderDescending
	default:
		return fmt.Errorf("invalid item sort order %q", text)
	}
	return nil
}

// ItemOrder is one upstream OrderBy pair.
type ItemOrder struct {
	SortBy    ItemSortBy
	SortOrder SortOrder
}

// ItemQueryParentAndIndexNumber is the compound cursor used by
// MinParentAndIndexNumber.
type ItemQueryParentAndIndexNumber struct {
	ParentIndexNumber int
	IndexNumber       int
}

// ParentalRatingScore is the nullable score/subscore pair used for parental
// rating bounds. Source: MediaBrowser.Model.Entities.ParentalRatingScore.
type ParentalRatingScore struct {
	Score    int
	SubScore *int
}

// ItemQueryDTOOptions selects materialized item data. Its fields are owned by
// the item materialization layer rather than Translate.
type ItemQueryDTOOptions struct {
	Fields                  []string
	ImageTypes              []item.ImageType
	ImageTypeLimit          int
	EnableImages            bool
	AddProgramRecordingInfo bool
	EnableUserData          bool
	AddCurrentProgram       bool
}

// ItemQuery is the complete pinned InternalItemsQuery input contract.
//
// The ItemQueryProperties classification tells callers whether an input is
// translated, service-owned, materialization-owned, or intentionally dropped;
// field presence alone does not promise SQL support. An empty ItemIDs slice
// imposes no ID filter. When SortBy is empty, results use SortName ascending.
// Every translated result order ends with id ascending so pages remain stable
// when requested sort values tie.
type ItemQuery struct {
	Recursive                              bool
	StartIndex                             *int
	Limit                                  *int
	User                                   *User
	IsFolder                               *bool
	IsFavorite                             *bool
	IsFavoriteOrLiked                      *bool
	IsLiked                                *bool
	IsPlayed                               *bool
	IsResumable                            *bool
	IncludeItemsByName                     *bool
	MediaTypes                             []item.MediaType
	IncludeItemTypes                       []item.Kind
	ExcludeItemTypes                       []item.Kind
	ExcludeTags                            []string
	ExcludeInheritedTags                   []string
	IncludeInheritedTags                   []string
	Genres                                 []string
	IsSpecialSeason                        *bool
	IsMissing                              *bool
	IsUnaired                              *bool
	CollapseBoxSetItems                    *bool
	CollapseBoxSetItemTypes                []item.Kind
	NameStartsWithOrGreater                string
	NameStartsWith                         string
	NameLessThan                           string
	NameContains                           string
	PresentationUniqueKey                  string
	Path                                   string
	Name                                   string
	UseRawName                             *bool
	Person                                 string
	PersonIDs                              []dto.UUID
	ItemIDs                                []dto.UUID
	OwnerIDs                               []dto.UUID
	ExtraTypes                             []item.ExtraType
	ExcludeItemIDs                         []dto.UUID
	AdjacentTo                             *dto.UUID
	PersonTypes                            []string
	Studios                                []string
	Is3D                                   *bool
	IsHD                                   *bool
	IsLocked                               *bool
	IsPlaceHolder                          *bool
	HasIMDbID                              *bool
	HasOverview                            *bool
	HasTMDbID                              *bool
	HasOfficialRating                      *bool
	HasTVDbID                              *bool
	HasThemeSong                           *bool
	HasThemeVideo                          *bool
	HasSubtitles                           *bool
	HasSpecialFeature                      *bool
	HasTrailer                             *bool
	HasParentalRating                      *bool
	StudioIDs                              []dto.UUID
	GenreIDs                               []dto.UUID
	ImageTypes                             []item.ImageType
	VideoTypes                             []item.VideoType
	BlockUnratedItems                      []item.UnratedItem
	Years                                  []int
	Tags                                   []string
	OfficialRatings                        []string
	MinPremiereDate                        *time.Time
	MaxPremiereDate                        *time.Time
	MinStartDate                           *time.Time
	MaxStartDate                           *time.Time
	MinEndDate                             *time.Time
	MaxEndDate                             *time.Time
	IsAiring                               *bool
	IsMovie                                *bool
	IsSports                               *bool
	IsKids                                 *bool
	IsNews                                 *bool
	IsSeries                               *bool
	MinIndexNumber                         *int
	MinParentAndIndexNumber                *ItemQueryParentAndIndexNumber
	AiredDuringSeason                      *int
	MinCriticRating                        *float64
	MinCommunityRating                     *float32
	MaxCommunityRating                     *float32
	MinRunTimeTicks                        *int64
	MaxRunTimeTicks                        *int64
	ChannelIDs                             []dto.UUID
	ParentIndexNumber                      *int
	ParentIndexNumberNotEquals             *int
	IndexNumber                            *int
	MinParentalRating                      *ParentalRatingScore
	MaxParentalRating                      *ParentalRatingScore
	HasDeadParentID                        *bool
	IsVirtualItem                          *bool
	ParentID                               *dto.UUID
	ParentType                             *item.Kind
	AncestorIDs                            []dto.UUID
	LinkedChildAncestorIDs                 []dto.UUID
	TopParentIDs                           []dto.UUID
	PresetViews                            []*item.CollectionType
	TrailerTypes                           []item.TrailerType
	SourceTypes                            []item.SourceType
	SeriesStatuses                         []item.SeriesStatus
	ExternalSeriesID                       string
	ExternalID                             string
	AlbumIDs                               []dto.UUID
	ArtistIDs                              []dto.UUID
	ExcludeArtistIDs                       []dto.UUID
	AncestorWithPresentationUniqueKey      string
	SeriesPresentationUniqueKey            string
	GroupByPresentationUniqueKey           bool
	GroupBySeriesPresentationUniqueKey     bool
	EnableTotalRecordCount                 bool
	ForceDirect                            bool
	ExcludeProviderIDs                     map[string]string
	EnableGroupByMetadataKey               bool
	HasChapterImages                       *bool
	OrderBy                                []ItemOrder
	MinDateCreated                         *time.Time
	MinDateLastSaved                       *time.Time
	MinDateLastSavedForUser                *time.Time
	DtoOptions                             ItemQueryDTOOptions
	HasNoAudioTrackWithLanguage            string
	HasNoInternalSubtitleTrackWithLanguage string
	HasNoExternalSubtitleTrackWithLanguage string
	HasNoSubtitleTrackWithLanguage         string
	IsDeadArtist                           *bool
	IsDeadStudio                           *bool
	IsDeadGenre                            *bool
	IsDeadPerson                           *bool
	DisplayAlbumFolders                    *bool
	HasAnyProviderID                       map[string]string
	HasAnyProviderIDs                      map[string][]string
	AlbumArtistIDs                         []dto.UUID
	BoxSetLibraryFolders                   []dto.UUID
	ContributingArtistIDs                  []dto.UUID
	HasAired                               *bool
	HasOwnerID                             *bool
	IncludeOwnedItems                      bool
	Is4K                                   *bool
	MaxHeight                              *int
	MaxWidth                               *int
	MinHeight                              *int
	MinWidth                               *int
	SearchTerm                             string
	SeriesTimerID                          string
	SkipDeserialization                    bool
	IncludeExtras                          bool
	AudioLanguages                         []string
	SubtitleLanguages                      []string

	// Deadfin-specific query inputs preserve existing behavior. ReferenceTime
	// makes IsUnaired deterministic; UserID supplies the current translator's
	// user identity; SortBy and SortOrder retain the established API.
	ReferenceTime                    *time.Time
	UserID                           *dto.UUID
	DisableLiveTVChannelUserDataName bool
	SortBy                           []ItemSortBy
	SortOrder                        []SortOrder
	// EpisodesOfSeasonID scopes the query to the episodes of this Season as
	// upstream Season.GetEpisodes selects them (Series.GetSeasonEpisodes,
	// Series.FilterEpisodesBySeason): the series' episodes linked to the
	// season or to a season with the same number, and the episodes whose
	// season number is the season's IndexNumber. A root-folder episode is
	// not a child of a season folder, so a parent scope misses it.
	// ItemStore.Query sets it for a user's ParentId query of a Season
	// (Season.GetItemsInternal).
	EpisodesOfSeasonID *dto.UUID
	// ExcludeSpecialsFromSeasons is the inverse of the server option
	// DisplaySpecialsWithinSeasons (default true). While it is false, a
	// special with AirsBefore/AirsAfterSeasonNumber n is also an episode of
	// season n in an EpisodesOfSeasonID scope.
	ExcludeSpecialsFromSeasons bool
	// flattenRootFolderIDs is set by ItemStore.Query when the query targets a
	// CollectionFolder whose Extra records physical root Folder IDs. The
	// non-recursive parent filter flattens those roots so a library browse
	// returns typed media items, never the intermediate root Folders, matching
	// upstream CollectionFolder.GetActualChildren (jellyfin@b04614e :349-351).
	flattenRootFolderIDs []dto.UUID
	// linkedOrderParentID orders the page by the entry order of this
	// Playlist's linked children. ItemStore.Query sets it for a direct
	// Playlist browse without an explicit sort.
	linkedOrderParentID *dto.UUID
	// recursiveParent holds the stored facts of ParentID. ItemStore.Query
	// sets it after it reads the parent row. With the facts known, a
	// recursive parent scope becomes an indexed predicate on the base
	// relation, so the query reads only the parent's subtree and never scans
	// the whole item table.
	recursiveParent *recursiveParentFacts
	// latestWindow narrows a top-parent scope to one newest-first page of
	// candidates. ItemStore.LatestItems sets it to walk a library from its
	// newest items and stop at its limit, instead of grouping every item.
	latestWindow *latestWindow
}

// recursiveParentFacts are the parent facts a recursive scope needs.
type recursiveParentFacts struct {
	// kind is empty when the parent does not exist.
	kind string
	// topParent is true when the parent is the top parent of each of its
	// descendants.
	topParent bool
}

// ItemQueryResult is an item page and its requested total. If total counting
// is disabled, TotalRecordCount is the number of items in Items.
type ItemQueryResult struct {
	Items            []item.Item
	TotalRecordCount int
}

// TranslatedItemQuery contains the executable page and pre-pagination count
// statements for an ItemQuery. Arguments use the supplied dialect's values and
// placeholder ordering.
type TranslatedItemQuery struct {
	SQL       string
	Args      []any
	CountSQL  string
	CountArgs []any
	// IDSQL selects the filtered item ids, unordered and unpaged. It takes
	// CountArgs.
	IDSQL string
	// FilteredFrom and FilteredWhere are the FROM source and WHERE clause
	// ("" or " WHERE ...") of the filtered relation, when no transform
	// follows the base filters. "SELECT <items columns> FROM " + FilteredFrom
	// + FilteredWhere then selects the filtered rows without a second lookup
	// by id. Both are empty when a transform applies; use IDSQL then. They
	// take CountArgs.
	FilteredFrom, FilteredWhere string
}

// Translate turns an ItemQuery into portable SQL for d. Both statements apply
// the same filters; CountSQL intentionally has no ordering or pagination.
func Translate(query ItemQuery, d Dialect) (TranslatedItemQuery, error) {
	if d == nil {
		return TranslatedItemQuery{}, fmt.Errorf("translate item query: dialect is required")
	}
	if query.StartIndex != nil && *query.StartIndex < 0 {
		return TranslatedItemQuery{}, fmt.Errorf("translate item query: start index must not be negative")
	}
	if query.Limit != nil && *query.Limit < 0 {
		return TranslatedItemQuery{}, fmt.Errorf("translate item query: limit must not be negative")
	}

	filterQuery := query
	structuralQuery := query
	if len(query.ItemIDs) != 0 {
		if query.SearchTerm != "" {
			// Source search candidates and explicit IDs are combined after the
			// ordinary structural transforms. The explicit branch is access-checked
			// by applyExplicitItemIDs rather than narrowed by search predicates.
			filterQuery.ItemIDs = nil
		} else {
			// Source ItemIds take precedence over an incompatible parent scope.
			filterQuery.ParentID = nil
			structuralQuery.ParentID = nil
		}
	}
	if filterQuery.Recursive && filterQuery.ParentID != nil && !filterQuery.ParentID.IsNil() {
		// LibraryManager replaces a recursive ParentId scope with an indexed
		// ancestor/top-parent/linked-child scope before repository filtering.
		filterQuery.ParentID = nil
		if len(filterQuery.flattenRootFolderIDs) != 0 {
			filterQuery.ExcludeItemIDs = append(filterQuery.ExcludeItemIDs, filterQuery.flattenRootFolderIDs...)
		}
	}
	if filterQuery.CollapseBoxSetItems != nil && *filterQuery.CollapseBoxSetItems {
		// BoxSet replacement changes the representative item; its name ranges
		// therefore run after collapsing over the replacement's own SortName.
		filterQuery.NameStartsWith = ""
		filterQuery.NameStartsWithOrGreater = ""
		filterQuery.NameLessThan = ""
	}
	where, filterArgs, err := translateItemQueryFilters(filterQuery, d)
	if err != nil {
		return TranslatedItemQuery{}, err
	}
	if itemQueryHidesDefaultInventoryItems(query) {
		if where == "" {
			where = " WHERE " + hiddenDefaultInventoryItemsClause
		} else {
			where += " AND " + hiddenDefaultInventoryItemsClause
		}
	} else if query.Recursive && query.ParentID != nil && !query.ParentID.IsNil() {
		// Items without ancestor support (people, studios, genres, ...) never
		// live under a parent, so the recursive parent scope cannot match
		// them. Excluding them up front keeps the per-candidate scope check
		// off the ~33k people and studio rows (Latest: ~200 ms per library).
		if where == "" {
			where = " WHERE " + withoutAncestorKindsClause
		} else {
			where += " AND " + withoutAncestorKindsClause
		}
	}
	source := "items"
	if !strings.Contains(where, resumeWinnerCTEReference) {
		source, where, filterArgs = applyRecursiveParentScope(structuralQuery, d, where, filterArgs)
	} else {
		where, filterArgs = applyResumeParentScope(structuralQuery, d, where, filterArgs)
	}
	relation, err := translateItemQueryIDRelation(structuralQuery, d, source, where, filterArgs)
	if err != nil {
		return TranslatedItemQuery{}, err
	}
	withSQL, selectedIDs, transformedArgs := relation.withSQL(), relation.current, relation.args
	filteredFrom, filteredWhere := "", ""
	if len(relation.ctes) == 1 && selectedIDs == "filtered_ids" && !strings.Contains(where, resumeWinnerCTEReference) {
		filteredFrom, filteredWhere = source, where
	}
	// directWhere selects the page straight from items when the relation is
	// the base filters, optionally narrowed by visibility. The planner then
	// sees every predicate on the ordered table and can walk an index in the
	// requested order and stop at the limit. Behind "id IN (SELECT id FROM
	// <cte>)" it sorts every item first (Live TV guide: 3.7 s to 0.6 s).
	directWhere, direct := "", false
	if source == "items" && !strings.Contains(where, resumeWinnerCTEReference) {
		switch {
		case len(relation.ctes) == 1 && selectedIDs == "filtered_ids":
			directWhere, direct = where, true
		case len(relation.ctes) == 2 && selectedIDs == "visible_ids" && relation.access != "":
			if where == "" {
				directWhere = " WHERE " + relation.access
			} else {
				directWhere = where + " AND (" + relation.access + ")"
			}
			direct = true
		}
	}

	// Inject the bounded Resume progress CTEs when the resumable filter set
	// the sentinel clause. The CTEs go before filtered_ids so the filtered
	// relation can join against resume_winner_ids by primary key instead of
	// scanning all items through an IN subquery.
	if strings.Contains(where, resumeWinnerCTEReference) && query.UserID != nil {
		userArg := uuidArgDialect(d, *query.UserID)
		if d.Placeholder(1) == "?" {
			// SQLite: positional placeholders. The CTE appears first in the
			// WITH clause, so its ? binds the first args. Prepend the two
			// user_id args so the existing ?s bind the remaining args.
			cteSQL := resumeProgressCTEs(d, *query.UserID, 1)
			withSQL = "WITH " + cteSQL + ", " + withSQL[len("WITH "):]
			transformedArgs = append([]any{userArg, userArg}, transformedArgs...)
		} else {
			// PostgreSQL: numbered placeholders. The CTE uses high-numbered
			// placeholders that refer to args appended at the end.
			base := len(transformedArgs) + 1
			cteSQL := resumeProgressCTEs(d, *query.UserID, base)
			withSQL = "WITH " + cteSQL + ", " + withSQL[len("WITH "):]
			transformedArgs = append(transformedArgs, userArg, userArg)
		}
		// Replace the sentinel with a join so filtered_ids uses an indexed
		// join against resume_winner_ids instead of an IN subquery that
		// scans all items.
		withSQL = strings.Replace(withSQL, resumeWinnerCTEReference, "1 = 1", 1)
		// Add the join to the filtered_ids CTE. The first CTE is always
		// filtered_ids; change its SELECT to join resume_winner_ids.
		withSQL = strings.Replace(withSQL, "filtered_ids AS (SELECT id FROM items", "filtered_ids AS (SELECT items.id FROM items JOIN resume_winner_ids ON resume_winner_ids.item_id = items.id", 1)
	}

	countSQL := withSQL + " SELECT COUNT(*) FROM " + selectedIDs
	idSQL := withSQL + " SELECT id FROM " + selectedIDs
	args := append([]any(nil), transformedArgs...)

	orderBy, orderArgs, err := translateItemQueryOrderWithArgs(query, d, len(args))
	if err != nil {
		return TranslatedItemQuery{}, err
	}
	args = append(args, orderArgs...)
	pageSQL := withSQL + " SELECT " + strings.Join(itemColumns, ", ") + " FROM items WHERE items.id IN (SELECT id FROM " + selectedIDs + ") ORDER BY " + orderBy
	if direct {
		pageSQL = "SELECT " + strings.Join(itemColumns, ", ") + " FROM items" + directWhere + " ORDER BY " + orderBy
	}

	if query.Limit != nil {
		pageSQL += " LIMIT " + d.Placeholder(len(args)+1)
		args = append(args, *query.Limit)
	}
	if query.StartIndex != nil {
		if query.Limit == nil {
			pageSQL += " LIMIT " + unlimitedLimit(d)
		}
		pageSQL += " OFFSET " + d.Placeholder(len(args)+1)
		args = append(args, *query.StartIndex)
	}

	return TranslatedItemQuery{
		SQL:           pageSQL,
		Args:          args,
		CountSQL:      countSQL,
		CountArgs:     transformedArgs,
		IDSQL:         idSQL,
		FilteredFrom:  filteredFrom,
		FilteredWhere: filteredWhere,
	}, nil
}

// applyRecursiveParentScope makes the parent's subtree drive a recursive
// ParentId query when ItemStore resolved the parent facts. It returns the
// FROM source and WHERE clause of filtered_ids with their arguments. The
// subtree selects the same rows as applyItemQueryRecursiveParent:
//
//   - CollectionFolder and UserView: the top parent or any ancestor is the
//     parent. When the parent is a top parent, every descendant has it as
//     top_parent_id (derivedHierarchy), so top_parent_id alone selects the
//     subtree.
//   - Playlist and BoxSet: the item is a linked child of the parent.
//   - Other kinds: any ancestor is the parent.
//   - Missing parent: no rows.
//
// Each subtree reads an index keyed by the parent ID (ix_items_kind_top,
// ix_items_top_parent, the ancestors and linked_children primary keys).
// "subtree CROSS JOIN items" keeps the subtree as the outer loop: SQLite does
// not reorder a CROSS JOIN, and without statistics it otherwise drives the
// query from primary_version_id IS NULL, which matches almost every item. A
// type-filtered top-parent scope stays a plain predicate, because
// ix_items_kind_top then matches both the kind and the top parent. The generic
// relation instead filtered every item and checked the scope per candidate,
// so a page of a one-item library cost a scan of the whole library.
//
// A resumable query keeps the generic relation: its rewrite joins
// resume_winner_ids into "SELECT id FROM items".
func applyRecursiveParentScope(query ItemQuery, d Dialect, where string, args []any) (string, string, []any) {
	if !query.Recursive || query.ParentID == nil || query.ParentID.IsNil() || query.recursiveParent == nil {
		return "items", where, args
	}
	and := func(clause string) string {
		if where == "" {
			return " WHERE " + clause
		}
		return where + " AND " + clause
	}
	parent := uuidArgDialect(d, *query.ParentID)
	var subtree string
	var subtreeArgs []any
	switch query.recursiveParent.kind {
	case "":
		return "items", and("1 = 0"), args
	case item.KindCollectionFolder.String(), item.KindUserView.String():
		switch {
		case query.recursiveParent.topParent && query.latestWindow.appliesTo(query):
			subtree, subtreeArgs = query.latestWindow.subtree(d, query.IsFolder)
		case query.recursiveParent.topParent && len(query.IncludeItemTypes) != 0:
			return "items", and("top_parent_id = " + d.Placeholder(len(args)+1)), append(args, parent)
		case query.recursiveParent.topParent:
			subtree, subtreeArgs = "SELECT id AS scope_item_id FROM items WHERE top_parent_id = %s", []any{parent}
		default:
			subtree, subtreeArgs = "SELECT item_id AS scope_item_id FROM ancestors WHERE ancestor_id = %s UNION SELECT id FROM items WHERE top_parent_id = %s", []any{parent, parent}
		}
	case item.KindPlaylist.String(), item.KindBoxSet.String():
		subtree, subtreeArgs = "SELECT child_item_id AS scope_item_id FROM linked_children WHERE parent_item_id = %s", []any{parent}
	default:
		subtree, subtreeArgs = "SELECT item_id AS scope_item_id FROM ancestors WHERE ancestor_id = %s", []any{parent}
	}
	marks := make([]any, len(subtreeArgs))
	for index := range marks {
		if d.Placeholder(1) == "?" {
			// SQLite binds "?" by position, and the subtree comes first.
			marks[index] = "?"
		} else {
			marks[index] = d.Placeholder(len(args) + index + 1)
		}
	}
	if d.Placeholder(1) == "?" {
		args = append(subtreeArgs, args...)
	} else {
		args = append(args, subtreeArgs...)
	}
	source := "(" + fmt.Sprintf(subtree, marks...) + ") AS parent_scope CROSS JOIN items"
	join := "items.id = parent_scope.scope_item_id"
	if where == "" {
		return source, " WHERE " + join, args
	}
	return source, " WHERE " + join + " AND " + strings.TrimPrefix(where, " WHERE "), args
}

// applyResumeParentScope is the recursive ParentId scope of a resumable
// query. The Resume rewrite needs "SELECT id FROM items" as the filtered_ids
// source, so applyRecursiveParentScope cannot drive the relation from the
// subtree. The subtree is a predicate instead; resume_winner_ids already
// bounds the candidates to the user's in-progress items. The subtree selects
// the same rows as applyRecursiveParentScope (GetResumeItems passes ParentId
// with Recursive = true, ItemsController.cs:984-985).
func applyResumeParentScope(query ItemQuery, d Dialect, where string, args []any) (string, []any) {
	if !query.Recursive || query.ParentID == nil || query.ParentID.IsNil() || query.recursiveParent == nil {
		return where, args
	}
	parent := uuidArgDialect(d, *query.ParentID)
	mark := func() string {
		args = append(args, parent)
		return d.Placeholder(len(args))
	}
	var clause string
	switch query.recursiveParent.kind {
	case "":
		clause = "1 = 0"
	case item.KindCollectionFolder.String(), item.KindUserView.String():
		if query.recursiveParent.topParent {
			clause = "items.top_parent_id = " + mark()
		} else {
			clause = "(items.top_parent_id = " + mark() + " OR items.id IN (SELECT item_id FROM ancestors WHERE ancestor_id = " + mark() + "))"
		}
	case item.KindPlaylist.String(), item.KindBoxSet.String():
		clause = "items.id IN (SELECT child_item_id FROM linked_children WHERE parent_item_id = " + mark() + ")"
	default:
		clause = "items.id IN (SELECT item_id FROM ancestors WHERE ancestor_id = " + mark() + ")"
	}
	if where == "" {
		return " WHERE " + clause, args
	}
	return where + " AND " + clause, args
}

// hiddenDefaultInventoryItemsClause excludes implementation roots and every
// kind whose SupportsAncestors capability is false from the unscoped
// recursive public inventory. Stock excludes these entities because items
// without ancestor support never live under a library root: the TopParentIds
// filter that a scoped recursive query applies never matches them. The
// implementation roots (AggregateFolder, CollectionFolder, PlaylistsFolder,
// UserRootFolder) carry ancestor support but are structural containers, not
// media items, so they stay hidden from the default listing. MusicArtist
// inherits ancestor support and is not excluded. The kind set is derived from
// item.KindsWithoutAncestors, which reads Definition, so the rule stays in
// sync with the capability table.
var hiddenDefaultInventoryItemsClause = buildHiddenDefaultInventoryItemsClause()

// withoutAncestorKindsClause excludes every kind whose SupportsAncestors
// capability is false.
var withoutAncestorKindsClause = buildWithoutAncestorKindsClause()

func buildWithoutAncestorKindsClause() string {
	kinds := make([]string, 0, 8)
	for _, k := range item.KindsWithoutAncestors() {
		kinds = append(kinds, "'"+k.String()+"'")
	}
	return "kind NOT IN (" + strings.Join(kinds, ", ") + ")"
}

func buildHiddenDefaultInventoryItemsClause() string {
	kinds := make([]string, 0, 12)
	for _, k := range []item.Kind{item.KindAggregateFolder, item.KindCollectionFolder, item.KindPlaylistsFolder, item.KindUserRootFolder} {
		kinds = append(kinds, "'"+k.String()+"'")
	}
	for _, k := range item.KindsWithoutAncestors() {
		kinds = append(kinds, "'"+k.String()+"'")
	}
	return "kind NOT IN (" + strings.Join(kinds, ", ") + ")"
}

// itemQueryHidesDefaultInventoryItems hides implementation roots and
// SupportsAncestors-false entities from the unscoped recursive public
// inventory. Type-scoped, parent-scoped, and explicit ID queries retain
// access when required by their dedicated API semantics.
func itemQueryHidesDefaultInventoryItems(query ItemQuery) bool {
	return query.Recursive &&
		(query.ParentID == nil || query.ParentID.IsNil()) &&
		len(query.ItemIDs) == 0 &&
		len(query.IncludeItemTypes) == 0
}

func directionSQL(order SortOrder) string {
	if order == SortOrderDescending {
		return "DESC"
	}
	return "ASC"
}

func unlimitedLimit(d Dialect) string {
	if d.Placeholder(1) == "?" {
		return "-1"
	}
	return "ALL"
}
