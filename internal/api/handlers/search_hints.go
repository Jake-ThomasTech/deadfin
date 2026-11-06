// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

// ItemSearchScorer scores the items a search query matches, as
// SqlSearchProvider does. GET /Search/Hints needs it.
type ItemSearchScorer interface {
	SearchScores(ctx context.Context, query store.ItemQuery, limit int) ([]store.SearchScore, error)
}

// searchHintsDefaultLimit is SqlSearchProvider.DefaultSearchLimit and the
// SearchManager fallback when the request has no limit.
const searchHintsDefaultLimit = 100

// searchHintsMaxParentDepth bounds the parent walk for inherited thumb and
// backdrop images.
const searchHintsMaxParentDepth = 16

type searchHintsRequest struct {
	IsMovie        *bool `query:"IsMovie"`
	IsSeries       *bool `query:"IsSeries"`
	IsNews         *bool `query:"IsNews"`
	IsKids         *bool `query:"IsKids"`
	IsSports       *bool `query:"IsSports"`
	IncludePeople  *bool `query:"IncludePeople"`
	IncludeMedia   *bool `query:"IncludeMedia"`
	IncludeGenres  *bool `query:"IncludeGenres"`
	IncludeStudios *bool `query:"IncludeStudios"`
	IncludeArtists *bool `query:"IncludeArtists"`
}

