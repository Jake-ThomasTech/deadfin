// SPDX-License-Identifier: GPL-2.0-only

package playstate_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/events"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playstate"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

// These tests define the application seam between the HTTP adapter and the
// real live-session, user-data, and notification collaborators. In particular,
// the service must resolve users from the authenticated session rather than
// from PlaySessionID or report body identity.
func TestServiceStartUpdatesLiveSessionAndPersistsEverySessionUser(t *testing.T) {
	fixture := newPlaystateServiceFixture(t, item.KindMovie)
	primaryBefore := fixture.seedUserData(t, fixture.primaryUser)
	additionalBefore := fixture.seedUserData(t, fixture.additionalUser)
	other, err := fixture.sessions.LogActivity(t.Context(), session.Activity{
		Client: "Other Client", Version: "1.0.0", DeviceID: "other-device", UserID: dto.UUID{0x23},
	})
	if err != nil {
		t.Fatalf("register other session: %v", err)
	}
	position := ticks(900)
	audio, subtitle := 8, 9
	report := playstate.Report{PlaybackState: session.PlaybackState{
		ItemID:              fixture.item.ID,
		MediaSourceID:       "main-source",
		PositionTicks:       &position,
		AudioStreamIndex:    &audio,
		SubtitleStreamIndex: &subtitle,
		CanSeek:             true,
		PlayMethod:          "DirectPlay",
		PlaySessionID:       other.ID,
	}}

	if err := fixture.application.Start(t.Context(), fixture.actor, report); err != nil {
		t.Fatalf("start playback: %v", err)
	}

	started := fixture.session(t)
	if got := started.NowPlaying; got == nil || got.ItemID != fixture.item.ID || got.PositionTicks == nil || *got.PositionTicks != position || got.MediaSourceID != "main-source" || got.PlaySessionID != other.ID || !got.CanSeek {
		t.Fatalf("started session playback state = %#v, want authenticated report state", got)
	}
	if redirected, found := fixture.sessions.Session(other.Client, other.DeviceID); !found || redirected.NowPlaying != nil {
		t.Fatalf("PlaySessionID redirected start to another session: %#v", redirected.NowPlaying)
	}
	for _, tc := range []struct {
		name   string
		userID dto.UUID
		before store.UserData
	}{
		{name: "primary user", userID: fixture.primaryUser, before: primaryBefore},
		{name: "additional session user", userID: fixture.additionalUser, before: additionalBefore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fixture.userData(t, tc.userID)
			if got.PlayCount != tc.before.PlayCount+1 || got.LastPlayed == nil || !got.LastPlayed.Equal(fixture.clock.Now()) || got.Played != tc.before.Played {
				t.Fatalf("start playback durable facts = %#v, want incremented count, fake-clock last played, and unchanged resumable played state", got)
			}
			if got.IsFavorite != tc.before.IsFavorite || !reflect.DeepEqual(got.Rating, tc.before.Rating) || !reflect.DeepEqual(got.Likes, tc.before.Likes) {
				t.Fatalf("start playback discarded unrelated user-data facts: got %#v, seeded %#v", got, tc.before)
			}
		})
	}
	fixture.assertEvents(t, 2)
}

func TestServiceStartWithZeroItemOnlyUpdatesLiveSession(t *testing.T) {
	fixture := newPlaystateServiceFixture(t, item.KindMovie)
	report := playstate.Report{PlaybackState: session.PlaybackState{PlaySessionID: "zero-item", PlayMethod: "DirectPlay"}}

	if err := fixture.application.Start(t.Context(), fixture.actor, report); err != nil {
		t.Fatalf("start zero-item playback: %v", err)
	}
	if got := fixture.session(t).NowPlaying; got == nil || !got.ItemID.IsNil() || got.PlaySessionID != "zero-item" {
		t.Fatalf("zero-item start session state = %#v, want live report without an item", got)
	}
	if _, err := fixture.items.GetUserData(t.Context(), fixture.primaryUser, dto.UUID{}.Dashed()); !store.IsNotFound(err) {
		t.Fatalf("zero-item start persisted user data error = %v, want not found", err)
	}
	fixture.assertEvents(t, 0)
}

// A valid but unknown item is not durable media state. Playback reports must
// still update the authenticated live session without creating user data or
// notifying subscribers.
func TestServiceUnknownItemOnlyTransitionsLiveSession(t *testing.T) {
	unknown := dto.UUID{0x99}
	position := ticks(300)
	audio, subtitle := 6, 7
	report := playstate.Report{PlaybackState: session.PlaybackState{
		ItemID:              unknown,
		MediaSourceID:       "unknown-source",
		PositionTicks:       &position,
		AudioStreamIndex:    &audio,
		SubtitleStreamIndex: &subtitle,
		IsPaused:            true,
		PlayMethod:          "DirectPlay",
		PlaySessionID:       "unknown-item-session",
	}}

	for _, tc := range []struct {
		name       string
		prepare    func(t *testing.T, fixture playstateServiceFixture)
		apply      func(context.Context, playstate.Application, playstate.Actor, playstate.Report) error
		assertLive func(t *testing.T, fixture playstateServiceFixture, report playstate.Report)
	}{
		{
			name:    "start records the reported state",
			prepare: func(_ *testing.T, _ playstateServiceFixture) {},
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Start(ctx, actor, report)
			},
			assertLive: func(t *testing.T, fixture playstateServiceFixture, report playstate.Report) {
				t.Helper()
				if got := fixture.session(t).NowPlaying; !reflect.DeepEqual(got, &report.PlaybackState) {
					t.Fatalf("unknown-item start live state = %#v, want %#v", got, &report.PlaybackState)
				}
			},
		},
		{
			name: "progress replaces the reported state",
			prepare: func(t *testing.T, fixture playstateServiceFixture) {
				fixture.startLivePlayback(t)
			},
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Progress(ctx, actor, report)
			},
			assertLive: func(t *testing.T, fixture playstateServiceFixture, report playstate.Report) {
				t.Helper()
				if got := fixture.session(t).NowPlaying; !reflect.DeepEqual(got, &report.PlaybackState) {
					t.Fatalf("unknown-item progress live state = %#v, want %#v", got, &report.PlaybackState)
				}
			},
		},
		{
			name: "stop clears the state",
			prepare: func(t *testing.T, fixture playstateServiceFixture) {
				fixture.startLivePlayback(t)
			},
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Stop(ctx, actor, report)
			},
			assertLive: func(t *testing.T, fixture playstateServiceFixture, _ playstate.Report) {
				t.Helper()
				if got := fixture.session(t).NowPlaying; got != nil {
					t.Fatalf("unknown-item stop retained live state %#v", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlaystateServiceFixture(t, item.KindMovie)
			tc.prepare(t, fixture)

			if err := tc.apply(t.Context(), fixture.application, fixture.actor, report); err != nil {
				t.Errorf("unknown-item %s returned error: %v", tc.name, err)
			}
			tc.assertLive(t, fixture, report)
			fixture.assertNoUserData(t)
			fixture.assertEvents(t, 0)
		})
	}
}

