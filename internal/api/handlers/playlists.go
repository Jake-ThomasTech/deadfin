// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playlists"
	"github.com/deadfin-org/deadfin/internal/store"
)

// PlaylistService is the playlist manager seam (internal/playlists).
type PlaylistService interface {
	ForUser(ctx context.Context, playlistID, userID dto.UUID) (playlists.Playlist, error)
	Exists(ctx context.Context, playlistID dto.UUID) (playlists.Playlist, error)
	Create(ctx context.Context, request playlists.CreateRequest) (dto.UUID, error)
	Update(ctx context.Context, request playlists.UpdateRequest) error
	AddItems(ctx context.Context, playlistID dto.UUID, itemIDs []dto.UUID, position *int, user *store.User) error
	RemoveEntries(ctx context.Context, playlistID dto.UUID, entryIDs []string) error
	Move(ctx context.Context, playlistID dto.UUID, entryID string, newIndex int, user store.User) error
	SetShare(ctx context.Context, playlistID, userID dto.UUID, canEdit bool) error
	RemoveShare(ctx context.Context, playlistID, userID dto.UUID) error
	Entries(ctx context.Context, playlist playlists.Playlist) ([]item.Item, error)
	VisibleEntries(ctx context.Context, playlist playlists.Playlist, user store.User) ([]item.Item, error)
}

// RegisterPlaylists registers the Playlists API (PlaylistsController.cs).
// Every route uses the controller's [Authorize] default policy. A nil
// service registers the routes with a 503 handler for the route ledger.
func RegisterPlaylists(rt *router.Router, service PlaylistService, users ItemUserResolver, build ItemDTOBuilder, clock clock.Clock) {
	h := playlistsHandler{service: service, users: users, build: build, clock: clock}
	policy := string(auth.PolicyDefaultAuthorization)
	rt.Handle(http.MethodPost, "/Playlists", router.Route{OperationID: "CreatePlaylist", Policy: policy}, h.create)
	rt.Handle(http.MethodPost, "/Playlists/{playlistId}", router.Route{OperationID: "UpdatePlaylist", Policy: policy}, h.update)
	rt.Handle(http.MethodGet, "/Playlists/{playlistId}", router.Route{OperationID: "GetPlaylist", Policy: policy}, h.get)
	rt.Handle(http.MethodGet, "/Playlists/{playlistId}/Users", router.Route{OperationID: "GetPlaylistUsers", Policy: policy}, h.listUsers)
	rt.Handle(http.MethodGet, "/Playlists/{playlistId}/Users/{userId}", router.Route{OperationID: "GetPlaylistUser", Policy: policy}, h.user)
	rt.Handle(http.MethodPost, "/Playlists/{playlistId}/Users/{userId}", router.Route{OperationID: "UpdatePlaylistUser", Policy: policy}, h.updateUser)
	rt.Handle(http.MethodDelete, "/Playlists/{playlistId}/Users/{userId}", router.Route{OperationID: "RemoveUserFromPlaylist", Policy: policy}, h.removeUser)
	rt.Handle(http.MethodPost, "/Playlists/{playlistId}/Items", router.Route{OperationID: "AddItemToPlaylist", Policy: policy}, h.addItems)
	rt.Handle(http.MethodPost, "/Playlists/{playlistId}/Items/{itemId}/Move/{newIndex}", router.Route{OperationID: "MoveItem", Policy: policy}, h.move)
	rt.Handle(http.MethodDelete, "/Playlists/{playlistId}/Items", router.Route{OperationID: "RemoveItemFromPlaylist", Policy: policy}, h.removeItems)
	rt.Handle(http.MethodGet, "/Playlists/{playlistId}/Items", router.Route{OperationID: "GetPlaylistItems", Policy: policy}, h.items)
}

type playlistsHandler struct {
	service PlaylistService
	users   ItemUserResolver
	build   ItemDTOBuilder
	clock   clock.Clock
}

func (h playlistsHandler) ready(w http.ResponseWriter) bool {
	if h.service == nil || h.users == nil || h.build == nil || h.clock == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return false
	}
	return true
}

