// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"strings"

	"github.com/deadfin-org/deadfin/internal/item"
)

// itemQueryIDRelation carries the filtered item IDs through every structural
// transform. The CTEs keep filtering, transformed counting, and final
// materialization on one relation without expanding the outer item row.
type itemQueryIDRelation struct {
	d       Dialect
	ctes    []string
	current string
	args    []any
	// access is the visibility predicate over the items alias "items" when
	// visibility is the only transform of filtered_ids. It takes the
	// arguments after the filter arguments.
	access string
}

func newItemQueryIDRelation(d Dialect, source, where string, filterArgs []any) itemQueryIDRelation {
	return itemQueryIDRelation{
		d:       d,
		ctes:    []string{"filtered_ids AS (SELECT id FROM " + source + where + ")"},
		current: "filtered_ids",
		args:    append([]any(nil), filterArgs...),
	}
}

func (r *itemQueryIDRelation) argument(value any) string {
	r.args = append(r.args, value)
	return r.d.Placeholder(len(r.args))
}

func (r *itemQueryIDRelation) add(name, sql string) {
	r.ctes = append(r.ctes, name+" AS ("+sql+")")
	r.current = name
}

// addMaterialized adds a CTE that the database computes once instead of
// inlining. PostgreSQL 12+ and SQLite 3.35+ accept AS MATERIALIZED.
func (r *itemQueryIDRelation) addMaterialized(name, sql string) {
	r.ctes = append(r.ctes, name+" AS MATERIALIZED ("+sql+")")
	r.current = name
}

func (r *itemQueryIDRelation) addAuxiliary(name, sql string) {
	r.ctes = append(r.ctes, name+" AS ("+sql+")")
}

func (r *itemQueryIDRelation) withSQL() string {
	return "WITH " + strings.Join(r.ctes, ", ")
}

// translateItemQueryIDRelation applies transforms in the same phase as the
// upstream repository: base filters first; visibility and adjacency restrict
// the filtered set; grouping chooses representative IDs; collapse may replace
// children with BoxSets; and only then do deferred name ranges see replacements.
func translateItemQueryIDRelation(query ItemQuery, d Dialect, source, where string, filterArgs []any) (itemQueryIDRelation, error) {
	relation := newItemQueryIDRelation(d, source, where, filterArgs)
	applyItemQueryRecursiveParent(&relation, query)
	applyItemQueryAccess(&relation, query)
	if err := applyItemQueryAdjacency(&relation, query); err != nil {
		return itemQueryIDRelation{}, err
	}
	applyItemQueryGrouping(&relation, query)
	applyItemQueryBoxSetCollapse(&relation, query)
	applyExplicitItemIDs(&relation, query)
	applyDeferredItemQueryNameRanges(&relation, query)
	return relation, nil
}

// applyItemQueryRecursiveParent is the generic recursive ParentId scope for a
// query whose parent kind is unknown. It resolves the kind in SQL and checks
// each filtered candidate. When ItemStore resolved the kind first,
// recursiveParentScopeClause already scoped the base relation.
func applyItemQueryRecursiveParent(relation *itemQueryIDRelation, query ItemQuery) {
	if !query.Recursive || query.ParentID == nil || query.ParentID.IsNil() || query.recursiveParent != nil {
		return
	}

	source := relation.current
	parentID := relation.argument(uuidArgDialect(relation.d, *query.ParentID))
	relation.addAuxiliary("recursive_parent", "SELECT id, kind FROM items WHERE id = "+parentID)
	collectionOrView := "EXISTS (SELECT 1 FROM recursive_parent AS parent WHERE parent.kind IN ('CollectionFolder', 'UserView') AND (candidate.top_parent_id = parent.id OR EXISTS (SELECT 1 FROM ancestors AS candidate_ancestor WHERE candidate_ancestor.item_id = candidate.id AND candidate_ancestor.ancestor_id = parent.id)))"
	linkedChild := "EXISTS (SELECT 1 FROM recursive_parent AS parent JOIN linked_children AS linked ON linked.parent_item_id = parent.id WHERE parent.kind IN ('Playlist', 'BoxSet') AND linked.child_item_id = candidate.id)"
	ancestor := "EXISTS (SELECT 1 FROM recursive_parent AS parent JOIN ancestors AS ancestor ON ancestor.ancestor_id = parent.id WHERE parent.kind NOT IN ('CollectionFolder', 'UserView', 'Playlist', 'BoxSet') AND ancestor.item_id = candidate.id)"
	relation.add("recursive_parent_ids", "SELECT selected.id FROM "+source+" AS selected JOIN items AS candidate ON candidate.id = selected.id WHERE "+collectionOrView+" OR "+linkedChild+" OR "+ancestor)
}

