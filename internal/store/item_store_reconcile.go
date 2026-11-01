// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playback"
)

// ReconcileChildrenResult describes the direct-child mutations applied by
// ReconcileChildren. Inserted and Updated retain desired input order; Deleted
// contains only deleted direct-child roots in lexical UUID order.
type ReconcileChildrenResult struct {
	Inserted []dto.UUID
	Updated  []dto.UUID
	Deleted  []dto.UUID
}

// ReconcileOptions controls scanner-plan persistence behavior.
type ReconcileOptions struct {
	// ForceRefresh writes retained children when their canonical item and
	// satellites are unchanged.
	ForceRefresh bool
	// PreserveChildIDs prevents the listed IDs from entering result.Deleted
	// and adds them to the preserved set in the stale-children CTE. The
	// scanner uses this during the physical-root topology cutover so legacy
	// flattened children under the CollectionFolder survive until the
	// per-root reconcile adopts them.
	PreserveChildIDs []dto.UUID
	// PreserveUnplannedChildren prevents the deletion of every existing
	// direct child that the desired plan omits. The remote-backfill mode uses
	// this so an enrichment-only plan never removes a stored child, satellite,
	// or user-data row the plan did not re-derive. An ordinary scan leaves
	// this flag false so its complete-child-set replacement stays unchanged.
	PreserveUnplannedChildren bool
}

type reconcileAdvisoryKey struct {
	first  int32
	second int32
}

const (
	reconcileGlobalLockFirst  int32 = 1380270927 // "RECO"
	reconcileGlobalLockSecond int32 = 1313032524 // "NCTL"
)

// ReconcileChildren atomically replaces parentID's complete direct-child set.
// Removed direct children are deleted with their subtrees. Reconciliation owns
// only this direct-child set: callers decide whether inaccessible children are
// present in desired. Retained children keep their user data; removed subtrees
// lose it along with their remaining satellites.
func (s *ItemStore) ReconcileChildren(ctx context.Context, parentID dto.UUID, desired []item.Item) (ReconcileChildrenResult, error) {
	return s.ReconcileScannedChildren(ctx, parentID, desired, nil)
}

// ReconciliationChildren reads a parent's complete direct-child snapshot on
// the reconciliation connection. It therefore cannot conflict with a
// concurrent reconciliation write on SQLite's shared in-memory database, while
// retaining complete child satellites for scanner lifecycle preservation.
func (s *ItemStore) ReconciliationChildren(ctx context.Context, parentID dto.UUID) (result []item.Item, err error) {
	if parentID.IsNil() {
		return nil, errors.New("reconciliation children: parent id is required")
	}
	tx, err := s.stores.Writer().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer rollbackOnReturn(tx, &err)
	children, err := s.reconcileDirectChildren(ctx, tx, parentID)
	if err != nil {
		return nil, err
	}
	result = make([]item.Item, 0, len(children))
	for _, child := range children {
		result = append(result, child)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID.String() < result[right].ID.String() })
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ScannedChildMedia is the source-derived payload for a child resolved during
// a library scan. An omitted child retains its previous media satellites;
// supplied empty slices intentionally replace the corresponding satellite set.
type ScannedChildMedia struct {
	ItemID      dto.UUID
	Streams     []playback.MediaStream
	Attachments []playback.MediaAttachment
}

// ReconcileScannedChildren commits the scanner's child plan and every supplied
// media source satellite in one transaction. It is deliberately scanner-shaped
// rather than a general batch API: media payloads must belong to the plan's
// direct children, preventing a folder scan from writing unrelated sources.
func (s *ItemStore) ReconcileScannedChildren(ctx context.Context, parentID dto.UUID, desired []item.Item, media []ScannedChildMedia) (ReconcileChildrenResult, error) {
	return s.ReconcileScannedPlan(ctx, parentID, desired, media, nil)
}

// ReconcileScannedPlan commits a direct child plan, its media satellites, and
// durable virtual metadata items in one transaction. Virtual items are written
// before child credit satellites so a cancellation or write failure cannot
// leave credits without their corresponding Person item.
func (s *ItemStore) ReconcileScannedPlan(ctx context.Context, parentID dto.UUID, desired []item.Item, media []ScannedChildMedia, virtuals []item.Item) (ReconcileChildrenResult, error) {
	return s.reconcileScannedPlan(ctx, parentID, desired, media, virtuals, nil, ReconcileOptions{})
}

// ReconcileScannedPlanAt stamps DateLastSaved only on scanner children whose
// transaction actually writes. The timestamp is applied after metadata refresh
// and after equality detection, preserving no-op rescans and the source save
// lifecycle.
func (s *ItemStore) ReconcileScannedPlanAt(ctx context.Context, parentID dto.UUID, desired []item.Item, media []ScannedChildMedia, virtuals []item.Item, savedAt time.Time) (ReconcileChildrenResult, error) {
	if savedAt.IsZero() {
		return s.ReconcileScannedPlan(ctx, parentID, desired, media, virtuals)
	}
	normalized := savedAt.UTC().Truncate(time.Microsecond)
	return s.reconcileScannedPlan(ctx, parentID, desired, media, virtuals, &normalized, ReconcileOptions{})
}

// ReconcileScannedPlanAtWithOptions commits a timestamped scanner plan with
// explicit persistence options. ForceRefresh writes retained children when
// their canonical item and satellites are unchanged. This behavior matches
// the upstream ForceSave condition for a full or replace-all refresh. The
// write advances DateLastSaved.
func (s *ItemStore) ReconcileScannedPlanAtWithOptions(ctx context.Context, parentID dto.UUID, desired []item.Item, media []ScannedChildMedia, virtuals []item.Item, savedAt time.Time, options ReconcileOptions) (ReconcileChildrenResult, error) {
	if savedAt.IsZero() {
		return s.reconcileScannedPlan(ctx, parentID, desired, media, virtuals, nil, options)
	}
	normalized := savedAt.UTC().Truncate(time.Microsecond)
	return s.reconcileScannedPlan(ctx, parentID, desired, media, virtuals, &normalized, options)
}

