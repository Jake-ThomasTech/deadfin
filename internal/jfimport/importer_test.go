// SPDX-License-Identifier: GPL-2.0-only

package jfimport_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/jfimport"
	"github.com/deadfin-org/deadfin/internal/playlists"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/jellyfinsource"
)

func id(n byte) dto.UUID { return dto.UUID{0: n} }

// nana is the Deadfin id minted for the Jellyfin user id(1).
var nana = id(101)

func mintNana() (dto.UUID, error) { return nana, nil }

type fakeSource struct {
	users     []jellyfinsource.User
	devices   []jellyfinsource.Device
	keys      []jellyfinsource.APIKey
	data      []jellyfinsource.UserData
	libraries []jellyfinsource.Library
}

func (f fakeSource) Users(context.Context) ([]jellyfinsource.User, error)        { return f.users, nil }
func (f fakeSource) Devices(context.Context) ([]jellyfinsource.Device, error)    { return f.devices, nil }
func (f fakeSource) APIKeys(context.Context) ([]jellyfinsource.APIKey, error)    { return f.keys, nil }
func (f fakeSource) UserData(context.Context) ([]jellyfinsource.UserData, error) { return f.data, nil }
func (f fakeSource) Libraries(context.Context) ([]jellyfinsource.Library, error) {
	return f.libraries, nil
}

type fakeIdentity struct {
	users   []store.User
	devices map[string]store.Device
	keys    map[string]store.APIKey
}

func (f *fakeIdentity) ListUsers(context.Context) ([]store.User, error) { return f.users, nil }
func (f *fakeIdentity) CreateUser(_ context.Context, user store.User) (store.User, error) {
	f.users = append(f.users, user)
	return user, nil
}
func (f *fakeIdentity) CreateDevice(_ context.Context, device store.Device) (store.Device, error) {
	f.devices[device.AccessToken] = device
	return device, nil
}
func (f *fakeIdentity) DeviceByToken(_ context.Context, token string) (store.Device, error) {
	if device, ok := f.devices[token]; ok {
		return device, nil
	}
	return store.Device{}, store.ErrNotFound
}
func (f *fakeIdentity) PutAPIKey(_ context.Context, key store.APIKey) (store.APIKey, error) {
	f.keys[key.AccessToken] = key
	return key, nil
}
func (f *fakeIdentity) APIKeyByToken(_ context.Context, token string) (store.APIKey, error) {
	if key, ok := f.keys[token]; ok {
		return key, nil
	}
	return store.APIKey{}, store.ErrNotFound
}

type userItem struct{ user, item dto.UUID }

type fakeItems struct {
	index store.ImportMatchIndex
	data  map[userItem]store.UserData
}

func (f *fakeItems) ImportMatchIndex(context.Context) (store.ImportMatchIndex, error) {
	return f.index, nil
}
func (f *fakeItems) MutateUserDataForItem(_ context.Context, userID, itemID dto.UUID, mutate func(*store.UserData) error) (store.UserData, error) {
	data := f.data[userItem{userID, itemID}]
	data.UserID, data.ItemID, data.CustomKey = userID, itemID, "deadfin-key"
	if err := mutate(&data); err != nil {
		return store.UserData{}, err
	}
	f.data[userItem{userID, itemID}] = data
	return data, nil
}

func newFakes() (*fakeIdentity, *fakeItems) {
	return &fakeIdentity{devices: map[string]store.Device{}, keys: map[string]store.APIKey{}},
		&fakeItems{data: map[userItem]store.UserData{}, index: store.ImportMatchIndex{
			ByUserDataKey:   map[string]dto.UUID{"tt0113277": id(10)},
			ByPath:          map[string]dto.UUID{"/media/tv/Show/S01E01.mkv": id(11)},
			LibrariesByName: map[string]dto.UUID{"Movies": id(90)},
		}}
}

