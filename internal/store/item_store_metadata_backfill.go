// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

// MetadataBackfillOptions controls the metadata-backfill write. It carries
// the same ForceRefresh intent as ReconcileOptions but the backfill applies its
// own merge rules so ordinary reconciliation and adoption stay unchanged.
type MetadataBackfillOptions struct {
	// ForceRefresh writes the item when its canonical form is unchanged, so
	// DateLastSaved advances on an identical backfill.
	ForceRefresh bool

	// BetweenItems, when non-nil, runs between two consecutive item
	// transactions: after the previous item committed, failed, or was
	// skipped, and before the next item's baseline row loads. No write
	// transaction is open while it runs. A non-nil error aborts the
	// remaining items and is returned joined with the collected item
	// errors. RefreshExistingMetadata leaves it nil.
	BetweenItems func() error
}

// MetadataBackfill merges a remote (provider) enrichment plan onto the stored
// item rows for an existing item set. It preserves probe, sidecar, locked, and
// user-visible fields the remote stage omits, saves non-empty provider values,
// and does not change item structure. A validation pass checks every item and
// virtual structural identity before any write transaction opens, so a
// structural mismatch leaves all rows unchanged. Each item then merges onto a
// baseline that its own transaction loads, so a concurrent writer that commits
// between two items is observed by the later item's merge. The merge does
// not use a stale preloaded baseline. A failed item rolls back only that
// item, so the items that already committed stay committed. The result mirrors
// the shape of ReconcileChildrenResult so the scanner's persistence-counting
// path stays unchanged.
//
// The backfill is enrichment-only. It rejects absent items and structural
// identity changes. It does not delete children, does not change parent
// assignment, does not delete derived media, and does not alter stale-child
// semantics. Only RefreshExistingMetadata selects this path.
// ReconcileScannedPlanAtWithOptions and ReconcileChildren keep their ordinary
// behavior.
func (s *ItemStore) MetadataBackfill(ctx context.Context, parentID dto.UUID, desired []item.Item, virtuals []item.Item, savedAt time.Time, options MetadataBackfillOptions) (ReconcileChildrenResult, error) {
	result := ReconcileChildrenResult{}
	canonical, err := s.canonicalizeBackfillItems(desired)
	if err != nil {
		return result, err
	}
	canonicalVirtuals, err := reconcileScannedVirtuals(virtuals)
	if err != nil {
		return result, err
	}
	for index := range canonicalVirtuals {
		canonicalVirtuals[index], err = s.canonicalReconcileItem(canonicalVirtuals[index])
		if err != nil {
			return result, fmt.Errorf("metadata backfill virtual item: %w", err)
		}
	}

	// Validate every item and virtual structural identity before any write.
	// A structural mismatch on any row returns an error before the virtual
	// transaction or any item transaction opens, so no virtual row and no
	// item row is committed when the plan is invalid. The read-only pass
	// uses GetBatch, which loads rows and satellites in one query.
	if err := s.validateBackfillStructure(ctx, canonical, canonicalVirtuals); err != nil {
		return result, err
	}

	// Write virtual items in one transaction so people credits resolve
	// before the item rows that reference them. Virtual items are not part
	// of the per-item failure boundary.
	if err := s.backfillVirtuals(ctx, canonicalVirtuals); err != nil {
		return result, err
	}

	// Process each item in its own transaction. Every item loads its own
	// baseline row and satellites inside that transaction, so a concurrent
	// writer that commits between two items is observed by the later
	// item's merge instead of being clobbered by a stale preloaded
	// baseline. An item failure rolls back that item only; the items that
	// already committed stay committed. The function collects every
	// per-item error and returns it joined so the scanner can report the
	// failure path.
	var itemErrors []error
	for index, child := range canonical {
		if index > 0 && options.BetweenItems != nil {
			if err := options.BetweenItems(); err != nil {
				itemErrors = append(itemErrors, fmt.Errorf("metadata backfill between items: %w", err))
				break
			}
		}
		updated, backfillErr := s.backfillOneItem(ctx, parentID, child, savedAt, options)
		if backfillErr != nil {
			itemErrors = append(itemErrors, backfillErr)
			continue
		}
		if updated {
			result.Updated = append(result.Updated, child.ID)
		}
	}

	if joined := errors.Join(itemErrors...); joined != nil {
		sort.Slice(result.Updated, func(i, j int) bool { return result.Updated[i].String() < result.Updated[j].String() })
		return result, joined
	}
	sort.Slice(result.Updated, func(i, j int) bool { return result.Updated[i].String() < result.Updated[j].String() })
	return result, nil
}

