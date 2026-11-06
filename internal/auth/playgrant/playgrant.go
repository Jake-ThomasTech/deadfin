// SPDX-License-Identifier: GPL-2.0-only

// Package playgrant holds the item-bound playback credentials of divergence
// D-54. An authenticated PlaybackInfo response issues a PlaySessionId; the
// grant lets a later direct-stream request that carries only that
// PlaySessionId play the same item as the same user. Stock Swiftfin and
// SharkParty build such URLs for AVPlayer and VLCKit, which cannot add an
// authorization header.
//
// The store is in memory, mutex-protected, and bounded. A clock.Clock drives
// every expiry comparison; there is no background goroutine. With
// persistence on, the store tracks each changed grant, a session.Persister
// writes it behind the change, and the next server process restores the
// grants at startup (#620).
package playgrant

import (
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
)

const (
	// IdleTimeout ends a grant that no stream request or playback report has
	// used for this long. A paused player sends progress reports, so a long
	// pause keeps its grant.
	IdleTimeout = 4 * time.Hour
	// MaxGrants bounds the stored grants. Issuing past the bound evicts the
	// least recently used grant.
	MaxGrants = 4096
	// MaxGrantsPerUser bounds one user's grants, so one account cannot evict
	// the grants of the others. Issuing past it evicts that user's least
	// recently used grant.
	MaxGrantsPerUser = 64
)

// Grant is the user and item that one PlaySessionId may stream.
type Grant struct {
	UserID         dto.UUID
	ItemID         dto.UUID
	MediaSourceIDs []string
	lastUsed       time.Time
}

// Store is the bounded in-memory grant table. It is safe for concurrent use.
type Store struct {
	clock clock.Clock

	mu     sync.Mutex
	grants map[string]Grant // keyed by PlaySessionId
	// dirty holds the PlaySessionIds changed or removed since the last
	// TakeChanges, while persistence is on.
	dirty  map[string]struct{}
	notify chan struct{}
}

// New constructs an empty grant store.
func New(c clock.Clock) *Store {
	return &Store{clock: c, grants: make(map[string]Grant)}
}

// Issue records that playSessionID may stream itemID, limited to
// mediaSourceIDs, as userID. A nil user or an empty PlaySessionId issues
// nothing. Issuing an existing PlaySessionId replaces its grant.
func (store *Store) Issue(playSessionID string, userID, itemID dto.UUID, mediaSourceIDs []string) {
	if store == nil || playSessionID == "" || userID.IsNil() || itemID.IsNil() {
		return
	}
	now := store.clock.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.sweep(now)
	delete(store.grants, playSessionID)
	store.markDirty(playSessionID)
	if store.countUser(userID) >= MaxGrantsPerUser {
		store.evictOldest(func(grant Grant) bool { return grant.UserID == userID })
	}
	if len(store.grants) >= MaxGrants {
		store.evictOldest(func(Grant) bool { return true })
	}
	store.grants[playSessionID] = Grant{
		UserID:         userID,
		ItemID:         itemID,
		MediaSourceIDs: append([]string(nil), mediaSourceIDs...),
		lastUsed:       now,
	}
}

// Authorize returns the user of the live grant for playSessionID when it
// covers itemID and mediaSourceID. An empty mediaSourceID selects the item's
// default source and matches any grant for the item. A match refreshes the
// grant's idle timer.
func (store *Store) Authorize(playSessionID string, itemID dto.UUID, mediaSourceID string) (dto.UUID, bool) {
	if store == nil || playSessionID == "" {
		return dto.NilUUID, false
	}
	now := store.clock.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	grant, ok := store.live(playSessionID, now)
	if !ok || grant.ItemID != itemID || !grant.coversSource(mediaSourceID) {
		return dto.NilUUID, false
	}
	grant.lastUsed = now
	store.grants[playSessionID] = grant
	store.markDirty(playSessionID)
	return grant.UserID, true
}

// Touch refreshes the idle timer of userID's live grant for playSessionID.
// Playback reports call it, so a paused player keeps its grant.
func (store *Store) Touch(playSessionID string, userID dto.UUID) {
	if store == nil || playSessionID == "" {
		return
	}
	now := store.clock.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	if grant, ok := store.live(playSessionID, now); ok && grant.UserID == userID {
		grant.lastUsed = now
		store.grants[playSessionID] = grant
		store.markDirty(playSessionID)
	}
}

// Has reports whether playSessionID has a live grant here (#652).
func (store *Store) Has(playSessionID string) bool {
	if store == nil || playSessionID == "" {
		return false
	}
	now := store.clock.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	_, ok := store.live(playSessionID, now)
	return ok
}