func (s *ItemStore) reconcileScannedPlan(ctx context.Context, parentID dto.UUID, desired []item.Item, media []ScannedChildMedia, virtuals []item.Item, savedAt *time.Time, options ReconcileOptions) (result ReconcileChildrenResult, err error) {
	canonical, err := s.reconcileDesiredChildren(parentID, desired)
	if err != nil {
		return ReconcileChildrenResult{}, err
	}
	canonicalMedia, err := reconcileScannedMedia(desired, media)
	if err != nil {
		return ReconcileChildrenResult{}, err
	}
	canonicalVirtuals, err := reconcileScannedVirtuals(virtuals)
	if err != nil {
		return ReconcileChildrenResult{}, err
	}
	for index := range canonicalVirtuals {
		canonicalVirtuals[index], err = s.canonicalReconcileItem(canonicalVirtuals[index])
		if err != nil {
			return ReconcileChildrenResult{}, fmt.Errorf("reconcile scanned virtual item: %w", err)
		}
	}

	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return ReconcileChildrenResult{}, err
	}
	defer rollbackOnReturn(tx, &err)

	if err := s.lockDerivedScope(ctx, tx, s.reconcileLockPlan(parentID, desired, canonicalVirtuals, options)); err != nil {
		return ReconcileChildrenResult{}, err
	}
	if err := s.lockReconcileParent(ctx, tx, parentID); err != nil {
		return ReconcileChildrenResult{}, err
	}
	for _, child := range desired {
		if err := s.validateUpsert(ctx, tx, child); err != nil {
			return ReconcileChildrenResult{}, fmt.Errorf("reconcile children: %w", err)
		}
	}
	for _, virtual := range canonicalVirtuals {
		if err := s.validateUpsert(ctx, tx, virtual); err != nil {
			return ReconcileChildrenResult{}, fmt.Errorf("reconcile scanned virtual item: %w", err)
		}
	}
	derivedInvalidated := make(map[dto.UUID]struct{}, len(desired)+len(canonicalVirtuals))
	changed := false
	var repairRoots []dto.UUID
	existingVirtuals, err := s.reconcileExistingVirtuals(ctx, tx, canonicalVirtuals)
	if err != nil {
		return ReconcileChildrenResult{}, err
	}
	for _, virtual := range canonicalVirtuals {
		if stored, found := existingVirtuals[virtual.ID]; found {
			stored, err = s.canonicalReconcileItem(stored)
			if err != nil {
				return ReconcileChildrenResult{}, fmt.Errorf("canonicalize persisted virtual item %s: %w", virtual.ID, err)
			}
			if virtual.Kind == item.KindPerson {
				virtual = mergeVirtualPerson(virtual, stored)
			} else {
				virtual = mergeVirtualImages(virtual, stored)
			}
			if reflect.DeepEqual(stored, virtual) {
				continue
			}
			if sourceRevisionChanged(stored, virtual) {
				derivedInvalidated[virtual.ID] = struct{}{}
			}
		}
		changed = true
		repairRoots = append(repairRoots, virtual.ID)
		if err := s.upsertItem(ctx, tx, virtual); err != nil {
			return ReconcileChildrenResult{}, err
		}
		if err := s.replaceSatellites(ctx, tx, virtual); err != nil {
			return ReconcileChildrenResult{}, err
		}
	}

	current, err := s.reconcileDirectChildren(ctx, tx, parentID)
	if err != nil {
		return ReconcileChildrenResult{}, err
	}
	existing, err := s.reconcileExistingItems(ctx, tx, desired)
	if err != nil {
		return ReconcileChildrenResult{}, err
	}

	desiredIDs := make(map[dto.UUID]struct{}, len(desired))
	for _, child := range desired {
		desiredIDs[child.ID] = struct{}{}
	}
	preservedIDs := make(map[dto.UUID]struct{}, len(options.PreserveChildIDs))
	for _, id := range options.PreserveChildIDs {
		preservedIDs[id] = struct{}{}
	}
	for id := range current {
		if _, retained := desiredIDs[id]; retained {
			continue
		}
		if _, preserved := preservedIDs[id]; preserved {
			continue
		}
		if options.PreserveUnplannedChildren {
			continue
		}
		result.Deleted = append(result.Deleted, id)
	}
	sort.Slice(result.Deleted, func(i, j int) bool { return result.Deleted[i].String() < result.Deleted[j].String() })

	mutated := make(map[dto.UUID]struct{}, len(desired))
	changed = changed || len(result.Deleted) != 0 || len(canonicalMedia) != 0
	for index, child := range desired {
		before, isDirectChild := current[child.ID]
		if isDirectChild && child.Kind == item.KindBoxSet {
			// A BoxSet's Manual links are its collection members, which
			// live in the store, not on disk (BoxSet.LinkedChildren). A
			// rescan keeps them beside the links the resolver found.
			desired[index] = carryCollectionLinks(child, before)
			if canonical[index], err = s.canonicalReconcileItem(desired[index]); err != nil {
				return ReconcileChildrenResult{}, fmt.Errorf("reconcile children: canonicalize child %s: %w", child.ID, err)
			}
		}
		if isDirectChild {
			stored, canonicalErr := s.canonicalReconcileItem(before)
			if canonicalErr != nil {
				return ReconcileChildrenResult{}, fmt.Errorf("canonicalize persisted child %s: %w", child.ID, canonicalErr)
			}
			if sourceRevisionChanged(stored, canonical[index]) {
				derivedInvalidated[child.ID] = struct{}{}
			}
			if reflect.DeepEqual(stored, canonical[index]) && !options.ForceRefresh {
				continue
			}
		} else if stored, found := existing[child.ID]; found && sourceRevisionChanged(stored, canonical[index]) {
			derivedInvalidated[child.ID] = struct{}{}
		}
		changed = true
		mutated[child.ID] = struct{}{}
		if _, found := existing[child.ID]; found {
			result.Updated = append(result.Updated, child.ID)
		} else {
			result.Inserted = append(result.Inserted, child.ID)
		}
	}
	for _, payload := range canonicalMedia {
		if _, found := existing[payload.ItemID]; !found {
			continue
		}
		stored, streamErr := s.streams(ctx, tx, payload.ItemID)
		if streamErr != nil {
			return ReconcileChildrenResult{}, fmt.Errorf("read persisted media streams for %s: %w", payload.ItemID, streamErr)
		}
		if !reflect.DeepEqual(stored, payload.Streams) {
			derivedInvalidated[payload.ItemID] = struct{}{}
		}
	}

	if !changed {
		if err := tx.Commit(); err != nil {
			return ReconcileChildrenResult{}, err
		}
		return result, nil
	}

	// Delete stale roots before writing retained/moved desired children. The CTE
	// excludes every desired ID, so a descendant being promoted to a direct child
	// keeps its row, satellites, and user data while its former ancestor is gone.
	var deleted []dto.UUID
	if len(result.Deleted) != 0 {
		if deleted, err = s.deleteReconciledStale(ctx, tx, parentID, desired, options.PreserveChildIDs); err != nil {
			return ReconcileChildrenResult{}, err
		}
	}
	if err := s.deleteDerivedMetadata(ctx, tx, derivedInvalidated); err != nil {
		return ReconcileChildrenResult{}, err
	}
	for _, child := range desired {
		if _, changed := mutated[child.ID]; !changed {
			continue
		}
		if previous, found := existing[child.ID]; found && previous.Kind != child.Kind {
			if err := s.removeFormerKindSatellites(ctx, tx, child.ID); err != nil {
				return ReconcileChildrenResult{}, err
			}
		}
		repairRoots = append(repairRoots, child.ID)
		persisted := child
		if savedAt != nil {
			value := *savedAt
			persisted.DateLastSaved = &value
		}
		if err := s.upsertItem(ctx, tx, persisted); err != nil {
			return ReconcileChildrenResult{}, err
		}
	}
	for index, child := range desired {
		if _, changed := mutated[child.ID]; !changed {
			continue
		}
		// An adopted child — one that existed under a different parent —
		// may carry satellites the scanner did not re-derive (provider IDs,
		// images from a prior lookup). The refresher produces a desired
		// item from local sources only; external metadata must survive the
		// reparent. Load the existing row's satellites and carry every
		// satellite family the desired item omits so replaceSatellites
		// re-inserts the same values instead of deleting them.
		_, isDirectChild := current[child.ID]
		if _, found := existing[child.ID]; found && !isDirectChild {
			previous := existing[child.ID]
			if err := s.loadSatellites(ctx, tx, &previous); err != nil {
				return ReconcileChildrenResult{}, err
			}
			desired[index] = mergeAdoptedSatellites(child, previous)
		}
		if err := s.replaceSatellites(ctx, tx, desired[index]); err != nil {
			return ReconcileChildrenResult{}, err
		}
	}
	for _, payload := range canonicalMedia {
		if err := s.replaceMediaTx(ctx, tx, payload); err != nil {
			return ReconcileChildrenResult{}, err
		}
	}
	if err := s.repairDerivedForTx(ctx, tx, repairRoots, deleted); err != nil {
		return ReconcileChildrenResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReconcileChildrenResult{}, err
	}
	return result, nil
}

