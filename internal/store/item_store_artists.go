// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

// ArtistNames returns the distinct artist and album artist values of all
// items, in no defined order (BaseItemRepository.GetAllArtistNames).
func (s *ItemStore) ArtistNames(ctx context.Context) ([]string, error) {
	rows, err := s.stores.Reader().QueryContext(ctx, "SELECT DISTINCT v.value FROM item_values v WHERE v.kind IN ('artist','album_artist') AND EXISTS (SELECT 1 FROM item_value_map m WHERE m.value_id = v.id)")
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, closeRows(rows, err)
		}
		names = append(names, name)
	}
	return names, closeRows(rows, rows.Err())
}

// InsertMissingItems inserts each of values whose id is not stored, with its
// satellites and derived rows, in one transaction. A stored item stays as
// it is. It returns the number of inserted items.
func (s *ItemStore) InsertMissingItems(ctx context.Context, values []item.Item) (inserted int, err error) {
	if len(values) == 0 {
		return 0, nil
	}
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollbackOnReturn(tx, &err)
	if err := s.lockDerivedWrite(ctx, tx, values...); err != nil {
		return 0, err
	}
	ids := make([]dto.UUID, len(values))
	for index, value := range values {
		ids[index] = value.ID
	}
	stored := make(map[dto.UUID]struct{}, len(values))
	err = s.forUUIDChunks(ids, func(_ []dto.UUID, marks string, args []any) error {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM items WHERE id IN ("+marks+")", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw any
			if err := rows.Scan(&raw); err != nil {
				return closeRows(rows, err)
			}
			id, err := scanUUID(raw)
			if err != nil {
				return closeRows(rows, err)
			}
			stored[id] = struct{}{}
		}
		return closeRows(rows, rows.Err())
	})
	if err != nil {
		return 0, err
	}
	var roots []dto.UUID
	for _, value := range values {
		if _, found := stored[value.ID]; found {
			continue
		}
		stored[value.ID] = struct{}{}
		if err := s.validateUpsert(ctx, tx, value); err != nil {
			return 0, err
		}
		if err := s.upsertItem(ctx, tx, value); err != nil {
			return 0, err
		}
		if err := s.replaceSatellites(ctx, tx, value); err != nil {
			return 0, err
		}
		roots = append(roots, value.ID)
	}
	if len(roots) == 0 {
		return 0, nil
	}
	if err := s.repairDerivedForTx(ctx, tx, roots, nil); err != nil {
		return 0, err
	}
	return len(roots), tx.Commit()
}

// DeadArtistIDs returns the unlocked MusicArtist items that are accessed by
// name (no parent) and that no artist or album artist value names, compared
// by clean name (InternalItemsQuery.IsDeadArtist with IsAccessedByName). It
// also returns those that an artist folder with the same clean name
// replaced (D-111).
func (s *ItemStore) DeadArtistIDs(ctx context.Context) ([]dto.UUID, error) {
	kind := item.KindMusicArtist.String()
	rows, err := s.stores.Reader().QueryContext(ctx, "SELECT a.id FROM items a WHERE a.kind = "+s.ph(1)+" AND a.parent_id IS NULL AND a.is_locked = "+s.ph(2)+" AND (NOT EXISTS (SELECT 1 FROM item_values v JOIN item_value_map m ON m.value_id = v.id WHERE v.kind IN ('artist','album_artist') AND v.clean_value = a.clean_name) OR EXISTS (SELECT 1 FROM items f WHERE f.kind = "+s.ph(3)+" AND f.parent_id IS NOT NULL AND f.clean_name = a.clean_name)) ORDER BY a.id", kind, boolArg(s.stores.Dialect(), false), kind)
	if err != nil {
		return nil, err
	}
	var ids []dto.UUID
	for rows.Next() {
		var raw any
		if err := rows.Scan(&raw); err != nil {
			return nil, closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return nil, closeRows(rows, err)
		}
		ids = append(ids, id)
	}
	return ids, closeRows(rows, rows.Err())
}