func applyExplicitItemIDs(relation *itemQueryIDRelation, query ItemQuery) {
	if query.SearchTerm == "" || len(query.ItemIDs) == 0 {
		return
	}

	values := make([]string, len(query.ItemIDs))
	for index, id := range query.ItemIDs {
		placeholder := relation.argument(uuidArgDialect(relation.d, id))
		if relation.d.Placeholder(1) != "?" {
			placeholder += "::uuid"
		}
		values[index] = "(" + placeholder + ")"
	}
	relation.addAuxiliary("explicit_item_ids(id)", "VALUES "+strings.Join(values, ", "))

	explicit := "SELECT explicit_item.id FROM items AS explicit_item JOIN explicit_item_ids AS requested ON requested.id = explicit_item.id"
	if visible := itemQueryAccessClause(relation, query, "explicit_item"); visible != "" {
		explicit += " WHERE " + visible
	}
	relation.addAuxiliary("accessible_explicit_item_ids", explicit)
	relation.add("search_or_explicit_ids", "SELECT id FROM "+relation.current+" UNION SELECT id FROM accessible_explicit_item_ids")
}

func applyItemQueryAdjacency(relation *itemQueryIDRelation, query ItemQuery) error {
	if query.AdjacentTo == nil || query.AdjacentTo.IsNil() {
		return nil
	}

	source := relation.current
	orderBy, orderArgs, err := translateItemQueryOrderWithArgs(query, relation.d, len(relation.args))
	if err != nil {
		return err
	}
	relation.args = append(relation.args, orderArgs...)
	targetID := relation.argument(uuidArgDialect(relation.d, *query.AdjacentTo))
	// Rank the already-filtered, parent-scoped candidates with the request
	// ordering so a sibling is never chosen from a different series or album.
	relation.addAuxiliary("adjacent_candidates", "SELECT items.id, ROW_NUMBER() OVER (ORDER BY "+orderBy+") AS position FROM items WHERE items.id IN (SELECT id FROM "+source+")")
	relation.addAuxiliary("adjacent_target", "SELECT position FROM adjacent_candidates WHERE id = "+targetID)
	relation.add("adjacent_filtered_ids", "SELECT selected.id FROM "+source+" AS selected WHERE NOT EXISTS (SELECT 1 FROM adjacent_target) OR EXISTS (SELECT 1 FROM adjacent_candidates AS candidate CROSS JOIN adjacent_target AS target WHERE candidate.id = selected.id AND candidate.position BETWEEN target.position - 1 AND target.position + 1)")
	return nil
}

func applyItemQueryGrouping(relation *itemQueryIDRelation, query ItemQuery) {
	if itemQueryGroupsPresentationKey(query) {
		applyItemQueryGroupedIDs(relation, "presentation_unique_key", true)
		return
	}
	if query.GroupBySeriesPresentationUniqueKey {
		applyItemQueryGroupedIDs(relation, "series_presentation_unique_key", false)
	}
}

func itemQueryGroupsPresentationKey(query ItemQuery) bool {
	if !query.GroupByPresentationUniqueKey || (query.IsResumable != nil && *query.IsResumable) || query.GroupBySeriesPresentationUniqueKey || strings.TrimSpace(query.PresentationUniqueKey) != "" || query.User == nil {
		return false
	}
	if len(query.IncludeItemTypes) == 0 {
		return true
	}
	for _, kind := range query.IncludeItemTypes {
		switch kind {
		case item.KindEpisode, item.KindVideo, item.KindMovie, item.KindMusicVideo, item.KindSeries, item.KindSeason:
			return true
		}
	}
	return false
}

