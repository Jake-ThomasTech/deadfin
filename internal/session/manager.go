// SPDX-License-Identifier: GPL-2.0-only

package session

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/events"
)

var (
	ErrNotFound = errors.New("session not found")
	ErrClosed   = errors.New("session manager is closed")
	ErrLimit    = errors.New("maximum active sessions reached")
)

// Activity identifies a client/device connection and its current metadata.
type Activity struct {
	Client            string
	Version           string
	DeviceID          string
	DeviceName        string
	UserID            dto.UUID
	AdditionalUserIDs []dto.UUID
	RemoteEndPoint    string
}

// AuthenticatedActivityOutcome describes the result of recording request activity.
type AuthenticatedActivityOutcome uint8

const (
	AuthenticatedActivityRejected AuthenticatedActivityOutcome = iota
	AuthenticatedActivityCreated
	AuthenticatedActivityTouched
)

// Capabilities are the playback controls and device profile reported by a
// session client. DeviceProfile remains opaque at this seam; the playback
// application adapter decodes it once into its typed model.
type Capabilities struct {
	PlayableMediaTypes           []string
	SupportedCommands            []string
	SupportsMediaControl         bool
	SupportsPersistentIdentifier bool
	DeviceProfile                []byte
}

// PlaybackState is the current client-reported state for an active item.
// It is stored only on the authenticated client/device session; PlaySessionID
// is opaque correlation data and never participates in session lookup.
type PlaybackState struct {
	CanSeek                bool
	ItemID                 dto.UUID
	MediaSourceID          string
	AudioStreamIndex       *int
	SubtitleStreamIndex    *int
	IsPaused               bool
	IsMuted                bool
	PositionTicks          *int64
	PlaybackStartTimeTicks *int64
	VolumeLevel            *int
	Brightness             *int
	AspectRatio            string
	PlayMethod             string
	LiveStreamID           string
	PlaySessionID          string
	RepeatMode             string
	PlaybackOrder          string
	PlaylistItemID         string
	NowPlayingQueue        *[]dto.QueueItem
	RunTimeTicks           *int64
}

// Target identifies one current incarnation of an active session. ID remains
// stable across authentication refreshes while Generation rotates, preventing
// in-flight playback reports from being applied to a reused identity.
type Target struct {
	ID         string
	Generation uint64
}

// Snapshot is an immutable view of a current in-memory session.
type Snapshot struct {
	ID                    string
	Target                Target
	Client                string
	DeviceID              string
	DeviceName            string
	Version               string
	UserID                dto.UUID
	AdditionalUserIDs     []dto.UUID
	RemoteEndPoint        string
	LastActivity          time.Time
	SupportsRemoteControl bool
	Capabilities          Capabilities
	NowPlaying            *PlaybackState
	LastPlaybackCheckIn   time.Time
	LastPausedDate        *time.Time
	NowPlayingQueue       []dto.QueueItem
	PlaylistItemID        string
	reportRevision        uint64
}

// ControlEvaluator applies caller-owned authorization facts to a copied session
// snapshot. The manager never resolves users or devices itself.
type ControlEvaluator func(Snapshot, dto.UUID) bool

// Query narrows the set of current session snapshots.
type Query struct {
	DeviceID            string
	ActiveWithinSeconds *int
	ControllableUserID  *dto.UUID
	CanControl          ControlEvaluator
}

// CapabilityReport targets either its explicit ID or the caller's current ID.
// A non-nil Owner restricts the target to sessions of that user; a session of
// another user reports ErrNotFound (divergence D-67).
type CapabilityReport struct {
	ID           *string
	CurrentID    string
	Owner        *dto.UUID
	Capabilities Capabilities
}

// ChangeKind identifies a committed registry transition relevant to live
// session projections.
type ChangeKind string

const (
	ChangeCreated      ChangeKind = "Created"
	ChangeActivity     ChangeKind = "Activity"
	ChangePlayback     ChangeKind = "Playback"
	ChangeCapabilities ChangeKind = "Capabilities"
	ChangeRemoved      ChangeKind = "Removed"
)

// Change is published only after its corresponding in-memory mutation.
// Force bypasses a WebSocket subscription's client-supplied periodic floor.
type Change struct {
	SessionID string
	Kind      ChangeKind
	Force     bool
}

// Options supplies dynamic watchdog policy and application callbacks. Hooks
// run without the registry lock held. OnSessionStarted and OnSessionEnded
// receive a copy of each session the registry creates or removes
// (SessionManager.OnSessionStarted and OnSessionEnded), after the change
// commits, on the goroutine that made the change.
type Options struct {
	InactiveSessionThreshold func() int
	OnIdlePlayback           func(context.Context, Snapshot, PlaybackState) error
	OnInactivePlayback       func(context.Context, Snapshot, PlaybackState) error
	// CloseLiveStream releases one consumer of an open live stream
	// (MediaSourceManager.CloseLiveStream). Nil keeps live streams open
	// until their own idle timeout.
	CloseLiveStream  func(ctx context.Context, liveStreamID string) error
	OnSessionStarted func(context.Context, Snapshot)
	OnSessionEnded   func(context.Context, Snapshot)
}

// lifecycleEvent is a session creation or removal that waits for the
// registry lock to be released before its hook runs.
type lifecycleEvent struct {
	started  bool
	snapshot Snapshot
}

// sessionKey preserves the boundary between independently case-folded client
// and device identity fields.
type sessionKey struct {
	client   string
	deviceID string
}

// PlaybackReportCommitLease validates a single accepted playback report at
// durable-commit time. A successful first Acquire retains the manager read
// lock until Close, preventing a newer report or session transition from
// invalidating the report between validation and its remaining commits.
//
// A lease is owned by one report and must be closed by that report's caller.
type PlaybackReportCommitLease struct {
	manager  *Manager
	target   Target
	revision uint64

	mu      sync.Mutex
	checked bool
	held    bool
	closed  bool
}

