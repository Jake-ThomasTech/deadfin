// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
)

// PlaySessionRecord is the persisted state of one live play session (#620).
// State is the session snapshot that internal/session encodes; the store
// does not read it.
type PlaySessionRecord struct {
	ID           string
	UserID       dto.UUID
	LastActivity time.Time
	State        []byte
}

// PlayGrantRecord is one persisted PlaySessionId grant (D-54, #620).
type PlayGrantRecord struct {
	PlaySessionID  string
	UserID         dto.UUID
	ItemID         dto.UUID
	MediaSourceIDs []string
	LastUsed       time.Time
}

// PlaySessionChanges is one write-behind batch: the records to store and the
// IDs to remove.
type PlaySessionChanges struct {
	Sessions        []PlaySessionRecord
	RemovedSessions []string
	Grants          []PlayGrantRecord
	RemovedGrants   []string
}

// Empty reports a batch with no change.
func (changes PlaySessionChanges) Empty() bool {
	return len(changes.Sessions) == 0 && len(changes.RemovedSessions) == 0 && len(changes.Grants) == 0 && len(changes.RemovedGrants) == 0
}

// PlaySessionStore persists live play sessions and PlaySessionId grants, so
// that playback continues across a server restart (#620).
type PlaySessionStore struct {
	stores Stores
	// owner is the replica whose rows this store writes (#652); "" is a
	// single server process, which writes every row.
	owner string
}

// NewPlaySessionStore constructs the play session adapter.
func NewPlaySessionStore(stores Stores) *PlaySessionStore {
	return &PlaySessionStore{stores: stores}
}

// WithOwner returns a store that writes and deletes only the rows that
// replica owner owns, and stores new rows with that owner (#652).
func (s *PlaySessionStore) WithOwner(owner string) *PlaySessionStore {
	return &PlaySessionStore{stores: s.stores, owner: owner}
}

// ownerGuard limits a DELETE to the rows of the store's owner.
// It uses placeholder index when the store has an owner.
func (s *PlaySessionStore) ownerGuard(table string, index int) string {
	if s.owner == "" {
		return ""
	}
	return " AND " + table + ".owner = " + s.p(index)
}

func (s *PlaySessionStore) p(index int) string { return s.stores.Dialect().Placeholder(index) }

func (s *PlaySessionStore) timeValue(value time.Time) any {
	return s.stores.Dialect().TimeValue(value.UTC())
}

// upsertGuard keeps a row that another replica owns: the update applies only
// when the row has the store's owner.
func (s *PlaySessionStore) upsertGuard(table string) string {
	if s.owner == "" {
		return ""
	}
	return " WHERE " + table + ".owner = excluded.owner"
}

func (s *PlaySessionStore) args(id string) []any {
	if s.owner == "" {
		return []any{id}
	}
	return []any{id, s.owner}
}

// SavePlaySessions applies one write-behind batch in one transaction. It
// applies the removals before it stores the records. A store with an owner
// leaves the rows of other replicas unchanged.
func (s *PlaySessionStore) SavePlaySessions(ctx context.Context, changes PlaySessionChanges) (err error) {
	if changes.Empty() {
		return nil
	}
	dialect := s.stores.Dialect()
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	for _, id := range changes.RemovedSessions {
		if _, err := tx.ExecContext(ctx, "DELETE FROM play_sessions WHERE id = "+s.p(1)+s.ownerGuard("play_sessions", 2), s.args(id)...); err != nil {
			return fmt.Errorf("delete play session: %w", err)
		}
	}
	for _, record := range changes.Sessions {
		if record.ID == "" || record.LastActivity.IsZero() {
			return errors.New("save play session: id and last activity are required")
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO play_sessions (id, user_id, last_activity, state, owner) VALUES ("+s.p(1)+", "+s.p(2)+", "+s.p(3)+", "+s.p(4)+", "+s.p(5)+") "+
				"ON CONFLICT (id) DO UPDATE SET user_id = excluded.user_id, last_activity = excluded.last_activity, state = excluded.state, owner = excluded.owner"+s.upsertGuard("play_sessions"),
			record.ID, uuidArgDialect(dialect, record.UserID), s.timeValue(record.LastActivity), record.State, s.owner)
		if err != nil {
			return fmt.Errorf("save play session: %w", err)
		}
	}
	for _, id := range changes.RemovedGrants {
		if _, err := tx.ExecContext(ctx, "DELETE FROM play_session_grants WHERE play_session_id = "+s.p(1)+s.ownerGuard("play_session_grants", 2), s.args(id)...); err != nil {
			return fmt.Errorf("delete play session grant: %w", err)
		}
	}
	for _, grant := range changes.Grants {
		if grant.PlaySessionID == "" || grant.LastUsed.IsZero() {
			return errors.New("save play session grant: PlaySessionId and last use are required")
		}
		sources, err := json.Marshal(append([]string{}, grant.MediaSourceIDs...))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO play_session_grants (play_session_id, user_id, item_id, media_source_ids, last_used, owner) VALUES ("+s.p(1)+", "+s.p(2)+", "+s.p(3)+", "+s.p(4)+", "+s.p(5)+", "+s.p(6)+") "+
				"ON CONFLICT (play_session_id) DO UPDATE SET user_id = excluded.user_id, item_id = excluded.item_id, media_source_ids = excluded.media_source_ids, last_used = excluded.last_used, owner = excluded.owner"+s.upsertGuard("play_session_grants"),
			grant.PlaySessionID, uuidArgDialect(dialect, grant.UserID), uuidArgDialect(dialect, grant.ItemID), string(sources), s.timeValue(grant.LastUsed), s.owner)
		if err != nil {
			return fmt.Errorf("save play session grant: %w", err)
		}
	}
	return tx.Commit()
}

