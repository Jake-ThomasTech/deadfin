// SPDX-License-Identifier: GPL-2.0-only

// Package playlists ports Jellyfin's PlaylistManager
// (Emby.Server.Implementations/Playlists/PlaylistManager.cs) and the Playlist
// visibility rules (MediaBrowser.Controller/Playlists/Playlist.cs).
//
// A playlist is a Playlist item under the PlaylistsFolder. Its directory
// exists under <data>/playlists as upstream creates it; its owner, shares,
// open access, and ordered entries persist in the store.
package playlists

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	dfpath "github.com/deadfin-org/deadfin/internal/path"
	"github.com/deadfin-org/deadfin/internal/store"
)

var (
	// ErrNotFound reports a playlist that does not exist or that the user
	// cannot see (GetPlaylistForUser returned null).
	ErrNotFound = errors.New("playlist not found")
	// ErrInvalid reports a request upstream rejects with ArgumentException.
	ErrInvalid = errors.New("invalid playlist request")
)

// Items is the item store seam.
type Items interface {
	Get(context.Context, dto.UUID) (item.Item, error)
	Query(context.Context, store.ItemQuery) (store.ItemQueryResult, error)
	CreatePlaylist(context.Context, item.Item, []store.ItemShare) error
	Playlist(context.Context, dto.UUID) (store.PlaylistState, error)
	MutatePlaylist(context.Context, dto.UUID, func(*store.PlaylistState) error) (store.PlaylistState, error)
}

// Users loads persisted users.
type Users interface {
	Get(context.Context, dto.UUID) (store.User, error)
}

// Options configures a Service.
type Options struct {
	Items Items
	Users Users
	Clock clock.Clock
	// ProgramData is the program data root. Item identities under it use the
	// portable relative key (LibraryManager.GetNewItemId).
	ProgramData string
	// DataPath is the application data path. A playlist under it is a shared
	// item whose visibility is owner, shares, and open access
	// (Playlist.IsSharedItem).
	DataPath string
}

// Service is the playlist manager.
type Service struct {
	items       Items
	users       Users
	clock       clock.Clock
	programData string
	dataPath    string
}

// New returns a Service.
func New(options Options) (*Service, error) {
	if options.Items == nil || options.Users == nil || options.Clock == nil {
		return nil, errors.New("playlists: items, users, and clock are required")
	}
	if options.DataPath == "" {
		return nil, errors.New("playlists: data path is required")
	}
	return &Service{items: options.Items, users: options.Users, clock: options.Clock, programData: options.ProgramData, dataPath: options.DataPath}, nil
}

// Playlist is one playlist as a user sees it.
type Playlist struct {
	Item  item.Item
	State store.PlaylistState
}

// IsOwner reports whether userID owns the playlist.
func (p Playlist) IsOwner(userID dto.UUID) bool { return p.State.OwnerUserID == userID }

// CanEdit reports whether userID owns the playlist or holds an editing share.
func (p Playlist) CanEdit(userID dto.UUID) bool {
	if p.IsOwner(userID) {
		return true
	}
	for _, share := range p.State.Shares {
		if share.CanEdit && share.UserID == userID {
			return true
		}
	}
	return false
}

// IsSharedWith reports whether userID holds any share.
func (p Playlist) IsSharedWith(userID dto.UUID) bool {
	for _, share := range p.State.Shares {
		if share.UserID == userID {
			return true
		}
	}
	return false
}

// Share returns userID's share.
func (p Playlist) Share(userID dto.UUID) (store.ItemShare, bool) {
	for _, share := range p.State.Shares {
		if share.UserID == userID {
			return share, true
		}
	}
	return store.ItemShare{}, false
}

// CreateRequest is PlaylistCreationRequest.
type CreateRequest struct {
	Name      string
	ItemIDs   []dto.UUID
	UserID    dto.UUID
	MediaType *item.MediaType
	Users     []store.ItemShare
	Public    *bool
}

