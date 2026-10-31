// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"context"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/item"
	dfpath "github.com/deadfin-org/deadfin/internal/path"
	"github.com/deadfin-org/deadfin/internal/store"
)

// ArtistEntity constructs the MusicArtist item that LibraryManager.GetArtist
// creates for an artist accessed by name: its path is MusicArtist.GetPath
// under the metadata artists directory, and its identity is the item-by-name
// id of that path.
func ArtistEntity(name string) item.Item {
	name = strings.TrimSpace(name)
	if name == "" {
		return item.Item{}
	}
	valid := strings.TrimRight(strings.TrimSpace(dfpath.ValidFilename(name)), ".")
	path := "%MetadataPath%/artists/" + valid
	identityPath := "metadata\\artists\\" + valid
	result := item.Item{ID: itemID(item.KindMusicArtist, identityPath, "", false), Kind: item.KindMusicArtist, Path: &path, Name: &name, IsFolder: true}
	if keys := item.UserDataKeys(result, item.UserDataInputs{}); len(keys) != 0 {
		result.PresentationUniqueKey = &keys[0]
	}
	return result
}

// validateArtists ports ArtistsValidator: every artist or album artist name
// on an item gets a MusicArtist. A name that a MusicArtist already has, by
// clean name, keeps that item, so an artist folder serves its tag name. It
// then deletes the unlocked artists accessed by name that no item names,
// and those that an artist folder of the same name replaced (D-111), so
// /Artists lists one artist per name.
// Names that differ only in case share one artist, named by the ordinal
// first spelling.
func (s *Scanner) validateArtists(ctx context.Context) error {
	names, err := s.items.ArtistNames(ctx)
	if err != nil {
		return err
	}
	sort.Strings(names)
	existing, err := s.items.QueryItemNames(ctx, store.ItemQuery{IncludeItemTypes: []item.Kind{item.KindMusicArtist}})
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(existing)+len(names))
	for _, value := range existing {
		if value.Kind == item.KindMusicArtist && value.Name != nil {
			known[item.CleanName(strings.TrimSpace(*value.Name))] = struct{}{}
		}
	}
	var create []item.Item
	for _, name := range names {
		key := item.CleanName(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		if _, found := known[key]; found {
			continue
		}
		known[key] = struct{}{}
		if entity := ArtistEntity(name); !entity.ID.IsNil() {
			create = append(create, entity)
		}
	}
	materializeScannedSortNames(create)
	if len(create) != 0 {
		var inserted int
		if err := s.storeWrite(ctx, func() error {
			var err error
			inserted, err = s.items.InsertMissingItems(ctx, create)
			return err
		}); err != nil {
			return err
		}
		s.logger.Info("artists validated", "names", len(names), "created", inserted)
	}
	dead, err := s.items.DeadArtistIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range dead {
		s.logger.Info("deleting dead MusicArtist", "id", id.String())
		if err := s.storeWrite(ctx, func() error { return s.items.Delete(ctx, id) }); err != nil {
			return err
		}
	}
	return nil
}
