// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

func (s *ItemStore) upsertItem(ctx context.Context, tx *sql.Tx, it item.Item) error {
	if it.Name != nil {
		clean := item.CleanName(*it.Name)
		it.CleanName = &clean
	}
	extra, err := json.Marshal(it.Extra)
	if err != nil {
		return fmt.Errorf("marshal item extra: %w", err)
	}
	args := itemArguments(it, extra, s.stores.Dialect())
	updates := make([]string, 0, len(itemColumns)-1)
	for _, column := range itemColumns[1:] {
		updates = append(updates, column+" = excluded."+column)
	}
	q := "INSERT INTO items (" + strings.Join(itemColumns, ", ") + ") VALUES (" + s.placeholders(len(itemColumns)) + ") ON CONFLICT(id) DO UPDATE SET " + strings.Join(updates, ", ")
	_, err = tx.ExecContext(ctx, q, args...)
	return err
}

func itemArguments(it item.Item, extra []byte, d Dialect) []any {
	return []any{
		uuidArgDialect(d, it.ID), it.Kind.String(), uuidPtrArg(d, it.ParentID), uuidPtrArg(d, it.TopParentID), uuidPtrArg(d, it.SeriesID), uuidPtrArg(d, it.SeasonID), uuidPtrArg(d, it.ChannelID), uuidPtrArg(d, it.PrimaryVersionID), uuidPtrArg(d, it.OwnerID), uuidArgDialect(d, it.PlaylistOwnerUserID),
		it.Path, it.Name, it.SortName, it.CleanName, it.ForcedSortName, it.OriginalTitle, it.OriginalLanguage, it.Overview, it.Tagline, it.OfficialRating, it.CustomRating, it.PresentationUniqueKey, it.SeriesPresentationUniqueKey, it.Album, it.ExternalID, it.ExternalSeriesID, it.ExternalServiceID, it.PreferredMetadataLanguage, it.PreferredMetadataCountryCode, it.SeriesName, it.SeasonName, it.EpisodeTitle, it.ShowID, it.Container, it.MediaSourceContainer,
		timeArg(d, it.StartDate), timeArg(d, it.EndDate), timeArg(d, it.PremiereDate), timeArg(d, it.DateCreated), timeArg(d, it.DateModified), timeArg(d, it.DateLastRefreshed), timeArg(d, it.DateLastSaved), timeArg(d, it.DateLastMediaAdded),
		it.ProductionYear, it.IndexNumber, it.ParentIndexNumber, it.Width, it.Height, it.TotalBitrate, it.InheritedParentalRatingValue, it.InheritedParentalRatingSubValue, it.AirsBeforeSeasonNumber, it.AirsAfterSeasonNumber, it.AirsBeforeEpisodeNumber, it.RunTimeTicks, it.Size, it.CommunityRating, it.CriticRating, it.LUFS, it.NormalizationGain,
		enumPtr(it.UnratedType), enum(it.SourceType), enumPtr(it.MediaType), enumPtr(it.ExtraType), enumPtr(it.VideoType), enumPtr(it.IsoType), enumPtr(it.Video3DFormat), enumPtr(it.SeriesStatus), enumPtr(it.ProgramAudio),
		boolArg(d, it.IsFolder), boolArg(d, it.IsVirtualItem), boolArg(d, it.IsInMixedFolder), boolArg(d, it.IsLocked), boolArg(d, it.IsMovie), boolArg(d, it.IsSeries), boolArg(d, it.IsRepeat), boolArg(d, it.IsPlaceHolder), boolArg(d, it.OpenAccess), string(extra), it.FileDevice, it.FileInode, timeArg(d, it.FileChangedAt),
	}
}

func uuidPtrArg(d Dialect, value *dto.UUID) any {
	if value == nil {
		return nil
	}
	return uuidArgDialect(d, *value)
}

func enumPtr(value any) any {
	switch v := value.(type) {
	case *item.UnratedItem:
		if v == nil {
			return nil
		}
		return enum(*v)
	case *item.MediaType:
		if v == nil {
			return nil
		}
		return enum(*v)
	case *item.ExtraType:
		if v == nil {
			return nil
		}
		return enum(*v)
	case *item.VideoType:
		if v == nil {
			return nil
		}
		return enum(*v)
	case *item.IsoType:
		if v == nil {
			return nil
		}
		return enum(*v)
	case *item.Video3DFormat:
		if v == nil {
			return nil
		}
		return enum(*v)
	case *item.SeriesStatus:
		if v == nil {
			return nil
		}
		return enum(*v)
	case *item.ProgramAudio:
		if v == nil {
			return nil
		}
		return enum(*v)
	case nil:
		return nil
	default:
		panic("unsupported item enum pointer")
	}
}

