// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
)

// userIDsRemappedKey is the store_metadata key that records a completed
// RemapUserIDs run. Its value is the RFC 3339 time of the run.
const userIDsRemappedKey = "user_ids_remapped"

// UserIDReferences names every column that holds a user id, other than
// users.id. RemapUserIDs rewrites each of them. A new column that holds a user
// id must be added here.
func UserIDReferences() []UserIDReference {
	return []UserIDReference{
		{Table: "devices", Column: "user_id"},
		{Table: "display_preferences", Column: "user_id"},
		{Table: "custom_item_display_preferences", Column: "user_id"},
		{Table: "access_schedules", Column: "user_id"},
		{Table: "user_data", Column: "user_id"},
		{Table: "item_shares", Column: "user_id"},
		{Table: "items", Column: "playlist_owner_user_id"},
		{Table: "play_sessions", Column: "user_id"},
		{Table: "play_session_grants", Column: "user_id"},
		{Table: "activity_log", Column: "user_id"},
	}
}

// UserIDReference is one table column that holds a user id.
type UserIDReference struct{ Table, Column string }

func (r UserIDReference) String() string { return r.Table + "." + r.Column }

// UserIDChange is the id change of one user.
type UserIDChange struct {
	Name     string
	Old, New dto.UUID
}

// UserIDRemapReport is the result of RemapUserIDs.
type UserIDRemapReport struct {
	// AlreadyRemapped reports that an earlier run completed; nothing changed.
	AlreadyRemapped bool
	// RemappedAt is the time of the earlier run when AlreadyRemapped is set.
	RemappedAt string
	Users      []UserIDChange
	// Rows counts rewritten rows by "table.column".
	Rows map[string]int64
}

// RemapUserIDs gives every user a new id from newID and rewrites every
// reference (UserIDReferences) in one identity transaction. Access tokens
// stay valid: only the owning user id of each device row changes. The run is
// recorded in store_metadata, and a later run returns AlreadyRemapped without
// a change. With dryRun set it counts the rows and rolls back.
//
// import-jellyfin once kept each Jellyfin user id. Clients that key saved
// users by id alone (Swiftfin) then confuse the two servers. This removes the shared ids.
func (s *Identity) RemapUserIDs(ctx context.Context, newID func() (dto.UUID, error), now time.Time, dryRun bool) (report UserIDRemapReport, err error) {
	report.Rows = map[string]int64{}
	tx, err := s.identityMutationTx(ctx)
	if err != nil {
		return report, err
	}
	defer rollbackOnReturn(tx, &err)

	var remappedAt string
	switch err := tx.QueryRowContext(ctx, "SELECT value FROM store_metadata WHERE key = "+s.p(1), userIDsRemappedKey).Scan(&remappedAt); {
	case err == nil:
		report.AlreadyRemapped, report.RemappedAt = true, remappedAt
		return report, nil
	case !errors.Is(err, sql.ErrNoRows):
		return report, err
	}

	users, err := s.listUsers(ctx, tx)
	if err != nil {
		return report, err
	}
	taken := make(map[dto.UUID]bool, 2*len(users))
	for _, user := range users {
		taken[user.ID] = true
	}
	for _, user := range users {
		id, err := newID()
		if err != nil {
			return report, fmt.Errorf("mint id for %q: %w", user.Name, err)
		}
		if id.IsNil() || taken[id] {
			return report, fmt.Errorf("mint id for %q: %s is not a fresh id", user.Name, id)
		}
		taken[id] = true
		if err := s.remapUserID(ctx, tx, user, id, report.Rows); err != nil {
			return report, fmt.Errorf("remap user %q: %w", user.Name, err)
		}
		report.Users = append(report.Users, UserIDChange{Name: user.Name, Old: user.ID, New: id})
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO store_metadata (key, value) VALUES ("+s.p(1)+", "+s.p(2)+")", userIDsRemappedKey, now.UTC().Format(time.RFC3339)); err != nil {
		return report, err
	}
	if dryRun {
		return report, tx.Rollback()
	}
	return report, tx.Commit()
}

// remapUserID moves one user to id. The foreign keys to users(id) have no ON
// UPDATE CASCADE, so it inserts the new row under a placeholder name, moves
// the references, deletes the old row, and restores the name.
func (s *Identity) remapUserID(ctx context.Context, tx *sql.Tx, user User, id dto.UUID, rows map[string]int64) error {
	moved := user
	moved.ID = id
	moved.NormalizedName = "�remap " + id.String()
	if err := s.insertUser(ctx, tx, moved); err != nil {
		return err
	}
	for _, ref := range UserIDReferences() {
		result, err := tx.ExecContext(ctx, "UPDATE "+ref.Table+" SET "+ref.Column+" = "+s.p(1)+" WHERE "+ref.Column+" = "+s.p(2), s.idValue(id), s.idValue(user.ID))
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		rows[ref.String()] += count
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = "+s.p(1), s.idValue(user.ID)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE users SET normalized_name = "+s.p(1)+" WHERE id = "+s.p(2), user.NormalizedName, s.idValue(id))
	return err
}
