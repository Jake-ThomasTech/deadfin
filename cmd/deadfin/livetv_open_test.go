// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/tasks"
)

// liveTSFixture renders a short MPEG-TS clip and serves it as an endless
// live channel: the clip repeats until the client disconnects.
func liveTSFixture(t *testing.T) *httptest.Server {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("live open test requires ffmpeg: %v", err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skipf("live open test requires ffprobe: %v", err)
	}
	clip := filepath.Join(t.TempDir(), "clip.ts")
	render := exec.CommandContext(t.Context(), ffmpegPath, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30000/1001", "-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "2", "-map", "0:v", "-map", "1:a", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-flags", "+ilme+ildct", "-g", "30",
		"-c:a:0", "ac3", "-c:a:1", "aac", "-f", "mpegts", clip)
	if output, err := render.CombinedOutput(); err != nil {
		t.Skipf("render TS fixture: %v: %s", err, output)
	}
	data, err := os.ReadFile(clip)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		for r.Context().Err() == nil {
			if _, err := w.Write(data); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// liveTvApp is a listening application with one M3U tuner of tunerCount
// streams whose channels are the given paths of the TS fixture.
type liveTvApp struct {
	app      *application
	token    string
	userID   string
	tunerID  string
	channels []string
}

func newLiveTvApp(t *testing.T, tunerCount int, paths ...string) liveTvApp {
	t.Helper()
	channel := liveTSFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := listener.Addr().String()
	root := filepath.Join(t.TempDir(), "program-data")
	if err := os.MkdirAll(filepath.Join(root, "config", "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	app := newCompositionApplicationWithListen(t, root, listen)
	server := &http.Server{Handler: app.Router(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Configuration", []byte(`{"ServerName":"Test","UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}`), http.StatusNoContent)
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/User", []byte(`{"Name":"admin","Password":"correct horse battery staple"}`), http.StatusNoContent)
	assertHTTPStatus(t, app.Router(), http.MethodPost, "/Startup/Complete", nil, http.StatusNoContent)
	login := compositionRequestFrom(app.Router(), http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"admin","Pw":"correct horse battery staple"}`), "127.0.0.1:8096", "")
	token := compositionAccessToken(t, login)
	var auth struct{ User struct{ Id string } }
	if err := json.Unmarshal(login.Body.Bytes(), &auth); err != nil {
		t.Fatal(err)
	}

	// The channel host is a name, not a LAN address, so the source is remote
	// like a provider stream.
	var m3u strings.Builder
	m3u.WriteString("#EXTM3U\n")
	for index, path := range paths {
		fmt.Fprintf(&m3u, "#EXTINF:-1 tvg-id=\"c%d.us\" tvg-name=\"C%d\",Channel %d\n%s%s\n", index, index, index, strings.Replace(channel.URL, "127.0.0.1", "localhost", 1), path)
	}
	playlist := filepath.Join(t.TempDir(), "tuner.m3u")
	if err := os.WriteFile(playlist, []byte(m3u.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	tunerBody := fmt.Sprintf(`{"Type":"m3u","Url":%q,"FriendlyName":"fixture","TunerCount":%d,"AllowStreamSharing":true}`, playlist, tunerCount)
	saved := authenticatedCompositionRequest(app.Router(), http.MethodPost, "/LiveTv/TunerHosts", []byte(tunerBody), token)
	if saved.Code != http.StatusOK {
		t.Fatalf("POST /LiveTv/TunerHosts = %d: %s", saved.Code, saved.Body.String())
	}
	var tuner struct{ Id string }
	if err := json.Unmarshal(saved.Body.Bytes(), &tuner); err != nil {
		t.Fatal(err)
	}
	return liveTvApp{app: app, token: token, userID: auth.User.Id, tunerID: tuner.Id, channels: waitForChannels(t, app, token, len(paths))}
}

// TestLiveTvAutoOpenPlaybackInfo is SharkParty's PlaybackInfo call for a
// channel: AutoOpenLiveStream opens the stream and answers the opened source
// with an HLS transcoding URL.
func TestLiveTvAutoOpenPlaybackInfo(t *testing.T) {
	live := newLiveTvApp(t, 1, "/live/user/pass/1.ts")
	app, token, tunerID, channelID := live.app, live.token, live.tunerID, live.channels[0]

	// SharkParty's AVPlayer and VLC profiles: HLS transcoding into fMP4
	// only. The M3U tuner keeps AllowFmp4TranscodingContainer off, so the
	// source asks for the most compatible (ts) profile (D-89).
	sharkParty := `{"UserId":"` + live.userID + `","AutoOpenLiveStream":true,"MaxStreamingBitrate":20000000,"DeviceProfile":{"MaxStreamingBitrate":20000000,` +
		`"DirectPlayProfiles":[{"Type":"Video","Container":"mp4,m4v,mov","VideoCodec":"h264,hevc","AudioCodec":"aac,ac3,eac3"}],` +
		`"TranscodingProfiles":[{"Container":"mp4","Type":"Video","VideoCodec":"hevc,h264,mpeg4","AudioCodec":"aac,ac3,alac,eac3,flac","Protocol":"hls","Context":"Streaming","MaxAudioChannels":"8","MinSegments":2,"BreakOnNonKeyFrames":true}]}}`
	rec := authenticatedCompositionRequest(app.Router(), http.MethodPost, "/Items/"+channelID+"/PlaybackInfo", []byte(sharkParty), token)
	var info struct {
		MediaSources  []struct{ LiveStreamId, TranscodingUrl string }
		PlaySessionId string
		ErrorCode     string
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &info) != nil {
		t.Fatalf("PlaybackInfo = %d: %s", rec.Code, rec.Body.String())
	}
	if info.ErrorCode != "" || len(info.MediaSources) != 1 || info.PlaySessionId == "" {
		t.Fatalf("PlaybackInfo: %s", rec.Body.String())
	}
	source := info.MediaSources[0]
	if source.LiveStreamId == "" || !strings.Contains(source.TranscodingUrl, "/master.m3u8?") || !strings.Contains(source.TranscodingUrl, "SegmentContainer=mp4") || !strings.Contains(source.TranscodingUrl, "LiveStreamId="+source.LiveStreamId) {
		t.Fatalf("opened source: %s", rec.Body.String())
	}
	if rec := authenticatedCompositionRequest(app.Router(), http.MethodPost, "/LiveStreams/Close?liveStreamId="+source.LiveStreamId, nil, token); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body.String())
	}

	// A client with no usable profile gets NoCompatibleStream, and the open
	// is released at once: the single tuner is free for the next open.
	unplayable := `{"UserId":"` + live.userID + `","AutoOpenLiveStream":true,"DeviceProfile":{"DirectPlayProfiles":[{"Type":"Video","Container":"webm","VideoCodec":"vp9","AudioCodec":"opus"}]}}`
	rec = authenticatedCompositionRequest(app.Router(), http.MethodPost, "/Items/"+channelID+"/PlaybackInfo", []byte(unplayable), token)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ErrorCode":"NoCompatibleStream"`) {
		t.Fatalf("unplayable PlaybackInfo = %d: %s", rec.Code, rec.Body.String())
	}
	if held := app.liveStreams.InUse(tunerID); held != 0 {
		t.Fatalf("unplayable open holds %d tuners", held)
	}
}

func waitForChannels(t *testing.T, app *application, token string, count int) []string {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		info, _ := app.scheduler.Get(tasks.RefreshGuideID)
		if info.State == tasks.StateIdle && info.LastExecutionResult != nil {
			rec := authenticatedCompositionRequest(app.Router(), http.MethodGet, "/LiveTv/Channels", nil, token)
			var channels struct{ Items []struct{ Id string } }
			if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &channels) == nil && len(channels.Items) == count {
				ids := make([]string, 0, count)
				for _, channel := range channels.Items {
					ids = append(ids, channel.Id)
				}
				return ids
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("channels not stored")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestLiveTvChannelSwitchReleasesPreviousStream is D-92: a client on a
// single-tuner host switches channels and reports the old channel's stop
// only after the new one plays. The open of the new channel releases the
// device's old stream first, and the late stop releases nothing more.
func TestLiveTvChannelSwitchReleasesPreviousStream(t *testing.T) {
	live := newLiveTvApp(t, 1, "/live/user/pass/1.ts", "/live/user/pass/2.ts")
	app, token := live.app, live.token
	profile := `"DeviceProfile":{"TranscodingProfiles":[{"Container":"ts","Type":"Video","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls","Context":"Streaming"}]}`
	open := func(channelID string) (liveStreamID, playSessionID, mediaSourceID string) {
		t.Helper()
		rec := authenticatedCompositionRequest(app.Router(), http.MethodPost, "/Items/"+channelID+"/PlaybackInfo",
			[]byte(`{"UserId":"`+live.userID+`","AutoOpenLiveStream":true,`+profile+`}`), token)
		var info struct {
			MediaSources  []struct{ Id, LiveStreamId string }
			PlaySessionId string
			ErrorCode     string
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &info) != nil || info.ErrorCode != "" || len(info.MediaSources) != 1 {
			t.Fatalf("PlaybackInfo %s = %d: %s", channelID, rec.Code, rec.Body.String())
		}
		return info.MediaSources[0].LiveStreamId, info.PlaySessionId, info.MediaSources[0].Id
	}
	report := func(path, channelID, liveStreamID, playSessionID, mediaSourceID string) {
		t.Helper()
		body := fmt.Sprintf(`{"ItemId":%q,"MediaSourceId":%q,"LiveStreamId":%q,"PlaySessionId":%q,"PlayMethod":"Transcode"}`, channelID, mediaSourceID, liveStreamID, playSessionID)
		if rec := authenticatedCompositionRequest(app.Router(), http.MethodPost, path, []byte(body), token); rec.Code != http.StatusNoContent {
			t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	brokerID := func(liveStreamID string) string { return liveStreamID[strings.LastIndexByte(liveStreamID, '_')+1:] }

	first, firstSession, firstSource := open(live.channels[0])
	// SharkParty re-sends the start report every 5 s while it buffers.
	// Repeated starts open nothing and add no consumer.
	for range 3 {
		report("/Sessions/Playing", live.channels[0], first, firstSession, firstSource)
	}
	if info, held := app.liveStreams.Info(brokerID(first)); !held || info.Consumers != 1 || app.liveStreams.InUse(live.tunerID) != 1 {
		t.Fatalf("after repeated starts: %+v held %t, in use %d", info, held, app.liveStreams.InUse(live.tunerID))
	}
	second, secondSession, secondSource := open(live.channels[1])
	if _, held := app.liveStreams.Info(brokerID(first)); held {
		t.Fatal("the switched-away stream is still open")
	}
	if inUse := app.liveStreams.InUse(live.tunerID); inUse != 1 {
		t.Fatalf("tuner holds %d streams, want 1", inUse)
	}
	report("/Sessions/Playing", live.channels[1], second, secondSession, secondSource)
	report("/Sessions/Playing", live.channels[1], second, secondSession, secondSource)

	// The late stop of the first channel leaves the second open.
	report("/Sessions/Playing/Stopped", live.channels[0], first, firstSession, firstSource)
	if _, held := app.liveStreams.Info(brokerID(second)); !held {
		t.Fatal("the late stop of the old channel closed the new stream")
	}
	report("/Sessions/Playing/Stopped", live.channels[1], second, secondSession, secondSource)
	if inUse := app.liveStreams.InUse(live.tunerID); inUse != 0 {
		t.Fatalf("tuner holds %d streams after the stop", inUse)
	}
}

// deviceRequest is an authenticated request from a named device.
func deviceRequest(handler http.Handler, method, path string, body []byte, token, deviceID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:8096"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	authorization := fmt.Sprintf(`MediaBrowser Client="Deadfin Test", Device="Deadfin Browser", DeviceId=%q, Version="1.0"`, deviceID)
	if token != "" {
		authorization += `, Token="` + token + `"`
	}
	req.Header.Set("Authorization", authorization)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestLiveTvLateStartAfterSwitchKeepsSharedStream is the D-92 guard: a
// device switches A→B while its start report for A is still in flight.
// The late start and the stop of A do not take the consumer of a second
// device that watches A, and the tuner count stays right.
func TestLiveTvLateStartAfterSwitchKeepsSharedStream(t *testing.T) {
	live := newLiveTvApp(t, 2, "/live/user/pass/1.ts", "/live/user/pass/2.ts")
	app := live.app
	login := deviceRequest(app.Router(), http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"admin","Pw":"correct horse battery staple"}`), "", "living-room")
	other := compositionAccessToken(t, login)
	devices := map[string]string{"deadfin-test-device": live.token, "living-room": other}
	profile := `"DeviceProfile":{"TranscodingProfiles":[{"Container":"ts","Type":"Video","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls","Context":"Streaming"}]}`
	type opened struct{ channel, liveStreamID, playSessionID, mediaSourceID string }
	open := func(device, channelID string) opened {
		t.Helper()
		rec := deviceRequest(app.Router(), http.MethodPost, "/Items/"+channelID+"/PlaybackInfo",
			[]byte(`{"UserId":"`+live.userID+`","AutoOpenLiveStream":true,`+profile+`}`), devices[device], device)
		var info struct {
			MediaSources  []struct{ Id, LiveStreamId string }
			PlaySessionId string
			ErrorCode     string
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &info) != nil || info.ErrorCode != "" || len(info.MediaSources) != 1 {
			t.Fatalf("PlaybackInfo %s from %s = %d: %s", channelID, device, rec.Code, rec.Body.String())
		}
		return opened{channelID, info.MediaSources[0].LiveStreamId, info.PlaySessionId, info.MediaSources[0].Id}
	}
	report := func(device, path string, o opened) {
		t.Helper()
		body := fmt.Sprintf(`{"ItemId":%q,"MediaSourceId":%q,"LiveStreamId":%q,"PlaySessionId":%q,"PlayMethod":"Transcode"}`, o.channel, o.mediaSourceID, o.liveStreamID, o.playSessionID)
		if rec := deviceRequest(app.Router(), http.MethodPost, path, []byte(body), devices[device], device); rec.Code != http.StatusNoContent {
			t.Fatalf("%s from %s = %d: %s", path, device, rec.Code, rec.Body.String())
		}
	}
	consumers := func(o opened) int {
		info, held := app.liveStreams.Info(o.liveStreamID[strings.LastIndexByte(o.liveStreamID, '_')+1:])
		if !held {
			return 0
		}
		return info.Consumers
	}

	tvA := open("deadfin-test-device", live.channels[0])
	report("deadfin-test-device", "/Sessions/Playing", tvA)
	roomA := open("living-room", live.channels[0])
	report("living-room", "/Sessions/Playing", roomA)
	if roomA.liveStreamID != tvA.liveStreamID || consumers(tvA) != 2 {
		t.Fatalf("A not shared: %s %s, consumers %d", tvA.liveStreamID, roomA.liveStreamID, consumers(tvA))
	}

	tvB := open("deadfin-test-device", live.channels[1])
	if consumers(tvA) != 1 {
		t.Fatalf("switch left A with %d consumers, want 1", consumers(tvA))
	}
	// The start for A that was in flight across the switch, then A's stop.
	report("deadfin-test-device", "/Sessions/Playing", tvA)
	report("deadfin-test-device", "/Sessions/Playing", tvB)
	report("deadfin-test-device", "/Sessions/Playing/Stopped", tvA)
	if consumers(tvA) != 1 || consumers(tvB) != 1 || app.liveStreams.InUse(live.tunerID) != 2 {
		t.Fatalf("after the late reports: A %d, B %d consumers, in use %d", consumers(tvA), consumers(tvB), app.liveStreams.InUse(live.tunerID))
	}

	report("living-room", "/Sessions/Playing/Stopped", roomA)
	report("deadfin-test-device", "/Sessions/Playing/Stopped", tvB)
	if inUse := app.liveStreams.InUse(live.tunerID); inUse != 0 {
		t.Fatalf("tuner holds %d streams after both stops", inUse)
	}
}
