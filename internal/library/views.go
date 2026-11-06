// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

// UserViewItemReader supplies persisted collection-folder rows through the
// same access-aware query seam used by other item projections.
type UserViewItemReader interface {
	Query(context.Context, store.ItemQuery) (store.ItemQueryResult, error)
}

// UserReader supplies the persisted user configuration required to build views.
type UserReader interface {
	Get(context.Context, dto.UUID) (store.User, error)
}

// Views constructs a user's library views from persisted collection-folder
// items and persisted user configuration. It is safe for concurrent callers
// when its readers are safe.
type Views struct {
	items  UserViewItemReader
	users  UserReader
	liveTv LiveTvFolderSource
}

// LiveTvFolderSource supplies the Live TV folder. *livetv.Service
// implements it.
type LiveTvFolderSource interface {
	// LiveTvFolder returns the folder; ok is false while Live TV has no
	// tuner.
	LiveTvFolder(ctx context.Context) (folder item.Item, ok bool, err error)
}

// WithLiveTv adds the Live TV folder to the views of users with Live TV
// access.
func (v *Views) WithLiveTv(source LiveTvFolderSource) *Views {
	v.liveTv = source
	return v
}

// NewViews constructs the view service over the required item and user read
// seams.
func NewViews(items UserViewItemReader, users UserReader) *Views {
	return &Views{items: items, users: users}
}

// UserViewsQuery selects the views visible to one user.
type UserViewsQuery struct {
	UserID                 dto.UUID
	IncludeExternalContent bool
	IncludeHidden          bool
	PresetViews            []item.CollectionType
}

// UserViewsResult is the unpaged, ordered item input for the detailed DTO
// projection performed by the handler.
type UserViewsResult struct {
	Items            []item.Item
	TotalRecordCount int
	StartIndex       int
}

// viewItem retains source ordering state without leaking a parallel DTO model.
// Only generated UserViews use DisplayParentID to inherit an ordered concrete
// folder's position.
type viewItem struct {
	value           item.Item
	displayParentID *dto.UUID
	hiddenID        dto.UUID
}

// GroupingOption describes one folder eligible for the per-user grouping UI.
type GroupingOption struct {
	Name string   `json:"Name"`
	ID   dto.UUID `json:"Id"`
}

type userViewConfiguration struct {
	GroupedFolders  []dto.UUID `json:"GroupedFolders"`
	MyMediaExcludes []dto.UUID `json:"MyMediaExcludes"`
	OrderedViews    []dto.UUID `json:"OrderedViews"`
}
type userViewPolicy struct {
	EnableAllFolders    *bool      `json:"EnableAllFolders"`
	EnabledFolders      []dto.UUID `json:"EnabledFolders"`
	BlockedMediaFolders []dto.UUID `json:"BlockedMediaFolders"`
	EnableLiveTvAccess  *bool      `json:"EnableLiveTvAccess"`
}

// Query returns persisted collection-folder views after applying
// UserViewManager's grouping, hidden-view, preset, and explicit ordering rules.
// It loads all candidate roots once, retaining the user's access context rather
// than reconstructing API rows from topology metadata.
func (v *Views) Query(ctx context.Context, query UserViewsQuery) (UserViewsResult, error) {
	if v == nil || v.items == nil || v.users == nil {
		return UserViewsResult{}, fmt.Errorf("library views: readers are required")
	}
	user, err := v.users.Get(ctx, query.UserID)
	if err != nil {
		return UserViewsResult{}, err
	}
	configuration, err := decodeUserViewConfiguration(user.Configuration)
	if err != nil {
		return UserViewsResult{}, err
	}
	policy, err := decodeUserViewPolicy(user.Policy)
	if err != nil {
		return UserViewsResult{}, err
	}
	folders, err := v.items.Query(ctx, store.ItemQuery{
		IncludeItemTypes:       []item.Kind{item.KindCollectionFolder},
		EnableTotalRecordCount: false,
		User:                   &user,
		UserID:                 &user.ID,
	})
	if err != nil {
		return UserViewsResult{}, err
	}

	grouped := idSet(configuration.GroupedFolders)
	preset := collectionSet(query.PresetViews)
	views := make([]viewItem, 0, len(folders.Items)+2)
	movieGroup := make([]item.Item, 0)
	tvGroup := make([]item.Item, 0)
	for _, folder := range folders.Items {
		collectionType, ok := viewItemCollectionType(folder)
		if !ok || !folderAllowed(policy, folder.ID) {
			continue
		}
		if grouped[folder.ID] && eligibleForGrouping(collectionType) {
			switch collectionType {
			case item.CollectionTypeMovies:
				movieGroup = append(movieGroup, folder)
			case item.CollectionTypeTVShows:
				tvGroup = append(tvGroup, folder)
			case item.CollectionTypeUnknown: // Jellyfin's nullable/mixed collection type.
				movieGroup = append(movieGroup, folder)
				tvGroup = append(tvGroup, folder)
			}
			continue
		}
		if collectionType == item.CollectionTypePlaylists || preset[collectionType] {
			views = append(views, shadowView(folder, collectionType, user.ID))
			continue
		}
		views = append(views, folderView(folder))
	}
	views = append(views, groupedViews(movieGroup, item.CollectionTypeMovies, "Movies", user.ID, preset)...)
	views = append(views, groupedViews(tvGroup, item.CollectionTypeTVShows, "TV Shows", user.ID, preset)...)
	// UserViewManager.GetUserViews adds the Live TV folder for a user in
	// LiveTvManager.GetEnabledUsers (UserViewManager.cs:117-134).
	if query.IncludeExternalContent && v.liveTv != nil && (policy.EnableLiveTvAccess == nil || *policy.EnableLiveTvAccess) {
		folder, ok, err := v.liveTv.LiveTvFolder(ctx)
		if err != nil {
			return UserViewsResult{}, err
		}
		if ok {
			views = append(views, folderView(folder))
		}
	}
	if !query.IncludeHidden {
		excluded := idSet(configuration.MyMediaExcludes)
		visible := views[:0]
		for _, view := range views {
			if !excluded[view.hiddenID] {
				visible = append(visible, view)
			}
		}
		views = visible
	}
	orderViews(views, configuration.OrderedViews)
	items := make([]item.Item, len(views))
	for index := range views {
		items[index] = views[index].value
	}
	return UserViewsResult{Items: items, TotalRecordCount: len(items)}, nil
}