// PlaySessions returns the sessions whose last activity is at or after
// since, oldest first.
func (s *PlaySessionStore) PlaySessions(ctx context.Context, since time.Time) (records []PlaySessionRecord, err error) {
	rows, err := s.stores.Reader().QueryContext(ctx,
		"SELECT id, user_id, last_activity, state FROM play_sessions WHERE last_activity >= "+s.p(1)+" ORDER BY last_activity ASC, id ASC",
		s.timeValue(since))
	if err != nil {
		return nil, fmt.Errorf("list play sessions: %w", err)
	}
	defer func() { err = closeRows(rows, err) }()
	for rows.Next() {
		var record PlaySessionRecord
		var userID, lastActivity any
		if err := rows.Scan(&record.ID, &userID, &lastActivity, &record.State); err != nil {
			return nil, err
		}
		if record.UserID, err = scanUUID(userID); err != nil {
			return nil, err
		}
		if record.LastActivity, err = s.stores.Dialect().ScanTime(lastActivity); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// PlayGrants returns the grants last used at or after since, oldest first.
func (s *PlaySessionStore) PlayGrants(ctx context.Context, since time.Time) (grants []PlayGrantRecord, err error) {
	rows, err := s.stores.Reader().QueryContext(ctx,
		"SELECT play_session_id, user_id, item_id, media_source_ids, last_used FROM play_session_grants WHERE last_used >= "+s.p(1)+" ORDER BY last_used ASC, play_session_id ASC",
		s.timeValue(since))
	if err != nil {
		return nil, fmt.Errorf("list play session grants: %w", err)
	}
	defer func() { err = closeRows(rows, err) }()
	for rows.Next() {
		var grant PlayGrantRecord
		var userID, itemID, lastUsed any
		var sources string
		if err := rows.Scan(&grant.PlaySessionID, &userID, &itemID, &sources, &lastUsed); err != nil {
			return nil, err
		}
		if grant.UserID, err = scanUUID(userID); err != nil {
			return nil, err
		}
		if grant.ItemID, err = scanUUID(itemID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(sources), &grant.MediaSourceIDs); err != nil {
			return nil, fmt.Errorf("decode play session grant sources: %w", err)
		}
		if grant.LastUsed, err = s.stores.Dialect().ScanTime(lastUsed); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// DeleteStalePlaySessions removes the sessions last active before sessions
// and the grants last used before grants.
func (s *PlaySessionStore) DeleteStalePlaySessions(ctx context.Context, sessions, grants time.Time) error {
	if _, err := s.stores.Writer().ExecContext(ctx, "DELETE FROM play_sessions WHERE last_activity < "+s.p(1), s.timeValue(sessions)); err != nil {
		return fmt.Errorf("delete stale play sessions: %w", err)
	}
	if _, err := s.stores.Writer().ExecContext(ctx, "DELETE FROM play_session_grants WHERE last_used < "+s.p(1), s.timeValue(grants)); err != nil {
		return fmt.Errorf("delete stale play session grants: %w", err)
	}
	return nil
}