// Manager owns the in-memory active-session registry.
type Manager struct {
	clock   clock.Clock
	options Options
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu             sync.RWMutex
	closed         bool
	nextGeneration uint64
	byKey          map[sessionKey]*Snapshot
	byID           map[string]sessionKey
	changes        *events.Bus[Change]
	controllers    map[string]int
	progress       map[string]*progressWorker
	// dirty holds the IDs of sessions changed or removed since the last
	// TakeChanges, while persistence is on (#620).
	dirty         map[string]struct{}
	persistNotify chan struct{}
	// liveStreams maps a live stream id to the sessions that play it:
	// session id to play session id and play session id to session id
	// (SessionManager._activeLiveStreamSessions).
	liveStreams map[string]map[string]string
	// switched holds, per live stream, the session and play session ids
	// whose consumer a channel switch released (D-92). A later stop report
	// for the pair releases nothing, and start and progress reports do not
	// track the stream again for SwitchReleaseHold.
	switched  map[string]map[string]switchMark
	lifecycle []lifecycleEvent
}

type progressWorker struct {
	target Target
	cancel context.CancelFunc
}

// New constructs an in-memory session registry with disabled optional hooks.
func New(c clock.Clock) *Manager {
	return NewWithOptions(c, Options{})
}

// NewWithOptions constructs an in-memory session registry. The manager owns
// every timer and worker it creates and stops them from Close.
func NewWithOptions(c clock.Clock, options Options) *Manager {
	if c == nil {
		panic("session: clock is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		clock:       c,
		options:     options,
		ctx:         ctx,
		cancel:      cancel,
		byKey:       make(map[sessionKey]*Snapshot),
		byID:        make(map[string]sessionKey),
		changes:     events.NewBus[Change](c),
		controllers: make(map[string]int),
		progress:    make(map[string]*progressWorker),
		liveStreams: make(map[string]map[string]string),
		switched:    make(map[string]map[string]switchMark),
	}
	idleTimer := c.NewTimer(5 * time.Minute)
	inactiveTimer := c.NewTimer(time.Minute)
	manager.wg.Add(1)
	go manager.runWatchdogs(idleTimer, inactiveTimer)
	return manager
}

// SetOptions replaces dynamic watchdog policy after application composition.
func (manager *Manager) SetOptions(options Options) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return ErrClosed
	}
	manager.options = options
	return nil
}

// LogActivity creates or refreshes the session keyed by Client+DeviceID.
func (manager *Manager) LogActivity(_ context.Context, activity Activity) (Snapshot, error) {
	if activity.Client == "" {
		return Snapshot{}, errors.New("session: client is required")
	}
	if activity.Version == "" {
		return Snapshot{}, errors.New("session: version is required")
	}
	if activity.DeviceID == "" {
		return Snapshot{}, errors.New("session: device ID is required")
	}

	key := newSessionKey(activity.Client, activity.DeviceID)
	now := manager.clock.Now()

	manager.mu.Lock()
	defer manager.unlockAndNotify()
	if manager.closed {
		return Snapshot{}, ErrClosed
	}

	session, found := manager.byKey[key]
	if !found {
		session = manager.createSessionLocked(key, activity, now)
		manager.publishChangeLocked(session.ID, ChangeCreated, true)
		return cloneSnapshot(*session), nil
	}

	if session.UserID != activity.UserID {
		manager.stopProgressLocked(session.ID)
		session.NowPlaying = nil
		session.LastPlaybackCheckIn = time.Time{}
		session.LastPausedDate = nil
		session.NowPlayingQueue = nil
		session.PlaylistItemID = ""
	}
	session.Target.Generation = manager.nextTargetGenerationLocked()
	session.Client = activity.Client
	session.Version = activity.Version
	session.UserID = activity.UserID
	session.AdditionalUserIDs = append([]dto.UUID(nil), activity.AdditionalUserIDs...)
	session.LastActivity = now
	if activity.DeviceName != "" {
		session.DeviceName = activity.DeviceName
	}
	if activity.RemoteEndPoint != "" {
		session.RemoteEndPoint = activity.RemoteEndPoint
	}
	manager.publishChangeLocked(session.ID, ChangeActivity, false)
	return cloneSnapshot(*session), nil
}

// Session resolves an exact case-insensitive Client+DeviceID session key.
func (manager *Manager) Session(client, deviceID string) (Snapshot, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.closed {
		return Snapshot{}, false
	}
	snapshot, found := manager.byKey[newSessionKey(client, deviceID)]
	if !found {
		return Snapshot{}, false
	}
	return cloneSnapshot(*snapshot), true
}

// SessionByID resolves an exact current session identifier.
func (manager *Manager) SessionByID(id string) (Snapshot, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.closed {
		return Snapshot{}, false
	}
	key, found := manager.byID[id]
	if !found {
		return Snapshot{}, false
	}
	snapshot, found := manager.byKey[key]
	if !found {
		return Snapshot{}, false
	}
	return cloneSnapshot(*snapshot), true
}

// RecordAuthenticatedActivity atomically records an authenticated request.
// It creates an absent client/device session, refreshes only the owner's
// session, and leaves a displaced user's session unchanged.
func (manager *Manager) RecordAuthenticatedActivity(activity Activity) AuthenticatedActivityOutcome {
	if activity.Client == "" || activity.DeviceID == "" {
		return AuthenticatedActivityRejected
	}

	key := newSessionKey(activity.Client, activity.DeviceID)
	now := manager.clock.Now()

	manager.mu.Lock()
	defer manager.unlockAndNotify()
	if manager.closed {
		return AuthenticatedActivityRejected
	}

	snapshot, found := manager.byKey[key]
	if !found {
		snapshot = manager.createSessionLocked(key, activity, now)
		manager.publishChangeLocked(snapshot.ID, ChangeCreated, true)
		return AuthenticatedActivityCreated
	}
	if snapshot.UserID != activity.UserID {
		return AuthenticatedActivityRejected
	}

	manager.touchSessionLocked(snapshot, activity, now)
	manager.publishChangeLocked(snapshot.ID, ChangeActivity, false)
	return AuthenticatedActivityTouched
}

