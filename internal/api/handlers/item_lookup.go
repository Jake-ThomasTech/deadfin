// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/identify"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/store"
)

// ItemLookupSearcher is the narrow seam behind the nine typed
type ItemLookupSearcher interface {
	Search(ctx context.Context, kind item.Kind, name string, year *int, providerIDs map[string]string, language, country string, albumArtists []string, songInfos []providers.SongInfo, seriesName string, artistProviderIDs map[string]string, itemID dto.UUID, searchProviderName string, includeDisabled bool) ([]providers.RemoteSearchResult, error)
	Apply(ctx context.Context, itemID dto.UUID, result providers.RemoteSearchResult, replaceAllImages bool) error
	ExternalIDs(ctx context.Context, itemID dto.UUID) ([]providers.ExternalIDDescriptor, error)
}

// RegisterItemLookup registers the ItemLookup routes (WP-P8-05): nine typed
// remote-search endpoints, the apply flow, and ExternalIdInfos.
func RegisterItemLookup(rt *router.Router, reader ItemReader, searcher ItemLookupSearcher) {
	h := itemLookupHandler{reader: reader, searcher: searcher}

	rt.Handle(http.MethodPost, "/Items/RemoteSearch/Movie", router.Route{
		OperationID: "GetMovieRemoteSearchResults",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.remoteSearchMovie)
	rt.Handle(http.MethodPost, "/Items/RemoteSearch/Trailer", router.Route{
		OperationID: "GetTrailerRemoteSearchResults",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.remoteSearchTrailer)
	rt.Handle(http.MethodPost, "/Items/RemoteSearch/MusicVideo", router.Route{
		OperationID: "GetMusicVideoRemoteSearchResults",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.remoteSearchMusicVideo)
	rt.Handle(http.MethodPost, "/Items/RemoteSearch/Series", router.Route{
		OperationID: "GetSeriesRemoteSearchResults",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.remoteSearchSeries)
	rt.Handle(http.MethodPost, "/Items/RemoteSearch/BoxSet", router.Route{
		OperationID: "GetBoxSetRemoteSearchResults",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.remoteSearchBoxSet)
	rt.Handle(http.MethodPost, "/Items/RemoteSearch/MusicArtist", router.Route{
		OperationID: "GetMusicArtistRemoteSearchResults",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.remoteSearchMusicArtist)
	rt.Handle(http.MethodPost, "/Items/RemoteSearch/MusicAlbum", router.Route{
		OperationID: "GetMusicAlbumRemoteSearchResults",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.remoteSearchMusicAlbum)
	rt.Handle(http.MethodPost, "/Items/RemoteSearch/Person", router.Route{
		OperationID: "GetPersonRemoteSearchResults",
		Policy:      string(auth.PolicyRequiresElevation),
	}, h.remoteSearchPerson)
	rt.Handle(http.MethodPost, "/Items/RemoteSearch/Book", router.Route{
		OperationID: "GetBookRemoteSearchResults",
		Policy:      string(auth.PolicyDefaultAuthorization),
	}, h.remoteSearchBook)

	rt.Handle(http.MethodPost, "/Items/RemoteSearch/Apply/{itemId}", router.Route{
		OperationID: "ApplySearchCriteria",
		Policy:      string(auth.PolicyRequiresElevation),
	}, h.apply)

	rt.Handle(http.MethodGet, "/Items/{itemId}/ExternalIdInfos", router.Route{
		OperationID: "GetExternalIdInfos",
		Policy:      string(auth.PolicyRequiresElevation),
	}, h.externalIDs)
}

type itemLookupHandler struct {
	reader   ItemReader
	searcher ItemLookupSearcher
}

// applyQuery binds the ApplySearchCriteria query. replaceAllImages defaults to
// true (ItemLookupController.cs:251).
type applyQuery struct {
	ReplaceAllImages bool `query:"replaceAllImages"`
}

func (h itemLookupHandler) apply(w http.ResponseWriter, r *http.Request) {
	id, err := dto.ParseUUID(chi.URLParam(r, "itemId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	var query applyQuery
	query.ReplaceAllImages = true
	if err := bind.QueryRequest(r, &query); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	// 404 before work, mirroring item_refresh.go.
	if h.reader == nil || h.searcher == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	result, err := h.reader.Query(r.Context(), store.ItemQuery{ItemIDs: []dto.UUID{id}, Recursive: true})
	if err != nil {
		slog.Default().LogAttrs(r.Context(), slog.LevelError, "apply remote search result: load item failed",
			slog.String("item", id.String()), slog.String("error", err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if len(result.Items) == 0 {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	var body dto.RemoteSearchResult
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	inner := dtoToSearchResult(body)
	if err := h.searcher.Apply(r.Context(), id, inner, query.ReplaceAllImages); err != nil {
		switch {
		case errors.Is(err, identify.ErrItemNotFound):
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		default:
			slog.Default().LogAttrs(r.Context(), slog.LevelError, "apply remote search result failed",
				slog.String("item", id.String()), slog.String("error", err.Error()))
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h itemLookupHandler) externalIDs(w http.ResponseWriter, r *http.Request) {
	id, err := dto.ParseUUID(chi.URLParam(r, "itemId"))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if h.reader == nil || h.searcher == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// 404 before work.
	result, err := h.reader.Query(r.Context(), store.ItemQuery{ItemIDs: []dto.UUID{id}, Recursive: true})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if len(result.Items) == 0 {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	descs, err := h.searcher.ExternalIDs(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, identify.ErrItemNotFound):
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	out := make([]dto.ExternalIdInfo, 0, len(descs))
	for _, d := range descs {
		out = append(out, dto.ExternalIdInfo{
			Name: d.ProviderName,
			Key:  d.Key,
			Type: d.Type,
		})
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

// --- remote search handlers ---

func (h itemLookupHandler) remoteSearchMovie(w http.ResponseWriter, r *http.Request) {
	var body dto.MovieInfoRemoteSearchQuery
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p := searchParamsFromFields(movieInfoFields(body.SearchInfo))
	p.itemID = body.ItemID
	h.runSearch(w, r, item.KindMovie, p, body.SearchProviderName, body.IncludeDisabledProviders)
}

func (h itemLookupHandler) remoteSearchSeries(w http.ResponseWriter, r *http.Request) {
	var body dto.SeriesInfoRemoteSearchQuery
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p := searchParamsFromFields(seriesInfoFields(body.SearchInfo))
	p.itemID = body.ItemID
	h.runSearch(w, r, item.KindSeries, p, body.SearchProviderName, body.IncludeDisabledProviders)
}

func (h itemLookupHandler) remoteSearchBoxSet(w http.ResponseWriter, r *http.Request) {
	var body dto.BoxSetInfoRemoteSearchQuery
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p := searchParamsFromFields(boxSetInfoFields(body.SearchInfo))
	p.itemID = body.ItemID
	h.runSearch(w, r, item.KindBoxSet, p, body.SearchProviderName, body.IncludeDisabledProviders)
}

func (h itemLookupHandler) remoteSearchTrailer(w http.ResponseWriter, r *http.Request) {
	var body dto.TrailerInfoRemoteSearchQuery
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p := searchParamsFromFields(trailerInfoFields(body.SearchInfo))
	p.itemID = body.ItemID
	h.runSearch(w, r, item.KindTrailer, p, body.SearchProviderName, body.IncludeDisabledProviders)
}

func (h itemLookupHandler) remoteSearchMusicVideo(w http.ResponseWriter, r *http.Request) {
	var body dto.MusicVideoInfoRemoteSearchQuery
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p := searchParamsFromFields(musicVideoInfoFields(body.SearchInfo))
	p.itemID = body.ItemID
	h.runSearch(w, r, item.KindMusicVideo, p, body.SearchProviderName, body.IncludeDisabledProviders)
}

func (h itemLookupHandler) remoteSearchPerson(w http.ResponseWriter, r *http.Request) {
	var body dto.PersonLookupInfoRemoteSearchQuery
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p := searchParamsFromFields(personLookupInfoFields(body.SearchInfo))
	p.itemID = body.ItemID
	h.runSearch(w, r, item.KindPerson, p, body.SearchProviderName, body.IncludeDisabledProviders)
}

func (h itemLookupHandler) remoteSearchBook(w http.ResponseWriter, r *http.Request) {
	var body dto.BookInfoRemoteSearchQuery
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p := searchParamsFromFields(bookInfoFields(body.SearchInfo))
	p.itemID = body.ItemID
	if body.SearchInfo != nil && body.SearchInfo.SeriesName != nil {
		p.seriesName = *body.SearchInfo.SeriesName
	}
	h.runSearch(w, r, item.KindBook, p, body.SearchProviderName, body.IncludeDisabledProviders)
}

func (h itemLookupHandler) remoteSearchMusicArtist(w http.ResponseWriter, r *http.Request) {
	var body dto.MusicArtistInfoRemoteSearchQuery
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p := searchParamsFromFields(artistInfoFields(body.SearchInfo))
	p.itemID = body.ItemID
	if body.SearchInfo != nil {
		p.songInfos = dtoSongInfosToProviders(body.SearchInfo.SongInfos)
	}
	h.runSearch(w, r, item.KindMusicArtist, p, body.SearchProviderName, body.IncludeDisabledProviders)
}

func (h itemLookupHandler) remoteSearchMusicAlbum(w http.ResponseWriter, r *http.Request) {
	var body dto.AlbumInfoRemoteSearchQuery
	if err := bind.JSONBody(r, &body); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	p := searchParamsFromFields(albumInfoFields(body.SearchInfo))
	p.itemID = body.ItemID
	if body.SearchInfo != nil {
		for _, a := range body.SearchInfo.AlbumArtists {
			if a != "" {
				p.albumArtists = append(p.albumArtists, a)
			}
		}
		p.artistProviderIDs = body.SearchInfo.ArtistProviderIDs
		p.songInfos = dtoSongInfosToProviders(body.SearchInfo.SongInfos)
	}
	h.runSearch(w, r, item.KindMusicAlbum, p, body.SearchProviderName, body.IncludeDisabledProviders)
}

// searchParams carries the extracted search fields, including the per-kind
// extras that the flattened Searcher seam forwards to providers.
type searchParams struct {
	name              string
	year              *int
	providerIDs       map[string]string
	language          string
	country           string
	albumArtists      []string
	songInfos         []providers.SongInfo
	seriesName        string
	artistProviderIDs map[string]string
	itemID            dto.UUID
}

// baseInfoFields is the common field set across all *Info types. The per-type
// free functions extract from each concrete DTO without defining methods on
// non-local types.
type baseInfoFields struct {
	name        *string
	year        *int
	providerIDs map[string]string
	language    *string
	country     *string
}

func searchParamsFromFields(info baseInfoFields) searchParams {
	var p searchParams
	if info.name != nil {
		p.name = *info.name
	}
	p.year = info.year
	p.providerIDs = info.providerIDs
	if info.language != nil {
		p.language = *info.language
	}
	if info.country != nil {
		p.country = *info.country
	}
	return p
}

// dtoSongInfosToProviders converts the wire SongInfo slice to the providers
// seam type, carrying the per-track fields that album/artist search needs.
func dtoSongInfosToProviders(in []dto.SongInfo) []providers.SongInfo {
	if len(in) == 0 {
		return nil
	}
	out := make([]providers.SongInfo, 0, len(in))
	for _, s := range in {
		var name, album string
		if s.Name != nil {
			name = *s.Name
		}
		if s.Album != nil {
			album = *s.Album
		}
		out = append(out, providers.SongInfo{
			Name:         name,
			AlbumArtists: s.AlbumArtists,
			Album:        album,
			Artists:      s.Artists,
			ProviderIDs:  s.ProviderIDs,
		})
	}
	return out
}

// Each *Info free function extracts the common fields from a pointer (nil → zero).
func movieInfoFields(i *dto.MovieInfo) baseInfoFields {
	if i == nil {
		return baseInfoFields{}
	}
	return baseInfoFields{i.Name, i.Year, i.ProviderIDs, i.MetadataLanguage, i.MetadataCountryCode}
}
func seriesInfoFields(i *dto.SeriesInfo) baseInfoFields {
	if i == nil {
		return baseInfoFields{}
	}
	return baseInfoFields{i.Name, i.Year, i.ProviderIDs, i.MetadataLanguage, i.MetadataCountryCode}
}
func boxSetInfoFields(i *dto.BoxSetInfo) baseInfoFields {
	if i == nil {
		return baseInfoFields{}
	}
	return baseInfoFields{i.Name, i.Year, i.ProviderIDs, i.MetadataLanguage, i.MetadataCountryCode}
}
func trailerInfoFields(i *dto.TrailerInfo) baseInfoFields {
	if i == nil {
		return baseInfoFields{}
	}
	return baseInfoFields{i.Name, i.Year, i.ProviderIDs, i.MetadataLanguage, i.MetadataCountryCode}
}
func musicVideoInfoFields(i *dto.MusicVideoInfo) baseInfoFields {
	if i == nil {
		return baseInfoFields{}
	}
	return baseInfoFields{i.Name, i.Year, i.ProviderIDs, i.MetadataLanguage, i.MetadataCountryCode}
}
func personLookupInfoFields(i *dto.PersonLookupInfo) baseInfoFields {
	if i == nil {
		return baseInfoFields{}
	}
	return baseInfoFields{i.Name, i.Year, i.ProviderIDs, i.MetadataLanguage, i.MetadataCountryCode}
}
func bookInfoFields(i *dto.BookInfo) baseInfoFields {
	if i == nil {
		return baseInfoFields{}
	}
	return baseInfoFields{i.Name, i.Year, i.ProviderIDs, i.MetadataLanguage, i.MetadataCountryCode}
}
func artistInfoFields(i *dto.ArtistInfo) baseInfoFields {
	if i == nil {
		return baseInfoFields{}
	}
	return baseInfoFields{i.Name, i.Year, i.ProviderIDs, i.MetadataLanguage, i.MetadataCountryCode}
}
func albumInfoFields(i *dto.AlbumInfo) baseInfoFields {
	if i == nil {
		return baseInfoFields{}
	}
	return baseInfoFields{i.Name, i.Year, i.ProviderIDs, i.MetadataLanguage, i.MetadataCountryCode}
}

func (h itemLookupHandler) runSearch(w http.ResponseWriter, r *http.Request, kind item.Kind, params searchParams, searchProviderName *string, includeDisabled bool) {
	if h.searcher == nil {
		// No searcher: return empty list, matching upstream's empty-list contract
		// when no provider matches.
		_ = render.JSON(w, r, http.StatusOK, []dto.RemoteSearchResult{})
		return
	}
	spName := ""
	if searchProviderName != nil {
		spName = *searchProviderName
	}
	results, err := h.searcher.Search(r.Context(), kind, params.name, params.year, params.providerIDs, params.language, params.country, params.albumArtists, params.songInfos, params.seriesName, params.artistProviderIDs, params.itemID, spName, includeDisabled)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	out := make([]dto.RemoteSearchResult, 0, len(results))
	for _, result := range results {
		out = append(out, searchResultToDTO(result))
	}
	_ = render.JSON(w, r, http.StatusOK, out)
}

// --- DTO conversion ---

func searchResultToDTO(r providers.RemoteSearchResult) dto.RemoteSearchResult {
	out := dto.RemoteSearchResult{
		ProviderIDs: r.ProviderIDs,
	}
	if r.Name != nil {
		out.Name = r.Name
	}
	if r.ProductionYear != nil {
		out.ProductionYear = r.ProductionYear
	}
	if r.IndexNumber != nil {
		out.IndexNumber = r.IndexNumber
	}
	if r.IndexNumberEnd != nil {
		out.IndexNumberEnd = r.IndexNumberEnd
	}
	if r.ParentIndexNumber != nil {
		out.ParentIndexNumber = r.ParentIndexNumber
	}
	if r.PremiereDate != nil {
		out.PremiereDate = parsePremiereDate(*r.PremiereDate)
	}
	if r.Overview != nil {
		out.Overview = r.Overview
	}
	if r.SearchProviderName != "" {
		out.SearchProviderName = &r.SearchProviderName
	}
	if r.ImageURL != "" {
		out.ImageURL = &r.ImageURL
	}
	if r.AlbumArtist != nil {
		album := searchResultToDTO(*r.AlbumArtist)
		out.AlbumArtist = &album
	}
	if len(r.Artists) > 0 {
		out.Artists = make([]dto.RemoteSearchResult, len(r.Artists))
		for i, a := range r.Artists {
			out.Artists[i] = searchResultToDTO(a)
		}
	}
	return out
}

func dtoToSearchResult(r dto.RemoteSearchResult) providers.RemoteSearchResult {
	out := providers.RemoteSearchResult{
		ProviderIDs: r.ProviderIDs,
	}
	if r.Name != nil {
		out.Name = r.Name
	}
	if r.ProductionYear != nil {
		out.ProductionYear = r.ProductionYear
	}
	if r.Overview != nil {
		out.Overview = r.Overview
	}
	if r.SearchProviderName != nil {
		out.SearchProviderName = *r.SearchProviderName
	}
	if r.ImageURL != nil {
		out.ImageURL = *r.ImageURL
	}
	return out
}

// parsePremiereDate parses a providers.RemoteSearchResult premiere date string
// (ISO 8601, as TMDb returns) into a dto.Time for the wire response. A parse
// failure yields nil rather than a malformed payload.
func parsePremiereDate(value string) *dto.Time {
	var t dto.Time
	if err := t.UnmarshalText([]byte(value)); err != nil {
		return nil
	}
	return &t
}