// callingUserID is User.GetUserId(): Guid.Empty for an API key.
func callingUserID(r *http.Request) dto.UUID {
	identity, ok := middleware.IdentityFromContext(r.Context())
	if !ok {
		return dto.NilUUID
	}
	return identity.UserID
}

// playlistForUser loads the playlist the calling user can see, writing the
// upstream 404 body when it cannot.
func (h playlistsHandler) playlistForUser(w http.ResponseWriter, r *http.Request, playlistID, userID dto.UUID) (playlists.Playlist, bool) {
	playlist, err := h.service.ForUser(r.Context(), playlistID, userID)
	if err != nil {
		playlistError(w, r, err)
		return playlists.Playlist{}, false
	}
	return playlist, true
}

func playlistError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, playlists.ErrNotFound):
		_ = render.JSON(w, r, http.StatusNotFound, "Playlist not found")
	case errors.Is(err, playlists.ErrInvalid):
		badRequest(w)
	default:
		internalError(w)
	}
}

func forbid(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) }

// create ports CreatePlaylist. Query values win over the body for name, ids,
// user, and media type; users and visibility come only from the body.
func (h playlistsHandler) create(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	var query struct {
		Name      string     `query:"name"`
		IDs       []dto.UUID `query:"ids"`
		UserID    *dto.UUID  `query:"userId"`
		MediaType string     `query:"mediaType"`
	}
	if err := bind.QueryRequest(r, &query); err != nil {
		badRequest(w)
		return
	}
	var body *dto.CreatePlaylistDto
	var decoded dto.CreatePlaylistDto
	present, err := bind.OptionalJSONBody(r, &decoded)
	if err != nil {
		badRequest(w)
		return
	}
	if present {
		body = &decoded
	}
	request := playlists.CreateRequest{Name: query.Name, ItemIDs: query.IDs}
	if len(request.ItemIDs) == 0 && body != nil {
		request.ItemIDs = body.IDs
	}
	if request.Name == "" && body != nil && body.Name != nil {
		request.Name = *body.Name
	}
	requested := query.UserID
	if requested == nil && body != nil {
		requested = body.UserID
	}
	mediaType := query.MediaType
	if mediaType == "" && body != nil && body.MediaType != nil {
		mediaType = *body.MediaType
	}
	if mediaType != "" {
		parsed, ok := parseMediaType(mediaType)
		if !ok {
			badRequest(w)
			return
		}
		request.MediaType = &parsed
	}
	if body != nil {
		for _, share := range body.Users {
			request.Users = append(request.Users, store.ItemShare{UserID: share.UserID, CanEdit: share.CanEdit})
		}
		// CreatePlaylistDto.IsPublic defaults to true when the body omits it.
		public := true
		if body.IsPublic != nil {
			public = *body.IsPublic
		}
		request.Public = &public
	}
	user, ok := resolveSelectedUser(w, r, requested, h.users)
	if !ok {
		return
	}
	request.UserID = user.ID
	id, err := h.service.Create(r.Context(), request)
	if err != nil {
		playlistError(w, r, err)
		return
	}
	_ = render.JSON(w, r, http.StatusOK, dto.PlaylistCreationResult{ID: id.String()})
}

func parseMediaType(value string) (item.MediaType, bool) {
	for _, candidate := range [...]item.MediaType{item.MediaTypeUnknown, item.MediaTypeVideo, item.MediaTypeAudio, item.MediaTypePhoto, item.MediaTypeBook} {
		if strings.EqualFold(candidate.String(), value) {
			return candidate, true
		}
	}
	if index, err := strconv.Atoi(value); err == nil && index >= 0 && index <= int(item.MediaTypeBook) {
		return item.MediaType(index), true
	}
	return item.MediaTypeUnknown, false
}