func (manager *Manager) createSessionLocked(key sessionKey, activity Activity, now time.Time) *Snapshot {
	deviceName := activity.DeviceName
	if deviceName == "" {
		deviceName = "Network Device"
	}
	id := manager.newSessionIDLocked(key, activity.Client+activity.DeviceID)
	snapshot := &Snapshot{
		ID:                id,
		Target:            Target{ID: id, Generation: manager.nextTargetGenerationLocked()},
		Client:            activity.Client,
		DeviceID:          activity.DeviceID,
		DeviceName:        deviceName,
		Version:           activity.Version,
		UserID:            activity.UserID,
		AdditionalUserIDs: append([]dto.UUID(nil), activity.AdditionalUserIDs...),
		RemoteEndPoint:    activity.RemoteEndPoint,
		LastActivity:      now,
		Capabilities: Capabilities{
			SupportsPersistentIdentifier: true,
		},
	}
	manager.byKey[key] = snapshot
	manager.byID[snapshot.ID] = key
	manager.lifecycle = append(manager.lifecycle, lifecycleEvent{started: true, snapshot: cloneSnapshot(*snapshot)})
	return snapshot
}

func (manager *Manager) touchSessionLocked(snapshot *Snapshot, activity Activity, now time.Time) {
	snapshot.Client = activity.Client
	snapshot.Version = activity.Version
	if activity.DeviceName != "" {
		snapshot.DeviceName = activity.DeviceName
	}
	snapshot.LastActivity = now
	if activity.RemoteEndPoint != "" {
		snapshot.RemoteEndPoint = activity.RemoteEndPoint
	}
}

// StartPlayback records the current item for an exact current target. The
// request context is accepted at this seam, but no external I/O is performed
// while mutating the in-memory registry.
func (manager *Manager) StartPlayback(ctx context.Context, target Target, state PlaybackState) error {
	_, err := manager.StartPlaybackWithRevision(ctx, target, state)
	return err
}

// StartPlaybackWithRevision records current playback and returns the revision
// that identifies this accepted report until a later report or target replaces it.
func (manager *Manager) StartPlaybackWithRevision(_ context.Context, target Target, state PlaybackState) (uint64, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return 0, ErrClosed
	}
	snapshot, found := manager.snapshotForTargetLocked(target)
	if !found {
		return 0, ErrNotFound
	}
	manager.updatePlayback(snapshot, state, manager.clock.Now())
	manager.restartProgressLocked(snapshot)
	manager.trackLiveStreamLocked(state.LiveStreamID, snapshot.ID, state.PlaySessionID)
	manager.publishChangeLocked(snapshot.ID, ChangePlayback, true)
	return manager.nextPlaybackReportRevisionLocked(snapshot), nil
}

// ProgressPlayback records a progress check-in for the authenticated target.
// A client may report progress after a session is replaced or removed, so a
// missing target is intentionally a no-op.
func (manager *Manager) ProgressPlayback(ctx context.Context, target Target, state PlaybackState) error {
	_, _, err := manager.ProgressPlaybackWithRevision(ctx, target, state)
	return err
}

// ProgressPlaybackWithRevision records progress and reports whether the target
// was accepted. A missing target is a successful no-op with no revision.
func (manager *Manager) ProgressPlaybackWithRevision(_ context.Context, target Target, state PlaybackState) (uint64, bool, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return 0, false, ErrClosed
	}
	snapshot, found := manager.snapshotForTargetLocked(target)
	if !found {
		return 0, false, nil
	}
	manager.updatePlayback(snapshot, state, manager.clock.Now())
	manager.restartProgressLocked(snapshot)
	manager.trackLiveStreamLocked(state.LiveStreamID, snapshot.ID, state.PlaySessionID)
	manager.publishChangeLocked(snapshot.ID, ChangePlayback, false)
	return manager.nextPlaybackReportRevisionLocked(snapshot), true, nil
}

// StopPlayback clears the active item while retaining terminal playback timing
// and any queue or playlist data reported by the authenticated target.
func (manager *Manager) StopPlayback(ctx context.Context, target Target, state PlaybackState) error {
	_, err := manager.StopPlaybackWithRevision(ctx, target, state)
	return err
}

// StopPlaybackWithRevision records terminal playback and returns the revision
// that identifies this accepted report until a later report or target replaces it.
func (manager *Manager) StopPlaybackWithRevision(ctx context.Context, target Target, state PlaybackState) (uint64, error) {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return 0, ErrClosed
	}
	snapshot, found := manager.snapshotForTargetLocked(target)
	if !found {
		manager.mu.Unlock()
		return 0, ErrNotFound
	}
	manager.updatePlaybackProjection(snapshot, state)
	manager.stopProgressLocked(snapshot.ID)
	snapshot.NowPlaying = nil
	manager.publishChangeLocked(snapshot.ID, ChangePlayback, true)
	revision := manager.nextPlaybackReportRevisionLocked(snapshot)
	var closing []string
	if state.LiveStreamID != "" && manager.untrackLiveStreamLocked(state.LiveStreamID, snapshot.ID) {
		closing = append(closing, state.LiveStreamID)
	}
	closeLiveStream := manager.options.CloseLiveStream
	manager.mu.Unlock()
	closeLiveStreams(ctx, closeLiveStream, closing)
	return revision, nil
}

