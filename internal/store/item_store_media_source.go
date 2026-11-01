// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playback"
)

// MediaSourceRecord loads an item and its media satellites through one
// repeatable-read transaction, preventing mixed source/stream snapshots.
func (s *ItemStore) MediaSourceRecord(ctx context.Context, id dto.UUID) (result playback.MediaSourceRecord, err error) {
	tx, err := s.stores.Reader().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return playback.MediaSourceRecord{}, err
	}
	defer rollbackOnReturn(tx, &err)

	stored, err := s.mediaSourceItem(ctx, tx, id)
	if err != nil {
		return playback.MediaSourceRecord{}, err
	}
	streams, err := s.streams(ctx, tx, id)
	if err != nil {
		return playback.MediaSourceRecord{}, err
	}
	attachments, err := s.attachments(ctx, tx, id)
	if err != nil {
		return playback.MediaSourceRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return playback.MediaSourceRecord{}, err
	}
	return mediaSourceRecord(stored, playback.MediaSourceDefault, streams, attachments)
}

// MediaSourceRecords loads an alternate-version source graph and every source
// satellite through a single repeatable-read transaction. Per-source satellite
// loads are intentionally batched to avoid an N+1 query shape.
func (s *ItemStore) MediaSourceRecords(ctx context.Context, id dto.UUID) (result []playback.MediaSourceRecord, err error) {
	tx, err := s.stores.Reader().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer rollbackOnReturn(tx, &err)

	requested, err := s.mediaSourceItem(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	items, err := s.sourceGraphItems(ctx, tx, requested)
	if err != nil {
		return nil, err
	}
	linkTypes, linkedIDs, err := s.sourceGraphLinks(ctx, tx, sourceItemIDs(items))
	if err != nil {
		return nil, err
	}
	if len(linkedIDs) != 0 {
		linked, linkedErr := s.mediaSourceItemsByID(ctx, tx, linkedIDs)
		if linkedErr != nil {
			return nil, linkedErr
		}
		items = appendUniqueItems(items, linked)
	}

	ids := sourceItemIDs(items)
	streamMap, err := s.sourceStreams(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	attachmentMap, err := s.sourceAttachments(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	result = make([]playback.MediaSourceRecord, 0, len(items))
	for _, stored := range items {
		record, recordErr := mediaSourceRecord(stored, mediaSourceType(requested.ID, stored.ID, linkTypes), streamMap[stored.ID], attachmentMap[stored.ID])
		if recordErr != nil {
			return nil, recordErr
		}
		result = append(result, record)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

type mediaSourceLink struct {
	childID      dto.UUID
	relationship item.LinkedChildType
}

// MediaSourceRecordsPage resolves default and presentation-equivalent sources
// for an entire DTO page. It keeps the graph and both media satellite families
// set-based instead of issuing MediaSourceRecords once per displayed item.
func (s *ItemStore) MediaSourceRecordsPage(ctx context.Context, requested []dto.UUID) (result map[dto.UUID][]playback.MediaSourceRecord, err error) {
	result = make(map[dto.UUID][]playback.MediaSourceRecord, len(requested))
	if len(requested) == 0 {
		return result, nil
	}
	tx, err := s.stores.Reader().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer rollbackOnReturn(tx, &err)
	byID := make(map[dto.UUID]item.Item)
	for start := 0; start < len(requested); start += 400 {
		end := min(start+400, len(requested))
		statement, allArgs := s.MediaSourceRecordsPageStatement(requested[start:end])
		rows, queryErr := tx.QueryContext(ctx, statement, allArgs...)
		if queryErr != nil {
			return nil, queryErr
		}
		for rows.Next() {
			stored, scanErr := scanItem(rows, s.stores.Dialect())
			if scanErr != nil {
				return nil, closeRows(rows, scanErr)
			}
			byID[stored.ID] = stored
		}
		if queryErr = closeRows(rows, rows.Err()); queryErr != nil {
			return nil, queryErr
		}
	}
	initial := make([]item.Item, 0, len(byID))
	for _, stored := range byID {
		initial = append(initial, stored)
	}
	linkedByParent := make(map[dto.UUID][]mediaSourceLink)
	linkedIDs := make([]dto.UUID, 0)
	for start := 0; start < len(initial); start += pageSatelliteBatchSize {
		end := min(start+pageSatelliteBatchSize, len(initial))
		marks, args := s.sourceIDArgs(sourceItemIDs(initial[start:end]))
		args = append(args, item.LinkedChildTypeLocalAlternateVersion.String(), item.LinkedChildTypeLinkedAlternateVersion.String())
		rows, linkErr := tx.QueryContext(ctx, "SELECT parent_item_id,child_item_id,type FROM linked_children WHERE parent_item_id IN ("+marks+") AND type IN ("+s.ph(len(args)-1)+", "+s.ph(len(args))+") ORDER BY parent_item_id,sort_order NULLS LAST,child_item_id", args...)
		if linkErr != nil {
			return nil, linkErr
		}
		for rows.Next() {
			var parentRaw, childRaw any
			var rawType string
			if linkErr = rows.Scan(&parentRaw, &childRaw, &rawType); linkErr != nil {
				return nil, closeRows(rows, linkErr)
			}
			parent, parentErr := scanUUID(parentRaw)
			if parentErr != nil {
				return nil, closeRows(rows, parentErr)
			}
			child, childErr := scanUUID(childRaw)
			if childErr != nil {
				return nil, closeRows(rows, childErr)
			}
			linkedByParent[parent] = append(linkedByParent[parent], mediaSourceLink{childID: child, relationship: linkedType(rawType)})
			linkedIDs = append(linkedIDs, child)
		}
		if linkErr = closeRows(rows, rows.Err()); linkErr != nil {
			return nil, linkErr
		}
	}
	for start := 0; start < len(linkedIDs); start += pageSatelliteBatchSize {
		end := min(start+pageSatelliteBatchSize, len(linkedIDs))
		linked, linkedErr := s.mediaSourceItemsByID(ctx, tx, linkedIDs[start:end])
		if linkedErr != nil {
			return nil, linkedErr
		}
		for _, stored := range linked {
			byID[stored.ID] = stored
		}
	}
	all := make([]item.Item, 0, len(byID))
	for _, stored := range byID {
		all = append(all, stored)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID.String() < all[j].ID.String() })
	streamMap := make(map[dto.UUID][]playback.MediaStream)
	attachmentMap := make(map[dto.UUID][]playback.MediaAttachment)
	allIDs := sourceItemIDs(all)
	for start := 0; start < len(allIDs); start += pageSatelliteBatchSize {
		end := min(start+pageSatelliteBatchSize, len(allIDs))
		streams, streamErr := s.sourceStreams(ctx, tx, allIDs[start:end])
		if streamErr != nil {
			return nil, streamErr
		}
		for id, values := range streams {
			streamMap[id] = values
		}
		attachments, attachmentErr := s.sourceAttachments(ctx, tx, allIDs[start:end])
		if attachmentErr != nil {
			return nil, attachmentErr
		}
		for id, values := range attachments {
			attachmentMap[id] = values
		}
	}
	for _, requestedID := range requested {
		root, ok := byID[requestedID]
		if !ok {
			continue
		}
		candidates, seen := []item.Item{root}, map[dto.UUID]struct{}{root.ID: {}}
		appendCandidate := func(candidate item.Item) {
			if _, exists := seen[candidate.ID]; !exists {
				seen[candidate.ID] = struct{}{}
				candidates = append(candidates, candidate)
			}
		}
		if root.PresentationUniqueKey != nil && *root.PresentationUniqueKey != "" {
			for _, candidate := range all {
				if candidate.PresentationUniqueKey != nil && *candidate.PresentationUniqueKey == *root.PresentationUniqueKey {
					appendCandidate(candidate)
				}
			}
		}
		parents := append([]item.Item(nil), candidates...)
		typeParents := append([]item.Item(nil), parents...)
		sort.Slice(typeParents, func(i, j int) bool { return typeParents[i].ID.String() < typeParents[j].ID.String() })
		linkTypes := make(map[dto.UUID]item.LinkedChildType)
		for _, parent := range typeParents {
			for _, linked := range linkedByParent[parent.ID] {
				if _, exists := linkTypes[linked.childID]; !exists {
					linkTypes[linked.childID] = linked.relationship
				}
			}
		}
		for _, parent := range parents {
			for _, linked := range linkedByParent[parent.ID] {
				if child, exists := byID[linked.childID]; exists {
					appendCandidate(child)
				}
			}
		}
		sort.SliceStable(candidates[1:], func(i, j int) bool {
			left, right := candidates[i+1], candidates[j+1]
			if dtoSourceFileRank(left) != dtoSourceFileRank(right) {
				return dtoSourceFileRank(left) < dtoSourceFileRank(right)
			}
			if dtoSourceThreeDRank(left) != dtoSourceThreeDRank(right) {
				return dtoSourceThreeDRank(left) < dtoSourceThreeDRank(right)
			}
			return dtoSourceWidth(streamMap[left.ID]) > dtoSourceWidth(streamMap[right.ID])
		})
		for _, stored := range candidates {
			record, recordErr := mediaSourceRecord(stored, mediaSourceType(requestedID, stored.ID, linkTypes), streamMap[stored.ID], attachmentMap[stored.ID])
			if recordErr != nil {
				return nil, recordErr
			}
			result[requestedID] = append(result[requestedID], record)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func dtoSourceFileRank(value item.Item) int {
	if value.VideoType == nil || *value.VideoType == item.VideoTypeVideoFile {
		return 0
	}
	return 1
}

func dtoSourceThreeDRank(value item.Item) int {
	if value.Video3DFormat == nil {
		return 0
	}
	return 1
}

func dtoSourceWidth(streams []playback.MediaStream) int {
	for _, stream := range streams {
		if stream.Type == playback.StreamTypeVideo && stream.Width != nil {
			return *stream.Width
		}
	}
	return 0
}

func mediaSourceType(requestedID, sourceID dto.UUID, linkTypes map[dto.UUID]item.LinkedChildType) playback.MediaSourceType {
	if sourceID == requestedID {
		return playback.MediaSourceDefault
	}
	if linkTypes[requestedID] == item.LinkedChildTypeLinkedAlternateVersion || linkTypes[sourceID] == item.LinkedChildTypeLinkedAlternateVersion {
		return playback.MediaSourceGrouping
	}
	return playback.MediaSourceDefault
}

func (s *ItemStore) mediaSourceItem(ctx context.Context, tx *sql.Tx, id dto.UUID) (item.Item, error) {
	stored, err := scanItem(tx.QueryRowContext(ctx, "SELECT "+strings.Join(itemColumns, ", ")+" FROM items WHERE id = "+s.ph(1), s.uuidArg(id)), s.stores.Dialect())
	if errors.Is(err, sql.ErrNoRows) {
		return item.Item{}, ErrNotFound
	}
	return stored, err
}

func (s *ItemStore) sourceGraphItems(ctx context.Context, tx *sql.Tx, requested item.Item) ([]item.Item, error) {
	conditions := []string{"id = " + s.ph(1)}
	args := []any{s.uuidArg(requested.ID)}
	if requested.PrimaryVersionID != nil {
		primary := s.uuidArg(*requested.PrimaryVersionID)
		index := len(args) + 1
		conditions = append(conditions, "id = "+s.ph(index), "primary_version_id = "+s.ph(index+1))
		args = append(args, primary, primary)
	}
	if requested.PresentationUniqueKey != nil && *requested.PresentationUniqueKey != "" {
		args = append(args, *requested.PresentationUniqueKey)
		conditions = append(conditions, "presentation_unique_key = "+s.ph(len(args)))
	}
	return s.mediaSourceItems(ctx, tx, "SELECT "+strings.Join(itemColumns, ", ")+" FROM items WHERE "+strings.Join(conditions, " OR ")+" ORDER BY id", args...)
}

func (s *ItemStore) sourceGraphLinks(ctx context.Context, tx *sql.Tx, ids []dto.UUID) (map[dto.UUID]item.LinkedChildType, []dto.UUID, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	marks, args := s.sourceIDArgs(ids)
	rows, err := tx.QueryContext(ctx, "SELECT child_item_id,type FROM linked_children WHERE parent_item_id IN ("+marks+") AND type IN ("+s.ph(len(args)+1)+", "+s.ph(len(args)+2)+") ORDER BY parent_item_id,sort_order NULLS LAST,child_item_id", append(args, item.LinkedChildTypeLocalAlternateVersion.String(), item.LinkedChildTypeLinkedAlternateVersion.String())...)
	if err != nil {
		return nil, nil, err
	}
	types := make(map[dto.UUID]item.LinkedChildType)
	ordered := make([]dto.UUID, 0)
	for rows.Next() {
		var raw any
		var rawType string
		if err := rows.Scan(&raw, &rawType); err != nil {
			return nil, nil, closeRows(rows, err)
		}
		child, err := scanUUID(raw)
		if err != nil {
			return nil, nil, closeRows(rows, err)
		}
		if _, exists := types[child]; !exists {
			types[child] = linkedType(rawType)
			ordered = append(ordered, child)
		}
	}
	return types, ordered, closeRows(rows, rows.Err())
}

func (s *ItemStore) mediaSourceItemsByID(ctx context.Context, tx *sql.Tx, ids []dto.UUID) ([]item.Item, error) {
	marks, args := s.sourceIDArgs(ids)
	return s.mediaSourceItems(ctx, tx, "SELECT "+strings.Join(itemColumns, ", ")+" FROM items WHERE id IN ("+marks+") ORDER BY id", args...)
}

func (s *ItemStore) mediaSourceItems(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]item.Item, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	items := make([]item.Item, 0)
	for rows.Next() {
		stored, err := scanItem(rows, s.stores.Dialect())
		if err != nil {
			return nil, closeRows(rows, err)
		}
		items = append(items, stored)
	}
	return items, closeRows(rows, rows.Err())
}

func (s *ItemStore) sourceStreams(ctx context.Context, tx *sql.Tx, ids []dto.UUID) (map[dto.UUID][]playback.MediaStream, error) {
	marks, args := s.sourceIDArgs(ids)
	rows, err := tx.QueryContext(ctx, "SELECT item_id,stream_index,stream_type,codec,language,display_title,is_default,is_forced,is_external,bitrate,width,height,channels,sample_rate,extra FROM media_streams WHERE item_id IN ("+marks+") ORDER BY item_id,stream_index", args...)
	if err != nil {
		return nil, err
	}
	result := make(map[dto.UUID][]playback.MediaStream, len(ids))
	for _, id := range ids {
		result[id] = make([]playback.MediaStream, 0)
	}
	for rows.Next() {
		var raw any
		var index int
		var streamType string
		var codec, language, title, defaultValue, forced, external, bitrate, width, height, channels, sampleRate, extra any
		if err := rows.Scan(&raw, &index, &streamType, &codec, &language, &title, &defaultValue, &forced, &external, &bitrate, &width, &height, &channels, &sampleRate, &extra); err != nil {
			return nil, closeRows(rows, err)
		}
		itemID, err := scanUUID(raw)
		if err != nil {
			return nil, closeRows(rows, err)
		}
		stream, err := streamFromColumns(index, streamType, codec, language, title, defaultValue, forced, external, bitrate, width, height, channels, sampleRate, asString(extra))
		if err != nil {
			return nil, closeRows(rows, err)
		}
		result[itemID] = append(result[itemID], stream)
	}
	return result, closeRows(rows, rows.Err())
}

func (s *ItemStore) sourceAttachments(ctx context.Context, tx *sql.Tx, ids []dto.UUID) (map[dto.UUID][]playback.MediaAttachment, error) {
	marks, args := s.sourceIDArgs(ids)
	rows, err := tx.QueryContext(ctx, "SELECT item_id,attachment_index,filename,mime_type,extra FROM media_attachments WHERE item_id IN ("+marks+") ORDER BY item_id,attachment_index", args...)
	if err != nil {
		return nil, err
	}
	result := make(map[dto.UUID][]playback.MediaAttachment)
	for rows.Next() {
		var raw, filename, mimeType, extra any
		var index int
		if err := rows.Scan(&raw, &index, &filename, &mimeType, &extra); err != nil {
			return nil, closeRows(rows, err)
		}
		itemID, err := scanUUID(raw)
		if err != nil {
			return nil, closeRows(rows, err)
		}
		attachment, err := attachmentFromColumns(index, filename, mimeType, asString(extra))
		if err != nil {
			return nil, closeRows(rows, err)
		}
		result[itemID] = append(result[itemID], attachment)
	}
	return result, closeRows(rows, rows.Err())
}

// MediaSourceRecordsPageStatement returns the statement that reads the
// requested items and the items that share their PresentationUniqueKey, with
// its arguments. Each branch of the UNION reads an index keyed by its own
// column. In the form "id IN (...) OR presentation_unique_key IN (...) ORDER
// BY id", SQLite walks the whole primary-key index instead, so every DTO page
// read the whole items table. The plan tests read the statement here.
func (s *ItemStore) MediaSourceRecordsPageStatement(ids []dto.UUID) (string, []any) {
	marks, args := s.sourceIDArgs(ids)
	secondMarks := make([]string, len(args))
	for index := range secondMarks {
		secondMarks[index] = s.ph(len(args) + index + 1)
	}
	args = append(args, args...)
	return "SELECT " + strings.Join(itemColumns, ", ") + " FROM items WHERE id IN (SELECT id FROM items WHERE id IN (" + marks + ") UNION SELECT id FROM items WHERE presentation_unique_key IN (SELECT presentation_unique_key FROM items WHERE id IN (" + strings.Join(secondMarks, ", ") + ") AND presentation_unique_key IS NOT NULL)) ORDER BY id", args
}

func (s *ItemStore) sourceIDArgs(ids []dto.UUID) (string, []any) {
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for index, id := range ids {
		marks[index] = s.ph(index + 1)
		args[index] = s.uuidArg(id)
	}
	return strings.Join(marks, ", "), args
}

func sourceItemIDs(items []item.Item) []dto.UUID {
	ids := make([]dto.UUID, len(items))
	for index, stored := range items {
		ids[index] = stored.ID
	}
	return ids
}

func appendUniqueItems(items []item.Item, additions []item.Item) []item.Item {
	seen := make(map[dto.UUID]struct{}, len(items)+len(additions))
	for _, stored := range items {
		seen[stored.ID] = struct{}{}
	}
	for _, stored := range additions {
		if _, exists := seen[stored.ID]; !exists {
			seen[stored.ID] = struct{}{}
			items = append(items, stored)
		}
	}
	return items
}

func mediaSourceRecord(stored item.Item, sourceType playback.MediaSourceType, streams []playback.MediaStream, attachments []playback.MediaAttachment) (playback.MediaSourceRecord, error) {
	revision, err := sourceRevision(stored, streams)
	if err != nil {
		return playback.MediaSourceRecord{}, err
	}
	if !hasCompleteSourceIdentity(stored) {
		revision = ""
	}
	container := stored.MediaSourceContainer
	if container == nil {
		container = stored.Container
	}
	return playback.MediaSourceRecord{
		ID: stored.ID, Path: stored.Path, Name: stored.Name, Container: container, Size: stored.Size, RunTimeTicks: stored.RunTimeTicks,
		TotalBitrate: stored.TotalBitrate, DateModified: stored.DateModified, FileDevice: stored.FileDevice, FileInode: stored.FileInode,
		FileChangedAt: stored.FileChangedAt, SourceRevision: revision, Type: sourceType, VideoType: playbackVideoType(stored.VideoType),
		IsoType: playbackIsoType(stored.IsoType), Video3DFormat: playbackVideo3DFormat(stored.Video3DFormat), MediaStreams: streams, MediaAttachments: attachments,
		Downloadable: item.CanDownload(stored),
	}, nil
}

func playbackVideoType(value *item.VideoType) *playback.VideoType {
	if value == nil {
		return nil
	}
	result := playback.VideoType(*value)
	return &result
}

func playbackIsoType(value *item.IsoType) *playback.IsoType {
	if value == nil {
		return nil
	}
	result := playback.IsoType(*value)
	return &result
}

func playbackVideo3DFormat(value *item.Video3DFormat) *playback.Video3DFormat {
	if value == nil {
		return nil
	}
	result := playback.Video3DFormat(*value)
	return &result
}

var _ = fmt.Sprintf

// DTOProjectionBatch is the page-scoped persistence projection consumed by
// dtobuild. Each requested relation is read once per bounded ID chunk, so an
// item page never turns optional DTO fields into per-item reader queries.
type DTOProjectionBatch struct {
	Streams              map[dto.UUID][]playback.MediaStream
	Attachments          map[dto.UUID][]playback.MediaAttachment
	UserData             map[dto.UUID]map[string]UserData
	FolderUserDataCounts map[dto.UUID]FolderUserDataCounts
	ChildCounts          map[dto.UUID]int
	RecursiveCounts      map[dto.UUID]int
}

// FolderUserDataCounts is the visible leaf played/total aggregate for one
// folder. It is intentionally separate from direct UserData: folders derive
// their played state from child leaves rather than their own persisted row.
type FolderUserDataCounts struct {
	Played int
	Total  int
}

// DTOProjections reads only the satellite families selected by DTO options.
// IDs are chunked below SQLite's variable limit and all rows are ordered by
// stable store keys before their maps are populated. Folder played counts are
// read only for the IDs in folderCounts, as Jellyfin's DtoService batches
// them only for folders whose UserData derives from their children.
func (s *ItemStore) DTOProjections(ctx context.Context, ids []dto.UUID, userID *dto.UUID, streams, userData, childCounts, recursiveCounts bool, folderCounts []dto.UUID, users ...*User) (result DTOProjectionBatch, err error) {
	result.Streams = make(map[dto.UUID][]playback.MediaStream)
	result.Attachments = make(map[dto.UUID][]playback.MediaAttachment)
	result.UserData = make(map[dto.UUID]map[string]UserData)
	result.FolderUserDataCounts = make(map[dto.UUID]FolderUserDataCounts)
	result.ChildCounts = make(map[dto.UUID]int)
	result.RecursiveCounts = make(map[dto.UUID]int)
	if len(ids) == 0 {
		return result, nil
	}
	tx, err := s.stores.Reader().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return DTOProjectionBatch{}, err
	}
	defer rollbackOnReturn(tx, &err)
	for start := 0; start < len(ids); start += pageSatelliteBatchSize {
		end := min(start+pageSatelliteBatchSize, len(ids))
		batch := ids[start:end]
		if streams {
			if err = s.dtoProjectionStreams(ctx, tx, batch, result.Streams); err != nil {
				return DTOProjectionBatch{}, err
			}
			attachments, attachmentErr := s.sourceAttachments(ctx, tx, batch)
			if attachmentErr != nil {
				return DTOProjectionBatch{}, attachmentErr
			}
			for id, values := range attachments {
				result.Attachments[id] = values
			}
		}
		if userData && userID != nil {
			if err = s.dtoProjectionUserData(ctx, tx, *userID, batch, result.UserData); err != nil {
				return DTOProjectionBatch{}, err
			}
		}
		if childCounts {
			if err = s.dtoProjectionCounts(ctx, tx, batch, false, result.ChildCounts); err != nil {
				return DTOProjectionBatch{}, err
			}
		}
		if recursiveCounts {
			if err = s.dtoProjectionCounts(ctx, tx, batch, true, result.RecursiveCounts); err != nil {
				return DTOProjectionBatch{}, err
			}
		}
	}
	if userData && userID != nil {
		var user *User
		if len(users) != 0 {
			user = users[0]
		}
		for start := 0; start < len(folderCounts); start += pageSatelliteBatchSize {
			end := min(start+pageSatelliteBatchSize, len(folderCounts))
			if err = s.dtoProjectionFolderUserDataCounts(ctx, tx, *userID, folderCounts[start:end], user, result.FolderUserDataCounts); err != nil {
				return DTOProjectionBatch{}, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return DTOProjectionBatch{}, err
	}
	return result, nil
}

func (s *ItemStore) dtoProjectionStreams(ctx context.Context, db queryer, ids []dto.UUID, out map[dto.UUID][]playback.MediaStream) error {
	marks, args := s.sourceIDArgs(ids)
	rows, err := db.QueryContext(ctx, "SELECT item_id,stream_index,stream_type,codec,language,display_title,is_default,is_forced,is_external,bitrate,width,height,channels,sample_rate,extra FROM media_streams WHERE item_id IN ("+marks+") ORDER BY item_id,stream_index", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var index int
		var streamType string
		var codec, language, title, defaultValue, forced, external, bitrate, width, height, channels, sampleRate, extra any
		if err := rows.Scan(&raw, &index, &streamType, &codec, &language, &title, &defaultValue, &forced, &external, &bitrate, &width, &height, &channels, &sampleRate, &extra); err != nil {
			return closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return closeRows(rows, err)
		}
		stream, err := streamFromColumns(index, streamType, codec, language, title, defaultValue, forced, external, bitrate, width, height, channels, sampleRate, asString(extra))
		if err != nil {
			return closeRows(rows, err)
		}
		out[id] = append(out[id], stream)
	}
	return closeRows(rows, rows.Err())
}

func (s *ItemStore) dtoProjectionUserData(ctx context.Context, db queryer, userID dto.UUID, ids []dto.UUID, out map[dto.UUID]map[string]UserData) error {
	// The page's keys CROSS JOIN user_data keeps the page as the outer loop.
	// With a plain JOIN SQLite drove the query from all of the user's
	// user_data rows and probed the keys of each.
	marks, args := s.sourceIDArgs(ids)
	args = append(args, s.uuidArg(userID))
	rows, err := db.QueryContext(ctx, "SELECT item_key.item_id,user_data.custom_key,user_data.item_id,user_data.played,user_data.position_ticks,user_data.play_count,user_data.is_favorite,user_data.rating,user_data.likes,user_data.last_played,user_data.audio_stream_index,user_data.subtitle_stream_index FROM item_user_data_keys AS item_key CROSS JOIN user_data WHERE user_data.custom_key = item_key.custom_key AND item_key.item_id IN ("+marks+") AND user_data.user_id = "+s.ph(len(args))+" ORDER BY item_key.item_id,user_data.custom_key", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var projectedRaw, itemRaw, played, rating, likes, last, audio, subtitle any
		var key string
		var data UserData
		if err := rows.Scan(&projectedRaw, &key, &itemRaw, &played, &data.PositionTicks, &data.PlayCount, &data.IsFavorite, &rating, &likes, &last, &audio, &subtitle); err != nil {
			return closeRows(rows, err)
		}
		projectedID, err := scanUUID(projectedRaw)
		if err != nil {
			return closeRows(rows, err)
		}
		data.ItemID, err = scanUUID(itemRaw)
		if err != nil {
			return closeRows(rows, err)
		}
		data.UserID, data.CustomKey = userID, key
		data.Played = asBool(played)
		data.Rating, data.Likes = optionalFloat32(rating), optionalInt(likes)
		data.LastPlayed, data.AudioStreamIndex, data.SubtitleStreamIndex = optionalTime(last, s.stores.Dialect()), optionalInt(audio), optionalInt(subtitle)
		if out[projectedID] == nil {
			out[projectedID] = make(map[string]UserData)
		}
		out[projectedID][key] = data
	}
	return closeRows(rows, rows.Err())
}

// dtoProjectionFolderUserDataCounts mirrors Jellyfin ItemCountService's
// GetPlayedAndTotalCountBatch. Every returned folder is aggregated in one SQL
// statement: its recursive leaves, directly linked leaves, and descendants of
// linked folders are UNIONed before counting. A target user's key-correlated
// UserData determines played status; absent data is unplayed.
func (s *ItemStore) dtoProjectionFolderUserDataCounts(ctx context.Context, db queryer, userID dto.UUID, ids []dto.UUID, user *User, out map[dto.UUID]FolderUserDataCounts) error {
	query, args := s.FolderUserDataCountsStatement(userID, ids, user)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var counts FolderUserDataCounts
		if err := rows.Scan(&raw, &counts.Played, &counts.Total); err != nil {
			return closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return closeRows(rows, err)
		}
		out[id] = counts
	}
	return closeRows(rows, rows.Err())
}

// FolderUserDataCountsStatement returns the statement that
// dtoProjectionFolderUserDataCounts runs for the folders in ids. It returns
// one row per folder: the folder ID, the played leaf count, and the leaf
// count.
func (s *ItemStore) FolderUserDataCountsStatement(userID dto.UUID, ids []dto.UUID, user *User) (string, []any) {
	values := make([]string, len(ids))
	args := make([]any, len(ids))
	for index, id := range ids {
		placeholder := s.ph(index + 1)
		if placeholder != "?" {
			placeholder += "::uuid"
		}
		values[index] = "(" + placeholder + ")"
		args[index] = s.uuidArg(id)
	}
	relation := newItemQueryIDRelation(s.stores.Dialect(), "items", "", args)
	// played_items resolves the user's played rows to item ids once, by
	// stored item id or by any custom key an item carries. A per-leaf EXISTS
	// with the same OR scanned every played row for every leaf. SQLite binds
	// "?" by position, so these arguments come before the access clause's.
	truth := s.stores.Dialect().Boolean(true)
	playedByID := relation.argument(s.uuidArg(userID))
	playedByKey := relation.argument(s.uuidArg(userID))
	playedItems := "played_items(item_id) AS (SELECT item_id FROM user_data WHERE user_id = " + playedByID + " AND played = " + truth +
		" UNION SELECT item_key.item_id FROM user_data AS user_data JOIN item_user_data_keys AS item_key ON item_key.custom_key = user_data.custom_key WHERE user_data.user_id = " + playedByKey + " AND user_data.played = " + truth + ")"
	visible := itemQueryAccessClause(&relation, ItemQuery{User: user}, "leaf")
	played := "leaf.id IN (SELECT item_id FROM played_items)"
	where := "leaf.is_folder = " + s.stores.Dialect().Boolean(false) + " AND leaf.is_virtual_item = " + s.stores.Dialect().Boolean(false)
	if visible != "" {
		where += " AND " + visible
	}
	// candidates CROSS JOIN leaf keeps the folder subtrees as the outer loop:
	// SQLite does not reorder a CROSS JOIN. With a plain JOIN it scanned every
	// item as the outer loop and probed candidates, so one Series cost a full
	// item-table scan. The linked-folder and season branches start from
	// requested for the same reason: with plain JOINs SQLite scanned all of
	// ancestors, or every Season, and probed requested last.
	query := "WITH requested(folder_id) AS (VALUES " + strings.Join(values, ", ") + "), " + playedItems + ", candidates(folder_id,leaf_id) AS (" +
		"SELECT requested.folder_id,ancestor.item_id FROM requested JOIN ancestors AS ancestor ON ancestor.ancestor_id = requested.folder_id " +
		"UNION SELECT requested.folder_id,linked.child_item_id FROM requested JOIN linked_children AS linked ON linked.parent_item_id = requested.folder_id " +
		"UNION SELECT requested.folder_id,descendant.item_id FROM requested CROSS JOIN linked_children AS linked CROSS JOIN items AS linked_folder CROSS JOIN ancestors AS descendant WHERE linked.parent_item_id = requested.folder_id AND linked_folder.id = linked.child_item_id AND linked_folder.is_folder = " + s.stores.Dialect().Boolean(true) + " AND descendant.ancestor_id = linked_folder.id" +
		// A season's children are its episodes (Season.GetChildren), which
		// include the series' root-folder episodes of the season and every
		// episode of a virtual (pathless) season.
		" UNION SELECT requested.folder_id,episode.id FROM requested CROSS JOIN items AS season CROSS JOIN items AS episode WHERE season.id = requested.folder_id AND season.kind = '" + item.KindSeason.String() + "' AND " + seasonEpisodeMatch("season", "episode", true) +
		") SELECT candidates.folder_id,SUM(CASE WHEN " + played + " THEN 1 ELSE 0 END),COUNT(*) FROM candidates CROSS JOIN items AS leaf WHERE leaf.id = candidates.leaf_id AND " + where + " GROUP BY candidates.folder_id"
	return query, relation.args
}

// CollectionChildCounts returns, for each requested collection folder, the
// number of its direct non-Folder children plus the direct children of its
// direct Folder children (the physical library roots). A collection with no
// such children is absent from the map. It counts in SQL; it does not load
// the items.
func (s *ItemStore) CollectionChildCounts(ctx context.Context, ids []dto.UUID) (map[dto.UUID]int, error) {
	out := make(map[dto.UUID]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, 2*len(ids)+2)
	list := func() string {
		marks := make([]string, len(ids))
		for index, id := range ids {
			args = append(args, s.uuidArg(id))
			marks[index] = s.ph(len(args))
		}
		return strings.Join(marks, ", ")
	}
	folder := func() string {
		args = append(args, item.KindFolder.String())
		return s.ph(len(args))
	}
	query := "SELECT collection_id, COUNT(*) FROM (" +
		"SELECT parent_id AS collection_id FROM items WHERE parent_id IN (" + list() + ") AND kind <> " + folder() +
		" UNION ALL SELECT root.parent_id FROM items AS root JOIN items AS child ON child.parent_id = root.id" +
		" WHERE root.parent_id IN (" + list() + ") AND root.kind = " + folder() +
		") AS counted GROUP BY collection_id"
	rows, err := s.stores.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw any
		var count int
		if err := rows.Scan(&raw, &count); err != nil {
			return nil, closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return nil, closeRows(rows, err)
		}
		out[id] = count
	}
	return out, closeRows(rows, rows.Err())
}

func (s *ItemStore) dtoProjectionCounts(ctx context.Context, db queryer, ids []dto.UUID, recursive bool, out map[dto.UUID]int) error {
	marks, args := s.sourceIDArgs(ids)
	query := "SELECT parent_id,COUNT(*) FROM items WHERE parent_id IN (" + marks + ") GROUP BY parent_id"
	if recursive {
		query = "SELECT ancestor_id,COUNT(*) FROM ancestors WHERE ancestor_id IN (" + marks + ") GROUP BY ancestor_id"
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var count int
		if err := rows.Scan(&raw, &count); err != nil {
			return closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return closeRows(rows, err)
		}
		out[id] = count
	}
	if err := closeRows(rows, rows.Err()); err != nil {
		return err
	}
	if !recursive {
		if err := s.linkedChildCounts(ctx, db, ids, out); err != nil {
			return err
		}
	}
	if err := s.seasonEpisodeCounts(ctx, db, ids, out); err != nil {
		return err
	}
	return s.albumTrackCounts(ctx, db, ids, out)
}

// linkedChildCounts ports ItemCountService.GetChildCountBatch
// (jellyfin@b04614e ItemCountService.cs:303-337), which DtoService reads only
// for folders: a folder with linked children (a BoxSet or a Playlist) counts
// its linked children, not its hierarchical children. A folder without linked
// children keeps its hierarchical count.
func (s *ItemStore) linkedChildCounts(ctx context.Context, db queryer, ids []dto.UUID, out map[dto.UUID]int) error {
	marks, args := s.sourceIDArgs(ids)
	rows, err := db.QueryContext(ctx, "SELECT linked.parent_item_id,COUNT(*) FROM linked_children AS linked JOIN items AS parent ON parent.id = linked.parent_item_id WHERE linked.parent_item_id IN ("+marks+") AND parent.is_folder = "+s.stores.Dialect().Boolean(true)+" GROUP BY linked.parent_item_id", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var count int
		if err := rows.Scan(&raw, &count); err != nil {
			return closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return closeRows(rows, err)
		}
		out[id] = count
	}
	return closeRows(rows, rows.Err())
}

// albumTrackCounts counts the tracks of each album in ids. A MusicAlbum's
// children are its Tracks, the recursive Audio children
// (MusicAlbum.GetEligibleChildrenForRecursiveChildren), so they are its
// child and recursive counts. The tracks of a multi-disc album are in disc
// folders, which are not counted. An album without tracks counts 0.
func (s *ItemStore) albumTrackCounts(ctx context.Context, db queryer, ids []dto.UUID, out map[dto.UUID]int) error {
	marks, args := s.sourceIDArgs(ids)
	rows, err := db.QueryContext(ctx, "SELECT album.id,COUNT(track.id) FROM items AS album LEFT JOIN ancestors AS scope ON scope.ancestor_id = album.id LEFT JOIN items AS track ON track.id = scope.item_id AND track.kind = '"+item.KindAudio.String()+"' WHERE album.id IN ("+marks+") AND album.kind = '"+item.KindMusicAlbum.String()+"' GROUP BY album.id", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var count int
		if err := rows.Scan(&raw, &count); err != nil {
			return closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return closeRows(rows, err)
		}
		out[id] = count
	}
	return closeRows(rows, rows.Err())
}

// seasonEpisodeCounts counts the episodes of each season in ids. A season's
// children are its episodes (Season.GetChildren, Season.GetEpisodes), so they
// are its child and recursive counts. They include the series' root-folder
// episodes of the season and every episode of a virtual (pathless) season.
// The counts replace the folder counts of a season, and a season without
// episodes counts 0. They use the default DisplaySpecialsWithinSeasons (D-57).
func (s *ItemStore) seasonEpisodeCounts(ctx context.Context, db queryer, ids []dto.UUID, out map[dto.UUID]int) error {
	marks, args := s.sourceIDArgs(ids)
	args = append(args, item.KindSeason.String())
	rows, err := db.QueryContext(ctx, "SELECT season.id,COUNT(episode.id) FROM items AS season LEFT JOIN items AS episode ON "+seasonEpisodeMatch("season", "episode", true)+" WHERE season.id IN ("+marks+") AND season.kind = "+s.ph(len(args))+" GROUP BY season.id", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw any
		var count int
		if err := rows.Scan(&raw, &count); err != nil {
			return closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return closeRows(rows, err)
		}
		out[id] = count
	}
	return closeRows(rows, rows.Err())
}