func TestServiceItemReaderFailureStillPropagates(t *testing.T) {
	readerErr := errors.New("item reader unavailable")
	unknown := dto.UUID{0x99}
	report := playstate.Report{PlaybackState: session.PlaybackState{ItemID: unknown}}

	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, fixture playstateServiceFixture)
		apply   func(context.Context, playstate.Application, playstate.Actor, playstate.Report) error
	}{
		{
			name:    "start",
			prepare: func(_ *testing.T, _ playstateServiceFixture) {},
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Start(ctx, actor, report)
			},
		},
		{
			name: "progress",
			prepare: func(t *testing.T, fixture playstateServiceFixture) {
				fixture.startLivePlayback(t)
			},
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Progress(ctx, actor, report)
			},
		},
		{
			name: "stop",
			prepare: func(t *testing.T, fixture playstateServiceFixture) {
				fixture.startLivePlayback(t)
			},
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Stop(ctx, actor, report)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlaystateServiceFixture(t, item.KindMovie)
			fixture.application = playstate.NewService(
				fixture.clock,
				playstateServiceConfiguration{},
				playstateServiceErrorItems{ItemStore: fixture.items, err: readerErr},
				fixture.users,
				fixture.sessions,
				fixture.changes,
			)
			tc.prepare(t, fixture)

			if err := tc.apply(t.Context(), fixture.application, fixture.actor, report); !errors.Is(err, readerErr) {
				t.Fatalf("%s item reader failure = %v, want %v", tc.name, err, readerErr)
			}
		})
	}
}

func TestServiceProgressAppliesConfiguredResumePolicyWithoutNotifications(t *testing.T) {
	for _, tc := range []struct {
		name         string
		position     int64
		wantPosition int64
		wantPlayed   bool
	}{
		{name: "position exactly at configured minimum remains resumable", position: ticks(120), wantPosition: ticks(120), wantPlayed: false},
		{name: "position above configured maximum completes playback", position: ticks(831), wantPosition: 0, wantPlayed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlaystateServiceFixture(t, item.KindMovie)
			primaryBefore := fixture.seedUserData(t, fixture.primaryUser)
			additionalBefore := fixture.seedUserData(t, fixture.additionalUser)
			if err := fixture.sessions.StartPlayback(t.Context(), fixture.actor.Target, session.PlaybackState{ItemID: fixture.item.ID, PlaySessionID: "existing-session"}); err != nil {
				t.Fatalf("seed live playback session: %v", err)
			}
			audio, subtitle := 14, 15
			report := playstate.Report{PlaybackState: session.PlaybackState{
				ItemID:              fixture.item.ID,
				PositionTicks:       &tc.position,
				AudioStreamIndex:    &audio,
				SubtitleStreamIndex: &subtitle,
				IsPaused:            true,
				PlaySessionID:       "opaque-playback-correlation",
			}}

			if err := fixture.application.Progress(t.Context(), fixture.actor, report); err != nil {
				t.Fatalf("report playback progress: %v", err)
			}

			progressed := fixture.session(t)
			if got := progressed.NowPlaying; got == nil || got.PositionTicks == nil || *got.PositionTicks != tc.position || !got.IsPaused || got.PlaySessionID != "opaque-playback-correlation" {
				t.Fatalf("progressed live playback state = %#v, want reported state", got)
			}
			for _, user := range []struct {
				userID dto.UUID
				before store.UserData
			}{{fixture.primaryUser, primaryBefore}, {fixture.additionalUser, additionalBefore}} {
				got := fixture.userData(t, user.userID)
				if got.PositionTicks != tc.wantPosition || got.Played != tc.wantPlayed || got.AudioStreamIndex == nil || *got.AudioStreamIndex != audio || got.SubtitleStreamIndex == nil || *got.SubtitleStreamIndex != subtitle {
					t.Fatalf("progress durable facts = %#v, want position %d, played %v, and selected streams", got, tc.wantPosition, tc.wantPlayed)
				}
				if got.PlayCount != user.before.PlayCount || !reflect.DeepEqual(got.LastPlayed, user.before.LastPlayed) || got.IsFavorite != user.before.IsFavorite || !reflect.DeepEqual(got.Rating, user.before.Rating) || !reflect.DeepEqual(got.Likes, user.before.Likes) {
					t.Fatalf("progress changed durable facts outside resume state: got %#v, seeded %#v", got, user.before)
				}
			}
			fixture.assertEvents(t, 0)
		})
	}
}

func TestServiceProgressForMissingSessionDoesNotPersistOrSignal(t *testing.T) {
	fixture := newPlaystateServiceFixture(t, item.KindMovie)
	before := fixture.seedUserData(t, fixture.primaryUser)
	position := ticks(300)
	missing := playstate.Actor{Client: "unknown-client", DeviceID: "unknown-device"}

	if err := fixture.application.Progress(t.Context(), missing, playstate.Report{PlaybackState: session.PlaybackState{ItemID: fixture.item.ID, PositionTicks: &position}}); err != nil {
		t.Fatalf("progress missing session: %v", err)
	}
	if _, found := fixture.sessions.Session(missing.Client, missing.DeviceID); found {
		t.Fatalf("progress created a missing session for %#v", missing)
	}
	if got := fixture.userData(t, fixture.primaryUser); !reflect.DeepEqual(got, before) {
		t.Fatalf("missing-session progress persisted %#v, want %#v", got, before)
	}
	fixture.assertEvents(t, 0)
}