func scenario() fakeSource {
	early := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	return fakeSource{
		libraries: []jellyfinsource.Library{{ID: id(80), Name: "Movies"}, {ID: id(81), Name: "Music"}},
		users: []jellyfinsource.User{{
			ID: id(1), Username: "nana", PasswordHash: "$PBKDF2-SHA512$iterations=210000$AA$BB",
			Permissions: map[int]bool{7: true, 16: false},
			Preferences: map[int]string{5: id(80).String() + "," + id(81).String()},
		}},
		devices: []jellyfinsource.Device{
			{UserID: id(1), AccessToken: "old", DeviceID: "tv", DateLastActivity: early},
			{UserID: id(1), AccessToken: "new", DeviceID: "tv", DateLastActivity: late},
		},
		keys: []jellyfinsource.APIKey{{Name: "Jellyseerr", AccessToken: "key"}},
		data: []jellyfinsource.UserData{
			// Matches by key.
			{UserID: id(1), ItemID: id(20), CustomDataKey: "tt0113277", Played: true, PlayCount: 2, LastPlayedDate: &early, ItemPath: "/elsewhere.mkv"},
			// Key differs; matches by path.
			{UserID: id(1), ItemID: id(21), CustomDataKey: "jellyfin-only-key", PlaybackPositionTicks: 42, LastPlayedDate: &late, ItemPath: "/media/tv/Show/S01E01.mkv"},
			// Live item Deadfin does not have.
			{UserID: id(1), ItemID: id(22), CustomDataKey: "nope", ItemPath: "/media/music/Artist/song.flac"},
			// Orphaned in Jellyfin and unmatched.
			{UserID: id(1), ItemID: jellyfinsource.PlaceholderItemID, CustomDataKey: "gone"},
			// Unknown user.
			{UserID: id(9), ItemID: id(20), CustomDataKey: "tt0113277"},
		},
	}
}

func TestRunImportsAndMatches(t *testing.T) {
	identity, items := newFakes()
	report, err := jfimport.Run(t.Context(), scenario(), identity, items, jfimport.Options{NewUserID: mintNana})
	if err != nil {
		t.Fatal(err)
	}

	if len(identity.users) != 1 || identity.users[0].ID != nana || identity.users[0].NormalizedName != "NANA" || identity.users[0].PasswordHash == "" {
		t.Fatalf("users = %+v", identity.users)
	}
	var policy dto.UserPolicy
	if err := json.Unmarshal(identity.users[0].Policy, &policy); err != nil {
		t.Fatal(err)
	}
	// Movies maps by name; Music has no Deadfin library and is dropped.
	if policy.EnableAllFolders || !reflect.DeepEqual(policy.EnabledFolders, []dto.UUID{id(90)}) || !policy.EnableMediaPlayback {
		t.Errorf("policy folders = %v all=%v playback=%v", policy.EnabledFolders, policy.EnableAllFolders, policy.EnableMediaPlayback)
	}

	// One token per device: the newest.
	if _, ok := identity.devices["new"]; !ok || len(identity.devices) != 1 {
		t.Errorf("devices = %v", identity.devices)
	}
	if _, ok := identity.keys["key"]; !ok {
		t.Errorf("api keys = %v", identity.keys)
	}

	if report.UserDataByKey != 1 || report.UserDataByPath != 1 || report.UserDataUnmatchedLive != 1 || report.UserDataUnmatchedOrphaned != 1 || report.UserDataSkippedUser != 1 {
		t.Errorf("report = %+v", report)
	}
	if report.UserDataUnmatchedByRoot["/media/music"] != 1 {
		t.Errorf("unmatched roots = %v", report.UserDataUnmatchedByRoot)
	}
	byKey := items.data[userItem{nana, id(10)}]
	if !byKey.Played || byKey.PlayCount != 2 {
		t.Errorf("key-matched data = %+v", byKey)
	}
	byPath := items.data[userItem{nana, id(11)}]
	if byPath.PositionTicks != 42 || byPath.CustomKey != "deadfin-key" {
		t.Errorf("path-matched data = %+v, want Deadfin's key with the imported position", byPath)
	}
}

