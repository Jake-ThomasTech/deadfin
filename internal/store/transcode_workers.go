// SPDX-License-Identifier: GPL-2.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrJoinTokenUnusable reports a join token that is revoked, expired or has
// no uses left. Enrollment with it fails and stores nothing.
var ErrJoinTokenUnusable = errors.New("transcode worker join token is revoked, expired or spent")

// Transcode worker input modes (docs/spikes/transcode-workers.md §3.3).
const (
	TranscodeWorkerInputShared = "shared"
	TranscodeWorkerInputStream = "stream"
	TranscodeWorkerInputAuto   = "auto"
)

// ValidTranscodeWorkerInputMode reports whether mode is a stored input mode.
func ValidTranscodeWorkerInputMode(mode string) bool {
	switch mode {
	case TranscodeWorkerInputShared, TranscodeWorkerInputStream, TranscodeWorkerInputAuto:
		return true
	}
	return false
}

// TranscodeWorkerJoinToken is one join token (#615). DerivedKey is
// HMAC(secret, label); the secret itself is never stored.
type TranscodeWorkerJoinToken struct {
	ID          string
	Name        string
	DerivedKey  []byte
	Fingerprint string
	// Expires is zero when the token does not expire.
	Expires time.Time
	// UsesLeft is nil when the token has no use limit.
	UsesLeft *int
	Created  time.Time
	// Revoked is zero while the token is active.
	Revoked time.Time
}

// Usable reports whether the token can enroll a worker at now.
func (token TranscodeWorkerJoinToken) Usable(now time.Time) bool {
	if !token.Revoked.IsZero() || (!token.Expires.IsZero() && !now.Before(token.Expires)) {
		return false
	}
	return token.UsesLeft == nil || *token.UsesLeft > 0
}

// TranscodeWorker is one enrolled worker identity.
type TranscodeWorker struct {
	ID          string
	Name        string
	JoinTokenID string
	DerivedKey  []byte
	InputMode   string
	// Pools are the worker's pool labels, sorted.
	Pools    []string
	Created  time.Time
	LastSeen time.Time
	// Revoked is zero while the identity is active.
	Revoked time.Time
}

// TranscodeWorkerUpdate changes the operator-owned fields of a worker. A nil
// field keeps its stored value.
type TranscodeWorkerUpdate struct {
	Name      *string
	InputMode *string
	Pools     *[]string
}

// TranscodeWorkerStore persists transcode worker join tokens, worker
// identities, pool labels and the extra server key pins.
type TranscodeWorkerStore struct{ stores Stores }

// NewTranscodeWorkerStore constructs the transcode worker adapter.
func NewTranscodeWorkerStore(stores Stores) *TranscodeWorkerStore {
	return &TranscodeWorkerStore{stores: stores}
}

func (s *TranscodeWorkerStore) p(index int) string { return s.stores.Dialect().Placeholder(index) }

func (s *TranscodeWorkerStore) placeholders(count int) string {
	result := s.p(1)
	for index := 2; index <= count; index++ {
		result += ", " + s.p(index)
	}
	return result
}

func (s *TranscodeWorkerStore) nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return s.stores.Dialect().TimeValue(value.UTC())
}

func (s *TranscodeWorkerStore) scanNullTime(value any) (time.Time, error) {
	if value == nil {
		return time.Time{}, nil
	}
	return s.stores.Dialect().ScanTime(value)
}

const joinTokenColumns = "id, name, derived_key, fingerprint, expires, uses_left, created, revoked"