// A report authenticated for a removed session must not be redirected to a
// later authentication that reused the same case-insensitive client/device
// identity. The service currently has no inter-call replacement hook, so this
// constructs the deterministic stale-actor flow at its public boundary.
func TestServiceProgressFromReplacedSessionDoesNotMutateReplacement(t *testing.T) {
	fixture := newPlaystateServiceFixture(t, item.KindMovie)
	original := fixture.session(t)
	replacementPrimary := dto.UUID{0x31}
	replacementAdditional := dto.UUID{0x32}
	fixture.users.byID[replacementPrimary] = store.User{ID: replacementPrimary, Configuration: []byte(`{"RememberAudioSelections":true,"RememberSubtitleSelections":true}`)}
	fixture.users.byID[replacementAdditional] = store.User{ID: replacementAdditional, Configuration: []byte(`{"RememberAudioSelections":true,"RememberSubtitleSelections":true}`)}
	primaryBefore := fixture.seedUserData(t, replacementPrimary)
	additionalBefore := fixture.seedUserData(t, replacementAdditional)

	if !fixture.sessions.Remove(original.ID) {
		t.Fatalf("remove original authenticated session %q", original.ID)
	}
	replacement, err := fixture.sessions.LogActivity(t.Context(), session.Activity{
		Client:            "SWIFTFIN",
		Version:           "2.0.0",
		DeviceID:          "LIVING-ROOM",
		DeviceName:        "Replacement Living Room",
		UserID:            replacementPrimary,
		AdditionalUserIDs: []dto.UUID{replacementAdditional},
	})
	if err != nil {
		t.Fatalf("register replacement session: %v", err)
	}
	position := ticks(300)

	if err := fixture.application.Progress(t.Context(), fixture.actor, playstate.Report{PlaybackState: session.PlaybackState{
		ItemID: fixture.item.ID, PositionTicks: &position, PlaySessionID: "stale-session-report",
	}}); err != nil {
		t.Fatalf("progress from replaced session: %v", err)
	}

	current, found := fixture.sessions.Session("swiftfin", "living-room")
	if !found || current.ID != replacement.ID || current.NowPlaying != nil {
		t.Fatalf("stale progress changed replacement live session = %#v, found=%t; want untouched replacement %q", current, found, replacement.ID)
	}
	if got := fixture.userData(t, replacementPrimary); !reflect.DeepEqual(got, primaryBefore) {
		t.Fatalf("stale progress changed replacement primary user data = %#v, want %#v", got, primaryBefore)
	}
	if got := fixture.userData(t, replacementAdditional); !reflect.DeepEqual(got, additionalBefore) {
		t.Fatalf("stale progress changed replacement additional user data = %#v, want %#v", got, additionalBefore)
	}
	fixture.assertEvents(t, 0)
}

// An authenticated user cannot retarget its report by supplying another live
// client/device pair. The actor's resolved target and user identity are the
// authority; both the authentic session and the known spoofed session must
// remain untouched.
func TestServiceProgressRejectsActorSpoofingKnownSessionIdentity(t *testing.T) {
	fixture := newPlaystateServiceFixture(t, item.KindMovie)
	knownUser := dto.UUID{0x33}
	fixture.users.byID[knownUser] = store.User{ID: knownUser, Configuration: []byte(`{"RememberAudioSelections":true,"RememberSubtitleSelections":true}`)}
	known, err := fixture.sessions.LogActivity(t.Context(), session.Activity{
		Client: "Known Client", Version: "1.0.0", DeviceID: "known-device", DeviceName: "Known Device", UserID: knownUser,
	})
	if err != nil {
		t.Fatalf("register known session: %v", err)
	}
	primaryBefore := fixture.seedUserData(t, fixture.primaryUser)
	knownBefore := fixture.seedUserData(t, knownUser)
	spoofed := fixture.actor
	spoofed.Client = known.Client
	spoofed.DeviceID = known.DeviceID
	position := ticks(300)

	if err := fixture.application.Progress(t.Context(), spoofed, playstate.Report{PlaybackState: session.PlaybackState{
		ItemID: fixture.item.ID, PositionTicks: &position, PlaySessionID: "spoofed-session-report",
	}}); err != nil {
		t.Fatalf("progress with spoofed known session identity: %v", err)
	}

	if authentic := fixture.session(t); authentic.NowPlaying != nil {
		t.Fatalf("spoofed progress changed authentic session = %#v", authentic)
	}
	if current, found := fixture.sessions.Session(known.Client, known.DeviceID); !found || current.ID != known.ID || current.NowPlaying != nil {
		t.Fatalf("spoofed progress changed known session = %#v, found=%t; want untouched session %q", current, found, known.ID)
	}
	if got := fixture.userData(t, fixture.primaryUser); !reflect.DeepEqual(got, primaryBefore) {
		t.Fatalf("spoofed progress changed authentic user data = %#v, want %#v", got, primaryBefore)
	}
	if got := fixture.userData(t, knownUser); !reflect.DeepEqual(got, knownBefore) {
		t.Fatalf("spoofed progress changed known session user data = %#v, want %#v", got, knownBefore)
	}
	fixture.assertEvents(t, 0)
}