func applyItemQueryGroupedIDs(relation *itemQueryIDRelation, column string, preferPrimary bool) {
	order := "grouped_item.id ASC"
	if preferPrimary {
		order = "CASE WHEN grouped_item.primary_version_id IS NULL THEN 0 ELSE 1 END ASC, grouped_item.id ASC"
	}
	source := relation.current
	relation.addAuxiliary("grouped_keys", "SELECT grouped_item."+column+" AS group_key FROM items AS grouped_item JOIN "+source+" AS selected ON selected.id = grouped_item.id GROUP BY "+column)
	sameGroup := "(grouped_item." + column + " = grouping.group_key OR (grouped_item." + column + " IS NULL AND grouping.group_key IS NULL))"
	relation.add("grouped_ranked_ids", "SELECT grouped_item.id, ROW_NUMBER() OVER (PARTITION BY grouped_item."+column+" ORDER BY "+order+") AS selection_rank FROM items AS grouped_item JOIN "+source+" AS selected ON selected.id = grouped_item.id JOIN grouped_keys AS grouping ON "+sameGroup)
	relation.add("grouped_ids", "SELECT id FROM grouped_ranked_ids WHERE selection_rank = 1")
}

func applyItemQueryBoxSetCollapse(relation *itemQueryIDRelation, query ItemQuery) {
	if query.CollapseBoxSetItems == nil || !*query.CollapseBoxSetItems {
		return
	}

	beforeCollapse := relation.current
	collapseLinks := "SELECT DISTINCT linked.child_item_id, linked.parent_item_id FROM linked_children AS linked JOIN " + beforeCollapse + " AS selected ON selected.id = linked.child_item_id JOIN items AS boxset ON boxset.id = linked.parent_item_id WHERE linked.type = 'Manual' AND boxset.kind = 'BoxSet'"
	if visible := itemQueryAccessClause(relation, query, "boxset"); visible != "" {
		collapseLinks += " AND " + visible
	}
	relation.addAuxiliary("collapse_links", collapseLinks)
	if len(query.CollapseBoxSetItemTypes) == 0 {
		relation.add("collapsed_ids", "SELECT candidate.id FROM items AS candidate JOIN "+beforeCollapse+" AS selected ON selected.id = candidate.id WHERE candidate.kind <> 'BoxSet' AND NOT EXISTS (SELECT 1 FROM collapse_links AS collapsed_link WHERE collapsed_link.child_item_id = candidate.id) UNION SELECT parent_item_id AS id FROM collapse_links")
		return
	}

	values := make([]string, len(query.CollapseBoxSetItemTypes))
	for i, kind := range query.CollapseBoxSetItemTypes {
		values[i] = "(" + relation.argument(kind.String()) + ")"
	}
	relation.addAuxiliary("collapse_types(kind)", "VALUES "+strings.Join(values, ", "))
	collapsible := "EXISTS (SELECT 1 FROM collapse_types WHERE kind = candidate.kind)"
	nonCollapsible := "NOT EXISTS (SELECT 1 FROM collapse_types WHERE kind = candidate.kind)"
	linkedChild := "EXISTS (SELECT 1 FROM collapse_links AS collapsed_link WHERE collapsed_link.child_item_id = candidate.id)"
	linkedBoxSet := "SELECT collapsed_link.parent_item_id AS id FROM collapse_links AS collapsed_link JOIN items AS child ON child.id = collapsed_link.child_item_id WHERE EXISTS (SELECT 1 FROM collapse_types WHERE kind = child.kind)"
	relation.add("collapsed_ids", "SELECT candidate.id FROM items AS candidate JOIN "+beforeCollapse+" AS selected ON selected.id = candidate.id WHERE "+nonCollapsible+" OR ("+collapsible+" AND candidate.kind <> 'BoxSet' AND NOT "+linkedChild+") UNION "+linkedBoxSet)
}

func applyDeferredItemQueryNameRanges(relation *itemQueryIDRelation, query ItemQuery) {
	if query.CollapseBoxSetItems == nil || !*query.CollapseBoxSetItems {
		return
	}
	clauses := make([]string, 0, 3)
	if query.NameStartsWith != "" {
		clauses = append(clauses, relation.d.SearchClause("late_item.sort_name", relation.argument(relation.d.EscapeLike(query.NameStartsWith)+"%")))
	}
	if query.NameStartsWithOrGreater != "" {
		clauses = append(clauses, "LOWER(late_item.sort_name) >= "+relation.argument(strings.ToLower(query.NameStartsWithOrGreater)))
	}
	if query.NameLessThan != "" {
		clauses = append(clauses, "LOWER(late_item.sort_name) < "+relation.argument(strings.ToLower(query.NameLessThan)))
	}
	if len(clauses) != 0 {
		relation.add("collapsed_name_ids", "SELECT late_item.id FROM items AS late_item JOIN "+relation.current+" AS selected ON selected.id = late_item.id WHERE "+strings.Join(clauses, " AND "))
	}
}
