// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/user"
	"github.com/go-chi/chi/v5"
)

// LiveTvGuide names the Live TV folder that holds the guide items.
// *livetv.Service implements it.
type LiveTvGuide interface {
	FolderID() dto.UUID
	// Hidden reports that no tuner is configured.
	Hidden() bool
}

// LiveTvUsers lists the users for GET /LiveTv/Info. *user.Service
// implements it.
type LiveTvUsers interface {
	List(ctx context.Context, actor user.Actor, options user.UserListOptions) ([]user.UserDTO, error)
}

// LiveTvReads are the dependencies of the channel and programme routes. A
// zero value keeps those routes on their empty stub shapes.
type LiveTvReads struct {
	Guide LiveTvGuide
	Items ItemReader
	Build ItemDTOBuilder
	Users ItemUserResolver
	Clock clock.Clock
	// AllUsers lists the users with Live TV access. Nil reports none.
	AllUsers LiveTvUsers
	// Files serves GET /LiveTv/LiveStreamFiles. Nil keeps the route on
	// its 404.
	Files LiveStreamFiles
}

func (r LiveTvReads) enabled() bool {
	return r.Guide != nil && r.Items != nil && r.Build != nil && r.Clock != nil
}

func registerLiveTvReads(rt *router.Router, reads LiveTvReads, access string) {
	h := liveTvReadHandler{reads: reads}
	rt.Handle(http.MethodGet, "/LiveTv/Info", router.Route{OperationID: "GetLiveTvInfo", Policy: access}, h.info)
	rt.Handle(http.MethodGet, "/LiveTv/Channels", router.Route{OperationID: "GetLiveTvChannels", Policy: access}, h.channels)
	rt.Handle(http.MethodGet, "/LiveTv/Channels/{channelId}", router.Route{OperationID: "GetChannel", Policy: access}, h.channel)
	rt.Handle(http.MethodGet, "/LiveTv/Programs", router.Route{OperationID: "GetLiveTvPrograms", Policy: access}, h.programsGet)
	rt.Handle(http.MethodPost, "/LiveTv/Programs", router.Route{OperationID: "GetPrograms", Policy: access}, h.programsPost)
	rt.Handle(http.MethodGet, "/LiveTv/Programs/Recommended", router.Route{OperationID: "GetRecommendedPrograms", Policy: access}, h.recommended)
	rt.Handle(http.MethodGet, "/LiveTv/Programs/{programId}", router.Route{OperationID: "GetProgram", Policy: access}, h.program)
	rt.Handle(http.MethodGet, "/LiveTv/LiveStreamFiles/{streamId}/stream.{container}", router.Route{OperationID: "GetLiveStreamFile", Anonymous: true}, h.liveStreamFile)
}

type liveTvReadHandler struct {
	reads LiveTvReads
}

// liveTvServiceInfo is MediaBrowser.Model.LiveTv.LiveTvServiceInfo.
type liveTvServiceInfo struct {
	Name               string   `json:"Name"`
	Status             string   `json:"Status"`
	HasUpdateAvailable bool     `json:"HasUpdateAvailable"`
	IsVisible          bool     `json:"IsVisible"`
	Tuners             []string `json:"Tuners"`
}

// liveTvInfoDTO is MediaBrowser.Model.LiveTv.LiveTvInfo.
type liveTvInfoDTO struct {
	Services     []liveTvServiceInfo `json:"Services"`
	IsEnabled    bool                `json:"IsEnabled"`
	EnabledUsers []string            `json:"EnabledUsers"`
}