func TestServiceProgressHonorsExplicitSelectionMemoryPreferences(t *testing.T) {
	fixture := newPlaystateServiceFixture(t, item.KindMovie)
	fixture.users.byID[fixture.primaryUser] = store.User{ID: fixture.primaryUser, Configuration: []byte(`{"RememberAudioSelections":false,"RememberSubtitleSelections":false}`)}
	fixture.seedUserData(t, fixture.primaryUser)
	fixture.seedUserData(t, fixture.additionalUser)
	fixture.startLivePlayback(t)
	position := ticks(120)
	audio, subtitle := 14, 15
	report := playstate.Report{PlaybackState: session.PlaybackState{
		ItemID: fixture.item.ID, PositionTicks: &position, AudioStreamIndex: &audio, SubtitleStreamIndex: &subtitle,
	}}

	if err := fixture.application.Progress(t.Context(), fixture.actor, report); err != nil {
		t.Fatalf("report progress with explicit selection preferences: %v", err)
	}
	if got := fixture.userData(t, fixture.primaryUser); got.AudioStreamIndex != nil || got.SubtitleStreamIndex != nil {
		t.Fatalf("disabled selection memory retained streams: %#v", got)
	}
	if got := fixture.userData(t, fixture.additionalUser); got.AudioStreamIndex == nil || *got.AudioStreamIndex != audio || got.SubtitleStreamIndex == nil || *got.SubtitleStreamIndex != subtitle {
		t.Fatalf("enabled selection memory did not retain reported streams: %#v", got)
	}
	fixture.assertEvents(t, 0)
}

func TestServiceStopPersistsNormalAndUnknownPositionTransitions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind         item.Kind
		position     *int64
		wantPosition int64
		wantPlayed   bool
		wantCountAdd int
	}{
		{name: "normal position applies configured resume policy", kind: item.KindMovie, position: int64Pointer(ticks(120)), wantPosition: ticks(120), wantPlayed: false},
		{name: "unknown position completes a playable item", kind: item.KindMovie, wantPosition: 0, wantPlayed: true, wantCountAdd: 1},
		{name: "unknown position keeps a non-playable item unplayed", kind: item.KindPhoto, wantPosition: 0, wantPlayed: false, wantCountAdd: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlaystateServiceFixture(t, tc.kind)
			primaryBefore := fixture.seedUserData(t, fixture.primaryUser)
			additionalBefore := fixture.seedUserData(t, fixture.additionalUser)
			fixture.startLivePlayback(t)
			report := playstate.Report{PlaybackState: session.PlaybackState{
				ItemID:        fixture.item.ID,
				PositionTicks: tc.position,
				PlaySessionID: "opaque-playback-correlation",
			}}

			if err := fixture.application.Stop(t.Context(), fixture.actor, report); err != nil {
				t.Fatalf("stop playback: %v", err)
			}
			if got := fixture.session(t).NowPlaying; got != nil {
				t.Fatalf("stopped session retained now-playing state %#v", got)
			}
			for _, user := range []struct {
				userID dto.UUID
				before store.UserData
			}{{fixture.primaryUser, primaryBefore}, {fixture.additionalUser, additionalBefore}} {
				got := fixture.userData(t, user.userID)
				if got.PositionTicks != tc.wantPosition || got.Played != tc.wantPlayed || got.PlayCount != user.before.PlayCount+tc.wantCountAdd {
					t.Fatalf("stop durable facts = %#v, want position %d, played %v, play count %d", got, tc.wantPosition, tc.wantPlayed, user.before.PlayCount+tc.wantCountAdd)
				}
				if !reflect.DeepEqual(got.AudioStreamIndex, user.before.AudioStreamIndex) || !reflect.DeepEqual(got.SubtitleStreamIndex, user.before.SubtitleStreamIndex) {
					t.Fatalf("stop changed remembered stream selections: got %#v, seeded %#v", got, user.before)
				}
			}
			fixture.assertEvents(t, 2)
		})
	}
}

func TestServicePartWayReportOnPlayedItemKeepsPosition(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(playstateServiceFixture, playstate.Report) error
	}{
		{name: "progress", send: func(fixture playstateServiceFixture, report playstate.Report) error {
			return fixture.application.Progress(t.Context(), fixture.actor, report)
		}},
		{name: "stop", send: func(fixture playstateServiceFixture, report playstate.Report) error {
			return fixture.application.Stop(t.Context(), fixture.actor, report)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlaystateServiceFixture(t, item.KindMovie)
			seeded := fixture.seedUserData(t, fixture.primaryUser)
			seeded.Played = true
			if err := fixture.items.UpsertUserData(t.Context(), seeded); err != nil {
				t.Fatalf("mark seeded user data played: %v", err)
			}
			fixture.startLivePlayback(t)
			report := playstate.Report{PlaybackState: session.PlaybackState{
				ItemID:        fixture.item.ID,
				PositionTicks: int64Pointer(ticks(120)),
				PlaySessionID: "opaque-playback-correlation",
			}}

			if err := tc.send(fixture, report); err != nil {
				t.Fatalf("%s playback: %v", tc.name, err)
			}
			got := fixture.userData(t, fixture.primaryUser)
			if got.PositionTicks != ticks(120) || !got.Played || got.PlayCount != seeded.PlayCount {
				t.Fatalf("%s on a played item = position %d, played %v, play count %d; want position %d, played true, play count %d", tc.name, got.PositionTicks, got.Played, got.PlayCount, ticks(120), seeded.PlayCount)
			}
		})
	}
}

func TestServiceStopFailureAndNegativePositionDoNotPersist(t *testing.T) {
	for _, tc := range []struct {
		name      string
		report    func(dto.UUID) playstate.Report
		wantError bool
		wantLive  bool
	}{
		{
			name: "failed stop clears only the live session",
			report: func(itemID dto.UUID) playstate.Report {
				position := ticks(600)
				return playstate.Report{PlaybackState: session.PlaybackState{ItemID: itemID, PositionTicks: &position}, Failed: true}
			},
		},
		{
			name: "negative position is rejected before clearing the session",
			report: func(itemID dto.UUID) playstate.Report {
				position := int64(-1)
				return playstate.Report{PlaybackState: session.PlaybackState{ItemID: itemID, PositionTicks: &position}}
			},
			wantError: true,
			wantLive:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlaystateServiceFixture(t, item.KindMovie)
			primaryBefore := fixture.seedUserData(t, fixture.primaryUser)
			additionalBefore := fixture.seedUserData(t, fixture.additionalUser)
			fixture.startLivePlayback(t)

			err := fixture.application.Stop(t.Context(), fixture.actor, tc.report(fixture.item.ID))
			if tc.wantError && err == nil {
				t.Fatal("negative stop position returned nil error")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("failed stop returned error: %v", err)
			}
			if got := fixture.session(t).NowPlaying; (got != nil) != tc.wantLive {
				t.Fatalf("live state after stop = %#v, want retained=%v", got, tc.wantLive)
			}
			for _, user := range []struct {
				userID dto.UUID
				before store.UserData
			}{{fixture.primaryUser, primaryBefore}, {fixture.additionalUser, additionalBefore}} {
				if got := fixture.userData(t, user.userID); !reflect.DeepEqual(got, user.before) {
					t.Fatalf("non-persisting stop stored %#v, want %#v", got, user.before)
				}
			}
			fixture.assertEvents(t, 0)
		})
	}
}

