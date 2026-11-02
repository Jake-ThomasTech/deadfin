// SPDX-License-Identifier: GPL-2.0-only

package jfimport

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/jellyfinsource"
)

// Source is the Jellyfin side of the import.
type Source interface {
	Users(context.Context) ([]jellyfinsource.User, error)
	Devices(context.Context) ([]jellyfinsource.Device, error)
	APIKeys(context.Context) ([]jellyfinsource.APIKey, error)
	UserData(context.Context) ([]jellyfinsource.UserData, error)
	Libraries(context.Context) ([]jellyfinsource.Library, error)
}

// Identity is the Deadfin identity store.
type Identity interface {
	ListUsers(context.Context) ([]store.User, error)
	CreateUser(context.Context, store.User) (store.User, error)
	CreateDevice(context.Context, store.Device) (store.Device, error)
	DeviceByToken(context.Context, string) (store.Device, error)
	PutAPIKey(context.Context, store.APIKey) (store.APIKey, error)
	APIKeyByToken(context.Context, string) (store.APIKey, error)
}

// Items is the Deadfin item store.
type Items interface {
	ImportMatchIndex(context.Context) (store.ImportMatchIndex, error)
	MutateUserDataForItem(ctx context.Context, userID, itemID dto.UUID, mutate func(*store.UserData) error) (store.UserData, error)
}

// Options controls one run.
type Options struct {
	// DryRun reads and matches everything and writes nothing.
	DryRun bool
	// Playlists imports Jellyfin playlists when set and the source reads them.
	Playlists PlaylistImporter
	// Collections imports Jellyfin collections when set and the source reads
	// them.
	Collections CollectionImporter
	// Trickplay imports Jellyfin TrickplayInfos when set and the source reads
	// them.
	Trickplay TrickplayImporter
	// NewUserID mints the Deadfin id of each created user. Nil uses a random
	// version-4 UUID.
	NewUserID func() (dto.UUID, error)
}

// Report counts what one run did.
type Report struct {
	UsersCreated, UsersMerged        int
	DevicesImported, DevicesExisting int
	APIKeysImported, APIKeysExisting int
	// User data, by how each row matched.
	UserDataByKey, UserDataByPath                          int
	UserDataUnmatchedLive                                  int
	UserDataUnmatchedOrphaned                              int
	UserDataUnmatchedByRoot                                map[string]int
	UserDataSkippedUser                                    int
	PlaylistsImported, PlaylistsExisting, PlaylistsSkipped int
	PlaylistEntriesMatched, PlaylistEntriesUnmatched       int
	CollectionsImported                                    int
	CollectionEntriesMatched, CollectionEntriesUnmatched   int
	TrickplayImported, TrickplayExisting                   int
	TrickplayNotWithMedia, TrickplayUnmatched              int
	Warnings                                               []string
}

// Run imports src into the Deadfin stores. It is safe to rerun: users match
// by name, existing tokens and keys are kept, and user data merges (see
// mergeUserData).
func Run(ctx context.Context, src Source, identity Identity, items Items, options Options) (Report, error) {
	report := Report{UserDataUnmatchedByRoot: map[string]int{}}

	index, err := items.ImportMatchIndex(ctx)
	if err != nil {
		return report, err
	}
	libraries, err := src.Libraries(ctx)
	if err != nil {
		return report, err
	}
	folders := NewFolderMap(libraries, index.LibrariesByName)
	for _, library := range libraries {
		if _, ok := folders[library.ID]; !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf("Jellyfin library %q has no Deadfin library of the same name", library.Name))
		}
	}

	userMap, err := importUsers(ctx, src, identity, folders, options, &report)
	if err != nil {
		return report, err
	}
	if err := importDevices(ctx, src, identity, userMap, options, &report); err != nil {
		return report, err
	}
	if err := importAPIKeys(ctx, src, identity, options, &report); err != nil {
		return report, err
	}
	if err := importUserData(ctx, src, items, index, userMap, options, &report); err != nil {
		return report, err
	}
	if err := importPlaylists(ctx, src, options.Playlists, index, userMap, options, &report); err != nil {
		return report, err
	}
	if err := importCollections(ctx, src, options.Collections, index, options, &report); err != nil {
		return report, err
	}
	if err := importTrickplay(ctx, src, options.Trickplay, index, options, &report); err != nil {
		return report, err
	}
	return report, nil
}