// searchHints ports SearchController.GetSearchHints and
// SearchManager.GetSearchHintsAsync
// (upstream/jellyfin/Jellyfin.Api/Controllers/SearchController.cs:83-243,
// upstream/jellyfin/Emby.Server.Implementations/Library/Search/SearchManager.cs:157-247).
//
// The database provider selects at most limit (default 100) candidates with
// the Items search filter, scored exact 100, prefix 80, word prefix 75, and
// contains 50. The item query then applies the type and flag filters to the
// candidates. The hints are ordered by score, then by sort name, and the
// total counts the filtered candidates, so it never exceeds the limit.
func (h itemHandler) searchHints(w http.ResponseWriter, r *http.Request) {
	scorer, ok := h.reader.(ItemSearchScorer)
	if h.reader == nil || h.build == nil || !ok {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	scope, err := bind.ItemsQueryRequest(r, h.clock)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	var request searchHintsRequest
	if err := bind.QueryRequest(r, &request); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	// searchTerm is [Required], and the manager rejects a blank term.
	if strings.TrimSpace(scope.SearchTerm) == "" {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	requested, err := bind.QueryScalar(r, "userId")
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	var users store.ItemQuery
	if !h.applyRequestUser(w, r, requested, &users) {
		return
	}

	exclude, include := searchHintKinds(scope.IncludeItemTypes, scope.ExcludeItemTypes, request)
	limit := searchHintsDefaultLimit
	if scope.Limit != nil {
		limit = *scope.Limit
	}
	notVirtual, noCollapse := false, false
	candidates := store.ItemQuery{
		SearchTerm:          scope.SearchTerm,
		MediaTypes:          scope.MediaTypes,
		IsVirtualItem:       &notVirtual,
		IncludeOwnedItems:   true,
		CollapseBoxSetItems: &noCollapse,
		User:                users.User,
		UserID:              users.UserID,
	}
	// SearchManager.BuildProviderQuery drops excluded kinds from the
	// included kinds; the provider then filters by the included kinds when
	// any remain, and otherwise by the excluded kinds.
	if providerInclude := slices.DeleteFunc(slices.Clone(include), func(kind item.Kind) bool { return slices.Contains(exclude, kind) }); len(providerInclude) != 0 {
		candidates.IncludeItemTypes = providerInclude
	} else {
		candidates.ExcludeItemTypes = exclude
	}
	hasParent := scope.ParentID != nil && !scope.ParentID.IsNil()
	if hasParent {
		parentID := *scope.ParentID
		candidates.ParentID = &parentID
		candidates.Recursive = true
	}
	scores, err := scorer.SearchScores(r.Context(), candidates, limit)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	result := dto.SearchHintResult{SearchHints: []dto.SearchHint{}}
	if len(scores) == 0 {
		_ = render.JSON(w, r, http.StatusOK, result)
		return
	}

	byScore := make(map[dto.UUID]float32, len(scores))
	ids := make([]dto.UUID, len(scores))
	for index, score := range scores {
		byScore[score.ID] = score.Score
		ids[index] = score.ID
	}
	includeItemsByName := !hasParent
	matched, err := h.reader.Query(r.Context(), store.ItemQuery{
		ItemIDs:             ids,
		IncludeItemTypes:    include,
		ExcludeItemTypes:    exclude,
		MediaTypes:          scope.MediaTypes,
		IncludeItemsByName:  &includeItemsByName,
		Recursive:           true,
		CollapseBoxSetItems: &noCollapse,
		IsKids:              request.IsKids,
		IsMovie:             request.IsMovie,
		IsNews:              request.IsNews,
		IsSeries:            request.IsSeries,
		IsSports:            request.IsSports,
		User:                users.User,
		UserID:              users.UserID,
	})
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	// The item query returns sort-name order; the stable sort keeps it
	// within one score.
	values := matched.Items
	sort.SliceStable(values, func(i, j int) bool { return byScore[values[i].ID] > byScore[values[j].ID] })
	result.TotalRecordCount = len(values)
	if scope.StartIndex != nil {
		values = values[min(max(*scope.StartIndex, 0), len(values)):]
	}
	if scope.Limit != nil {
		values = values[:min(max(*scope.Limit, 0), len(values))]
	}
	hints, err := h.searchHintPage(r.Context(), values, users.UserID)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	result.SearchHints = hints
	_ = render.JSON(w, r, http.StatusOK, result)
}

// searchHintKinds ports SearchManager.BuildExcludeItemTypes and
// BuildIncludeItemTypes.
func searchHintKinds(includeKinds, excludeKinds []item.Kind, request searchHintsRequest) (exclude, include []item.Kind) {
	enabled := func(value *bool) bool { return value == nil || *value }
	add := func(list []item.Kind, kinds ...item.Kind) []item.Kind {
		for _, kind := range kinds {
			if !slices.Contains(list, kind) {
				list = append(list, kind)
			}
		}
		return list
	}
	exclude = append(slices.Clone(excludeKinds), item.KindYear, item.KindFolder, item.KindCollectionFolder)
	if !enabled(request.IncludeGenres) {
		exclude = add(exclude, item.KindGenre, item.KindMusicGenre)
	}
	if !enabled(request.IncludePeople) {
		exclude = add(exclude, item.KindPerson)
	}
	if !enabled(request.IncludeStudios) {
		exclude = add(exclude, item.KindStudio)
	}
	if !enabled(request.IncludeArtists) {
		exclude = add(exclude, item.KindMusicArtist)
	}

	include = slices.Clone(includeKinds)
	if enabled(request.IncludeMedia) {
		return exclude, include
	}
	emptyOrContains := func(kind item.Kind) bool { return len(include) == 0 || slices.Contains(include, kind) }
	if enabled(request.IncludeGenres) && emptyOrContains(item.KindGenre) {
		include = add(include, item.KindGenre, item.KindMusicGenre)
	}
	if enabled(request.IncludePeople) && emptyOrContains(item.KindPerson) {
		include = add(include, item.KindPerson)
	}
	if enabled(request.IncludeStudios) && emptyOrContains(item.KindStudio) {
		include = add(include, item.KindStudio)
	}
	if enabled(request.IncludeArtists) && emptyOrContains(item.KindMusicArtist) {
		include = add(include, item.KindMusicArtist)
	}
	return exclude, include
}

// searchHintImages are the image facts of one hint item or one of its
// parents.
type searchHintImages struct {
	kind     item.Kind
	parentID *dto.UUID
	thumb    string
	backdrop string
}

// searchHintPage projects one page of items to search hints
// (SearchController.GetSearchHintResult).
func (h itemHandler) searchHintPage(ctx context.Context, values []item.Item, userID *dto.UUID) ([]dto.SearchHint, error) {
	hints := make([]dto.SearchHint, 0, len(values))
	if len(values) == 0 {
		return hints, nil
	}
	options := store.ItemQueryDTOOptions{
		EnableImages:   true,
		ImageTypeLimit: 1,
		ImageTypes:     []item.ImageType{item.ImageTypePrimary, item.ImageTypeThumb, item.ImageTypeBackdrop},
		Fields:         []string{"PrimaryImageAspectRatio"},
	}
	page, err := h.build.Build(ctx, values, options, userID)
	if err != nil {
		return nil, err
	}
	built := make(map[dto.UUID]dto.BaseItemDto, len(page.Items))
	for _, value := range page.Items {
		built[value.ID] = value
	}
	images := make(map[dto.UUID]searchHintImages, len(values))
	for _, value := range values {
		images[value.ID] = searchHintImageFacts(value, built[value.ID])
	}
	if err := h.loadSearchHintParents(ctx, images, options); err != nil {
		return nil, err
	}
	channels, err := h.searchHintChannelNames(ctx, values)
	if err != nil {
		return nil, err
	}

	for _, value := range values {
		projected := built[value.ID]
		hint := dto.SearchHint{
			ItemID: value.ID, ID: value.ID, Name: projected.Name,
			IndexNumber: value.IndexNumber, ProductionYear: value.ProductionYear, ParentIndexNumber: value.ParentIndexNumber,
			Type: dto.Kind(item.ClientTypeName(value)), RunTimeTicks: value.RunTimeTicks, MediaType: projected.MediaType,
			EndDate: value.EndDate, Artists: []string{},
		}
		if value.IsFolder {
			hint.IsFolder = new(bool)
			*hint.IsFolder = true
		}
		if tag := projected.ImageTags[item.ImageTypePrimary.String()]; tag != "" {
			hint.PrimaryImageTag = &tag
			hint.PrimaryImageAspectRatio = projected.PrimaryImageAspectRatio
		}
		// Episode thumbs fall back to the series first, then to any parent.
		thumb := searchHintImageOwner(images, value.ID, func(facts searchHintImages) bool { return facts.thumb != "" }, value.Kind == item.KindEpisode)
		if thumb != nil {
			tag, owner := images[*thumb].thumb, thumb.String()
			hint.ThumbImageTag, hint.ThumbImageItemID = &tag, &owner
		}
		if backdrop := searchHintImageOwner(images, value.ID, func(facts searchHintImages) bool { return facts.backdrop != "" }, false); backdrop != nil {
			tag, owner := images[*backdrop].backdrop, backdrop.String()
			hint.BackdropImageTag, hint.BackdropImageItemID = &tag, &owner
		}
		switch value.Kind {
		case item.KindEpisode, item.KindSeason:
			// IHasSeries.
			series := projected.SeriesName
			hint.Series = &series
		case item.KindProgram:
			hint.StartDate = value.StartDate
		case item.KindSeries:
			if value.SeriesStatus != nil {
				status := value.SeriesStatus.String()
				hint.Status = &status
			}
		case item.KindMusicAlbum:
			hint.Artists = stringsOrEmptySlice(projected.Artists)
			if projected.AlbumArtist != "" {
				albumArtist := projected.AlbumArtist
				hint.AlbumArtist = &albumArtist
			}
		case item.KindAudio:
			if projected.AlbumArtist != "" {
				albumArtist := projected.AlbumArtist
				hint.AlbumArtist = &albumArtist
			}
			hint.Artists = stringsOrEmptySlice(projected.Artists)
			if projected.Album != "" {
				album := projected.Album
				hint.Album = &album
			}
			hint.AlbumID = projected.AlbumID
		}
		if value.ChannelID != nil && !value.ChannelID.IsNil() {
			channel := *value.ChannelID
			hint.ChannelID = &channel
			if name, found := channels[channel]; found {
				hint.ChannelName = &name
			}
		}
		hints = append(hints, hint)
	}
	return hints, nil
}

func searchHintImageFacts(value item.Item, projected dto.BaseItemDto) searchHintImages {
	facts := searchHintImages{kind: value.Kind, parentID: value.ParentID, thumb: projected.ImageTags[item.ImageTypeThumb.String()]}
	if len(projected.BackdropImageTags) != 0 {
		facts.backdrop = projected.BackdropImageTags[0]
	}
	return facts
}

// loadSearchHintParents reads the parent chain of every hint item, one level
// per query, as BaseItem.GetParents walks it. The parent lookups do not apply
// the user's access, as upstream does not.
func (h itemHandler) loadSearchHintParents(ctx context.Context, images map[dto.UUID]searchHintImages, options store.ItemQueryDTOOptions) error {
	for range searchHintsMaxParentDepth {
		var pending []dto.UUID
		for _, facts := range images {
			if facts.parentID == nil || facts.parentID.IsNil() {
				continue
			}
			if _, loaded := images[*facts.parentID]; !loaded && !slices.Contains(pending, *facts.parentID) {
				pending = append(pending, *facts.parentID)
			}
		}
		if len(pending) == 0 {
			return nil
		}
		parents, err := h.reader.Query(ctx, store.ItemQuery{ItemIDs: pending, IncludeOwnedItems: true})
		if err != nil {
			return err
		}
		page, err := h.build.Build(ctx, parents.Items, options, nil)
		if err != nil {
			return err
		}
		built := make(map[dto.UUID]dto.BaseItemDto, len(page.Items))
		for _, value := range page.Items {
			built[value.ID] = value
		}
		for _, value := range parents.Items {
			images[value.ID] = searchHintImageFacts(value, built[value.ID])
		}
		for _, id := range pending {
			if _, loaded := images[id]; !loaded {
				// A missing parent ends its chain.
				images[id] = searchHintImages{}
			}
		}
	}
	return nil
}

// searchHintImageOwner returns the item that supplies an image: the item
// itself, else (when seriesFirst) the first Series parent with the image,
// else the first parent with it.
func searchHintImageOwner(images map[dto.UUID]searchHintImages, id dto.UUID, has func(searchHintImages) bool, seriesFirst bool) *dto.UUID {
	if has(images[id]) {
		return &id
	}
	parents := make([]dto.UUID, 0, 4)
	for current := images[id]; current.parentID != nil && !current.parentID.IsNil() && len(parents) < searchHintsMaxParentDepth; {
		parent := *current.parentID
		parents = append(parents, parent)
		current = images[parent]
	}
	if seriesFirst {
		for _, parent := range parents {
			if images[parent].kind == item.KindSeries && has(images[parent]) {
				return &parent
			}
		}
	}
	for _, parent := range parents {
		if has(images[parent]) {
			return &parent
		}
	}
	return nil
}

func (h itemHandler) searchHintChannelNames(ctx context.Context, values []item.Item) (map[dto.UUID]string, error) {
	var ids []dto.UUID
	for _, value := range values {
		if value.ChannelID != nil && !value.ChannelID.IsNil() && !slices.Contains(ids, *value.ChannelID) {
			ids = append(ids, *value.ChannelID)
		}
	}
	names := make(map[dto.UUID]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}
	channels, err := h.reader.Query(ctx, store.ItemQuery{ItemIDs: ids})
	if err != nil {
		return nil, err
	}
	for _, channel := range channels.Items {
		if channel.Name != nil {
			names[channel.ID] = *channel.Name
		}
	}
	return names, nil
}

func stringsOrEmptySlice(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