func (manager *Manager) updatePlayback(snapshot *Snapshot, state PlaybackState, now time.Time) {
	wasPaused := snapshot.NowPlaying != nil && snapshot.NowPlaying.IsPaused
	manager.updatePlaybackProjection(snapshot, state)
	snapshot.NowPlaying = clonePlaybackState(state)
	snapshot.LastPlaybackCheckIn = now
	snapshot.LastActivity = now

	if state.IsPaused && !wasPaused {
		pausedAt := now
		snapshot.LastPausedDate = &pausedAt
	} else if !state.IsPaused {
		snapshot.LastPausedDate = nil
	}
}

func (manager *Manager) updatePlaybackProjection(snapshot *Snapshot, state PlaybackState) {
	if state.NowPlayingQueue != nil {
		snapshot.NowPlayingQueue = cloneQueue(*state.NowPlayingQueue)
	}
	snapshot.PlaylistItemID = state.PlaylistItemID
}

// SetPlaybackRuntime enriches the current client report with the canonical
// item duration used to stop automatic progress at the end of media.
func (manager *Manager) SetPlaybackRuntime(target Target, runTimeTicks *int64) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return false
	}
	snapshot, found := manager.snapshotForTargetLocked(target)
	if !found || snapshot.NowPlaying == nil {
		return false
	}
	if runTimeTicks == nil {
		snapshot.NowPlaying.RunTimeTicks = nil
	} else {
		value := *runTimeTicks
		snapshot.NowPlaying.RunTimeTicks = &value
	}
	manager.markDirtyLocked(snapshot.ID)
	return true
}

// CheckLimit admits a candidate client/device before token issuance. A refresh
// of the existing client/device session does not consume another active slot.
func (manager *Manager) CheckLimit(userID dto.UUID, client, deviceID string, maximum int) error {
	if maximum < 1 {
		return nil
	}
	registryKey := newSessionKey(client, deviceID)
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.closed {
		return ErrClosed
	}
	active := 0
	for key, snapshot := range manager.byKey {
		if snapshot.UserID != userID {
			continue
		}
		if key == registryKey {
			return nil
		}
		active++
	}
	if active >= maximum {
		return ErrLimit
	}
	return nil
}

// Sessions returns immutable snapshots matching query, newest activity first.
func (manager *Manager) Sessions(query Query) []Snapshot {
	manager.mu.RLock()
	if manager.closed {
		manager.mu.RUnlock()
		return nil
	}
	if query.ControllableUserID != nil && query.CanControl == nil {
		manager.mu.RUnlock()
		return nil
	}
	now := manager.clock.Now()
	result := make([]Snapshot, 0, len(manager.byKey))
	for _, session := range manager.byKey {
		if query.DeviceID != "" && !strings.EqualFold(session.DeviceID, query.DeviceID) {
			continue
		}
		if query.ActiveWithinSeconds != nil && *query.ActiveWithinSeconds > 0 && session.LastActivity.Before(now.Add(-time.Duration(*query.ActiveWithinSeconds)*time.Second)) {
			continue
		}
		result = append(result, cloneSnapshot(*session))
	}
	manager.mu.RUnlock()

	if query.ControllableUserID != nil {
		filtered := result[:0]
		for _, snapshot := range result {
			if query.CanControl(cloneSnapshot(snapshot), *query.ControllableUserID) {
				filtered = append(filtered, snapshot)
			}
		}
		result = filtered
	}

	sort.Slice(result, func(left, right int) bool {
		if result[left].LastActivity.Equal(result[right].LastActivity) {
			return result[left].ID < result[right].ID
		}
		return result[left].LastActivity.After(result[right].LastActivity)
	})
	return result
}

// ReportCapabilities replaces a session's client capabilities from a defensive
// copy. When ID is absent, CurrentID selects the target session.
func (manager *Manager) ReportCapabilities(_ context.Context, report CapabilityReport) error {
	id := report.CurrentID
	if report.ID != nil {
		id = *report.ID
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return ErrClosed
	}
	key, found := manager.byID[id]
	if !found {
		return ErrNotFound
	}
	session, found := manager.byKey[key]
	if !found || (report.Owner != nil && session.UserID != *report.Owner) {
		return ErrNotFound
	}
	session.Capabilities = cloneCapabilities(report.Capabilities)
	session.SupportsRemoteControl = manager.controllers[session.ID] > 0 && session.Capabilities.SupportsMediaControl
	manager.publishChangeLocked(session.ID, ChangeCapabilities, true)
	return nil
}

// ControllerConnected records a live command transport for one session.
func (manager *Manager) ControllerConnected(id string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return ErrClosed
	}
	key, found := manager.byID[id]
	if !found {
		return ErrNotFound
	}
	snapshot, found := manager.byKey[key]
	if !found {
		return ErrNotFound
	}
	manager.controllers[id]++
	snapshot.SupportsRemoteControl = snapshot.Capabilities.SupportsMediaControl
	manager.publishChangeLocked(id, ChangeCapabilities, true)
	return nil
}

// ControllerDisconnected removes one live command transport and reports
// whether it was the session's final controller.
func (manager *Manager) ControllerDisconnected(id string) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return false
	}
	key, found := manager.byID[id]
	if !found {
		return false
	}
	snapshot, found := manager.byKey[key]
	if !found {
		return false
	}
	if manager.controllers[id] > 1 {
		manager.controllers[id]--
	} else {
		delete(manager.controllers, id)
		snapshot.SupportsRemoteControl = false
	}
	manager.publishChangeLocked(id, ChangeCapabilities, true)
	return manager.controllers[id] == 0
}

// Subscribe registers for committed live-session transitions.
func (manager *Manager) Subscribe(options events.SubscriptionOptions) *events.Subscription[Change] {
	return manager.changes.Subscribe(options)
}

