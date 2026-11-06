// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/anime"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

type itemQueryFilterBuilder struct {
	d                                Dialect
	userID                           *dto.UUID
	disableLiveTVChannelUserDataName bool
	boundedCandidates                bool
	clauses                          []string
	args                             []any
}

func (b *itemQueryFilterBuilder) argument(value any) string {
	b.args = append(b.args, value)
	return b.d.Placeholder(len(b.args))
}

func (b *itemQueryFilterBuilder) uuidList(ids []dto.UUID) string {
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = b.argument(uuidArgDialect(b.d, id))
	}
	return strings.Join(placeholders, ", ")
}

func (b *itemQueryFilterBuilder) stringList(values []string) string {
	placeholders := make([]string, len(values))
	for i, value := range values {
		placeholders[i] = b.argument(value)
	}
	return strings.Join(placeholders, ", ")
}

func itemQuerySourceTypeValues(sourceTypes []item.SourceType) ([]string, error) {
	if len(sourceTypes) == 1 {
		value := sourceTypes[0].String()
		if value == "" {
			return nil, fmt.Errorf("translate item query: unsupported source type %d", sourceTypes[0])
		}
		return []string{enum(sourceTypes[0])}, nil
	}

	values := make([]string, 0, len(sourceTypes))
	seen := make(map[item.SourceType]struct{}, len(sourceTypes))
	for _, sourceType := range sourceTypes {
		value := sourceType.String()
		if value == "" {
			return nil, fmt.Errorf("translate item query: unsupported source type %d", sourceType)
		}
		if _, found := seen[sourceType]; found {
			continue
		}
		seen[sourceType] = struct{}{}
		values = append(values, enum(sourceType))
	}
	return values, nil
}