// validateBackfillStructure checks every item and virtual structural identity
// before any write transaction opens. It loads the stored rows for all desired
// item IDs and virtual IDs in one read-only query. Each desired item must exist
// in the store and must not change Kind, ParentID, or Path. Each stored virtual
// must not change Kind. A virtual (Person, Studio, Genre) keeps its stored
// Path: its ID comes from the lowercased name, so a credit whose name differs
// only in case from the stored name has the same ID and a different literal
// path. Upstream LibraryManager.GetItemByName returns the stored item for that
// ID and does not compare the path. An absent virtual is legal because the
// virtual transaction inserts it. Any failure returns an error before the virtual
// transaction or any item transaction starts, so no row is committed.
func (s *ItemStore) validateBackfillStructure(ctx context.Context, desired, virtuals []item.Item) error {
	ids := make([]dto.UUID, 0, len(desired)+len(virtuals))
	for _, child := range desired {
		ids = append(ids, child.ID)
	}
	for _, virtual := range virtuals {
		ids = append(ids, virtual.ID)
	}
	stored, err := s.GetBatch(ctx, ids)
	if err != nil {
		return fmt.Errorf("metadata backfill validate: %w", err)
	}
	storedByID := make(map[dto.UUID]item.Item, len(stored))
	for _, row := range stored {
		storedByID[row.ID] = row
	}
	for _, child := range desired {
		row, found := storedByID[child.ID]
		if !found {
			return fmt.Errorf("metadata backfill: item %s is not a stored child — the backfill is enrichment-only and must not insert items", child.ID)
		}
		if err := rejectStructuralChange(child, row); err != nil {
			return err
		}
	}
	for _, virtual := range virtuals {
		row, found := storedByID[virtual.ID]
		if !found {
			continue
		}
		if row.Kind != virtual.Kind {
			return fmt.Errorf("metadata backfill: virtual item %s kind mismatch: stored %s, desired %s — the backfill is enrichment-only", virtual.ID, row.Kind, virtual.Kind)
		}
	}
	return nil
}