func (s *TranscodeWorkerStore) scanJoinToken(row rowScanner) (TranscodeWorkerJoinToken, error) {
	var token TranscodeWorkerJoinToken
	var expires, created, revoked any
	var uses sql.NullInt64
	if err := row.Scan(&token.ID, &token.Name, &token.DerivedKey, &token.Fingerprint, &expires, &uses, &created, &revoked); err != nil {
		return TranscodeWorkerJoinToken{}, err
	}
	var err error
	if token.Expires, err = s.scanNullTime(expires); err != nil {
		return TranscodeWorkerJoinToken{}, err
	}
	if token.Created, err = s.stores.Dialect().ScanTime(created); err != nil {
		return TranscodeWorkerJoinToken{}, err
	}
	if token.Revoked, err = s.scanNullTime(revoked); err != nil {
		return TranscodeWorkerJoinToken{}, err
	}
	if uses.Valid {
		left := int(uses.Int64)
		token.UsesLeft = &left
	}
	return token, nil
}

// CreateJoinToken stores a new join token.
func (s *TranscodeWorkerStore) CreateJoinToken(ctx context.Context, token TranscodeWorkerJoinToken) error {
	if token.ID == "" || len(token.DerivedKey) == 0 || token.Created.IsZero() {
		return errors.New("create transcode worker join token: id, key and created time are required")
	}
	var uses any
	if token.UsesLeft != nil {
		uses = *token.UsesLeft
	}
	_, err := s.stores.Writer().ExecContext(ctx,
		"INSERT INTO transcode_worker_join_tokens ("+joinTokenColumns+") VALUES ("+s.placeholders(8)+")",
		token.ID, token.Name, token.DerivedKey, token.Fingerprint, s.nullTime(token.Expires), uses,
		s.stores.Dialect().TimeValue(token.Created.UTC()), s.nullTime(token.Revoked))
	if err != nil {
		return fmt.Errorf("create transcode worker join token: %w", err)
	}
	return nil
}

// JoinToken returns one join token, or ErrNotFound.
func (s *TranscodeWorkerStore) JoinToken(ctx context.Context, id string) (TranscodeWorkerJoinToken, error) {
	token, err := s.scanJoinToken(s.stores.Reader().QueryRowContext(ctx,
		"SELECT "+joinTokenColumns+" FROM transcode_worker_join_tokens WHERE id = "+s.p(1), id))
	if errors.Is(err, sql.ErrNoRows) {
		return TranscodeWorkerJoinToken{}, ErrNotFound
	}
	return token, err
}