func reconcileScannedVirtuals(values []item.Item) ([]item.Item, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := append([]item.Item(nil), values...)
	sort.Slice(result, func(left, right int) bool { return result[left].ID.String() < result[right].ID.String() })
	for index := range result {
		if result[index].ID.IsNil() {
			return nil, errors.New("reconcile scanned plan: virtual item id is required")
		}
		if result[index].Kind.String() == "" {
			return nil, errors.New("reconcile scanned plan: virtual item kind is required")
		}
		if index != 0 && result[index-1].ID == result[index].ID {
			return nil, fmt.Errorf("reconcile scanned plan: duplicate virtual item %s", result[index].ID)
		}
	}
	return result, nil
}

func reconcileScannedMedia(desired []item.Item, media []ScannedChildMedia) ([]ScannedChildMedia, error) {
	if len(media) == 0 {
		return nil, nil
	}
	desiredIDs := make(map[dto.UUID]struct{}, len(desired))
	for _, child := range desired {
		desiredIDs[child.ID] = struct{}{}
	}
	result := make([]ScannedChildMedia, len(media))
	seenItems := make(map[dto.UUID]struct{}, len(media))
	for index, payload := range media {
		if payload.ItemID.IsNil() {
			return nil, errors.New("reconcile scanned children: media item id is required")
		}
		if _, found := desiredIDs[payload.ItemID]; !found {
			return nil, fmt.Errorf("reconcile scanned children: media item %s is not a desired child", payload.ItemID)
		}
		if _, duplicate := seenItems[payload.ItemID]; duplicate {
			return nil, fmt.Errorf("reconcile scanned children: duplicate media item %s", payload.ItemID)
		}
		seenItems[payload.ItemID] = struct{}{}
		payload.Streams = append([]playback.MediaStream(nil), payload.Streams...)
		payload.Attachments = append([]playback.MediaAttachment(nil), payload.Attachments...)
		sort.SliceStable(payload.Streams, func(left, right int) bool { return payload.Streams[left].Index < payload.Streams[right].Index })
		sort.SliceStable(payload.Attachments, func(left, right int) bool { return payload.Attachments[left].Index < payload.Attachments[right].Index })
		for streamIndex := 1; streamIndex < len(payload.Streams); streamIndex++ {
			if payload.Streams[streamIndex-1].Index == payload.Streams[streamIndex].Index {
				return nil, fmt.Errorf("reconcile scanned children: duplicate stream index %d for %s", payload.Streams[streamIndex].Index, payload.ItemID)
			}
		}
		for attachmentIndex := 1; attachmentIndex < len(payload.Attachments); attachmentIndex++ {
			if payload.Attachments[attachmentIndex-1].Index == payload.Attachments[attachmentIndex].Index {
				return nil, fmt.Errorf("reconcile scanned children: duplicate attachment index %d for %s", payload.Attachments[attachmentIndex].Index, payload.ItemID)
			}
		}
		result[index] = payload
	}
	return result, nil
}

