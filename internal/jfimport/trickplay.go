// SPDX-License-Identifier: GPL-2.0-only

package jfimport

import (
	"context"
	"fmt"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/jellyfinsource"
)

// TrickplaySource is the optional Source part that reads Jellyfin
// TrickplayInfos.
type TrickplaySource interface {
	TrickplayInfos(context.Context) ([]jellyfinsource.TrickplayInfo, error)
}

// TrickplayImportResult is the outcome of one imported rendition.
type TrickplayImportResult uint8

const (
	// TrickplayImported persisted the rendition.
	TrickplayImported TrickplayImportResult = iota
	// TrickplayExisting left the item's persisted rendition of that width.
	TrickplayExisting
	// TrickplayNotWithMedia found no tiles saved with the media. Tiles in
	// Jellyfin's own data directory are not reachable from Deadfin.
	TrickplayNotWithMedia
)

// TrickplayImporter persists one Jellyfin rendition for a Deadfin item.
type TrickplayImporter interface {
	ImportTrickplay(ctx context.Context, itemID dto.UUID, info jellyfinsource.TrickplayInfo) (TrickplayImportResult, error)
}

// importTrickplay carries Jellyfin TrickplayInfos over for items that match
// by path, so existing trickplay is not generated again. Only renditions
// whose tiles are saved with the media are usable; the importer decides that.
func importTrickplay(ctx context.Context, src Source, importer TrickplayImporter, index store.ImportMatchIndex, options Options, report *Report) error {
	trickplays, ok := src.(TrickplaySource)
	if !ok {
		return nil
	}
	values, err := trickplays.TrickplayInfos(ctx)
	if err != nil {
		return err
	}
	for _, value := range values {
		itemID, ok := index.ByPath[value.ItemPath]
		if !ok || value.ItemPath == "" {
			report.TrickplayUnmatched++
			continue
		}
		if options.DryRun || importer == nil {
			report.TrickplayImported++
			continue
		}
		result, err := importer.ImportTrickplay(ctx, itemID, value)
		if err != nil {
			return fmt.Errorf("import trickplay width %d for item %s: %w", value.Width, itemID, err)
		}
		switch result {
		case TrickplayImported:
			report.TrickplayImported++
		case TrickplayExisting:
			report.TrickplayExisting++
		case TrickplayNotWithMedia:
			report.TrickplayNotWithMedia++
		}
	}
	return nil
}