// JoinTokens returns every join token, oldest first.
func (s *TranscodeWorkerStore) JoinTokens(ctx context.Context) (tokens []TranscodeWorkerJoinToken, err error) {
	rows, err := s.stores.Reader().QueryContext(ctx,
		"SELECT "+joinTokenColumns+" FROM transcode_worker_join_tokens ORDER BY created ASC, id ASC")
	if err != nil {
		return nil, err
	}
	defer func() { err = closeRows(rows, err) }()
	for rows.Next() {
		token, err := s.scanJoinToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// RevokeJoinToken stops new enrollments with a token. Workers it enrolled
// stay. Revoking a revoked token keeps the first revocation time.
func (s *TranscodeWorkerStore) RevokeJoinToken(ctx context.Context, id string, now time.Time) error {
	return s.revoke(ctx, "transcode_worker_join_tokens", id, now)
}

func (s *TranscodeWorkerStore) revoke(ctx context.Context, table, id string, now time.Time) error {
	result, err := s.stores.Writer().ExecContext(ctx,
		"UPDATE "+table+" SET revoked = COALESCE(revoked, "+s.p(1)+") WHERE id = "+s.p(2),
		s.stores.Dialect().TimeValue(now.UTC()), id)
	if err != nil {
		return fmt.Errorf("revoke %s: %w", table, err)
	}
	if changed, err := result.RowsAffected(); err == nil && changed == 0 {
		return ErrNotFound
	}
	return nil
}

// EnrollWorker spends one use of the join token and stores the worker
// identity in one transaction. It returns ErrJoinTokenUnusable when the
// token is revoked, expired at now or spent, and ErrNotFound when it does
// not exist.
func (s *TranscodeWorkerStore) EnrollWorker(ctx context.Context, worker TranscodeWorker, now time.Time) (err error) {
	if worker.ID == "" || worker.JoinTokenID == "" || len(worker.DerivedKey) == 0 {
		return errors.New("enroll transcode worker: id, join token and key are required")
	}
	if worker.InputMode == "" {
		worker.InputMode = TranscodeWorkerInputAuto
	}
	if !ValidTranscodeWorkerInputMode(worker.InputMode) {
		return fmt.Errorf("enroll transcode worker: input mode %q is not shared, stream or auto", worker.InputMode)
	}
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	token, err := s.scanJoinToken(tx.QueryRowContext(ctx,
		"SELECT "+joinTokenColumns+" FROM transcode_worker_join_tokens WHERE id = "+s.p(1), worker.JoinTokenID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("enroll transcode worker: %w", err)
	}
	if !token.Usable(now) {
		return ErrJoinTokenUnusable
	}
	if token.UsesLeft != nil {
		// The guard keeps a concurrent enrollment from spending a use
		// twice on a backend without the single-writer lock.
		result, err := tx.ExecContext(ctx,
			"UPDATE transcode_worker_join_tokens SET uses_left = uses_left - 1 WHERE id = "+s.p(1)+" AND uses_left > 0", token.ID)
		if err != nil {
			return fmt.Errorf("enroll transcode worker: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return ErrJoinTokenUnusable
		}
	}
	d := s.stores.Dialect()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO transcode_workers (id, name, join_token_id, derived_key, input_mode, created, last_seen, revoked) VALUES ("+s.placeholders(8)+")",
		worker.ID, worker.Name, worker.JoinTokenID, worker.DerivedKey, worker.InputMode,
		d.TimeValue(now.UTC()), d.TimeValue(now.UTC()), nil); err != nil {
		return fmt.Errorf("enroll transcode worker: %w", err)
	}
	if err := s.replacePoolsTx(ctx, tx, worker.ID, worker.Pools); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *TranscodeWorkerStore) replacePoolsTx(ctx context.Context, tx *sql.Tx, workerID string, pools []string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM transcode_worker_pools WHERE worker_id = "+s.p(1), workerID); err != nil {
		return fmt.Errorf("replace transcode worker pools: %w", err)
	}
	for _, pool := range NormalizeTranscodeWorkerPools(pools) {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO transcode_worker_pools (worker_id, pool) VALUES ("+s.placeholders(2)+")", workerID, pool); err != nil {
			return fmt.Errorf("replace transcode worker pools: %w", err)
		}
	}
	return nil
}

// NormalizeTranscodeWorkerPools trims, lower-cases, removes empty and
// duplicate labels, and sorts.
func NormalizeTranscodeWorkerPools(pools []string) []string {
	seen := make(map[string]bool, len(pools))
	var result []string
	for _, pool := range pools {
		pool = strings.ToLower(strings.TrimSpace(pool))
		if pool == "" || seen[pool] {
			continue
		}
		seen[pool] = true
		result = append(result, pool)
	}
	sort.Strings(result)
	return result
}

const workerColumns = "id, name, join_token_id, derived_key, input_mode, created, last_seen, revoked"

func (s *TranscodeWorkerStore) scanWorker(row rowScanner) (TranscodeWorker, error) {
	var worker TranscodeWorker
	var created, lastSeen, revoked any
	if err := row.Scan(&worker.ID, &worker.Name, &worker.JoinTokenID, &worker.DerivedKey, &worker.InputMode, &created, &lastSeen, &revoked); err != nil {
		return TranscodeWorker{}, err
	}
	var err error
	d := s.stores.Dialect()
	if worker.Created, err = d.ScanTime(created); err != nil {
		return TranscodeWorker{}, err
	}
	if worker.LastSeen, err = d.ScanTime(lastSeen); err != nil {
		return TranscodeWorker{}, err
	}
	if worker.Revoked, err = s.scanNullTime(revoked); err != nil {
		return TranscodeWorker{}, err
	}
	return worker, nil
}

// Worker returns one worker identity with its pools, or ErrNotFound.
func (s *TranscodeWorkerStore) Worker(ctx context.Context, id string) (TranscodeWorker, error) {
	worker, err := s.scanWorker(s.stores.Reader().QueryRowContext(ctx,
		"SELECT "+workerColumns+" FROM transcode_workers WHERE id = "+s.p(1), id))
	if errors.Is(err, sql.ErrNoRows) {
		return TranscodeWorker{}, ErrNotFound
	}
	if err != nil {
		return TranscodeWorker{}, err
	}
	pools, err := s.pools(ctx, id)
	if err != nil {
		return TranscodeWorker{}, err
	}
	worker.Pools = pools[id]
	return worker, nil
}

// Workers returns every worker identity with its pools, oldest first.
func (s *TranscodeWorkerStore) Workers(ctx context.Context) (workers []TranscodeWorker, err error) {
	rows, err := s.stores.Reader().QueryContext(ctx,
		"SELECT "+workerColumns+" FROM transcode_workers ORDER BY created ASC, id ASC")
	if err != nil {
		return nil, err
	}
	defer func() { err = closeRows(rows, err) }()
	for rows.Next() {
		worker, err := s.scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, worker)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pools, err := s.pools(ctx, "")
	if err != nil {
		return nil, err
	}
	for index := range workers {
		workers[index].Pools = pools[workers[index].ID]
	}
	return workers, nil
}

// pools returns the pool labels of one worker, or of every worker when id is
// empty.
func (s *TranscodeWorkerStore) pools(ctx context.Context, id string) (result map[string][]string, err error) {
	query := "SELECT worker_id, pool FROM transcode_worker_pools"
	var args []any
	if id != "" {
		query += " WHERE worker_id = " + s.p(1)
		args = append(args, id)
	}
	rows, err := s.stores.Reader().QueryContext(ctx, query+" ORDER BY worker_id ASC, pool ASC", args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = closeRows(rows, err) }()
	result = make(map[string][]string)
	for rows.Next() {
		var worker, pool string
		if err := rows.Scan(&worker, &pool); err != nil {
			return nil, err
		}
		result[worker] = append(result[worker], pool)
	}
	return result, rows.Err()
}

// UpdateWorker changes the name, input mode or pools of a worker.
func (s *TranscodeWorkerStore) UpdateWorker(ctx context.Context, id string, update TranscodeWorkerUpdate) (err error) {
	if update.InputMode != nil && !ValidTranscodeWorkerInputMode(*update.InputMode) {
		return fmt.Errorf("update transcode worker: input mode %q is not shared, stream or auto", *update.InputMode)
	}
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM transcode_workers WHERE id = "+s.p(1), id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if update.Name != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE transcode_workers SET name = "+s.p(1)+" WHERE id = "+s.p(2), *update.Name, id); err != nil {
			return fmt.Errorf("update transcode worker: %w", err)
		}
	}
	if update.InputMode != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE transcode_workers SET input_mode = "+s.p(1)+" WHERE id = "+s.p(2), *update.InputMode, id); err != nil {
			return fmt.Errorf("update transcode worker: %w", err)
		}
	}
	if update.Pools != nil {
		if err := s.replacePoolsTx(ctx, tx, id, *update.Pools); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RevokeWorker deletes the worker's ability to connect. The row stays, so
// the operator sees the revoked identity; the key is never accepted again.
func (s *TranscodeWorkerStore) RevokeWorker(ctx context.Context, id string, now time.Time) error {
	return s.revoke(ctx, "transcode_workers", id, now)
}

// DeleteWorker removes a worker identity and its pool labels (#623: an
// ephemeral worker's identity ends with its connection). A missing worker
// is not an error.
func (s *TranscodeWorkerStore) DeleteWorker(ctx context.Context, id string) (err error) {
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete transcode worker: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, "DELETE FROM transcode_worker_pools WHERE worker_id = "+s.p(1), id); err != nil {
		return fmt.Errorf("delete transcode worker pools: %w", err)
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM transcode_workers WHERE id = "+s.p(1), id); err != nil {
		return fmt.Errorf("delete transcode worker: %w", err)
	}
	return tx.Commit()
}

