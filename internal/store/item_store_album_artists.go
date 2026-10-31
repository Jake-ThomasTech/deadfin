// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"database/sql"
	"slices"
	"sort"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

// MusicAlbumArtists holds the Artists and AlbumArtists values of one item.
type MusicAlbumArtists struct {
	Artists      []string
	AlbumArtists []string
}

// MusicAlbumTracks is one MusicAlbum with its stored artist values and the
// artist values of its tracks (MusicAlbum.Tracks, the recursive Audio
// children, so a multi-disc album includes the tracks of its disc folders).
// Tracks are in path order. Folders are the distinct parents of the tracks:
// the album and its disc folders.
type MusicAlbumTracks struct {
	ID       dto.UUID
	IsLocked bool
	Album    MusicAlbumArtists
	Tracks   []MusicAlbumArtists
	Folders  []dto.UUID
}

// MusicAlbumArtistUpdate replaces the artist values of one MusicAlbum.
type MusicAlbumArtistUpdate struct {
	ID     dto.UUID
	Values MusicAlbumArtists
}

// QueryMusicAlbumTracks returns every MusicAlbum with its artist values and
// those of its tracks, in album id order.
func (s *ItemStore) QueryMusicAlbumTracks(ctx context.Context) ([]MusicAlbumTracks, error) {
	db := s.stores.Reader()
	rows, err := db.QueryContext(ctx, "SELECT id, is_locked FROM items WHERE kind = "+s.ph(1), item.KindMusicAlbum.String())
	if err != nil {
		return nil, err
	}
	var albums []MusicAlbumTracks
	index := map[dto.UUID]int{}
	for rows.Next() {
		var raw, locked any
		if err := rows.Scan(&raw, &locked); err != nil {
			return nil, closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return nil, closeRows(rows, err)
		}
		index[id] = len(albums)
		albums = append(albums, MusicAlbumTracks{ID: id, IsLocked: asBool(locked)})
	}
	if err := closeRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	if len(albums) == 0 {
		return nil, nil
	}
	ids := make([]dto.UUID, len(albums))
	for position, album := range albums {
		ids[position] = album.ID
	}
	err = s.forUUIDChunks(ids, func(_ []dto.UUID, marks string, args []any) error {
		values, err := db.QueryContext(ctx, "SELECT m.item_id, v.kind, v.value FROM item_value_map m JOIN item_values v ON v.id = m.value_id WHERE m.item_id IN ("+marks+") AND v.kind IN ('artist','album_artist') ORDER BY m.item_id, v.kind, m.sort_order", args...)
		if err != nil {
			return err
		}
		for values.Next() {
			var raw any
			var kind, value string
			if err := values.Scan(&raw, &kind, &value); err != nil {
				return closeRows(values, err)
			}
			id, err := scanUUID(raw)
			if err != nil {
				return closeRows(values, err)
			}
			appendMusicArtistValue(&albums[index[id]].Album, kind, value)
		}
		return closeRows(values, values.Err())
	})
	if err != nil {
		return nil, err
	}
	type track struct {
		album  dto.UUID
		parent dto.UUID
		path   string
		values MusicAlbumArtists
	}
	tracks := map[dto.UUID]*track{}
	err = s.forUUIDChunks(ids, func(_ []dto.UUID, marks string, args []any) error {
		values, err := db.QueryContext(ctx, "SELECT scope.ancestor_id, track.id, track.parent_id, track.path, tv.kind, tv.value FROM ancestors AS scope JOIN items AS track ON track.id = scope.item_id AND track.kind = '"+item.KindAudio.String()+"' LEFT JOIN (SELECT m.item_id, m.sort_order, v.kind, v.value FROM item_value_map m JOIN item_values v ON v.id = m.value_id WHERE v.kind IN ('artist','album_artist')) AS tv ON tv.item_id = track.id WHERE scope.ancestor_id IN ("+marks+") ORDER BY track.id, tv.kind, tv.sort_order", args...)
		if err != nil {
			return err
		}
		for values.Next() {
			var rawAlbum, rawTrack, rawParent, path, kind, value any
			if err := values.Scan(&rawAlbum, &rawTrack, &rawParent, &path, &kind, &value); err != nil {
				return closeRows(values, err)
			}
			albumID, err := scanUUID(rawAlbum)
			if err != nil {
				return closeRows(values, err)
			}
			trackID, err := scanUUID(rawTrack)
			if err != nil {
				return closeRows(values, err)
			}
			current, found := tracks[trackID]
			if !found {
				current = &track{album: albumID}
				if !isNil(rawParent) {
					if current.parent, err = scanUUID(rawParent); err != nil {
						return closeRows(values, err)
					}
				}
				if !isNil(path) {
					current.path = asString(path)
				}
				tracks[trackID] = current
			}
			if !isNil(kind) && !isNil(value) {
				appendMusicArtistValue(&current.values, asString(kind), asString(value))
			}
		}
		return closeRows(values, values.Err())
	})
	if err != nil {
		return nil, err
	}
	ordered := make([]dto.UUID, 0, len(tracks))
	for id := range tracks {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(left, right int) bool {
		a, b := tracks[ordered[left]], tracks[ordered[right]]
		if a.path != b.path {
			return a.path < b.path
		}
		return ordered[left].String() < ordered[right].String()
	})
	for _, id := range ordered {
		current := tracks[id]
		album := &albums[index[current.album]]
		album.Tracks = append(album.Tracks, current.values)
		if !current.parent.IsNil() && !slices.Contains(album.Folders, current.parent) {
			album.Folders = append(album.Folders, current.parent)
		}
	}
	return albums, nil
}

func appendMusicArtistValue(target *MusicAlbumArtists, kind, value string) {
	switch kind {
	case "artist":
		target.Artists = append(target.Artists, value)
	case "album_artist":
		target.AlbumArtists = append(target.AlbumArtists, value)
	}
}

// UpdateMusicAlbumArtists replaces the Artists and AlbumArtists values of
// each MusicAlbum in updates and repairs their user data keys, which name
// the first album artist. It leaves the other values and satellites as
// they are.
func (s *ItemStore) UpdateMusicAlbumArtists(ctx context.Context, updates []MusicAlbumArtistUpdate) (err error) {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	ids := make([]dto.UUID, len(updates))
	writes := make([]item.Item, len(updates))
	for position, update := range updates {
		ids[position] = update.ID
		writes[position] = item.Item{ID: update.ID, Kind: item.KindMusicAlbum, Values: item.ItemValues{Artists: update.Values.Artists, AlbumArtists: update.Values.AlbumArtists}}
	}
	if err := s.lockDerivedScope(ctx, tx, func(context.Context, *sql.Tx) (derivedLockScope, error) {
		return derivedLockScope{items: ids, writes: writes, replaced: ids}, nil
	}); err != nil {
		return err
	}
	previous, err := s.mappedValueIDs(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, "DELETE FROM item_value_map WHERE item_id = "+s.ph(1)+" AND value_id IN (SELECT id FROM item_values WHERE kind IN ('artist','album_artist'))", s.uuidArg(update.ID)); err != nil {
			return err
		}
		for kind, values := range map[string][]string{"artist": update.Values.Artists, "album_artist": update.Values.AlbumArtists} {
			for order, value := range values {
				if err := s.insertValue(ctx, tx, update.ID, kind, value, order); err != nil {
					return err
				}
			}
		}
	}
	if err := s.deleteOrphanValues(ctx, tx, previous); err != nil {
		return err
	}
	if err := s.repairScopedUserDataKeys(ctx, tx, ids); err != nil {
		return err
	}
	return tx.Commit()
}
