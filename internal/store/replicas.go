// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Replica is one server process in replica mode (#652). A replica is live
// while LeaseExpires is after now.
type Replica struct {
	ID           string
	URL          string
	Draining     bool
	LeaseExpires time.Time
}

// ReplicaStore keeps the replica rows, the forward key and the owners of the
// rows that replicas serve: play sessions, PlaySessionId grants and remote
// transcode jobs (#652).
type ReplicaStore struct{ stores Stores }

// NewReplicaStore constructs the replica adapter.
func NewReplicaStore(stores Stores) *ReplicaStore {
	return &ReplicaStore{stores: stores}
}

func (s *ReplicaStore) p(index int) string { return s.stores.Dialect().Placeholder(index) }

func (s *ReplicaStore) timeValue(value time.Time) any {
	return s.stores.Dialect().TimeValue(value.UTC())
}

// claimable is the condition under which replica me can take a row of table:
// the row has no owner, me owns it, or its owner is not live at now. It
// uses the placeholders first and first+1.
func (s *ReplicaStore) claimable(table string, first int) string {
	return "(" + table + ".owner = '' OR " + table + ".owner = " + s.p(first) +
		" OR NOT EXISTS (SELECT 1 FROM server_replicas r WHERE r.id = " + table + ".owner AND r.lease_expires > " + s.p(first+1) + "))"
}

// RenewReplica stores the row of replica.
func (s *ReplicaStore) RenewReplica(ctx context.Context, replica Replica) error {
	if replica.ID == "" || replica.URL == "" || replica.LeaseExpires.IsZero() {
		return errors.New("renew replica: id, url and lease are required")
	}
	draining := 0
	if replica.Draining {
		draining = 1
	}
	_, err := s.stores.Writer().ExecContext(ctx,
		"INSERT INTO server_replicas (id, url, draining, lease_expires) VALUES ("+s.p(1)+", "+s.p(2)+", "+s.p(3)+", "+s.p(4)+") "+
			"ON CONFLICT (id) DO UPDATE SET url = excluded.url, draining = excluded.draining, lease_expires = excluded.lease_expires",
		replica.ID, replica.URL, draining, s.timeValue(replica.LeaseExpires))
	if err != nil {
		return fmt.Errorf("renew replica: %w", err)
	}
	return nil
}

// LiveReplicas returns the replicas whose lease is not over at now, by ID.
func (s *ReplicaStore) LiveReplicas(ctx context.Context, now time.Time) (replicas []Replica, err error) {
	rows, err := s.stores.Reader().QueryContext(ctx,
		"SELECT id, url, draining, lease_expires FROM server_replicas WHERE lease_expires > "+s.p(1)+" ORDER BY id ASC", s.timeValue(now))
	if err != nil {
		return nil, fmt.Errorf("list replicas: %w", err)
	}
	defer func() { err = closeRows(rows, err) }()
	for rows.Next() {
		var replica Replica
		var draining int
		var expires any
		if err := rows.Scan(&replica.ID, &replica.URL, &draining, &expires); err != nil {
			return nil, err
		}
		replica.Draining = draining != 0
		if replica.LeaseExpires, err = s.stores.Dialect().ScanTime(expires); err != nil {
			return nil, err
		}
		replicas = append(replicas, replica)
	}
	return replicas, rows.Err()
}

// DeleteExpiredReplicas removes the rows of the replicas whose lease ended
// before now.
func (s *ReplicaStore) DeleteExpiredReplicas(ctx context.Context, now time.Time) error {
	if _, err := s.stores.Writer().ExecContext(ctx, "DELETE FROM server_replicas WHERE lease_expires <= "+s.p(1), s.timeValue(now)); err != nil {
		return fmt.Errorf("delete expired replicas: %w", err)
	}
	return nil
}

// ReplicaSecret returns the forward key. The first caller stores the key
// that generate makes; every later caller reads it.
func (s *ReplicaStore) ReplicaSecret(ctx context.Context, generate func() ([]byte, error)) ([]byte, error) {
	var secret []byte
	err := s.stores.Reader().QueryRowContext(ctx, "SELECT secret FROM server_replica_secret WHERE id = 1").Scan(&secret)
	if err == nil {
		return secret, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read replica secret: %w", err)
	}
	candidate, err := generate()
	if err != nil {
		return nil, err
	}
	if len(candidate) == 0 {
		return nil, errors.New("replica secret is empty")
	}
	if _, err := s.stores.Writer().ExecContext(ctx,
		"INSERT INTO server_replica_secret (id, secret) VALUES (1, "+s.p(1)+") ON CONFLICT (id) DO NOTHING", candidate); err != nil {
		return nil, fmt.Errorf("store replica secret: %w", err)
	}
	if err := s.stores.Writer().QueryRowContext(ctx, "SELECT secret FROM server_replica_secret WHERE id = 1").Scan(&secret); err != nil {
		return nil, fmt.Errorf("read replica secret: %w", err)
	}
	return secret, nil
}