func (s *ItemStore) reconcileDesiredChildren(parentID dto.UUID, desired []item.Item) ([]item.Item, error) {
	if parentID.IsNil() {
		return nil, errors.New("reconcile children: parent id is required")
	}
	seen := make(map[dto.UUID]struct{}, len(desired))
	canonical := make([]item.Item, len(desired))
	for index, child := range desired {
		if err := validateReconcileChildRules(parentID, child); err != nil {
			return nil, err
		}
		if _, duplicate := seen[child.ID]; duplicate {
			return nil, fmt.Errorf("reconcile children: duplicate child id %s", child.ID)
		}
		seen[child.ID] = struct{}{}
		var err error
		canonical[index], err = s.canonicalReconcileItem(child)
		if err != nil {
			return nil, fmt.Errorf("reconcile children: canonicalize child %s: %w", child.ID, err)
		}
	}
	if err := validateReconcilePersonNames(desired); err != nil {
		return nil, err
	}
	return canonical, nil
}

// validateReconcileChildRules enforces the per-child rules shared by
// reconcileDesiredChildren and ValidateReconcileChild: ID required, known
// kind, parent match, playlist-owner and open-access constraints, and
// satellite validation. The cross-child rules — duplicate child ID and
// validateReconcilePersonNames — stay plan-level in reconcileDesiredChildren.
func validateReconcileChildRules(parentID dto.UUID, child item.Item) error {
	if child.ID.IsNil() {
		return errors.New("reconcile children: child id is required")
	}
	if child.Kind.String() == "" {
		return errors.New("reconcile children: child known kind is required")
	}
	if child.ParentID == nil || *child.ParentID != parentID {
		return errors.New("reconcile children: each child parent must match parent id")
	}
	if child.Kind != item.KindPlaylist && !child.PlaylistOwnerUserID.IsNil() {
		return errors.New("reconcile children: playlist owner is only valid for Playlist")
	}
	if child.Kind != item.KindPlaylist && child.OpenAccess {
		return errors.New("reconcile children: open access is only valid for Playlist")
	}
	if err := validateReconcileSatellites(child); err != nil {
		return err
	}
	return nil
}

// ValidateReconcileChild validates one child against the per-child rules
// that reconcileDesiredChildren enforces inside its loop, plus a
// canonicalReconcileItem dry-run that errors only on Extra JSON round-trip
// failure. It excludes the cross-child rules — duplicate child ID and
// validateReconcilePersonNames — which stay plan-level in
// reconcileDesiredChildren. The scanner calls this during quarantine to
// decide whether a candidate can enter the reconcile plan.
func (s *ItemStore) ValidateReconcileChild(parentID dto.UUID, child item.Item) error {
	if parentID.IsNil() {
		return errors.New("reconcile children: parent id is required")
	}
	if err := validateReconcileChildRules(parentID, child); err != nil {
		return err
	}
	if _, err := s.canonicalReconcileItem(child); err != nil {
		return fmt.Errorf("reconcile children: canonicalize child %s: %w", child.ID, err)
	}
	return nil
}

func validateReconcilePersonNames(desired []item.Item) error {
	names := make(map[dto.UUID]string)
	for _, child := range desired {
		for _, credit := range child.People {
			if previous, exists := names[credit.PersonID]; exists {
				if !strings.EqualFold(previous, credit.Name) {
					return fmt.Errorf("reconcile children: conflicting names for person %s", credit.PersonID)
				}
				continue
			}
			names[credit.PersonID] = credit.Name
		}
	}
	return nil
}

func validateReconcileSatellites(child item.Item) error {
	imageKeys := make(map[struct {
		typeValue item.ImageType
		index     int
	}]struct{}, len(child.ImageInfos))
	for _, image := range child.ImageInfos {
		if image.Type.String() == "" {
			return errors.New("reconcile children: image type is required")
		}
		key := struct {
			typeValue item.ImageType
			index     int
		}{image.Type, image.Index}
		if _, duplicate := imageKeys[key]; duplicate {
			return errors.New("reconcile children: duplicate image type and index")
		}
		imageKeys[key] = struct{}{}
	}
	lockedFields := make(map[item.MetadataField]struct{}, len(child.LockedFields))
	for _, field := range child.LockedFields {
		if field.String() == "" {
			return errors.New("reconcile children: metadata field is required")
		}
		if _, duplicate := lockedFields[field]; duplicate {
			return errors.New("reconcile children: duplicate metadata field")
		}
		lockedFields[field] = struct{}{}
	}
	trailerTypes := make(map[item.TrailerType]struct{}, len(child.TrailerTypes))
	for _, trailer := range child.TrailerTypes {
		if trailer.String() == "" {
			return errors.New("reconcile children: trailer type is required")
		}
		if _, duplicate := trailerTypes[trailer]; duplicate {
			return errors.New("reconcile children: duplicate trailer type")
		}
		trailerTypes[trailer] = struct{}{}
	}
	people := make(map[struct {
		id   dto.UUID
		kind item.PersonKind
	}]struct{}, len(child.People))
	for _, person := range child.People {
		if person.Type.String() == "" {
			return errors.New("reconcile children: person kind is required")
		}
		key := struct {
			id   dto.UUID
			kind item.PersonKind
		}{person.PersonID, person.Type}
		if _, duplicate := people[key]; duplicate {
			return errors.New("reconcile children: duplicate person and kind")
		}
		people[key] = struct{}{}
	}
	linkedChildren := make(map[dto.UUID]struct{}, len(child.LinkedChildren))
	for _, linked := range child.LinkedChildren {
		if linked.Type.String() == "" {
			return errors.New("reconcile children: linked child type is required")
		}
		if _, duplicate := linkedChildren[linked.ItemID]; duplicate {
			return errors.New("reconcile children: duplicate linked child")
		}
		linkedChildren[linked.ItemID] = struct{}{}
	}
	values := make(map[string]struct{})
	for kind, list := range itemValueLists(child.Values) {
		for _, value := range list {
			key := kind + "\x00" + value
			if _, duplicate := values[key]; duplicate {
				return fmt.Errorf("reconcile children: duplicate %s value", kind)
			}
			values[key] = struct{}{}
		}
	}
	return nil
}