func (s *ItemStore) replaceSatellites(ctx context.Context, tx *sql.Tx, it item.Item) error {
	previousValues, err := s.mappedValueIDs(ctx, tx, []dto.UUID{it.ID})
	if err != nil {
		return err
	}
	for table, column := range map[string]string{"item_images": "item_id", "provider_ids": "item_id", "locked_fields": "item_id", "trailer_types": "item_id", "item_value_map": "item_id", "people_map": "item_id", "linked_children": "parent_item_id", "item_additional_parts": "item_id"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+column+" = "+s.ph(1), s.uuidArg(it.ID)); err != nil {
			return err
		}
	}
	for _, image := range it.ImageInfos {
		if _, err := tx.ExecContext(ctx, "INSERT INTO item_images(item_id,type,idx,path,date_modified,width,height,blurhash) VALUES ("+s.placeholders(8)+")", s.uuidArg(it.ID), image.Type.String(), image.Index, image.Path, timeArg(s.stores.Dialect(), image.DateModified), image.Width, image.Height, image.BlurHash); err != nil {
			return err
		}
	}
	for _, provider := range it.ProviderIDs.Snapshot() {
		if _, err := tx.ExecContext(ctx, "INSERT INTO provider_ids(item_id,provider,value) VALUES ("+s.placeholders(3)+")", s.uuidArg(it.ID), provider.Name, provider.Value); err != nil {
			return err
		}
	}
	for _, field := range it.LockedFields {
		if _, err := tx.ExecContext(ctx, "INSERT INTO locked_fields(item_id,field) VALUES ("+s.placeholders(2)+")", s.uuidArg(it.ID), field.String()); err != nil {
			return err
		}
	}
	for _, trailerType := range it.TrailerTypes {
		if _, err := tx.ExecContext(ctx, "INSERT INTO trailer_types(item_id,type) VALUES ("+s.placeholders(2)+")", s.uuidArg(it.ID), trailerType.String()); err != nil {
			return err
		}
	}
	for order, path := range it.AdditionalParts {
		if err := s.insertAdditionalPart(ctx, tx, it.ID, path, order); err != nil {
			return err
		}
	}
	for kind, values := range itemValueLists(it.Values) {
		for order, value := range values {
			if err := s.insertValue(ctx, tx, it.ID, kind, value, order); err != nil {
				return err
			}
		}
	}
	for listOrder, person := range it.People {
		if _, err := tx.ExecContext(ctx, "INSERT INTO people(id,name,clean_name) VALUES ("+s.placeholders(3)+") ON CONFLICT(id) DO UPDATE SET name=excluded.name, clean_name=excluded.clean_name", s.uuidArg(person.PersonID), person.Name, item.CleanName(person.Name)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO people_map(item_id,person_id,role,type,sort_order,list_order) VALUES ("+s.placeholders(6)+")", s.uuidArg(it.ID), s.uuidArg(person.PersonID), person.Role, person.Type.String(), person.SortOrder, listOrder); err != nil {
			return err
		}
	}
	for _, child := range it.LinkedChildren {
		if _, err := tx.ExecContext(ctx, "INSERT INTO linked_children(parent_item_id,child_item_id,type,sort_order) VALUES ("+s.placeholders(4)+")", s.uuidArg(it.ID), s.uuidArg(child.ItemID), child.Type.String(), child.SortOrder); err != nil {
			return err
		}
	}
	return s.deleteOrphanValues(ctx, tx, previousValues)
}

func (s *ItemStore) insertAdditionalPart(ctx context.Context, tx *sql.Tx, id dto.UUID, path string, order int) error {
	if path == "" {
		return errors.New("upsert item: additional part path is required")
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO item_additional_parts(item_id,path,sort_order) VALUES ("+s.placeholders(3)+")", s.uuidArg(id), path, order)
	return err
}

func itemValueLists(values item.ItemValues) map[string][]string {
	return map[string][]string{"genre": values.Genres, "tag": values.Tags, "inherited_tag": values.InheritedTags, "artist": values.Artists, "album_artist": values.AlbumArtists, "studio": values.Studios, "production_location": values.ProductionLocations}
}

func (s *ItemStore) insertValue(ctx context.Context, tx *sql.Tx, itemID dto.UUID, kind, value string, order int) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO item_values(kind,value,clean_value) VALUES ("+s.placeholders(3)+") ON CONFLICT(kind,value) DO UPDATE SET clean_value=excluded.clean_value", kind, value, item.CleanName(value)); err != nil {
		return err
	}
	var valueID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM item_values WHERE kind = "+s.ph(1)+" AND value = "+s.ph(2), kind, value).Scan(&valueID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO item_value_map(item_id,value_id,sort_order) VALUES ("+s.placeholders(3)+")", s.uuidArg(itemID), valueID, order)
	return err
}