// MediaSourceId identifies the separately persisted alternate version while
// ItemId remains the item displayed to the client.
func TestServiceAlternateMediaSourceLifecycleUsesAlternateUserData(t *testing.T) {
	position := ticks(120)
	for _, tc := range []struct {
		name     string
		notifies bool
		prepare  func(t *testing.T, fixture playstateServiceFixture)
		apply    func(context.Context, playstate.Application, playstate.Actor, playstate.Report) error
		assert   func(t *testing.T, got, before store.UserData)
	}{
		{
			name:     "start",
			notifies: true,
			prepare:  func(_ *testing.T, _ playstateServiceFixture) {},
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Start(ctx, actor, report)
			},
			assert: func(t *testing.T, got, before store.UserData) {
				t.Helper()
				if got.PlayCount != before.PlayCount+1 || got.LastPlayed == nil {
					t.Fatalf("alternate start user data = %#v, want incremented count and last-played time", got)
				}
			},
		},
		{
			name:    "progress",
			prepare: func(t *testing.T, fixture playstateServiceFixture) { fixture.startLivePlayback(t) },
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Progress(ctx, actor, report)
			},
			assert: func(t *testing.T, got, before store.UserData) {
				t.Helper()
				if got.PositionTicks != position || got.PlayCount != before.PlayCount {
					t.Fatalf("alternate progress user data = %#v, want position %d without a count change", got, position)
				}
			},
		},
		{
			name:     "stop",
			notifies: true,
			prepare:  func(t *testing.T, fixture playstateServiceFixture) { fixture.startLivePlayback(t) },
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Stop(ctx, actor, report)
			},
			assert: func(t *testing.T, got, before store.UserData) {
				t.Helper()
				if got.PositionTicks != position || got.PlayCount != before.PlayCount {
					t.Fatalf("alternate stop user data = %#v, want position %d without a count change", got, position)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlaystateServiceFixture(t, item.KindMovie)
			alternate := fixture.addLinkedAlternateMovie(t)
			before := make([]struct {
				userID    dto.UUID
				primary   store.UserData
				alternate store.UserData
			}, 0, 2)
			for _, userID := range []dto.UUID{fixture.primaryUser, fixture.additionalUser} {
				before = append(before, struct {
					userID    dto.UUID
					primary   store.UserData
					alternate store.UserData
				}{userID: userID, primary: fixture.seedUserData(t, userID), alternate: fixture.seedUserDataForItem(t, userID, alternate)})
			}
			tc.prepare(t, fixture)
			report := playstate.Report{PlaybackState: session.PlaybackState{
				ItemID: fixture.item.ID, MediaSourceID: alternate.ID.Dashed(), PositionTicks: &position,
			}}
			if err := tc.apply(t.Context(), fixture.application, fixture.actor, report); err != nil {
				t.Fatalf("report alternate %s: %v", tc.name, err)
			}
			for _, user := range before {
				if got := fixture.userData(t, user.userID); !reflect.DeepEqual(got, user.primary) {
					t.Fatalf("alternate %s changed displayed primary user data %#v, want %#v", tc.name, got, user.primary)
				}
				tc.assert(t, fixture.userDataForItem(t, user.userID, alternate), user.alternate)
			}

			if tc.notifies {
				fixture.assertUserDataChanged(t, []playstate.UserDataChanged{
					{UserID: fixture.primaryUser, ItemID: alternate.ID},
					{UserID: fixture.additionalUser, ItemID: alternate.ID},
				})
			} else {
				fixture.assertEvents(t, 0)
			}
		})
	}
}

func TestServiceInvalidAlternateMediaSourceFallsBackToDisplayedItem(t *testing.T) {
	position := ticks(120)
	for _, tc := range []struct {
		name   string
		kind   item.Kind
		source func(playstateServiceFixture, item.Item) string
	}{
		{name: "malformed source", kind: item.KindMovie, source: func(_ playstateServiceFixture, _ item.Item) string { return "not-a-uuid" }},
		{name: "displayed item source", kind: item.KindMovie, source: func(fixture playstateServiceFixture, _ item.Item) string { return fixture.item.ID.Dashed() }},
		{name: "non-member source", kind: item.KindMovie, source: func(_ playstateServiceFixture, alternate item.Item) string { return alternate.ID.Dashed() }},
		{name: "non-video displayed item", kind: item.KindPhoto, source: func(_ playstateServiceFixture, alternate item.Item) string { return alternate.ID.Dashed() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlaystateServiceFixture(t, tc.kind)
			alternate := fixture.addUnlinkedMovie(t)
			primaryBefore := fixture.seedUserData(t, fixture.primaryUser)
			alternateBefore := fixture.seedUserDataForItem(t, fixture.primaryUser, alternate)
			fixture.startLivePlayback(t)

			report := playstate.Report{PlaybackState: session.PlaybackState{
				ItemID: fixture.item.ID, MediaSourceID: tc.source(fixture, alternate), PositionTicks: &position,
			}}
			if err := fixture.application.Progress(t.Context(), fixture.actor, report); err != nil {
				t.Fatalf("report progress: %v", err)
			}

			if got := fixture.userData(t, fixture.primaryUser); got.ItemID != fixture.item.ID {
				t.Fatalf("fallback progress user data = %#v, want displayed item %s", got, fixture.item.ID)
			}
			if got := fixture.userDataForItem(t, fixture.primaryUser, alternate); !reflect.DeepEqual(got, alternateBefore) {
				t.Fatalf("fallback progress changed alternate user data %#v, want %#v", got, alternateBefore)
			}
			if primaryBefore.PlayCount != fixture.userData(t, fixture.primaryUser).PlayCount {
				t.Fatalf("fallback progress changed displayed item play count")
			}
		})
	}
}