// backfillVirtuals writes virtual items in one transaction so people credits
// resolve before the item rows that reference them. A stored virtual merges
// the scanned candidate onto its stored row, so satellite families and fields
// that the sparse candidate omits stay from the stored row. An absent virtual
// inserts through the ordinary upsert path with the full candidate.
func (s *ItemStore) backfillVirtuals(ctx context.Context, virtuals []item.Item) error {
	if len(virtuals) == 0 {
		return nil
	}
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)

	if err := s.lockDerivedWrite(ctx, tx, virtuals...); err != nil {
		return err
	}
	var written []dto.UUID
	existingVirtuals, err := s.reconcileExistingVirtuals(ctx, tx, virtuals)
	if err != nil {
		return err
	}
	for _, virtual := range virtuals {
		if stored, found := existingVirtuals[virtual.ID]; found {
			raw := stored
			stored, err = s.canonicalReconcileItem(stored)
			if err != nil {
				return fmt.Errorf("metadata backfill canonicalize virtual %s: %w", virtual.ID, err)
			}
			merged := mergeBackfillItem(mergeVirtualImages(virtual, stored), stored)
			if reflect.DeepEqual(stored, merged) {
				continue
			}
			keepDerivedHierarchy(&merged, raw)
			written = append(written, merged.ID)
			if err := s.upsertItem(ctx, tx, merged); err != nil {
				return err
			}
			if err := s.replaceSatellites(ctx, tx, merged); err != nil {
				return err
			}
			continue
		}
		written = append(written, virtual.ID)
		if err := s.upsertItem(ctx, tx, virtual); err != nil {
			return err
		}
		if err := s.replaceSatellites(ctx, tx, virtual); err != nil {
			return err
		}
	}
	if err := s.repairDerivedForTx(ctx, tx, written, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// backfillOneItem merges one desired item onto its stored row inside one
// transaction. The transaction takes the derived-mutation and parent locks,
// loads this item's own baseline row and satellites, validates identity,
// merges the remote patch, and writes the item and satellites before it
// commits. The pre-write validation pass already checked structural identity,
// but the in-transaction check stays as a defense against a concurrent change
// between the pass and this transaction. An error rolls back only this item,
// so the items that already committed stay committed. Absent-item, structural,
// and canonicalization errors return bare because they are enrichment-contract
// rejections rather than write failures.
func (s *ItemStore) backfillOneItem(ctx context.Context, parentID dto.UUID, desired item.Item, savedAt time.Time, options MetadataBackfillOptions) (updated bool, err error) {
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("metadata backfill item %s: %w", desired.ID, err)
	}
	defer rollbackOnReturn(tx, &err)
	wrap := func(writeErr error) error {
		return fmt.Errorf("metadata backfill item %s: %w", desired.ID, writeErr)
	}

	if err := s.lockDerivedScope(ctx, tx, func(context.Context, *sql.Tx) (derivedLockScope, error) {
		return derivedLockScope{items: []dto.UUID{desired.ID}, parents: []dto.UUID{parentID}, writes: []item.Item{desired}, replaced: []dto.UUID{desired.ID}}, nil
	}); err != nil {
		return false, wrap(err)
	}
	if err := s.lockReconcileParent(ctx, tx, parentID); err != nil {
		return false, wrap(err)
	}
	existing, err := s.reconcileExistingItems(ctx, tx, []item.Item{desired})
	if err != nil {
		return false, wrap(err)
	}
	storedRow, found := existing[desired.ID]
	if !found {
		return false, fmt.Errorf("metadata backfill: item %s is not a stored child — the backfill is enrichment-only and must not insert items", desired.ID)
	}
	if err := rejectStructuralChange(desired, storedRow); err != nil {
		return false, err
	}
	if err := s.loadSatellites(ctx, tx, &storedRow); err != nil {
		return false, wrap(err)
	}
	canonicalStored, canonicalErr := s.canonicalReconcileItem(storedRow)
	if canonicalErr != nil {
		return false, fmt.Errorf("metadata backfill canonicalize stored %s: %w", desired.ID, canonicalErr)
	}
	merged := mergeBackfillItem(desired, canonicalStored)
	if reflect.DeepEqual(canonicalStored, merged) && !options.ForceRefresh {
		return false, nil
	}
	merged.DateLastSaved = &savedAt
	keepDerivedHierarchy(&merged, storedRow)
	if err := s.validateUpsert(ctx, tx, merged); err != nil {
		return false, wrap(err)
	}
	if err := s.upsertItem(ctx, tx, merged); err != nil {
		return false, wrap(err)
	}
	if err := s.replaceSatellites(ctx, tx, merged); err != nil {
		return false, wrap(err)
	}
	if err := s.repairDerivedForTx(ctx, tx, []dto.UUID{merged.ID}, nil); err != nil {
		return false, wrap(err)
	}
	return true, tx.Commit()
}

// keepDerivedHierarchy restores the derived hierarchy columns that
// canonicalReconcileItem clears for comparison. A backfill changes no
// hierarchy, so the stored values are the correct values; writing them back
// keeps the item's library, series, and season links valid before the scoped
// repair runs.
func keepDerivedHierarchy(merged *item.Item, stored item.Item) {
	merged.TopParentID = stored.TopParentID
	merged.SeriesID = stored.SeriesID
	merged.SeasonID = stored.SeasonID
}

// rejectStructuralChange reports an error when the desired item changes a
// structural identity field that a remote provider cannot change.
func rejectStructuralChange(desired, stored item.Item) error {
	if stored.Kind != desired.Kind {
		return fmt.Errorf("metadata backfill: item %s kind mismatch: stored %s, desired %s — the backfill is enrichment-only", desired.ID, stored.Kind, desired.Kind)
	}
	if !equalUUIDPtrFields(stored.ParentID, desired.ParentID) {
		return fmt.Errorf("metadata backfill: item %s parent mismatch — the backfill is enrichment-only", desired.ID)
	}
	if !equalStringPtrFields(stored.Path, desired.Path) {
		return fmt.Errorf("metadata backfill: item %s path mismatch — the backfill is enrichment-only", desired.ID)
	}
	return nil
}

