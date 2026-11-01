// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playback"
)

func (s *ItemStore) loadSatellites(ctx context.Context, db queryer, it *item.Item) error {
	if err := s.providers(ctx, db, it); err != nil {
		return err
	}
	var err error
	if it.ImageInfos, err = s.images(ctx, db, it.ID); err != nil {
		return err
	}
	if it.LockedFields, err = s.lockedFields(ctx, db, it.ID); err != nil {
		return err
	}
	if it.Values, err = s.itemValues(ctx, db, it.ID); err != nil {
		return err
	}
	if it.People, err = s.people(ctx, db, it.ID); err != nil {
		return err
	}
	if it.LinkedChildren, err = s.linkedChildren(ctx, db, it.ID); err != nil {
		return err
	}
	if it.TrailerTypes, err = s.trailerTypes(ctx, db, it.ID); err != nil {
		return err
	}
	if it.AdditionalParts, err = s.additionalParts(ctx, db, it.ID); err != nil {
		return err
	}
	return nil
}

// pageSatelliteBatchSize stays below SQLite's conservative 999-variable limit.
// Each satellite query consumes one variable per page item.
const pageSatelliteBatchSize = 900

// loadPageSatellites loads the relation satellites for a Query result page. Get
// and write-path callers retain the single-item loader above so their existing
// transaction and error contracts stay unchanged.
func (s *ItemStore) loadPageSatellites(ctx context.Context, db queryer, items []item.Item) error {
	for start := 0; start < len(items); start += pageSatelliteBatchSize {
		end := min(start+pageSatelliteBatchSize, len(items))
		if err := s.loadSatelliteBatch(ctx, db, items[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *ItemStore) loadSatelliteBatch(ctx context.Context, db queryer, items []item.Item) error {
	ids := make([]dto.UUID, len(items))
	byID := make(map[dto.UUID]*item.Item, len(items))
	for index := range items {
		ids[index] = items[index].ID
		byID[items[index].ID] = &items[index]
	}
	marks, args := s.sourceIDArgs(ids)

	if err := s.batchProviders(ctx, db, marks, args, byID); err != nil {
		return err
	}
	if err := s.batchImages(ctx, db, marks, args, byID); err != nil {
		return err
	}
	if err := s.batchLockedFields(ctx, db, marks, args, byID); err != nil {
		return err
	}
	if err := s.batchItemValues(ctx, db, marks, args, byID); err != nil {
		return err
	}
	if err := s.batchPeople(ctx, db, marks, args, byID); err != nil {
		return err
	}
	if err := s.batchLinkedChildren(ctx, db, marks, args, byID); err != nil {
		return err
	}
	if err := s.batchTrailerTypes(ctx, db, marks, args, byID); err != nil {
		return err
	}
	return s.batchAdditionalParts(ctx, db, marks, args, byID)
}

func pageSatelliteItem(byID map[dto.UUID]*item.Item, raw any) (*item.Item, error) {
	id, err := scanUUID(raw)
	if err != nil {
		return nil, err
	}
	stored, ok := byID[id]
	if !ok {
		return nil, errors.New("load page satellites: relation row item is outside page")
	}
	return stored, nil
}

func (s *ItemStore) batchProviders(ctx context.Context, db queryer, marks string, args []any, byID map[dto.UUID]*item.Item) error {
	rows, err := db.QueryContext(ctx, "SELECT item_id,provider,value FROM provider_ids WHERE item_id IN ("+marks+") ORDER BY item_id,provider", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var name, value string
		if err := rows.Scan(&raw, &name, &value); err != nil {
			return closeRows(rows, err)
		}
		stored, err := pageSatelliteItem(byID, raw)
		if err != nil {
			return closeRows(rows, err)
		}
		stored.ProviderIDs.Set(name, value)
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) batchImages(ctx context.Context, db queryer, marks string, args []any, byID map[dto.UUID]*item.Item) error {
	rows, err := db.QueryContext(ctx, "SELECT item_id,type,idx,path,date_modified,width,height,blurhash FROM item_images WHERE item_id IN ("+marks+") ORDER BY item_id,type,idx", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw, modified, blur any
		var typ string
		var info item.ImageInfo
		if err := rows.Scan(&raw, &typ, &info.Index, &info.Path, &modified, &info.Width, &info.Height, &blur); err != nil {
			return closeRows(rows, err)
		}
		stored, err := pageSatelliteItem(byID, raw)
		if err != nil {
			return closeRows(rows, err)
		}
		info.Type = imageType(typ)
		info.DateModified = optionalTime(modified, s.stores.Dialect())
		if !isNil(blur) {
			value := asString(blur)
			info.BlurHash = &value
		}
		stored.ImageInfos = append(stored.ImageInfos, info)
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) batchLockedFields(ctx context.Context, db queryer, marks string, args []any, byID map[dto.UUID]*item.Item) error {
	rows, err := db.QueryContext(ctx, "SELECT item_id,field FROM locked_fields WHERE item_id IN ("+marks+") ORDER BY item_id,CASE field WHEN 'Cast' THEN 0 WHEN 'Genres' THEN 1 WHEN 'ProductionLocations' THEN 2 WHEN 'Studios' THEN 3 WHEN 'Tags' THEN 4 WHEN 'Name' THEN 5 WHEN 'Overview' THEN 6 WHEN 'Runtime' THEN 7 WHEN 'OfficialRating' THEN 8 ELSE 9 END", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var name string
		if err := rows.Scan(&raw, &name); err != nil {
			return closeRows(rows, err)
		}
		stored, err := pageSatelliteItem(byID, raw)
		if err != nil {
			return closeRows(rows, err)
		}
		stored.LockedFields = append(stored.LockedFields, metadataField(name))
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) batchItemValues(ctx context.Context, db queryer, marks string, args []any, byID map[dto.UUID]*item.Item) error {
	rows, err := db.QueryContext(ctx, "SELECT m.item_id,v.kind,v.value FROM item_value_map m JOIN item_values v ON v.id=m.value_id WHERE m.item_id IN ("+marks+") ORDER BY m.item_id,v.kind,m.sort_order,m.value_id", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var kind, value string
		if err := rows.Scan(&raw, &kind, &value); err != nil {
			return closeRows(rows, err)
		}
		stored, err := pageSatelliteItem(byID, raw)
		if err != nil {
			return closeRows(rows, err)
		}
		switch kind {
		case "genre":
			stored.Values.Genres = append(stored.Values.Genres, value)
		case "tag":
			stored.Values.Tags = append(stored.Values.Tags, value)
		case "inherited_tag":
			stored.Values.InheritedTags = append(stored.Values.InheritedTags, value)
		case "artist":
			stored.Values.Artists = append(stored.Values.Artists, value)
		case "album_artist":
			stored.Values.AlbumArtists = append(stored.Values.AlbumArtists, value)
		case "studio":
			stored.Values.Studios = append(stored.Values.Studios, value)
		case "production_location":
			stored.Values.ProductionLocations = append(stored.Values.ProductionLocations, value)
		}
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) batchPeople(ctx context.Context, db queryer, marks string, args []any, byID map[dto.UUID]*item.Item) error {
	rows, err := db.QueryContext(ctx, "SELECT m.item_id,m.person_id,p.name,m.role,m.type,m.sort_order FROM people_map m JOIN people p ON p.id=m.person_id WHERE m.item_id IN ("+marks+") ORDER BY m.item_id,m.sort_order NULLS LAST,m.list_order,m.person_id,m.type", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rawItem, rawPerson, role, order any
		var typ, name string
		if err := rows.Scan(&rawItem, &rawPerson, &name, &role, &typ, &order); err != nil {
			return closeRows(rows, err)
		}
		stored, err := pageSatelliteItem(byID, rawItem)
		if err != nil {
			return closeRows(rows, err)
		}
		personID, err := scanUUID(rawPerson)
		if err != nil {
			return closeRows(rows, err)
		}
		credit := item.PersonCredit{PersonID: personID, Name: name, Type: personKind(typ), SortOrder: optionalInt(order)}
		if !isNil(role) {
			value := asString(role)
			credit.Role = &value
		}
		stored.People = append(stored.People, credit)
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) batchLinkedChildren(ctx context.Context, db queryer, marks string, args []any, byID map[dto.UUID]*item.Item) error {
	rows, err := db.QueryContext(ctx, "SELECT parent_item_id,child_item_id,type,sort_order FROM linked_children WHERE parent_item_id IN ("+marks+") ORDER BY parent_item_id,sort_order NULLS LAST,child_item_id", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rawParent, rawChild, order any
		var typ string
		if err := rows.Scan(&rawParent, &rawChild, &typ, &order); err != nil {
			return closeRows(rows, err)
		}
		stored, err := pageSatelliteItem(byID, rawParent)
		if err != nil {
			return closeRows(rows, err)
		}
		child, err := scanUUID(rawChild)
		if err != nil {
			return closeRows(rows, err)
		}
		stored.LinkedChildren = append(stored.LinkedChildren, item.LinkedChildRef{ItemID: child, Type: linkedType(typ), SortOrder: optionalInt(order)})
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) batchTrailerTypes(ctx context.Context, db queryer, marks string, args []any, byID map[dto.UUID]*item.Item) error {
	rows, err := db.QueryContext(ctx, "SELECT item_id,type FROM trailer_types WHERE item_id IN ("+marks+") ORDER BY item_id,type", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var name string
		if err := rows.Scan(&raw, &name); err != nil {
			return closeRows(rows, err)
		}
		stored, err := pageSatelliteItem(byID, raw)
		if err != nil {
			return closeRows(rows, err)
		}
		for value := range 5 {
			trailerType := item.TrailerType(value + 1)
			if trailerType.String() == name {
				stored.TrailerTypes = append(stored.TrailerTypes, trailerType)
				break
			}
		}
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) batchAdditionalParts(ctx context.Context, db queryer, marks string, args []any, byID map[dto.UUID]*item.Item) error {
	rows, err := db.QueryContext(ctx, "SELECT item_id,path FROM item_additional_parts WHERE item_id IN ("+marks+") ORDER BY item_id,sort_order", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var path string
		if err := rows.Scan(&raw, &path); err != nil {
			return closeRows(rows, err)
		}
		stored, err := pageSatelliteItem(byID, raw)
		if err != nil {
			return closeRows(rows, err)
		}
		stored.AdditionalParts = append(stored.AdditionalParts, path)
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) providers(ctx context.Context, db queryer, it *item.Item) error {
	rows, err := db.QueryContext(ctx, "SELECT provider,value FROM provider_ids WHERE item_id = "+s.ph(1)+" ORDER BY provider", s.uuidArg(it.ID))
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return closeRows(rows, err)
		}
		it.ProviderIDs.Set(name, value)
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) images(ctx context.Context, db queryer, id dto.UUID) ([]item.ImageInfo, error) {
	rows, err := db.QueryContext(ctx, "SELECT type,idx,path,date_modified,width,height,blurhash FROM item_images WHERE item_id = "+s.ph(1)+" ORDER BY type,idx", s.uuidArg(id))
	if err != nil {
		return nil, err
	}
	var out []item.ImageInfo
	for rows.Next() {
		var typ string
		var info item.ImageInfo
		var modified, blur any
		if err := rows.Scan(&typ, &info.Index, &info.Path, &modified, &info.Width, &info.Height, &blur); err != nil {
			return nil, closeRows(rows, err)
		}
		info.Type = imageType(typ)
		info.DateModified = optionalTime(modified, s.stores.Dialect())
		if !isNil(blur) {
			v := asString(blur)
			info.BlurHash = &v
		}
		out = append(out, info)
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) lockedFields(ctx context.Context, db queryer, id dto.UUID) ([]item.MetadataField, error) {
	rows, err := db.QueryContext(ctx, "SELECT field FROM locked_fields WHERE item_id = "+s.ph(1)+" ORDER BY CASE field WHEN 'Cast' THEN 0 WHEN 'Genres' THEN 1 WHEN 'ProductionLocations' THEN 2 WHEN 'Studios' THEN 3 WHEN 'Tags' THEN 4 WHEN 'Name' THEN 5 WHEN 'Overview' THEN 6 WHEN 'Runtime' THEN 7 WHEN 'OfficialRating' THEN 8 ELSE 9 END", s.uuidArg(id))
	if err != nil {
		return nil, err
	}
	var out []item.MetadataField
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, metadataField(name))
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) itemValues(ctx context.Context, db queryer, id dto.UUID) (item.ItemValues, error) {
	rows, err := db.QueryContext(ctx, "SELECT v.kind,v.value FROM item_value_map m JOIN item_values v ON v.id=m.value_id WHERE m.item_id = "+s.ph(1)+" ORDER BY v.kind,m.sort_order,m.value_id", s.uuidArg(id))
	if err != nil {
		return item.ItemValues{}, err
	}
	var out item.ItemValues
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return out, closeRows(rows, err)
		}
		switch kind {
		case "genre":
			out.Genres = append(out.Genres, value)
		case "tag":
			out.Tags = append(out.Tags, value)
		case "inherited_tag":
			out.InheritedTags = append(out.InheritedTags, value)
		case "artist":
			out.Artists = append(out.Artists, value)
		case "album_artist":
			out.AlbumArtists = append(out.AlbumArtists, value)
		case "studio":
			out.Studios = append(out.Studios, value)
		case "production_location":
			out.ProductionLocations = append(out.ProductionLocations, value)
		}
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) people(ctx context.Context, db queryer, id dto.UUID) ([]item.PersonCredit, error) {
	rows, err := db.QueryContext(ctx, "SELECT m.person_id,p.name,m.role,m.type,m.sort_order FROM people_map m JOIN people p ON p.id=m.person_id WHERE m.item_id = "+s.ph(1)+" ORDER BY m.sort_order NULLS LAST,m.list_order,m.person_id,m.type", s.uuidArg(id))
	if err != nil {
		return nil, err
	}
	var out []item.PersonCredit
	for rows.Next() {
		var raw, role, order any
		var typ, name string
		if err := rows.Scan(&raw, &name, &role, &typ, &order); err != nil {
			return nil, closeRows(rows, err)
		}
		personID, err := scanUUID(raw)
		if err != nil {
			return nil, closeRows(rows, err)
		}
		credit := item.PersonCredit{PersonID: personID, Name: name, Type: personKind(typ), SortOrder: optionalInt(order)}
		if !isNil(role) {
			v := asString(role)
			credit.Role = &v
		}
		out = append(out, credit)
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) linkedChildren(ctx context.Context, db queryer, id dto.UUID) ([]item.LinkedChildRef, error) {
	rows, err := db.QueryContext(ctx, "SELECT child_item_id,type,sort_order FROM linked_children WHERE parent_item_id = "+s.ph(1)+" ORDER BY sort_order NULLS LAST,child_item_id", s.uuidArg(id))
	if err != nil {
		return nil, err
	}
	var out []item.LinkedChildRef
	for rows.Next() {
		var raw, order any
		var typ string
		if err := rows.Scan(&raw, &typ, &order); err != nil {
			return nil, closeRows(rows, err)
		}
		child, err := scanUUID(raw)
		if err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, item.LinkedChildRef{ItemID: child, Type: linkedType(typ), SortOrder: optionalInt(order)})
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) trailerTypes(ctx context.Context, db queryer, id dto.UUID) ([]item.TrailerType, error) {
	rows, err := db.QueryContext(ctx, "SELECT type FROM trailer_types WHERE item_id = "+s.ph(1)+" ORDER BY type", s.uuidArg(id))
	if err != nil {
		return nil, err
	}
	var out []item.TrailerType
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, closeRows(rows, err)
		}
		for value := range 5 {
			trailerType := item.TrailerType(value + 1)
			if trailerType.String() == name {
				out = append(out, trailerType)
				break
			}
		}
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) additionalParts(ctx context.Context, db queryer, id dto.UUID) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT path FROM item_additional_parts WHERE item_id = "+s.ph(1)+" ORDER BY sort_order", s.uuidArg(id))
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, path)
	}
	return out, closeRows(rows, rows.Err())
}

func imageType(value string) item.ImageType {
	for i := 0; i <= int(item.ImageTypeProfile); i++ {
		v := item.ImageType(i)
		if v.String() == value {
			return v
		}
	}
	return item.ImageTypePrimary
}

func metadataField(value string) item.MetadataField {
	for i := range 9 {
		v := item.MetadataField(i)
		if v.String() == value {
			return v
		}
	}
	return item.MetadataField(0)
}

func personKind(value string) item.PersonKind {
	for i := 0; i <= int(item.PersonKindNarrator); i++ {
		v := item.PersonKind(i)
		if v.String() == value {
			return v
		}
	}
	return item.PersonKindUnknown
}

func linkedType(value string) item.LinkedChildType {
	for i := 0; i < 16; i++ {
		v := item.LinkedChildType(i)
		if v.String() == value {
			return v
		}
	}
	return item.LinkedChildType(0)
}

func (s *ItemStore) streams(ctx context.Context, db queryer, id dto.UUID) ([]playback.MediaStream, error) {
	return s.streamsWithLock(ctx, db, id, false)
}

func (s *ItemStore) streamsWithLock(ctx context.Context, db queryer, id dto.UUID, lockRows bool) ([]playback.MediaStream, error) {
	query := "SELECT stream_index,stream_type,codec,language,display_title,is_default,is_forced,is_external,bitrate,width,height,channels,sample_rate,extra FROM media_streams WHERE item_id = " + s.ph(1) + " ORDER BY stream_index"
	if lockRows && s.ph(1) != "?" {
		query += " FOR SHARE"
	}
	rows, err := db.QueryContext(ctx, query, s.uuidArg(id))
	if err != nil {
		return nil, err
	}
	out := make([]playback.MediaStream, 0)
	for rows.Next() {
		var index int
		var streamType string
		var codec, language, title, defaultValue, forced, external, bitrate, width, height, channels, sampleRate, extra any
		if err := rows.Scan(&index, &streamType, &codec, &language, &title, &defaultValue, &forced, &external, &bitrate, &width, &height, &channels, &sampleRate, &extra); err != nil {
			return nil, closeRows(rows, err)
		}
		stream, err := streamFromColumns(index, streamType, codec, language, title, defaultValue, forced, external, bitrate, width, height, channels, sampleRate, asString(extra))
		if err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, stream)
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) replaceStreams(ctx context.Context, id dto.UUID, streams []playback.MediaStream) (err error) {
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	if err := s.lockDerivedItems(ctx, tx, id); err != nil {
		return err
	}
	if err := s.requireItem(ctx, tx, id, "replace streams"); err != nil {
		return err
	}
	beforeRevision, found, err := s.currentSourceRevisionTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := s.replaceStreamsTx(ctx, tx, id, streams); err != nil {
		return err
	}
	invalidated, err := s.sourceRevisionChangedTx(ctx, tx, id, beforeRevision, found)
	if err != nil {
		return err
	}
	if invalidated {
		if err := s.deleteDerivedMetadataFor(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *ItemStore) replaceStreamsTx(ctx context.Context, tx *sql.Tx, id dto.UUID, streams []playback.MediaStream) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM media_streams WHERE item_id = "+s.ph(1), s.uuidArg(id)); err != nil {
		return err
	}
	for _, stream := range streams {
		extra, err := streamExtra(stream)
		if err != nil {
			return err
		}
		typeJSON, err := json.Marshal(stream.Type)
		if err != nil {
			return err
		}
		streamType := strings.Trim(string(typeJSON), "\"")
		if _, err = tx.ExecContext(ctx, "INSERT INTO media_streams(item_id,stream_index,stream_type,codec,language,display_title,is_default,is_forced,is_external,bitrate,width,height,channels,sample_rate,extra) VALUES ("+s.placeholders(15)+")", s.uuidArg(id), stream.Index, streamType, stream.Codec, stream.Language, stream.Title, boolArg(s.stores.Dialect(), stream.IsDefault), boolArg(s.stores.Dialect(), stream.IsForced), boolArg(s.stores.Dialect(), stream.IsExternal), stream.BitRate, stream.Width, stream.Height, stream.Channels, stream.SampleRate, extra); err != nil {
			return err
		}
	}
	return nil
}

func (s *ItemStore) chapters(ctx context.Context, db queryer, id dto.UUID) ([]Chapter, error) {
	rows, err := db.QueryContext(ctx, "SELECT chapter_index,start_position_ticks,name,image_path,extra FROM chapters WHERE item_id = "+s.ph(1)+" ORDER BY chapter_index", s.uuidArg(id))
	if err != nil {
		return nil, err
	}
	var out []Chapter
	for rows.Next() {
		var x Chapter
		var name, path, extra any
		if err := rows.Scan(&x.Index, &x.StartPositionTicks, &name, &path, &extra); err != nil {
			return nil, closeRows(rows, err)
		}
		x.Name = optionalString(name)
		x.ImagePath = optionalString(path)
		x.Extra = json.RawMessage(asString(extra))
		out = append(out, x)
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) replaceChapters(ctx context.Context, id dto.UUID, chapters []Chapter) (err error) {
	for _, chapter := range chapters {
		if len(chapter.Extra) != 0 && !json.Valid(chapter.Extra) {
			return errors.New("replace chapters: extra must be valid JSON")
		}
	}
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	if err := s.requireItem(ctx, tx, id, "replace chapters"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM chapters WHERE item_id = "+s.ph(1), s.uuidArg(id)); err != nil {
		return err
	}
	for _, chapter := range chapters {
		if _, err = tx.ExecContext(ctx, "INSERT INTO chapters(item_id,chapter_index,start_position_ticks,name,image_path,extra) VALUES ("+s.placeholders(6)+")", s.uuidArg(id), chapter.Index, chapter.StartPositionTicks, chapter.Name, chapter.ImagePath, jsonText(chapter.Extra)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *ItemStore) attachments(ctx context.Context, db queryer, id dto.UUID) ([]playback.MediaAttachment, error) {
	rows, err := db.QueryContext(ctx, "SELECT attachment_index,filename,mime_type,extra FROM media_attachments WHERE item_id = "+s.ph(1)+" ORDER BY attachment_index", s.uuidArg(id))
	if err != nil {
		return nil, err
	}
	var out []playback.MediaAttachment
	for rows.Next() {
		var index int
		var filename, mimeType, extra any
		if err := rows.Scan(&index, &filename, &mimeType, &extra); err != nil {
			return nil, closeRows(rows, err)
		}
		attachment, err := attachmentFromColumns(index, filename, mimeType, asString(extra))
		if err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, attachment)
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) replaceAttachments(ctx context.Context, id dto.UUID, attachments []playback.MediaAttachment) (err error) {
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	if err := s.requireItem(ctx, tx, id, "replace attachments"); err != nil {
		return err
	}
	if err := s.replaceAttachmentsTx(ctx, tx, id, attachments); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *ItemStore) replaceAttachmentsTx(ctx context.Context, tx *sql.Tx, id dto.UUID, attachments []playback.MediaAttachment) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM media_attachments WHERE item_id = "+s.ph(1), s.uuidArg(id)); err != nil {
		return err
	}
	for _, attachment := range attachments {
		extra, err := attachmentExtra(attachment)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO media_attachments(item_id,attachment_index,filename,mime_type,extra) VALUES ("+s.placeholders(5)+")", s.uuidArg(id), attachment.Index, attachment.FileName, attachment.MimeType, extra); err != nil {
			return err
		}
	}
	return nil
}

func (s *ItemStore) replaceMediaTx(ctx context.Context, tx *sql.Tx, media ScannedChildMedia) error {
	if err := s.requireItem(ctx, tx, media.ItemID, "reconcile scanned children media"); err != nil {
		return err
	}
	if err := s.replaceStreamsTx(ctx, tx, media.ItemID, media.Streams); err != nil {
		return err
	}
	return s.replaceAttachmentsTx(ctx, tx, media.ItemID, media.Attachments)
}

func optionalString(v any) *string {
	if isNil(v) {
		return nil
	}
	s := asString(v)
	return &s
}

func jsonText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}