func TestServiceCompletionPropagatesPlayedStateAcrossAlternateVersions(t *testing.T) {
	position := ticks(900)
	for _, tc := range []struct {
		name     string
		notifies bool
		apply    func(context.Context, playstate.Application, playstate.Actor, playstate.Report) error
		version  func(playstateServiceFixture, item.Item) string
		itemID   func(playstateServiceFixture, item.Item) dto.UUID
	}{
		{
			name: "progress completes displayed primary",
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Progress(ctx, actor, report)
			},
			version: func(fixture playstateServiceFixture, _ item.Item) string { return fixture.item.ID.Dashed() },
			itemID:  func(fixture playstateServiceFixture, _ item.Item) dto.UUID { return fixture.item.ID },
		},
		{
			name: "progress completes alternate",
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Progress(ctx, actor, report)
			},
			version: func(_ playstateServiceFixture, alternate item.Item) string { return alternate.ID.Dashed() },
			itemID:  func(_ playstateServiceFixture, alternate item.Item) dto.UUID { return alternate.ID },
		},
		{
			name:     "stop completes displayed primary",
			notifies: true,
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Stop(ctx, actor, report)
			},
			version: func(fixture playstateServiceFixture, _ item.Item) string { return fixture.item.ID.Dashed() },
			itemID:  func(fixture playstateServiceFixture, _ item.Item) dto.UUID { return fixture.item.ID },
		},
		{
			name:     "stop completes alternate",
			notifies: true,
			apply: func(ctx context.Context, application playstate.Application, actor playstate.Actor, report playstate.Report) error {
				return application.Stop(ctx, actor, report)
			},
			version: func(_ playstateServiceFixture, alternate item.Item) string { return alternate.ID.Dashed() },
			itemID:  func(_ playstateServiceFixture, alternate item.Item) dto.UUID { return alternate.ID },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPlaystateServiceFixture(t, item.KindMovie)
			alternate := fixture.addLinkedAlternateMovie(t)
			before := make([]struct {
				userID    dto.UUID
				primary   store.UserData
				alternate store.UserData
			}, 0, 2)
			for _, userID := range []dto.UUID{fixture.primaryUser, fixture.additionalUser} {
				before = append(before, struct {
					userID    dto.UUID
					primary   store.UserData
					alternate store.UserData
				}{userID: userID, primary: fixture.seedUserData(t, userID), alternate: fixture.seedUserDataForItem(t, userID, alternate)})
			}
			fixture.startLivePlayback(t)
			report := playstate.Report{PlaybackState: session.PlaybackState{
				ItemID: fixture.item.ID, MediaSourceID: tc.version(fixture, alternate), PositionTicks: &position,
			}}
			if err := tc.apply(t.Context(), fixture.application, fixture.actor, report); err != nil {
				t.Fatalf("complete playback: %v", err)
			}
			for _, user := range before {
				fixture.assertCompletedUserData(t, fixture.userData(t, user.userID), user.primary)
				fixture.assertCompletedUserData(t, fixture.userDataForItem(t, user.userID, alternate), user.alternate)
			}
			if tc.notifies {
				fixture.assertUserDataChanged(t, []playstate.UserDataChanged{
					{UserID: fixture.primaryUser, ItemID: tc.itemID(fixture, alternate)},
					{UserID: fixture.additionalUser, ItemID: tc.itemID(fixture, alternate)},
				})
			} else {
				fixture.assertEvents(t, 0)
			}
		})
	}
}

func TestServiceUserDataChangedSubscriptionTrailingCoalescesStartAndStop(t *testing.T) {
	fixture := newPlaystateServiceFixture(t, item.KindMovie)
	alternate := fixture.addLinkedAlternateMovie(t)
	fixture.seedUserData(t, fixture.primaryUser)
	fixture.seedUserDataForItem(t, fixture.primaryUser, alternate)
	refreshed, err := fixture.sessions.LogActivity(t.Context(), session.Activity{
		Client: fixture.actor.Client, Version: "1.0.0", DeviceID: fixture.actor.DeviceID, DeviceName: "Living Room", UserID: fixture.primaryUser,
	})
	if err != nil {
		t.Fatalf("replace fixture session with one user: %v", err)
	}
	fixture.actor.Target = refreshed.Target
	subscription := fixture.changes.Subscribe(events.SubscriptionOptions{Coalesce: 500 * time.Millisecond, Buffer: 2})
	t.Cleanup(func() {
		if err := subscription.Close(context.Background()); err != nil {
			t.Errorf("close coalesced user-data subscription: %v", err)
		}
	})

	if err := fixture.application.Start(t.Context(), fixture.actor, playstate.Report{PlaybackState: session.PlaybackState{
		ItemID: fixture.item.ID, MediaSourceID: alternate.ID.Dashed(),
	}}); err != nil {
		t.Fatalf("start alternate playback: %v", err)
	}
	fixture.clock.Advance(250 * time.Millisecond)
	if err := fixture.application.Stop(t.Context(), fixture.actor, playstate.Report{PlaybackState: session.PlaybackState{
		ItemID: fixture.item.ID, MediaSourceID: fixture.item.ID.Dashed(),
	}}); err != nil {
		t.Fatalf("stop displayed playback: %v", err)
	}

	fixture.clock.Advance(499 * time.Millisecond)
	assertNoUserDataChanged(t, subscription.Events())
	fixture.clock.Advance(time.Millisecond)
	assertUserDataChanged(t, subscription.Events(), []playstate.UserDataChanged{{
		UserID: fixture.primaryUser, ItemID: fixture.item.ID,
	}})
}

