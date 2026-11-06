// SPDX-License-Identifier: GPL-2.0-only

package session_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/session"
)

// liveCloses records the live stream ids the close hook receives.
type liveCloses struct {
	mu  sync.Mutex
	ids []string
}

func (closes *liveCloses) close(_ context.Context, id string) error {
	closes.mu.Lock()
	defer closes.mu.Unlock()
	closes.ids = append(closes.ids, id)
	return nil
}

func (closes *liveCloses) got() []string {
	closes.mu.Lock()
	defer closes.mu.Unlock()
	return append([]string(nil), closes.ids...)
}

func newLiveManager(t *testing.T) (*session.Manager, *liveCloses) {
	t.Helper()
	manager, _ := newManager(t)
	closes := &liveCloses{}
	if err := manager.SetOptions(session.Options{CloseLiveStream: closes.close}); err != nil {
		t.Fatal(err)
	}
	return manager, closes
}

func playLive(t *testing.T, manager *session.Manager, snapshot session.Snapshot, liveStreamID, playSessionID string) {
	t.Helper()
	state := session.PlaybackState{LiveStreamID: liveStreamID, PlaySessionID: playSessionID}
	if err := manager.StartPlayback(t.Context(), snapshot.Target, state); err != nil {
		t.Fatalf("StartPlayback: %v", err)
	}
}

// Source: SessionManager.cs:1139 (OnPlaybackStopped) and :325-362
// (CloseLiveStreamIfNeededAsync). A stop with LiveStreamId releases the
// stream once; a repeated stop finds no mapping and releases again, as
// upstream does.
func TestManagerStopPlaybackClosesLiveStream(t *testing.T) {
	manager, closes := newLiveManager(t)
	web := logActivity(t, manager, "Jellyfin Web", "device-1", sessionTestUserA)
	playLive(t, manager, web, "live-1", "play-1")

	if err := manager.StopPlayback(t.Context(), web.Target, session.PlaybackState{LiveStreamID: "live-1", PlaySessionID: "play-1"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1"}) {
		t.Fatalf("closed %v, want [live-1]", got)
	}
	if err := manager.StopPlayback(t.Context(), web.Target, session.PlaybackState{}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); len(got) != 1 {
		t.Fatalf("a stop without LiveStreamId closed %v", got)
	}
}

// A play session that already released the stream (a killed transcode)
// does not release it again when the session stops.
func TestManagerCloseLiveStreamIfNeededByPlaySession(t *testing.T) {
	manager, closes := newLiveManager(t)
	web := logActivity(t, manager, "Jellyfin Web", "device-1", sessionTestUserA)
	other := logActivity(t, manager, "Swiftfin", "device-2", sessionTestUserB)
	playLive(t, manager, web, "live-1", "play-1")
	playLive(t, manager, other, "live-1", "play-2")

	manager.CloseLiveStreamIfNeeded(t.Context(), "live-1", "play-1")
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1"}) {
		t.Fatalf("closed %v, want one release", got)
	}
	manager.CloseLiveStreamIfNeeded(t.Context(), "live-1", web.ID)
	if got := closes.got(); len(got) != 1 {
		t.Fatalf("second release for one viewer: %v", got)
	}
	manager.CloseLiveStreamIfNeeded(t.Context(), "live-1", other.ID)
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1", "live-1"}) {
		t.Fatalf("closed %v, want a release per viewer", got)
	}
}

// Source: SessionManager.cs:308-321 (CloseIfNeededAsync). Ending a session
// releases the live streams it still plays, and only those.
func TestManagerRemoveClosesLiveStreamsOfTheSession(t *testing.T) {
	manager, closes := newLiveManager(t)
	web := logActivity(t, manager, "Jellyfin Web", "device-1", sessionTestUserA)
	other := logActivity(t, manager, "Swiftfin", "device-2", sessionTestUserB)
	playLive(t, manager, web, "live-1", "play-1")
	playLive(t, manager, other, "live-2", "play-2")

	if !manager.ReportSessionEnded(web.ID) {
		t.Fatal("session not removed")
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1"}) {
		t.Fatalf("closed %v, want [live-1]", got)
	}
	if ended := manager.EndDeviceSessions("DEVICE-2"); len(ended) != 1 {
		t.Fatalf("ended %v", ended)
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1", "live-2"}) {
		t.Fatalf("closed %v, want [live-1 live-2]", got)
	}
}