// UpdateRequest is PlaylistUpdateRequest. Nil members are unchanged.
type UpdateRequest struct {
	ID     dto.UUID
	UserID dto.UUID
	Name   *string
	IDs    *[]dto.UUID
	Users  *[]store.ItemShare
	Public *bool
}

// ForUser ports GetPlaylistForUser: the playlist when it exists and userID
// can see it, else ErrNotFound.
func (s *Service) ForUser(ctx context.Context, playlistID, userID dto.UUID) (Playlist, error) {
	if userID.IsNil() {
		return Playlist{}, ErrNotFound
	}
	user, err := s.users.Get(ctx, userID)
	if err != nil {
		if store.IsNotFound(err) {
			return Playlist{}, ErrNotFound
		}
		return Playlist{}, err
	}
	playlist, err := s.load(ctx, playlistID)
	if err != nil {
		return Playlist{}, err
	}
	visible, err := s.visible(ctx, playlist, user)
	if err != nil {
		return Playlist{}, err
	}
	if !visible {
		return Playlist{}, ErrNotFound
	}
	return playlist, nil
}

// Exists returns the playlist without a user visibility check. It serves
// RemoveItemFromPlaylist for API keys, which upstream does not scope to a user.
func (s *Service) Exists(ctx context.Context, playlistID dto.UUID) (Playlist, error) {
	return s.load(ctx, playlistID)
}

func (s *Service) load(ctx context.Context, playlistID dto.UUID) (Playlist, error) {
	value, err := s.items.Get(ctx, playlistID)
	if err != nil {
		if store.IsNotFound(err) {
			return Playlist{}, ErrNotFound
		}
		return Playlist{}, err
	}
	if value.Kind != item.KindPlaylist {
		return Playlist{}, ErrNotFound
	}
	state, err := s.items.Playlist(ctx, playlistID)
	if err != nil {
		if store.IsNotFound(err) {
			return Playlist{}, ErrNotFound
		}
		return Playlist{}, err
	}
	return Playlist{Item: value, State: state}, nil
}

// visible ports Playlist.IsVisible. A playlist under the data path is a
// shared item: open access, the owner, and shared users see it. Any other
// playlist (a scanned .m3u) follows the library visibility rules.
func (s *Service) visible(ctx context.Context, playlist Playlist, user store.User) (bool, error) {
	if s.isSharedItem(playlist.Item) {
		return playlist.State.OpenAccess || playlist.IsOwner(user.ID) || playlist.IsSharedWith(user.ID), nil
	}
	ids, err := s.visibleIDs(ctx, []dto.UUID{playlist.Item.ID}, &user)
	if err != nil {
		return false, err
	}
	return ids[playlist.Item.ID], nil
}

func (s *Service) isSharedItem(value item.Item) bool {
	if value.Path == nil || *value.Path == "" {
		return false
	}
	return containsSubPath(s.dataPath, *value.Path)
}