// GroupingOptions returns eligible persisted collection folders in stable
// display-name order. Its identities intentionally remain CollectionFolder IDs,
// distinct from generated UserView identities.
func (v *Views) GroupingOptions(ctx context.Context, userID dto.UUID) ([]GroupingOption, error) {
	if v == nil || v.items == nil || v.users == nil {
		return nil, fmt.Errorf("library views: readers are required")
	}
	user, err := v.users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	policy, err := decodeUserViewPolicy(user.Policy)
	if err != nil {
		return nil, err
	}
	folders, err := v.items.Query(ctx, store.ItemQuery{
		IncludeItemTypes:       []item.Kind{item.KindCollectionFolder},
		EnableTotalRecordCount: false,
		User:                   &user,
		UserID:                 &user.ID,
	})
	if err != nil {
		return nil, err
	}
	options := make([]GroupingOption, 0, len(folders.Items))
	for _, folder := range folders.Items {
		collectionType, ok := viewItemCollectionType(folder)
		if folderAllowed(policy, folder.ID) && ok && eligibleForGrouping(collectionType) {
			options = append(options, GroupingOption{Name: viewItemName(folder), ID: folder.ID})
		}
	}
	sort.SliceStable(options, func(i, j int) bool {
		if options[i].Name != options[j].Name {
			return options[i].Name < options[j].Name
		}
		return options[i].ID.String() < options[j].ID.String()
	})
	return options, nil
}

func decodeUserViewConfiguration(raw []byte) (userViewConfiguration, error) {
	if len(raw) == 0 {
		return userViewConfiguration{}, nil
	}
	var configuration userViewConfiguration
	if err := json.Unmarshal(raw, &configuration); err != nil {
		return userViewConfiguration{}, fmt.Errorf("library views: decode user configuration: %w", err)
	}
	return configuration, nil
}

func decodeUserViewPolicy(raw []byte) (userViewPolicy, error) {
	if len(raw) == 0 {
		return userViewPolicy{}, nil
	}
	var policy userViewPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return userViewPolicy{}, fmt.Errorf("library views: decode user policy: %w", err)
	}
	return policy, nil
}

func folderAllowed(policy userViewPolicy, folderID dto.UUID) bool {
	if len(policy.BlockedMediaFolders) != 0 {
		for _, blocked := range policy.BlockedMediaFolders {
			if blocked == folderID {
				return false
			}
		}
		return true
	}
	if policy.EnableAllFolders == nil || *policy.EnableAllFolders {
		return true
	}
	for _, enabled := range policy.EnabledFolders {
		if enabled == folderID {
			return true
		}
	}
	return false
}