// importUsers creates each Jellyfin user under a fresh random id. Clients
// such as Swiftfin key saved users by user id alone, so a Deadfin user must not
// share its id with the Jellyfin user it came from. A Deadfin user with the
// same name (a rerun, or the setup-wizard account) is kept and absorbs the
// Jellyfin user's tokens and data. It returns Jellyfin id -> Deadfin id; every
// later step writes through this map.
func importUsers(ctx context.Context, src Source, identity Identity, folders FolderMap, options Options, report *Report) (map[dto.UUID]dto.UUID, error) {
	users, err := src.Users(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := identity.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	newUserID := options.NewUserID
	if newUserID == nil {
		newUserID = randomUserID
	}
	byName := map[string]store.User{}
	taken := map[dto.UUID]bool{}
	for _, user := range existing {
		byName[user.NormalizedName] = user
		taken[user.ID] = true
	}
	userMap := map[dto.UUID]dto.UUID{}
	for _, user := range users {
		normalized := strings.ToUpper(user.Username)
		if match, ok := byName[normalized]; ok {
			userMap[user.ID] = match.ID
			report.UsersMerged++
			continue
		}
		policy, warnings := Policy(user, folders)
		for _, warning := range warnings {
			report.Warnings = append(report.Warnings, fmt.Sprintf("user %q: %s", user.Username, warning))
		}
		policyJSON, err := json.Marshal(policy)
		if err != nil {
			return nil, fmt.Errorf("encode policy for %q: %w", user.Username, err)
		}
		configJSON, err := json.Marshal(Configuration(user, folders))
		if err != nil {
			return nil, fmt.Errorf("encode configuration for %q: %w", user.Username, err)
		}
		id, err := newUserID()
		if err != nil {
			return nil, fmt.Errorf("mint id for %q: %w", user.Username, err)
		}
		if id.IsNil() || id == user.ID || taken[id] {
			return nil, fmt.Errorf("mint id for %q: %s is not a fresh id", user.Username, id)
		}
		taken[id] = true
		userMap[user.ID] = id
		report.UsersCreated++
		if options.DryRun {
			continue
		}
		if _, err := identity.CreateUser(ctx, store.User{
			ID: id, Name: user.Username, NormalizedName: normalized, PasswordHash: user.PasswordHash,
			Policy: policyJSON, Configuration: configJSON, LastLoginDate: user.LastLoginDate, LastActivityDate: user.LastActivityDate,
		}); err != nil {
			return nil, fmt.Errorf("create user %q: %w", user.Username, err)
		}
	}
	return userMap, nil
}

// randomUserID returns a random version-4 UUID.
func randomUserID() (dto.UUID, error) {
	var id dto.UUID
	if _, err := rand.Read(id[:]); err != nil {
		return dto.UUID{}, err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id, nil
}

// importDevices keeps each device's newest token: Deadfin holds one token per
// user and device id.
func importDevices(ctx context.Context, src Source, identity Identity, userMap map[dto.UUID]dto.UUID, options Options, report *Report) error {
	devices, err := src.Devices(ctx)
	if err != nil {
		return err
	}
	newest := map[string]jellyfinsource.Device{}
	for _, device := range devices {
		key := device.UserID.String() + "\x00" + device.DeviceID
		if current, ok := newest[key]; !ok || device.DateLastActivity.After(current.DateLastActivity) {
			newest[key] = device
		}
	}
	keys := make([]string, 0, len(newest))
	for key := range newest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		device := newest[key]
		userID, ok := userMap[device.UserID]
		if !ok {
			continue
		}
		if _, err := identity.DeviceByToken(ctx, device.AccessToken); err == nil {
			report.DevicesExisting++
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("look up device token: %w", err)
		}
		report.DevicesImported++
		if options.DryRun {
			continue
		}
		if _, err := identity.CreateDevice(ctx, store.Device{
			UserID: userID, AccessToken: device.AccessToken, AppName: device.AppName, AppVersion: device.AppVersion,
			DeviceName: device.DeviceName, DeviceID: device.DeviceID,
			DateCreated: device.DateCreated, DateModified: device.DateModified, DateLastActivity: device.DateLastActivity,
		}); err != nil {
			return fmt.Errorf("create device %q: %w", device.DeviceName, err)
		}
	}
	return nil
}

func importAPIKeys(ctx context.Context, src Source, identity Identity, options Options, report *Report) error {
	keys, err := src.APIKeys(ctx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := identity.APIKeyByToken(ctx, key.AccessToken); err == nil {
			report.APIKeysExisting++
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("look up api key: %w", err)
		}
		report.APIKeysImported++
		if options.DryRun {
			continue
		}
		if _, err := identity.PutAPIKey(ctx, store.APIKey{Name: key.Name, AccessToken: key.AccessToken, DateCreated: key.DateCreated, DateLastActivity: key.DateLastActivity}); err != nil {
			return fmt.Errorf("create api key %q: %w", key.Name, err)
		}
	}
	return nil
}

// importUserData matches each row to a Deadfin item by user-data key, then by
// path, and merges it into the row Deadfin keys for that item. Rows that match
// no item are reported, not stored: Deadfin removes user data whose key no
// item carries (store.DeleteDetachedUserData).
func importUserData(ctx context.Context, src Source, items Items, index store.ImportMatchIndex, userMap map[dto.UUID]dto.UUID, options Options, report *Report) error {
	rows, err := src.UserData(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		userID, ok := userMap[row.UserID]
		if !ok {
			report.UserDataSkippedUser++
			continue
		}
		itemID, matched := index.ByUserDataKey[row.CustomDataKey]
		byPath := false
		if !matched && row.ItemPath != "" {
			itemID, matched = index.ByPath[row.ItemPath]
			byPath = matched
		}
		if !matched {
			if row.Orphaned() {
				report.UserDataUnmatchedOrphaned++
			} else {
				report.UserDataUnmatchedLive++
				report.UserDataUnmatchedByRoot[pathRoot(row.ItemPath)]++
			}
			continue
		}
		if byPath {
			report.UserDataByPath++
		} else {
			report.UserDataByKey++
		}
		if options.DryRun {
			continue
		}
		if _, err := items.MutateUserDataForItem(ctx, userID, itemID, func(data *store.UserData) error {
			mergeUserData(data, row)
			return nil
		}); err != nil {
			return fmt.Errorf("import user data %s for item %s: %w", row.CustomDataKey, itemID, err)
		}
	}
	return nil
}

// mergeUserData folds an imported row into existing Deadfin state. Played and
// favorite are kept if either side has them; the play count is the larger; the
// position and streams come from whichever side was played last.
func mergeUserData(data *store.UserData, row jellyfinsource.UserData) {
	importedNewer := row.LastPlayedDate != nil && (data.LastPlayed == nil || row.LastPlayedDate.After(*data.LastPlayed))
	data.Played = data.Played || row.Played
	data.IsFavorite = data.IsFavorite || row.IsFavorite
	if row.PlayCount > data.PlayCount {
		data.PlayCount = row.PlayCount
	}
	if importedNewer || data.LastPlayed == nil && data.PositionTicks == 0 {
		data.PositionTicks = row.PlaybackPositionTicks
		data.AudioStreamIndex = row.AudioStreamIndex
		data.SubtitleStreamIndex = row.SubtitleStreamIndex
	}
	if importedNewer {
		last := *row.LastPlayedDate
		data.LastPlayed = &last
	}
	if data.Rating == nil && row.Rating != nil {
		rating := float32(*row.Rating)
		data.Rating = &rating
	}
	if data.Likes == nil && row.Likes != nil {
		likes := 0
		if *row.Likes {
			likes = 1
		}
		data.Likes = &likes
	}
}

// pathRoot is the first two path segments, for the unmatched report.
func pathRoot(path string) string {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) < 2 {
		return path
	}
	return "/" + parts[0] + "/" + parts[1]
}