// A new play session on the same session replaces the old one; one stop
// releases the stream once.
func TestManagerLiveStreamPlaySessionChange(t *testing.T) {
	manager, closes := newLiveManager(t)
	web := logActivity(t, manager, "Jellyfin Web", "device-1", sessionTestUserA)
	playLive(t, manager, web, "live-1", "play-1")
	if err := manager.ProgressPlayback(t.Context(), web.Target, session.PlaybackState{LiveStreamID: "live-1", PlaySessionID: "play-2"}); err != nil {
		t.Fatal(err)
	}
	manager.CloseLiveStreamIfNeeded(t.Context(), "live-1", "play-1")
	if got := closes.got(); len(got) != 0 {
		t.Fatalf("stale play session released the stream: %v", got)
	}
	if err := manager.StopPlayback(t.Context(), web.Target, session.PlaybackState{LiveStreamID: "live-1"}); err != nil {
		t.Fatal(err)
	}
	if !manager.Remove(web.ID) {
		t.Fatal("not removed")
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1"}) {
		t.Fatalf("closed %v, want one release", got)
	}
}

// D-92: before a device opens another channel, its sessions stop counting
// as viewers of the streams the release filter names, and each such stream
// loses the device's consumer once. A late progress report does not track
// the stream again and a late stop report releases nothing, so the other
// viewer's consumer stays.
func TestManagerReleaseDeviceLiveStreams(t *testing.T) {
	manager, closes := newLiveManager(t)
	tv := logActivity(t, manager, "SharkParty", "device-1", sessionTestUserA)
	other := logActivity(t, manager, "Swiftfin", "device-2", sessionTestUserB)
	playLive(t, manager, tv, "live-1", "play-1")
	playLive(t, manager, other, "live-1", "play-2")

	var asked []string
	manager.ReleaseDeviceLiveStreams(t.Context(), "DEVICE-1", func(id string) bool {
		asked = append(asked, id)
		return true
	})
	if !reflect.DeepEqual(asked, []string{"live-1"}) {
		t.Fatalf("filter asked %v, want the device's stream once", asked)
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1"}) {
		t.Fatalf("closed %v, want one release", got)
	}

	if err := manager.ProgressPlayback(t.Context(), tv.Target, session.PlaybackState{LiveStreamID: "live-1", PlaySessionID: "play-1"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.StopPlayback(t.Context(), tv.Target, session.PlaybackState{LiveStreamID: "live-1", PlaySessionID: "play-1"}); err != nil {
		t.Fatal(err)
	}
	manager.CloseLiveStreamIfNeeded(t.Context(), "live-1", "play-1")
	if !manager.ReportSessionEnded(tv.ID) {
		t.Fatal("session not removed")
	}
	if got := closes.got(); len(got) != 1 {
		t.Fatalf("late reports released again: %v", got)
	}
	if err := manager.StopPlayback(t.Context(), other.Target, session.PlaybackState{LiveStreamID: "live-1", PlaySessionID: "play-2"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1", "live-1"}) {
		t.Fatalf("closed %v, want the other viewer's release", got)
	}
}

// A switched-away stream that no other session plays loses its consumer at
// once. A new start report of the same stream tracks it again, so its stop
// releases again.
func TestManagerReleaseDeviceLiveStreamsSoleViewer(t *testing.T) {
	manager, closes := newLiveManager(t)
	tv := logActivity(t, manager, "SharkParty", "device-1", sessionTestUserA)
	playLive(t, manager, tv, "live-1", "play-1")

	manager.ReleaseDeviceLiveStreams(t.Context(), "device-1", func(string) bool { return false })
	if got := closes.got(); len(got) != 0 {
		t.Fatalf("filter false closed %v", got)
	}
	manager.ReleaseDeviceLiveStreams(t.Context(), "device-1", func(string) bool { return true })
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1"}) {
		t.Fatalf("closed %v, want [live-1]", got)
	}
	if err := manager.StopPlayback(t.Context(), tv.Target, session.PlaybackState{LiveStreamID: "live-1"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); len(got) != 1 {
		t.Fatalf("late stop closed again: %v", got)
	}

	playLive(t, manager, tv, "live-1", "play-4")
	if err := manager.StopPlayback(t.Context(), tv.Target, session.PlaybackState{LiveStreamID: "live-1", PlaySessionID: "play-4"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1", "live-1"}) {
		t.Fatalf("closed %v, want a release for the new start", got)
	}
}

// Repeated start reports of one stream (SharkParty re-sends them while it
// buffers) track it once, so one stop releases it once.
func TestManagerRepeatedStartTracksOnce(t *testing.T) {
	manager, closes := newLiveManager(t)
	tv := logActivity(t, manager, "SharkParty", "device-1", sessionTestUserA)
	other := logActivity(t, manager, "Swiftfin", "device-2", sessionTestUserB)
	playLive(t, manager, other, "live-1", "play-2")
	for range 3 {
		playLive(t, manager, tv, "live-1", "play-1")
	}
	if err := manager.StopPlayback(t.Context(), tv.Target, session.PlaybackState{LiveStreamID: "live-1", PlaySessionID: "play-1"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1"}) {
		t.Fatalf("closed %v, want one release", got)
	}
	// The other viewer is still tracked: its stop releases once more.
	if err := manager.StopPlayback(t.Context(), other.Target, session.PlaybackState{LiveStreamID: "live-1", PlaySessionID: "play-2"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-1", "live-1"}) {
		t.Fatalf("closed %v, want a release per viewer", got)
	}
}

// D-92: a start report that was in flight across a switch (SharkParty
// re-sends starts every 5 s while it is slow) does not track the released
// stream again, so its stop cannot take the consumer of another device
// that shares the stream. After SwitchReleaseHold, or after the device
// opens the stream again, a start tracks it as before.
func TestManagerLateStartAfterSwitchIsIgnored(t *testing.T) {
	manager, fake := newManager(t)
	closes := &liveCloses{}
	if err := manager.SetOptions(session.Options{CloseLiveStream: closes.close}); err != nil {
		t.Fatal(err)
	}
	tv := logActivity(t, manager, "SharkParty", "device-1", sessionTestUserA)
	other := logActivity(t, manager, "Swiftfin", "device-2", sessionTestUserB)
	playLive(t, manager, tv, "live-A", "play-1")
	playLive(t, manager, other, "live-A", "play-2")

	// tv switches A→B.
	manager.ReleaseDeviceLiveStreams(t.Context(), "device-1", func(id string) bool { return id == "live-A" })
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-A"}) {
		t.Fatalf("switch released %v", got)
	}
	playLive(t, manager, tv, "live-B", "play-3")

	// The late start and stop of A change nothing.
	fake.Advance(5 * time.Second)
	playLive(t, manager, tv, "live-A", "play-1")
	if err := manager.StopPlayback(t.Context(), tv.Target, session.PlaybackState{LiveStreamID: "live-A", PlaySessionID: "play-1"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); len(got) != 1 {
		t.Fatalf("late start and stop released %v", got)
	}
	// device-2 still owns its consumer of A.
	if err := manager.StopPlayback(t.Context(), other.Target, session.PlaybackState{LiveStreamID: "live-A", PlaySessionID: "play-2"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); !reflect.DeepEqual(got, []string{"live-A", "live-A"}) {
		t.Fatalf("closed %v, want device-2's release", got)
	}

	// A switch, then the device opens the stream again: its start tracks.
	playLive(t, manager, tv, "live-C", "play-4")
	manager.ReleaseDeviceLiveStreams(t.Context(), "device-1", func(id string) bool { return id == "live-C" })
	manager.ReopenDeviceLiveStream("DEVICE-1", "live-C")
	playLive(t, manager, tv, "live-C", "play-5")
	if err := manager.StopPlayback(t.Context(), tv.Target, session.PlaybackState{LiveStreamID: "live-C", PlaySessionID: "play-5"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); len(got) != 4 || got[2] != "live-C" || got[3] != "live-C" {
		t.Fatalf("closed %v, want the switch and the reopened stop", got)
	}

	// After the hold a start tracks again.
	playLive(t, manager, tv, "live-D", "play-6")
	manager.ReleaseDeviceLiveStreams(t.Context(), "device-1", func(id string) bool { return id == "live-D" })
	fake.Advance(session.SwitchReleaseHold)
	playLive(t, manager, tv, "live-D", "play-6")
	if err := manager.StopPlayback(t.Context(), tv.Target, session.PlaybackState{LiveStreamID: "live-D", PlaySessionID: "play-6"}); err != nil {
		t.Fatal(err)
	}
	if got := closes.got(); len(got) != 6 {
		t.Fatalf("closed %v, want a release after the hold", got)
	}
}