func translateItemQueryFilters(query ItemQuery, d Dialect) (string, []any, error) {
	if itemQueryNeedsUser(query) && (query.UserID == nil || query.UserID.IsNil()) {
		return "", nil, fmt.Errorf("translate item query: user-relative filters require a user ID")
	}

	b := itemQueryFilterBuilder{d: d, userID: query.UserID, disableLiveTVChannelUserDataName: query.DisableLiveTVChannelUserDataName, boundedCandidates: query.latestWindow.appliesTo(query), clauses: make([]string, 0, 24)}
	if len(query.ItemIDs) != 0 {
		b.clauses = append(b.clauses, "id IN ("+b.uuidList(query.ItemIDs)+")")
	}
	if len(query.ExcludeItemIDs) != 0 {
		b.clauses = append(b.clauses, "id NOT IN ("+b.uuidList(query.ExcludeItemIDs)+")")
	}
	if query.ParentID != nil && !query.ParentID.IsNil() {
		if len(query.flattenRootFolderIDs) != 0 {
			parentArg := b.argument(uuidArgDialect(d, *query.ParentID))
			excludeRoots := b.uuidList(query.flattenRootFolderIDs)
			childRoots := b.uuidList(query.flattenRootFolderIDs)
			b.clauses = append(b.clauses, "((parent_id = "+parentArg+" AND id NOT IN ("+excludeRoots+")) OR parent_id IN ("+childRoots+"))")
		} else {
			b.clauses = append(b.clauses, "parent_id = "+b.argument(uuidArgDialect(d, *query.ParentID)))
		}
	}
	if query.EpisodesOfSeasonID != nil {
		b.clauses = append(b.clauses, "id IN (SELECT episode.id FROM items AS season JOIN items AS episode ON "+seasonEpisodeMatch("season", "episode", !query.ExcludeSpecialsFromSeasons)+" WHERE season.id = "+b.argument(uuidArgDialect(d, *query.EpisodesOfSeasonID))+" AND season.kind = '"+item.KindSeason.String()+"')")
	}
	if len(query.AncestorIDs) != 0 {
		b.clauses = append(b.clauses, "EXISTS (SELECT 1 FROM ancestors AS ancestor WHERE ancestor.item_id = items.id AND ancestor.ancestor_id IN ("+b.uuidList(query.AncestorIDs)+"))")
	}
	if len(query.TopParentIDs) != 0 {
		b.clauses = append(b.clauses, "top_parent_id IN ("+b.uuidList(query.TopParentIDs)+")")
	}
	if len(query.IncludeItemTypes) != 0 {
		b.clauses = append(b.clauses, "kind IN ("+b.stringList(storedKindNames(query.IncludeItemTypes))+")")
	}
	if len(query.IncludeItemTypes) == 0 && len(query.ExcludeItemTypes) != 0 {
		b.clauses = append(b.clauses, "kind NOT IN ("+b.stringList(storedKindNames(query.ExcludeItemTypes))+")")
	}
	if len(query.MediaTypes) != 0 {
		values := make([]string, len(query.MediaTypes))
		for i, mediaType := range query.MediaTypes {
			values[i] = enum(mediaType)
		}
		b.clauses = append(b.clauses, "media_type IN ("+b.stringList(values)+")")
	}
	if len(query.SourceTypes) != 0 {
		values, err := itemQuerySourceTypeValues(query.SourceTypes)
		if err != nil {
			return "", nil, err
		}
		b.clauses = append(b.clauses, "source_type IN ("+b.stringList(values)+")")
	}

	b.addItemValues("genre", query.Genres, false)
	b.addItemValues("studio", query.Studios, false)
	b.addItemValues("tag", itemQueryTags(query, true), false)
	b.addItemValues("tag", itemQueryTags(query, false), true)
	b.addProviderFilter(query.HasAnyProviderID, nil, false)
	b.addProviderFilter(nil, query.HasAnyProviderIDs, false)
	b.addProviderFilter(query.ExcludeProviderIDs, nil, true)
	b.addProviderPresenceFilter("imdb", query.HasIMDbID)
	b.addProviderPresenceFilter("tmdb", query.HasTMDbID)
	b.addPeopleFilter(query)

	if len(query.Years) != 0 {
		b.clauses = append(b.clauses, "production_year IN ("+b.intList(query.Years)+")")
	}
	if query.MinPremiereDate != nil {
		b.clauses = append(b.clauses, "premiere_date >= "+b.argument(d.TimeValue(query.MinPremiereDate.UTC())))
	}
	if query.MaxPremiereDate != nil {
		b.clauses = append(b.clauses, "premiere_date <= "+b.argument(d.TimeValue(query.MaxPremiereDate.UTC())))
	}
	if query.MinDateLastSavedForUser != nil {
		b.clauses = append(b.clauses, "date_last_saved >= "+b.argument(d.TimeValue(query.MinDateLastSavedForUser.UTC())))
	}
	if query.MinCommunityRating != nil {
		b.clauses = append(b.clauses, "community_rating >= "+b.argument(*query.MinCommunityRating))
	}
	if query.MaxCommunityRating != nil {
		b.clauses = append(b.clauses, "community_rating <= "+b.argument(*query.MaxCommunityRating))
	}
	if query.MinRunTimeTicks != nil {
		b.clauses = append(b.clauses, "run_time_ticks >= "+b.argument(*query.MinRunTimeTicks))
	}
	if query.MaxRunTimeTicks != nil {
		b.clauses = append(b.clauses, "run_time_ticks <= "+b.argument(*query.MaxRunTimeTicks))
	}
	if query.SearchTerm != "" {
		cleanPattern := "%" + item.CleanName(query.SearchTerm) + "%"
		originalPattern := "%" + query.SearchTerm + "%"
		cleanSearch := d.SearchClause("clean_name", b.argument(cleanPattern))
		originalSearch := d.SearchClause("original_title", b.argument(originalPattern))
		b.clauses = append(b.clauses, "("+cleanSearch+" OR "+originalSearch+" OR "+b.animeTitleSearch(query.SearchTerm, cleanPattern)+")")
	}
	b.addBooleanFact("is_folder", query.IsFolder)
	if query.IsVirtualItem != nil {
		b.addBooleanFact("is_virtual_item", query.IsVirtualItem)
	} else {
		b.addBooleanFact("is_virtual_item", query.IsMissing)
	}
	if query.IsUnaired != nil {
		if query.ReferenceTime == nil || query.ReferenceTime.IsZero() {
			return "", nil, fmt.Errorf("translate item query: unaired filtering requires a reference time")
		}
		operator := "<"
		if *query.IsUnaired {
			operator = ">="
		}
		b.clauses = append(b.clauses, "premiere_date "+operator+" "+b.argument(d.TimeValue(query.ReferenceTime.UTC())))
	}
	b.addUserDataFiltersWithAggregates(query)
	if err := b.translateScalarFilters(query); err != nil {
		return "", nil, err
	}
	b.addValueFilters(query)
	b.addMediaFilters(query)

	if len(b.clauses) == 0 {
		return "", b.args, nil
	}
	return " WHERE " + strings.Join(b.clauses, " AND "), b.args, nil
}