// DeviceProfile returns a defensive copy of the profile for the exact active
// client/device session.
func (manager *Manager) DeviceProfile(client, deviceID string) ([]byte, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.closed {
		return nil, false
	}
	snapshot, found := manager.byKey[newSessionKey(client, deviceID)]
	if !found || len(snapshot.Capabilities.DeviceProfile) == 0 {
		return nil, false
	}
	return append([]byte(nil), snapshot.Capabilities.DeviceProfile...), true
}

// Remove drops the session identified by id and closes the live streams
// it still plays (SessionManager.CloseIfNeededAsync).
func (manager *Manager) Remove(id string) bool {
	manager.mu.Lock()
	if manager.closed {
		manager.unlockAndNotify()
		return false
	}
	key, found := manager.byID[id]
	if !found {
		manager.unlockAndNotify()
		return false
	}
	closing := manager.removeLocked(key, id)
	closeLiveStream := manager.options.CloseLiveStream
	manager.unlockAndNotify()
	closeLiveStreams(manager.ctx, closeLiveStream, closing)
	return true
}

// removeLocked drops one session and returns the live streams that no
// other session plays. It must run under a lock that unlockAndNotify
// releases.
func (manager *Manager) removeLocked(key sessionKey, id string) []string {
	if snapshot, found := manager.byKey[key]; found {
		manager.lifecycle = append(manager.lifecycle, lifecycleEvent{snapshot: cloneSnapshot(*snapshot)})
	}
	var closing []string
	for liveStreamID, sessions := range manager.liveStreams {
		if _, plays := sessions[id]; plays && manager.untrackLiveStreamLocked(liveStreamID, id) {
			closing = append(closing, liveStreamID)
		}
	}
	sort.Strings(closing)
	for liveStreamID := range manager.switched {
		manager.clearSwitchedLocked(liveStreamID, id)
	}
	delete(manager.byID, id)
	delete(manager.byKey, key)
	delete(manager.controllers, id)
	manager.stopProgressLocked(id)
	manager.publishChangeLocked(id, ChangeRemoved, true)
	return closing
}

// CloseLiveStreamIfNeeded releases one consumer of a live stream for a
// session or play session that stops playing it
// (SessionManager.CloseLiveStreamIfNeededAsync).
func (manager *Manager) CloseLiveStreamIfNeeded(ctx context.Context, liveStreamID, sessionOrPlaySessionID string) {
	if liveStreamID == "" {
		return
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return
	}
	release := manager.untrackLiveStreamLocked(liveStreamID, sessionOrPlaySessionID)
	closeLiveStream := manager.options.CloseLiveStream
	manager.mu.Unlock()
	if release {
		closeLiveStreams(ctx, closeLiveStream, []string{liveStreamID})
	}
}

// SwitchReleaseHold is how long after a channel switch released a stream
// the device's start and progress reports of that stream are ignored, so a
// report that was in flight across the switch does not track the stream
// again (D-92). It matches the broker's idle close.
const SwitchReleaseHold = 60 * time.Second

// switchMark is a session or play session whose consumer of a live stream
// a channel switch released.
type switchMark struct {
	partner string
	device  string
	until   time.Time
}

// ReleaseDeviceLiveStreams releases, before a device opens another live
// stream, the live streams that the device's sessions play and for which
// release reports true (D-92). Each release is the release a stop report
// would do: the session stops counting as a viewer, and a stream that no
// other session plays loses one consumer. A later stop or progress report
// of a released stream does nothing. A start or progress report of it
// tracks it again only after SwitchReleaseHold, or after the device opens
// the stream again (ReopenDeviceLiveStream). release runs under the
// registry lock and must not call the manager.
func (manager *Manager) ReleaseDeviceLiveStreams(ctx context.Context, deviceID string, release func(liveStreamID string) bool) {
	if deviceID == "" || release == nil {
		return
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return
	}
	var closing []string
	for liveStreamID, sessions := range manager.liveStreams {
		for id, key := range manager.byID {
			if !strings.EqualFold(key.deviceID, deviceID) {
				continue
			}
			playSessionID, plays := sessions[id]
			if !plays || !release(liveStreamID) {
				continue
			}
			if manager.untrackLiveStreamLocked(liveStreamID, id) {
				closing = append(closing, liveStreamID)
			}
			marks := manager.switched[liveStreamID]
			if marks == nil {
				marks = make(map[string]switchMark)
				manager.switched[liveStreamID] = marks
			}
			until := manager.clock.Now().Add(SwitchReleaseHold)
			marks[id] = switchMark{partner: playSessionID, device: key.deviceID, until: until}
			if playSessionID != "" {
				marks[playSessionID] = switchMark{partner: id, device: key.deviceID, until: until}
			}
		}
	}
	sort.Strings(closing)
	closeLiveStream := manager.options.CloseLiveStream
	manager.mu.Unlock()
	closeLiveStreams(ctx, closeLiveStream, closing)
}