// Len reports the stored grant count, expired ones included until a sweep.
func (store *Store) Len() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.grants)
}

func (store *Store) live(playSessionID string, now time.Time) (Grant, bool) {
	grant, ok := store.grants[playSessionID]
	if !ok {
		return Grant{}, false
	}
	if expired(grant, now) {
		delete(store.grants, playSessionID)
		store.markDirty(playSessionID)
		return Grant{}, false
	}
	return grant, true
}

func (store *Store) sweep(now time.Time) {
	for key, grant := range store.grants {
		if expired(grant, now) {
			delete(store.grants, key)
			store.markDirty(key)
		}
	}
}

func (store *Store) countUser(userID dto.UUID) int {
	count := 0
	for _, grant := range store.grants {
		if grant.UserID == userID {
			count++
		}
	}
	return count
}

func (store *Store) evictOldest(match func(Grant) bool) {
	oldestKey, found := "", false
	var oldest time.Time
	for key, grant := range store.grants {
		if !match(grant) {
			continue
		}
		if !found || grant.lastUsed.Before(oldest) || (grant.lastUsed.Equal(oldest) && key < oldestKey) {
			oldestKey, oldest, found = key, grant.lastUsed, true
		}
	}
	if found {
		delete(store.grants, oldestKey)
		store.markDirty(oldestKey)
	}
}

// EnablePersistence starts change tracking and returns the channel that
// signals a change. The channel holds at most one signal.
func (store *Store) EnablePersistence() <-chan struct{} {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.notify == nil {
		store.notify = make(chan struct{}, 1)
		store.dirty = make(map[string]struct{})
	}
	return store.notify
}

func (store *Store) markDirty(playSessionID string) {
	if store.dirty == nil {
		return
	}
	store.dirty[playSessionID] = struct{}{}
	select {
	case store.notify <- struct{}{}:
	default:
	}
}

// MarkDirty queues the grants of playSessionIDs for the next TakeChanges
// again, as after a failed write.
func (store *Store) MarkDirty(playSessionIDs []string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, id := range playSessionIDs {
		store.markDirty(id)
	}
}

// TakeChanges returns each grant changed since the last call, and the
// PlaySessionIds of the removed ones.
func (store *Store) TakeChanges() (grants []store.PlayGrantRecord, removed []string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for id := range store.dirty {
		delete(store.dirty, id)
		grant, found := store.grants[id]
		if !found {
			removed = append(removed, id)
			continue
		}
		grants = append(grants, grant.record(id))
	}
	return grants, removed
}

// Restore adds the persisted grants of a previous server process. It skips
// an expired grant and keeps the bounds: past them it evicts as Issue does.
func (store *Store) Restore(records []store.PlayGrantRecord) {
	now := store.clock.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, record := range records {
		grant := Grant{
			UserID:         record.UserID,
			ItemID:         record.ItemID,
			MediaSourceIDs: append([]string(nil), record.MediaSourceIDs...),
			lastUsed:       record.LastUsed,
		}
		if record.PlaySessionID == "" || grant.UserID.IsNil() || grant.ItemID.IsNil() || expired(grant, now) {
			continue
		}
		if _, found := store.grants[record.PlaySessionID]; found {
			continue
		}
		if store.countUser(grant.UserID) >= MaxGrantsPerUser {
			store.evictOldest(func(other Grant) bool { return other.UserID == grant.UserID })
		}
		if len(store.grants) >= MaxGrants {
			store.evictOldest(func(Grant) bool { return true })
		}
		store.grants[record.PlaySessionID] = grant
	}
}

func (grant Grant) record(playSessionID string) store.PlayGrantRecord {
	return store.PlayGrantRecord{
		PlaySessionID:  playSessionID,
		UserID:         grant.UserID,
		ItemID:         grant.ItemID,
		MediaSourceIDs: append([]string(nil), grant.MediaSourceIDs...),
		LastUsed:       grant.lastUsed,
	}
}

func expired(grant Grant, now time.Time) bool {
	return now.Sub(grant.lastUsed) > IdleTimeout
}

func (grant Grant) coversSource(mediaSourceID string) bool {
	if mediaSourceID == "" {
		return true
	}
	for _, id := range grant.MediaSourceIDs {
		if sameSourceID(id, mediaSourceID) {
			return true
		}
	}
	return false
}

// sameSourceID compares media source IDs as GUIDs when both parse, so a
// client that sends the dashed form matches the N-format the server issued.
func sameSourceID(issued, requested string) bool {
	if issued == requested {
		return true
	}
	a, errA := dto.ParseUUID(issued)
	b, errB := dto.ParseUUID(requested)
	return errA == nil && errB == nil && a == b
}