func (b *itemQueryFilterBuilder) intList(values []int) string {
	placeholders := make([]string, len(values))
	for i, value := range values {
		placeholders[i] = b.argument(value)
	}
	return strings.Join(placeholders, ", ")
}

func itemQueryTags(query ItemQuery, include bool) []string {
	tags := query.Tags
	if !include {
		tags = query.ExcludeTags
	}
	additional := 0
	for _, category := range []*bool{query.IsSports, query.IsNews, query.IsKids} {
		if category != nil && *category == include {
			additional++
		}
	}
	if additional == 0 {
		return tags
	}

	merged := make([]string, 0, len(tags)+additional)
	merged = append(merged, tags...)
	for _, category := range []struct {
		value *bool
		tag   string
	}{{query.IsSports, "Sports"}, {query.IsNews, "News"}, {query.IsKids, "Kids"}} {
		if category.value != nil && *category.value == include {
			merged = append(merged, category.tag)
		}
	}
	return merged
}

func (b *itemQueryFilterBuilder) addItemValues(kind string, values []string, exclude bool) {
	if len(values) == 0 {
		return
	}
	clean := make([]string, len(values))
	for i, value := range values {
		clean[i] = item.CleanName(value)
	}
	prefix := "EXISTS"
	if exclude {
		prefix = "NOT EXISTS"
	}
	b.clauses = append(b.clauses, prefix+" (SELECT 1 FROM item_value_map AS ivm JOIN item_values AS iv ON iv.id = ivm.value_id WHERE ivm.item_id = items.id AND iv.kind = "+b.argument(kind)+" AND iv.clean_value IN ("+b.stringList(clean)+"))")
}

func (b *itemQueryFilterBuilder) addProviderFilter(single map[string]string, multiple map[string][]string, exclude bool) {
	conditions := make([]string, 0, len(single)+len(multiple))
	providers := make([]string, 0, len(single)+len(multiple))
	seen := make(map[string]struct{}, len(single)+len(multiple))
	for provider := range single {
		seen[provider] = struct{}{}
	}
	for provider := range multiple {
		seen[provider] = struct{}{}
	}
	for provider := range seen {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		if value, ok := single[provider]; ok {
			if value == "" && !exclude {
				conditions = append(conditions, "provider.provider = "+b.argument(provider))
			} else {
				conditions = append(conditions, "(provider.provider = "+b.argument(provider)+" AND provider.value = "+b.argument(value)+")")
			}
		}
		for _, value := range multiple[provider] {
			conditions = append(conditions, "(provider.provider = "+b.argument(provider)+" AND provider.value = "+b.argument(value)+")")
		}
	}
	if len(conditions) == 0 {
		return
	}
	prefix := "EXISTS"
	if exclude {
		prefix = "NOT EXISTS"
	}
	b.clauses = append(b.clauses, prefix+" (SELECT 1 FROM provider_ids AS provider WHERE provider.item_id = items.id AND ("+strings.Join(conditions, " OR ")+"))")
}

