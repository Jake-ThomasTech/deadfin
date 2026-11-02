// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"fmt"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

// normalizePlanCredits collapses intra-item duplicate people on
// (PersonID, PersonKind) across the assembled plan. Every scanner producer
// sets PersonID before this pass runs: nfoPeople in the Refresher,
// appendProbePeople for probe credits, and appendAudioArtistCredits for
// audio artists. The pass keys on the computed PersonID, not on folded
// names, so two credits that share the same durable Person entity but
// arrived with different roles collapse into one credit.
//
// First occurrence wins its list position, original casing, and SortOrder.
// A later duplicate credit fills the retained credit's Role only when the
// retained Role is nil or empty, then is dropped. This matches stock
// PeopleHelper.AddPerson merge and role fill.
func normalizePlanCredits(items []item.Item) {
	for index := range items {
		credits := items[index].People
		if len(credits) <= 1 {
			continue
		}
		seen := make(map[personCreditKey]int, len(credits))
		result := make([]item.PersonCredit, 0, len(credits))
		for _, credit := range credits {
			key := personCreditKey{credit.PersonID, credit.Type}
			if pos, found := seen[key]; found {
				if (result[pos].Role == nil || *result[pos].Role == "") && credit.Role != nil && *credit.Role != "" {
					role := *credit.Role
					result[pos].Role = &role
				}
				continue
			}
			seen[key] = len(result)
			result = append(result, credit)
		}
		items[index].People = result
	}
}

// NormalizeItemLists collapses duplicate credits and values on one item,
// as the scanner does before it stores a plan. The store keys credits on
// (item, person, kind) and values on (item, value), so a provider result with
// a repeated credit or value must pass through this before ItemStore.Upsert.
func NormalizeItemLists(it *item.Item) {
	plan := []item.Item{*it}
	normalizePlanCredits(plan)
	normalizePlanValues(plan)
	*it = plan[0]
}

type personCreditKey struct {
	id   dto.UUID
	kind item.PersonKind
}

// normalizePlanValues collapses intra-item duplicate values across the
// scanner-owned value lists: Genres, Tags, Artists, AlbumArtists, Studios,
// and ProductionLocations. InheritedTags is store-derived and stays
// untouched. The pass trims each value, drops empties, collapses
// fold-equal duplicates, and keeps first-occurrence order and casing. The
// fold is dotNetInvariantLower, the same fold that produces entity IDs, so
// a stronger case-insensitive collapse implies store validity.
func normalizePlanValues(items []item.Item) {
	for index := range items {
		values := &items[index].Values
		values.Genres = normalizeValueList(values.Genres)
		values.Tags = normalizeValueList(values.Tags)
		values.Artists = normalizeValueList(values.Artists)
		values.AlbumArtists = normalizeValueList(values.AlbumArtists)
		values.Studios = normalizeValueList(values.Studios)
		values.ProductionLocations = normalizeValueList(values.ProductionLocations)
	}
}

func normalizeValueList(values []string) []string {
	if len(values) == 0 {
		return values
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		fold := dotNetInvariantLower(trimmed)
		if _, found := seen[fold]; found {
			continue
		}
		seen[fold] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

// quarantineResult records the outcome of one candidate's quarantine
// decision: the RefreshProgress skip record and whether the candidate was
// dropped or substituted with a stored prior.
type quarantineResult struct {
	skip          RefreshProgress
	substituted   bool
	quarantinedID dto.UUID
}

// quarantinePlan runs the validator over every candidate in desired and
// quarantines failures inside resolveFolder, before virtuals and child
// tasks are derived. A quarantined candidate contributes nothing: no row
// (unless a prior is substituted), no virtuals, no child task, no
// ScannedChildMedia entry.
//
// For each candidate that fails validate:
//
//   - A stored prior exists at the same normalized path (existingByPath):
//     substitute the stored item verbatim at the candidate's plan position
//     and drop the candidate's ScannedChildMedia entry by ItemID. Reconcile
//     compares the canonical prior against the stored row, finds it
//     unchanged, and touches neither the row nor its satellites.
//   - No prior exists: drop the candidate and its media entry. For a
//     folder, no child task is derived, so the subtree is never created.
//   - Dependent cascade: also quarantine any candidate whose
//     PrimaryVersionID names a quarantined ID.
//
// Either way, emit a RefreshProgress skip record with the item identity and
// cause. The caller (resolveFolder) filters quarantined IDs from the
// children loop.
func quarantinePlan(parentID dto.UUID, desired []item.Item, media []store.ScannedChildMedia, existingByPath map[string]item.Item, validate func(dto.UUID, item.Item) error) ([]item.Item, []store.ScannedChildMedia, []RefreshProgress) {
	quarantined := make(map[dto.UUID]struct{})
	skipped := make([]RefreshProgress, 0)
	mediaDrop := make(map[dto.UUID]struct{})

	// First pass: validate every candidate and record quarantined IDs.
	for _, candidate := range desired {
		if candidate.ID.IsNil() {
			continue
		}
		if err := validate(parentID, candidate); err != nil {
			quarantined[candidate.ID] = struct{}{}
			path := ""
			if candidate.Path != nil {
				path = *candidate.Path
			}
			skipped = append(skipped, RefreshProgress{
				ItemID: candidate.ID,
				Path:   path,
				Status: "skipped",
				Error:  fmt.Errorf("quarantine item %s: %w", path, err),
			})
		}
	}

	// Second pass: cascade to dependents whose PrimaryVersionID names a
	// quarantined ID.
	for _, candidate := range desired {
		if candidate.PrimaryVersionID == nil {
			continue
		}
		if _, isQuarantined := quarantined[candidate.ID]; isQuarantined {
			continue
		}
		if _, primaryQuarantined := quarantined[*candidate.PrimaryVersionID]; primaryQuarantined {
			quarantined[candidate.ID] = struct{}{}
			path := ""
			if candidate.Path != nil {
				path = *candidate.Path
			}
			skipped = append(skipped, RefreshProgress{
				ItemID: candidate.ID,
				Path:   path,
				Status: "skipped",
				Error:  fmt.Errorf("quarantine item %s: primary version %s was quarantined", path, *candidate.PrimaryVersionID),
			})
		}
	}

	if len(quarantined) == 0 {
		return desired, media, nil
	}

	// Build survivor list, substituting priors where available.
	result := make([]item.Item, 0, len(desired))
	for _, candidate := range desired {
		if _, isQuarantined := quarantined[candidate.ID]; !isQuarantined {
			result = append(result, candidate)
			continue
		}
		mediaDrop[candidate.ID] = struct{}{}
		if candidate.Path != nil {
			if prior, found := existingByPath[normalizedEntryPath(*candidate.Path)]; found {
				result = append(result, prior)
			}
		}
	}

	// Drop media entries for every quarantined candidate. A substituted
	// prior shares the quarantined candidate's ID, but the ScannedChildMedia
	// entry holds the candidate's fresh probe streams, not the prior's.
	// Reconcile retains the prior's stored media satellites because no
	// payload is supplied for that ID (an omitted media entry keeps the
	// previous satellites). Supplying the candidate's streams would let
	// replaceMediaTx overwrite the prior's streams and attachments, which
	// is the data loss this containment prevents.
	filteredMedia := make([]store.ScannedChildMedia, 0, len(media))
	for _, payload := range media {
		if _, dropped := mediaDrop[payload.ItemID]; dropped {
			continue
		}
		filteredMedia = append(filteredMedia, payload)
	}

	return result, filteredMedia, skipped
}