// reconcileLockPlan returns the derived-state lock scope of a reconcile: the
// desired children and virtual items it may write, the current direct
// children it may delete with their subtrees, and parentID as the new parent.
func (s *ItemStore) reconcileLockPlan(parentID dto.UUID, desired, virtuals []item.Item, options ReconcileOptions) derivedLockPlan {
	return func(ctx context.Context, tx *sql.Tx) (derivedLockScope, error) {
		scope := derivedLockScope{parents: []dto.UUID{parentID}}
		scope.writes = append(append(scope.writes, desired...), virtuals...)
		kept := make(map[dto.UUID]struct{}, len(desired)+len(options.PreserveChildIDs))
		for _, value := range scope.writes {
			scope.items = append(scope.items, value.ID)
			scope.replaced = append(scope.replaced, value.ID)
			kept[value.ID] = struct{}{}
		}
		for _, id := range options.PreserveChildIDs {
			kept[id] = struct{}{}
		}
		if options.PreserveUnplannedChildren {
			return scope, nil
		}
		rows, err := tx.QueryContext(ctx, "SELECT id FROM items WHERE parent_id = "+s.ph(1), s.uuidArg(parentID))
		if err != nil {
			return derivedLockScope{}, err
		}
		var stale []dto.UUID
		for rows.Next() {
			var raw any
			if err := rows.Scan(&raw); err != nil {
				return derivedLockScope{}, closeRows(rows, err)
			}
			id, err := scanUUID(raw)
			if err != nil {
				return derivedLockScope{}, closeRows(rows, err)
			}
			if _, found := kept[id]; !found {
				stale = append(stale, id)
			}
		}
		if err := closeRows(rows, rows.Err()); err != nil {
			return derivedLockScope{}, err
		}
		for _, id := range stale {
			subtree, err := s.subtreeIDs(ctx, tx, id)
			if err != nil {
				return derivedLockScope{}, err
			}
			scope.items = append(scope.items, id)
			scope.deleted = append(scope.deleted, subtree...)
		}
		return scope, nil
	}
}

// lockReconcileParent serializes PostgreSQL reconciliations of one parent
// before reading the direct-child set. The caller takes its derived-state
// scope lock first (lockDerivedScope); the parent-specific keys and the row
// lock then serialize replacements of the same direct-child set, which the
// scope keys alone do not cover when the two child sets are disjoint.
func (s *ItemStore) lockReconcileParent(ctx context.Context, tx *sql.Tx, parentID dto.UUID) error {
	if s.ph(1) != "?" {
		keys := [2]reconcileAdvisoryKey{}
		for index := range keys {
			offset := index * 8
			keys[index] = reconcileAdvisoryKey{
				first:  int32(binary.BigEndian.Uint32(parentID[offset : offset+4])),
				second: int32(binary.BigEndian.Uint32(parentID[offset+4 : offset+8])),
			}
		}
		sort.Slice(keys[:], func(i, j int) bool {
			if keys[i].first == keys[j].first {
				return keys[i].second < keys[j].second
			}
			return keys[i].first < keys[j].first
		})
		for _, key := range keys {
			if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+s.ph(1)+", "+s.ph(2)+")", key.first, key.second); err != nil {
				return fmt.Errorf("reconcile children parent lock: %w", err)
			}
		}
	}
	query := "SELECT 1 FROM items WHERE id = " + s.ph(1)
	if s.ph(1) != "?" {
		query += " FOR UPDATE"
	}
	var found int
	if err := tx.QueryRowContext(ctx, query, s.uuidArg(parentID)).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("reconcile children parent: item does not exist")
		}
		return fmt.Errorf("reconcile children parent: read item: %w", err)
	}
	return nil
}

// removeFormerKindSatellites drops payloads which item.Item cannot express.
// A changed kind receives a complete scanner replacement, so those payloads
// belong to the former kind rather than the new item representation.
func (s *ItemStore) removeFormerKindSatellites(ctx context.Context, tx *sql.Tx, id dto.UUID) error {
	for _, table := range []string{"item_shares", "boxset_library_folders", "media_streams", "media_attachments", "chapters"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE item_id = "+s.ph(1), s.uuidArg(id)); err != nil {
			return err
		}
	}
	return nil
}

func (s *ItemStore) reconcileDirectChildren(ctx context.Context, tx *sql.Tx, parentID dto.UUID) (map[dto.UUID]item.Item, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+strings.Join(itemColumns, ", ")+" FROM items WHERE parent_id = "+s.ph(1)+" ORDER BY id", s.uuidArg(parentID))
	if err != nil {
		return nil, err
	}
	children := make(map[dto.UUID]item.Item)
	for rows.Next() {
		child, err := scanItem(rows, s.stores.Dialect())
		if err != nil {
			return nil, closeRows(rows, err)
		}
		children[child.ID] = child
	}
	if err := closeRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	for id, child := range children {
		if err := s.loadSatellites(ctx, tx, &child); err != nil {
			return nil, err
		}
		children[id] = child
	}
	return children, nil
}