// info ports LiveTvManager.GetLiveTvInfo (LiveTvManager.cs:1191-1224). The
// one service is the built-in DefaultLiveTvService, named "Emby", so
// IsEnabled is always true. A user is enabled when a tuner exists and the
// user has EnableLiveTvAccess.
func (h liveTvReadHandler) info(w http.ResponseWriter, r *http.Request) {
	if !h.reads.enabled() {
		liveTvStub{}.info(w, r)
		return
	}
	out := liveTvInfoDTO{
		Services:     []liveTvServiceInfo{{Name: "Emby", Status: "Ok", Tuners: []string{}}},
		IsEnabled:    true,
		EnabledUsers: []string{},
	}
	if !h.reads.Guide.Hidden() && h.reads.AllUsers != nil {
		users, err := h.reads.AllUsers.List(r.Context(), user.Actor{Administrator: true}, user.UserListOptions{})
		if err != nil {
			writeLiveTvError(w, err)
			return
		}
		for _, candidate := range users {
			var policy struct{ EnableLiveTvAccess bool }
			if len(candidate.Policy) != 0 && json.Unmarshal(candidate.Policy, &policy) == nil && policy.EnableLiveTvAccess {
				out.EnabledUsers = append(out.EnabledUsers, candidate.ID.String())
			}
		}
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

// liveTvChannelsRequest is the GetLiveTvChannels query. The source binds
// type and isDisliked but GetInternalChannels does not use them.
type liveTvChannelsRequest struct {
	StartIndex            *int     `query:"startIndex"`
	Limit                 *int     `query:"limit"`
	IsMovie               *bool    `query:"isMovie"`
	IsSeries              *bool    `query:"isSeries"`
	IsNews                *bool    `query:"isNews"`
	IsKids                *bool    `query:"isKids"`
	IsSports              *bool    `query:"isSports"`
	IsFavorite            *bool    `query:"isFavorite"`
	IsLiked               *bool    `query:"isLiked"`
	EnableImages          *bool    `query:"enableImages"`
	ImageTypeLimit        *int     `query:"imageTypeLimit"`
	EnableImageTypes      []string `query:"enableImageTypes"`
	Fields                []string `query:"fields"`
	EnableUserData        *bool    `query:"enableUserData"`
	SortBy                []string `query:"sortBy"`
	SortOrder             string   `query:"sortOrder"`
	EnableFavoriteSorting bool     `query:"enableFavoriteSorting"`
	AddCurrentProgram     *bool    `query:"addCurrentProgram"`
}

// channels ports GetLiveTvChannels and LiveTvManager.GetInternalChannels
// (LiveTvController.cs:138-206, LiveTvManager.cs:113-153).
func (h liveTvReadHandler) channels(w http.ResponseWriter, r *http.Request) {
	if !h.reads.enabled() {
		_ = render.JSON(w, r, http.StatusOK, dto.NewQueryResult[struct{}]())
		return
	}
	var request liveTvChannelsRequest
	if err := bind.QueryRequest(r, &request); err != nil || negative(request.StartIndex) || negative(request.Limit) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	sortOrder := store.SortOrderAscending
	if request.SortOrder != "" {
		parsed, ok := bind.ParseText[store.SortOrder](request.SortOrder)
		if !ok {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		sortOrder = parsed
	}
	query := store.ItemQuery{
		IncludeItemTypes:       []item.Kind{item.KindLiveTvChannel},
		TopParentIDs:           []dto.UUID{h.reads.Guide.FolderID()},
		IsMovie:                request.IsMovie,
		IsSeries:               request.IsSeries,
		IsNews:                 request.IsNews,
		IsKids:                 request.IsKids,
		IsSports:               request.IsSports,
		IsFavorite:             request.IsFavorite,
		IsLiked:                request.IsLiked,
		StartIndex:             request.StartIndex,
		Limit:                  request.Limit,
		EnableTotalRecordCount: true,
	}
	if !h.applyUser(w, r, bind.QueryFirst(r, "userId"), &query) {
		return
	}
	for _, sortBy := range bind.DelimitedEnums("sortBy", request.SortBy, bind.ParseText[store.ItemSortBy]) {
		query.OrderBy = append(query.OrderBy, store.ItemOrder{SortBy: sortBy, SortOrder: sortOrder})
	}
	if request.EnableFavoriteSorting {
		query.OrderBy = append([]store.ItemOrder{{SortBy: store.ItemSortByIsFavoriteOrLiked, SortOrder: store.SortOrderDescending}}, query.OrderBy...)
	}
	// The source tests the empty initial OrderBy, so SortName is always
	// appended.
	query.OrderBy = append(query.OrderBy, store.ItemOrder{SortBy: store.ItemSortBySortName, SortOrder: store.SortOrderAscending})
	options := liveTvDtoOptions(request.Fields, request.EnableImages, request.EnableUserData, request.ImageTypeLimit, request.EnableImageTypes)
	options.AddCurrentProgram = request.AddCurrentProgram == nil || *request.AddCurrentProgram
	query.DtoOptions = options
	h.writePage(w, r, query, request.StartIndex)
}

// channel ports GetChannel (LiveTvController.cs:218-238): any visible item,
// projected with the default DtoOptions.
func (h liveTvReadHandler) channel(w http.ResponseWriter, r *http.Request) {
	if !h.reads.enabled() {
		liveTvStub{}.notConfigured(w, r)
		return
	}
	id, err := dto.ParseUUID(chi.URLParam(r, "channelId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	query := store.ItemQuery{}
	if !h.applyUser(w, r, bind.QueryFirst(r, "userId"), &query) {
		return
	}
	if id.IsNil() {
		query.IncludeItemTypes = []item.Kind{item.KindUserRootFolder}
	} else {
		query.ItemIDs = []dto.UUID{id}
	}
	h.writeItem(w, r, query)
}

// program ports GetProgram and LiveTvManager.GetProgram
// (LiveTvController.cs:749-772, LiveTvManager.cs:177-196): the item is read
// without a user check; the user only overlays user data.
func (h liveTvReadHandler) program(w http.ResponseWriter, r *http.Request) {
	if !h.reads.enabled() {
		liveTvStub{}.notConfigured(w, r)
		return
	}
	id, err := dto.ParseUUID(chi.URLParam(r, "programId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	query := store.ItemQuery{}
	if !h.applyUser(w, r, bind.QueryFirst(r, "userId"), &query) {
		return
	}
	query.User = nil
	query.ItemIDs = []dto.UUID{id}
	h.writeItem(w, r, query)
}

// liveTvProgramsRequest is the GetLiveTvPrograms query.
type liveTvProgramsRequest struct {
	ChannelIDs             []dto.UUID `query:"channelIds"`
	MinStartDate           *dto.Time  `query:"minStartDate"`
	HasAired               *bool      `query:"hasAired"`
	IsAiring               *bool      `query:"isAiring"`
	MaxStartDate           *dto.Time  `query:"maxStartDate"`
	MinEndDate             *dto.Time  `query:"minEndDate"`
	MaxEndDate             *dto.Time  `query:"maxEndDate"`
	IsMovie                *bool      `query:"isMovie"`
	IsSeries               *bool      `query:"isSeries"`
	IsNews                 *bool      `query:"isNews"`
	IsKids                 *bool      `query:"isKids"`
	IsSports               *bool      `query:"isSports"`
	StartIndex             *int       `query:"startIndex"`
	Limit                  *int       `query:"limit"`
	SortBy                 []string   `query:"sortBy"`
	SortOrder              []string   `query:"sortOrder"`
	Genres                 []string   `query:"genres"`
	GenreIDs               []dto.UUID `query:"genreIds"`
	EnableImages           *bool      `query:"enableImages"`
	ImageTypeLimit         *int       `query:"imageTypeLimit"`
	EnableImageTypes       []string   `query:"enableImageTypes"`
	EnableUserData         *bool      `query:"enableUserData"`
	SeriesTimerID          string     `query:"seriesTimerId"`
	LibrarySeriesID        *dto.UUID  `query:"librarySeriesId"`
	Fields                 []string   `query:"fields"`
	EnableTotalRecordCount *bool      `query:"enableTotalRecordCount"`
}

// liveTvProgramsQuery is the InternalItemsQuery that GetLiveTvPrograms and
// GetPrograms hand to LiveTvManager.GetPrograms.
type liveTvProgramsQuery struct {
	query           store.ItemQuery
	seriesTimerID   string
	librarySeriesID *dto.UUID
	startIndex      *int
}

// programsGet ports GetLiveTvPrograms (LiveTvController.cs:542-615).
func (h liveTvReadHandler) programsGet(w http.ResponseWriter, r *http.Request) {
	if !h.reads.enabled() {
		_ = render.JSON(w, r, http.StatusOK, dto.NewQueryResult[struct{}]())
		return
	}
	var request liveTvProgramsRequest
	if err := bind.QueryRequest(r, &request); err != nil || negative(request.StartIndex) || negative(request.Limit) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	genres := make([]string, 0, len(request.Genres))
	for _, value := range request.Genres {
		genres = append(genres, strings.Split(value, "|")...)
	}
	programs := liveTvProgramsQuery{
		query: store.ItemQuery{
			ChannelIDs:   request.ChannelIDs,
			HasAired:     request.HasAired,
			IsAiring:     request.IsAiring,
			MinStartDate: request.MinStartDate.StdPtr(),
			MaxStartDate: request.MaxStartDate.StdPtr(),
			MinEndDate:   request.MinEndDate.StdPtr(),
			MaxEndDate:   request.MaxEndDate.StdPtr(),
			IsMovie:      request.IsMovie,
			IsSeries:     request.IsSeries,
			IsNews:       request.IsNews,
			IsKids:       request.IsKids,
			IsSports:     request.IsSports,
			StartIndex:   request.StartIndex,
			Limit:        request.Limit,
			Genres:       nonEmpty(genres),
			GenreIDs:     request.GenreIDs,
			OrderBy: liveTvOrderBy(
				bind.DelimitedEnums("sortBy", request.SortBy, bind.ParseText[store.ItemSortBy]),
				bind.DelimitedEnums("sortOrder", request.SortOrder, bind.ParseText[store.SortOrder])),
			EnableTotalRecordCount: request.EnableTotalRecordCount == nil || *request.EnableTotalRecordCount,
			DtoOptions:             liveTvDtoOptions(request.Fields, request.EnableImages, request.EnableUserData, request.ImageTypeLimit, request.EnableImageTypes),
		},
		seriesTimerID:   request.SeriesTimerID,
		librarySeriesID: request.LibrarySeriesID,
		startIndex:      request.StartIndex,
	}
	if !h.applyUser(w, r, bind.QueryFirst(r, "userId"), &programs.query) {
		return
	}
	h.writePrograms(w, r, programs)
}

// getProgramsDTO is Jellyfin.Api.Models.LiveTvDtos.GetProgramsDto.
type getProgramsDTO struct {
	ChannelIDs             commaList `json:"ChannelIds"`
	UserID                 *string   `json:"UserId"`
	MinStartDate           *dto.Time `json:"MinStartDate"`
	HasAired               *bool     `json:"HasAired"`
	IsAiring               *bool     `json:"IsAiring"`
	MaxStartDate           *dto.Time `json:"MaxStartDate"`
	MinEndDate             *dto.Time `json:"MinEndDate"`
	MaxEndDate             *dto.Time `json:"MaxEndDate"`
	IsMovie                *bool     `json:"IsMovie"`
	IsSeries               *bool     `json:"IsSeries"`
	IsNews                 *bool     `json:"IsNews"`
	IsKids                 *bool     `json:"IsKids"`
	IsSports               *bool     `json:"IsSports"`
	StartIndex             *int      `json:"StartIndex"`
	Limit                  *int      `json:"Limit"`
	SortBy                 commaList `json:"SortBy"`
	SortOrder              commaList `json:"SortOrder"`
	Genres                 pipeList  `json:"Genres"`
	GenreIDs               commaList `json:"GenreIds"`
	EnableImages           *bool     `json:"EnableImages"`
	EnableTotalRecordCount *bool     `json:"EnableTotalRecordCount"`
	ImageTypeLimit         *int      `json:"ImageTypeLimit"`
	EnableImageTypes       commaList `json:"EnableImageTypes"`
	EnableUserData         *bool     `json:"EnableUserData"`
	SeriesTimerID          *string   `json:"SeriesTimerId"`
	LibrarySeriesID        *string   `json:"LibrarySeriesId"`
	Fields                 commaList `json:"Fields"`
}

// programsPost ports GetPrograms (LiveTvController.cs:626-681). The user
// comes only from the body.
func (h liveTvReadHandler) programsPost(w http.ResponseWriter, r *http.Request) {
	if !h.reads.enabled() {
		_ = render.JSON(w, r, http.StatusOK, dto.NewQueryResult[struct{}]())
		return
	}
	var body getProgramsDTO
	if err := bind.JSONBody(r, &body); err != nil {
		writeBindError(w, err)
		return
	}
	channelIDs, okChannels := uuidList(body.ChannelIDs)
	genreIDs, okGenres := uuidList(body.GenreIDs)
	userID, okUser := optionalUUID(body.UserID)
	seriesID, okSeries := optionalUUID(body.LibrarySeriesID)
	if !okChannels || !okGenres || !okUser || !okSeries || negative(body.StartIndex) || negative(body.Limit) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	programs := liveTvProgramsQuery{
		query: store.ItemQuery{
			ChannelIDs:   channelIDs,
			HasAired:     body.HasAired,
			IsAiring:     body.IsAiring,
			MinStartDate: body.MinStartDate.StdPtr(),
			MaxStartDate: body.MaxStartDate.StdPtr(),
			MinEndDate:   body.MinEndDate.StdPtr(),
			MaxEndDate:   body.MaxEndDate.StdPtr(),
			IsMovie:      body.IsMovie,
			IsSeries:     body.IsSeries,
			IsNews:       body.IsNews,
			IsKids:       body.IsKids,
			IsSports:     body.IsSports,
			StartIndex:   body.StartIndex,
			Limit:        body.Limit,
			Genres:       nonEmpty(body.Genres),
			GenreIDs:     genreIDs,
			OrderBy: liveTvOrderBy(
				bind.DelimitedEnums("SortBy", body.SortBy, bind.ParseText[store.ItemSortBy]),
				bind.DelimitedEnums("SortOrder", body.SortOrder, bind.ParseText[store.SortOrder])),
			EnableTotalRecordCount: body.EnableTotalRecordCount == nil || *body.EnableTotalRecordCount,
			DtoOptions:             liveTvDtoOptions(body.Fields, body.EnableImages, body.EnableUserData, body.ImageTypeLimit, body.EnableImageTypes),
		},
		librarySeriesID: seriesID,
		startIndex:      body.StartIndex,
	}
	if body.SeriesTimerID != nil {
		programs.seriesTimerID = *body.SeriesTimerID
	}
	if userID != nil && !userID.IsNil() {
		user, ok := resolveSelectedUser(w, r, userID, h.reads.Users)
		if !ok {
			return
		}
		applyItemUser(&programs.query, user)
	}
	h.writePrograms(w, r, programs)
}

// liveTvRecommendedRequest is the GetRecommendedPrograms query.
type liveTvRecommendedRequest struct {
	StartIndex             *int       `query:"startIndex"`
	Limit                  *int       `query:"limit"`
	IsAiring               *bool      `query:"isAiring"`
	HasAired               *bool      `query:"hasAired"`
	IsSeries               *bool      `query:"isSeries"`
	IsMovie                *bool      `query:"isMovie"`
	IsNews                 *bool      `query:"isNews"`
	IsKids                 *bool      `query:"isKids"`
	IsSports               *bool      `query:"isSports"`
	EnableImages           *bool      `query:"enableImages"`
	ImageTypeLimit         *int       `query:"imageTypeLimit"`
	EnableImageTypes       []string   `query:"enableImageTypes"`
	GenreIDs               []dto.UUID `query:"genreIds"`
	Fields                 []string   `query:"fields"`
	EnableUserData         *bool      `query:"enableUserData"`
	EnableTotalRecordCount *bool      `query:"enableTotalRecordCount"`
}

// recommended ports GetRecommendedPrograms and
// LiveTvManager.GetRecommendedProgramsAsync (LiveTvController.cs:694-738,
// LiveTvManager.cs:272-338). A request that is not for airing programmes is
// a GetPrograms request.
func (h liveTvReadHandler) recommended(w http.ResponseWriter, r *http.Request) {
	if !h.reads.enabled() {
		_ = render.JSON(w, r, http.StatusOK, dto.NewQueryResult[struct{}]())
		return
	}
	var request liveTvRecommendedRequest
	if err := bind.QueryRequest(r, &request); err != nil || negative(request.StartIndex) || negative(request.Limit) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	programs := liveTvProgramsQuery{
		query: store.ItemQuery{
			IsAiring:               request.IsAiring,
			HasAired:               request.HasAired,
			IsSeries:               request.IsSeries,
			IsMovie:                request.IsMovie,
			IsNews:                 request.IsNews,
			IsKids:                 request.IsKids,
			IsSports:               request.IsSports,
			StartIndex:             request.StartIndex,
			Limit:                  request.Limit,
			GenreIDs:               request.GenreIDs,
			EnableTotalRecordCount: request.EnableTotalRecordCount == nil || *request.EnableTotalRecordCount,
			DtoOptions:             liveTvDtoOptions(request.Fields, request.EnableImages, request.EnableUserData, request.ImageTypeLimit, request.EnableImageTypes),
		},
		startIndex: request.StartIndex,
	}
	if !h.applyUser(w, r, bind.QueryFirst(r, "userId"), &programs.query) {
		return
	}
	if request.IsAiring == nil || !*request.IsAiring {
		h.writePrograms(w, r, programs)
		return
	}

	query := programs.query
	query.IncludeItemTypes = []item.Kind{item.KindLiveTvProgram}
	query.TopParentIDs = []dto.UUID{h.reads.Guide.FolderID()}
	query.OrderBy = []store.ItemOrder{{SortBy: store.ItemSortByStartDate, SortOrder: store.SortOrderAscending}}
	query.StartIndex, query.Limit = nil, nil
	if request.Limit != nil && *request.Limit > 0 {
		limit := max(*request.Limit*4, 200)
		query.Limit = &limit
	}
	h.setReferenceTime(&query)
	found, err := h.reads.Items.Query(r.Context(), query)
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	scores, err := h.recommendationScores(r.Context(), found.Items, query.UserID)
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	ordered := append([]item.Item(nil), found.Items...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := startDay(ordered[i]), startDay(ordered[j])
		if !left.Equal(right) {
			return left.Before(right)
		}
		return scores[ordered[i].ID] > scores[ordered[j].ID]
	})
	if request.Limit != nil && *request.Limit > 0 && len(ordered) > *request.Limit {
		ordered = ordered[:*request.Limit]
	}
	page, err := h.reads.Build.Build(r.Context(), ordered, withoutLiveTvRemovedFields(query.DtoOptions), query.UserID)
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	page.TotalRecordCount = len(found.Items)
	page.StartIndex = intValue(request.StartIndex)
	_ = render.JSON(w, r, http.StatusOK, page)
}

// recommendationScores ports GetRecommendationScore with
// factorChannelWatchCount set: the programme's live and new-episode facts
// and the user's like, favorite and play count on its channel.
func (h liveTvReadHandler) recommendationScores(ctx context.Context, programs []item.Item, userID *dto.UUID) (map[dto.UUID]int, error) {
	channelIDs := make([]dto.UUID, 0)
	seen := make(map[dto.UUID]struct{})
	for _, program := range programs {
		if program.ChannelID != nil {
			if _, ok := seen[*program.ChannelID]; !ok {
				seen[*program.ChannelID] = struct{}{}
				channelIDs = append(channelIDs, *program.ChannelID)
			}
		}
	}
	userData := make(map[dto.UUID]*dto.UserItemDataDto)
	if len(channelIDs) != 0 && userID != nil {
		channels, err := h.reads.Items.Query(ctx, store.ItemQuery{ItemIDs: channelIDs, UserID: userID})
		if err != nil {
			return nil, err
		}
		page, err := h.reads.Build.Build(ctx, channels.Items, store.ItemQueryDTOOptions{EnableUserData: true}, userID)
		if err != nil {
			return nil, err
		}
		for index := range page.Items {
			userData[page.Items[index].ID] = page.Items[index].UserData
		}
	}
	scores := make(map[dto.UUID]int, len(programs))
	for _, program := range programs {
		score := 0
		if programHasTag(program, "Live") {
			score++
		}
		if program.IsSeries && !program.IsRepeat {
			score++
		}
		if program.ChannelID != nil {
			if data := userData[*program.ChannelID]; data != nil {
				if data.Likes != nil {
					if *data.Likes {
						score += 2
					} else {
						score -= 2
					}
				}
				if data.IsFavorite {
					score += 3
				}
				score += data.PlayCount
			}
		}
		scores[program.ID] = score
	}
	return scores, nil
}

// writePrograms ports LiveTvManager.GetPrograms (LiveTvManager.cs:198-270).
// No series timers exist, so a series timer filter matches nothing.
func (h liveTvReadHandler) writePrograms(w http.ResponseWriter, r *http.Request, programs liveTvProgramsQuery) {
	if strings.TrimSpace(programs.seriesTimerID) != "" {
		_ = render.JSON(w, r, http.StatusOK, dto.NewQueryResult[dto.BaseItemDto]())
		return
	}
	query := programs.query
	query.IncludeItemTypes = []item.Kind{item.KindLiveTvProgram}
	query.TopParentIDs = []dto.UUID{h.reads.Guide.FolderID()}
	if len(query.OrderBy) == 0 {
		query.OrderBy = []store.ItemOrder{{SortBy: store.ItemSortByStartDate, SortOrder: store.SortOrderAscending}}
	}
	if programs.librarySeriesID != nil && !programs.librarySeriesID.IsNil() {
		isSeries := true
		query.IsSeries = &isSeries
		series, err := h.reads.Items.Query(r.Context(), store.ItemQuery{ItemIDs: []dto.UUID{*programs.librarySeriesID}, IncludeItemTypes: []item.Kind{item.KindSeries}})
		if err != nil {
			writeLiveTvError(w, err)
			return
		}
		if len(series.Items) != 0 && series.Items[0].Name != nil {
			query.Name = *series.Items[0].Name
		}
	}
	query.DtoOptions = withoutLiveTvRemovedFields(query.DtoOptions)
	h.setReferenceTime(&query)
	h.writePage(w, r, query, programs.startIndex)
}

func (h liveTvReadHandler) writePage(w http.ResponseWriter, r *http.Request, query store.ItemQuery, startIndex *int) {
	found, err := h.reads.Items.Query(r.Context(), query)
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	page, err := h.reads.Build.Build(r.Context(), found.Items, query.DtoOptions, query.UserID)
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	page.TotalRecordCount = found.TotalRecordCount
	page.StartIndex = intValue(startIndex)
	_ = render.JSON(w, r, http.StatusOK, page)
}

func (h liveTvReadHandler) writeItem(w http.ResponseWriter, r *http.Request, query store.ItemQuery) {
	found, err := h.reads.Items.Query(r.Context(), query)
	if err != nil {
		writeLiveTvError(w, err)
		return
	}
	if len(found.Items) == 0 {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	options := bind.DefaultItemQueryDTOOptions()
	page, err := h.reads.Build.BuildDetailed(r.Context(), found.Items[:1], options, query.UserID)
	if err != nil || len(page.Items) == 0 {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	_ = render.JSON(w, r, http.StatusOK, page.Items[0])
}

// applyUser ports RequestHelpers.GetUserId for the Live TV routes.
func (h liveTvReadHandler) applyUser(w http.ResponseWriter, r *http.Request, requested string, query *store.ItemQuery) bool {
	if isGlobalAPIKeyItemRequest(r, requested) {
		return true
	}
	user, ok := resolveItemUser(w, r, requested, h.reads.Users)
	if !ok {
		return false
	}
	applyItemUser(query, user)
	return true
}

func (h liveTvReadHandler) setReferenceTime(query *store.ItemQuery) {
	if query.IsAiring != nil || query.HasAired != nil {
		now := h.reads.Clock.Now().UTC()
		query.ReferenceTime = &now
	}
}

// liveTvDtoOptions ports new DtoOptions { Fields = fields } with
// AddAdditionalDtoOptions.
func liveTvDtoOptions(fields []string, enableImages, enableUserData *bool, imageTypeLimit *int, imageTypes []string) store.ItemQueryDTOOptions {
	options := bind.DefaultItemQueryDTOOptions()
	options.Fields = bind.ItemFields("fields", fields)
	if enableImages != nil {
		options.EnableImages = *enableImages
	}
	if enableUserData != nil {
		options.EnableUserData = *enableUserData
	}
	if imageTypeLimit != nil {
		options.ImageTypeLimit = *imageTypeLimit
	}
	if parsed := bind.DelimitedEnums("enableImageTypes", imageTypes, bind.ParseImageType); len(parsed) != 0 {
		options.ImageTypes = parsed
	}
	return options
}

// withoutLiveTvRemovedFields ports LiveTvManager.RemoveFields.
func withoutLiveTvRemovedFields(options store.ItemQueryDTOOptions) store.ItemQueryDTOOptions {
	out := options
	out.Fields = make([]string, 0, len(options.Fields))
	for _, field := range options.Fields {
		switch field {
		case "CanDelete", "CanDownload", "DisplayPreferencesId", "Etag":
			continue
		}
		out.Fields = append(out.Fields, field)
	}
	return out
}

// liveTvOrderBy ports RequestHelpers.GetOrderBy.
func liveTvOrderBy(sortBy []store.ItemSortBy, sortOrder []store.SortOrder) []store.ItemOrder {
	if len(sortBy) == 0 {
		return nil
	}
	fallback := store.SortOrderAscending
	if len(sortOrder) != 0 {
		fallback = sortOrder[0]
	}
	out := make([]store.ItemOrder, len(sortBy))
	for index, value := range sortBy {
		order := fallback
		if index < len(sortOrder) {
			order = sortOrder[index]
		}
		out[index] = store.ItemOrder{SortBy: value, SortOrder: order}
	}
	return out
}

func startDay(program item.Item) time.Time {
	if program.StartDate == nil {
		return time.Time{}
	}
	return program.StartDate.UTC().Truncate(24 * time.Hour)
}

func programHasTag(program item.Item, tag string) bool {
	for _, value := range program.Values.Tags {
		if strings.EqualFold(value, tag) {
			return true
		}
	}
	return false
}

func negative(value *int) bool { return value != nil && *value < 0 }

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func uuidList(values []string) ([]dto.UUID, bool) {
	out := make([]dto.UUID, 0, len(values))
	for _, value := range values {
		parsed, err := dto.ParseUUID(strings.TrimSpace(value))
		if err != nil {
			return nil, false
		}
		out = append(out, parsed)
	}
	return out, true
}

func optionalUUID(value *string) (*dto.UUID, bool) {
	if value == nil || *value == "" {
		return nil, true
	}
	parsed, err := dto.ParseUUID(*value)
	if err != nil {
		return nil, false
	}
	return &parsed, true
}

// commaList is a JsonCommaDelimitedCollectionConverter value: a JSON array
// or one comma-delimited string.
type commaList []string

func (l *commaList) UnmarshalJSON(data []byte) error {
	values, err := delimitedJSON(data, ",")
	*l = values
	return err
}

// pipeList is a JsonPipeDelimitedCollectionConverter value.
type pipeList []string

func (l *pipeList) UnmarshalJSON(data []byte) error {
	values, err := delimitedJSON(data, "|")
	*l = values
	return err
}

func delimitedJSON(data []byte, separator string) ([]string, error) {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		return nil, nil
	}
	if len(data) != 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return nil, err
		}
		out := make([]string, 0)
		for _, value := range strings.Split(text, separator) {
			if value = strings.TrimSpace(value); value != "" {
				out = append(out, value)
			}
		}
		return out, nil
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}
