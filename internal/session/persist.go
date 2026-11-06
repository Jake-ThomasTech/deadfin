// SPDX-License-Identifier: GPL-2.0-only

package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
)

// Session persistence (#620): a Persister writes each changed session behind
// the change, at a bounded rate, and the next server process restores the
// sessions at startup. A client that reports progress or opens its WebSocket
// again after a server restart finds its session. Live command transports and
// report revisions do not persist: a WebSocket attaches again on its own, and
// a report from before the restart cannot commit after it.

const (
	// ReloadWindow is the last-activity cutoff of a restore: a session that
	// had no activity for this long before the restart is not restored.
	ReloadWindow = 24 * time.Hour
	// DefaultPersistInterval is the shortest time between two writes.
	DefaultPersistInterval = 2 * time.Second

	persistStateVersion = 1
)

// persistedSession is the encoded state of one session.
type persistedSession struct {
	Version             int             `json:"v"`
	Client              string          `json:"client"`
	DeviceID            string          `json:"deviceId"`
	DeviceName          string          `json:"deviceName"`
	AppVersion          string          `json:"appVersion"`
	UserID              dto.UUID        `json:"userId"`
	AdditionalUserIDs   []dto.UUID      `json:"additionalUserIds,omitempty"`
	RemoteEndPoint      string          `json:"remoteEndPoint,omitempty"`
	LastActivity        time.Time       `json:"lastActivity"`
	Capabilities        Capabilities    `json:"capabilities"`
	NowPlaying          *PlaybackState  `json:"nowPlaying,omitempty"`
	LastPlaybackCheckIn time.Time       `json:"lastPlaybackCheckIn"`
	LastPausedDate      *time.Time      `json:"lastPausedDate,omitempty"`
	NowPlayingQueue     []dto.QueueItem `json:"nowPlayingQueue,omitempty"`
	PlaylistItemID      string          `json:"playlistItemId,omitempty"`
}

func encodeSession(snapshot *Snapshot) (store.PlaySessionRecord, error) {
	state, err := json.Marshal(persistedSession{
		Version:             persistStateVersion,
		Client:              snapshot.Client,
		DeviceID:            snapshot.DeviceID,
		DeviceName:          snapshot.DeviceName,
		AppVersion:          snapshot.Version,
		UserID:              snapshot.UserID,
		AdditionalUserIDs:   snapshot.AdditionalUserIDs,
		RemoteEndPoint:      snapshot.RemoteEndPoint,
		LastActivity:        snapshot.LastActivity,
		Capabilities:        snapshot.Capabilities,
		NowPlaying:          snapshot.NowPlaying,
		LastPlaybackCheckIn: snapshot.LastPlaybackCheckIn,
		LastPausedDate:      snapshot.LastPausedDate,
		NowPlayingQueue:     snapshot.NowPlayingQueue,
		PlaylistItemID:      snapshot.PlaylistItemID,
	})
	if err != nil {
		return store.PlaySessionRecord{}, err
	}
	return store.PlaySessionRecord{ID: snapshot.ID, UserID: snapshot.UserID, LastActivity: snapshot.LastActivity, State: state}, nil
}

// Record returns the current state of session id as a store record (#652).
func (manager *Manager) Record(id string) (store.PlaySessionRecord, bool, error) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.closed {
		return store.PlaySessionRecord{}, false, nil
	}
	key, found := manager.byID[id]
	if !found {
		return store.PlaySessionRecord{}, false, nil
	}
	record, err := encodeSession(manager.byKey[key])
	return record, err == nil, err
}

// IDFor returns the ID that a new session of client and deviceID gets: the
// upstream MD5 of the two (#652). A replica routes a request by it before
// the session exists.
func IDFor(client, deviceID string) string {
	return sessionID(client + deviceID)
}

// EnablePersistence starts change tracking and returns the channel that
// signals a change. The channel holds at most one signal.
func (manager *Manager) EnablePersistence() <-chan struct{} {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.persistNotify == nil {
		manager.persistNotify = make(chan struct{}, 1)
		manager.dirty = make(map[string]struct{})
	}
	return manager.persistNotify
}

func (manager *Manager) markDirtyLocked(id string) {
	if manager.dirty == nil {
		return
	}
	manager.dirty[id] = struct{}{}
	select {
	case manager.persistNotify <- struct{}{}:
	default:
	}
}

// MarkDirty queues the sessions of ids for the next TakeChanges again, as
// after a failed write.
func (manager *Manager) MarkDirty(ids []string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return
	}
	for _, id := range ids {
		manager.markDirtyLocked(id)
	}
}

// TakeChanges returns the current state of each session changed since the
// last call, and the IDs of the removed ones.
func (manager *Manager) TakeChanges() (records []store.PlaySessionRecord, removed []string, err error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed || len(manager.dirty) == 0 {
		return nil, nil, nil
	}
	for id := range manager.dirty {
		delete(manager.dirty, id)
		key, found := manager.byID[id]
		if !found {
			removed = append(removed, id)
			continue
		}
		record, encodeErr := encodeSession(manager.byKey[key])
		if encodeErr != nil {
			err = errors.Join(err, encodeErr)
			continue
		}
		records = append(records, record)
	}
	return records, removed, err
}