func containsSubPath(parent, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(candidate))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Create ports CreatePlaylist. It returns the new playlist id.
func (s *Service) Create(ctx context.Context, request CreateRequest) (dto.UUID, error) {
	if strings.TrimSpace(request.Name) == "" {
		return dto.NilUUID, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if request.UserID.IsNil() {
		return dto.NilUUID, fmt.Errorf("%w: user is required", ErrInvalid)
	}
	user, err := s.users.Get(ctx, request.UserID)
	if err != nil {
		if store.IsNotFound(err) {
			return dto.NilUUID, fmt.Errorf("%w: user not found", ErrInvalid)
		}
		return dto.NilUUID, err
	}
	folder, err := s.playlistsFolder(ctx)
	if err != nil {
		return dto.NilUUID, err
	}
	mediaType, err := s.inferMediaType(ctx, request)
	if err != nil {
		return dto.NilUUID, err
	}

	// GetTargetPath appends "1" until the name is free. Path.Combine does
	// not normalize, so "." and ".." name existing directories there and
	// take the suffix too; the playlist never leaves the playlists folder.
	folderName := dfpath.ValidFilename(request.Name)
	var directory string
	for {
		if folderName == "." || folderName == ".." {
			folderName += "1"
			continue
		}
		candidate, ok := dfpath.JoinLocal(*folder.Path, folderName)
		if !ok {
			return dto.NilUUID, fmt.Errorf("%w: invalid playlist name", ErrInvalid)
		}
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			directory = candidate
			break
		} else if err != nil {
			return dto.NilUUID, fmt.Errorf("playlists: inspect %s: %w", candidate, err)
		}
		folderName += "1"
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return dto.NilUUID, fmt.Errorf("playlists: create %s: %w", directory, err)
	}
	created := s.clock.Now().UTC()
	if info, err := os.Stat(directory); err == nil {
		created = info.ModTime().UTC()
	}

	name := request.Name
	id := library.ItemID(item.KindPlaylist, directory, s.programData)
	parentID := folder.ID
	topParentID := folder.ID
	if folder.TopParentID != nil {
		topParentID = *folder.TopParentID
	}
	value := item.Item{
		ID:                  id,
		Kind:                item.KindPlaylist,
		ParentID:            &parentID,
		TopParentID:         &topParentID,
		PlaylistOwnerUserID: request.UserID,
		Path:                &directory,
		Name:                &name,
		DateCreated:         &created,
		DateModified:        &created,
		MediaType:           &mediaType,
		IsFolder:            true,
		OpenAccess:          request.Public != nil && *request.Public,
	}
	if sortName, ok := item.ComputeSortName(value, item.DefaultSortOptions()); ok {
		value.SortName = &sortName
	}
	if keys := item.UserDataKeys(value, item.UserDataInputs{}); len(keys) != 0 {
		value.PresentationUniqueKey = &keys[0]
	}
	if err := s.items.CreatePlaylist(ctx, value, request.Users); err != nil {
		return dto.NilUUID, err
	}
	if len(request.ItemIDs) > 0 {
		if err := s.addInternal(ctx, id, request.ItemIDs, &user, nil); err != nil {
			return dto.NilUUID, err
		}
	}
	return id, nil
}

// playlistsFolder ports GetPlaylistsFolder.
func (s *Service) playlistsFolder(ctx context.Context) (item.Item, error) {
	result, err := s.items.Query(ctx, store.ItemQuery{IncludeItemTypes: []item.Kind{item.KindPlaylistsFolder}})
	if err != nil {
		return item.Item{}, err
	}
	for _, folder := range result.Items {
		if folder.Path != nil && *folder.Path != "" {
			return folder, nil
		}
	}
	return item.Item{}, fmt.Errorf("%w: playlists folder is missing", ErrInvalid)
}

// inferMediaType ports the media-type selection in CreatePlaylist.
func (s *Service) inferMediaType(ctx context.Context, request CreateRequest) (item.MediaType, error) {
	if request.MediaType != nil && *request.MediaType != item.MediaTypeUnknown {
		return *request.MediaType, nil
	}
	for _, id := range request.ItemIDs {
		value, err := s.items.Get(ctx, id)
		if err != nil {
			if store.IsNotFound(err) {
				return item.MediaTypeUnknown, fmt.Errorf("%w: no item exists with the supplied id", ErrInvalid)
			}
			return item.MediaTypeUnknown, err
		}
		mediaType := item.MediaTypeOf(value)
		switch {
		case mediaType != item.MediaTypeUnknown:
		case value.Kind == item.KindMusicArtist || value.Kind == item.KindMusicAlbum || value.Kind == item.KindMusicGenre:
			mediaType = item.MediaTypeAudio
		case value.Kind == item.KindGenre:
			mediaType = item.MediaTypeVideo
		case item.IsFolder(value):
			folder := false
			result, err := s.items.Query(ctx, store.ItemQuery{ParentID: &value.ID, Recursive: true, IsFolder: &folder, MediaTypes: []item.MediaType{item.MediaTypeAudio, item.MediaTypeVideo}})
			if err != nil {
				return item.MediaTypeUnknown, err
			}
			for _, child := range result.Items {
				if supportsAddingToPlaylist(child) {
					if childType := item.MediaTypeOf(child); childType != item.MediaTypeUnknown {
						mediaType = childType
						break
					}
				}
			}
		}
		if mediaType != item.MediaTypeUnknown {
			return mediaType, nil
		}
	}
	return item.MediaTypeAudio, nil
}

