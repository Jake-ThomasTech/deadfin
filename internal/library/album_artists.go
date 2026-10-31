// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
)

// albumScanFolders records the folders a scan reconciled. An album refresh
// takes its artists from its songs when the refresh is a full refresh or a
// child changed (AlbumMetadataService.UpdateMetadataFromChildren).
type albumScanFolders struct {
	scanned map[dto.UUID]struct{}
	changed map[dto.UUID]struct{}
	full    bool
}

func newAlbumScanFolders(opts RefreshOptions) albumScanFolders {
	return albumScanFolders{scanned: map[dto.UUID]struct{}{}, changed: map[dto.UUID]struct{}{}, full: opts.ReplaceAllMetadata || opts.Metadata >= RefreshModeFullRefresh}
}

func (folders albumScanFolders) record(progress RefreshProgress) {
	if progress.Status != "scanned" || progress.ItemID.IsNil() {
		return
	}
	folders.scanned[progress.ItemID] = struct{}{}
	if progress.Inserted+progress.Updated+progress.Deleted != 0 {
		folders.changed[progress.ItemID] = struct{}{}
	}
}

// refreshes reports whether the scan refreshed album from its songs. The
// album folder and its disc folders hold the songs.
func (folders albumScanFolders) refreshes(album store.MusicAlbumTracks) bool {
	visited := append([]dto.UUID{album.ID}, album.Folders...)
	want := folders.changed
	if folders.full {
		want = folders.scanned
	}
	for _, id := range visited {
		if _, found := want[id]; found {
			return true
		}
	}
	return false
}

// mergeMusicAlbumArtists ports AlbumMetadataService.SetArtistsFromSongs and
// SetAlbumArtistFromSongs for the albums the scan refreshed. An album that
// has no artist values while its songs have some also takes them, so a
// library scanned before this step gets them on its next scan. A locked
// album keeps its values.
func (s *Scanner) mergeMusicAlbumArtists(ctx context.Context, folders albumScanFolders) error {
	albums, err := s.items.QueryMusicAlbumTracks(ctx)
	if err != nil {
		return err
	}
	var updates []store.MusicAlbumArtistUpdate
	for _, album := range albums {
		if album.IsLocked {
			continue
		}
		songs := store.MusicAlbumArtists{Artists: artistsFromSongs(album.Tracks, func(song store.MusicAlbumArtists) []string { return song.Artists }), AlbumArtists: artistsFromSongs(album.Tracks, func(song store.MusicAlbumArtists) []string { return song.AlbumArtists })}
		backfill := len(album.Album.Artists) == 0 && len(album.Album.AlbumArtists) == 0 && (len(songs.Artists) != 0 || len(songs.AlbumArtists) != 0)
		if !backfill && !folders.refreshes(album) {
			continue
		}
		if slices.Equal(album.Album.Artists, songs.Artists) && slices.Equal(album.Album.AlbumArtists, songs.AlbumArtists) {
			continue
		}
		updates = append(updates, store.MusicAlbumArtistUpdate{ID: album.ID, Values: songs})
	}
	if len(updates) == 0 {
		return nil
	}
	return s.storeWrite(ctx, func() error { return s.items.UpdateMusicAlbumArtists(ctx, updates) })
}

// artistsFromSongs groups the names of the songs case-insensitively, keeps
// the first spelling of each group, and orders the groups by song count,
// most first; equal counts keep their first-seen order (GroupBy with
// StringComparer.OrdinalIgnoreCase, then a stable OrderByDescending).
func artistsFromSongs(songs []store.MusicAlbumArtists, names func(store.MusicAlbumArtists) []string) []string {
	type group struct {
		name  string
		count int
	}
	var groups []group
	index := map[string]int{}
	for _, song := range songs {
		for _, name := range names(song) {
			key := strings.ToLower(name)
			if position, found := index[key]; found {
				groups[position].count++
				continue
			}
			index[key] = len(groups)
			groups = append(groups, group{name: name, count: 1})
		}
	}
	sort.SliceStable(groups, func(left, right int) bool { return groups[left].count > groups[right].count })
	result := make([]string, len(groups))
	for position, value := range groups {
		result[position] = value.name
	}
	return result
}