// mergeVirtualImages keeps the stored images of a by-name entity and adds only
// the candidate images of a type the stored row does not have. A scan derives
// a person's Primary image from one credit's provider image; upstream sets it
// only when the person has no Primary image, and another folder that credits
// the same person without an image must not remove it.
func mergeVirtualImages(candidate, stored item.Item) item.Item {
	images := append([]item.ImageInfo(nil), stored.ImageInfos...)
	for _, image := range candidate.ImageInfos {
		found := false
		for _, existing := range stored.ImageInfos {
			if existing.Type == image.Type {
				found = true
				break
			}
		}
		if !found {
			images = append(images, image)
		}
	}
	if len(images) == 0 {
		images = candidate.ImageInfos
	}
	candidate.ImageInfos = images
	return candidate
}

// mergeVirtualPerson ports LibraryManager.SavePeopleMetadataAsync for a
// stored person. The stored person keeps its metadata, such as the overview
// and dates that a person refresh or an edit wrote. A credit provider ID
// replaces a different stored value, and a credit image fills a missing
// image type.
func mergeVirtualPerson(candidate, stored item.Item) item.Item {
	result := stored
	result.ImageInfos = mergeVirtualImages(candidate, stored).ImageInfos
	for _, id := range candidate.ProviderIDs.Snapshot() {
		if strings.TrimSpace(id.Value) == "" {
			continue
		}
		if current, _ := result.ProviderIDs.Get(id.Name); !strings.EqualFold(current, id.Value) {
			result.ProviderIDs.Set(id.Name, id.Value)
		}
	}
	if result.SortName == nil {
		result.SortName = candidate.SortName
	}
	if result.PresentationUniqueKey == nil {
		result.PresentationUniqueKey = candidate.PresentationUniqueKey
	}
	return result
}

func (s *ItemStore) reconcileExistingVirtuals(ctx context.Context, tx *sql.Tx, virtuals []item.Item) (map[dto.UUID]item.Item, error) {
	if len(virtuals) == 0 {
		return nil, nil
	}
	args := make([]any, len(virtuals))
	marks := make([]string, len(virtuals))
	for index, virtual := range virtuals {
		args[index] = s.uuidArg(virtual.ID)
		marks[index] = s.ph(index + 1)
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+strings.Join(itemColumns, ", ")+" FROM items WHERE id IN ("+strings.Join(marks, ", ")+")", args...)
	if err != nil {
		return nil, err
	}
	result := make(map[dto.UUID]item.Item, len(virtuals))
	for rows.Next() {
		stored, err := scanItem(rows, s.stores.Dialect())
		if err != nil {
			return nil, closeRows(rows, err)
		}
		result[stored.ID] = stored
	}
	if err := closeRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	for id, stored := range result {
		if err := s.loadSatellites(ctx, tx, &stored); err != nil {
			return nil, err
		}
		result[id] = stored
	}
	return result, nil
}

func (s *ItemStore) reconcileExistingItems(ctx context.Context, tx *sql.Tx, desired []item.Item) (map[dto.UUID]item.Item, error) {
	if len(desired) == 0 {
		return nil, nil
	}
	args := make([]any, len(desired))
	marks := make([]string, len(desired))
	for index, child := range desired {
		args[index] = s.uuidArg(child.ID)
		marks[index] = s.ph(index + 1)
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+strings.Join(itemColumns, ", ")+" FROM items WHERE id IN ("+strings.Join(marks, ", ")+")", args...)
	if err != nil {
		return nil, err
	}
	found := make(map[dto.UUID]item.Item, len(desired))
	for rows.Next() {
		stored, err := scanItem(rows, s.stores.Dialect())
		if err != nil {
			return nil, closeRows(rows, err)
		}
		found[stored.ID] = stored
	}
	return found, closeRows(rows, rows.Err())
}

func (s *ItemStore) deleteReconciledStale(ctx context.Context, tx *sql.Tx, parentID dto.UUID, desired []item.Item, preserveIDs []dto.UUID) ([]dto.UUID, error) {
	cte, args := s.reconcileStaleChildrenCTE(parentID, desired, preserveIDs)
	rows, err := tx.QueryContext(ctx, cte+" SELECT id FROM stale", args...)
	if err != nil {
		return nil, err
	}
	var stale []dto.UUID
	for rows.Next() {
		var raw any
		if err := rows.Scan(&raw); err != nil {
			return nil, closeRows(rows, err)
		}
		id, err := scanUUID(raw)
		if err != nil {
			return nil, closeRows(rows, err)
		}
		stale = append(stale, id)
	}
	if err := closeRows(rows, rows.Err()); err != nil {
		return nil, err
	}
	tables := []string{"item_images", "provider_ids", "locked_fields", "trailer_types", "item_value_map", "item_shares", "boxset_library_folders", "people_map", "media_streams", "media_attachments", "chapters", "item_additional_parts", "user_data", "item_user_data_keys"}
	return stale, s.deleteItemRows(ctx, tx, stale, tables)
}

