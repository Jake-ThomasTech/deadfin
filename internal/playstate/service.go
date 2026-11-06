// SPDX-License-Identifier: GPL-2.0-only

package playstate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/deadfin-org/deadfin/internal/activitylog"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/events"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
)

// Actor identifies the authenticated user and current client/device session
// authorized to submit a playback report.
type Actor struct {
	UserID   dto.UUID
	Client   string
	DeviceID string
	Target   session.Target
}

// Report is a client playback report. Failed marks a stopped report whose
// persisted playback state must not be changed.
type Report struct {
	session.PlaybackState
	Failed bool
}

// Application applies client playback reports to the live session and durable
// user data.
type Application interface {
	Start(context.Context, Actor, Report) error
	Progress(context.Context, Actor, Report) error
	Stop(context.Context, Actor, Report) error
}

// ConfigurationReader supplies the server resume-policy settings.
type ConfigurationReader interface {
	System() config.ServerConfiguration
}

// ItemStore supplies canonical item facts and durable per-user mutations whose
// commit can be guarded by the caller's live-session lease.
type ItemStore interface {
	Get(context.Context, dto.UUID) (item.Item, error)
	MediaSourceRecords(context.Context, dto.UUID) ([]playback.MediaSourceRecord, error)
	MutateUserDataForItemGuarded(context.Context, dto.UUID, dto.UUID, func(*store.UserData) error, func() bool) (store.UserData, bool, error)
	MutateUserDataForItemAndVersionsGuarded(context.Context, dto.UUID, dto.UUID, []dto.UUID, func(*store.UserData) (bool, error), func() bool) (store.UserData, bool, error)
}

// UserReader supplies persisted user playback preferences.
type UserReader interface {
	Get(context.Context, dto.UUID) (store.User, error)
}

// UserDataChanged identifies user data that has committed a playback change.
type UserDataChanged struct {
	UserID  dto.UUID
	ItemID  dto.UUID
	ItemIDs []dto.UUID
}

// Service is the playback application seam between transport reports and the
// session, item, user, and notification collaborators.
type Service struct {
	clock         clock.Clock
	configuration ConfigurationReader
	items         ItemStore
	users         UserReader
	sessions      *session.Manager
	changes       *events.Bus[UserDataChanged]
	activity      activitylog.Recorder
}

// NewService constructs the playback application service over its required
// collaborators. The caller retains ownership of all injected collaborators.
func NewService(c clock.Clock, configuration ConfigurationReader, items ItemStore, users UserReader, sessions *session.Manager, changes *events.Bus[UserDataChanged]) *Service {
	if c == nil {
		panic("playstate: clock is required")
	}
	if configuration == nil {
		panic("playstate: configuration is required")
	}
	if items == nil {
		panic("playstate: item store is required")
	}
	if users == nil {
		panic("playstate: user reader is required")
	}
	if sessions == nil {
		panic("playstate: session manager is required")
	}
	if changes == nil {
		panic("playstate: user-data change bus is required")
	}
	return &Service{
		clock:         c,
		configuration: configuration,
		items:         items,
		users:         users,
		sessions:      sessions,
		changes:       changes,
	}
}

// WithActivity makes the service record playback start and stop entries in
// the activity log. Call it before the service handles a report.
func (service *Service) WithActivity(recorder activitylog.Recorder) *Service {
	service.activity = recorder
	return service
}