// TouchWorker records a connection of the worker at now.
func (s *TranscodeWorkerStore) TouchWorker(ctx context.Context, id string, now time.Time) error {
	_, err := s.stores.Writer().ExecContext(ctx,
		"UPDATE transcode_workers SET last_seen = "+s.p(1)+" WHERE id = "+s.p(2),
		s.stores.Dialect().TimeValue(now.UTC()), id)
	if err != nil {
		return fmt.Errorf("touch transcode worker: %w", err)
	}
	return nil
}

// Pins returns the extra server key fingerprints, sorted.
func (s *TranscodeWorkerStore) Pins(ctx context.Context) (pins []string, err error) {
	rows, err := s.stores.Reader().QueryContext(ctx, "SELECT fingerprint FROM transcode_worker_pins ORDER BY fingerprint ASC")
	if err != nil {
		return nil, err
	}
	defer func() { err = closeRows(rows, err) }()
	for rows.Next() {
		var pin string
		if err := rows.Scan(&pin); err != nil {
			return nil, err
		}
		pins = append(pins, pin)
	}
	return pins, rows.Err()
}

// SetPins replaces the extra server key fingerprints.
func (s *TranscodeWorkerStore) SetPins(ctx context.Context, pins []string, now time.Time) (err error) {
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	if _, err := tx.ExecContext(ctx, "DELETE FROM transcode_worker_pins"); err != nil {
		return fmt.Errorf("set transcode worker pins: %w", err)
	}
	seen := make(map[string]bool, len(pins))
	for _, pin := range pins {
		if pin == "" || seen[pin] {
			continue
		}
		seen[pin] = true
		if _, err := tx.ExecContext(ctx, "INSERT INTO transcode_worker_pins (fingerprint, created) VALUES ("+s.placeholders(2)+")",
			pin, s.stores.Dialect().TimeValue(now.UTC())); err != nil {
			return fmt.Errorf("set transcode worker pins: %w", err)
		}
	}
	return tx.Commit()
}