// AddItems ports AddItemToPlaylistAsync. user nil means an API key without a
// user, which skips the visibility filter.
func (s *Service) AddItems(ctx context.Context, playlistID dto.UUID, itemIDs []dto.UUID, position *int, user *store.User) error {
	return s.addInternal(ctx, playlistID, itemIDs, user, position)
}

// addInternal ports AddToPlaylistInternal: it expands the requested items,
// keeps those that support playlists, drops entries already present, and
// inserts the rest at position.
func (s *Service) addInternal(ctx context.Context, playlistID dto.UUID, itemIDs []dto.UUID, user *store.User, position *int) error {
	expanded, err := s.playlistItems(ctx, itemIDs, user)
	if err != nil {
		return err
	}
	_, err = s.items.MutatePlaylist(ctx, playlistID, func(state *store.PlaylistState) error {
		existing := make(map[dto.UUID]bool, len(state.Children))
		for _, child := range state.Children {
			existing[child] = true
		}
		add := make([]dto.UUID, 0, len(expanded))
		for _, value := range expanded {
			if !supportsAddingToPlaylist(value) || existing[value.ID] {
				continue
			}
			existing[value.ID] = true
			add = append(add, value.ID)
		}
		if len(add) == 0 {
			return errNoChange
		}
		state.Children = insertAt(state.Children, add, position)
		now := s.clock.Now().UTC()
		state.DateLastMediaAdded = &now
		return nil
	})
	if errors.Is(err, errNoChange) {
		return nil
	}
	if errors.Is(err, store.ErrNotPlaylist) {
		return fmt.Errorf("%w: no playlist exists with id %s", ErrInvalid, playlistID)
	}
	return err
}

// errNoChange aborts a MutatePlaylist transaction that has nothing to write.
var errNoChange = errors.New("playlist unchanged")

func insertAt(children, add []dto.UUID, position *int) []dto.UUID {
	out := make([]dto.UUID, 0, len(children)+len(add))
	switch {
	case position == nil || *position >= len(children):
		out = append(append(out, children...), add...)
	case *position <= 0:
		out = append(append(out, add...), children...)
	default:
		out = append(append(append(out, children[:*position]...), add...), children[*position:]...)
	}
	return out
}

// playlistItems ports Playlist.GetPlaylistItems: requested items in request
// order, visible to user, with genres, artists, and folders expanded to
// their playable descendants.
func (s *Service) playlistItems(ctx context.Context, itemIDs []dto.UUID, user *store.User) ([]item.Item, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	query := store.ItemQuery{ItemIDs: itemIDs}
	applyUser(&query, user)
	result, err := s.items.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	byID := make(map[dto.UUID]item.Item, len(result.Items))
	for _, value := range result.Items {
		byID[value.ID] = value
	}
	var out []item.Item
	for _, id := range itemIDs {
		value, ok := byID[id]
		if !ok {
			continue
		}
		expanded, err := s.expand(ctx, value, user)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded...)
	}
	return out, nil
}

func (s *Service) expand(ctx context.Context, value item.Item, user *store.User) ([]item.Item, error) {
	byArtistOrder := []store.ItemOrder{
		{SortBy: store.ItemSortByAlbumArtist, SortOrder: store.SortOrderAscending},
		{SortBy: store.ItemSortByAlbum, SortOrder: store.SortOrderAscending},
		{SortBy: store.ItemSortBySortName, SortOrder: store.SortOrderAscending},
	}
	var query store.ItemQuery
	switch {
	case value.Kind == item.KindMusicGenre:
		query = store.ItemQuery{Recursive: true, IncludeItemTypes: []item.Kind{item.KindAudio}, GenreIDs: []dto.UUID{value.ID}, OrderBy: byArtistOrder}
	case value.Kind == item.KindMusicArtist:
		query = store.ItemQuery{Recursive: true, IncludeItemTypes: []item.Kind{item.KindAudio}, ArtistIDs: []dto.UUID{value.ID}, OrderBy: byArtistOrder}
	case item.IsFolder(value):
		folder := false
		id := value.ID
		query = store.ItemQuery{ParentID: &id, Recursive: true, IsFolder: &folder, MediaTypes: []item.MediaType{item.MediaTypeAudio, item.MediaTypeVideo}}
	default:
		return []item.Item{value}, nil
	}
	applyUser(&query, user)
	result, err := s.items.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return result.Items, nil
}

