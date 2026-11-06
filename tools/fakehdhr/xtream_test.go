// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const testPass = "FAKEPASS-test-0001"

func xtreamServer(t *testing.T) (string, *server, blockSource) {
	t.Helper()
	ts, s, src := testServer(t, 2, nil)
	s.xt = &xtream{user: "fakeuser", pass: testPass, movies: 3, episodes: 2}
	return ts.URL, s, src
}

func TestXtreamPlaylistAndGuide(t *testing.T) {
	base, s, _ := xtreamServer(t)
	creds := "?username=fakeuser&password=" + testPass
	for _, path := range []string{"/get.php?username=fakeuser&password=wrong&type=m3u_plus", "/xmltv.php?username=x&password=" + testPass, "/player_api.php"} {
		if code, _ := body(t, base+path); code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", path, code)
		}
	}
	code, m3u := body(t, base+"/get.php"+creds+"&type=m3u_plus&output=ts")
	if code != 200 {
		t.Fatalf("get.php = %d", code)
	}
	for _, want := range []string{
		`tvg-id="fake-atsc.fake" tvg-name="FAKE-ATSC"`,
		base + "/live/fakeuser/" + testPass + "/10000.ts\n",
		base + "/movie/fakeuser/" + testPass + "/500002.mp4\n",
		base + "/series/fakeuser/" + testPass + "/800001.mkv\n",
		`group-title="VOD | Broadcast"`,
	} {
		if !strings.Contains(m3u, want) {
			t.Errorf("m3u_plus lacks %q", want)
		}
	}
	if got := strings.Count(m3u, "#EXTINF"); got != len(s.channels)+3+2 {
		t.Errorf("entries = %d", got)
	}
	if code, xml := body(t, base+"/xmltv.php"+creds); code != 200 || !strings.Contains(xml, `<channel id="fake-atsc.fake">`) {
		t.Fatalf("xmltv.php = %d", code)
	}
	if s.authRejects.Load() != 3 {
		t.Fatalf("auth rejects = %d", s.authRejects.Load())
	}
}

func TestXtreamLiveTunesAndNeverLogsTheCredentials(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	base, s, src := xtreamServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/live/fakeuser/"+testPass+"/10002.ts", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("live tune: %v %v", err, resp)
	}
	if got := <-src.started; got != "4.1" {
		t.Fatalf("stream id 10002 tuned %s, want 4.1", got)
	}
	cancel()
	resp.Body.Close()
	// Wait for the handler's last log line before reading the buffer.
	for active, _ := s.tuners.counts(); active > 0; active, _ = s.tuners.counts() {
		runtime.Gosched()
	}
	for _, path := range []string{"/live/fakeuser/wrong/10002.ts", "/live/fakeuser/" + testPass + "/99999.ts", "/movie/fakeuser/" + testPass + "/500000.mp4"} {
		if code, _ := body(t, base+path); code == 200 {
			t.Errorf("%s = 200", path)
		}
	}
	if logs.Len() == 0 || strings.Contains(logs.String(), testPass) {
		t.Fatalf("log output leaks the password or is empty:\n%s", logs.String())
	}
}

func TestXtreamBansAfterFailedLogins(t *testing.T) {
	base, s, _ := xtreamServer(t)
	now := testEpoch
	s.clock = func() time.Time { return now }
	s.xt.banAfter, s.xt.banFor = 3, 10*time.Minute
	good := base + "/get.php?username=fakeuser&password=" + testPass
	bad := base + "/get.php?username=fakeuser&password=wrong"
	for i := range 3 {
		if code, _ := body(t, bad); code != http.StatusUnauthorized {
			t.Fatalf("bad login %d = %d", i, code)
		}
	}
	// Banned now, even with the right password.
	if code, b := body(t, good); code != http.StatusForbidden || !strings.Contains(b, "banned") {
		t.Fatalf("after 3 failures = %d %q", code, b)
	}
	now = now.Add(9 * time.Minute)
	if code, _ := body(t, good); code != http.StatusForbidden {
		t.Fatalf("still inside the ban = %d", code)
	}
	now = now.Add(2 * time.Minute)
	if code, _ := body(t, good); code != 200 {
		t.Fatalf("after the ban = %d", code)
	}
	if s.authRejects.Load() != 3 || s.banRejects.Load() != 2 || s.xtRequests.Load() != 6 {
		t.Fatalf("counters: auth %d ban %d requests %d", s.authRejects.Load(), s.banRejects.Load(), s.xtRequests.Load())
	}
}

