// SPDX-License-Identifier: GPL-2.0-only

// Package catalog turns tuner channels and guide programmes into stored
// items: the Live TV folder, LiveTvChannel items under it and LiveTvProgram
// items under each channel. It ports the item construction of
// src/Jellyfin.LiveTv/Guide/GuideManager.cs (GetChannel, GetProgram), the ids
// of LiveTvDtoService.cs and LiveTvManager.GetInternalLiveTvFolder. The
// functions are pure; the store diff (ReconcileChildren) writes only what
// changed.
package catalog

import (
	"crypto/md5" // #nosec G501 -- compatibility identity, not security.
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

// ServiceName is DefaultLiveTvService.Name, the service of every tuner
// channel and guide programme.
const ServiceName = "Emby"

// internalVersion is LiveTvDtoService.InternalVersionNumber.
const internalVersion = "4"

// FolderName is the en-US HeaderLiveTV string. The folder id derives from
// it (LibraryManager.GetNamedView), as upstream with the default UI culture.
const FolderName = "Live TV"

// ExternalServiceProvider is the provider id key that names a channel's
// service (GuideManager.ExternalServiceTag).
const ExternalServiceProvider = "ExternalServiceId"

// EtagProvider is the provider id key that stores a programme's guide etag
// (GuideManager.EtagKey).
const EtagProvider = "ProgramEtag"

// ChannelID returns the item id of a tuner channel
// (LiveTvDtoService.GetInternalChannelId, LiveTvDtoService.cs:403-408).
func ChannelID(externalID string) dto.UUID {
	return newItemID(channelCLRType, ServiceName+externalID+internalVersion, "")
}

// ProgramID returns the item id of a guide programme
// (LiveTvDtoService.GetInternalProgramId, LiveTvDtoService.cs:424-429).
func ProgramID(externalID string) dto.UUID {
	return newItemID(programCLRType, ServiceName+externalID+internalVersion, "")
}

// Folder returns the Live TV folder: the named UserView
// <metadata>/views/livetv (LibraryManager.GetNamedView,
// LibraryManager.cs:2860-2902). programData makes the id key portable, as
// GetNewItemId does.
func Folder(internalMetadataPath, programData string, now time.Time) item.Item {
	path := filepath.Join(internalMetadataPath, "views", "livetv")
	name := FolderName
	viewType := item.CollectionTypeLiveTV
	created := now.UTC()
	return item.Item{
		ID:             newItemID(userViewCLRType, path+"_namedview_"+name, programData),
		Kind:           item.KindUserView,
		Path:           &path,
		Name:           &name,
		ForcedSortName: &name,
		DateCreated:    &created,
		DateModified:   &created,
		IsFolder:       true,
		Extra:          item.ExtraFields{UserView: &item.UserViewExtra{ViewType: &viewType}},
	}
}

func ptr[T any](value T) *T { return &value }

// stringOrNil maps a .NET null-or-value string: empty is null.
func stringOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// presentationKey is BaseItem.CreatePresentationUniqueKey, which neither
// Live TV type overrides: the id in "N" format.
func presentationKey(id dto.UUID) *string {
	return ptr(id.String())
}

// The CLR type names that LibraryManager.GetNewItemId hashes with the key.
const (
	channelCLRType  = "MediaBrowser.Controller.LiveTv.LiveTvChannel"
	programCLRType  = "MediaBrowser.Controller.LiveTv.LiveTvProgram"
	userViewCLRType = "MediaBrowser.Controller.Entities.UserView"
)

// newItemID ports LibraryManager.GetNewItemId as library.ItemID does: a key
// under programData becomes relative with backslashes, the key is
// lower-cased invariantly, and the id is the .NET Guid of
// md5(UTF-16LE(type + key)). library is not imported because it links
// libvips.
func newItemID(clrType, key, programData string) dto.UUID {
	if programData != "" && strings.HasPrefix(key, programData) {
		key = strings.TrimLeft(key[len(programData):], "/\\")
		key = strings.ReplaceAll(key, "/", "\\")
	}
	key = strings.Map(func(r rune) rune {
		if r == '\u0130' {
			return r
		}
		return unicode.ToLower(r)
	}, key)
	units := utf16.Encode([]rune(clrType + key))
	encoded := make([]byte, len(units)*2)
	for index, unit := range units {
		encoded[index*2] = byte(unit)
		encoded[index*2+1] = byte(unit >> 8)
	}
	sum := md5.Sum(encoded) // #nosec G401 -- compatibility identity, not security.
	return dto.UUID{sum[3], sum[2], sum[1], sum[0], sum[5], sum[4], sum[7], sum[6], sum[8], sum[9], sum[10], sum[11], sum[12], sum[13], sum[14], sum[15]}
}

// withSortName stores the item's computed SortName. Upstream BaseItem
// clears its cached SortName whenever GuideManager assigns Name, so the
// value follows the current name and channel number
// (BaseItem.cs:196-206, 536-553; LiveTvChannel.CreateSortName).
func withSortName(value item.Item) item.Item {
	value.SortName = nil
	if sortName, ok := item.ComputeSortName(value, item.DefaultSortOptions()); ok {
		value.SortName = &sortName
	}
	return value
}
