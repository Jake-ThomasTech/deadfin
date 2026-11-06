// SPDX-License-Identifier: GPL-2.0-only

package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
)

// memoryPlaySessions is an in-memory PersistStore.
type memoryPlaySessions struct {
	mu       sync.Mutex
	sessions map[string]store.PlaySessionRecord
	grants   map[string]store.PlayGrantRecord
	writes   int
	fail     error
	saved    chan struct{}
}

func newMemoryPlaySessions() *memoryPlaySessions {
	return &memoryPlaySessions{
		sessions: make(map[string]store.PlaySessionRecord),
		grants:   make(map[string]store.PlayGrantRecord),
		saved:    make(chan struct{}, 16),
	}
}

func (m *memoryPlaySessions) SavePlaySessions(_ context.Context, changes store.PlaySessionChanges) error {
	m.mu.Lock()
	defer func() {
		m.mu.Unlock()
		m.saved <- struct{}{}
	}()
	m.writes++
	if m.fail != nil {
		return m.fail
	}
	for _, id := range changes.RemovedSessions {
		delete(m.sessions, id)
	}
	for _, record := range changes.Sessions {
		m.sessions[record.ID] = record
	}
	for _, id := range changes.RemovedGrants {
		delete(m.grants, id)
	}
	for _, grant := range changes.Grants {
		m.grants[grant.PlaySessionID] = grant
	}
	return nil
}

func (m *memoryPlaySessions) records() []store.PlaySessionRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	records := make([]store.PlaySessionRecord, 0, len(m.sessions))
	for _, record := range m.sessions {
		records = append(records, record)
	}
	return records
}

func (m *memoryPlaySessions) writeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes
}

func (m *memoryPlaySessions) waitSaved(t *testing.T) {
	t.Helper()
	select {
	case <-m.saved:
	case <-time.After(5 * time.Second):
		t.Fatal("the persister wrote nothing")
	}
}

// advanceUntilSaved steps the fake clock by the persist interval until the
// next write: the persister arms its interval timer after a write returns.
func (m *memoryPlaySessions) advanceUntilSaved(t *testing.T, fake *clock.Fake) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		fake.Advance(2 * time.Second)
		select {
		case <-m.saved:
			return
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("the persister wrote nothing")
		}
	}
}

func startTestPersister(t *testing.T, fake *clock.Fake, manager *session.Manager, memory *memoryPlaySessions) *session.Persister {
	t.Helper()
	persister := session.StartPersister(session.PersisterConfig{Clock: fake, Store: memory, Sessions: manager, Interval: 2 * time.Second})
	t.Cleanup(func() { _ = persister.Close(context.Background()) })
	return persister
}

// TestPersistedSessionRestoresInTheNextManager: a playing session with
// capabilities and a queue survives a restart. The restored session keeps
// its ID with a new target generation, and a progress report on the new
// target applies.
func TestPersistedSessionRestoresInTheNextManager(t *testing.T) {
	defer goleak.VerifyNone(t)
	manager, fake := newManager(t)
	memory := newMemoryPlaySessions()
	persister := startTestPersister(t, fake, manager, memory)

	live := logActivity(t, manager, "Swiftfin iOS", "apple-tv", sessionTestUserA)
	profile := []byte(`{"Name":"tvOS"}`)
	if err := manager.ReportCapabilities(t.Context(), session.CapabilityReport{CurrentID: live.ID, Capabilities: session.Capabilities{
		PlayableMediaTypes: []string{"Video"}, SupportsMediaControl: true, DeviceProfile: profile,
	}}); err != nil {
		t.Fatal(err)
	}
	itemID := dto.UUID{0x51}
	position, audio := int64(9_000_000_000), 2
	queue := []dto.QueueItem{{ID: itemID}}
	if err := manager.StartPlayback(t.Context(), live.Target, session.PlaybackState{
		ItemID: itemID, MediaSourceID: "51", PositionTicks: &position, AudioStreamIndex: &audio, IsPaused: true,
		PlaySessionID: "play-1", PlayMethod: "DirectPlay", NowPlayingQueue: &queue,
	}); err != nil {
		t.Fatal(err)
	}
	if err := persister.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	records := memory.records()
	if len(records) != 1 || records[0].ID != live.ID || records[0].UserID != sessionTestUserA {
		t.Fatalf("persisted records = %+v", records)
	}

	next := session.New(fake)
	defer next.Close()
	if restored := next.Restore(records); restored != 1 {
		t.Fatalf("Restore = %d, want 1", restored)
	}
	got, found := next.Session("swiftfin ios", "APPLE-TV")
	if !found {
		t.Fatal("the restored session is missing")
	}
	if got.ID != live.ID || got.UserID != sessionTestUserA || got.DeviceName != "apple-tv" || got.Version != live.Version || !got.LastActivity.Equal(fake.Now()) {
		t.Fatalf("restored identity = %+v", got)
	}
	if string(got.Capabilities.DeviceProfile) != string(profile) || !got.Capabilities.SupportsMediaControl || got.SupportsRemoteControl {
		t.Fatalf("restored capabilities = %+v, remote control %v", got.Capabilities, got.SupportsRemoteControl)
	}
	if got.NowPlaying == nil || got.NowPlaying.ItemID != itemID || *got.NowPlaying.PositionTicks != position || *got.NowPlaying.AudioStreamIndex != 2 ||
		!got.NowPlaying.IsPaused || got.NowPlaying.PlaySessionID != "play-1" || got.LastPausedDate == nil || len(got.NowPlayingQueue) != 1 {
		t.Fatalf("restored playback = %+v", got)
	}
	if profile, found := next.DeviceProfile("Swiftfin iOS", "apple-tv"); !found || len(profile) == 0 {
		t.Fatal("the restored device profile is not served")
	}

	// The request middleware touches the session; the report then applies.
	if outcome := next.RecordAuthenticatedActivity(session.Activity{Client: "Swiftfin iOS", Version: "1.0", DeviceID: "apple-tv", UserID: sessionTestUserA}); outcome != session.AuthenticatedActivityTouched {
		t.Fatalf("activity on the restored session = %v, want touched", outcome)
	}
	later := position + 50_000_000
	if _, accepted, err := next.ProgressPlaybackWithRevision(t.Context(), got.Target, session.PlaybackState{ItemID: itemID, PositionTicks: &later}); err != nil || !accepted {
		t.Fatalf("progress on the restored target = %v, %v", accepted, err)
	}
	if after, _ := next.SessionByID(live.ID); after.NowPlaying == nil || *after.NowPlaying.PositionTicks != later {
		t.Fatalf("progress after restore = %+v", after.NowPlaying)
	}
}