func TestXtreamPlayerAPI(t *testing.T) {
	base, s, _ := xtreamServer(t)
	api := base + "/player_api.php?username=fakeuser&password=" + testPass
	var account struct {
		UserInfo struct {
			Auth           int    `json:"auth"`
			MaxConnections string `json:"max_connections"`
		} `json:"user_info"`
	}
	getJSON(t, api, &account)
	if account.UserInfo.Auth != 1 || account.UserInfo.MaxConnections != "2" {
		t.Fatalf("account = %+v", account)
	}
	var cats []struct {
		ID   string `json:"category_id"`
		Name string `json:"category_name"`
	}
	getJSON(t, api+"&action=get_live_categories", &cats)
	if len(cats) == 0 || cats[0].ID != "1" || cats[0].Name != "Broadcast" {
		t.Fatalf("categories = %+v", cats)
	}
	type stream struct {
		ID       string `json:"stream_id"`
		Name     string `json:"name"`
		EPG      string `json:"epg_channel_id"`
		Category string `json:"category_id"`
		Type     string `json:"stream_type"`
	}
	var all, news []stream
	getJSON(t, api+"&action=get_live_streams", &all)
	if len(all) != len(s.channels) || all[2].ID != "10002" || all[2].EPG != "fake-h264.fake" || all[2].Type != "live" {
		t.Fatalf("streams = %d, third %+v", len(all), all[2])
	}
	newsID := cats[slices.IndexFunc(cats, func(c struct {
		ID   string `json:"category_id"`
		Name string `json:"category_name"`
	}) bool {
		return c.Name == "News"
	})].ID
	getJSON(t, api+"&action=get_live_streams&category_id="+newsID, &news)
	if len(news) != 1 || news[0].Name != "FAKE-NEWS" {
		t.Fatalf("news category = %+v", news)
	}
	// The JSON guide endpoints answer, but every call is counted: a client
	// must take the guide from xmltv.php.
	if code, _ := body(t, api+"&action=get_short_epg&stream_id=10002"); code != 200 || s.jsonEPGCalls.Load() != 1 {
		t.Fatalf("get_short_epg = %d, counted %d", code, s.jsonEPGCalls.Load())
	}
}

func TestXtreamMixedTypesQuirk(t *testing.T) {
	base, s, _ := xtreamServer(t)
	s.quirks = quirks{"xtream-types": true}
	_, raw := body(t, base+"/player_api.php?username=fakeuser&password="+testPass+"&action=get_live_streams")
	for _, want := range []string{`"stream_id":10001`, `"stream_id":"10000"`, `"epg_channel_id":""`, `"stream_icon":null`} {
		if !strings.Contains(raw, want) {
			t.Errorf("mixed types lack %s", want)
		}
	}
}

func TestUserAgentRecordAndBlock(t *testing.T) {
	base, s, _ := xtreamServer(t)
	s.blockUA = true
	do := func(path, ua string) int {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("User-Agent", ua)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	m3u := "/get.php?username=fakeuser&password=" + testPass
	for ua, want := range map[string]int{"": 403, "Lavf/61.7.100": 403, "Go-http-client/1.1": 403, "Deadfin/0.9.0": 200} {
		if got := do(m3u, ua); got != want {
			t.Errorf("UA %q = %d, want %d", ua, got, want)
		}
	}
	if got := do("/live/fakeuser/"+testPass+"/99999.ts", "Lavf/61.7.100"); got != 403 {
		t.Errorf("stream with Lavf UA = %d", got)
	}
	ua := s.userAgents()
	if ua["lineup|Deadfin/0.9.0"] != 1 || ua["lineup|Lavf/61.7.100"] != 1 || ua["stream|Lavf/61.7.100"] != 1 {
		t.Fatalf("user agents = %v", ua)
	}
}
