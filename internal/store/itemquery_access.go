// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

type playbackItemQueryReader interface {
	Query(context.Context, ItemQuery) (ItemQueryResult, error)
}

type playbackItemUserReader interface {
	Get(context.Context, dto.UUID) (User, error)
}

// PlaybackItemAccessReader adapts the existing policy-aware ItemQuery seam to
// playback source selection. It deliberately supplies both User and UserID:
// the former applies visibility policy and the latter preserves any user-data
// query behavior attached to an item projection.
type PlaybackItemAccessReader struct {
	items playbackItemQueryReader
	users playbackItemUserReader
}

func NewPlaybackItemAccessReader(items playbackItemQueryReader, users playbackItemUserReader) PlaybackItemAccessReader {
	return PlaybackItemAccessReader{items: items, users: users}
}

// AccessiblePlaybackItemIDs returns only requested IDs that the selected
// persisted user may see. A missing item and an inaccessible item both produce
// an absent ID, so playback can return its compatible not-found result.
func (reader PlaybackItemAccessReader) AccessiblePlaybackItemIDs(ctx context.Context, userID dto.UUID, ids []dto.UUID) ([]dto.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	user, err := reader.users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	result, err := reader.items.Query(ctx, ItemQuery{ItemIDs: ids, User: &user, UserID: &user.ID, IncludeOwnedItems: true})
	if err != nil {
		return nil, err
	}
	accessible := make([]dto.UUID, len(result.Items))
	for index, it := range result.Items {
		accessible[index] = it.ID
	}
	return accessible, nil
}

// applyItemQueryAccess moves the subtype-specific visibility check into the ID
// relation. Playlist ownership is deliberately distinct from generic owner_id:
// only public playlists, the playlist owner, or an explicit share are visible.
// This is divergence D-03's SQL replacement for upstream's post-query object
// filtering, so inaccessible rows cannot consume a page slot or count.
func applyItemQueryAccess(relation *itemQueryIDRelation, query ItemQuery) {
	visible := itemQueryAccessClause(relation, query, "items")
	if visible == "" {
		return
	}

	source := relation.current
	sql := "SELECT items.id FROM items JOIN " + source + " AS selected ON selected.id = items.id WHERE " + visible
	if itemQuerySharedListsChannelOnly(query) {
		relation.addMaterialized("visible_ids", sql)
		return
	}
	if source == "filtered_ids" {
		relation.access = visible
	}
	relation.add("visible_ids", sql)
}

// itemQueryAccessClause returns the target-user visibility predicate for one
// item alias. Explicit ItemIds reuse it so their source-required UNION cannot
// disclose an inaccessible existing row or count a nonexistent one.
func itemQueryAccessClause(relation *itemQueryIDRelation, query ItemQuery, alias string) string {
	if query.User == nil {
		return ""
	}
	policy := itemQueryUserPolicy{}
	if len(query.User.Policy) != 0 && json.Unmarshal(query.User.Policy, &policy) != nil {
		policy = itemQueryUserPolicy{}
	}
	if policy.IsAdministrator {
		return ""
	}

	ownerID := relation.argument(uuidArgDialect(relation.d, query.User.ID))
	shareID := relation.argument(uuidArgDialect(relation.d, query.User.ID))
	playlistKind := item.KindPlaylist.String()
	clauses := []string{"(" + alias + ".kind <> '" + playlistKind + "' OR " + alias + ".open_access = " + relation.d.Boolean(true) + " OR " + alias + ".playlist_owner_user_id = " + ownerID + " OR EXISTS (SELECT 1 FROM item_shares AS playlist_share WHERE playlist_share.item_id = " + alias + ".id AND playlist_share.user_id = " + shareID + "))"}
	if policy.EnableAllFolders != nil && !*policy.EnableAllFolders {
		topParentFolders := itemQueryAccessUUIDList(relation, policy.EnabledFolders)
		itemFolders := itemQueryAccessUUIDList(relation, policy.EnabledFolders)
		ancestorFolders := itemQueryAccessUUIDList(relation, policy.EnabledFolders)
		// Upstream Live TV reads pass the Live TV folder as explicit
		// TopParentIds, so EnabledFolders never scopes them
		// (LiveTvManager.GetInternalChannels, GetPrograms). EnableLiveTvAccess
		// gates them instead; it defaults to true (UserPolicy).
		liveTV := ""
		if policy.EnableLiveTvAccess == nil || *policy.EnableLiveTvAccess {
			liveTV = " OR " + alias + ".source_type = " + relation.argument(enum(item.SourceTypeLiveTV))
		}
		clauses = append(clauses, "("+alias+".kind = '"+playlistKind+"' OR ("+alias+".top_parent_id IN ("+topParentFolders+") OR "+alias+".id IN ("+itemFolders+") OR EXISTS (SELECT 1 FROM ancestors AS policy_ancestor WHERE policy_ancestor.item_id = "+alias+".id AND policy_ancestor.ancestor_id IN ("+ancestorFolders+")))"+liveTV+itemQueryAccessByNameClause(query, alias)+")")
	}
	// A user without Live TV access sees no Live TV item, also through a
	// shared playlist or collection (D-95). Upstream scopes item queries to
	// the user's views, which omit Live TV for that user
	// (UserViewManager.cs:132), but checks only the parental rating on
	// linked children (BaseItem.IsVisible).
	if policy.EnableLiveTvAccess != nil && !*policy.EnableLiveTvAccess {
		clauses = append(clauses, alias+".source_type <> "+relation.argument(enum(item.SourceTypeLiveTV)))
	}
	if policy.LimitLiveTvToSharedLists != nil && *policy.LimitLiveTvToSharedLists {
		clauses = append(clauses, itemQueryAccessSharedListsClause(relation, query.User.ID, alias))
	}
	clauses = append(clauses, itemQueryAccessTagClause(relation, alias, policy.BlockedTags, true))
	clauses = append(clauses, itemQueryAccessTagClause(relation, alias, policy.AllowedTags, false))
	if policy.MaxParentalRating != nil {
		lessThanScore := relation.argument(*policy.MaxParentalRating)
		equalScore := relation.argument(*policy.MaxParentalRating)
		subScore := 0
		if policy.MaxParentalSubRating != nil {
			subScore = *policy.MaxParentalSubRating
		}
		sub := relation.argument(subScore)
		clauses = append(clauses, "("+alias+".inherited_parental_rating_value IS NULL OR "+alias+".inherited_parental_rating_value < "+lessThanScore+" OR ("+alias+".inherited_parental_rating_value = "+equalScore+" AND COALESCE("+alias+".inherited_parental_rating_subvalue, 0) <= "+sub+"))")
	}
	if len(policy.BlockUnratedItems) != 0 {
		values := make([]string, 0, len(policy.BlockUnratedItems))
		for _, unrated := range policy.BlockUnratedItems {
			values = append(values, relation.argument(enum(item.UnratedItem(unrated))))
		}
		clauses = append(clauses, "("+alias+".inherited_parental_rating_value IS NOT NULL OR "+alias+".unrated_type IS NULL OR "+alias+".unrated_type NOT IN ("+strings.Join(values, ", ")+"))")
	}
	return strings.Join(itemQueryNonEmptyClauses(clauses), " AND ")
}