// Start records the report in the authenticated live session before updating
// every user associated with that exact current session target.
func (service *Service) Start(ctx context.Context, actor Actor, report Report) error {
	_, found := service.actorSnapshot(actor)
	if !found {
		return session.ErrNotFound
	}
	revision, err := service.sessions.StartPlaybackWithRevision(ctx, actor.Target, report.PlaybackState)
	if err != nil {
		return err
	}
	snapshot, found := service.actorSnapshot(actor)
	if !found {
		return session.ErrNotFound
	}
	if report.ItemID.IsNil() {
		return nil
	}
	if err := service.recordPlayback(ctx, snapshot, report.ItemID, true); err != nil {
		return err
	}

	facts, err := service.itemFacts(ctx, report.ItemID, report.MediaSourceID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	service.sessions.SetPlaybackRuntime(actor.Target, facts.runTimeTicks)

	for _, userID := range sessionUsers(snapshot) {
		lease := service.sessions.NewPlaybackReportCommitLease(actor.Target, revision)
		now := service.clock.Now()
		_, committed, err := service.items.MutateUserDataForItemGuarded(ctx, userID, facts.id, func(data *store.UserData) error {
			data.PlayCount++
			data.LastPlayed = &now
			if facts.supportsPlayedStatus && !facts.supportsPositionTicksResume {
				data.Played = true
			}
			return nil
		}, lease.Acquire)
		lease.Close()
		if err != nil {
			return err
		}
		if !committed {
			return nil
		}
		service.publish(userID, facts.id)
	}
	return nil
}

// Progress updates the authenticated live session and persists only the report
// fields that can affect durable resume state or stream selection preferences.
func (service *Service) Progress(ctx context.Context, actor Actor, report Report) error {
	_, found := service.actorSnapshot(actor)
	if !found {
		return nil
	}
	revision, accepted, err := service.sessions.ProgressPlaybackWithRevision(ctx, actor.Target, report.PlaybackState)
	if err != nil {
		return err
	}
	if !accepted {
		return nil
	}
	snapshot, found := service.actorSnapshot(actor)
	if !found {
		return nil
	}
	if report.ItemID.IsNil() {
		return nil
	}

	facts, err := service.itemFacts(ctx, report.ItemID, report.MediaSourceID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	service.sessions.SetPlaybackRuntime(actor.Target, facts.runTimeTicks)
	if report.PositionTicks == nil && report.AudioStreamIndex == nil && report.SubtitleStreamIndex == nil {
		return nil
	}

	for _, userID := range sessionUsers(snapshot) {
		lease := service.sessions.NewPlaybackReportCommitLease(actor.Target, revision)
		preferences, err := service.selectionPreferences(ctx, userID)
		if err != nil {
			return err
		}
		_, committed, err := service.items.MutateUserDataForItemAndVersionsGuarded(ctx, userID, facts.id, facts.versionIDs, func(data *store.UserData) (bool, error) {
			complete := false
			if report.PositionTicks != nil {
				complete = service.applyResume(data, facts, report.PositionTicks)
			}
			applySelectionPreferences(data, preferences, report.PlaybackState)
			return complete, nil
		}, lease.Acquire)
		lease.Close()
		if err != nil {
			return err
		}
		if !committed {
			return nil
		}
	}
	return nil
}

// Stop clears the authenticated live session. A successful, non-failed report
// then persists its final durable playback state for every session user.
func (service *Service) Stop(ctx context.Context, actor Actor, report Report) error {
	if report.PositionTicks != nil && *report.PositionTicks < 0 {
		// Release the live stream first so a stalled client does not hold
		// a tuner (SessionManager.OnPlaybackStopped).
		if snapshot, found := service.actorSnapshot(actor); found {
			service.sessions.CloseLiveStreamIfNeeded(ctx, report.LiveStreamID, snapshot.ID)
		}
		return errors.New("playstate: position ticks must not be negative")
	}

	_, found := service.actorSnapshot(actor)
	if !found {
		return session.ErrNotFound
	}
	revision, err := service.sessions.StopPlaybackWithRevision(ctx, actor.Target, report.PlaybackState)
	if err != nil {
		return err
	}
	snapshot, found := service.actorSnapshot(actor)
	if !found {
		return session.ErrNotFound
	}
	if report.ItemID.IsNil() {
		return nil
	}
	if err := service.recordPlayback(ctx, snapshot, report.ItemID, false); err != nil {
		return err
	}
	if report.Failed {
		return nil
	}

	facts, err := service.itemFacts(ctx, report.ItemID, report.MediaSourceID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, userID := range sessionUsers(snapshot) {
		lease := service.sessions.NewPlaybackReportCommitLease(actor.Target, revision)
		_, committed, err := service.items.MutateUserDataForItemAndVersionsGuarded(ctx, userID, facts.id, facts.versionIDs, func(data *store.UserData) (bool, error) {
			if report.PositionTicks == nil {
				data.PlayCount++
				data.PositionTicks = 0
				data.Played = facts.supportsPlayedStatus
				return data.Played, nil
			}
			return service.applyResume(data, facts, report.PositionTicks), nil
		}, lease.Acquire)
		lease.Close()
		if err != nil {
			return err
		}
		if !committed {
			return nil
		}
		service.publish(userID, facts.id)
	}
	return nil
}

// recordPlayback writes the PlaybackStartLogger or PlaybackStopLogger entry
// for the displayed library item. An unknown item, theme media, and a user
// that no longer exists write no entry, as upstream.
func (service *Service) recordPlayback(ctx context.Context, snapshot session.Snapshot, itemID dto.UUID, started bool) error {
	if service.activity == nil {
		return nil
	}
	played, err := service.items.Get(ctx, itemID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	if played.ExtraType != nil && (*played.ExtraType == item.ExtraTypeThemeSong || *played.ExtraType == item.ExtraTypeThemeVideo) {
		return nil
	}
	user, err := service.users.Get(ctx, snapshot.UserID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	playbackItem := activitylog.PlaybackItem{
		ID:         played.ID,
		Name:       stringValue(played.Name),
		SeriesName: stringValue(played.SeriesName),
		Artists:    played.Values.Artists,
		MediaType:  item.MediaTypeOf(played).String(),
	}
	if started {
		service.activity.Record(activitylog.PlaybackStarted(user.ID, user.Name, playbackItem, snapshot.DeviceName))
		return nil
	}
	if entry, ok := activitylog.PlaybackStopped(user.ID, user.Name, playbackItem, snapshot.DeviceName); ok {
		service.activity.Record(entry)
	}
	return nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (service *Service) actorSnapshot(actor Actor) (session.Snapshot, bool) {
	snapshot, found := service.sessions.Session(actor.Client, actor.DeviceID)
	if !found || snapshot.Target != actor.Target || snapshot.UserID != actor.UserID || !strings.EqualFold(snapshot.Client, actor.Client) || !strings.EqualFold(snapshot.DeviceID, actor.DeviceID) {
		return session.Snapshot{}, false
	}
	return snapshot, true
}

type canonicalItem struct {
	id                          dto.UUID
	versionIDs                  []dto.UUID
	runTimeTicks                *int64
	isAudiobook                 bool
	isBook                      bool
	supportsPlayedStatus        bool
	supportsPositionTicksResume bool
}

func (service *Service) itemFacts(ctx context.Context, id dto.UUID, mediaSourceID string) (canonicalItem, error) {
	displayed, err := service.items.Get(ctx, id)
	if err != nil {
		return canonicalItem{}, err
	}
	facts := canonicalItemFor(displayed)
	if item.MediaTypeOf(displayed) != item.MediaTypeVideo {
		return facts, nil
	}

	records, err := service.items.MediaSourceRecords(ctx, displayed.ID)
	if err != nil {
		return canonicalItem{}, err
	}
	facts.versionIDs = sourceItemIDs(displayed.ID, records)
	candidateID, err := dto.ParseUUID(mediaSourceID)
	if err != nil || candidateID == displayed.ID {
		return facts, nil
	}
	for _, record := range records {
		if record.ID != candidateID {
			continue
		}
		selected, err := service.items.Get(ctx, candidateID)
		if err != nil {
			return canonicalItem{}, err
		}
		facts = canonicalItemFor(selected)
		facts.versionIDs = sourceItemIDs(displayed.ID, records)
		return facts, nil
	}
	return facts, nil
}

func canonicalItemFor(value item.Item) canonicalItem {
	return canonicalItem{
		id:                          value.ID,
		versionIDs:                  []dto.UUID{value.ID},
		runTimeTicks:                value.RunTimeTicks,
		isAudiobook:                 value.Kind == item.KindAudioBook,
		isBook:                      value.Kind == item.KindBook,
		supportsPlayedStatus:        item.SupportsPlayedStatus(value),
		supportsPositionTicksResume: item.SupportsPositionTicksResume(value),
	}
}

func sourceItemIDs(displayedID dto.UUID, records []playback.MediaSourceRecord) []dto.UUID {
	ids := make([]dto.UUID, 0, 1+len(records))
	seen := make(map[dto.UUID]struct{}, 1+len(records))
	for _, id := range append([]dto.UUID{displayedID}, sourceRecordIDs(records)...) {
		if id.IsNil() {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func sourceRecordIDs(records []playback.MediaSourceRecord) []dto.UUID {
	ids := make([]dto.UUID, len(records))
	for index, record := range records {
		ids[index] = record.ID
	}
	return ids
}

func (service *Service) applyResume(data *store.UserData, facts canonicalItem, positionTicks *int64) bool {
	result := ApplyResumePolicy(service.resumeSettings(), ResumeInput{
		PositionTicks:               positionTicks,
		RunTimeTicks:                facts.runTimeTicks,
		IsAudiobook:                 facts.isAudiobook,
		IsBook:                      facts.isBook,
		SupportsPlayedStatus:        facts.supportsPlayedStatus,
		SupportsPositionTicksResume: facts.supportsPositionTicksResume,
	}, data.Played)
	data.PositionTicks = result.PositionTicks
	data.Played = result.Played
	// Only this update's completion marks the versions played; an item that
	// was played before keeps a part-way position (UserDataManager.UpdatePlayState
	// returns playedToCompletion).
	return result.PlayedToCompletion
}

func (service *Service) resumeSettings() ResumeSettings {
	configuration := service.configuration.System()
	return ResumeSettings{
		MinResumePct:             configuration.MinResumePct,
		MaxResumePct:             configuration.MaxResumePct,
		MinResumeDurationSeconds: configuration.MinResumeDurationSeconds,
		MinAudiobookResume:       configuration.MinAudiobookResume,
		MaxAudiobookResume:       configuration.MaxAudiobookResume,
	}
}

func (service *Service) selectionPreferences(ctx context.Context, userID dto.UUID) (dto.UserConfiguration, error) {
	user, err := service.users.Get(ctx, userID)
	if err != nil {
		return dto.UserConfiguration{}, err
	}
	configuration := dto.DefaultUserConfiguration()
	if len(user.Configuration) == 0 {
		return configuration, nil
	}
	if err := json.Unmarshal(user.Configuration, &configuration); err != nil {
		return dto.UserConfiguration{}, err
	}
	return configuration, nil
}

func applySelectionPreferences(data *store.UserData, configuration dto.UserConfiguration, state session.PlaybackState) {
	if !configuration.RememberAudioSelections {
		data.AudioStreamIndex = nil
	} else if state.AudioStreamIndex != nil {
		index := *state.AudioStreamIndex
		data.AudioStreamIndex = &index
	}
	if !configuration.RememberSubtitleSelections {
		data.SubtitleStreamIndex = nil
	} else if state.SubtitleStreamIndex != nil {
		index := *state.SubtitleStreamIndex
		data.SubtitleStreamIndex = &index
	}
}

func (service *Service) publish(userID, itemID dto.UUID) {
	service.changes.Publish(UserDataChanged{UserID: userID, ItemID: itemID})
}

func sessionUsers(snapshot session.Snapshot) []dto.UUID {
	users := make([]dto.UUID, 0, 1+len(snapshot.AdditionalUserIDs))
	users = append(users, snapshot.UserID)
	users = append(users, snapshot.AdditionalUserIDs...)
	return users
}