// TestRestoreSkipsReapedSessions: a session idle past the ReloadWindow, a
// record that does not decode, and a key that is already live are not
// restored.
func TestRestoreSkipsReapedSessions(t *testing.T) {
	manager, fake := newManager(t)
	memory := newMemoryPlaySessions()
	persister := startTestPersister(t, fake, manager, memory)
	logActivity(t, manager, "Jellyfin Web", "stale", sessionTestUserA)
	fake.Advance(session.ReloadWindow)
	fresh := logActivity(t, manager, "Jellyfin Web", "fresh", sessionTestUserA)
	if err := persister.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	records := append(memory.records(), store.PlaySessionRecord{ID: "broken", UserID: sessionTestUserA, LastActivity: fake.Now(), State: []byte("{")})
	if len(records) != 3 {
		t.Fatalf("persisted %d records, want 3", len(records))
	}
	fake.Advance(time.Second)

	next := session.New(fake)
	defer next.Close()
	if restored := next.Restore(records); restored != 1 {
		t.Fatalf("Restore = %d, want only the fresh session", restored)
	}
	if _, found := next.SessionByID(fresh.ID); !found {
		t.Fatal("the fresh session is missing")
	}
	if _, found := next.Session("Jellyfin Web", "stale"); found {
		t.Fatal("a session idle past the ReloadWindow was restored")
	}
	if restored := next.Restore(records); restored != 0 {
		t.Fatalf("a second Restore added %d sessions over live ones", restored)
	}
}

// TestPersisterWritesBehindAtABoundedRate: the first change writes at once,
// later changes wait for the interval, a removal deletes the record, and a
// failed write is retried.
func TestPersisterWritesBehindAtABoundedRate(t *testing.T) {
	manager, fake := newManager(t)
	memory := newMemoryPlaySessions()
	persister := startTestPersister(t, fake, manager, memory)

	first := logActivity(t, manager, "Jellyfin Web", "one", sessionTestUserA)
	memory.waitSaved(t)
	logActivity(t, manager, "Jellyfin Web", "two", sessionTestUserA)
	logActivity(t, manager, "Jellyfin Web", "three", sessionTestUserA)
	select {
	case <-memory.saved:
		t.Fatal("a second write came before the interval")
	case <-time.After(50 * time.Millisecond):
	}
	memory.advanceUntilSaved(t, fake)
	if writes, records := memory.writeCount(), len(memory.records()); writes != 2 || records != 3 {
		t.Fatalf("writes = %d, records = %d; want 2 and 3", writes, records)
	}

	memory.mu.Lock()
	memory.fail = errors.New("disk full")
	memory.mu.Unlock()
	manager.Remove(first.ID)
	memory.advanceUntilSaved(t, fake)
	memory.mu.Lock()
	memory.fail = nil
	memory.mu.Unlock()
	memory.advanceUntilSaved(t, fake)
	if records := memory.records(); len(records) != 2 {
		t.Fatalf("records after a retried removal = %d, want 2", len(records))
	}
	if err := persister.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	goleak.VerifyNone(t)
}

// TestPersisterCloseKeepsLaterRemovals: after Close, a session that ends as
// its WebSocket closes at shutdown stays in the store.
func TestPersisterCloseKeepsLaterRemovals(t *testing.T) {
	manager, fake := newManager(t)
	memory := newMemoryPlaySessions()
	persister := startTestPersister(t, fake, manager, memory)
	live := logActivity(t, manager, "Jellyfin Web", "browser", sessionTestUserA)
	if err := persister.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager.ReportSessionEnded(live.ID)
	if err := persister.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if records := memory.records(); len(records) != 1 || records[0].ID != live.ID {
		t.Fatalf("records = %+v, want the session kept", records)
	}
}