func TestRunDryRunWritesNothing(t *testing.T) {
	identity, items := newFakes()
	report, err := jfimport.Run(t.Context(), scenario(), identity, items, jfimport.Options{NewUserID: mintNana, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(identity.users) != 0 || len(identity.devices) != 0 || len(identity.keys) != 0 || len(items.data) != 0 {
		t.Errorf("dry run wrote: users=%d devices=%d keys=%d data=%d", len(identity.users), len(identity.devices), len(identity.keys), len(items.data))
	}
	if report.UsersCreated != 1 || report.UserDataByKey != 1 {
		t.Errorf("dry-run report = %+v", report)
	}
}

func TestRunIsRerunnableAndMergesByName(t *testing.T) {
	identity, items := newFakes()
	if _, err := jfimport.Run(t.Context(), scenario(), identity, items, jfimport.Options{NewUserID: mintNana}); err != nil {
		t.Fatal(err)
	}
	report, err := jfimport.Run(t.Context(), scenario(), identity, items, jfimport.Options{NewUserID: mintNana})
	if err != nil {
		t.Fatal(err)
	}
	if report.UsersCreated != 0 || report.UsersMerged != 1 || report.DevicesExisting != 1 || report.APIKeysExisting != 1 || len(identity.users) != 1 {
		t.Errorf("rerun report = %+v users=%d", report, len(identity.users))
	}
	if data := items.data[userItem{nana, id(10)}]; data.PlayCount != 2 {
		t.Errorf("rerun changed merged data: %+v", data)
	}

	// A Deadfin account with the same name but another id absorbs the data.
	identity, items = newFakes()
	identity.users = []store.User{{ID: id(50), Name: "Nana", NormalizedName: "NANA"}}
	report, err = jfimport.Run(t.Context(), scenario(), identity, items, jfimport.Options{NewUserID: mintNana})
	if err != nil {
		t.Fatal(err)
	}
	if report.UsersMerged != 1 || len(identity.users) != 1 || identity.devices["new"].UserID != id(50) {
		t.Errorf("merge report = %+v devices = %v", report, identity.devices)
	}
	if _, ok := items.data[userItem{id(50), id(10)}]; !ok {
		t.Errorf("data not merged into the existing account: %v", items.data)
	}
}

func TestRunMergeKeepsTheMostAdvancedState(t *testing.T) {
	identity, items := newFakes()
	later := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items.data[userItem{nana, id(10)}] = store.UserData{PlayCount: 5, PositionTicks: 99, LastPlayed: &later, IsFavorite: true}
	if _, err := jfimport.Run(t.Context(), scenario(), identity, items, jfimport.Options{NewUserID: mintNana}); err != nil {
		t.Fatal(err)
	}
	got := items.data[userItem{nana, id(10)}]
	if !got.Played || got.PlayCount != 5 || got.PositionTicks != 99 || !got.IsFavorite || !got.LastPlayed.Equal(later) {
		t.Errorf("merged = %+v, want played, count 5, the newer position, favorite kept", got)
	}
}

func TestConfigurationMapsJellyfinSettings(t *testing.T) {
	user := jellyfinsource.User{SubtitleMode: 4, AudioLanguagePreference: "jpn", SubtitleLanguagePreference: "eng", EnableNextEpisodeAutoPlay: true,
		Preferences: map[int]string{11: id(80).String()}}
	config := jfimport.Configuration(user, jfimport.FolderMap{id(80): id(90)})
	if config.SubtitleMode != dto.SubtitleModeSmart || *config.AudioLanguagePreference != "jpn" || *config.SubtitleLanguagePreference != "eng" ||
		!config.EnableNextEpisodeAutoPlay || !reflect.DeepEqual(config.OrderedViews, []dto.UUID{id(90)}) {
		t.Errorf("configuration = %+v", config)
	}
}

type containerSource struct {
	fakeSource
	playlists   []jellyfinsource.Container
	collections []jellyfinsource.Container
}

func (c containerSource) Containers(_ context.Context, typeName string) ([]jellyfinsource.Container, error) {
	switch typeName {
	case jellyfinsource.PlaylistType:
		return c.playlists, nil
	case jellyfinsource.BoxSetType:
		return c.collections, nil
	}
	return nil, nil
}

type fakePlaylists struct{ imported []playlists.ImportRequest }

func (f *fakePlaylists) Import(_ context.Context, request playlists.ImportRequest) (bool, error) {
	for _, existing := range f.imported {
		if existing.Name == request.Name {
			return false, nil
		}
	}
	f.imported = append(f.imported, request)
	return true, nil
}

func TestRunImportsPlaylists(t *testing.T) {
	identity, items := newFakes()
	src := containerSource{fakeSource: scenario(), playlists: []jellyfinsource.Container{
		{
			Name: "Mix", OwnerUserID: id(1), OpenAccess: true, MediaType: "Video",
			Shares:     []jellyfinsource.Share{{UserID: id(9), CanEdit: true}, {UserID: id(1)}},
			ChildPaths: []string{"/media/tv/Show/S01E01.mkv", "/media/missing.mkv"}, UnresolvedChildren: 1,
		},
		{Name: "Orphan", OwnerUserID: id(9)},
	}}
	importer := &fakePlaylists{}
	report, err := jfimport.Run(t.Context(), src, identity, items, jfimport.Options{NewUserID: mintNana, Playlists: importer})
	if err != nil {
		t.Fatal(err)
	}
	if len(importer.imported) != 1 {
		t.Fatalf("imported = %+v", importer.imported)
	}
	got := importer.imported[0]
	// The unknown user's share and the owner's own share drop.
	if got.Name != "Mix" || got.OwnerUserID != nana || !got.OpenAccess || got.MediaType != item.MediaTypeVideo || len(got.Shares) != 0 {
		t.Fatalf("request = %+v", got)
	}
	if !reflect.DeepEqual(got.Children, []dto.UUID{id(11)}) {
		t.Fatalf("children = %v", got.Children)
	}
	if report.PlaylistsImported != 1 || report.PlaylistsSkipped != 1 || report.PlaylistEntriesMatched != 1 || report.PlaylistEntriesUnmatched != 2 {
		t.Fatalf("report = %+v", report)
	}

	report, err = jfimport.Run(t.Context(), src, identity, items, jfimport.Options{NewUserID: mintNana, Playlists: importer})
	if err != nil || report.PlaylistsImported != 0 || report.PlaylistsExisting != 1 {
		t.Fatalf("rerun report = %+v, %v", report, err)
	}

	dry := &fakePlaylists{}
	report, err = jfimport.Run(t.Context(), src, identity, items, jfimport.Options{NewUserID: mintNana, DryRun: true, Playlists: dry})
	if err != nil || len(dry.imported) != 0 || report.PlaylistsImported != 1 {
		t.Fatalf("dry run imported %d, report = %+v, %v", len(dry.imported), report, err)
	}
}

type fakeCollections map[string][]dto.UUID

func (f fakeCollections) ImportCollection(_ context.Context, collection jfimport.CollectionImport) ([]string, error) {
	f[collection.Name] = append(f[collection.Name], collection.Children...)
	return nil, nil
}

func TestRunImportsCollections(t *testing.T) {
	identity, items := newFakes()
	src := containerSource{fakeSource: scenario(), collections: []jellyfinsource.Container{
		{Name: "Heist", ChildPaths: []string{"/media/tv/Show/S01E01.mkv", "/media/missing.mkv"}, UnresolvedChildren: 1},
		{Name: "Empty"},
		{ID: id(70)},
	}}
	imported := fakeCollections{}
	report, err := jfimport.Run(t.Context(), src, identity, items, jfimport.Options{NewUserID: mintNana, Collections: imported})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(imported["Heist"], []dto.UUID{id(11)}) || len(imported) != 2 {
		t.Fatalf("imported = %v", imported)
	}
	if report.CollectionsImported != 2 || report.CollectionEntriesMatched != 1 || report.CollectionEntriesUnmatched != 2 || len(report.Warnings) == 0 {
		t.Fatalf("report = %+v", report)
	}

	dry := fakeCollections{}
	if _, err := jfimport.Run(t.Context(), src, identity, items, jfimport.Options{NewUserID: mintNana, DryRun: true, Collections: dry}); err != nil || len(dry) != 0 {
		t.Fatalf("dry run imported %v, %v", dry, err)
	}
}

// TestRunMintsFreshUserIDs proves that no imported user keeps its Jellyfin
// id, that every reference the importer writes uses the new id, and that a
// rerun matches by name and creates nothing.
func TestRunMintsFreshUserIDs(t *testing.T) {
	identity, items := newFakes()
	src := scenario()
	src.users = append(src.users, jellyfinsource.User{ID: id(2), Username: "kai"})
	src.devices = append(src.devices, jellyfinsource.Device{UserID: id(2), AccessToken: "kai-token", DeviceID: "phone"})
	containers := containerSource{fakeSource: src, playlists: []jellyfinsource.Container{
		{Name: "Shared", OwnerUserID: id(1), Shares: []jellyfinsource.Share{{UserID: id(2), CanEdit: true}}},
	}}
	importer := &fakePlaylists{}
	report, err := jfimport.Run(t.Context(), containers, identity, items, jfimport.Options{Playlists: importer})
	if err != nil {
		t.Fatal(err)
	}
	if report.UsersCreated != 2 || len(identity.users) != 2 {
		t.Fatalf("report = %+v users = %+v", report, identity.users)
	}
	byName := map[string]dto.UUID{}
	for _, user := range identity.users {
		if user.ID == id(1) || user.ID == id(2) || user.ID.IsNil() {
			t.Fatalf("user %q kept a Jellyfin or nil id %s", user.Name, user.ID)
		}
		if user.ID[6]>>4 != 4 {
			t.Errorf("user %q id %s is not a version-4 UUID", user.Name, user.ID)
		}
		byName[user.NormalizedName] = user.ID
	}
	nanaID, kaiID := byName["NANA"], byName["KAI"]
	if nanaID == kaiID {
		t.Fatalf("users share id %s", nanaID)
	}
	if identity.devices["new"].UserID != nanaID || identity.devices["kai-token"].UserID != kaiID {
		t.Errorf("devices = %+v", identity.devices)
	}
	if _, ok := items.data[userItem{nanaID, id(10)}]; !ok {
		t.Errorf("user data not written under the new id: %v", items.data)
	}
	if len(importer.imported) != 1 {
		t.Fatalf("playlists = %+v", importer.imported)
	}
	playlist := importer.imported[0]
	if playlist.OwnerUserID != nanaID || len(playlist.Shares) != 1 || playlist.Shares[0].UserID != kaiID {
		t.Errorf("playlist owner/shares = %s %+v", playlist.OwnerUserID, playlist.Shares)
	}

	report, err = jfimport.Run(t.Context(), containers, identity, items, jfimport.Options{Playlists: importer})
	if err != nil {
		t.Fatal(err)
	}
	if report.UsersCreated != 0 || report.UsersMerged != 2 || len(identity.users) != 2 || report.DevicesImported != 0 {
		t.Errorf("rerun report = %+v users = %d", report, len(identity.users))
	}
	if identity.devices["kai-token"].UserID != kaiID {
		t.Errorf("rerun moved kai's token: %+v", identity.devices["kai-token"])
	}
}