func (s *ItemStore) reconcileStaleChildrenCTE(parentID dto.UUID, desired []item.Item, preserveIDs []dto.UUID) (string, []any) {
	args := make([]any, 0, len(desired)+len(preserveIDs)+1)
	next := 1
	parts := make([]string, 0, 5)
	desiredFilter := ""
	staleFilter := ""
	hasPreserve := len(preserveIDs) != 0
	if len(desired) != 0 {
		values := make([]string, len(desired))
		for index, child := range desired {
			placeholder := s.ph(next)
			if placeholder != "?" {
				placeholder += "::uuid"
			}
			values[index] = "(" + placeholder + ")"
			next++
			args = append(args, s.uuidArg(child.ID))
		}
		parts = append(parts, "desired(id) AS (VALUES "+strings.Join(values, ", ")+")")
	}
	if hasPreserve {
		values := make([]string, len(preserveIDs))
		for index, id := range preserveIDs {
			placeholder := s.ph(next)
			if placeholder != "?" {
				placeholder += "::uuid"
			}
			values[index] = "(" + placeholder + ")"
			next++
			args = append(args, s.uuidArg(id))
		}
		parts = append(parts, "preserve_ids(id) AS (VALUES "+strings.Join(values, ", ")+")")
	}
	preservedSource := "SELECT id FROM desired"
	if hasPreserve {
		preservedSource = "SELECT id FROM desired UNION SELECT id FROM preserve_ids"
	}
	if len(desired) != 0 {
		parts = append(parts, "preserved(id) AS ("+preservedSource+" UNION SELECT item_id FROM ancestors WHERE ancestor_id IN (SELECT id FROM desired))")
		desiredFilter = " AND NOT EXISTS (SELECT 1 FROM preserved WHERE preserved.id = items.id)"
		staleFilter = " AND NOT EXISTS (SELECT 1 FROM preserved WHERE preserved.id = ancestors.item_id)"
	} else if hasPreserve {
		parts = append(parts, "preserved(id) AS ("+preservedSource+")")
		desiredFilter = " AND NOT EXISTS (SELECT 1 FROM preserved WHERE preserved.id = items.id)"
		staleFilter = " AND NOT EXISTS (SELECT 1 FROM preserved WHERE preserved.id = ancestors.item_id)"
	}
	parent := s.ph(next)
	args = append(args, s.uuidArg(parentID))
	parts = append(parts, "stale_roots(id) AS (SELECT id FROM items WHERE parent_id = "+parent+desiredFilter+")")
	parts = append(parts, "stale(id) AS (SELECT ancestors.item_id FROM ancestors WHERE ancestors.ancestor_id IN (SELECT id FROM stale_roots)"+staleFilter+" UNION SELECT id FROM stale_roots)")
	return "WITH " + strings.Join(parts, ", "), args
}

func (s *ItemStore) canonicalReconcileItem(value item.Item) (item.Item, error) {
	value.TopParentID = nil
	value.SeriesID = nil
	value.SeasonID = nil
	if value.Name == nil {
		value.CleanName = nil
	} else {
		clean := item.CleanName(*value.Name)
		value.CleanName = &clean
	}
	value.StartDate = reconcileTime(value.StartDate)
	value.EndDate = reconcileTime(value.EndDate)
	value.PremiereDate = reconcileTime(value.PremiereDate)
	value.DateCreated = reconcileTime(value.DateCreated)
	value.DateModified = reconcileTime(value.DateModified)
	value.FileChangedAt = reconcileTime(value.FileChangedAt)
	value.DateLastRefreshed = reconcileTime(value.DateLastRefreshed)
	value.DateLastSaved = reconcileTime(value.DateLastSaved)
	value.DateLastMediaAdded = reconcileTime(value.DateLastMediaAdded)

	extra, err := json.Marshal(value.Extra)
	if err != nil {
		return item.Item{}, err
	}
	value.Extra = item.ExtraFields{}
	if err := json.Unmarshal(extra, &value.Extra); err != nil {
		return item.Item{}, err
	}
	value.ProviderIDs = reconcileProviders(value.ProviderIDs)
	value.ImageInfos = reconcileImages(value.ImageInfos)
	value.LockedFields = reconcileMetadataFields(value.LockedFields)
	value.Values.Genres = reconcileStrings(value.Values.Genres)
	value.Values.Tags = reconcileStrings(value.Values.Tags)
	value.Values.InheritedTags = reconcileStrings(value.Values.InheritedTags)
	value.Values.Artists = reconcileStrings(value.Values.Artists)
	value.Values.AlbumArtists = reconcileStrings(value.Values.AlbumArtists)
	value.Values.Studios = reconcileStrings(value.Values.Studios)
	value.Values.ProductionLocations = reconcileStrings(value.Values.ProductionLocations)
	value.People = reconcilePeople(value.People)
	value.TrailerTypes = reconcileTrailerTypes(value.TrailerTypes)
	value.LinkedChildren = reconcileLinkedChildren(value.LinkedChildren)
	return value, nil
}

func reconcileTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC().Truncate(time.Microsecond)
	return &normalized
}

func reconcileProviders(value item.ProviderIDs) item.ProviderIDs {
	entries := value.Snapshot()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name == entries[j].Name {
			return entries[i].Value < entries[j].Value
		}
		return entries[i].Name < entries[j].Name
	})
	var canonical item.ProviderIDs
	for _, entry := range entries {
		canonical.Set(entry.Name, entry.Value)
	}
	return canonical
}

func reconcileImages(values []item.ImageInfo) []item.ImageInfo {
	if len(values) == 0 {
		return nil
	}
	canonical := append([]item.ImageInfo(nil), values...)
	for index := range canonical {
		canonical[index].DateModified = reconcileTime(canonical[index].DateModified)
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Type == canonical[j].Type {
			return canonical[i].Index < canonical[j].Index
		}
		return canonical[i].Type.String() < canonical[j].Type.String()
	})
	return canonical
}

func reconcileMetadataFields(values []item.MetadataField) []item.MetadataField {
	if len(values) == 0 {
		return nil
	}
	canonical := append([]item.MetadataField(nil), values...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i] < canonical[j] })
	return canonical
}

func reconcileStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return values
}

// reconcilePeople orders credits as the people read does: sort_order with
// nil last, then list_order, which is the slice position.
func reconcilePeople(values []item.PersonCredit) []item.PersonCredit {
	if len(values) == 0 {
		return nil
	}
	canonical := append([]item.PersonCredit(nil), values...)
	sort.SliceStable(canonical, func(i, j int) bool {
		if canonical[i].SortOrder == nil || canonical[j].SortOrder == nil {
			return canonical[i].SortOrder != nil && canonical[j].SortOrder == nil
		}
		return *canonical[i].SortOrder < *canonical[j].SortOrder
	})
	return canonical
}

func reconcileTrailerTypes(values []item.TrailerType) []item.TrailerType {
	if len(values) == 0 {
		return nil
	}
	canonical := append([]item.TrailerType(nil), values...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].String() < canonical[j].String() })
	return canonical
}