// itemQueryAccessByNameKinds lists the item-by-name kinds of upstream
// BaseItemRepository.GetItemByNameTypesInQuery as an SQL value list.
const itemQueryAccessByNameKinds = "'Person', 'Genre', 'MusicGenre', 'MusicArtist', 'Studio'"

// itemQueryAccessByNameClause admits an item-by-name with no library
// ancestry to a restricted-folder user when the query names explicit item
// IDs. Upstream BaseItem.IsVisibleStandalone checks folder access only for an
// item with collection folders, and LibraryManager.AddUserToQuery adds no
// TopParentIds scope to an ItemIds query. A Person, Genre or Studio has no
// parent and no owner, so it has no collection folder. Other queries keep the
// folder scope: upstream scopes them to the user's views, which these items
// are not in. The parental-rating and tag clauses still apply.
func itemQueryAccessByNameClause(query ItemQuery, alias string) string {
	if len(query.ItemIDs) == 0 {
		return ""
	}
	return " OR (" + alias + ".kind IN (" + itemQueryAccessByNameKinds + ") AND " + alias + ".parent_id IS NULL AND " + alias + ".owner_id IS NULL)"
}

// itemQueryAccessClauseArguments applies the shared visibility policy to a
// standalone SQL statement while extending that statement's argument list.
// It deliberately shares itemQueryAccessClause so specialized queries cannot
// drift from ItemQuery policy semantics.
func itemQueryAccessClauseArguments(d Dialect, query ItemQuery, alias string, args []any) (string, []any) {
	if query.User == nil {
		return "", args
	}
	relation := itemQueryIDRelation{d: d, args: args}
	return itemQueryAccessClause(&relation, query, alias), relation.args
}

type itemQueryUserPolicy struct {
	IsAdministrator    bool
	EnableAllFolders   *bool
	EnabledFolders     []dto.UUID
	EnableLiveTvAccess *bool
	BlockedTags        []string
	AllowedTags        []string
	// LimitLiveTvToSharedLists is Deadfin-only (D-105).
	LimitLiveTvToSharedLists *bool
	MaxParentalRating        *int
	MaxParentalSubRating     *int
	BlockUnratedItems        []int
}

func itemQueryAccessUUIDList(relation *itemQueryIDRelation, values []dto.UUID) string {
	if len(values) == 0 {
		return "NULL"
	}
	placeholders := make([]string, len(values))
	for index, value := range values {
		placeholders[index] = relation.argument(uuidArgDialect(relation.d, value))
	}
	return strings.Join(placeholders, ", ")
}