func equalUUIDPtrFields(a, b *dto.UUID) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func equalStringPtrFields(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// canonicalizeBackfillItems validates and canonicalizes the desired backfill
// items. It rejects structural identity changes a remote provider cannot make:
// kind, parent, and path must match the stored row.
func (s *ItemStore) canonicalizeBackfillItems(desired []item.Item) ([]item.Item, error) {
	canonical := make([]item.Item, len(desired))
	for index, child := range desired {
		value, err := s.canonicalReconcileItem(child)
		if err != nil {
			return nil, fmt.Errorf("metadata backfill canonicalize item: %w", err)
		}
		canonical[index] = value
	}
	return canonical, nil
}

// mergeBackfillItem merges a remote (desired) enrichment onto the stored row
// baseline. It preserves structural identity, locked fields, probe fields,
// sidecar fields, and other satellite families. It accepts non-empty provider
// values for unlocked fields.
func mergeBackfillItem(desired, stored item.Item) item.Item {
	result := stored

	// Structural identity stays from the stored row. A remote result cannot
	// change scanner-owned filesystem identity.
	result.ID = stored.ID
	result.Kind = stored.Kind
	result.ParentID = stored.ParentID
	result.TopParentID = stored.TopParentID
	result.SeriesID = stored.SeriesID
	result.SeasonID = stored.SeasonID
	result.ChannelID = stored.ChannelID
	result.PrimaryVersionID = stored.PrimaryVersionID
	result.OwnerID = stored.OwnerID
	result.PlaylistOwnerUserID = stored.PlaylistOwnerUserID
	result.Path = stored.Path
	result.IsFolder = stored.IsFolder
	result.IsVirtualItem = stored.IsVirtualItem
	result.IsInMixedFolder = stored.IsInMixedFolder
	result.SourceType = stored.SourceType
	result.MediaType = stored.MediaType
	result.OpenAccess = stored.OpenAccess

	// Preserve lock state. A remote result cannot replace a locked field.
	result.IsLocked = stored.IsLocked
	result.LockedFields = stored.LockedFields

	// Preserve filesystem probe facts. The backfill skips media probing, so
	// these fields are absent from the desired item and must stay from the
	// stored row.
	result.DateCreated = stored.DateCreated
	result.DateModified = stored.DateModified
	result.FileChangedAt = stored.FileChangedAt
	result.DateLastRefreshed = stored.DateLastRefreshed
	result.DateLastMediaAdded = stored.DateLastMediaAdded
	result.Size = stored.Size
	result.FileDevice = stored.FileDevice
	result.FileInode = stored.FileInode
	result.Container = stored.Container
	result.MediaSourceContainer = stored.MediaSourceContainer

	// Probe-derived runtime stays from the stored row unless the remote stage
	// supplies a non-empty value and the field is not locked.
	if !isFieldLocked(stored, item.MetadataFieldRuntime) {
		if desired.RunTimeTicks != nil && *desired.RunTimeTicks != 0 {
			result.RunTimeTicks = desired.RunTimeTicks
		}
	}

	// Width, height, and bitrate stay from the stored row.
	result.Width = stored.Width
	result.Height = stored.Height
	result.TotalBitrate = stored.TotalBitrate

	// Merge text fields. A non-empty remote value replaces an unlocked stored
	// value. An omitted, nil, or empty remote value preserves the stored value.
	//
	// Name-family fields (Name, SortName, ForcedSortName, OriginalTitle): a
	// genuine non-empty provider-supplied name must still enrich an unlocked
	// stored name. The
	// resolver seeds Name from the filename, and a provider such as
	// MusicBrainz can persist a provider ID without supplying a name, so the
	// scanner clears the filename-derived name back to the stored sidecar
	// value before the desired item reaches this merge. The store does not
	// over-correct into "never replace Name": that would drop a real provider
	// title (provider/name-enrichment-dropped). The signal is whether the
	// value came from the provider stage, not whether it is non-empty, and
	// the scanner owns that signal. A locked Name stays from the stored row.
	result.Name = mergeBackfillString(stored.Name, desired.Name, isFieldLocked(stored, item.MetadataFieldName))
	result.SortName = mergeBackfillString(stored.SortName, desired.SortName, isFieldLocked(stored, item.MetadataFieldName))
	result.ForcedSortName = mergeBackfillString(stored.ForcedSortName, desired.ForcedSortName, isFieldLocked(stored, item.MetadataFieldName))
	result.OriginalTitle = mergeBackfillString(stored.OriginalTitle, desired.OriginalTitle, isFieldLocked(stored, item.MetadataFieldName))
	result.Overview = mergeBackfillString(stored.Overview, desired.Overview, isFieldLocked(stored, item.MetadataFieldOverview))
	result.Tagline = mergeBackfillString(stored.Tagline, desired.Tagline, isFieldLocked(stored, item.MetadataFieldOverview))
	result.OfficialRating = mergeBackfillString(stored.OfficialRating, desired.OfficialRating, isFieldLocked(stored, item.MetadataFieldOfficialRating))
	result.CustomRating = mergeBackfillString(stored.CustomRating, desired.CustomRating, isFieldLocked(stored, item.MetadataFieldOfficialRating))
	// OriginalLanguage and Album are provider-owned fields. TMDb writes
	// OriginalLanguage from the movie response original_language at
	// provider.go:174, and TheAudioDb writes Album. Neither has an individual
	// MetadataField lock entry, so the global IsLocked flag protects both.
	locked := stored.IsLocked
	result.OriginalLanguage = mergeBackfillString(stored.OriginalLanguage, desired.OriginalLanguage, locked)
	result.Album = mergeBackfillString(stored.Album, desired.Album, locked)
	result.ExternalID = mergeBackfillString(stored.ExternalID, desired.ExternalID, false)
	result.ExternalSeriesID = mergeBackfillString(stored.ExternalSeriesID, desired.ExternalSeriesID, false)
	result.ExternalServiceID = mergeBackfillString(stored.ExternalServiceID, desired.ExternalServiceID, false)
	result.PreferredMetadataLanguage = mergeBackfillString(stored.PreferredMetadataLanguage, desired.PreferredMetadataLanguage, false)
	result.PreferredMetadataCountryCode = mergeBackfillString(stored.PreferredMetadataCountryCode, desired.PreferredMetadataCountryCode, false)
	result.SeriesName = mergeBackfillString(stored.SeriesName, desired.SeriesName, false)
	result.SeasonName = mergeBackfillString(stored.SeasonName, desired.SeasonName, false)
	result.EpisodeTitle = mergeBackfillString(stored.EpisodeTitle, desired.EpisodeTitle, false)
	result.ShowID = mergeBackfillString(stored.ShowID, desired.ShowID, false)
	result.PresentationUniqueKey = mergeBackfillString(stored.PresentationUniqueKey, desired.PresentationUniqueKey, false)
	result.SeriesPresentationUniqueKey = mergeBackfillString(stored.SeriesPresentationUniqueKey, desired.SeriesPresentationUniqueKey, false)
	result.CleanName = mergeBackfillString(stored.CleanName, desired.CleanName, false)

	// Merge numbers. A non-empty remote value replaces an unlocked stored value.
	// The global IsLocked flag protects every provider-owned field that has no
	// individual MetadataField lock entry.
	result.ProductionYear = mergeBackfillIntPtr(stored.ProductionYear, desired.ProductionYear, locked)
	result.IndexNumber = mergeBackfillIntPtr(stored.IndexNumber, desired.IndexNumber, locked)
	result.ParentIndexNumber = mergeBackfillIntPtr(stored.ParentIndexNumber, desired.ParentIndexNumber, locked)
	result.CommunityRating = mergeBackfillRatingPtr(stored.CommunityRating, desired.CommunityRating, locked)
	result.CriticRating = mergeBackfillRatingPtr(stored.CriticRating, desired.CriticRating, locked)
	result.AirsBeforeSeasonNumber = mergeBackfillIntPtr(stored.AirsBeforeSeasonNumber, desired.AirsBeforeSeasonNumber, locked)
	result.AirsAfterSeasonNumber = mergeBackfillIntPtr(stored.AirsAfterSeasonNumber, desired.AirsAfterSeasonNumber, locked)
	result.AirsBeforeEpisodeNumber = mergeBackfillIntPtr(stored.AirsBeforeEpisodeNumber, desired.AirsBeforeEpisodeNumber, locked)
	result.InheritedParentalRatingValue = mergeBackfillIntPtr(stored.InheritedParentalRatingValue, desired.InheritedParentalRatingValue, locked)
	result.InheritedParentalRatingSubValue = mergeBackfillIntPtr(stored.InheritedParentalRatingSubValue, desired.InheritedParentalRatingSubValue, locked)
	result.LUFS = mergeBackfillFloat32Ptr(stored.LUFS, desired.LUFS, locked)
	result.NormalizationGain = mergeBackfillFloat32Ptr(stored.NormalizationGain, desired.NormalizationGain, locked)

	// Merge dates. A non-empty remote value replaces an unlocked stored value.
	result.StartDate = mergeBackfillTimePtr(stored.StartDate, desired.StartDate, locked)
	result.EndDate = mergeBackfillTimePtr(stored.EndDate, desired.EndDate, locked)
	result.PremiereDate = mergeBackfillTimePtr(stored.PremiereDate, desired.PremiereDate, locked)

	// Merge enum and classification fields. A non-nil remote value replaces the
	// stored value when the field is not locked. SeriesStatus is a provider-owned
	// field: TMDb writes it from the series response status at
	// provider_series.go:125. It has no individual MetadataField lock entry,
	// so the global IsLocked flag protects it.
	result.UnratedType = mergeBackfillEnumPtr(stored.UnratedType, desired.UnratedType)
	result.ExtraType = mergeBackfillEnumPtr(stored.ExtraType, desired.ExtraType)
	result.VideoType = mergeBackfillEnumPtr(stored.VideoType, desired.VideoType)
	result.IsoType = mergeBackfillEnumPtr(stored.IsoType, desired.IsoType)
	result.Video3DFormat = mergeBackfillEnumPtr(stored.Video3DFormat, desired.Video3DFormat)
	if locked {
		result.SeriesStatus = stored.SeriesStatus
	} else {
		result.SeriesStatus = mergeBackfillEnumPtr(stored.SeriesStatus, desired.SeriesStatus)
	}
	result.ProgramAudio = mergeBackfillEnumPtr(stored.ProgramAudio, desired.ProgramAudio)

	// Merge booleans. A remote boolean that is false does not clear a stored
	// true, because the remote stage omits booleans it does not set. The
	// desired value replaces the stored value only when the desired value is
	// true or when the stored value is false.
	result.IsMovie = stored.IsMovie || desired.IsMovie
	result.IsSeries = stored.IsSeries || desired.IsSeries
	result.IsRepeat = stored.IsRepeat || desired.IsRepeat
	result.IsPlaceHolder = stored.IsPlaceHolder || desired.IsPlaceHolder

	// Merge provider IDs by key. A non-empty remote value replaces the same
	// key. Stored values for keys the remote omits stay unchanged. A globally
	// locked item keeps every stored provider ID.
	if locked {
		result.ProviderIDs = stored.ProviderIDs
	} else {
		result.ProviderIDs = mergeBackfillProviderIDs(stored.ProviderIDs, desired.ProviderIDs)
	}

	// Merge images by (Type, Index). A remote image replaces only the same
	// key. Stored images for keys the remote omits stay unchanged. A globally
	// locked item keeps the stored images.
	if locked {
		result.ImageInfos = stored.ImageInfos
	} else {
		result.ImageInfos = mergeBackfillImages(stored.ImageInfos, desired.ImageInfos)
	}

	// Merge taxonomy values. A non-empty remote slice replaces the stored
	// slice when the field is not locked. An omitted, nil, or empty remote
	// slice preserves the stored slice. The global IsLocked flag protects
	// every taxonomy field, not only the fields in LockedFields.
	result.Values = mergeBackfillValues(stored, desired)

	// Merge people. A non-empty remote slice replaces the stored people when
	// Cast is not locked. An omitted, nil, or empty remote slice preserves the
	// stored people.
	if isFieldLocked(stored, item.MetadataFieldCast) {
		result.People = stored.People
	} else if len(desired.People) > 0 {
		result.People = desired.People
	} else {
		result.People = stored.People
	}

	// Preserve satellite families the backfill does not produce. The remote
	// stage does not re-derive these families.
	result.TrailerTypes = stored.TrailerTypes
	result.LinkedChildren = stored.LinkedChildren
	result.AdditionalParts = stored.AdditionalParts

	// Merge the extra payload. A non-empty remote value replaces an unlocked
	// stored value. The remote stage produces RemoteTrailers and the movie
	// collection name. Unrelated stored Extra fields stay from the stored row.
	// A globally locked item keeps the stored Extra.
	if locked {
		result.Extra = stored.Extra
	} else {
		result.Extra = mergeBackfillExtra(stored.Extra, desired.Extra)
	}

	// DateLastSaved is set by the caller after merge.

	return result
}

// isFieldLocked reports whether the named metadata field is locked on the
// stored item.
func isFieldLocked(stored item.Item, field item.MetadataField) bool {
	if stored.IsLocked {
		return true
	}
	for _, locked := range stored.LockedFields {
		if locked == field {
			return true
		}
	}
	return false
}

// mergeBackfillString returns the desired string when it is non-empty and the
// field is not locked, otherwise the stored string.
func mergeBackfillString(stored, desired *string, locked bool) *string {
	if locked {
		return stored
	}
	if desired != nil && *desired != "" {
		return desired
	}
	return stored
}

// mergeBackfillIntPtr returns the desired int when it is non-nil and the field
// is not locked, otherwise the stored int.
func mergeBackfillIntPtr(stored, desired *int, locked bool) *int {
	if locked {
		return stored
	}
	if desired != nil {
		return desired
	}
	return stored
}

// mergeBackfillFloat32Ptr returns the desired float32 when it is non-nil and
// the field is not locked, otherwise the stored float32.
func mergeBackfillFloat32Ptr(stored, desired *float32, locked bool) *float32 {
	if locked {
		return stored
	}
	if desired != nil {
		return desired
	}
	return stored
}

// mergeBackfillRatingPtr returns the desired float32 when it is non-nil and
// non-zero and the field is not locked, otherwise the stored float32. TMDb
// assigns CommunityRating from VoteAverage, and VoteAverage is a JSON number
// that defaults to zero when the response omits it. A zero desired value is
// the omission, not an explicit clear, so the merge preserves the stored
// rating.
func mergeBackfillRatingPtr(stored, desired *float32, locked bool) *float32 {
	if locked {
		return stored
	}
	if desired != nil && *desired != 0 {
		return desired
	}
	return stored
}

// mergeBackfillTimePtr returns the desired time when it is non-nil and the
// field is not locked, otherwise the stored time.
func mergeBackfillTimePtr(stored, desired *time.Time, locked bool) *time.Time {
	if locked {
		return stored
	}
	if desired != nil {
		return desired
	}
	return stored
}

// mergeBackfillEnumPtr returns the desired enum when it is non-nil, otherwise
// the stored enum. Enum fields are not lock-gated by MetadataField because the
// lock set does not cover them.
func mergeBackfillEnumPtr[T any](stored, desired *T) *T {
	if desired != nil {
		return desired
	}
	return stored
}

// mergeBackfillProviderIDs merges provider IDs by key. A non-empty remote
// value replaces the stored value for the same key. Stored values for keys
// the remote omits stay unchanged.
func mergeBackfillProviderIDs(stored, desired item.ProviderIDs) item.ProviderIDs {
	storedEntries := stored.Snapshot()
	desiredEntries := desired.Snapshot()
	if len(storedEntries) == 0 && len(desiredEntries) == 0 {
		return item.ProviderIDs{}
	}
	result := item.ProviderIDs{}
	for _, pid := range storedEntries {
		result.Set(pid.Name, pid.Value)
	}
	for _, pid := range desiredEntries {
		if pid.Value != "" {
			result.Set(pid.Name, pid.Value)
		}
	}
	return result
}

// mergeBackfillImages merges images by (Type, Index). A remote image replaces
// only the image with the same key. Stored images for keys the remote omits
// stay unchanged. A remote image with an empty Path is no replacement: the
// remote stage omits image data that way, so the stored image stays.
func mergeBackfillImages(stored, desired []item.ImageInfo) []item.ImageInfo {
	if len(stored) == 0 && len(desired) == 0 {
		return nil
	}
	result := make([]item.ImageInfo, 0, len(stored)+len(desired))
	result = append(result, stored...)
	for _, remote := range desired {
		if remote.Path == "" {
			continue
		}
		replaced := false
		for index, existing := range result {
			if existing.Type == remote.Type && existing.Index == remote.Index {
				result[index] = remote
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, remote)
		}
	}
	return result
}

// mergeBackfillValues merges the text-valued metadata satellites. A non-empty
// remote slice replaces the stored slice when the field is not locked. An
// omitted, nil, or empty remote slice preserves the stored slice. The global
// IsLocked flag protects every taxonomy field.
func mergeBackfillValues(stored, desired item.Item) item.ItemValues {
	result := item.ItemValues{}
	result.Genres = mergeBackfillStringSlice(stored.Values.Genres, desired.Values.Genres, isFieldLocked(stored, item.MetadataFieldGenres))
	result.Tags = mergeBackfillStringSlice(stored.Values.Tags, desired.Values.Tags, isFieldLocked(stored, item.MetadataFieldTags))
	result.InheritedTags = mergeBackfillStringSlice(stored.Values.InheritedTags, desired.Values.InheritedTags, isFieldLocked(stored, item.MetadataFieldTags))
	result.Artists = mergeBackfillStringSlice(stored.Values.Artists, desired.Values.Artists, isFieldLocked(stored, item.MetadataFieldGenres))
	result.AlbumArtists = mergeBackfillStringSlice(stored.Values.AlbumArtists, desired.Values.AlbumArtists, isFieldLocked(stored, item.MetadataFieldGenres))
	result.Studios = mergeBackfillStringSlice(stored.Values.Studios, desired.Values.Studios, isFieldLocked(stored, item.MetadataFieldStudios))
	result.ProductionLocations = mergeBackfillStringSlice(stored.Values.ProductionLocations, desired.Values.ProductionLocations, isFieldLocked(stored, item.MetadataFieldProductionLocations))
	return result
}

// mergeBackfillStringSlice returns the desired slice when it is non-nil and
// non-empty and the field is not locked, otherwise the stored slice.
func mergeBackfillStringSlice(stored, desired []string, locked bool) []string {
	if locked {
		return stored
	}
	if len(desired) > 0 {
		return desired
	}
	return stored
}

// mergeBackfillExtra merges the non-query extra payload. A non-empty remote
// value replaces an unlocked stored value. The remote stage produces
// RemoteTrailers and the movie TMDb collection name. An omitted, nil, or
// empty remote value preserves the stored value. Unrelated stored Extra
// fields stay from the stored row because the remote stage does not produce
// them. The merge never mutates the stored Movie pointer: backfillOneItem
// compares the merged item against the stored baseline to decide whether to
// write, so a mutation of the shared baseline would hide a real change and
// skip the write.
func mergeBackfillExtra(stored, desired item.ExtraFields) item.ExtraFields {
	result := stored

	// Remote trailers: a non-empty remote slice replaces the stored slice.
	if len(desired.RemoteTrailers) > 0 {
		result.RemoteTrailers = desired.RemoteTrailers
	}

	// Movie collection name: a non-nil, non-empty remote value replaces the
	// stored value. The remote stage allocates the Movie extra only when it
	// supplies a collection name. Allocate a fresh MovieExtra so the stored
	// baseline pointer stays unchanged for the no-op comparison.
	if desired.Movie != nil && desired.Movie.TmdbCollectionName != nil && *desired.Movie.TmdbCollectionName != "" {
		mergedMovie := &item.MovieExtra{}
		if result.Movie != nil {
			*mergedMovie = *result.Movie
		}
		mergedMovie.TmdbCollectionName = desired.Movie.TmdbCollectionName
		result.Movie = mergedMovie
	}

	return result
}