func reconcileLinkedChildren(values []item.LinkedChildRef) []item.LinkedChildRef {
	if len(values) == 0 {
		return nil
	}
	canonical := append([]item.LinkedChildRef(nil), values...)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].SortOrder == nil {
			if canonical[j].SortOrder != nil {
				return false
			}
		} else if canonical[j].SortOrder == nil {
			return true
		} else if *canonical[i].SortOrder != *canonical[j].SortOrder {
			return *canonical[i].SortOrder < *canonical[j].SortOrder
		}
		return canonical[i].ItemID.String() < canonical[j].ItemID.String()
	})
	return canonical
}

func (s *ItemStore) deleteDerivedMetadata(ctx context.Context, tx *sql.Tx, ids map[dto.UUID]struct{}) error {
	if len(ids) == 0 {
		return nil
	}
	ordered := make([]dto.UUID, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].String() < ordered[right].String() })
	for _, id := range ordered {
		if err := s.deleteDerivedMetadataFor(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *ItemStore) deleteDerivedMetadataFor(ctx context.Context, tx *sql.Tx, id dto.UUID) error {
	for _, table := range []string{"keyframes", "trickplay_infos"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE item_id = "+s.ph(1), s.uuidArg(id)); err != nil {
			return err
		}
	}
	return nil
}

// mergeAdoptedSatellites carries satellite families from a previous item
// row into a desired item when the scanner did not re-derive them. Each
// family has its own rule:
//
//   - Provider IDs merge per key and images per (type, index): the desired
//     entry wins and a previous entry the desired item omits carries. An NFO
//     carrying one tmdb id does not drop a prior imdb id.
//   - LockedFields take the union. A lock only protects, and upstream merges
//     locks the same way (MergeBaseItemData: LockedFields.Concat.Distinct).
//   - People, Genres, Tags, Studios, ProductionLocations, Artists, and
//     AlbumArtists carry only when the desired family is absent. The scanner
//     reads the moved item before the refresh and already applied the
//     upstream MergeData rule for the refresh mode (library
//     mergeStoredMetadata), so a desired family is the merged result. A
//     union here would put back values that a ReplaceAllMetadata refresh
//     removed.
//   - TrailerTypes carry when absent: no scanner or provider produces them,
//     so an absent desired family says nothing about the stored one.
//   - LinkedChildren carry when absent. The key (parent, child) cannot tell
//     a resolver link from a user's collection link, so a per-link merge
//     could keep a stale resolver link or drop a user link. A BoxSet keeps
//     its user links through carryCollectionLinks.
//   - AdditionalParts and InheritedTags never carry. The video resolver
//     builds the complete ordered part list, so an empty list means the
//     stack has no more parts. No scan derives InheritedTags, and a write of
//     a direct child does not keep them either.
func mergeAdoptedSatellites(desired, previous item.Item) item.Item {
	for _, pid := range previous.ProviderIDs.Snapshot() {
		if _, ok := desired.ProviderIDs.Get(pid.Name); !ok {
			desired.ProviderIDs.Set(pid.Name, pid.Value)
		}
	}
	for _, prev := range previous.ImageInfos {
		if !imageInfoContainsTypeIndex(desired.ImageInfos, prev) {
			desired.ImageInfos = append(desired.ImageInfos, prev)
		}
	}
	desired.LockedFields = unionLockedFields(desired.LockedFields, previous.LockedFields)
	if len(desired.TrailerTypes) == 0 {
		desired.TrailerTypes = previous.TrailerTypes
	}
	if len(desired.LinkedChildren) == 0 {
		desired.LinkedChildren = previous.LinkedChildren
	}
	if desired.People == nil {
		desired.People = previous.People
	}
	if desired.Values.Genres == nil {
		desired.Values.Genres = previous.Values.Genres
	}
	if desired.Values.Tags == nil {
		desired.Values.Tags = previous.Values.Tags
	}
	if desired.Values.Artists == nil {
		desired.Values.Artists = previous.Values.Artists
	}
	if desired.Values.AlbumArtists == nil {
		desired.Values.AlbumArtists = previous.Values.AlbumArtists
	}
	if desired.Values.Studios == nil {
		desired.Values.Studios = previous.Values.Studios
	}
	if desired.Values.ProductionLocations == nil {
		desired.Values.ProductionLocations = previous.Values.ProductionLocations
	}
	return desired
}

// unionLockedFields keeps desired's fields in order and appends the previous
// fields it lacks.
func unionLockedFields(desired, previous []item.MetadataField) []item.MetadataField {
	merged := desired
	for _, field := range previous {
		if !slices.Contains(merged, field) {
			merged = append(slices.Clip(merged), field)
		}
	}
	return merged
}

// imageInfoContainsTypeIndex reports whether values contains an image
// with the same Type and Index as target. Type plus Index is the image
// identity the scanner and reconcileImages use, so it is the per-image
// key for the merge.
func imageInfoContainsTypeIndex(values []item.ImageInfo, target item.ImageInfo) bool {
	for _, v := range values {
		if v.Type == target.Type && v.Index == target.Index {
			return true
		}
	}
	return false
}

// carryCollectionLinks appends previous's Manual links that desired lacks,
// in their stored order, after desired's own links.
func carryCollectionLinks(desired, previous item.Item) item.Item {
	if len(previous.LinkedChildren) == 0 {
		return desired
	}
	present := make(map[dto.UUID]struct{}, len(desired.LinkedChildren))
	for _, link := range desired.LinkedChildren {
		present[link.ItemID] = struct{}{}
	}
	links := append([]item.LinkedChildRef(nil), desired.LinkedChildren...)
	for _, link := range previous.LinkedChildren {
		if link.Type != item.LinkedChildTypeManual || link.ItemID.IsNil() {
			continue
		}
		if _, ok := present[link.ItemID]; ok {
			continue
		}
		present[link.ItemID] = struct{}{}
		links = append(links, link)
	}
	desired.LinkedChildren = links
	return desired
}