type playstateServiceFixture struct {
	clock          *clock.Fake
	items          *store.ItemStore
	sessions       *session.Manager
	users          playstateServiceUsers
	configuration  playstateServiceConfiguration
	changes        *events.Bus[playstate.UserDataChanged]
	subscription   *events.Subscription[playstate.UserDataChanged]
	application    playstate.Application
	item           item.Item
	userDataKey    string
	actor          playstate.Actor
	primaryUser    dto.UUID
	additionalUser dto.UUID
}

type playstateServiceConfiguration struct{ system config.ServerConfiguration }

func (configuration playstateServiceConfiguration) System() config.ServerConfiguration {
	return configuration.system
}

type playstateServiceUsers struct{ byID map[dto.UUID]store.User }

func (users playstateServiceUsers) Get(_ context.Context, id dto.UUID) (store.User, error) {
	user, found := users.byID[id]
	if !found {
		return store.User{}, errors.New("playstate fixture user not found")
	}
	return user, nil
}

type playstateServiceErrorItems struct {
	*store.ItemStore
	err error
}

func (items playstateServiceErrorItems) Get(context.Context, dto.UUID) (item.Item, error) {
	return item.Item{}, items.err
}

func newPlaystateServiceFixture(t *testing.T, kind item.Kind) playstateServiceFixture {
	t.Helper()
	fake := clock.NewFake(time.Date(2026, time.July, 27, 15, 4, 5, 0, time.UTC))
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open in-memory SQLite store: %v", err)
	}
	t.Cleanup(func() {
		if err := stores.Close(); err != nil {
			t.Errorf("close in-memory SQLite store: %v", err)
		}
	})
	items := store.NewItemStore(stores)
	runtime := ticks(1_000)
	name := "playstate fixture"
	persistedItem := item.Item{ID: dto.UUID{0x44}, Kind: kind, Name: &name, RunTimeTicks: &runtime}
	if err := items.Upsert(t.Context(), persistedItem); err != nil {
		t.Fatalf("persist playstate item: %v", err)
	}
	keys := item.UserDataKeys(persistedItem, item.UserDataInputs{})
	if len(keys) == 0 {
		t.Fatalf("persisted item %s has no user-data key", persistedItem.ID)
	}
	primaryUser, additionalUser := dto.UUID{0x21}, dto.UUID{0x22}
	sessions := session.New(fake)
	actor := playstate.Actor{Client: "Swiftfin", DeviceID: "living-room", UserID: primaryUser}
	authenticated, err := sessions.LogActivity(t.Context(), session.Activity{
		Client:            actor.Client,
		Version:           "1.0.0",
		DeviceID:          actor.DeviceID,
		DeviceName:        "Living Room",
		UserID:            primaryUser,
		AdditionalUserIDs: []dto.UUID{additionalUser},
	})
	if err != nil {
		t.Fatalf("register authenticated playback session: %v", err)
	}
	actor.Target = authenticated.Target
	changes := events.NewBus[playstate.UserDataChanged](fake)
	subscription := changes.Subscribe(events.SubscriptionOptions{Buffer: 8})
	t.Cleanup(func() {
		if err := changes.Close(context.Background()); err != nil {
			t.Errorf("close user-data change bus: %v", err)
		}
	})
	configuration := playstateServiceConfiguration{system: config.ServerConfiguration{
		MinResumePct:             12,
		MaxResumePct:             83,
		MinResumeDurationSeconds: 401,
		MinAudiobookResume:       6,
		MaxAudiobookResume:       7,
	}}
	users := playstateServiceUsers{byID: map[dto.UUID]store.User{
		primaryUser:    {ID: primaryUser, Configuration: []byte(`{"RememberAudioSelections":true,"RememberSubtitleSelections":true}`)},
		additionalUser: {ID: additionalUser, Configuration: []byte(`{"RememberAudioSelections":true,"RememberSubtitleSelections":true}`)},
	}}
	var application playstate.Application = playstate.NewService(fake, configuration, items, users, sessions, changes)
	return playstateServiceFixture{
		clock:          fake,
		items:          items,
		sessions:       sessions,
		users:          users,
		configuration:  configuration,
		changes:        changes,
		subscription:   subscription,
		application:    application,
		item:           persistedItem,
		userDataKey:    keys[0],
		actor:          actor,
		primaryUser:    primaryUser,
		additionalUser: additionalUser,
	}
}

func (fixture playstateServiceFixture) addLinkedAlternateMovie(t *testing.T) item.Item {
	t.Helper()
	alternate := fixture.addUnlinkedMovie(t)
	primary := fixture.item
	primary.LinkedChildren = []item.LinkedChildRef{{ItemID: alternate.ID, Type: item.LinkedChildTypeLinkedAlternateVersion}}
	if err := fixture.items.Upsert(t.Context(), primary); err != nil {
		t.Fatalf("link alternate movie %s to displayed item: %v", alternate.ID, err)
	}
	return alternate
}

func (fixture playstateServiceFixture) addUnlinkedMovie(t *testing.T) item.Item {
	t.Helper()
	runtime := ticks(1_000)
	name := "alternate playstate fixture"
	alternate := item.Item{ID: dto.UUID{0x45}, Kind: item.KindMovie, Name: &name, RunTimeTicks: &runtime}
	if err := fixture.items.Upsert(t.Context(), alternate); err != nil {
		t.Fatalf("persist alternate movie: %v", err)
	}
	return alternate
}

func (fixture playstateServiceFixture) seedUserData(t *testing.T, userID dto.UUID) store.UserData {
	t.Helper()
	return fixture.seedUserDataForItem(t, userID, fixture.item)
}

func (fixture playstateServiceFixture) seedUserDataForItem(t *testing.T, userID dto.UUID, value item.Item) store.UserData {
	t.Helper()
	keys := item.UserDataKeys(value, item.UserDataInputs{})
	if len(keys) == 0 {
		t.Fatalf("item %s has no user-data key", value.ID)
	}
	rating := float32(8.5)
	likes := 3
	audio, subtitle := 1, 2
	lastPlayed := fixture.clock.Now().Add(-time.Hour)
	data := store.UserData{
		UserID:              userID,
		CustomKey:           keys[0],
		ItemID:              value.ID,
		PositionTicks:       ticks(50),
		PlayCount:           7,
		IsFavorite:          true,
		Rating:              &rating,
		Likes:               &likes,
		LastPlayed:          &lastPlayed,
		AudioStreamIndex:    &audio,
		SubtitleStreamIndex: &subtitle,
	}
	if err := fixture.items.UpsertUserData(t.Context(), data); err != nil {
		t.Fatalf("seed persisted user data: %v", err)
	}
	return data
}

