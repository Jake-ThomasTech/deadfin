// SPDX-License-Identifier: GPL-2.0-only

package jfimport

import (
	"context"
	"fmt"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playlists"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/jellyfinsource"
)

// ContainerSource is the optional Source part that reads Jellyfin playlists
// and collections.
type ContainerSource interface {
	Containers(ctx context.Context, typeName string) ([]jellyfinsource.Container, error)
}

// PlaylistImporter writes one imported playlist. It returns false when the
// playlist already exists.
type PlaylistImporter interface {
	Import(ctx context.Context, request playlists.ImportRequest) (bool, error)
}

// importPlaylists carries each Jellyfin playlist over with its owner, shares,
// and entries. Entries match Deadfin items by path; unmatched entries are
// counted and dropped. A playlist whose owner did not import is skipped.
func importPlaylists(ctx context.Context, src Source, importer PlaylistImporter, index store.ImportMatchIndex, userMap map[dto.UUID]dto.UUID, options Options, report *Report) error {
	containers, ok := src.(ContainerSource)
	if !ok || importer == nil {
		return nil
	}
	values, err := containers.Containers(ctx, jellyfinsource.PlaylistType)
	if err != nil {
		return err
	}
	for _, value := range values {
		owner, ok := userMap[value.OwnerUserID]
		if !ok {
			report.PlaylistsSkipped++
			report.Warnings = append(report.Warnings, fmt.Sprintf("playlist %q: owner %s did not import; skipped", value.Name, value.OwnerUserID))
			continue
		}
		request := playlists.ImportRequest{
			Name: value.Name, OwnerUserID: owner, OpenAccess: value.OpenAccess,
			MediaType: importMediaType(value.MediaType), DateCreated: value.DateCreated,
		}
		for _, share := range value.Shares {
			if userID, ok := userMap[share.UserID]; ok && userID != owner {
				request.Shares = append(request.Shares, store.ItemShare{UserID: userID, CanEdit: share.CanEdit})
			}
		}
		report.PlaylistEntriesUnmatched += value.UnresolvedChildren
		for _, path := range value.ChildPaths {
			if itemID, ok := index.ByPath[path]; ok {
				request.Children = append(request.Children, itemID)
				report.PlaylistEntriesMatched++
			} else {
				report.PlaylistEntriesUnmatched++
			}
		}
		if options.DryRun {
			report.PlaylistsImported++
			continue
		}
		created, err := importer.Import(ctx, request)
		if err != nil {
			return fmt.Errorf("import playlist %q: %w", value.Name, err)
		}
		if created {
			report.PlaylistsImported++
		} else {
			report.PlaylistsExisting++
		}
	}
	return nil
}

func importMediaType(name string) item.MediaType {
	for _, candidate := range [...]item.MediaType{item.MediaTypeVideo, item.MediaTypeAudio, item.MediaTypePhoto, item.MediaTypeBook} {
		if candidate.String() == name {
			return candidate
		}
	}
	return item.MediaTypeUnknown
}

// CollectionImport is one Jellyfin collection to import.
type CollectionImport struct {
	Name     string
	Children []dto.UUID
	// ProviderIDs are the collection's Jellyfin provider IDs (for example
	// Tmdb, the TMDb collection ID).
	ProviderIDs map[string]string
	// Images are the collection's Jellyfin images.
	Images []jellyfinsource.Image
}

// CollectionImporter writes one imported collection: it creates or finds the
// BoxSet by name, links the children, and keeps the provider IDs and
// images. It returns a warning for each image it could not keep.
type CollectionImporter interface {
	ImportCollection(ctx context.Context, collection CollectionImport) ([]string, error)
}

// importCollections carries each Jellyfin collection (BoxSet) over by name,
// with its provider IDs and images. Members match Deadfin items by path;
// unmatched members are counted and dropped. A rerun finds the same BoxSet
// and links nothing twice.
func importCollections(ctx context.Context, src Source, importer CollectionImporter, index store.ImportMatchIndex, options Options, report *Report) error {
	containers, ok := src.(ContainerSource)
	if !ok || importer == nil {
		return nil
	}
	values, err := containers.Containers(ctx, jellyfinsource.BoxSetType)
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.Name == "" {
			report.Warnings = append(report.Warnings, fmt.Sprintf("collection %s has no name; skipped", value.ID))
			continue
		}
		report.CollectionEntriesUnmatched += value.UnresolvedChildren
		var children []dto.UUID
		for _, path := range value.ChildPaths {
			if itemID, ok := index.ByPath[path]; ok {
				children = append(children, itemID)
				report.CollectionEntriesMatched++
			} else {
				report.CollectionEntriesUnmatched++
			}
		}
		report.CollectionsImported++
		if options.DryRun {
			continue
		}
		warnings, err := importer.ImportCollection(ctx, CollectionImport{Name: value.Name, Children: children, ProviderIDs: value.ProviderIDs, Images: value.Images})
		if err != nil {
			return fmt.Errorf("import collection %q: %w", value.Name, err)
		}
		report.Warnings = append(report.Warnings, warnings...)
	}
	return nil
}