// ReopenDeviceLiveStream drops the switch marks of a device on a live
// stream after the device opened the stream again, so its next start report
// tracks the new consumer (D-92).
func (manager *Manager) ReopenDeviceLiveStream(deviceID, liveStreamID string) {
	if deviceID == "" || liveStreamID == "" {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for id, mark := range manager.switched[liveStreamID] {
		if strings.EqualFold(mark.device, deviceID) {
			manager.clearSwitchedLocked(liveStreamID, id)
		}
	}
}

// clearSwitchedLocked drops the switch mark of a session or play session
// on a live stream, with its partner id, and reports whether there was one.
func (manager *Manager) clearSwitchedLocked(liveStreamID, sessionOrPlaySessionID string) bool {
	marks := manager.switched[liveStreamID]
	mark, found := marks[sessionOrPlaySessionID]
	if !found {
		return false
	}
	delete(marks, sessionOrPlaySessionID)
	if mark.partner != "" {
		delete(marks, mark.partner)
	}
	if len(marks) == 0 {
		delete(manager.switched, liveStreamID)
	}
	return true
}

// trackLiveStreamLocked records that a session plays a live stream
// (SessionManager.UpdateLiveStreamActiveSessionMappings). A report of a
// stream that a switch released less than SwitchReleaseHold ago is ignored
// (D-92).
func (manager *Manager) trackLiveStreamLocked(liveStreamID, sessionID, playSessionID string) {
	if liveStreamID == "" {
		return
	}
	if marks := manager.switched[liveStreamID]; marks != nil {
		mark, switched := marks[sessionID]
		if !switched && playSessionID != "" {
			mark, switched = marks[playSessionID]
		}
		if switched {
			if manager.clock.Now().Before(mark.until) {
				return
			}
			manager.clearSwitchedLocked(liveStreamID, sessionID)
			if playSessionID != "" {
				manager.clearSwitchedLocked(liveStreamID, playSessionID)
			}
		}
	}
	sessions := manager.liveStreams[liveStreamID]
	if sessions == nil {
		sessions = make(map[string]string)
		manager.liveStreams[liveStreamID] = sessions
	}
	if playSessionID == "" {
		if _, found := sessions[sessionID]; !found {
			sessions[sessionID] = ""
		}
		return
	}
	if current, found := sessions[sessionID]; !found || current != playSessionID {
		if current != "" {
			delete(sessions, current)
		}
		sessions[sessionID] = playSessionID
		sessions[playSessionID] = sessionID
	}
}

// untrackLiveStreamLocked removes a session or play session from a live
// stream and reports whether to release one consumer of the stream
// (SessionManager.CloseLiveStreamIfNeededAsync). A stream with no recorded
// session is released, as upstream does.
func (manager *Manager) untrackLiveStreamLocked(liveStreamID, sessionOrPlaySessionID string) bool {
	if manager.clearSwitchedLocked(liveStreamID, sessionOrPlaySessionID) {
		return false
	}
	sessions, found := manager.liveStreams[liveStreamID]
	if !found {
		return true
	}
	release := false
	if other, found := sessions[sessionOrPlaySessionID]; found {
		delete(sessions, sessionOrPlaySessionID)
		if other != "" {
			delete(sessions, other)
		}
		release = true
	}
	if len(sessions) == 0 {
		delete(manager.liveStreams, liveStreamID)
	}
	return release
}

// closeLiveStreams runs the close hook without the registry lock. A close
// failure is not the caller's failure; the stream's idle timeout still
// applies.
func closeLiveStreams(ctx context.Context, closeLiveStream func(context.Context, string) error, ids []string) {
	if closeLiveStream == nil {
		return
	}
	for _, id := range ids {
		_ = closeLiveStream(ctx, id)
	}
}

// EndDeviceSessions removes every session whose device ID matches deviceID
// without regard to case and returns the removed session IDs in sorted order.
// It ports the session loop of SessionManager.Logout(Device).
func (manager *Manager) EndDeviceSessions(deviceID string) []string {
	manager.mu.Lock()
	if manager.closed {
		manager.unlockAndNotify()
		return nil
	}
	var ended, closing []string
	for key, snapshot := range manager.byKey {
		if !strings.EqualFold(snapshot.DeviceID, deviceID) {
			continue
		}
		ended = append(ended, snapshot.ID)
		closing = append(closing, manager.removeLocked(key, snapshot.ID)...)
	}
	closeLiveStream := manager.options.CloseLiveStream
	manager.unlockAndNotify()
	closeLiveStreams(manager.ctx, closeLiveStream, closing)
	sort.Strings(ended)
	return ended
}

// ReportSessionEnded removes a session when its final controller disconnects.
func (manager *Manager) ReportSessionEnded(id string) bool {
	return manager.Remove(id)
}

// Close releases registry state. It is safe to call once; a repeated close
// reports ErrClosed so callers can distinguish an already-closed manager.
func (manager *Manager) Close() error {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return ErrClosed
	}
	manager.closed = true
	manager.cancel()
	for _, worker := range manager.progress {
		worker.cancel()
	}
	manager.byKey = nil
	manager.byID = nil
	manager.controllers = nil
	manager.progress = nil
	manager.liveStreams = nil
	manager.switched = nil
	manager.mu.Unlock()
	manager.wg.Wait()
	return manager.changes.Close(context.Background())
}

// unlockAndNotify releases the registry lock and then runs the session
// lifecycle hooks for the creations and removals made under it.
func (manager *Manager) unlockAndNotify() {
	pending := manager.lifecycle
	manager.lifecycle = nil
	options := manager.options
	manager.mu.Unlock()
	for _, event := range pending {
		switch {
		case event.started && options.OnSessionStarted != nil:
			options.OnSessionStarted(manager.ctx, event.snapshot)
		case !event.started && options.OnSessionEnded != nil:
			options.OnSessionEnded(manager.ctx, event.snapshot)
		}
	}
}

func (manager *Manager) publishChangeLocked(id string, kind ChangeKind, force bool) {
	manager.markDirtyLocked(id)
	manager.changes.Publish(Change{SessionID: id, Kind: kind, Force: force})
}

func (manager *Manager) restartProgressLocked(snapshot *Snapshot) {
	manager.stopProgressLocked(snapshot.ID)
	ctx, cancel := context.WithCancel(manager.ctx)
	worker := &progressWorker{target: snapshot.Target, cancel: cancel}
	manager.progress[snapshot.ID] = worker
	timer := manager.clock.NewTimer(time.Second)
	manager.wg.Add(1)
	go manager.runProgress(ctx, snapshot.ID, worker, timer)
}