func (fixture playstateServiceFixture) userData(t *testing.T, userID dto.UUID) store.UserData {
	t.Helper()
	return fixture.userDataForItem(t, userID, fixture.item)
}

func (fixture playstateServiceFixture) userDataForItem(t *testing.T, userID dto.UUID, value item.Item) store.UserData {
	t.Helper()
	keys := item.UserDataKeys(value, item.UserDataInputs{})
	if len(keys) == 0 {
		t.Fatalf("item %s has no user-data key", value.ID)
	}
	data, err := fixture.items.GetUserData(t.Context(), userID, keys[0])
	if err != nil {
		t.Fatalf("read persisted user data for %s: %v", value.ID, err)
	}
	return data
}

func (fixture playstateServiceFixture) assertNoUserData(t *testing.T) {
	t.Helper()
	for _, userID := range []dto.UUID{fixture.primaryUser, fixture.additionalUser} {
		if _, err := fixture.items.GetUserData(t.Context(), userID, fixture.userDataKey); !store.IsNotFound(err) {
			t.Fatalf("unknown-item report persisted user data for %s: %v", userID, err)
		}
	}
}

func (fixture playstateServiceFixture) session(t *testing.T) session.Snapshot {
	t.Helper()
	snapshot, found := fixture.sessions.Session(fixture.actor.Client, fixture.actor.DeviceID)
	if !found {
		t.Fatalf("authenticated playback session %s/%s disappeared", fixture.actor.Client, fixture.actor.DeviceID)
	}
	return snapshot
}

func (fixture playstateServiceFixture) startLivePlayback(t *testing.T) {
	t.Helper()
	if err := fixture.sessions.StartPlayback(t.Context(), fixture.actor.Target, session.PlaybackState{ItemID: fixture.item.ID, PlaySessionID: "live-before-stop"}); err != nil {
		t.Fatalf("seed live playback state: %v", err)
	}
}

func (fixture playstateServiceFixture) assertCompletedUserData(t *testing.T, got, before store.UserData) {
	t.Helper()
	if !got.Played || got.PositionTicks != 0 {
		t.Fatalf("completed user data = %#v, want Played=true and PositionTicks=0", got)
	}
	if got.ItemID != before.ItemID || got.PlayCount != before.PlayCount || got.IsFavorite != before.IsFavorite || !reflect.DeepEqual(got.Rating, before.Rating) || !reflect.DeepEqual(got.Likes, before.Likes) || !reflect.DeepEqual(got.LastPlayed, before.LastPlayed) || !reflect.DeepEqual(got.AudioStreamIndex, before.AudioStreamIndex) || !reflect.DeepEqual(got.SubtitleStreamIndex, before.SubtitleStreamIndex) {
		t.Fatalf("completion changed user-data facts outside played state: got %#v, before %#v", got, before)
	}
}

func (fixture playstateServiceFixture) assertUserDataChanged(t *testing.T, want []playstate.UserDataChanged) {
	t.Helper()
	assertUserDataChanged(t, fixture.subscription.Events(), want)
}

func assertUserDataChanged(t *testing.T, events <-chan playstate.UserDataChanged, want []playstate.UserDataChanged) {
	t.Helper()
	for index, expected := range want {
		if got := receiveUserDataChanged(t, events); !reflect.DeepEqual(got, expected) {
			t.Fatalf("user-data event %d = %#v, want %#v", index, got, expected)
		}
	}
	assertNoUserDataChanged(t, events)
}

func receiveUserDataChanged(t *testing.T, events <-chan playstate.UserDataChanged) playstate.UserDataChanged {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case change, ok := <-events:
		if !ok {
			t.Fatal("user-data event subscription closed before delivery")
		}
		return change
	case <-timer.C:
		t.Fatal("user-data event was not delivered after deterministic work yielded")
		return playstate.UserDataChanged{}
	}
}

func assertNoUserDataChanged(t *testing.T, events <-chan playstate.UserDataChanged) {
	t.Helper()
	select {
	case change, ok := <-events:
		if !ok {
			t.Fatal("user-data event subscription closed unexpectedly")
		}
		t.Fatalf("unexpected user-data event %#v", change)
	default:
	}
}

func (fixture playstateServiceFixture) assertEvents(t *testing.T, want int) {
	t.Helper()
	for index := range want {
		select {
		case <-fixture.subscription.Events():
		default:
			t.Fatalf("user-data change events = %d before return, want %d", index, want)
		}
	}
	select {
	case extra := <-fixture.subscription.Events():
		t.Fatalf("unexpected extra user-data change event %#v", extra)
	default:
	}
}

// Source: SessionManager.cs:1059-1070. A stop with a negative position is
// rejected, but it still releases the live stream so a stalled client does
// not hold a tuner.
func TestServiceNegativeStopReleasesLiveStream(t *testing.T) {
	fixture := newPlaystateServiceFixture(t, item.KindMovie)
	var closed []string
	if err := fixture.sessions.SetOptions(session.Options{CloseLiveStream: func(_ context.Context, id string) error {
		closed = append(closed, id)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.sessions.StartPlayback(t.Context(), fixture.actor.Target, session.PlaybackState{ItemID: fixture.item.ID, LiveStreamID: "live-1", PlaySessionID: "play-1"}); err != nil {
		t.Fatal(err)
	}
	position := int64(-1)
	report := playstate.Report{PlaybackState: session.PlaybackState{ItemID: fixture.item.ID, PositionTicks: &position, LiveStreamID: "live-1"}}
	if err := fixture.application.Stop(t.Context(), fixture.actor, report); err == nil {
		t.Fatal("negative stop position returned nil error")
	}
	if !reflect.DeepEqual(closed, []string{"live-1"}) {
		t.Fatalf("closed %v, want [live-1]", closed)
	}
}