func applyUser(query *store.ItemQuery, user *store.User) {
	if user == nil {
		return
	}
	query.User = user
	query.UserID = &user.ID
}

// supportsAddingToPlaylist ports BaseItem.SupportsAddingToPlaylist after
// expansion: Video and Audio subtypes (Video.cs:150, Audio.cs:49), and the
// Series, Season, and music containers that expansion replaces. A Live TV
// channel is accepted too, so a playlist can hold a curated channel list;
// pinned LiveTvChannel keeps the BaseItem default false (D-94).
func supportsAddingToPlaylist(value item.Item) bool {
	switch value.Kind {
	case item.KindMovie, item.KindEpisode, item.KindVideo, item.KindMusicVideo, item.KindTrailer,
		item.KindAudio, item.KindAudioBook,
		item.KindSeries, item.KindSeason, item.KindMusicAlbum, item.KindMusicArtist, item.KindMusicGenre,
		item.KindLiveTvChannel:
		return true
	default:
		return false
	}
}

// Entries returns the playlist's resolvable entries in order
// (Playlist.GetManageableItems): entries whose item still exists.
func (s *Service) Entries(ctx context.Context, playlist Playlist) ([]item.Item, error) {
	return s.resolve(ctx, playlist.State.Children, nil)
}

// VisibleEntries returns the entries user can see, in playlist order.
func (s *Service) VisibleEntries(ctx context.Context, playlist Playlist, user store.User) ([]item.Item, error) {
	return s.resolve(ctx, playlist.State.Children, &user)
}

func (s *Service) resolve(ctx context.Context, ids []dto.UUID, user *store.User) ([]item.Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query := store.ItemQuery{ItemIDs: ids}
	applyUser(&query, user)
	result, err := s.items.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	byID := make(map[dto.UUID]item.Item, len(result.Items))
	for _, value := range result.Items {
		byID[value.ID] = value
	}
	out := make([]item.Item, 0, len(ids))
	for _, id := range ids {
		if value, ok := byID[id]; ok {
			out = append(out, value)
		}
	}
	return out, nil
}

func (s *Service) visibleIDs(ctx context.Context, ids []dto.UUID, user *store.User) (map[dto.UUID]bool, error) {
	values, err := s.resolve(ctx, ids, user)
	if err != nil {
		return nil, err
	}
	out := make(map[dto.UUID]bool, len(values))
	for _, value := range values {
		out[value.ID] = true
	}
	return out, nil
}

// RemoveEntries ports RemoveItemFromPlaylistAsync. An entry id is the
// N-format id of the entry's item; unknown entry ids are ignored.
func (s *Service) RemoveEntries(ctx context.Context, playlistID dto.UUID, entryIDs []string) error {
	remove := make(map[dto.UUID]bool, len(entryIDs))
	for _, entry := range entryIDs {
		if id, err := dto.ParseUUID(entry); err == nil {
			remove[id] = true
		}
	}
	playlist, err := s.load(ctx, playlistID)
	if err != nil {
		return err
	}
	// GetManageableItems drops entries whose item no longer exists, so the
	// rewrite removes them too.
	resolvable, err := s.Entries(ctx, playlist)
	if err != nil {
		return err
	}
	_, err = s.items.MutatePlaylist(ctx, playlistID, func(state *store.PlaylistState) error {
		keep := make([]dto.UUID, 0, len(resolvable))
		for _, value := range resolvable {
			if !remove[value.ID] {
				keep = append(keep, value.ID)
			}
		}
		state.Children = keep
		return nil
	})
	if errors.Is(err, store.ErrNotPlaylist) {
		return ErrNotFound
	}
	return err
}