// Configured reports whether an operator ever created a join token. Until
// then the worker connect route answers 404: transcode workers are off.
// After it, the route stays, so a revoked worker gets a refusal it does not
// retry.
func (s *TranscodeWorkerStore) Configured(ctx context.Context) (bool, error) {
	var found int
	err := s.stores.Reader().QueryRowContext(ctx, "SELECT 1 FROM transcode_worker_join_tokens LIMIT 1").Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Policy returns the stored placement policy JSON (#616), or ErrNotFound
// when the operator never set one.
func (s *TranscodeWorkerStore) Policy(ctx context.Context) ([]byte, error) {
	var policy string
	err := s.stores.Reader().QueryRowContext(ctx, "SELECT policy FROM transcode_worker_policy WHERE id = 1").Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read transcode worker policy: %w", err)
	}
	return []byte(policy), nil
}

// SetPolicy replaces the stored placement policy JSON.
func (s *TranscodeWorkerStore) SetPolicy(ctx context.Context, policy []byte, now time.Time) error {
	_, err := s.stores.Writer().ExecContext(ctx,
		"INSERT INTO transcode_worker_policy (id, policy, updated) VALUES (1, "+s.p(1)+", "+s.p(2)+
			") ON CONFLICT (id) DO UPDATE SET policy = EXCLUDED.policy, updated = EXCLUDED.updated",
		string(policy), s.stores.Dialect().TimeValue(now.UTC()))
	if err != nil {
		return fmt.Errorf("set transcode worker policy: %w", err)
	}
	return nil
}

// ScalingTokenHash returns the SHA-256 of the dashboard scaling token
// (#617), or ErrNotFound when none is set.
func (s *TranscodeWorkerStore) ScalingTokenHash(ctx context.Context) ([]byte, error) {
	var hash []byte
	err := s.stores.Reader().QueryRowContext(ctx, "SELECT token_hash FROM transcode_worker_scaling_token WHERE id = 1").Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read transcode scaling token: %w", err)
	}
	return hash, nil
}