func (h playlistsHandler) update(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	var body dto.UpdatePlaylistDto
	if present, err := bind.OptionalJSONBody(r, &body); err != nil || !present {
		badRequest(w)
		return
	}
	caller := callingUserID(r)
	playlist, ok := h.playlistForUser(w, r, playlistID, caller)
	if !ok {
		return
	}
	if !playlist.CanEdit(caller) {
		forbid(w)
		return
	}
	request := playlists.UpdateRequest{ID: playlistID, UserID: caller, Name: body.Name, Public: body.IsPublic}
	if body.IDs != nil {
		ids := []dto.UUID(*body.IDs)
		request.IDs = &ids
	}
	if body.Users != nil {
		shares := make([]store.ItemShare, 0, len(*body.Users))
		for _, share := range *body.Users {
			shares = append(shares, store.ItemShare{UserID: share.UserID, CanEdit: share.CanEdit})
		}
		request.Users = &shares
	}
	if err := h.service.Update(r.Context(), request); err != nil {
		playlistError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h playlistsHandler) get(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	playlist, ok := h.playlistForUser(w, r, playlistID, callingUserID(r))
	if !ok {
		return
	}
	entries, err := h.service.Entries(r.Context(), playlist)
	if err != nil {
		internalError(w)
		return
	}
	result := dto.PlaylistDto{OpenAccess: playlist.State.OpenAccess, Shares: sharesDTO(playlist.State.Shares), ItemIDs: make([]dto.UUID, 0, len(entries))}
	for _, entry := range entries {
		result.ItemIDs = append(result.ItemIDs, entry.ID)
	}
	_ = render.JSON(w, r, http.StatusOK, result)
}

func sharesDTO(shares []store.ItemShare) []dto.PlaylistUserPermissions {
	out := make([]dto.PlaylistUserPermissions, 0, len(shares))
	for _, share := range shares {
		out = append(out, dto.PlaylistUserPermissions{UserID: share.UserID, CanEdit: share.CanEdit})
	}
	return out
}

func (h playlistsHandler) listUsers(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	caller := callingUserID(r)
	playlist, ok := h.playlistForUser(w, r, playlistID, caller)
	if !ok {
		return
	}
	if !playlist.IsOwner(caller) {
		forbid(w)
		return
	}
	_ = render.JSON(w, r, http.StatusOK, sharesDTO(playlist.State.Shares))
}

func (h playlistsHandler) user(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	userID, ok := routeUUID(w, r, "userId")
	if !ok {
		return
	}
	caller := callingUserID(r)
	playlist, ok := h.playlistForUser(w, r, playlistID, caller)
	if !ok {
		return
	}
	// The owner always receives its own edit permission, whatever userId
	// names (PlaylistsController.GetPlaylistUser).
	if playlist.IsOwner(caller) {
		_ = render.JSON(w, r, http.StatusOK, dto.PlaylistUserPermissions{UserID: caller, CanEdit: true})
		return
	}
	if !playlist.CanEdit(caller) && userID != caller {
		forbid(w)
		return
	}
	share, found := playlist.Share(userID)
	if !found {
		_ = render.JSON(w, r, http.StatusNotFound, "User permissions not found")
		return
	}
	_ = render.JSON(w, r, http.StatusOK, dto.PlaylistUserPermissions{UserID: share.UserID, CanEdit: share.CanEdit})
}

func (h playlistsHandler) updateUser(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	userID, ok := routeUUID(w, r, "userId")
	if !ok {
		return
	}
	var body dto.UpdatePlaylistUserDto
	if _, err := bind.OptionalJSONBody(r, &body); err != nil {
		badRequest(w)
		return
	}
	caller := callingUserID(r)
	playlist, ok := h.playlistForUser(w, r, playlistID, caller)
	if !ok {
		return
	}
	if !playlist.IsOwner(caller) {
		forbid(w)
		return
	}
	if err := h.service.SetShare(r.Context(), playlistID, userID, body.CanEdit != nil && *body.CanEdit); err != nil {
		playlistError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h playlistsHandler) removeUser(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	userID, ok := routeUUID(w, r, "userId")
	if !ok {
		return
	}
	caller := callingUserID(r)
	playlist, ok := h.playlistForUser(w, r, playlistID, caller)
	if !ok {
		return
	}
	if !playlist.CanEdit(caller) {
		forbid(w)
		return
	}
	if _, found := playlist.Share(userID); !found {
		_ = render.JSON(w, r, http.StatusNotFound, "User permissions not found")
		return
	}
	if err := h.service.RemoveShare(r.Context(), playlistID, userID); err != nil {
		playlistError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h playlistsHandler) addItems(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	var query struct {
		IDs      []dto.UUID `query:"ids"`
		Position *int       `query:"position"`
		UserID   *dto.UUID  `query:"userId"`
	}
	if err := bind.QueryRequest(r, &query); err != nil {
		badRequest(w)
		return
	}
	user, ok := resolveSelectedUser(w, r, query.UserID, h.users)
	if !ok {
		return
	}
	playlist, ok := h.playlistForUser(w, r, playlistID, user.ID)
	if !ok {
		return
	}
	if !playlist.CanEdit(user.ID) {
		forbid(w)
		return
	}
	if err := h.service.AddItems(r.Context(), playlistID, query.IDs, query.Position, &user); err != nil {
		playlistError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h playlistsHandler) move(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	newIndex, err := strconv.Atoi(chi.URLParam(r, "newIndex"))
	if err != nil {
		badRequest(w)
		return
	}
	caller := callingUserID(r)
	playlist, ok := h.playlistForUser(w, r, playlistID, caller)
	if !ok {
		return
	}
	if !playlist.CanEdit(caller) {
		forbid(w)
		return
	}
	user, err := h.users.Get(r.Context(), caller)
	if err != nil {
		internalError(w)
		return
	}
	if err := h.service.Move(r.Context(), playlistID, chi.URLParam(r, "itemId"), newIndex, user); err != nil {
		playlistError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h playlistsHandler) removeItems(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	var query struct {
		EntryIDs []string `query:"entryIds"`
	}
	if err := bind.QueryRequest(r, &query); err != nil {
		badRequest(w)
		return
	}
	caller := callingUserID(r)
	if !caller.IsNil() {
		playlist, ok := h.playlistForUser(w, r, playlistID, caller)
		if !ok {
			return
		}
		if !playlist.CanEdit(caller) {
			forbid(w)
			return
		}
	} else {
		identity, _ := middleware.IdentityFromContext(r.Context())
		if !identity.APIKey {
			forbid(w)
			return
		}
	}
	if err := h.service.RemoveEntries(r.Context(), playlistID, query.EntryIDs); err != nil {
		if errors.Is(err, playlists.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		playlistError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// items ports GetPlaylistItems. The userId query selects the viewer without
// the RequestHelpers.GetUserId check, as upstream does.
func (h playlistsHandler) items(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	playlistID, ok := routeUUID(w, r, "playlistId")
	if !ok {
		return
	}
	query, err := bind.ItemsQueryRequest(r, h.clock)
	if err != nil {
		badRequest(w)
		return
	}
	// ItemsQueryRequest leaves UserID unset, so read userId here.
	requested, err := bind.QueryScalar(r, "userId")
	if err != nil {
		badRequest(w)
		return
	}
	viewer := callingUserID(r)
	if requested != "" {
		id, err := dto.ParseUUID(requested)
		if err != nil {
			badRequest(w)
			return
		}
		if !id.IsNil() {
			viewer = id
		}
	}
	playlist, ok := h.playlistForUser(w, r, playlistID, viewer)
	if !ok {
		return
	}
	if !playlist.State.OpenAccess && !playlist.IsOwner(viewer) && !playlist.IsSharedWith(viewer) {
		forbid(w)
		return
	}
	user, err := h.users.Get(r.Context(), viewer)
	if err != nil {
		internalError(w)
		return
	}
	entries, err := h.service.VisibleEntries(r.Context(), playlist, user)
	if err != nil {
		internalError(w)
		return
	}
	total := len(entries)
	if query.StartIndex != nil && *query.StartIndex > 0 {
		entries = entries[min(*query.StartIndex, len(entries)):]
	}
	if query.Limit != nil && *query.Limit >= 0 && *query.Limit < len(entries) {
		entries = entries[:*query.Limit]
	}
	page, err := h.build.Build(r.Context(), entries, query.DtoOptions, &user.ID)
	if err != nil {
		internalError(w)
		return
	}
	for index := range page.Items {
		page.Items[index].PlaylistItemID = entries[index].ID.String()
	}
	page.TotalRecordCount = total
	if query.StartIndex != nil {
		page.StartIndex = *query.StartIndex
	}
	_ = render.JSON(w, r, http.StatusOK, page)
}