func (manager *Manager) stopProgressLocked(id string) {
	if worker := manager.progress[id]; worker != nil {
		delete(manager.progress, id)
		worker.cancel()
	}
}

func (manager *Manager) runProgress(ctx context.Context, id string, worker *progressWorker, timer clock.Timer) {
	defer manager.wg.Done()
	defer timer.Stop()
	defer func() {
		manager.mu.Lock()
		if manager.progress != nil && manager.progress[id] == worker {
			delete(manager.progress, id)
		}
		manager.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			if !manager.automaticProgress(id, worker) {
				return
			}
			timer.Reset(time.Second)
		}
	}
}

func (manager *Manager) automaticProgress(id string, worker *progressWorker) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed || manager.progress[id] != worker {
		return false
	}
	snapshot, found := manager.snapshotForTargetLocked(worker.target)
	if !found || snapshot.NowPlaying == nil {
		return false
	}
	if snapshot.NowPlaying.IsPaused {
		return true
	}
	position := int64(0)
	if snapshot.NowPlaying.PositionTicks != nil && *snapshot.NowPlaying.PositionTicks > 0 {
		position = *snapshot.NowPlaying.PositionTicks
	}
	position += 10_000_000
	if snapshot.NowPlaying.RunTimeTicks != nil && position >= *snapshot.NowPlaying.RunTimeTicks {
		return true
	}
	snapshot.NowPlaying.PositionTicks = &position
	snapshot.LastActivity = manager.clock.Now()
	manager.publishChangeLocked(snapshot.ID, ChangePlayback, false)
	return true
}

func (manager *Manager) runWatchdogs(idleTimer, inactiveTimer clock.Timer) {
	defer manager.wg.Done()
	defer idleTimer.Stop()
	defer inactiveTimer.Stop()
	for {
		select {
		case <-manager.ctx.Done():
			return
		case <-idleTimer.C():
			manager.checkIdlePlayback()
			idleTimer.Reset(5 * time.Minute)
		case <-inactiveTimer.C():
			manager.checkInactivePlayback()
			inactiveTimer.Reset(time.Minute)
		}
	}
}

func (manager *Manager) checkIdlePlayback() {
	options := manager.optionsSnapshot()
	now := manager.clock.Now()
	for _, candidate := range manager.playingSessions(func(snapshot Snapshot) bool {
		return now.Sub(snapshot.LastPlaybackCheckIn) > 5*time.Minute
	}) {
		if options.OnIdlePlayback != nil {
			_ = options.OnIdlePlayback(manager.ctx, candidate, *candidate.NowPlaying)
			continue
		}
		_, _ = manager.StopPlaybackWithRevision(manager.ctx, candidate.Target, *candidate.NowPlaying)
	}
}

func (manager *Manager) checkInactivePlayback() {
	options := manager.optionsSnapshot()
	if options.InactiveSessionThreshold == nil {
		return
	}
	threshold := options.InactiveSessionThreshold()
	if threshold <= 0 {
		return
	}
	now := manager.clock.Now()
	for _, candidate := range manager.playingSessions(func(snapshot Snapshot) bool {
		return snapshot.NowPlaying.IsPaused && snapshot.LastPausedDate != nil &&
			now.Sub(*snapshot.LastPausedDate) > time.Duration(threshold)*time.Minute
	}) {
		if options.OnInactivePlayback != nil {
			_ = options.OnInactivePlayback(manager.ctx, candidate, *candidate.NowPlaying)
		}
	}
}

func (manager *Manager) optionsSnapshot() Options {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.options
}

func (manager *Manager) playingSessions(accept func(Snapshot) bool) []Snapshot {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.closed {
		return nil
	}
	result := make([]Snapshot, 0, len(manager.byKey))
	for _, snapshot := range manager.byKey {
		if snapshot.NowPlaying == nil {
			continue
		}
		cloned := cloneSnapshot(*snapshot)
		if accept(cloned) {
			result = append(result, cloned)
		}
	}
	return result
}

func newSessionKey(client, deviceID string) sessionKey {
	return sessionKey{client: strings.ToLower(client), deviceID: strings.ToLower(deviceID)}
}

func (manager *Manager) nextTargetGenerationLocked() uint64 {
	manager.nextGeneration++
	if manager.nextGeneration == 0 {
		panic("session: target generation exhausted")
	}
	return manager.nextGeneration
}

func (manager *Manager) newSessionIDLocked(key sessionKey, legacyKey string) string {
	id := sessionID(legacyKey)
	if _, found := manager.byID[id]; !found {
		return id
	}
	for attempt := uint64(0); ; attempt++ {
		id = fallbackSessionID(key, attempt)
		if _, found := manager.byID[id]; !found {
			return id
		}
	}
}

// NewPlaybackReportCommitLease creates a commit-time validator for one
// accepted playback report. It does not acquire a manager lock until Acquire,
// so reports that do not require a durable mutation never block transitions.
func (manager *Manager) NewPlaybackReportCommitLease(target Target, revision uint64) *PlaybackReportCommitLease {
	return &PlaybackReportCommitLease{manager: manager, target: target, revision: revision}
}

// Acquire validates the report exactly once. When valid, it retains the
// manager read lock until Close so no later playback report or target change
// can occur before the caller's transaction commits. Subsequent calls return
// the result of that first validation.
func (lease *PlaybackReportCommitLease) Acquire() bool {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.checked {
		return lease.held
	}
	lease.checked = true
	if lease.closed {
		return false
	}

	lease.manager.mu.RLock()
	if lease.manager.closed {
		lease.manager.mu.RUnlock()
		return false
	}
	snapshot, found := lease.manager.snapshotForTargetLocked(lease.target)
	if !found || snapshot.reportRevision != lease.revision {
		lease.manager.mu.RUnlock()
		return false
	}
	lease.held = true
	return true
}