// SetScalingTokenHash replaces the dashboard scaling token.
func (s *TranscodeWorkerStore) SetScalingTokenHash(ctx context.Context, hash []byte, now time.Time) error {
	_, err := s.stores.Writer().ExecContext(ctx,
		"INSERT INTO transcode_worker_scaling_token (id, token_hash, created) VALUES (1, "+s.p(1)+", "+s.p(2)+
			") ON CONFLICT (id) DO UPDATE SET token_hash = EXCLUDED.token_hash, created = EXCLUDED.created",
		hash, s.stores.Dialect().TimeValue(now.UTC()))
	if err != nil {
		return fmt.Errorf("set transcode scaling token: %w", err)
	}
	return nil
}

// DeleteScalingToken removes the dashboard scaling token.
func (s *TranscodeWorkerStore) DeleteScalingToken(ctx context.Context) error {
	if _, err := s.stores.Writer().ExecContext(ctx, "DELETE FROM transcode_worker_scaling_token"); err != nil {
		return fmt.Errorf("delete transcode scaling token: %w", err)
	}
	return nil
}

// TranscodeWorkerJob is the record of one job that runs on a transcode
// worker (#618). The worker keeps the job through a server restart; the next
// server process adopts it when the worker presents it before LeaseExpires.
type TranscodeWorkerJob struct {
	ID       string
	WorkerID string
	// Playlist and SegmentPattern are canonical paths in the transcode
	// directory; they name the job's family.
	Playlist       string
	SegmentPattern string
	PlaySessionID  string
	DeviceID       string
	// TokenHash is SHA-256 of the job's data-plane token.
	TokenHash     []byte
	SegmentLength int
	StartNumber   int
	// InputMode is the worker's input mode at placement. SourcePath and
	// SourceSize name the source that a streamed job reads.
	InputMode    string
	SourcePath   string
	SourceSize   int64
	Created      time.Time
	LeaseExpires time.Time
	// Owner is the server replica that placed or adopted the job (#652);
	// "" without replica mode.
	Owner string
}

const transcodeWorkerJobColumns = "id, worker_id, playlist, segment_pattern, play_session_id, device_id, token_hash, segment_length, start_number, input_mode, source_path, source_size, created, lease_expires, owner"

func (s *TranscodeWorkerStore) scanJob(row rowScanner) (TranscodeWorkerJob, error) {
	var job TranscodeWorkerJob
	var created, expires any
	if err := row.Scan(&job.ID, &job.WorkerID, &job.Playlist, &job.SegmentPattern, &job.PlaySessionID, &job.DeviceID,
		&job.TokenHash, &job.SegmentLength, &job.StartNumber, &job.InputMode, &job.SourcePath, &job.SourceSize, &created, &expires, &job.Owner); err != nil {
		return TranscodeWorkerJob{}, err
	}
	var err error
	if job.Created, err = s.stores.Dialect().ScanTime(created); err != nil {
		return TranscodeWorkerJob{}, err
	}
	if job.LeaseExpires, err = s.stores.Dialect().ScanTime(expires); err != nil {
		return TranscodeWorkerJob{}, err
	}
	return job, nil
}