// addProviderPresenceFilter translates a public provider-identity boolean.
// A nil value leaves the relation unconstrained; true requires any value for
// the normalized provider name, while false requires that no such value exists.
func (b *itemQueryFilterBuilder) addProviderPresenceFilter(providerName string, present *bool) {
	if present == nil {
		return
	}
	prefix := "EXISTS"
	if !*present {
		prefix = "NOT EXISTS"
	}
	b.clauses = append(b.clauses, prefix+" (SELECT 1 FROM provider_ids AS provider WHERE provider.item_id = items.id AND LOWER(provider.provider) = "+b.argument(strings.ToLower(providerName))+")")
}

func (b *itemQueryFilterBuilder) addPeopleFilter(query ItemQuery) {
	if len(query.PersonIDs) != 0 {
		b.clauses = append(b.clauses, "EXISTS (SELECT 1 FROM people_map AS person_map WHERE person_map.item_id = items.id AND person_map.person_id IN ("+b.uuidList(query.PersonIDs)+"))")
	}
	if person := strings.TrimSpace(query.Person); person != "" {
		b.clauses = append(b.clauses, "EXISTS (SELECT 1 FROM people_map AS person_map JOIN people AS person ON person.id = person_map.person_id WHERE person_map.item_id = items.id AND person.clean_name = "+b.argument(item.CleanName(person))+")")
	}
}

func (b *itemQueryFilterBuilder) addBooleanFact(column string, value *bool) {
	if value != nil {
		b.clauses = append(b.clauses, column+" = "+b.argument(boolArg(b.d, *value)))
	}
}

func itemQueryNeedsUser(query ItemQuery) bool {
	return query.IsPlayed != nil || query.IsResumable != nil || query.IsFavorite != nil || query.IsFavoriteOrLiked != nil || query.IsLiked != nil || query.MinDateLastSavedForUser != nil
}

func (b *itemQueryFilterBuilder) addUserDataPredicate(include bool, predicate func() string) {
	fact := userDataFactForItem(b, "items", predicate)
	if !include {
		fact = "NOT (" + fact + ")"
	}
	b.clauses = append(b.clauses, fact)
}

// animeTitleSearch matches an item through the anime titles stored under one
// of its provider ids, so a search for "Attack on Titan",
// "Shingeki", or "進撃" finds the same series. Titles match on clean_title
// like clean_name does, and on fold_title, the kana and romaji folding of
// anime.FoldTitle. Items without anime titles never match this branch.
//
// The subquery is uncorrelated: it matches the anime titles once and reaches
// their items through ix_provider_ids_provider_value_item. A correlated
// EXISTS probed provider_ids and anime_titles for every item the name did
// not match, which made an unfiltered search (GET /Search/Hints) read every
// provider id of the library (deadfin#826).
func (b *itemQueryFilterBuilder) animeTitleSearch(term, cleanPattern string) string {
	d := b.d
	conditions := d.SearchClause("anime_title.clean_title", b.argument(cleanPattern))
	if folded := anime.FoldTitle(term); folded != "" {
		conditions += " OR " + d.SearchClause("anime_title.fold_title", b.argument("%"+folded+"%"))
	}
	return "items.id IN (SELECT anime_provider.item_id FROM anime_titles AS anime_title JOIN provider_ids AS anime_provider ON anime_provider.provider = anime_title.provider AND anime_provider.value = anime_title.external_id WHERE " + conditions + ")"
}

// storedKindNames ports ItemTypeLookup.BaseItemKindNames
// (ItemTypeLookup.cs:42-58): the client kinds TvChannel and TvProgram name
// the stored LiveTvChannel and LiveTvProgram rows.
func storedKindNames(kinds []item.Kind) []string {
	values := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case item.KindTvChannel:
			kind = item.KindLiveTvChannel
		case item.KindTvProgram:
			kind = item.KindLiveTvProgram
		}
		if name := kind.String(); !slices.Contains(values, name) {
			values = append(values, name)
		}
	}
	return values
}