// Restore adds the persisted sessions of a previous server process. The
// session user is the record's UserID, which a user id remap rewrites. It
// skips a session last active before the ReloadWindow, one that does not
// decode, and one whose ID or client and device are already present. Each
// restored session gets a new target generation. It returns the restored
// count.
func (manager *Manager) Restore(records []store.PlaySessionRecord) int {
	cutoff := manager.clock.Now().Add(-ReloadWindow)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return 0
	}
	restored := 0
	for _, record := range records {
		var state persistedSession
		if record.ID == "" || record.UserID.IsNil() || json.Unmarshal(record.State, &state) != nil || state.Version != persistStateVersion {
			continue
		}
		if state.LastActivity.Before(cutoff) || state.Client == "" || state.DeviceID == "" {
			continue
		}
		key := newSessionKey(state.Client, state.DeviceID)
		if _, found := manager.byKey[key]; found {
			continue
		}
		if _, found := manager.byID[record.ID]; found {
			continue
		}
		snapshot := &Snapshot{
			ID:                  record.ID,
			Target:              Target{ID: record.ID, Generation: manager.nextTargetGenerationLocked()},
			Client:              state.Client,
			DeviceID:            state.DeviceID,
			DeviceName:          state.DeviceName,
			Version:             state.AppVersion,
			UserID:              record.UserID,
			AdditionalUserIDs:   state.AdditionalUserIDs,
			RemoteEndPoint:      state.RemoteEndPoint,
			LastActivity:        state.LastActivity,
			Capabilities:        cloneCapabilities(state.Capabilities),
			NowPlaying:          state.NowPlaying,
			LastPlaybackCheckIn: state.LastPlaybackCheckIn,
			LastPausedDate:      state.LastPausedDate,
			NowPlayingQueue:     state.NowPlayingQueue,
			PlaylistItemID:      state.PlaylistItemID,
		}
		manager.byKey[key] = snapshot
		manager.byID[record.ID] = key
		manager.publishChangeLocked(record.ID, ChangeCreated, true)
		restored++
	}
	return restored
}

// GrantChanges is the change feed of a PlaySessionId grant store.
type GrantChanges interface {
	EnablePersistence() <-chan struct{}
	TakeChanges() (grants []store.PlayGrantRecord, removed []string)
	MarkDirty(ids []string)
}

// PersistStore stores one write-behind batch.
type PersistStore interface {
	SavePlaySessions(context.Context, store.PlaySessionChanges) error
}

// PersisterConfig configures a Persister.
type PersisterConfig struct {
	Clock    clock.Clock
	Store    PersistStore
	Sessions *Manager
	// Grants is optional.
	Grants GrantChanges
	// Interval is the shortest time between two writes;
	// DefaultPersistInterval when zero.
	Interval time.Duration
	Logger   *slog.Logger
}

// Persister writes session and grant changes behind them. It owns one
// goroutine; Close stops it and writes the last changes.
type Persister struct {
	config         PersisterConfig
	sessionsNotify <-chan struct{}
	grantsNotify   <-chan struct{}
	cancel         context.CancelFunc
	done           chan struct{}

	flushMu sync.Mutex
	closeMu sync.Mutex
	closed  bool
}

// StartPersister enables change tracking and starts the write-behind loop.
func StartPersister(config PersisterConfig) *Persister {
	if config.Clock == nil || config.Store == nil || config.Sessions == nil {
		panic("session: persister needs a clock, a store and a session manager")
	}
	if config.Interval <= 0 {
		config.Interval = DefaultPersistInterval
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	persister := &Persister{
		config:         config,
		sessionsNotify: config.Sessions.EnablePersistence(),
		cancel:         cancel,
		done:           make(chan struct{}),
	}
	if config.Grants != nil {
		persister.grantsNotify = config.Grants.EnablePersistence()
	}
	go persister.run(ctx)
	return persister
}

func (persister *Persister) run(ctx context.Context) {
	defer close(persister.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-persister.sessionsNotify:
		case <-persister.grantsNotify:
		}
		if err := persister.Flush(ctx); err != nil && ctx.Err() == nil {
			persister.config.Logger.Warn("persist play sessions; retrying", "error", err.Error())
		}
		timer := persister.config.Clock.NewTimer(persister.config.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
	}
}

// Flush writes the pending changes now. A failed write queues them again.
func (persister *Persister) Flush(ctx context.Context) error {
	persister.flushMu.Lock()
	defer persister.flushMu.Unlock()
	records, removed, encodeErr := persister.config.Sessions.TakeChanges()
	changes := store.PlaySessionChanges{Sessions: records, RemovedSessions: removed}
	if persister.config.Grants != nil {
		changes.Grants, changes.RemovedGrants = persister.config.Grants.TakeChanges()
	}
	if changes.Empty() {
		return encodeErr
	}
	if err := persister.config.Store.SavePlaySessions(ctx, changes); err != nil {
		ids := append([]string(nil), removed...)
		for _, record := range records {
			ids = append(ids, record.ID)
		}
		persister.config.Sessions.MarkDirty(ids)
		if persister.config.Grants != nil {
			grants := append([]string(nil), changes.RemovedGrants...)
			for _, grant := range changes.Grants {
				grants = append(grants, grant.PlaySessionID)
			}
			persister.config.Grants.MarkDirty(grants)
		}
		return errors.Join(encodeErr, err)
	}
	return encodeErr
}

// Close stops the write-behind loop and writes the last changes. Call it
// before the transports close: a WebSocket that closes at shutdown ends its
// session, and that removal must not reach the store.
func (persister *Persister) Close(ctx context.Context) error {
	persister.closeMu.Lock()
	defer persister.closeMu.Unlock()
	if persister.closed {
		return nil
	}
	persister.closed = true
	persister.cancel()
	<-persister.done
	return persister.Flush(ctx)
}