// Close releases a successfully acquired manager read lock. It is idempotent.
func (lease *PlaybackReportCommitLease) Close() {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return
	}
	lease.closed = true
	if lease.held {
		lease.manager.mu.RUnlock()
		lease.held = false
	}
}

// IsCurrentPlaybackReport reports whether target still identifies the active
// session incarnation and revision remains its most recently accepted report.
func (manager *Manager) IsCurrentPlaybackReport(target Target, revision uint64) bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.closed {
		return false
	}
	snapshot, found := manager.snapshotForTargetLocked(target)
	return found && snapshot.reportRevision == revision
}

func (manager *Manager) nextPlaybackReportRevisionLocked(snapshot *Snapshot) uint64 {
	snapshot.reportRevision++
	if snapshot.reportRevision == 0 {
		panic("session: playback report revision exhausted")
	}
	return snapshot.reportRevision
}

func (manager *Manager) snapshotForTargetLocked(target Target) (*Snapshot, bool) {
	key, found := manager.byID[target.ID]
	if !found {
		return nil, false
	}
	snapshot, found := manager.byKey[key]
	if !found || snapshot.Target != target {
		return nil, false
	}
	return snapshot, true
}

func fallbackSessionID(key sessionKey, attempt uint64) string {
	client := utf16.Encode([]rune(key.client))
	device := utf16.Encode([]rune(key.deviceID))
	bytes := make([]byte, 1+4+2*len(client)+4+2*len(device)+8)
	bytes[0] = 1
	offset := 1
	binary.LittleEndian.PutUint32(bytes[offset:], uint32(len(client)))
	offset += 4
	for _, codeUnit := range client {
		binary.LittleEndian.PutUint16(bytes[offset:], codeUnit)
		offset += 2
	}
	binary.LittleEndian.PutUint32(bytes[offset:], uint32(len(device)))
	offset += 4
	for _, codeUnit := range device {
		binary.LittleEndian.PutUint16(bytes[offset:], codeUnit)
		offset += 2
	}
	binary.LittleEndian.PutUint64(bytes[offset:], attempt)
	return guidN(md5.Sum(bytes))
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.AdditionalUserIDs = append([]dto.UUID(nil), snapshot.AdditionalUserIDs...)
	snapshot.Capabilities = cloneCapabilities(snapshot.Capabilities)
	snapshot.NowPlayingQueue = cloneQueue(snapshot.NowPlayingQueue)

	if snapshot.NowPlaying != nil {
		snapshot.NowPlaying = clonePlaybackState(*snapshot.NowPlaying)
	}
	if snapshot.LastPausedDate != nil {
		pausedAt := *snapshot.LastPausedDate
		snapshot.LastPausedDate = &pausedAt
	}

	return snapshot
}

func clonePlaybackState(state PlaybackState) *PlaybackState {
	copy := state
	if state.AudioStreamIndex != nil {
		value := *state.AudioStreamIndex
		copy.AudioStreamIndex = &value
	}
	if state.SubtitleStreamIndex != nil {
		value := *state.SubtitleStreamIndex
		copy.SubtitleStreamIndex = &value
	}
	if state.PositionTicks != nil {
		value := *state.PositionTicks
		copy.PositionTicks = &value
	}
	if state.PlaybackStartTimeTicks != nil {
		value := *state.PlaybackStartTimeTicks
		copy.PlaybackStartTimeTicks = &value
	}
	if state.VolumeLevel != nil {
		value := *state.VolumeLevel
		copy.VolumeLevel = &value
	}
	if state.Brightness != nil {
		value := *state.Brightness
		copy.Brightness = &value
	}
	if state.NowPlayingQueue != nil {
		copy.NowPlayingQueue = pointerToQueue(cloneQueue(*state.NowPlayingQueue))
	}
	if state.RunTimeTicks != nil {
		value := *state.RunTimeTicks
		copy.RunTimeTicks = &value
	}

	return &copy
}

func cloneQueue(queue []dto.QueueItem) []dto.QueueItem {
	cloned := append([]dto.QueueItem(nil), queue...)
	for index := range cloned {
		if cloned[index].PlaylistItemID != nil {
			value := *cloned[index].PlaylistItemID
			cloned[index].PlaylistItemID = &value
		}
	}
	return cloned
}

func pointerToQueue(queue []dto.QueueItem) *[]dto.QueueItem {
	return &queue
}

func cloneCapabilities(capabilities Capabilities) Capabilities {
	capabilities.PlayableMediaTypes = append([]string(nil), capabilities.PlayableMediaTypes...)
	capabilities.SupportedCommands = append([]string(nil), capabilities.SupportedCommands...)
	capabilities.DeviceProfile = append([]byte(nil), capabilities.DeviceProfile...)
	return capabilities
}

func sessionID(key string) string {
	codeUnits := utf16.Encode([]rune(key))
	bytes := make([]byte, len(codeUnits)*2)
	for index, codeUnit := range codeUnits {
		bytes[index*2] = byte(codeUnit)
		bytes[index*2+1] = byte(codeUnit >> 8)
	}
	hash := md5.Sum(bytes)
	return guidN(hash)
}

func guidN(hash [md5.Size]byte) string {
	const hexadecimal = "0123456789abcdef"
	var result [md5.Size * 2]byte
	order := [md5.Size]byte{3, 2, 1, 0, 5, 4, 7, 6, 8, 9, 10, 11, 12, 13, 14, 15}
	for index, source := range order {
		value := hash[source]
		result[index*2] = hexadecimal[value>>4]
		result[index*2+1] = hexadecimal[value&0x0f]
	}
	return string(result[:])
}