// ReleaseReplica clears the owner of every row that replica id owns and
// deletes its replica row, in one transaction. The other replicas can then
// claim the rows at once.
func (s *ReplicaStore) ReleaseReplica(ctx context.Context, id string) (err error) {
	if id == "" {
		return errors.New("release replica: id is required")
	}
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	for _, table := range []string{"play_sessions", "play_session_grants", "transcode_worker_jobs"} {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET owner = '' WHERE owner = "+s.p(1), id); err != nil {
			return fmt.Errorf("release %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM server_replicas WHERE id = "+s.p(1), id); err != nil {
		return fmt.Errorf("delete replica: %w", err)
	}
	return tx.Commit()
}

// PlaySessionOwner returns the owner of a play session row. found is false
// when there is no row.
func (s *ReplicaStore) PlaySessionOwner(ctx context.Context, id string) (owner string, found bool, err error) {
	return s.owner(ctx, "play_sessions", "id", id)
}

// PlayGrantOwner returns the owner of a PlaySessionId grant row. found is
// false when there is no row.
func (s *ReplicaStore) PlayGrantOwner(ctx context.Context, playSessionID string) (owner string, found bool, err error) {
	return s.owner(ctx, "play_session_grants", "play_session_id", playSessionID)
}

func (s *ReplicaStore) owner(ctx context.Context, table, column, id string) (string, bool, error) {
	var owner string
	err := s.stores.Reader().QueryRowContext(ctx, "SELECT owner FROM "+table+" WHERE "+column+" = "+s.p(1), id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s owner: %w", table, err)
	}
	return owner, true, nil
}

const playSessionColumns = "id, user_id, last_activity, state"

func (s *ReplicaStore) scanPlaySession(row rowScanner) (PlaySessionRecord, error) {
	var record PlaySessionRecord
	var userID, lastActivity any
	if err := row.Scan(&record.ID, &userID, &lastActivity, &record.State); err != nil {
		return PlaySessionRecord{}, err
	}
	var err error
	if record.UserID, err = scanUUID(userID); err != nil {
		return PlaySessionRecord{}, err
	}
	if record.LastActivity, err = s.stores.Dialect().ScanTime(lastActivity); err != nil {
		return PlaySessionRecord{}, err
	}
	return record, nil
}

const playGrantColumns = "play_session_id, user_id, item_id, media_source_ids, last_used"

func (s *ReplicaStore) scanPlayGrant(row rowScanner) (PlayGrantRecord, error) {
	var grant PlayGrantRecord
	var userID, itemID, lastUsed any
	var sources string
	if err := row.Scan(&grant.PlaySessionID, &userID, &itemID, &sources, &lastUsed); err != nil {
		return PlayGrantRecord{}, err
	}
	var err error
	if grant.UserID, err = scanUUID(userID); err != nil {
		return PlayGrantRecord{}, err
	}
	if grant.ItemID, err = scanUUID(itemID); err != nil {
		return PlayGrantRecord{}, err
	}
	if err := json.Unmarshal([]byte(sources), &grant.MediaSourceIDs); err != nil {
		return PlayGrantRecord{}, fmt.Errorf("decode play session grant sources: %w", err)
	}
	if grant.LastUsed, err = s.stores.Dialect().ScanTime(lastUsed); err != nil {
		return PlayGrantRecord{}, err
	}
	return grant, nil
}

// ClaimPlaySession makes me the owner of the play session row id when the
// row is claimable at now, and returns the row. claimed is false when there
// is no row or a live replica other than me owns it.
func (s *ReplicaStore) ClaimPlaySession(ctx context.Context, id, me string, now time.Time) (record PlaySessionRecord, claimed bool, err error) {
	record, err = s.scanPlaySession(s.stores.Writer().QueryRowContext(ctx,
		"UPDATE play_sessions SET owner = "+s.p(1)+" WHERE id = "+s.p(2)+" AND "+s.claimable("play_sessions", 3)+" RETURNING "+playSessionColumns,
		me, id, me, s.timeValue(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return PlaySessionRecord{}, false, nil
	}
	if err != nil {
		return PlaySessionRecord{}, false, fmt.Errorf("claim play session: %w", err)
	}
	return record, true, nil
}

// ClaimNewPlaySession stores record with owner me when there is no row for
// it, or claims the existing row when it is claimable at now. It reports
// false when a live replica other than me owns the row.
func (s *ReplicaStore) ClaimNewPlaySession(ctx context.Context, record PlaySessionRecord, me string, now time.Time) (claimed bool, err error) {
	if record.ID == "" || record.LastActivity.IsZero() {
		return false, errors.New("claim play session: id and last activity are required")
	}
	result, err := s.stores.Writer().ExecContext(ctx,
		"INSERT INTO play_sessions (id, user_id, last_activity, state, owner) VALUES ("+s.p(1)+", "+s.p(2)+", "+s.p(3)+", "+s.p(4)+", "+s.p(5)+") ON CONFLICT (id) DO NOTHING",
		record.ID, uuidArgDialect(s.stores.Dialect(), record.UserID), s.timeValue(record.LastActivity), record.State, me)
	if err != nil {
		return false, fmt.Errorf("claim play session: %w", err)
	}
	if inserted, err := result.RowsAffected(); err == nil && inserted == 1 {
		return true, nil
	}
	_, claimed, err = s.ClaimPlaySession(ctx, record.ID, me, now)
	return claimed, err
}

// ClaimPlayGrant makes me the owner of the grant row of playSessionID when
// it is claimable at now, and returns the row. claimed is false when there is
// no row or a live replica other than me owns it.
func (s *ReplicaStore) ClaimPlayGrant(ctx context.Context, playSessionID, me string, now time.Time) (grant PlayGrantRecord, claimed bool, err error) {
	grant, err = s.scanPlayGrant(s.stores.Writer().QueryRowContext(ctx,
		"UPDATE play_session_grants SET owner = "+s.p(1)+" WHERE play_session_id = "+s.p(2)+" AND "+s.claimable("play_session_grants", 3)+" RETURNING "+playGrantColumns,
		me, playSessionID, me, s.timeValue(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return PlayGrantRecord{}, false, nil
	}
	if err != nil {
		return PlayGrantRecord{}, false, fmt.Errorf("claim play session grant: %w", err)
	}
	return grant, true, nil
}

// ClaimPlaySessions claims every claimable play session row last active at
// or after since, and returns them oldest first. A replica calls it at
// startup in place of PlaySessionStore.PlaySessions.
func (s *ReplicaStore) ClaimPlaySessions(ctx context.Context, me string, since, now time.Time) (records []PlaySessionRecord, err error) {
	rows, err := s.stores.Writer().QueryContext(ctx,
		"UPDATE play_sessions SET owner = "+s.p(1)+" WHERE last_activity >= "+s.p(2)+" AND "+s.claimable("play_sessions", 3)+" RETURNING "+playSessionColumns,
		me, s.timeValue(since), me, s.timeValue(now))
	if err != nil {
		return nil, fmt.Errorf("claim play sessions: %w", err)
	}
	defer func() { err = closeRows(rows, err) }()
	for rows.Next() {
		record, err := s.scanPlaySession(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortPlaySessions(records)
	return records, nil
}

// ClaimPlayGrants claims every claimable grant row last used at or after
// since, and returns them oldest first.
func (s *ReplicaStore) ClaimPlayGrants(ctx context.Context, me string, since, now time.Time) (grants []PlayGrantRecord, err error) {
	rows, err := s.stores.Writer().QueryContext(ctx,
		"UPDATE play_session_grants SET owner = "+s.p(1)+" WHERE last_used >= "+s.p(2)+" AND "+s.claimable("play_session_grants", 3)+" RETURNING "+playGrantColumns,
		me, s.timeValue(since), me, s.timeValue(now))
	if err != nil {
		return nil, fmt.Errorf("claim play session grants: %w", err)
	}
	defer func() { err = closeRows(rows, err) }()
	for rows.Next() {
		grant, err := s.scanPlayGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortPlayGrants(grants)
	return grants, nil
}

// ClaimTranscodeJob makes me the owner of the job record id when it is
// claimable at now. It reports false when there is no record or a live
// replica other than me owns it.
func (s *ReplicaStore) ClaimTranscodeJob(ctx context.Context, id, me string, now time.Time) (bool, error) {
	result, err := s.stores.Writer().ExecContext(ctx,
		"UPDATE transcode_worker_jobs SET owner = "+s.p(1)+" WHERE id = "+s.p(2)+" AND "+s.claimable("transcode_worker_jobs", 3),
		me, id, me, s.timeValue(now))
	if err != nil {
		return false, fmt.Errorf("claim transcode worker job: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

func sortPlaySessions(records []PlaySessionRecord) {
	slices.SortFunc(records, func(a, b PlaySessionRecord) int {
		if order := a.LastActivity.Compare(b.LastActivity); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func sortPlayGrants(grants []PlayGrantRecord) {
	slices.SortFunc(grants, func(a, b PlayGrantRecord) int {
		if order := a.LastUsed.Compare(b.LastUsed); order != 0 {
			return order
		}
		return strings.Compare(a.PlaySessionID, b.PlaySessionID)
	})
}