// Move ports MoveItemAsync, including its index adjustment between the
// entries user can see and all entries.
func (s *Service) Move(ctx context.Context, playlistID dto.UUID, entryID string, newIndex int, user store.User) error {
	playlist, err := s.load(ctx, playlistID)
	if err != nil {
		return err
	}
	children, err := s.Entries(ctx, playlist)
	if err != nil {
		return err
	}
	visible, err := s.visibleIDs(ctx, playlist.State.Children, &user)
	if err != nil {
		return err
	}
	entry, err := dto.ParseUUID(entryID)
	if err != nil {
		return nil
	}
	accessible := make([]dto.UUID, 0, len(children))
	for _, child := range children {
		if visible[child.ID] {
			accessible = append(accessible, child.ID)
		}
	}
	if indexOf(accessible, entry) == newIndex {
		return nil
	}
	if len(accessible) == 0 {
		return nil
	}
	priorIndex := max(newIndex-1, 0)
	priorIndex = min(priorIndex, len(accessible)-1)
	prior := accessible[priorIndex]
	all := make([]dto.UUID, len(children))
	for index, child := range children {
		all[index] = child.ID
	}
	adjusted := adjustedIndex(indexOf(all, prior), newIndex)
	_, err = s.items.MutatePlaylist(ctx, playlistID, func(state *store.PlaylistState) error {
		at := indexOf(state.Children, entry)
		if at < 0 {
			return errNoChange
		}
		list := append(append([]dto.UUID{}, state.Children[:at]...), state.Children[at+1:]...)
		if newIndex >= len(list) || adjusted >= len(list) {
			list = append(list, entry)
		} else {
			list = append(list[:adjusted], append([]dto.UUID{entry}, list[adjusted:]...)...)
		}
		state.Children = list
		return nil
	})
	if errors.Is(err, errNoChange) {
		return nil
	}
	if errors.Is(err, store.ErrNotPlaylist) {
		return ErrNotFound
	}
	return err
}

// adjustedIndex ports PlaylistManager.DetermineAdjustedIndex.
func adjustedIndex(priorIndexAll, newIndex int) int {
	if newIndex == 0 {
		if priorIndexAll > 0 {
			return priorIndexAll - 1
		}
		return 0
	}
	return priorIndexAll + 1
}

func indexOf(values []dto.UUID, target dto.UUID) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

// Update ports UpdatePlaylist.
func (s *Service) Update(ctx context.Context, request UpdateRequest) error {
	if request.IDs != nil {
		user, err := s.users.Get(ctx, request.UserID)
		if err != nil {
			return err
		}
		if _, err := s.items.MutatePlaylist(ctx, request.ID, func(state *store.PlaylistState) error {
			state.Children = nil
			return nil
		}); err != nil {
			return err
		}
		if err := s.addInternal(ctx, request.ID, *request.IDs, &user, nil); err != nil {
			return err
		}
	}
	_, err := s.items.MutatePlaylist(ctx, request.ID, func(state *store.PlaylistState) error {
		if request.Name != nil {
			state.Name = *request.Name
			state.SortName = nil
			named := item.Item{Kind: item.KindPlaylist, Name: request.Name}
			if sortName, ok := item.ComputeSortName(named, item.DefaultSortOptions()); ok {
				state.SortName = &sortName
			}
		}
		if request.Users != nil {
			state.Shares = append([]store.ItemShare(nil), (*request.Users)...)
		}
		if request.Public != nil {
			state.OpenAccess = *request.Public
		}
		return nil
	})
	return err
}