func idSet(ids []dto.UUID) map[dto.UUID]bool {
	set := make(map[dto.UUID]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

func collectionSet(types []item.CollectionType) map[item.CollectionType]bool {
	set := make(map[item.CollectionType]bool, len(types))
	for _, typ := range types {
		set[typ] = true
	}
	return set
}

func eligibleForGrouping(typ item.CollectionType) bool {
	return typ == item.CollectionTypeMovies || typ == item.CollectionTypeTVShows || typ == item.CollectionTypeUnknown
}

func folderView(folder item.Item) viewItem {
	return viewItem{value: folder, hiddenID: folder.ID}
}

func shadowView(folder item.Item, typ item.CollectionType, userID dto.UUID) viewItem {
	parentID := folder.ID
	viewType := typ
	viewUserID := userID
	value := folder
	value.ID = sourceUserViewID("38_namedview_" + viewItemName(folder) + folder.ID.Dashed() + typ.String())
	value.Kind = item.KindUserView
	value.IsFolder = true
	value.Extra.CollectionFolder = nil
	value.Extra.UserView = &item.UserViewExtra{ViewType: &viewType, DisplayParentID: parentID, UserID: &viewUserID}
	return viewItem{value: value, displayParentID: &parentID, hiddenID: folder.ID}
}

func groupedViews(folders []item.Item, typ item.CollectionType, name string, userID dto.UUID, preset map[item.CollectionType]bool) []viewItem {
	if len(folders) == 0 {
		return nil
	}
	if len(folders) == 1 && viewItemCollectionTypeIs(folders[0], typ) {
		if preset[typ] {
			return []viewItem{shadowView(folders[0], typ, userID)}
		}
		return []viewItem{folderView(folders[0])}
	}
	viewType := typ
	viewUserID := userID
	parentID, path := groupedViewRoot(folders)
	value := item.Item{
		ID:       sourceUserViewID("38_namedview_" + name + userID.String() + typ.String()),
		Kind:     item.KindUserView,
		ParentID: parentID,
		Path:     path,
		Name:     &name,
		SortName: &name,
		IsFolder: true,
		Extra: item.ExtraFields{UserView: &item.UserViewExtra{
			ViewType: &viewType,
			UserID:   &viewUserID,
		}},
	}
	return []viewItem{{value: value, hiddenID: value.ID}}
}

func groupedViewRoot(folders []item.Item) (*dto.UUID, *string) {
	var parentID *dto.UUID
	var path *string
	parentSet, parentConsistent := false, true
	pathSet, pathConsistent := false, true
	for index := range folders {
		folder := &folders[index]
		if !parentSet {
			parentSet = true
			if folder.ParentID != nil {
				value := *folder.ParentID
				parentID = &value
			}
		} else if parentConsistent && ((parentID == nil) != (folder.ParentID == nil) || (parentID != nil && *parentID != *folder.ParentID)) {
			parentConsistent = false
			parentID = nil
		}
		if !pathSet {
			pathSet = true
			if folder.Path != nil {
				value := filepath.Dir(*folder.Path)
				path = &value
			}
		} else if pathConsistent {
			candidate := ""
			if folder.Path != nil {
				candidate = filepath.Dir(*folder.Path)
			}
			if (path == nil) != (folder.Path == nil) || (path != nil && *path != candidate) {
				pathConsistent = false
				path = nil
			}
		}
	}
	return parentID, path
}

func orderViews(views []viewItem, ordered []dto.UUID) {
	positions := make(map[dto.UUID]int, len(ordered))
	for index, id := range ordered {
		if _, exists := positions[id]; !exists {
			positions[id] = index
		}
	}
	sort.SliceStable(views, func(i, j int) bool {
		left, leftOrdered := viewOrder(views[i], positions)
		right, rightOrdered := viewOrder(views[j], positions)
		if leftOrdered != rightOrdered {
			return leftOrdered
		}
		if leftOrdered && left != right {
			return left < right
		}
		leftName, rightName := viewItemName(views[i].value), viewItemName(views[j].value)
		if leftName != rightName {
			return leftName < rightName
		}
		return views[i].value.ID.String() < views[j].value.ID.String()
	})
}

func viewOrder(view viewItem, positions map[dto.UUID]int) (int, bool) {
	if position, ok := positions[view.value.ID]; ok {
		return position, true
	}
	if view.displayParentID != nil {
		position, ok := positions[*view.displayParentID]
		return position, ok
	}
	return 0, false
}

func viewItemCollectionType(value item.Item) (item.CollectionType, bool) {
	if value.Extra.CollectionFolder != nil && value.Extra.CollectionFolder.CollectionType != nil {
		return *value.Extra.CollectionFolder.CollectionType, true
	}
	if value.Extra.UserView != nil && value.Extra.UserView.ViewType != nil {
		return *value.Extra.UserView.ViewType, true
	}
	return item.CollectionTypeUnknown, false
}

func viewItemCollectionTypeIs(value item.Item, want item.CollectionType) bool {
	got, ok := viewItemCollectionType(value)
	return ok && got == want
}

func viewItemName(value item.Item) string {
	if value.Name == nil {
		return ""
	}
	return *value.Name
}

// sourceUserViewID reproduces LibraryManager.GetNewItemId for UserView keys.
func sourceUserViewID(key string) dto.UUID {
	return sourceEntityID("MediaBrowser.Controller.Entities.UserView", strings.ToLower(key))
}

// sourceEntityID reproduces string.GetMD5() as a .NET Guid: UTF-16LE MD5,
// then Guid's mixed-endian representation rendered in N format.
func sourceEntityID(typeName, key string) dto.UUID {
	runes := utf16.Encode([]rune(typeName + key))
	encoded := make([]byte, len(runes)*2)
	for index, value := range runes {
		binary.LittleEndian.PutUint16(encoded[index*2:], value)
	}
	digest := md5.Sum(encoded)
	var text [32]byte
	hex.Encode(text[:], []byte{digest[3], digest[2], digest[1], digest[0], digest[5], digest[4], digest[7], digest[6], digest[8], digest[9], digest[10], digest[11], digest[12], digest[13], digest[14], digest[15]})
	id, err := dto.ParseUUID(string(text[:]))
	if err != nil {
		panic("library views: deterministic entity ID")
	}
	return id
}
