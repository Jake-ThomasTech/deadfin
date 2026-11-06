// SPDX-License-Identifier: GPL-2.0-only

package playstate_test

import (
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/playstate"
	"github.com/deadfin-org/deadfin/internal/session"
)

// TestServiceProgressAfterRestoreUpdatesUserData (#620): a session restored
// in the next server process takes a progress report from the same client,
// and the report saves the position for every session user.
func TestServiceProgressAfterRestoreUpdatesUserData(t *testing.T) {
	fixture := newPlaystateServiceFixture(t, item.KindMovie)
	fixture.seedUserData(t, fixture.primaryUser)
	fixture.seedUserData(t, fixture.additionalUser)
	fixture.sessions.EnablePersistence()
	if err := fixture.application.Start(t.Context(), fixture.actor, playstate.Report{PlaybackState: session.PlaybackState{ItemID: fixture.item.ID, PlaySessionID: "play-1"}}); err != nil {
		t.Fatalf("start: %v", err)
	}
	records, _, err := fixture.sessions.TakeChanges()
	if err != nil || len(records) != 1 {
		t.Fatalf("TakeChanges = %d records, %v", len(records), err)
	}

	fixture.clock.Advance(10 * time.Second)
	restarted := session.New(fixture.clock)
	t.Cleanup(func() { _ = restarted.Close() })
	if restored := restarted.Restore(records); restored != 1 {
		t.Fatalf("Restore = %d", restored)
	}
	application := playstate.NewService(fixture.clock, fixture.configuration, fixture.items, fixture.users, restarted, fixture.changes)
	// The request middleware records the activity of the authenticated
	// request before the handler resolves the target.
	if outcome := restarted.RecordAuthenticatedActivity(session.Activity{Client: fixture.actor.Client, Version: "1.0.0", DeviceID: fixture.actor.DeviceID, UserID: fixture.primaryUser}); outcome != session.AuthenticatedActivityTouched {
		t.Fatalf("activity = %v, want the restored session touched", outcome)
	}
	snapshot, found := restarted.Session(fixture.actor.Client, fixture.actor.DeviceID)
	if !found || snapshot.NowPlaying == nil || snapshot.NowPlaying.ItemID != fixture.item.ID {
		t.Fatalf("restored session = %+v, %v", snapshot, found)
	}
	actor := fixture.actor
	actor.Target = snapshot.Target
	position := ticks(300)
	if err := application.Progress(t.Context(), actor, playstate.Report{PlaybackState: session.PlaybackState{ItemID: fixture.item.ID, PositionTicks: &position, PlaySessionID: "play-1"}}); err != nil {
		t.Fatalf("progress after restore: %v", err)
	}
	if got := fixture.userData(t, fixture.primaryUser); got.PositionTicks != position {
		t.Fatalf("primary position = %d, want %d", got.PositionTicks, position)
	}
	if got := fixture.userData(t, fixture.additionalUser); got.PositionTicks != position {
		t.Fatalf("additional user position = %d, want %d", got.PositionTicks, position)
	}
}