// CreateJob stores a new job record.
func (s *TranscodeWorkerStore) CreateJob(ctx context.Context, job TranscodeWorkerJob) error {
	if job.ID == "" || job.WorkerID == "" || job.Playlist == "" || job.SegmentPattern == "" || len(job.TokenHash) == 0 ||
		job.SegmentLength <= 0 || job.Created.IsZero() || job.LeaseExpires.IsZero() {
		return errors.New("create transcode worker job: id, worker, family, token, segment length and times are required")
	}
	if job.InputMode == "" {
		job.InputMode = TranscodeWorkerInputAuto
	}
	dialect := s.stores.Dialect()
	_, err := s.stores.Writer().ExecContext(ctx,
		"INSERT INTO transcode_worker_jobs ("+transcodeWorkerJobColumns+") VALUES ("+s.placeholders(15)+")",
		job.ID, job.WorkerID, job.Playlist, job.SegmentPattern, job.PlaySessionID, job.DeviceID, job.TokenHash,
		job.SegmentLength, job.StartNumber, job.InputMode, job.SourcePath, job.SourceSize, dialect.TimeValue(job.Created.UTC()), dialect.TimeValue(job.LeaseExpires.UTC()), job.Owner)
	if err != nil {
		return fmt.Errorf("create transcode worker job: %w", err)
	}
	return nil
}

// Job returns one job record, or ErrNotFound.
func (s *TranscodeWorkerStore) Job(ctx context.Context, id string) (TranscodeWorkerJob, error) {
	job, err := s.scanJob(s.stores.Reader().QueryRowContext(ctx,
		"SELECT "+transcodeWorkerJobColumns+" FROM transcode_worker_jobs WHERE id = "+s.p(1), id))
	if errors.Is(err, sql.ErrNoRows) {
		return TranscodeWorkerJob{}, ErrNotFound
	}
	return job, err
}

// LiveJobs returns the job records whose lease is not over at now, oldest
// first.
func (s *TranscodeWorkerStore) LiveJobs(ctx context.Context, now time.Time) (jobs []TranscodeWorkerJob, err error) {
	rows, err := s.stores.Reader().QueryContext(ctx,
		"SELECT "+transcodeWorkerJobColumns+" FROM transcode_worker_jobs WHERE lease_expires > "+s.p(1)+" ORDER BY created ASC, id ASC",
		s.stores.Dialect().TimeValue(now.UTC()))
	if err != nil {
		return nil, fmt.Errorf("list transcode worker jobs: %w", err)
	}
	defer func() { err = closeRows(rows, err) }()
	for rows.Next() {
		job, err := s.scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// RenewJobs moves the lease of the named job records to expires.
func (s *TranscodeWorkerStore) RenewJobs(ctx context.Context, ids []string, expires time.Time) (err error) {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.stores.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnReturn(tx, &err)
	value := s.stores.Dialect().TimeValue(expires.UTC())
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE transcode_worker_jobs SET lease_expires = "+s.p(1)+" WHERE id = "+s.p(2), value, id); err != nil {
			return fmt.Errorf("renew transcode worker job: %w", err)
		}
	}
	return tx.Commit()
}

// DeleteJob removes one job record. A missing record is not an error.
func (s *TranscodeWorkerStore) DeleteJob(ctx context.Context, id string) error {
	if _, err := s.stores.Writer().ExecContext(ctx, "DELETE FROM transcode_worker_jobs WHERE id = "+s.p(1), id); err != nil {
		return fmt.Errorf("delete transcode worker job: %w", err)
	}
	return nil
}

// DeleteExpiredJobs removes the job records whose lease is over at now.
func (s *TranscodeWorkerStore) DeleteExpiredJobs(ctx context.Context, now time.Time) error {
	if _, err := s.stores.Writer().ExecContext(ctx, "DELETE FROM transcode_worker_jobs WHERE lease_expires <= "+s.p(1),
		s.stores.Dialect().TimeValue(now.UTC())); err != nil {
		return fmt.Errorf("delete expired transcode worker jobs: %w", err)
	}
	return nil
}