// SetShare ports AddUserToShares: it replaces userID's share. Upstream looks
// the playlist up as the target user and fails when that user cannot see it
// yet; Deadfin adds the share (divergence D-42).
func (s *Service) SetShare(ctx context.Context, playlistID, userID dto.UUID, canEdit bool) error {
	_, err := s.items.MutatePlaylist(ctx, playlistID, func(state *store.PlaylistState) error {
		shares := make([]store.ItemShare, 0, len(state.Shares)+1)
		for _, share := range state.Shares {
			if share.UserID != userID {
				shares = append(shares, share)
			}
		}
		state.Shares = append(shares, store.ItemShare{UserID: userID, CanEdit: canEdit})
		return nil
	})
	if errors.Is(err, store.ErrNotPlaylist) {
		return ErrNotFound
	}
	return err
}

// RemoveShare ports RemoveUserFromShares.
func (s *Service) RemoveShare(ctx context.Context, playlistID, userID dto.UUID) error {
	_, err := s.items.MutatePlaylist(ctx, playlistID, func(state *store.PlaylistState) error {
		shares := state.Shares[:0]
		for _, share := range state.Shares {
			if share.UserID != userID {
				shares = append(shares, share)
			}
		}
		state.Shares = shares
		return nil
	})
	if errors.Is(err, store.ErrNotPlaylist) {
		return ErrNotFound
	}
	return err
}

// ImportRequest is one playlist carried over from Jellyfin.
type ImportRequest struct {
	Name        string
	OwnerUserID dto.UUID
	Shares      []store.ItemShare
	OpenAccess  bool
	MediaType   item.MediaType
	Children    []dto.UUID
	DateCreated *time.Time
}

// Import creates a playlist from Jellyfin state without expanding or
// filtering its entries. It returns false when a playlist of that name
// already exists, so the importer can be rerun.
func (s *Service) Import(ctx context.Context, request ImportRequest) (bool, error) {
	if strings.TrimSpace(request.Name) == "" || request.OwnerUserID.IsNil() {
		return false, fmt.Errorf("%w: name and owner are required", ErrInvalid)
	}
	folder, err := s.playlistsFolder(ctx)
	if err != nil {
		return false, err
	}
	directory := filepath.Join(*folder.Path, dfpath.ValidFilename(request.Name))
	id := library.ItemID(item.KindPlaylist, directory, s.programData)
	if _, err := s.items.Get(ctx, id); err == nil {
		return false, nil
	} else if !store.IsNotFound(err) {
		return false, err
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return false, fmt.Errorf("playlists: create %s: %w", directory, err)
	}
	created := s.clock.Now().UTC()
	if request.DateCreated != nil {
		created = request.DateCreated.UTC()
	}
	name := request.Name
	mediaType := request.MediaType
	if mediaType == item.MediaTypeUnknown {
		mediaType = item.MediaTypeAudio
	}
	parentID, topParentID := folder.ID, folder.ID
	if folder.TopParentID != nil {
		topParentID = *folder.TopParentID
	}
	value := item.Item{
		ID: id, Kind: item.KindPlaylist, ParentID: &parentID, TopParentID: &topParentID,
		PlaylistOwnerUserID: request.OwnerUserID, Path: &directory, Name: &name,
		DateCreated: &created, DateModified: &created, MediaType: &mediaType,
		IsFolder: true, OpenAccess: request.OpenAccess,
	}
	if sortName, ok := item.ComputeSortName(value, item.DefaultSortOptions()); ok {
		value.SortName = &sortName
	}
	if keys := item.UserDataKeys(value, item.UserDataInputs{}); len(keys) != 0 {
		value.PresentationUniqueKey = &keys[0]
	}
	seen := make(map[dto.UUID]bool, len(request.Children))
	for _, child := range request.Children {
		if child.IsNil() || seen[child] {
			continue
		}
		seen[child] = true
		order := len(value.LinkedChildren)
		value.LinkedChildren = append(value.LinkedChildren, item.LinkedChildRef{ItemID: child, Type: item.LinkedChildTypeManual, SortOrder: &order})
	}
	if err := s.items.CreatePlaylist(ctx, value, request.Shares); err != nil {
		return false, err
	}
	return true, nil
}