// itemQueryAccessSharedListsClause admits a Live TV channel only when a
// playlist that the user can see links it: a public playlist, the user's own,
// or one shared with the user. A programme follows its channel. Other kinds
// pass (D-105).
//
// The two kinds use different shapes because their row counts differ. A
// channel looks up its own linked_children rows by child_item_id. Programmes
// outnumber channels by an order of magnitude, so a programme tests its
// channel against the set of listed channels, which the database builds once.
func itemQueryAccessSharedListsClause(relation *itemQueryIDRelation, userID dto.UUID, alias string) string {
	visibleList := func() string {
		ownerID := relation.argument(uuidArgDialect(relation.d, userID))
		shareID := relation.argument(uuidArgDialect(relation.d, userID))
		return "channel_list.kind = '" + item.KindPlaylist.String() + "' AND (channel_list.open_access = " + relation.d.Boolean(true) +
			" OR channel_list.playlist_owner_user_id = " + ownerID +
			" OR EXISTS (SELECT 1 FROM item_shares AS channel_list_share WHERE channel_list_share.item_id = channel_list.id AND channel_list_share.user_id = " + shareID + "))"
	}
	const lists = "linked_children AS channel_list_entry JOIN items AS channel_list ON channel_list.id = channel_list_entry.parent_item_id"
	channel := item.KindLiveTvChannel.String()
	program := item.KindLiveTvProgram.String()
	return "(" + alias + ".kind NOT IN ('" + channel + "', '" + program + "')" +
		" OR (" + alias + ".kind = '" + channel + "' AND EXISTS (SELECT 1 FROM " + lists + " WHERE channel_list_entry.child_item_id = " + alias + ".id AND " + visibleList() + "))" +
		" OR (" + alias + ".kind = '" + program + "' AND " + alias + ".channel_id IS NOT NULL AND " + alias + ".channel_id IN (SELECT channel_list_entry.child_item_id FROM " + lists + " WHERE " + visibleList() + ")))"
}

// itemQuerySharedListsChannelOnly reports whether query uses the D-105
// shared-list filter and asks only for channels. Its visible set is small next
// to the item table, but the planner cannot estimate the filter, so it would
// walk the whole table in sort order; materializing the set lets the sort run
// over the visible channels alone.
func itemQuerySharedListsChannelOnly(query ItemQuery) bool {
	if query.User == nil || len(query.IncludeItemTypes) == 0 {
		return false
	}
	for _, kind := range query.IncludeItemTypes {
		if kind != item.KindLiveTvChannel {
			return false
		}
	}
	policy := itemQueryUserPolicy{}
	if len(query.User.Policy) == 0 || json.Unmarshal(query.User.Policy, &policy) != nil {
		return false
	}
	return !policy.IsAdministrator && policy.LimitLiveTvToSharedLists != nil && *policy.LimitLiveTvToSharedLists
}

func itemQueryAccessTagClause(relation *itemQueryIDRelation, alias string, tags []string, exclude bool) string {
	if len(tags) == 0 {
		return ""
	}
	cleanTags := make([]string, 0, len(tags))
	for _, tag := range tags {
		if clean := item.CleanName(tag); clean != "" {
			cleanTags = append(cleanTags, clean)
		}
	}
	if len(cleanTags) == 0 {
		return ""
	}
	// tagged is the uncorrelated set of items that carry a matching tag. Each
	// path tests membership in it, so the database builds the set once instead
	// of scanning item_value_map for every row (#761). The IS NOT NULL guards
	// keep a NULL series or top parent from turning the whole test to NULL.
	tagged := func() string {
		kind := relation.argument("tag")
		values := make([]string, len(cleanTags))
		for index, clean := range cleanTags {
			values[index] = relation.argument(clean)
		}
		return "SELECT policy_value_map.item_id FROM item_value_map AS policy_value_map JOIN item_values AS policy_value ON policy_value.id = policy_value_map.value_id WHERE policy_value.kind = " + kind + " AND policy_value.clean_value IN (" + strings.Join(values, ", ") + ")"
	}
	matches := "(" + alias + ".id IN (" + tagged() + ")" +
		" OR (" + alias + ".series_id IS NOT NULL AND " + alias + ".series_id IN (" + tagged() + "))" +
		" OR (" + alias + ".top_parent_id IS NOT NULL AND " + alias + ".top_parent_id IN (" + tagged() + "))" +
		" OR EXISTS (SELECT 1 FROM ancestors AS policy_tag_ancestor WHERE policy_tag_ancestor.item_id = " + alias + ".id AND policy_tag_ancestor.ancestor_id IN (" + tagged() + ")))"
	if exclude {
		return "NOT " + matches
	}
	return matches
}

func itemQueryNonEmptyClauses(clauses []string) []string {
	result := clauses[:0]
	for _, clause := range clauses {
		if clause != "" {
			result = append(result, clause)
		}
	}
	return result
}
