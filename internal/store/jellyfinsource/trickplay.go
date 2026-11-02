// SPDX-License-Identifier: GPL-2.0-only

package jellyfinsource

import (
	"context"
	"fmt"

	"github.com/deadfin-org/deadfin/internal/dto"
)

// TrickplayInfo is one Jellyfin TrickplayInfos row with the path of its item.
// Interval is milliseconds; Bandwidth is bits per second.
type TrickplayInfo struct {
	ItemID         dto.UUID
	ItemPath       string
	Width          int
	Height         int
	TileWidth      int
	TileHeight     int
	ThumbnailCount int
	Interval       int
	Bandwidth      int
}

// TrickplayInfos returns every trickplay rendition whose item still exists.
// A database without the TrickplayInfos table has none.
func (s *Source) TrickplayInfos(ctx context.Context) ([]TrickplayInfo, error) {
	present, err := s.hasTable(ctx, "TrickplayInfos")
	if err != nil || !present {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.ItemId, COALESCE(b.Path,''), t.Width, t.Height, t.TileWidth, t.TileHeight, t.ThumbnailCount, t.Interval, t.Bandwidth
		FROM TrickplayInfos t JOIN BaseItems b ON b.Id = t.ItemId ORDER BY t.ItemId, t.Width`)
	if err != nil {
		return nil, fmt.Errorf("jellyfinsource: read trickplay infos: %w", err)
	}
	defer rows.Close()
	var out []TrickplayInfo
	for rows.Next() {
		var info TrickplayInfo
		var itemID string
		if err := rows.Scan(&itemID, &info.ItemPath, &info.Width, &info.Height, &info.TileWidth, &info.TileHeight, &info.ThumbnailCount, &info.Interval, &info.Bandwidth); err != nil {
			return nil, fmt.Errorf("jellyfinsource: scan trickplay info: %w", err)
		}
		if info.ItemID, err = dto.ParseUUID(itemID); err != nil {
			return nil, fmt.Errorf("jellyfinsource: trickplay info item: %w", err)
		}
		out = append(out, info)
	}
	return out, rows.Err()
}
